# nightme Makefile
# Minimal, opinionated build/test/dev workflow.

BINARY        ?= bin/nightme
GO            ?= go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
GIT_COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -X github.com/cnlangzi/nightme/internal/version.GitCommit=$(GIT_COMMIT) \
                -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compile binary to bin/nightme with version metadata.
	@mkdir -p bin
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/nightme

.PHONY: test
test: ## Run all tests with race detector.
	$(GO) test -race ./...

.PHONY: dev
dev: ## Run nightme in dev mode (uses example config).
	$(GO) run ./cmd/nightme

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/

.PHONY: lint
lint: ## Run go vet (matches CI).
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go source.
	$(GO) fmt ./...

.PHONY: install
install: ## Install nightme to $$GOBIN.
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/nightme