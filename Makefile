BINARY := pgdx
PKG     := github.com/capitaltg/pgdx
# Version from git (tag/commit, -dirty if uncommitted); falls back to "dev".
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X $(PKG)/cmd.version=$(VERSION)"
# Release ldflags also strip symbols/DWARF to shrink the binary (matches CI).
REL_LDFLAGS := -ldflags "-s -w -X $(PKG)/cmd.version=$(VERSION)"
DIST := dist
# Cross-compile targets, mirroring .github/workflows/release.yml.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build dist install test cover vet fmt tidy clean

all: fmt vet test build

## build: compile the pgdx binary (with version stamped in)
build:
	go build $(LDFLAGS) -o $(BINARY) .

## dist: cross-compile + archive all release targets into ./dist (like CI)
dist:
	@rm -rf $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		bin=$(BINARY); [ "$$os" = windows ] && bin=$(BINARY).exe; \
		name=$(BINARY)_$(VERSION)_$${os}_$${arch}; \
		echo ">> building $$name"; \
		mkdir -p $(DIST)/$$name; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath $(REL_LDFLAGS) \
			-o $(DIST)/$$name/$$bin . || exit 1; \
		cp LICENSE README.md $(DIST)/$$name/; \
		if [ "$$os" = windows ] && command -v zip >/dev/null 2>&1; then \
			(cd $(DIST) && zip -qr $$name.zip $$name); \
		else \
			[ "$$os" = windows ] && echo "   (zip not found; using tar.gz — CI still ships a .zip)"; \
			tar -czf $(DIST)/$$name.tar.gz -C $(DIST) $$name; \
		fi; \
	done
	@echo "--- $(DIST) ---"; ls -1 $(DIST)/*.tar.gz $(DIST)/*.zip 2>/dev/null || true

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

## clean: remove the built binary and dist artifacts
clean:
	rm -f $(BINARY)
	rm -rf $(DIST)
