.PHONY: all build tray test vet fmt-check lint

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

all: build tray

build:
	go build -ldflags "-X main.Version=$(VERSION)" -o bin/mcpctl ./cmd/mcpctl

tray:
	go build -o bin/mcpctl-tray ./cmd/mcpctl-tray

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

lint: vet fmt-check
