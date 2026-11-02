# pombar build helpers. All builds are static: CGO_ENABLED=0.

BINARY  ?= pombar
PKG     := ./cmd/pombar
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOOS    ?= linux
GOARCH  ?= $(shell go env GOARCH)

export CGO_ENABLED := 0

.PHONY: all build build-all test vet lint fmt clean

all: vet test build

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

# Cross-compile the release targets.
build-all:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-arm64 $(PKG)

test:
	go test ./...

vet:
	go vet ./...

# gofmt + vet always; golangci-lint too when it is installed.
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed, skipped"; fi

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
