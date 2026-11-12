# Architecture overview

This document describes Skep as of 0.1.0, Milestone 4. It covers what exists
now. Planned subsystems are listed in `CLAUDE.md` and are not described here
until they are built.

## Shape of the system

Two binaries run on a host ([ADR 0004](../decisions/0004-privilege-boundary.md),
[ADR 0006](../decisions/0006-api-split-authentication-tls.md)).
`skep-api` is the only network listener and runs unprivileged. `skep-agent`
runs as root, discovers the host, keeps the node's state in SQLite, runs
tasks, and listens on a Unix socket only.

```
            HTTPS (TLS 1.3; session cookie or bearer token)
                          |
  skep-api (user skep)    |
        +-----------------v------------------+
        | internal/api        public API     |  route table, sessions (via
        |                                    |  internal/auth), validation
        +-----------------+------------------+
                          | api.Backend (interface)
        +-----------------v------------------+
        | internal/agentclient               |  HTTP+JSON over the socket,
        +-----------------+------------------+  actor headers on every call
                          |
         /var/run/skep/agent.sock  (root:skep 0660, peer UID checked)
                          |
  skep-agent (root)       |
        +-----------------v------------------+
        | internal/internalapi               |  peer allow-list, mandatory
        |   /internal/v1                     |  actor, limits
        +-----------------+------------------+
                          | internalapi.Backend (interface)
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

`cmd/skep-agent` and `cmd/skep-api` are the only places that know about
all of the packages on their side. Each loads its configuration, builds the
logger, and wires its layers together.

### Layer rules

- **`internal/api`** translates HTTP to calls on the `api.Backend` interface
  and back. It holds no business logic and imports neither `internal/agent`
  nor `internal/hardware`.
- **`internal/agentclient`** implements `api.Backend` by calling the agent.
  An unreachable agent becomes `503 agent_unavailable`; service errors
  cross the socket with their codes intact.
- **`internal/internalapi`** is the agent's listener and handlers. It
  refuses peers outside the allow-list and requests without an actor
  before any handler runs.
- **`internal/agent`** holds the logic. It knows nothing about HTTP.
- **`internal/hardware`** talks to the operating system. It knows nothing
  about the API or the database.

The public API and the internal API are separate interfaces with separate
versions (`/api/v1`, `/internal/v1`). Shared types live in `pkg/models`;
the error envelope and middleware they have in common live in
`internal/httpkit`.

Either process can start first or restart alone. In development,
`make run-dev` starts both, exactly as in production but as the developer.

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

## Tasks

Every operation the agent performs on the host runs as a task
([ADR 0005](../decisions/0005-agent-task-engine.md)). `internal/tasks` is
the engine; it depends on a `Store` interface that `internal/database`
implements.

- A task is `pending`, `running`, or one of four terminal states:
  `succeeded`, `failed`, `cancelled`, `interrupted`. A terminal task never
  changes.
- Each state change is one transaction that also writes a task event and
  the audit entry. The database enforces the legal transitions with
  triggers, independently of the engine.
- **Nothing is resumed.** At start, before anything is served, every
  unfinished task is marked `interrupted`. A graceful shutdown does the
  same for tasks it stops.
- A resource has at most one unfinished task. A second submission is
  refused with `resource_busy`; it is not queued.
- Idempotency keys make a retried submission return the original task. The
  same key with a different request, or from a different actor, is a
  conflict.
- Task types are registered in code, each with a time limit and a
  declaration of whether a running task may be cancelled.
- Every task carries a mandatory actor and writes a `requested` and a
  `finished` entry to the agent's audit log.

The only task type so far is `discovery.refresh`, which the agent submits
as `system` once at start. It runs discovery and stores the result in the
local node's `observed_*` columns. Over the API tasks can be read and
cancelled (`POST /tasks/{id}/cancel`, permission `task:cancel`); the agent
records the authenticated user as the actor of the cancellation.
Submission over the socket arrives with the first operation that needs
it.

## Persistence

SQLite holds local state, using the pure-Go `modernc.org/sqlite` driver
([ADR 0001](../decisions/0001-go-toolchain-and-sqlite-driver.md)). It is not
the cluster database.

There are two databases, each owned by one process and closed to the other
([ADR 0004](../decisions/0004-privilege-boundary.md)):

| Database | Owner | Migrations | Holds |
|---|---|---|---|
| `<data_dir>/agent/agent.db` | `skep-agent` | `migrations/sqlite/agent` | Node identity, VMs, disks, networks, containers, storage, interfaces, tasks, task events, operation audit |
| `<data_dir>/api/api.db` | `skep-api` | `migrations/sqlite/api` | Users, roles, sessions, access audit |

`internal/database` opens and migrates both and holds the agent's stores.
The stores of `api.db` are in `internal/apidb`, which only `skep-api`
links, so the agent binary contains no identity code.

- Migrations are plain SQL files named `NNNN_name.sql`, embedded in the
  binary, and applied at startup, each in its own transaction. Applied
  versions and their SHA-256 checksums are recorded in `schema_migrations`.
  The two sets are versioned independently.
- The runner refuses to start when an applied migration was edited
  afterwards, or when the database is newer than the binary.
- The directory holding a database is created with mode `0700` and the file
  with mode `0600`. The data directory above it is created `0755`, because
  the two processes share it and run as different users.
- The node ID is a random UUID generated on first start and stored in the
  `nodes` table (`is_local = 1`). It does not change when the hostname does.
- Tasks and operation audit entries carry a mandatory actor, as asserted by
  the API, and the audit log also records the kernel-verified UID of the
  caller.

### Desired and observed state

Desired state and observed state never share a column. In every table,
`desired_*` columns and configuration columns hold what an administrator
asked for; `observed_*` columns hold what the agent last saw, with
`observed_at` recording when. History is kept separately in `task_events`
and the append-only `audit_log` tables.

So far the agent writes `schema_migrations`, the local row in `nodes`,
`tasks`, `task_events`, and `audit_log`. The other tables define the schema
that later milestones build on. Expect
the schema to gain migrations as those subsystems are designed in detail.

## Configuration and logging

Configuration is a typed model (`internal/config`) loaded from defaults, a
JSON file, environment variables, and flags, then validated as a whole. JSON
was chosen because the standard library parses it; no dependency was added
for a configuration format. Logging uses `log/slog`, JSON in production and
text in dev mode.

## FreeBSD validation status

Validated means it ran on FreeBSD 15.1-RELEASE-p6 (amd64, UFS-root, `hearst`)
and the output was checked. Everything else is **pending FreeBSD validation**,
even though it cross-compiles and passes unit tests on Linux.

Last validation run: 2026-11-11, in two host states. First with vmm not
loaded and no pools; then with vmm loaded and the file-backed pool `skeptest`
imported, both as an unprivileged user and as root.

| Item | Status |
|---|---|
| Agent starts, migrates the database, serves all three endpoints, stops on SIGTERM | Validated (unprivileged and root) |
| Hostname, OS name, version, release, kernel, architecture | Validated |
| CPU model, core and thread counts | Validated (2 vCPU VM, no SMT) |
| Total and available memory | Validated |
| bhyve `unavailable` when vmm is not loaded and `vmm.ko` is present | Validated |
| bhyve `available` when vmm is loaded; `/dev/vmmctl` detected | Validated (unprivileged and root) |
| Discovery does not load or unload kernel modules | Validated (`kldstat` before and after, both states) |
| ZFS module detection and version | Validated |
| `zpool list` with no pools | Validated |
| `zpool list` with one healthy pool, including sizes and percentages | Validated (`skeptest`, unprivileged and root) |
| Discovery leaves the pool untouched | Validated (`zpool list` before and after) |
| Database directory `0700`, file `0600`, shared data directory `0755` | Validated (unprivileged and root) |
| Task engine, store, and audit test suites run natively (restart sweep, shutdown, cancellation, idempotency, concurrent submissions) | Validated (2026-11-12) |
| Startup `discovery.refresh` task, observed node state stored, audit entries written; history survives a restart and a `kill -9` | Validated (2026-11-12) |
| Interruption of a real in-flight task on a running agent, by `kill -9`, by `service stop`, and by a reboot | Validated (2026-11-12, test-only `test.sleep` task; see `tests/integration/rcd`) |
| Unix socket peer credentials (`LOCAL_PEERCRED`), UID allow-list, socket file permissions | Validated (see `tests/integration/peercred`) |
| A degraded or faulted pool; a pool reporting `-` for fragmentation | **Pending FreeBSD validation** (parser tested against a synthetic fixture) |
| ZFS `unavailable` when `zfs.ko` is not loaded | **Pending FreeBSD validation** (needs the pool exported and zfs unloaded) |
| QEMU detection and version parsing | **Pending FreeBSD validation** (QEMU is not installed on the lab VM) |
| Core count on a host with SMT | **Pending FreeBSD validation** (the lab VM has no SMT) |
| Running under rc.d as a service: start, stop, restart, status, crash supervision, start at boot | Validated (2026-11-12, Milestone 3 as `skep`, and again for Milestone 4 with the agent as root; see `tests/integration/rcd`) |
| A crash loop (agent exits at every start) and `skep_agent_restart_delay=0` under rc.d | Validated (2026-11-12, see `tests/integration/rcd`) |
| A tmpfs `/var/run` | **Pending FreeBSD validation** |
| Agent on a Unix socket only, API through the socket, peer UID check, mandatory actor: both processes as one ordinary user | Validated (2026-11-12) |
| Unit tests of the new packages run natively (`internalapi`, `agentclient`, `apitls`, `auth`, `apidb`, `api`, `privdrop`, `sysaccount`, `tests/e2e`, both commands) | Validated (2026-11-12) |
| TLS: certificate generation, file modes, TLS 1.3 only, renewal with the same key on a live server, `SIGHUP` reload, `tls regenerate`, `tls fingerprint`, operator certificates | Validated as an ordinary user (2026-11-12, see `tests/integration/apitls`) |
| Password prompt without echo on a pseudo-terminal and over SSH; `--password-stdin` | Validated (2026-11-12, see `tests/integration/adminprompt`) |
| Password prompt on the system console | **Pending FreeBSD validation** |
| Argon2id time with the default parameters | Validated: 121 to 151 ms per hash on hearst (2026-11-12) |
| Login (cookie and bearer), `401` without a session, task cancel through the API with the user recorded by the agent | Validated as an ordinary user (2026-11-12) |
| Agent socket under `/var/run/skep` with a root agent and the real `skep` account (`root:skep 0660`; refusal by file permissions and by the peer check); ownership change of `/var/db/skep/agent` from `skep` to root on upgrade | Validated (2026-11-12, see `tests/integration/rcd`) |
| `skep-api` under rc.d as `skep`, binding its port without privileges, generating its key as `skep`, start at boot; `skep-api admin` started as root giving up root | Validated (2026-11-12, same) |
| Task cancel through the API under rc.d: the agent records the user as the actor and uid 1002 (`skep`) as the verified peer | Validated (2026-11-12, same) |
| ZFS-root host | **Pending FreeBSD validation** (the lab VM is UFS-root) |
| arm64 | Not a target yet |

## Known limitations

- Permissions are global: there is no per-resource scoping and no custom
  roles yet.
- Sessions and users are per node.
- The local node is the only node.
- Tasks, task events, and audit entries are never removed. Retention is not
  designed yet, which is why the inventory is refreshed only at start and
  not periodically.
- `os.release` on FreeBSD is the kernel release (`kern.osrelease`). The
  userland patch level can differ and is not reported.
- Memory "available" on FreeBSD is an estimate: free, inactive, and laundry
  pages. It does not account for the ZFS ARC, which the kernel can also
  shrink.
- CPU virtualization support (VT-x, EPT) is not probed while vmm is
  unloaded.
- Discovery searches a fixed list of directories for binaries, not `$PATH`.
