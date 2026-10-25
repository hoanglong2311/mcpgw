.PHONY: all build tray app install install-app release test vet fmt-check lint clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX ?= $(HOME)/.local
APPDIR ?= $(HOME)/Applications
GOARCH ?= $(shell go env GOARCH)
LDFLAGS := -X main.Version=$(VERSION)
APP := bin/mcpctl-tray.app

all: build tray

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mcpctl ./cmd/mcpctl

tray:
	go build -o bin/mcpctl-tray ./cmd/mcpctl-tray

app: tray
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources
	cp bin/mcpctl-tray $(APP)/Contents/MacOS/mcpctl-tray
	sed 's/@VERSION@/$(VERSION)/' packaging/Info.plist > $(APP)/Contents/Info.plist
	codesign --force --sign - $(APP)

install: build
	install -d $(PREFIX)/bin
	install -m 755 bin/mcpctl $(PREFIX)/bin/mcpctl

install-app: app
	install -d $(APPDIR)
	rm -rf $(APPDIR)/mcpctl-tray.app
	cp -R $(APP) $(APPDIR)/

release:
	rm -rf release && mkdir -p release
	for arch in arm64 amd64; do \
		$(MAKE) clean && $(MAKE) build app GOARCH=$$arch && \
		mkdir -p release/mcpctl_$(VERSION)_darwin_$$arch && \
		cp -R bin/mcpctl $(APP) config.example.toml README.md release/mcpctl_$(VERSION)_darwin_$$arch/ && \
		tar -C release -czf release/mcpctl_$(VERSION)_darwin_$$arch.tar.gz mcpctl_$(VERSION)_darwin_$$arch || exit 1; \
	done
	cd release && shasum -a 256 *.tar.gz > checksums.txt

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

lint: vet fmt-check

clean:
	rm -rf bin
