# nightme Makefile
# Minimal, opinionated build/test/dev workflow.

GO            ?= go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
GIT_COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -X github.com/cnlangzi/nightme/internal/version.Version=$(VERSION) \
                -X github.com/cnlangzi/nightme/internal/version.GitCommit=$(GIT_COMMIT) \
                -X github.com/cnlangzi/nightme/internal/version.BuildDate=$(BUILD_DATE)

# Windows icon / manifest embedding runs through go-winres, which
# produces cmd/nightme/rsrc_windows_<arch>.syso from winres.json.
# Go's linker auto-includes any *.syso in the main package's source
# dir, so once winres runs the resulting binary carries the icon +
# DPI-aware manifest. The .syso files are git-ignored — they are
# generated, not source.
WINRES        ?= go-winres
WINRES_JSON   ?= assets/winres.json
SYSO_PKG      ?= cmd/nightme

# System-tray icon assets. Per-OS targets that produce the byte
# payload that cmd/nightme/tray_assets.go embeds into the binary
# via //go:embed. Windows reuses the existing logo.ico (set up by
# the winres target above); macOS needs a single-color alpha
# "template" set so the menu bar auto-inverts for dark mode, plus
# a .icns for the NightMe.app bundle; Linux takes a plain 64x64
# PNG. tray-assets is a no-op on Windows (the .ico path is
# already covered by winres).
#
# Paths are repo-root-relative (tray-assets runs from the repo
# root, unlike the winres target which `cd`s into cmd/nightme/).
TRAY_TEMPLATE_22 := cmd/nightme/assets/trayTemplate.png
TRAY_TEMPLATE_44 := cmd/nightme/assets/trayTemplate@2x.png
TRAY_TEMPLATE_66 := cmd/nightme/assets/trayTemplate@3x.png
TRAY_ICNS        := cmd/nightme/assets/trayTemplate.icns
TRAY_PNG         := cmd/nightme/assets/tray.png
TRAY_ICONSET_DIR := cmd/nightme/assets/.iconset

# macOS .app bundle. app-bundle wraps bin/nightme into a Finder-
# navigable bundle so the menu-bar icon shows the proper icns
# (a raw binary's menu-bar icon is the generic executable glyph,
# not our logo). The bundle is NOT shipped in release artifacts;
# users on macOS run `make app-bundle` themselves and copy
# dist/NightMe.app to /Applications.
APP_NAME   := NightMe
APP_BUNDLE := dist/$(APP_NAME).app
APP_PLIST  := scripts/$(APP_NAME)/Info.plist
APP_ICON   := cmd/nightme/assets/trayTemplate.icns

# Cross-compile knobs. Defaults track the host so `make build`
# on a developer laptop produces a native binary. nightme does
# NOT support cross-compile in CI: each hosted runner builds +
# vets on its own native OS (see .github/workflows/{ci,release}.yml).
# The systray dependency is CGo (Cocoa / GTK3+AppIndicator /
# Win32) and the cross-compile toolchain is not configured here.
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

.PHONY: winres
winres: ## Embed Windows icon + manifest into cmd/nightme/rsrc_windows_*.syso (no-op off-Windows).
	@if [ "$(GOOS)" = "windows" ]; then \
		command -v $(WINRES) >/dev/null 2>&1 || { \
			echo "[winres] $(WINRES) not on PATH; installing via 'go install'..."; \
			$(GO) install github.com/tc-hib/go-winres@latest; \
		}; \
		cd $(SYSO_PKG) && \
		$(WINRES) make \
			--in $(WINRES_JSON) \
			--out rsrc \
			--arch 386,amd64,arm64 \
			--product-version $(VERSION) \
			--file-version $(VERSION); \
	else \
		echo "[winres] GOOS=$(GOOS) != windows; skipping"; \
	fi

.PHONY: tray-assets
tray-assets: ## Build macOS .icns template + Linux tray.png (no-op on Windows).
ifeq ($(GOOS),darwin)
	@command -v sips     >/dev/null 2>&1 || { echo "sips required (Xcode CLT)";     exit 1; }
	@command -v iconutil >/dev/null 2>&1 || { echo "iconutil required (Xcode CLT)"; exit 1; }
	@mkdir -p $(TRAY_ICONSET_DIR)
	@# Generate single-color alpha template (menu bar auto-inverts).
	@sips -s format png --resampleWidth 22 logo.png --out $(TRAY_TEMPLATE_22) >/dev/null
	@sips -s format png --resampleWidth 44 logo.png --out $(TRAY_TEMPLATE_44) >/dev/null
	@sips -s format png --resampleWidth 66 logo.png --out $(TRAY_TEMPLATE_66) >/dev/null
	@cp $(TRAY_TEMPLATE_22) $(TRAY_ICONSET_DIR)/icon_22x22.png
	@cp $(TRAY_TEMPLATE_44) $(TRAY_ICONSET_DIR)/icon_44x44.png
	@cp $(TRAY_TEMPLATE_66) $(TRAY_ICONSET_DIR)/icon_66x66.png
	@iconutil -c icns $(TRAY_ICONSET_DIR) -o $(TRAY_ICNS)
	@rm -rf $(TRAY_ICONSET_DIR)
	@echo "[tray-assets] built $(TRAY_TEMPLATE_22) $(TRAY_TEMPLATE_44) $(TRAY_TEMPLATE_66) $(TRAY_ICNS)"
else ifeq ($(GOOS),linux)
	@command -v convert >/dev/null 2>&1 || { echo "ImageMagick 'convert' required"; exit 1; }
	@convert logo.png -resize 64x64 $(TRAY_PNG)
	@echo "[tray-assets] built $(TRAY_PNG)"
else
	@echo "[tray-assets] GOOS=$(GOOS): Windows uses logo.ico via go-winres; skipping"
endif

.PHONY: build
build: winres tray-assets ## Compile binary to bin/nightme[.exe] with version metadata.
	@mkdir -p bin
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/nightme

.PHONY: release
release: winres tray-assets ## Build a versioned binary into dist/nightme-<GOOS>-<GOARCH>[.exe] (host-only).
	@mkdir -p $(BIN_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -ldflags '$(LDFLAGS)' -o $(RELEASE_BIN) ./cmd/nightme

.PHONY: app-bundle
app-bundle: build ## Wrap bin/nightme into dist/NightMe.app (macOS only).
ifneq ($(GOOS),darwin)
	@echo "[app-bundle] GOOS=$(GOOS) != darwin; skipping"
else
	@rm -rf $(APP_BUNDLE)
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@mkdir -p $(APP_BUNDLE)/Contents/Resources
	@cp bin/nightme $(APP_BUNDLE)/Contents/MacOS/nightme
	@cp $(APP_PLIST)  $(APP_BUNDLE)/Contents/Info.plist
	@[ -f $(APP_ICON) ] && cp $(APP_ICON) $(APP_BUNDLE)/Contents/Resources/AppIcon.icns || \
		echo "warn: $(APP_ICON) not built; run 'make tray-assets' first"
	@echo "Built: $(APP_BUNDLE)"
endif

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
	rm -f $(TRAY_TEMPLATE_22) $(TRAY_TEMPLATE_44) $(TRAY_TEMPLATE_66) $(TRAY_ICNS) $(TRAY_PNG)
	rm -rf $(TRAY_ICONSET_DIR)
	rm -f $(SYSO_PKG)/rsrc_windows_*.syso

.PHONY: lint
lint: ## Run go vet (matches CI).
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format all Go source.
	$(GO) fmt ./...

.PHONY: install
install: ## Install nightme to $$GOBIN.
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/nightme
