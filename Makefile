# Top-level Makefile for the whole repo — the single entry point.
#
#   desktop/   Go binary (go.mod lives here; recipes cd in to run go)
#   extension/ WebExtension (zero-dep, Node built-in test runner)
#
# Capture/redact/upload stay pure Go (CGO_ENABLED=0) so they cross-compile
# cleanly; only the macOS tray (later) pulls in Cgo. Tests need cgo for -race.

DESKTOP := desktop
BINARY  := aiscan
PREFIX  ?= $(HOME)/.local

# Version metadata stamped into the binary (see internal/buildinfo).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
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
build: ## Build the desktop binary into desktop/bin (pure Go)
	cd $(DESKTOP) && CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/aiscan

.PHONY: install
install: build ## Build and copy the desktop binary to $(PREFIX)/bin
	@mkdir -p $(PREFIX)/bin
	cp $(DESKTOP)/bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed $(BINARY) to $(PREFIX)/bin"

.PHONY: run
run: ## Run the desktop binary (ARGS="capture --window-days 7")
	cd $(DESKTOP) && go run ./cmd/aiscan $(ARGS)

.PHONY: test
test: test-desktop test-extension ## Run ALL tests (Go + JS)

.PHONY: test-desktop
test-desktop: ## Run Go tests with the race detector
	cd $(DESKTOP) && CGO_ENABLED=1 go test -race ./...

.PHONY: test-extension
test-extension: ## Run JS tests
	npm --prefix extension test

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
	rm -rf $(DESKTOP)/bin

.PHONY: prepush
prepush: fmtcheck lint test ## Run before pushing: format check + lint + all tests
	@echo "prepush ok"
