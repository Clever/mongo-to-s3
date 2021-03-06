package config

import (
	json "github.com/pquerna/ffjson/ffjson"

	"gopkg.in/Clever/optimus.v3"
	"gopkg.in/yaml.v2"
	"reflect"
)

type Config map[string]Table

type Table struct {
	Destination string  `yaml:"dest"`
	Source      string  `yaml:"source"`
	Fields      []Field `yaml:"columns"`
	Meta        Meta    `yaml:"meta"`
}

type Field struct {
	Destination string `yaml:"dest"`
	Source      string `yaml:"source"`
	PII         bool   `yaml:"pii"`
}

type Meta struct {
	Database       string `yaml:"database"`
	DataDateColumn string `yaml:"datadatecolumn"`
	// UseProjectionOptimization makes the query more efficient by only requesting the
	// listed fields. However, note that if there are reused fields
	// (e.g. data.name and data.name.first) then the parent one will not be complete/included
	// if there are other fields in it. This breaks things like oauthclients and launchpads,
	// so we can't turn it on for everything
	UseProjectionOptimization bool `yaml:"projection_optimization"`
}

// ParseYAML marshalls data into a Config
func ParseYAML(data []byte) (Config, error) {
	config := Config{}
	err := yaml.Unmarshal(data, &config)
	return config, err
}

// FieldMap returns a mapping of all fields between source and destination
func (t Table) FieldMap() map[string][]string {
	mappings := make(map[string][]string)

	for _, field := range t.Fields {
		if field.Destination != "" {
			list := mappings[field.Source]
			mappings[field.Source] = append(list, field.Destination)
		}
	}

	return mappings
}

// GetPopulateDateFn returns a function which creates and populates the data date column
// we do this so that we have a good idea of when the data was created downstream
func GetPopulateDateFn(dataDateColumn, timestamp string) func(optimus.Row) (optimus.Row, error) {
	return func(r optimus.Row) (optimus.Row, error) {
		r[dataDateColumn] = timestamp
		return r, nil
	}
}

// GetExistentialTransformerFn returns a function which turns a PII field into a boolean
// whether it exists or not. Runs before the field map.
func GetExistentialTransformerFn(t Table) func(optimus.Row) (optimus.Row, error) {
	return func(r optimus.Row) (optimus.Row, error) {
		for _, field := range t.Fields {
			if field.PII {
				val, ok := r[field.Source]
				if !ok {
					r[field.Source] = ok
				} else {
					r[field.Source] = !IsZeroOfUnderlyingType(val)
				}
			}
		}
		return r, nil
	}
}

func IsZeroOfUnderlyingType(x interface{}) bool {
	return reflect.DeepEqual(x, reflect.Zero(reflect.TypeOf(x)).Interface())
}

// Flattener returns a function which flattens nested optimus rows into flat rows
// with dot-separated keys
func Flattener() func(optimus.Row) (optimus.Row, error) {
	return func(r optimus.Row) (optimus.Row, error) {
		outRow := optimus.Row{}
		flatten(r, "", &outRow)
		return outRow, nil
	}
}

func rowToMap(r optimus.Row) map[string]interface{} {
	m := map[string]interface{}{}
	for k, v := range r {
		m[k] = v
	}
	return m
}

// flattens a nested json struct
// to start, pass "" as a lkey
func flatten(inputJSON optimus.Row, lkey string, flattened *optimus.Row) {
	for rkey, value := range inputJSON {
		key := lkey + rkey
		switch v := value.(type) {
		case map[string]interface{}:
			flatten(optimus.Row(v), key+".", flattened)
		case optimus.Row:
			flatten(v, key+".", flattened)
		case []interface{}:
			jsonVal, _ := json.Marshal(v)
			(*flattened)[key] = string(jsonVal)
			for _, subvalues := range v {
				switch sv := subvalues.(type) {
				case map[string]interface{}:
					flatten(optimus.Row(sv), key+".", flattened)
				case optimus.Row:
					flatten(sv, key+".", flattened)
				}
			}
		default:
			(*flattened)[key] = v
		}
	}
}
