# Live Git (lg) — build & install
#
# `lg` is a self-contained native binary: once built it runs with NO Go
# toolchain present, like any brew-installed CLI. You only need Go to BUILD it.

BINARY      := lg
PKG         := ./cmd/lg
PREFIX      ?= $(HOME)/.local
BINDIR      := $(PREFIX)/bin
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X github.com/iamtaehyunpark/livegit/internal/cli.Version=$(VERSION)

# CGO_ENABLED=0 => fully static, no libc dependency, maximally portable.
GOBUILD     := CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)"

# Platforms for `make release`.
PLATFORMS   := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build install uninstall test vet clean release agents docs publish

AGENTDIR := internal/agentbin/data
DOCSDIR  := internal/docs

## docs: sync the embedded guides with the repo-root originals (so `lg init`
## drops the current GUIDE.md / AGENTS.md into each project root; AGENTS.md is
## also dropped as CLAUDE.md — mapped in internal/docs/embed.go, not a file here.
## Do NOT copy the repo-root CLAUDE.md: that's this repo's own dev notes).
docs:
	@cp GUIDE.md AGENTS.md $(DOCSDIR)/
	@echo "synced embedded docs in $(DOCSDIR)"

## agents: cross-compile the Linux agent binaries embedded into the host binary
## (so `lg init` can deploy a matching agent to a passworded Source).
##
## The agents are built into a STAGING dir with $(AGENTDIR) EMPTY, then copied
## in. This is deliberate: the Linux agent must NOT embed the agent binaries
## (only the host binary deploys agents). Building straight into $(AGENTDIR)
## makes each build embed the previous binaries — geometric growth that blows
## past the linker's 2 GiB section limit after enough rebuilds.
AGENTSTAGE := bin/agents
agents:
	@rm -f $(AGENTDIR)/lg-linux-*        # empty the embed dir so agents embed nothing
	@mkdir -p $(AGENTSTAGE) $(AGENTDIR)
	@for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -ldflags "$(LDFLAGS)" \
	    -o $(AGENTSTAGE)/lg-linux-$$arch $(PKG); \
	done
	@cp $(AGENTSTAGE)/lg-linux-amd64 $(AGENTSTAGE)/lg-linux-arm64 $(AGENTDIR)/
	@echo "embedded linux agents ($(VERSION)) in $(AGENTDIR)"

## build: compile the binary into ./bin/lg (embeds the Linux agents + guides)
build: agents docs
	@mkdir -p bin
	$(GOBUILD) -o bin/$(BINARY) $(PKG)
	@# On macOS, re-sign with a full ad-hoc signature. The linker's adhoc
	@# signature plus stripping can yield "Code Signature Invalid" SIGKILLs;
	@# an explicit codesign produces a robust, page-hash-consistent signature.
	@if [ "$$(uname)" = "Darwin" ]; then codesign --force --sign - bin/$(BINARY) >/dev/null 2>&1 || true; fi
	@echo "built bin/$(BINARY) ($(VERSION))"

## install: build and atomically place at $(BINDIR) (defaults to ~/.local/bin)
install: build
	@mkdir -p $(BINDIR)
	@# Atomic install in a SINGLE shell so the temp name is consistent: copy to
	@# a temp file in the same dir, then rename. The rename gives the new binary
	@# a fresh inode instead of overwriting in place — overwriting a running or
	@# page-cached binary causes "Invalid Page" code-signature SIGKILLs on
	@# Apple Silicon (the exact failure we hit).
	@tmp="$(BINDIR)/.$(BINARY).new.$$$$"; \
	  cp bin/$(BINARY) "$$tmp" && chmod +x "$$tmp" && \
	  { [ "$$(uname)" = "Darwin" ] && xattr -c "$$tmp" 2>/dev/null || true; } && \
	  mv -f "$$tmp" "$(BINDIR)/$(BINARY)" && \
	  echo "installed $(BINDIR)/$(BINARY)"
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

## publish: cut a full release in one step — usage: make publish TAG=vX.Y.Z
## Tags, builds release binaries, creates the GitHub release, regenerates the
## Homebrew formula from the new checksums, and pushes it to the tap repo.
publish:
	@test -n "$(TAG)" || { echo "usage: make publish TAG=vX.Y.Z"; exit 1; }
	@./scripts/publish-release.sh "$(TAG)"

## release: cross-compile static binaries for all platforms into ./dist.
## Depends on `agents` so the embedded Linux agents are rebuilt at THIS version —
## a stale agent in $(AGENTDIR) would fail EnsureAgent's version check on every
## `lg connect` and re-upload endlessly. `docs` keeps the embedded guides fresh.
release: agents docs
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/$(BINARY)-$$os-$$arch; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out $(PKG); \
	done
	@echo "release binaries in ./dist"
