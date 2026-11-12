# Skep build targets. Run "make help" for a summary.

VERSION    ?= 0.1.0
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GO      ?= go
PKG     := github.com/greenbeanorg/skep
BIN_DIR := bin
CMDS    := skep-agent

LDFLAGS := -s -w \
	-X $(PKG)/pkg/version.Version=$(VERSION) \
	-X $(PKG)/pkg/version.Commit=$(COMMIT) \
	-X $(PKG)/pkg/version.BuildDate=$(BUILD_DATE)

# CGO is off everywhere: Skep uses a pure-Go SQLite driver so the FreeBSD
# cross-compile needs no C toolchain.
export CGO_ENABLED := 0

.PHONY: all build build-freebsd test vet lint fmt run-dev clean help web-install web-check web-build

all: vet test build build-freebsd

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build development binaries for this machine into bin/
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "build $(BIN_DIR)/$$cmd"; \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

build-freebsd: ## Cross-compile the FreeBSD amd64 binaries into bin/freebsd-amd64/
	@mkdir -p $(BIN_DIR)/freebsd-amd64
	@for cmd in $(CMDS); do \
		echo "build $(BIN_DIR)/freebsd-amd64/$$cmd"; \
		GOOS=freebsd GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/freebsd-amd64/$$cmd ./cmd/$$cmd || exit 1; \
	done

test: ## Run the unit tests
	$(GO) test -count=1 ./...

vet: ## Run go vet for Linux and for the FreeBSD target
	$(GO) vet ./...
	GOOS=freebsd GOARCH=amd64 $(GO) vet ./...

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

fmt: ## Format the Go sources
	gofmt -w .

run-dev: ## Run the agent in development mode on 127.0.0.1:8765
	$(GO) run -ldflags '$(LDFLAGS)' ./cmd/skep-agent -dev

# The web interface needs Node.js and npm. It is not part of "all".
web-install: ## Install the web interface's dependencies
	cd web && npm install

web-check: ## Type-check and test the web interface
	cd web && npm run typecheck && npm test

web-build: ## Build the web interface into web/dist/
	cd web && npm run build

clean: ## Remove build output and the development data directory
	rm -rf $(BIN_DIR) .skep-dev
