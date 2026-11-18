# Skep

Skep is an open-source virtualization and infrastructure management platform
built on the FreeBSD® operating system. It manages virtual machines (bhyve,
with QEMU as an optional secondary engine), jails, OpenZFS storage, and
networking on single hosts and, later, multi-node clusters, through a REST
API, a web UI, and a CLI.

**Status: 0.1.0, Milestone 7 phase 7a "Storage".** The code contains the
host agent, the API server with TLS, accounts, sessions, and permissions,
the `skepctl` command-line client, the web interface, read-only host
discovery, a task engine with history, two audit logs, bhyve virtual
machines (Milestone 6), and storage management: snapshots, rollback,
clones, disk changes, directory pools, and resource monitoring (phase 7a).
It cannot give jails a network interface yet (phase 7b, step 8), and it does not manage host networks.

> **Security notice:** this is pre-release software. `skep-api` listens on
> loopback by default; opening it to a network is an explicit
> configuration change. See
> [docs/security/overview.md](docs/security/overview.md) for what is and is
> not protected.

## What works today

| Area | State |
|---|---|
| `skep-agent` | Runs as root with no network listener; discovers the host, runs tasks, and serves the internal API on a Unix socket to `skep-api` only |
| `skep-api` | Runs unprivileged, serves the public API on loopback over TLS 1.3, and reaches the host only through the agent socket |
| TLS | Self-signed certificate generated at first start and renewed with the same key, or an operator-supplied certificate reloaded on change and on `SIGHUP`; no plaintext listener |
| Host discovery | Read-only: CPU, memory, OS, bhyve, QEMU, ZFS (FreeBSD native, Linux dev mode) |
| Node API | `GET /api/v1/health`, `/system`, `/nodes/{id}/hardware`, `/tasks`, `/tasks/{id}`; `POST /tasks/{id}/cancel`, the first operation that reaches the agent on behalf of a user |
| Task engine | Persistent tasks with states, events, idempotency keys, cancellation, and an audit trail; nothing is resumed after a restart |
| FreeBSD services | rc.d scripts for both processes, supervised, started at boot |
| Database | SQLite with versioned migrations and a stable node ID; separate databases for the agent and the API |
| Users | Accounts with Argon2id password hashes and three built-in roles; the first administrator is created with a local command (`skep-api admin create`), never with default credentials |
| Sessions | Login with a cookie session (browsers) or a bearer session (scripts), never both; idle and absolute timeouts; logout; login rate limits that never lock an account; every endpoint except the health status and the login needs a session |
| Permissions | One role per user; every endpoint requires a permission and is denied by default; `admin`, `operator`, and `viewer` are built in; every denial is audited |
| User management | `GET/POST /users`, `GET/PUT/DELETE /users/{id}`, `GET /roles`, `GET /audit`; the last administrator cannot be removed |
| Retention | Finished tasks are deleted after 90 days; both audit logs are sealed monthly into hash-chained archive segments, and an entry leaves the live table only when it is in one (never by default for the agent's log, after 90 days for the access log); `audit verify` and `audit export` on the host |
| `skepctl` | Command-line client on the public API: bearer-session login with the token in a `0600` file, the server's public key pinned on first use and never skipped, logout, read-only `system`, `hardware`, `tasks`, and `audit` commands, `vm` (Milestone 6), and `vm snapshot` and `vm clone` (phase 7a) |
| Web interface | Served by `skep-api` on the same address as the API, embedded in the binary; cookie-session login; dashboard, node, tasks (with cancel), virtual machines (with the serial console and snapshots), storage, users, roles, access audit log, settings, own account; the other sections are placeholders that name their milestone |
| Virtual machines (Milestone 6) | bhyve UEFI guests with zvol disks, bridged virtio-net, and install media: create, update while stopped, delete, start, stop, force-stop, restart, all as tasks; a per-VM runner that survives the agent; the serial console in the browser; `skepctl vm`. Host confinement in `agent.json`. **Verified on FreeBSD 15.1** (`hearst`): every check of ADR 0009 decision 11 passed (guest network isolation after a fix to the lab firewall) (see [tests/integration/vm](tests/integration/vm/README.md) and the [guide](docs/administration/virtual-machines.md)) |
| Storage (Milestone 7, phase 7a) | Snapshots of VMs (also while running, crash-consistent), rollback, linked clones, disks added, grown, and removed after creation, directory pools for hosts without ZFS, the dataset inventory, current resource use (`GET /metrics`) with the `storage_capacity` and `storage_locked` health checks; the Storage section and a VM's snapshots in the web interface; `skepctl vm snapshot` and `vm clone`. **Verified on FreeBSD 15.1** (`hearst`): every 7a check of ADR 0010 decision 16 (see [tests/integration/storage](tests/integration/storage/README.md) and the [guide](docs/administration/storage.md)) |
| Jail templates (Milestone 7, phase 7b, in progress) | `GET/POST /storage/templates`, `GET/DELETE /storage/templates/{id}`, `GET /storage/template-sources` (admin-only builds): a FreeBSD userland built from the host's pkgbase repositories with package scripts and triggers off, the scripts' known commands run in a throwaway no-network build jail, every script and trigger checked against a compiled allowlist, and the host's `/etc`, `/var/db/pkg`, and `/usr/share/certs` checked unchanged; or from an operator-placed `base.txz`. The pkgbase builder **ran on FreeBSD 15.1** (`hearst`), through its test binary and through the deployed agent; the `dist` source is **pending FreeBSD validation** (see the [guide](docs/administration/jail-templates.md)) |
| Jails (Milestone 7, phase 7b, in progress) | `GET/POST /containers`, `GET/PUT/DELETE /containers/{id}`, `POST /containers/{id}/actions`, `GET /containers/{id}/console-log`: jails on ZFS clones of a template, run by jail(8) from a generated `jail.conf` with a fixed baseline (VNET, securelevel 2, every `allow.*` off but `suser` and `reserved_ports`), checked against the kernel after every start; root's SSH key and sshd written before the first start through `O_RESOLVE_BENEATH` opens, never after. **Verified on FreeBSD 15.1** (`hearst`) for step 7 (see [tests/integration/jails](tests/integration/jails/README.md) and the [guide](docs/administration/jails.md)). No network interface yet (step 8), no limits, snapshots, or clones (step 9), no `skepctl` yet (step 10); the web UI's Containers section is built (step 10, tested with a fake API, pending validation on `hearst`) |
| Not yet | Jail networking, limits, snapshots, and their UI (the rest of phase 7b), network management, graphical consoles, metric history, clustering |

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
make build             # the web interface, then the binaries for this machine with it embedded: bin/skep-agent, bin/skep-api, bin/skepctl
make build-freebsd     # the same for FreeBSD amd64, in bin/freebsd-amd64/
make build-go          # Go only, no Node.js: skep-api then serves a placeholder page instead of the interface
make build-freebsd-go  # the same for FreeBSD amd64
```

`make build` and `make build-freebsd` need Node.js and npm for the web
interface ([web/README.md](web/README.md)). The Go-only targets and
`go build ./...` do not.

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

This builds the Go binaries (without the web interface; in development the
interface is served by Vite, see [web/README.md](web/README.md)) and starts
both processes with `-dev`, as you: state goes to
`./.skep-dev/`, logs are human-readable text at debug level, the agent
listens on the Unix socket `.skep-dev/run/agent.sock` (which only your own
UID may open), and the API listens on `127.0.0.1:8443` with a self-signed
certificate it generates in `.skep-dev/api/tls/`. There is no combined
binary and no flag that turns a security control off. Then, in another
terminal:

```sh
# There are no default credentials: create an administrator first.
bin/skep-api admin create --username andy -dev

# Trust exactly the certificate this skep-api generated.
CA=.skep-dev/api/tls/cert.pem
API=https://127.0.0.1:8443/api/v1

# Without a session only the health status is readable: ok or degraded
# (HTTP 200, still serving), or unavailable (HTTP 503).
curl -s --cacert $CA $API/health

# Log in for a bearer session (scripts) and keep the token.
TOKEN=$(curl -s --cacert $CA $API/auth/login -H 'Content-Type: application/json' \
    -d '{"username":"andy","password":"YOUR PASSWORD","session":"bearer"}' |
    sed -E 's/.*"token":"([^"]+)".*/\1/')
AUTH="Authorization: Bearer $TOKEN"

curl -s --cacert $CA -H "$AUTH" $API/system

# The hardware endpoint takes the node_id reported by /system.
ID=$(curl -s --cacert $CA -H "$AUTH" $API/system | sed -E 's/.*"node_id":"([^"]+)".*/\1/')
curl -s --cacert $CA -H "$AUTH" $API/nodes/$ID/hardware

# The agent refreshes its inventory at start, as a task.
curl -s --cacert $CA -H "$AUTH" "$API/tasks?limit=5"

curl -s --cacert $CA -H "$AUTH" -X POST $API/auth/logout

# The public key pin of the certificate, to compare out of band.
bin/skep-api tls fingerprint -dev
```

Stop both with Ctrl-C. `make clean` removes `bin/` and `.skep-dev/`.

## Test

```sh
make test   # unit tests with the race detector (needs a C compiler)
make vet    # go vet for Linux and for the FreeBSD target
make lint   # gofmt check, vet, and staticcheck if it is installed
make install-hooks   # once per clone: a pre-push hook that runs make lint
```

The pre-push hook lints the commits being pushed, not the working tree,
and refuses the push if `make lint` fails on any of them. Install
`staticcheck` (`go install honnef.co/go/tools/cmd/staticcheck@latest`) so
that the hook checks what CI checks; without it that part is skipped with
a message.

The unit tests replay fixtures captured from a real FreeBSD 15.1 host, so the
FreeBSD discovery logic is tested on Linux. That tests the parsing and
decision logic. It does not prove runtime behaviour on FreeBSD; see below.

## Cross-compile and validate on FreeBSD

```sh
make build-freebsd
scripts/deploy-test.sh hearst          # stage, check size and SHA-256, install root-owned, sync
ssh hearst 'sudo service skep_agent restart; sudo service skep_api restart'
```

With the host alone, the script installs where the package will:
`skep-agent` and `skep-api` in `/usr/local/sbin`, `skepctl` in
`/usr/local/bin`, the paths the rc.d scripts use by default. With a
directory, it installs the files named after it there instead (test
binaries, or `agent.json` into `/usr/local/etc/skep`). Stop the services
before replacing their binaries.

`scripts/deploy-test.sh` installs with `sudo`: programs `root:wheel 0555`,
`agent.json` `root:wheel 0600`, into a directory it makes `root:wheel 0755`.
The agent runs as root, so nothing it executes or reads as configuration
may be writable by another user. The script warns when a directory above
the target is not root's, since its owner can replace the target. It ends
with `sync` on the host. Without it, a hard
reset shortly after a copy can leave an empty binary behind: that happened
on `hearst` during Milestone 6 (see
[tests/integration/vm](tests/integration/vm/results/step3-first-start-FAILED.txt)).

A successful cross-compile proves that the code compiles for FreeBSD and
nothing more. FreeBSD behaviour counts as validated only after it has run on
FreeBSD. The current validation state is tracked in
[docs/architecture/overview.md](docs/architecture/overview.md#freebsd-validation-status).

To run Skep as services, see
[docs/installation/freebsd-service.md](docs/installation/freebsd-service.md).
The native service test is in
[tests/integration/rcd](tests/integration/rcd/README.md), and the native
retention test in
[tests/integration/retention](tests/integration/retention/README.md).

## Web interface

The web interface is in [web/](web/README.md) and is served by `skep-api`
on the same address as the API, from files embedded in the binary. Open
`https://` plus the listen address in a browser and log in with the
administrator created by `skep-api admin create`. How to check the
certificate on the first visit, and what the interface does, is in
[docs/administration/web-ui.md](docs/administration/web-ui.md).

## Configuration

Configuration is layered, lowest precedence first: built-in defaults, a JSON
file, environment variables, command-line flags.

`skep-agent` (`/usr/local/etc/skep/agent.json`):

| Setting | JSON key | Environment | Flag | Default (production / dev) |
|---|---|---|---|---|
| Config file | | `SKEP_CONFIG` | `-config` | `/usr/local/etc/skep/agent.json` if present |
| Dev mode | `dev_mode` | `SKEP_DEV` | `-dev` | `false` |
| Socket path | `socket.path` | `SKEP_SOCKET` | `-socket` | `/var/run/skep/agent.sock` / `<data_dir>/run/agent.sock` |
| Socket group | `socket.group` | | | `skep` (mode `0660`) / none (mode `0600`) |
| Peers allowed to connect | `socket.allowed_users` | | | `["root", "skep"]` / your own UID |
| Request time limit | `socket.request_timeout` | | | `30s` |
| Connection limit | `socket.max_connections` | | | `64` |
| Shutdown time limit | `shutdown_timeout` | | | `10s` |
| State directory | `data_dir` | `SKEP_DATA_DIR` | `-data-dir` | `/var/db/skep` / `./.skep-dev` |
| Agent database file | `database.path` | | | `<data_dir>/agent/agent.db` |
| Log level | `log.level` | `SKEP_LOG_LEVEL` | `-log-level` | `info` / `debug` |
| Log format | `log.format` | `SKEP_LOG_FORMAT` | `-log-format` | `json` / `text` |
| Command timeout | `discovery.command_timeout` | | | `5s` |
| Discovery cache | `discovery.cache_ttl` | | | `10s` |
| Task workers | `tasks.workers` | | | `4` |
| Task queue size | `tasks.queue_size` | | | `64` |
| Task retention | `retention.tasks_days` | | | `90` days after a task finished; `0` keeps tasks forever, otherwise at least `7` |
| Audit entries kept in the database | `retention.audit_live_days` | | | `0`: sealed, never removed; otherwise at least `35` days after the end of their month |
| Audit archive | `retention.audit_archive` | | | `true`; `false` seals nothing and never removes an audit entry |
| UEFI firmware directory | `hypervisor.bhyve.firmware_dir` | | | `/usr/local/share/edk2-bhyve` |
| Memory left to the host | `hypervisor.bhyve.host_memory_reserve_bytes` | | | `1073741824` (1 GiB) |
| CPUs per VM | `hypervisor.bhyve.max_vcpus_per_vm` | | | `0`: the lower of the host's CPUs and `hw.vmm.maxcpu` |
| VM watch interval | `hypervisor.bhyve.watch_interval` | | | `2s` |
| Pools VMs may use | `storage.pools` | | | none: no VM can be created |
| Directory pools VMs may use | `storage.directories` | | | none; each is `{"name", "path"}`, the path `root` `0700` on local UFS or ZFS |
| Dataset root | `storage.dataset_root` | | | `skep`: everything under `<pool>/skep` |
| Install media directory | `storage.media_dir` | | | `<data_dir>/media` |
| Snapshots per VM | `storage.max_snapshots_per_resource` | | | `64` (at most 1000) |
| Capacity warning, failure | `storage.capacity_warn_percent`, `storage.capacity_fail_percent` | | | `80`, `90`: the `storage_capacity` health check; 1 ≤ warn < fail ≤ 100 |
| Bridges NICs may join | `network.bridges` | | | none: no VM can have a NIC |
| Management interfaces | `network.management_interfaces` | | | none; a bridge with one as a member is refused |

`skep-api` (`/usr/local/etc/skep/api.json`):

| Setting | JSON key | Environment | Flag | Default (production / dev) |
|---|---|---|---|---|
| Config file | | `SKEP_API_CONFIG` | `-config` | `/usr/local/etc/skep/api.json` if present |
| Dev mode | `dev_mode` | `SKEP_DEV` | `-dev` | `false` |
| Listen address | `listen_address` | `SKEP_API_LISTEN` | `-listen` | `127.0.0.1:8443`; set the management address to open the API to a network |
| Agent socket | `agent_socket` | `SKEP_SOCKET` | `-agent-socket` | `/var/run/skep/agent.sock` / `<data_dir>/run/agent.sock` |
| State directory | `data_dir` | `SKEP_DATA_DIR` | `-data-dir` | `/var/db/skep` / `./.skep-dev` |
| Timeouts | `read_timeout`, `write_timeout`, `shutdown_timeout` | | | `15s`, `1m`, `10s` |
| API database file | `database.path` | | | `<data_dir>/api/api.db` |
| Operator certificate | `tls.cert_file`, `tls.key_file` | | | empty: generate a self-signed certificate in `<data_dir>/api/tls/` |
| Argon2id memory | `auth.argon2.memory_kib` | | | `65536` (64 MiB); 19456 to 1048576 |
| Argon2id passes | `auth.argon2.passes` | | | `3`; 2 to 16 |
| Session idle timeout | `auth.session.idle_timeout` | | | `30m`; at most `24h` |
| Session absolute timeout | `auth.session.absolute_timeout` | | | `12h`; at most `720h` |
| Login attempts per address | `auth.login.per_address_per_minute` | | | `10` |
| Failures before a delay | `auth.login.failures_before_delay` | | | `5` (per username and address; the delay doubles from 1 s) |
| Longest login delay | `auth.login.max_delay` | | | `1m` |
| Failures per username | `auth.login.per_username_failures` | | | `50` in 15 minutes, across all addresses |
| Trusted reverse proxies | `auth.trusted_proxies` | | | none: `X-Forwarded-For` is ignored |
| Access audit entries kept in the database | `retention.audit_live_days` | | | `90` days after the end of their month, once sealed; `0` never removes; otherwise at least `35` |
| Access audit archive | `retention.audit_archive` | | | `true`; `false` seals nothing and never removes an entry |
| Log level | `log.level` | `SKEP_LOG_LEVEL` | `-log-level` | `info` / `debug` |
| Log format | `log.format` | `SKEP_LOG_FORMAT` | `-log-format` | `json` / `text` |

Complete examples with every key at its production default are in
[packaging/etc](packaging/etc). Unknown keys in a file are rejected.

### Retention

Finished tasks are deleted with their events after 90 days. Audit entries
are different: every month each process seals the previous month of its
audit log into an archive segment in `audit-archive/` next to its
database, and an entry is removed from the database only when it is in a
sealed, verified segment. Skep never deletes a segment file. See
[ADR 0007](docs/decisions/0007-retention.md) and the
[service guide](docs/installation/freebsd-service.md#retention-and-the-audit-archive).

```sh
skep-agent audit verify            # check the index and every segment file
skep-agent audit export --month 2026-10   # archived entries as JSON Lines
skep-api audit verify              # the same for the access audit log
```

### TLS

`skep-api` speaks TLS 1.3 only and has no plaintext listener.

- **Without configuration** it generates an ECDSA P-256 key and a
  self-signed certificate (397 days; names: the hostname, `localhost`, and
  the host's addresses at that moment) at first start. It renews the
  certificate 30 days before expiry **with the same key**, without a
  restart, so a client that pinned the key keeps working.
- `skep-api tls fingerprint` prints the SHA-256 of the public key, the
  value to pin or compare. `skep-api tls regenerate` is the only way to get
  a new key; it also picks up a changed hostname or address. Both are run
  on the host, as root or as the service account.
- **With `tls.cert_file` and `tls.key_file`** it serves that pair, reloads
  it when the files change or on `SIGHUP`, refuses a pair that does not
  match (the old one stays in use), and never writes to the files. It warns
  from 30 days before expiry. `Strict-Transport-Security` is sent only in
  this mode.

### Administrators

There are no default credentials. The first administrator is created on
the host, with a command that has no network path:

```sh
skep-api admin create --username andy          # asks for the password twice, without echo
skep-api admin reset-password --username andy  # recovery for a lost password; ends that user's sessions
skep-api admin disable --username carol        # disable a user; ends that user's sessions
skep-api admin delete --username carol --yes   # delete a user; ends that user's sessions
skep-api admin hash-benchmark                  # how long one password hash takes on this host
```

Run them as root (they change to the service account before opening
anything) or as `skep`; in development add `-dev`. The password is read
from the terminal. For unattended installs `--password-stdin` reads it
from standard input; it is never accepted as an argument or from the
environment. `admin create` works at any time, not only for the first
administrator. `disable` and `delete` refuse the only enabled
administrator, as the API does. Every one of these commands is written to
the access audit log with the actor `local:<uid>`.

Passwords are 12 to 256 characters, with no composition rules, and are
stored as Argon2id hashes (64 MiB, 3 passes, 1 lane by default).

### skepctl

`skepctl` is the command-line client. It talks only to the public API, from
the host or from any machine that can reach it:

```sh
skepctl login --server 10.79.20.170 --username andy   # shows the key and asks before trusting it
skepctl system
skepctl hardware
skepctl tasks list --status running
skepctl tasks show TASK_ID
skepctl audit list --limit 20
skepctl logout
```

On first contact it shows the server's public key pin; compare it with
`skep-api tls fingerprint` on the host before answering. A changed key is
refused until it is named with `--trust-key`. For a server with an
operator-supplied certificate, `--ca-file FILE` or `--system-roots`
verifies the chain and host name instead, so renewals with a new key keep
working. There is no option to skip verification. The token and the pins are kept in `~/.config/skepctl`, `0600`.
See [docs/administration/skepctl.md](docs/administration/skepctl.md).

## API

The API lives under `/api/v1` and is documented in
[api/openapi.yaml](api/openapi.yaml). Errors always have this shape:

```json
{"error":{"code":"not_found","message":"the requested resource does not exist","request_id":"bffe76d4cd48e82bd841254b"}}
```

## Repository layout

```
cmd/skep-agent/        the host agent binary (root, Unix socket only)
cmd/skep-api/          the API server binary (unprivileged, the only network listener)
cmd/skepctl/           the command-line client (public API only)
internal/agent/        the agent's service layer (business logic, no HTTP)
internal/api/          public API handlers (no business logic)
internal/internalapi/  the agent's internal API: socket listener, peer check, handlers
internal/agentclient/  skep-api's client for the agent socket
internal/agentproto/   what both sides share about the internal API (actor headers)
internal/httpkit/      error envelope, request IDs, access log, recovery
internal/auth/         password hashing (Argon2id), users and roles, sessions, login limits
internal/apidb/        storage of api.db: users, roles, sessions, access audit
internal/termpass/     reading a password from the terminal without echo
internal/apitls/       skep-api's certificate: generation, renewal, reload
internal/privdrop/     giving up root in skep-api's local commands
internal/hardware/     read-only host discovery (FreeBSD native, Linux dev)
internal/database/     SQLite access, migration runner, node identity, the agent's stores
internal/config/       typed configuration
internal/logging/      log/slog setup
internal/tasks/        task engine (states, cancellation, idempotency, restart rules)
internal/audit/        operation audit entries
internal/auditarchive/ audit archive segments: seal, verify, remove
internal/auditcmd/     the audit verify and audit export commands
internal/retention/    the maintenance.retention task
internal/peercred/     Unix socket peer credentials, read from the kernel
internal/sysaccount/   user and group name resolution
internal/ctlstate/     skepctl's pins and session tokens on disk
internal/webui/        serves the web interface from skep-api (embedded with the build tag skep_webui)
web/                   the web interface: React, TypeScript, Vite, Tailwind CSS
packaging/             rc.d scripts and sample configuration
scripts/               development helpers
migrations/sqlite/     versioned SQL migrations, one set per database (agent, api)
tests/integration/     native tests that run on a real FreeBSD host
tests/e2e/             agent and API together in one process
pkg/client/            Go client for the public API, with public key pinning
pkg/models/            public API data types
pkg/version/           build metadata
api/openapi.yaml       API description
docs/                  architecture, security, and decision records
```

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [Security overview](docs/security/overview.md)
- [Running Skep as FreeBSD services](docs/installation/freebsd-service.md)
- [skepctl](docs/administration/skepctl.md)
- [The web interface](docs/administration/web-ui.md)
- [Virtual machines](docs/administration/virtual-machines.md)
- [Storage](docs/administration/storage.md)
- [Jail templates](docs/administration/jail-templates.md)
- [Jails](docs/administration/jails.md)
- Decision records:
  [0001 Go toolchain and SQLite driver](docs/decisions/0001-go-toolchain-and-sqlite-driver.md),
  [0002 Discovery abstraction](docs/decisions/0002-discovery-abstraction.md),
  [0003 Read-only discovery](docs/decisions/0003-read-only-discovery.md),
  [0004 Privilege boundary](docs/decisions/0004-privilege-boundary.md),
  [0005 Agent task engine](docs/decisions/0005-agent-task-engine.md),
  [0006 API split, authentication, and TLS](docs/decisions/0006-api-split-authentication-tls.md),
  [0007 Retention](docs/decisions/0007-retention.md),
  [0008 The first web interface](docs/decisions/0008-web-ui.md),
  [0009 The first working VM on bhyve](docs/decisions/0009-first-vm-bhyve.md),
  [0010 Storage management and jails](docs/decisions/0010-storage-and-jails.md)

## Trademarks

FreeBSD® is a registered trademark of The FreeBSD Foundation. Skep is an
independent project and is not affiliated with or endorsed by The FreeBSD
Foundation. Other names mentioned in this repository, such as Proxmox®, are
trademarks of their respective owners and are used only to describe
compatibility or for comparison.
