# Top-level Makefile for the whole repo — the single entry point.
#
#   desktop/   Go binary (go.mod lives here; recipes cd in to run go)
#   extension/ WebExtension (zero-dep, Node built-in test runner)
#
# Capture/redact/upload stay pure Go (CGO_ENABLED=0) so they cross-compile
# cleanly. The macOS tray is Cocoa, so on Darwin the build needs Cgo — building
# with it off there fails in fyne.io/systray (undefined nativeLoop, etc.).
# Tests need cgo for -race regardless.

DESKTOP := desktop
BINARY  := aiscan
PREFIX  ?= $(HOME)/.local

# 1 on macOS (tray needs Cocoa), 0 elsewhere (keep the binary pure/static).
# Override on the command line if cross-compiling.
CGO ?= $(shell [ "$$(uname)" = Darwin ] && echo 1 || echo 0)

# Version metadata stamped into the binary (see internal/buildinfo). Release
# tags are prefixed per client (desktop-vX.Y.Z); strip the prefix so the
# binary reports plain semver. No desktop tag reachable → "dev", which the
# autoupdater treats as never-update.
VERSION ?= $(shell (git describe --tags --match 'desktop-v*' --dirty 2>/dev/null || echo desktop-vdev) | sed 's/^desktop-v//')
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%d)
BI      := github.com/sleuth-io/aiscan-clients/desktop/internal/buildinfo
LDFLAGS := -ldflags "-X $(BI).Version=$(VERSION) -X $(BI).Commit=$(COMMIT) -X $(BI).Date=$(DATE)"

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the desktop binary into desktop/bin (Cgo on macOS for the tray)
	@echo "Building $(BINARY)..."
	@cd $(DESKTOP) && CGO_ENABLED=$(CGO) go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/aiscan
	@echo "Built: $(DESKTOP)/bin/$(BINARY)"

.PHONY: dmg
dmg: build ## macOS: wrap the built binary into Aiscan.app + dist/Aiscan.dmg
	@$(DESKTOP)/packaging/macos/make-dmg.sh $(DESKTOP)/bin/$(BINARY) $(VERSION) dist
	@echo "Built: dist/Aiscan.dmg"

.PHONY: install
install: build ## Build and copy the desktop binary to $(PREFIX)/bin
	@echo "Installing $(BINARY)..."
	@mkdir -p $(PREFIX)/bin
	@rm -f $(PREFIX)/bin/$(BINARY) && cp $(DESKTOP)/bin/$(BINARY) $(PREFIX)/bin/
	@printf "\033[32m✓\033[0m $(BINARY) installed to $(PREFIX)/bin/$(BINARY)\n"
	@case ":$$PATH:" in \
		*":$(PREFIX)/bin:"*) ;; \
		*) echo ""; \
		   echo "⚠ Warning: $(PREFIX)/bin is not in your PATH"; \
		   echo "Add this to your ~/.bashrc or ~/.zshrc:"; \
		   echo "  export PATH=\"\$$PATH:$(PREFIX)/bin\"" ;; \
	esac

.PHONY: run
run: ## Run the desktop binary (ARGS="capture --window-days 7")
	cd $(DESKTOP) && go run ./cmd/aiscan $(ARGS)

# Cut a release. Suggests the next version, guesses whether it should move the download pointer
# (see scripts/release.sh), and pushes the tag that triggers the release workflow.
#   make release TRAIN=desktop                     # suggest next patch, confirm, tag+push
#   make release TRAIN=extension VERSION=0.2.0     # explicit version
#   make release TRAIN=desktop RELEASE_ARGS=--not-latest
RELEASE_ARGS ?=
.PHONY: release
release: ## Cut a client release (TRAIN=desktop|extension [VERSION=x.y.z] [RELEASE_ARGS=--not-latest])
	@test -n "$(TRAIN)" || { echo "usage: make release TRAIN=desktop|extension [VERSION=x.y.z] [RELEASE_ARGS=--not-latest]"; exit 2; }
	@scripts/release.sh $(TRAIN) $(VERSION) $(RELEASE_ARGS)

.PHONY: test
test: test-desktop test-extension ## Run ALL tests (Go + JS)

.PHONY: test-desktop
test-desktop: ## Run Go tests with the race detector
	cd $(DESKTOP) && CGO_ENABLED=1 go test -race ./...

.PHONY: test-extension
test-extension: ## Run JS tests
	npm --prefix extension test

.PHONY: build-extension
build-extension: ## Stage the extension for both browsers into extension/dist (dev)
	npm --prefix extension run build

.PHONY: lint-extension
lint-extension: build-extension ## web-ext lint the staged Firefox build
	npm --prefix extension run lint

.PHONY: lint
lint: ## Lint everything (Go vet; JS has no linter configured)
	cd $(DESKTOP) && go vet ./...

.PHONY: format
format: ## Format Go code and tidy go.mod
	cd $(DESKTOP) && gofmt -s -w . && go mod tidy

.PHONY: fmtcheck
fmtcheck: ## Fail if any Go file is not gofmt-clean
	@unformatted=$$(cd $(DESKTOP) && gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: update-deps
update-deps: ## Update Go dependencies to latest and tidy
	cd $(DESKTOP) && go get -u ./... && go mod tidy

.PHONY: clean
clean: ## Remove build output
	rm -rf $(DESKTOP)/bin dist

.PHONY: prepush
prepush: fmtcheck lint test ## Run before pushing: format check + lint + all tests
	@echo "prepush ok"
