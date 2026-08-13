# nightme Makefile
# Minimal, opinionated build/test/dev workflow.

GO            ?= go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
GIT_COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -X github.com/cnlangzi/nightme/internal/version.Version=$(VERSION) \
                -X github.com/cnlangzi/nightme/internal/version.GitCommit=$(GIT_COMMIT) \
                -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(BUILD_DATE)

# Cross-compile knobs. Defaults track the host so `make release`
# on a developer laptop produces a native binary; CI overrides
# GOOS/GOARCH per matrix row.
GOOS          ?= $(shell go env GOOS)
GOARCH        ?= $(shell go env GOARCH)
# EXT is .exe on Windows, empty elsewhere. Applied uniformly to
# both the `build` and `release` outputs so callers can `./bin/nightme`
# on Linux/macOS and `./bin/nightme.exe` on Windows. Windows's
# CreateProcess rejects a path without the .exe suffix, so the
# build target MUST use $(EXT) — see `make restart` below.
EXT           ?= $(if $(filter windows,$(GOOS)),.exe,)
BIN_DIR       ?= dist
BIN_NAME      ?= nightme
BINARY        ?= bin/$(BIN_NAME)$(EXT)
RELEASE_BIN   := $(BIN_DIR)/$(BIN_NAME)-$(GOOS)-$(GOARCH)$(EXT)

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compile binary to bin/nightme[.exe] with version metadata.
	@mkdir -p bin
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/nightme

.PHONY: release
release: ## Build a versioned binary into dist/nightme-<GOOS>-<GOARCH>[.exe].
	@mkdir -p $(BIN_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -ldflags '$(LDFLAGS)' -o $(RELEASE_BIN) ./cmd/nightme

.PHONY: release-all
release-all: ## Build every release matrix row into dist/. CI does this per-row instead.
	@for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		GOOS=$${pair%/*} GOARCH=$${pair#*/} $(MAKE) release; \
	done

# restart runs the local `nightme restart` subcommand on the
# freshly built binary. Requires the binary to actually exist
# with the platform-correct suffix (handled by BINARY above);
# Windows CreateProcess can't run a path missing the .exe.
.PHONY: restart
restart: build ## Build and restart nightme daemon.
	$(BINARY) restart

.PHONY: test
test: ## Run all tests with race detector.
	$(GO) test -race ./...

.PHONY: dev
dev: ## Run nightme in dev mode (uses example config).
	$(GO) run ./cmd/nightme

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/ $(BIN_DIR)/

.PHONY: lint
lint: ## Run go vet (matches CI).
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go source.
	$(GO) fmt ./...

.PHONY: install
install: ## Install nightme to $$GOBIN.
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/nightme
