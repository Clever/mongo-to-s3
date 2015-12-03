package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Clever/mongo-to-s3/config"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	"github.com/Clever/pathio"
	"gopkg.in/Clever/optimus.v3"
	json "gopkg.in/Clever/optimus.v3/sinks/json"
	mongosource "gopkg.in/Clever/optimus.v3/sources/mongo"
	"gopkg.in/Clever/optimus.v3/transformer"
	"gopkg.in/mgo.v2"
)

var (
	configPath = flag.String("config", "config.yml", "Path to config file (default: config.yml)")
	url        = flag.String("database", "", "NECESSARY: Database url of existing instance")
	bucket     = flag.String("bucket", "clever-analytics", "s3 bucket to upload to (default: clever-analytics)")
)

// Running instance using fab takes up to ~10 minutes, so will retry over this time period, then fail after 10 minutes
func mongoConnection(url string) *mgo.Session {
	s, err := mgo.DialWithTimeout(url, 10*time.Minute)
	if err != nil {
		log.Fatal("err connecting to mongo instance: ", err)
	}
	s.SetMode(mgo.Monotonic, true)
	return s
}

// parseConfigFile loads the config from wherever it is, then parses it
func parseConfigFile(path string) config.Config {
	reader, err := pathio.Reader(path)
	defer reader.Close()
	if err != nil {
		log.Fatal("err opening config file: ", err)
	}
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Fatal("err reading file: ", err)
	}
	configYaml, err := config.ParseYAML(data)
	if err != nil {
		log.Fatal("err parsing config file: ", err)
	}

	return configYaml
}

func configuredOptimusTable(s *mgo.Session, table config.Table) optimus.Table {
	collection := s.DB("").C(table.Source)
	iter := collection.Find(nil).Iter()
	return mongosource.New(iter)
}

func formatFilename(timestamp, collectionName, extension string) string {
	return fmt.Sprintf("mongo_%s_%s%s", collectionName, timestamp, extension)
}

func exportData(source optimus.Table, table config.Table, sink optimus.Sink, timestamp string) (int, error) {
	rows := 0
	datePopulator := config.GetPopulateDateFn(table.Meta.DataDateColumn, timestamp)
	err := transformer.New(source).Map(config.Flattener()).Fieldmap(table.FieldMap()).Map(datePopulator).Map(
		func(d optimus.Row) (optimus.Row, error) {
			rows = rows + 1
			return optimus.Row(d), nil
		}).Sink(sink)
	return rows, err
}

func copyConfigFile(bucket, timestamp, path string) string {
	input, err := pathio.Reader(path)
	if err != nil {
		log.Fatal("error opening config file", err)
	}
	// config_name is parsed from the input path b/c we have a different configs`
	// get the yaml file at the end of the path
	pathRegex := regexp.MustCompile("(.*/)?(.+)\\.yml")
	matches := pathRegex.FindStringSubmatch(path)
	if len(matches) < 3 {
		log.Fatalf("issue parsing config filename from config path: %s, err: %s", path, err)
	}
	outPath := formatFilename(timestamp, matches[2], ".yml")
	if bucket != "" {
		outPath = fmt.Sprintf("s3://%s/%s", bucket, outPath)
	}
	outputBytes, err := ioutil.ReadAll(input)
	if err != nil {
		log.Fatal("error reading config file: ", err)
	}
	log.Printf("uploading conf file to: %s", outPath)
	err = pathio.Write(outPath, outputBytes)
	if err != nil {
		log.Fatal("error writing output file: ", err)
	}
	return outPath
}

// Always compute students last, if it exists, since the number is so large
func orderTables(configYaml config.Config) []config.Table {
	var tableOrder []config.Table
	for key, table := range configYaml {
		if key == "students" {
			continue
		}
		tableOrder = append(tableOrder, table)
	}
	if table, ok := configYaml["students"]; ok {
		tableOrder = append(tableOrder, table)
	}
	return tableOrder
}

func main() {
	flag.Parse()
	if *url == "" {
		log.Fatal("Database url of existing instance is necessary")
	}
	fmt.Println("Connecting to mongo: ", *url)
	mongoClient := mongoConnection(*url)
	log.Println("Connected to mongo")

	// Times are rounded down to the nearest hour
	timestamp := time.Now().UTC().Add(-1 * time.Hour / 2).Round(time.Hour).Format(time.RFC3339)

	//awsClient := aws.NewClient("us-west-1")
	/* UNUSED for now: https://clever.atlassian.net/browse/IP-349
	//var instance fab.Instance
	if instance.SnapshotID != "" {
		snapshot, err := awsClient.FindSnapshot(instance.SnapshotID)
		if err != nil {
			log.Println("err finding latest snapshot: ", err)
		} else {
			timestamp = snapshot.StartTime.Add(-1 * time.Hour / 2).Round(time.Hour).Format(time.RFC3339)
		}
	} */

	configYaml := parseConfigFile(*configPath)
	confFileName := copyConfigFile(*bucket, timestamp, *configPath)
	var tables []string
	tableOrder := orderTables(configYaml)
	for _, table := range tableOrder {
		tables = append(tables, table.Destination)
		outputName := formatFilename(timestamp, table.Destination, ".json.gz")

		// Gzip output into pipe so that we don't need to store locally
		reader, writer := io.Pipe()
		go func() {
			zippedOutput := gzip.NewWriter(writer) // sorcery
			sink := json.New(zippedOutput)

			source := configuredOptimusTable(mongoClient, table)
			count, err := exportData(source, table, sink, timestamp)
			if err != nil {
				log.Fatal("err reading table: ", err)
			}
			log.Println(table.Destination, " collection: ", count, " items")

			// ALWAYS close the gzip first
			zippedOutput.Close()
			writer.Close()
		}()

		// Upload file to bucket
		if *bucket != "" {
			s3Path := fmt.Sprintf("s3://%s/%s", *bucket, outputName)
			log.Printf("uploading file: %s to path: %s", outputName, s3Path)
			region, err := getRegionForBucket(*bucket)
			if err != nil {
				log.Fatalf("err getting region for bucket: %s", err)
			}
			log.Printf("found bucket region: %s", region)

			// required to do this since we can't pipe together the gzip output and pathio, unfortunately
			// TODO: modify Pathio so that we can support io.Pipe and use Pathio here: https://clever.atlassian.net/browse/IP-353
			// from https://github.com/aws/aws-sdk-go/wiki/Getting-Started-Common-Examples
			client := s3.New(aws.NewConfig().WithRegion(region))
			uploader := s3manager.NewUploader(&s3manager.UploadOptions{S3: client})
			_, err = uploader.Upload(&s3manager.UploadInput{
				Body:   reader,
				Bucket: aws.String(*bucket),
				Key:    aws.String(outputName),
			})
			if err != nil {
				log.Fatalf("err uploading to s3 path: %s, err: %s", s3Path, err)
			}
		}
	}

	// submit gearman job for all tables
	// doing this all at the end to ensure that the data in redshift is updated
	// at the same time for different collections

	if len(os.Getenv("CLEVER_JOB_ENDPOINT")) == 0 {
		log.Println("Not posting s3-to-redshift job")
	} else {
		log.Println("Submitting job to Gearman admin")
		client := &http.Client{}
		endpoint := os.Getenv("CLEVER_JOB_ENDPOINT") + "/s3-to-redshift"
		payload := fmt.Sprintf("--bucket %s --schema mongo --tables %s --truncate --config %s", *bucket, strings.Join(tables, ","), confFileName)
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader([]byte(payload)))
		if err != nil {
			log.Fatalf("Error creating new request: %s", err)
		}
		req.Header.Add("Content-Type", "text/plain")
		_, err = client.Do(req)
		if err != nil {
			log.Fatalf("Error submitting job:%s", err)
		}
	}

	/* UNUSED until we can figure out how to deploy: https://clever.atlassian.net/browse/IP-349
	if instance.ID != "" {
		log.Println("terminating instance")
		err := c.TerminateInstance(instance.ID)
		if err != nil {
			log.Println("err terminating instance: ", err)
		}
	} */
}

// getRegionForBucket looks up the region name for the given bucket
func getRegionForBucket(name string) (string, error) {
	// Any region will work for the region lookup, but the request MUST use
	// PathStyle
	config := aws.NewConfig().WithRegion("us-west-1").WithS3ForcePathStyle(true)
	client := s3.New(config)
	params := s3.GetBucketLocationInput{
		Bucket: aws.String(name),
	}
	resp, err := client.GetBucketLocation(&params)
	if err != nil {
		return "", fmt.Errorf("Failed to get location for bucket '%s', %s", name, err)
	}
	if resp.LocationConstraint == nil {
		// "US Standard", returns an empty region. So return any region in the US
		return "us-east-1", nil
	}
	return *resp.LocationConstraint, nil
}
