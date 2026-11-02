# pomobar build helpers. All builds are static: CGO_ENABLED=0.

BINARY  ?= pomobar
PKG     := ./cmd/pomobar
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOOS    ?= linux
GOARCH  ?= $(shell go env GOARCH)

export CGO_ENABLED := 0

.PHONY: all build build-all dist test vet lint fmt clean

all: vet test build

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

# Cross-compile the release targets.
build-all:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-arm64 $(PKG)

# Release archives: one tar.gz per target with the binary, README, LICENSE
# and THIRD_PARTY_NOTICES.md, plus SHA256SUMS. Fails if a binary is not
# statically linked.
DIST_TARGETS := linux/amd64 linux/arm64
dist: clean
	@set -e; mkdir -p dist; for t in $(DIST_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; name=$(BINARY)-$(VERSION)-$$os-$$arch; \
		mkdir -p dist/$$name; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name/$(BINARY) $(PKG); \
		file dist/$$name/$(BINARY) | grep -q 'statically linked' || { echo "$$name: not static"; exit 1; }; \
		cp README.md LICENSE THIRD_PARTY_NOTICES.md dist/$$name/; \
		tar -C dist -czf dist/$$name.tar.gz --owner=0 --group=0 --sort=name $$name; \
		rm -r dist/$$name; \
	done; cd dist && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS

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
