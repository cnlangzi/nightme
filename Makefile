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
# on a developer laptop produces a native binary.
#
# The DEFAULT Linux build has no CGo dependency at all (the tray
# is behind -tags gui; see GO_TAGS below), so
# `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 make build` cross-compiles
# cleanly. Every OTHER configuration is CGo — macOS (Cocoa),
# Windows (Win32) and `-tags gui` Linux (GTK3+AppIndicator) — and
# no cross toolchain is configured here, so CI builds those on
# their own native runner (see .github/workflows/{ci,release}.yml).
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

# Go build tags. Currently only `gui` is meaningful: it opts the
# Linux build in to the system-tray implementation (cmd/nightme/
# tray_gui.go). Leaving it empty on Linux selects the no-op stub
# in cmd/nightme/tray_stub.go, which is what we want by default —
# see the header comment in cmd/nightme/tray.go for the full
# reasoning, but in short:
#
#   getlantern/systray links libayatana-appindicator3.so.1 +
#   libgtk-3.so.0, and a Linux host without the GTK3 runtime
#   cannot exec the binary at all (ld.so refuses before main()).
#   Linux boxes are mostly servers, so tray-off is the safe
#   default and `make build-gui` is the opt-in.
#
# macOS and Windows ignore this knob — their tray backings (Cocoa /
# Win32) ship with the OS, so tray_gui.go is unconditional there.
GO_TAGS       ?=

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
	@# Linux tray icon (full color, 64x64).
	@convert logo.png -resize 64x64 $(TRAY_PNG)
	@# macOS menu-bar template icons. Cocoa treats these as
	# templates when systray.SetTemplateIcon is called: the
	# alpha channel is the mask, the RGB is ignored (Cocoa
	# draws the alpha shape in the menu-bar foreground
	# color, which is black in light mode and white in
	# dark mode). Plain -resize preserves the alpha; do
	# NOT use -alpha extract — that strips the alpha
	# channel and produces a grayscale PNG that the
	# template API cannot use. On a real macOS dev box,
	# 'make tray-assets' (GOOS=darwin) regenerates these
	# via sips for sharper anti-aliasing.
	@convert logo.png -resize 22x22 $(TRAY_TEMPLATE_22)
	@convert logo.png -resize 44x44 $(TRAY_TEMPLATE_44)
	@convert logo.png -resize 66x66 $(TRAY_TEMPLATE_66)
	@echo "[tray-assets] built $(TRAY_PNG) + $(TRAY_TEMPLATE_22) $(TRAY_TEMPLATE_44) $(TRAY_TEMPLATE_66)"
else
	@echo "[tray-assets] GOOS=$(GOOS): Windows uses logo.ico via go-winres; skipping"
endif

.PHONY: build
build: winres ## Compile binary to bin/nightme[.exe] with version metadata.
	@mkdir -p bin
	$(GO) build -tags '$(GO_TAGS)' -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/nightme

# build-gui produces the Linux tray-enabled variant alongside the
# default tray-less one. Requires libgtk-3-dev +
# libayatana-appindicator3-dev at build time and libgtk-3-0 +
# libayatana-appindicator3-1 at run time.
#
# BIN_NAME is overridden rather than BINARY so $(EXT) still
# applies; the release pipeline packages this as
# nightme_<v>_linux_<arch>-gui.tar.gz with the binary renamed back
# to plain `nightme` so users can drop it straight over the
# default install.
#
# No-op off Linux: macOS and Windows already build the tray into
# the default binary, so a separate GUI variant would be identical
# to `make build`.
.PHONY: build-gui
build-gui: ## Compile the Linux tray-enabled binary to bin/nightme-gui (Linux only).
ifneq ($(GOOS),linux)
	@echo "[build-gui] GOOS=$(GOOS): tray is already in the default binary; skipping"
else
	@$(MAKE) build GO_TAGS=gui BIN_NAME=$(BIN_NAME)-gui
endif

.PHONY: release
release: winres ## Build a versioned binary into dist/nightme-<GOOS>-<GOARCH>[.exe] (host-only).
	@mkdir -p $(BIN_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -tags '$(GO_TAGS)' -ldflags '$(LDFLAGS)' -o $(RELEASE_BIN) ./cmd/nightme

# tray-assets is intentionally NOT in the build/release dependency
# chain. The tray-icon byte payload that //go:embed resolves in
# cmd/nightme/tray_assets.go is committed to the repo (Linux:
# assets/tray.png; macOS: assets/trayTemplate{,_@2x,_@3x}.png;
# Windows: assets/logo-32.ico — produced by go-winres from
# assets/winres.json). CI and release pipelines consume the
# pre-built assets directly; ImageMagick is NOT installed on
# the hosted runners (the prior setup tried to invoke convert
# on every Linux build, which fails with 'ImageMagick
# required'). `make tray-assets` exists for dev use (e.g. a
# dev who wants to regenerate the macOS templates with sips
# for sharper anti-aliasing) and is otherwise a no-op.

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
