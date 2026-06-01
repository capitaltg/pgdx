BINARY := pgdx
PKG     := github.com/capitaltg/pgdx
# Version from git (tag/commit, -dirty if uncommitted); falls back to "dev".
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X $(PKG)/cmd.version=$(VERSION)"

.PHONY: all build install test cover vet fmt tidy clean

all: fmt vet test build

## build: compile the pgdx binary (with version stamped in)
build:
	go build $(LDFLAGS) -o $(BINARY) .

## install: install pgdx into $GOBIN / $GOPATH/bin
install:
	go install $(LDFLAGS) .

## test: run the test suite
test:
	go test ./...

## cover: run tests with coverage
cover:
	go test -cover ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format the code
fmt:
	gofmt -s -w .

## tidy: tidy module dependencies
tidy:
	go mod tidy

## clean: remove the built binary
clean:
	rm -f $(BINARY)
