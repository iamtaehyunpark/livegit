# Live Git (lg) — build & install
#
# `lg` is a self-contained native binary: once built it runs with NO Go
# toolchain present, like any brew-installed CLI. You only need Go to BUILD it.

BINARY      := lg
PKG         := ./cmd/lg
PREFIX      ?= $(HOME)/.local
BINDIR      := $(PREFIX)/bin
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X github.com/taehyun/lg/internal/cli.Version=$(VERSION)

# CGO_ENABLED=0 => fully static, no libc dependency, maximally portable.
GOBUILD     := CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)"

# Platforms for `make release`.
PLATFORMS   := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build install uninstall test vet clean release

## build: compile the binary into ./bin/lg
build:
	@mkdir -p bin
	$(GOBUILD) -o bin/$(BINARY) $(PKG)
	@echo "built bin/$(BINARY) ($(VERSION))"

## install: build and copy to $(BINDIR) (defaults to ~/.local/bin)
install: build
	@mkdir -p $(BINDIR)
	@cp bin/$(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed $(BINDIR)/$(BINARY)"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; \
	  *) echo "NOTE: $(BINDIR) is not on your PATH — add: export PATH=\"$(BINDIR):\$$PATH\"";; esac

## uninstall: remove the installed binary
uninstall:
	@rm -f $(BINDIR)/$(BINARY) && echo "removed $(BINDIR)/$(BINARY)"

## test: run the test suite
test:
	go test ./...

## vet: static checks
vet:
	go vet ./...

## clean: remove build artifacts
clean:
	rm -rf bin dist

## release: cross-compile static binaries for all platforms into ./dist
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/$(BINARY)-$$os-$$arch; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out $(PKG); \
	done
	@echo "release binaries in ./dist"
