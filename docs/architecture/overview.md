# Architecture overview

This document describes Skep as of 0.1.0 (Foundation). It covers what exists
now. Planned subsystems are listed in `CLAUDE.md` and are not described here
until they are built.

## Shape of the system in 0.1.0

One binary, `skep-agent`, runs on a host. It discovers the host read-only,
keeps local state in SQLite, and serves a small read-only REST API.

```
            HTTP (loopback only, no auth in 0.1.0)
                          |
        +-----------------v------------------+
        | internal/api        HTTP layer     |  routing, validation, error
        |                                    |  envelope, request IDs, logging
        +-----------------+------------------+
                          | api.Backend (interface)
        +-----------------v------------------+
        | internal/agent      service layer  |  node identity, caching,
        |                                    |  health, system document
        +--------+-------------------+-------+
                 |                   |
   hardware.Discoverer        agent.Pinger
                 |                   |
   +-------------v-----+   +---------v-----------+
   | internal/hardware |   | internal/database   |
   | read-only         |   | SQLite, migrations, |
   | discovery         |   | node identity       |
   +-------------------+   +---------------------+
```

`cmd/skep-agent` is the only place that knows about all of these packages. It
loads the configuration, builds the logger, opens and migrates the database,
resolves the node ID, and wires the layers together.

### Layer rules

- **`internal/api`** translates HTTP to calls on the `api.Backend` interface
  and back. It holds no business logic and imports neither `internal/agent`
  nor `internal/hardware`.
- **`internal/agent`** holds the logic. It knows nothing about HTTP.
- **`internal/hardware`** talks to the operating system. It knows nothing
  about the API or the database.

Because the HTTP layer depends only on `api.Backend`, moving the API into a
separate `skep-api` process later means writing a new `Backend` that calls
agents over the network. The handlers do not change.

## Host discovery

See [ADR 0002](../decisions/0002-discovery-abstraction.md) and
[ADR 0003](../decisions/0003-read-only-discovery.md).

`hardware.Discoverer` has one method, `Discover`. Each platform provides an
implementation:

| Platform | Source of facts | Role |
|---|---|---|
| FreeBSD | `sysctl(3)` through `golang.org/x/sys/unix`, file existence checks, `zpool list`, `qemu-* --version` | Production |
| Linux | `/proc`, `/sys`, `/etc/os-release`, `zpool list`, `qemu-* --version` | Development only |
| Other | Go runtime | Builds and runs; every capability is `unknown` |

Each capability (bhyve, QEMU, ZFS) reports `available`, `unavailable`, or
`unknown` together with a reason. A failed probe never fails the whole pass;
missing facts are listed in `warnings`.

How each capability is decided on FreeBSD:

- **bhyve** is `available` when the sysctl OID `hw.vmm.maxcpu` exists (the
  vmm module is loaded or compiled in) and `/usr/sbin/bhyve` exists. When vmm
  is not loaded, discovery reports whether `vmm.ko` is present, and does not
  load it.
- **QEMU** is `available` when a `qemu-system-*` binary is found. It is
  always reported with `acceleration: tcg`. There is no KVM on FreeBSD.
- **ZFS** is `available` when the sysctl OID `vfs.zfs.version.module` exists
  and the `zpool` binary is found. Pools are listed with a fixed
  `zpool list -H -p -o ...` invocation, and only when the module is already
  loaded.

The service layer caches a discovery result for `discovery.cache_ttl`
(10 seconds by default), and serialises discovery so concurrent requests
share one pass.

## Persistence

SQLite holds local node state, using the pure-Go `modernc.org/sqlite` driver
([ADR 0001](../decisions/0001-go-toolchain-and-sqlite-driver.md)). It is not
the cluster database.

- Migrations are plain SQL files in `migrations/sqlite/`, named
  `NNNN_name.sql`, embedded in the binary, and applied at startup, each in
  its own transaction. Applied versions and their SHA-256 checksums are
  recorded in `schema_migrations`.
- The runner refuses to start when an applied migration was edited
  afterwards, or when the database is newer than the binary.
- The database directory is created with mode `0700` and the file with mode
  `0600`.
- The node ID is a random UUID generated on first start and stored in the
  `nodes` table (`is_local = 1`). It does not change when the hostname does.

### Desired and observed state

Desired state and observed state never share a column. In every table,
`desired_*` columns and configuration columns hold what an administrator
asked for; `observed_*` columns hold what the agent last saw, with
`observed_at` recording when. History is kept separately in `task_events`
and the append-only `audit_log`.

In 0.1.0 only `schema_migrations` and the local row in `nodes` are written.
The other tables define the schema that later milestones build on. Expect
the schema to gain migrations as those subsystems are designed in detail.

## Configuration and logging

Configuration is a typed model (`internal/config`) loaded from defaults, a
JSON file, environment variables, and flags, then validated as a whole. JSON
was chosen because the standard library parses it; no dependency was added
for a configuration format. Logging uses `log/slog`, JSON in production and
text in dev mode.

## FreeBSD validation status

Validated means it ran on FreeBSD 15.1-RELEASE-p6 (amd64, `hearst`) and the
output was checked. Everything else is **pending FreeBSD validation**, even
though it cross-compiles and passes unit tests on Linux.

| Item | Status |
|---|---|
| Agent starts, migrates the database, serves all three endpoints, stops on SIGTERM | Validated (unprivileged user) |
| Hostname, OS name, version, release, kernel, architecture | Validated |
| CPU model, core and thread counts | Validated (2 vCPU VM, no SMT) |
| Total and available memory | Validated |
| bhyve reported unavailable when vmm is not loaded and `vmm.ko` is present | Validated |
| Discovery does not load vmm | Validated (`kldstat` before and after) |
| ZFS module detection and version | Validated |
| `zpool list` with no pools | Validated |
| Database directory `0700`, file `0600` | Validated |
| bhyve reported available when vmm is loaded; `/dev/vmmctl` detection | **Pending FreeBSD validation** |
| `zpool list` parsing with real pools | **Pending FreeBSD validation** (parser tested against a synthetic fixture) |
| ZFS reported unavailable when `zfs.ko` is not loaded | **Pending FreeBSD validation** |
| QEMU detection and version parsing | **Pending FreeBSD validation** (QEMU is not installed on the lab VM) |
| Core count on a host with SMT | **Pending FreeBSD validation** |
| Running as root, and under rc.d | **Pending FreeBSD validation** (no rc.d script yet) |
| UFS-root versus ZFS-root hosts | UFS-root validated; ZFS-root **pending FreeBSD validation** |
| arm64 | Not a target yet |

## Known limitations

- No authentication, authorization, or TLS. Loopback only.
- The local node is the only node.
- `os.release` on FreeBSD is the kernel release (`kern.osrelease`). The
  userland patch level can differ and is not reported.
- Memory "available" on FreeBSD is an estimate: free, inactive, and laundry
  pages. It does not account for the ZFS ARC, which the kernel can also
  shrink.
- CPU virtualization support (VT-x, EPT) is not probed while vmm is
  unloaded.
- Discovery searches a fixed list of directories for binaries, not `$PATH`.
