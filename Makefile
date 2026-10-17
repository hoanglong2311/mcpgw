.PHONY: build test vet fmt-check lint

build:
	go build -o bin/mcpctl ./cmd/mcpctl

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

lint: vet fmt-check
