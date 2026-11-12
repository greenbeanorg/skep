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

.PHONY: all build build-freebsd build-freebsd-test test vet lint fmt run-dev clean help

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

fmt: ## Format the Go sources
	gofmt -w .

run-dev: build ## Run skep-agent and skep-api in development mode (API on 127.0.0.1:8443)
	./scripts/run-dev.sh

clean: ## Remove build output and the development data directory
	rm -rf $(BIN_DIR) .skep-dev
