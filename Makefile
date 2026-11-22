# Skep build targets. Run "make help" for a summary.

VERSION    ?= 0.1.0
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GO      ?= go
PKG     := github.com/greenbeanorg/skep
BIN_DIR := bin
CMDS    := skep-agent skep-api skepctl

LDFLAGS := -s -w \
	-X $(PKG)/pkg/version.Version=$(VERSION) \
	-X $(PKG)/pkg/version.Commit=$(COMMIT) \
	-X $(PKG)/pkg/version.BuildDate=$(BUILD_DATE)

# CGO is off everywhere: Skep uses a pure-Go SQLite driver so the FreeBSD
# cross-compile needs no C toolchain.
export CGO_ENABLED := 0

# The web interface is built with Node.js into web/dist, copied to
# internal/webui/dist, and embedded in skep-api by the build tag skep_webui
# (ADR 0008, decision 4). "build" and "build-freebsd" do all of that;
# "build-go" and "build-freebsd-go" build the Go binaries alone, with a
# placeholder page instead of the interface, for machines without Node.js.
WEBUI_DIST := internal/webui/dist
WEBUI_TAGS := -tags skep_webui

.PHONY: all build build-go build-freebsd build-freebsd-go build-freebsd-test test vet lint fmt install-hooks run-dev clean help web-install web-check web-build web-e2e notices notices-check

all: vet test build build-freebsd

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: web-build ## Build the web interface, then the binaries for this machine with it embedded, into bin/
	@$(MAKE) --no-print-directory build-go GOTAGS="$(WEBUI_TAGS)"

build-go: ## Build the binaries for this machine into bin/ without the web interface (no Node.js needed)
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "build $(BIN_DIR)/$$cmd $(GOTAGS)"; \
		$(GO) build -trimpath $(GOTAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

build-freebsd: web-build ## Build the web interface, then cross-compile the FreeBSD amd64 binaries with it embedded
	@$(MAKE) --no-print-directory build-freebsd-go GOTAGS="$(WEBUI_TAGS)"

build-freebsd-go: ## Cross-compile the FreeBSD amd64 binaries into bin/freebsd-amd64/ without the web interface
	@mkdir -p $(BIN_DIR)/freebsd-amd64
	@for cmd in $(CMDS); do \
		echo "build $(BIN_DIR)/freebsd-amd64/$$cmd $(GOTAGS)"; \
		GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath $(GOTAGS) -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/freebsd-amd64/$$cmd ./cmd/$$cmd || exit 1; \
	done

# The test agent contains the test-only task types (build tag
# skep_testtasks) and the rc.d integration test. Neither is ever installed
# on a real host; they exist for native validation on the FreeBSD test VM.
build-freebsd-test: ## Cross-compile the FreeBSD test agent and integration tests into bin/freebsd-amd64-test/
	@mkdir -p $(BIN_DIR)/freebsd-amd64-test
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath -tags skep_testtasks -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/freebsd-amd64-test/skep-agent-testbuild ./cmd/skep-agent
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath \
		-o $(BIN_DIR)/freebsd-amd64-test/rcd-test ./tests/integration/rcd
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath \
		-o $(BIN_DIR)/freebsd-amd64-test/apitls-test ./tests/integration/apitls
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath \
		-o $(BIN_DIR)/freebsd-amd64-test/adminprompt-test ./tests/integration/adminprompt
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath \
		-o $(BIN_DIR)/freebsd-amd64-test/skepctl-test ./tests/integration/skepctl
	GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath \
		-o $(BIN_DIR)/freebsd-amd64-test/retention-test ./tests/integration/retention

test: ## Run the unit tests, with the race detector (needs a C compiler)
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...
	CGO_ENABLED=1 $(GO) test -race -count=1 -tags skep_testtasks ./cmd/...

vet: ## Run go vet for Linux and for the FreeBSD target
	$(GO) vet ./...
	GOOS=freebsd GOARCH=amd64 $(GO) vet ./...
	GOOS=freebsd GOARCH=amd64 $(GO) vet -tags skep_testtasks ./cmd/...

lint: vet ## Check formatting, vet, and run staticcheck when it is installed
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: these files need formatting:"; echo "$$unformatted"; exit 1; \
	fi
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./... && GOOS=freebsd GOARCH=amd64 staticcheck ./...; \
	else \
		echo "staticcheck is not installed; skipped (CI runs it)."; \
	fi

# The hook is linked, not copied, so it follows the repository. It lints
# the commits being pushed (see scripts/hooks/pre-push).
install-hooks: ## Install the git pre-push hook, which runs "make lint" on pushed commits
	@./scripts/install-hooks.sh

fmt: ## Format the Go sources
	gofmt -w .

# run-dev builds the Go binaries only: in development the interface is
# served by Vite (web/README.md), so skep-api needs nothing embedded.
run-dev: build-go ## Run skep-agent and skep-api in development mode (API on 127.0.0.1:8443)
	./scripts/run-dev.sh

# The web interface needs Node.js and npm.
notices: ## Regenerate THIRD_PARTY_NOTICES from the Go modules and npm packages that ship (needs make web-install)
	$(GO) run ./scripts/notices

notices-check: ## Fail when THIRD_PARTY_NOTICES is stale (CI runs this)
	$(GO) run ./scripts/notices -check

web-install: ## Install the web interface's dependencies (npm ci)
	cd web && npm ci

web-check: ## Type-check the web interface, check the generated API types, and run its tests
	cd web && npm run typecheck && npm run check-types && npm test

web-build: ## Build the web interface and copy it to $(WEBUI_DIST) for embedding
	cd web && npm run build
	rm -rf $(WEBUI_DIST)
	cp -R web/dist $(WEBUI_DIST)

web-e2e: build ## Run the browser tests against skep-api with the interface embedded, trusting it by its key pin (needs Playwright's browsers)
	./scripts/web-e2e.sh

clean: ## Remove build output, the embedded interface copy, and the development data directory
	rm -rf $(BIN_DIR) .skep-dev web/dist $(WEBUI_DIST)
