.PHONY: all build clean test $(PKGS)
SHELL := /bin/bash
PKG := github.com/Clever/mongo-to-s3
SUBPKGS := $(addprefix $(PKG)/, aws fab config)
PKGS := $(PKG) $(SUBPKGS)
GOLINT := $(GOPATH)/bin/golint

GOVERSION := $(shell go version | grep 1.5)
ifeq "$(GOVERSION)" ""
		$(error must be running Go version 1.5)
endif
export GO15VENDOREXPERIMENT=1

test: $(PKGS)

all: build test

$(GOLINT):
	go get github.com/golang/lint/golint

build: clean
	GO15VENDOREXPERIMENT=1 go build -o "mongo-to-s3" $(PKG)

$(PKGS): $(GOLINT)
	@echo ""
	@echo "FORMATTING $@..."
	gofmt -w=true $(GOPATH)/src/$@/*.go
	@echo ""
	@echo "LINTING $@..."
	$(GOLINT) $(GOPATH)/src/$@/*.go
	@echo ""
	@echo "TESTING COVERAGE $@..."
	go test -cover -coverprofile=$(GOPATH)/src/$@/c.out $@ -test.v

clean:
	rm mongo-to-s3
