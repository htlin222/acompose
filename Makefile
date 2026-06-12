BIN := acompose
PKG := ./src
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

darwin:
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w $(LDFLAGS)" -o $(BIN)-darwin-arm64 $(PKG)

fmt:
	gofmt -w src

vet:
	go vet $(PKG)

test:
	go test $(PKG)

dryrun: build
	cd examples && ../$(BIN) up --dry-run

check: fmt vet test dryrun

.PHONY: build darwin fmt vet test dryrun check
