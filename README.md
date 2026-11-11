# Skep

Skep is an open-source virtualization and infrastructure management platform
built on the FreeBSD® operating system. It manages virtual machines (bhyve,
with QEMU as an optional secondary engine), jails, OpenZFS storage, and
networking on single hosts and, later, multi-node clusters, through a REST
API, a web UI, and a CLI.

**Status: 0.1.0 "Foundation".** This release contains the host agent,
read-only host discovery, the first three API endpoints, and the local
database schema. It cannot create or manage anything yet.

> **Security notice for 0.1.0:** the API has **no authentication and no TLS**.
> The agent binds to `127.0.0.1` by default and refuses to start on any other
> address unless you explicitly set `api.allow_insecure_remote`. Do not expose
> it to a network. See [docs/security/overview.md](docs/security/overview.md).

## What works today

| Area | State |
|---|---|
| `skep-agent` | Starts, discovers the host, serves the API, shuts down cleanly |
| Host discovery | Read-only: CPU, memory, OS, bhyve, QEMU, ZFS (FreeBSD native, Linux dev mode) |
| API | `GET /api/v1/health`, `GET /api/v1/system`, `GET /api/v1/nodes/{id}/hardware` |
| Database | SQLite with versioned migrations and a stable node ID; separate schemas for the agent and the future API |
| Not yet | VM or jail management, storage or network changes, authentication, TLS, UI, CLI, clustering |

## Platforms

- **Production host:** FreeBSD 15.x, amd64. The qualification target is
  FreeBSD 15.1-RELEASE.
- **Development and CI:** Linux. A Linux build is a development build only.
  It runs the agent with Linux discovery so you can work on the API, but it
  is never a Skep host: bhyve is always reported unavailable there.

## Requirements

- Go 1.26 or newer. `go.mod` pins toolchain `go1.27.4`; with the default
  `GOTOOLCHAIN=auto`, an older Go downloads that toolchain automatically.
- GNU Make and Git.
- No C compiler is needed. Skep builds with `CGO_ENABLED=0`.

## Build

```sh
make build          # development binary for this machine: bin/skep-agent
make build-freebsd  # FreeBSD amd64 binary: bin/freebsd-amd64/skep-agent
```

The version, commit, and build date are injected with `-ldflags`:

```sh
$ bin/skep-agent -version
skep-agent 0.1.0 (commit bba5d75, built 2026-11-11T20:23:35Z, go1.27.4 linux/amd64)
```

Override the version with `make build VERSION=0.1.1`.

## Run in development mode

```sh
make run-dev
```

This runs the agent with `-dev`: state goes to `./.skep-dev/`, logs are
human-readable text at debug level, and the API listens on
`127.0.0.1:8765`. Then, in another terminal:

```sh
curl -s http://127.0.0.1:8765/api/v1/health
curl -s http://127.0.0.1:8765/api/v1/system

# The hardware endpoint takes the node_id reported by /system.
ID=$(curl -s http://127.0.0.1:8765/api/v1/system | sed -E 's/.*"node_id":"([^"]+)".*/\1/')
curl -s http://127.0.0.1:8765/api/v1/nodes/$ID/hardware
```

Stop the agent with Ctrl-C. `make clean` removes `bin/` and `.skep-dev/`.

## Test

```sh
make test   # unit tests
make vet    # go vet for Linux and for the FreeBSD target
make lint   # gofmt check, vet, and staticcheck if it is installed
```

The unit tests replay fixtures captured from a real FreeBSD 15.1 host, so the
FreeBSD discovery logic is tested on Linux. That tests the parsing and
decision logic. It does not prove runtime behaviour on FreeBSD; see below.

## Cross-compile and validate on FreeBSD

```sh
make build-freebsd
scp bin/freebsd-amd64/skep-agent hearst:/tmp/skep-agent
ssh hearst '/tmp/skep-agent -data-dir /tmp/skep-data'
```

A successful cross-compile proves that the code compiles for FreeBSD and
nothing more. FreeBSD behaviour counts as validated only after it has run on
FreeBSD. The current validation state is tracked in
[docs/architecture/overview.md](docs/architecture/overview.md#freebsd-validation-status).

## Configuration

Configuration is layered, lowest precedence first: built-in defaults, a JSON
file, environment variables, command-line flags.

| Setting | JSON key | Environment | Flag | Default (production / dev) |
|---|---|---|---|---|
| Config file | | `SKEP_CONFIG` | `-config` | `/usr/local/etc/skep/agent.json` if present |
| Dev mode | `dev_mode` | `SKEP_DEV` | `-dev` | `false` |
| Listen address | `api.listen_address` | `SKEP_LISTEN` | `-listen` | `127.0.0.1:8765` |
| Allow non-loopback bind | `api.allow_insecure_remote` | | | `false` |
| State directory | `data_dir` | `SKEP_DATA_DIR` | `-data-dir` | `/var/db/skep` / `./.skep-dev` |
| Agent database file | `database.path` | | | `<data_dir>/agent/agent.db` |
| Log level | `log.level` | `SKEP_LOG_LEVEL` | `-log-level` | `info` / `debug` |
| Log format | `log.format` | `SKEP_LOG_FORMAT` | `-log-format` | `json` / `text` |
| Command timeout | `discovery.command_timeout` | | | `5s` |
| Discovery cache | `discovery.cache_ttl` | | | `10s` |

A complete example with every key at its production default is in
[packaging/etc/agent.json.sample](packaging/etc/agent.json.sample). Unknown
keys in the file are rejected.

## API

The API lives under `/api/v1` and is documented in
[api/openapi.yaml](api/openapi.yaml). Errors always have this shape:

```json
{"error":{"code":"not_found","message":"the requested resource does not exist","request_id":"bffe76d4cd48e82bd841254b"}}
```

## Repository layout

```
cmd/skep-agent/        the host agent binary
internal/agent/        service layer (business logic, no HTTP)
internal/api/          HTTP handlers and middleware (no business logic)
internal/hardware/     read-only host discovery (FreeBSD native, Linux dev)
internal/database/     SQLite access, migration runner, node identity
internal/config/       typed configuration
internal/logging/      log/slog setup
internal/peercred/     Unix socket peer credentials (for the agent socket; not used yet)
migrations/sqlite/     versioned SQL migrations, one set per database (agent, api)
tests/integration/     manual probes that run on a real FreeBSD host
pkg/models/            public API data types
pkg/version/           build metadata
api/openapi.yaml       API description
docs/                  architecture, security, and decision records
```

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [Security overview](docs/security/overview.md)
- Decision records:
  [0001 Go toolchain and SQLite driver](docs/decisions/0001-go-toolchain-and-sqlite-driver.md),
  [0002 Discovery abstraction](docs/decisions/0002-discovery-abstraction.md),
  [0003 Read-only discovery](docs/decisions/0003-read-only-discovery.md),
  [0004 Privilege boundary](docs/decisions/0004-privilege-boundary.md)

## Trademarks

FreeBSD® is a registered trademark of The FreeBSD Foundation. Skep is an
independent project and is not affiliated with or endorsed by The FreeBSD
Foundation. Other names mentioned in this repository, such as Proxmox®, are
trademarks of their respective owners and are used only to describe
compatibility or for comparison.
