# Architecture overview

This document describes Skep as of 0.1.0, Milestone 5. It covers what exists
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
        |   /api/v1                          |  internal/auth), validation
        | internal/webui      everything     |  the embedded web interface:
        |   outside /api                     |  static files, no data
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

Clients sit in front of `skep-api` and use only `/api/v1`. The first is
`skepctl` (`cmd/skepctl`), built on the Go client in `pkg/client`, which
pins the server's public key on every connection; its local state (pins and
session tokens) is in `internal/ctlstate`. See
[docs/administration/skepctl.md](../administration/skepctl.md).

The second is the web interface (`web/`,
[ADR 0008](../decisions/0008-web-ui.md)). `skep-api` serves its built
files on every path outside `/api`, from the same origin as the API, so the
cookie session and the origin check of ADR 0006 apply unchanged; an unknown
path under `/api` stays a JSON 404. The files are embedded with `go:embed`
behind the build tag `skep_webui`, which `make build` and
`make build-freebsd` set after building the interface with Node.js; a
build without the tag (`go build ./...`, `make build-go`) carries a
placeholder page that says so, so Go builds and tests need no Node.js.
`internal/webui` sets the interface's own headers (a CSP without inline
script or style, `Referrer-Policy`, COOP) and caches only the hashed
files. Nothing it serves is data, and nothing it serves is written to the
access audit log. The one server-side addition for the interface is the
`X-Skep-Background` header: a request that carries it is authenticated
like any other but does not extend the session's idle timeout, so an
unattended page idles out. See
[docs/administration/web-ui.md](../administration/web-ui.md).

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
and the `audit_log` tables.

### Retention

Nothing grows without bound except what the operator chooses to keep
([ADR 0007](../decisions/0007-retention.md)).

| Data | Rule | Default |
|---|---|---|
| Finished tasks and their events | Deleted by age; the newest finished task of each resource is kept | 90 days (`retention.tasks_days`, floor 7, `0` keeps them) |
| Agent operation audit | Sealed monthly into `agent/audit-archive/`; live entries removed only if the operator sets a number of days | Sealed, never removed (`retention.audit_live_days: 0`) |
| API access audit | Sealed monthly into `api/audit-archive/`; live entries removed after their segment is sealed and re-verified | 90 days after the end of the month |
| Archive segments | Never deleted by Skep | |

An audit entry leaves the live table only when it is inside a sealed
segment: a trigger in each database refuses every other delete. Segments
are ranges of entry IDs, written as gzip-compressed JSON Lines, and form
a hash chain (`internal/auditarchive`).

The agent does this in a daily task, `maintenance.retention`, whose own
audit entry records what was removed. `skep-api` has no task engine; a
timer does the same for the access log and records each run as an access
audit entry (`audit.archive`). Both report a broken archive, or a month
still unsealed 7 days into the next, through the health check. The check
is not critical: health becomes `degraded`, not `unavailable`, and the
health endpoint still answers `200` (only a failed critical check, the
agent or its database unreachable, answers `503`).

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

## Virtual machines

Milestone 6 ([ADR 0009](../decisions/0009-first-vm-bhyve.md)), from the
request to the guest:

```
skep-api  /api/v1/vms, /vms/{id}/actions, /vms/{id}/console/serial
   |  (Unix socket: actor headers, peer UID checked)
skep-agent
   internal/vm          validation, tasks (vm.create ... vm.restart), memory budget,
                        the watcher that writes observed state every 2 s
   internal/hypervisor  the engine interface; bhyve is the only adapter
   internal/console     the console broker: nmdm B side, 64 KiB scrollback, 4 clients
   internal/hostops     every privileged call, bounded by the confinement in agent.json
      |
      +-- zfs/ifconfig/bhyvectl (exec, fixed paths)
      +-- skep-agent vm-runner (one per running VM, own session)
             taps -> bhyve -k bhyve.conf skep-<uuid>   (restarted after a guest reboot)
```

- **State.** Desired state (`desired_state`, the configuration) is written
  by tasks acting for a user; observed state (`observed_*`) only by the
  watcher. Nothing reconciles them: a difference is shown as drift. The
  runner reports through `state.json` in `/var/run/skep/vm/<id>/`; the
  agent reads it, checks that the runner is alive and is the right
  process, and checks `/dev/vmm/skep-<uuid>`.
- **Survival.** The runner is not a child the agent depends on. An agent
  restart or crash leaves guests running; the restarted agent observes
  every VM before it serves its socket. A host reboot empties `/var/run`,
  so every VM is observed stopped.
- **Development machines** use `internal/hostops/hostfake`: pools,
  bridges, taps, and guests in memory, with the real runner loop running
  in a goroutine and a console that echoes. It never ships in a FreeBSD
  binary (CI checks the marker string).

## Storage and monitoring

Phase 7a ([ADR 0010](../decisions/0010-storage-and-jails.md)):

- **Snapshots, rollback, clones** are storage operations, the same for any
  engine, so `internal/vm` calls `internal/hostops` directly for them and
  asks the adapter only whether the VM runs. Each is a task that holds the
  VM. Names are derived (`@skep-<snapshot-id>`); a destroy or rollback
  checks the `skep:` owner property of every dataset first.
- **Disks** go through `hostops.Disks`, which routes create, grow, remove,
  and delete to ZFS (zvols) or to a directory pool (raw files opened
  through `os.Root`) by the pool's kind.
- **The inventory** (`internal/storage`) lists what is under each pool's
  dataset root and judges each owner by the tree, the property, and the
  database together.
- **Monitoring** (`internal/monitoring`) samples every 30 seconds: pool
  space, the ZFS inventory (disk use, key status), directory pool files,
  and `ps` of each running VM's bhyve (after checking its
  `kern.proc.args`). It keeps the latest sample only. `GET /metrics`
  serves it, and the `storage_capacity` and `storage_locked` health probes
  read it, so a health request never runs a ZFS command.

## Jail templates

Phase 7b, step 6 ([ADR 0010](../decisions/0010-storage-and-jails.md),
decision 7 and the step 6 notes):

```
skep-api  /api/v1/storage/templates (storage:write to build or delete)
   |
skep-agent
   internal/templates   the service, the tasks, the compiled allowlist,
                        the build sequence and its clean-up, the start-up sweep
   internal/hostops     Templates: datasets and work area, pkg (scripts and
                        triggers off), the build jail (jail -c, jexec -l,
                        jail -r), the host-file record
```

- **Order and judgement in `internal/templates`, mechanics in
  `internal/hostops`.** hostops offers typed steps whose names and paths
  are derived from the pool and the template ID. The builder decides the
  order, applies the allowlist and the release rule, and compares the
  host files.
- **Clean-up** runs on its own context, so a cancelled or timed-out task
  still removes its build jail and its dataset. The agent removes any
  leftover `skep-build-*` jail at start, before it serves its socket.
- **Development machines** simulate a whole build in `hostfake`, with the
  real 15.1 script texts, so the allowlist and the tasks run unchanged.

## Jails

Phase 7b, step 7 ([ADR 0010](../decisions/0010-storage-and-jails.md),
decisions 8, 9, and 13):

```
skep-api  /api/v1/containers (container:read, write, delete, power)
   |
skep-agent
   internal/containers  the service and its tasks (create, update, delete,
                        start, stop, restart), observation from jls
   internal/hostops     Jails: the 0700 <root>/jail parent, zfs clone of
                        @skep-base, the jail.conf renderer and the baseline
                        check, jail -c / jail -r (jail -R when jail -r fails,
                        amendment u), the O_RESOLVE_BENEATH writer
   internal/database    ContainerStore (the containers table of agent/0001
                        and agent/0005)
```

- **No runner process.** A jail is `persist`ent in the kernel and outlives
  the agent; the agent observes every jail from one `jls` before it
  serves its socket and on every read.
- **The jail.conf** is rendered from a typed `JailSpec` into
  `/var/run/skep/jails/<id>/` before every start and stop; the kernel's
  view of the parameters is checked after `jail -c`.
- **Writes into a jail** (rc.conf and root's SSH key, only before the first
  start) go through `jailwrite_freebsd.go`; other systems have a stub, and
  the dev fake records what would have been written.

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
| TLS: a new pair swapped in without a mismatched pair ever being loaded: a reload between the renames, a crash between them, a mismatched pending pair refused, 4272 reads during 200 regenerations | Validated as an ordinary user (2026-11-13, `internal/apitls` unit tests on `hearst`, 50 repeats) |
| Password prompt without echo on a pseudo-terminal and over SSH; `--password-stdin` | Validated (2026-11-12, see `tests/integration/adminprompt`) |
| Password prompt on the system console | **Pending FreeBSD validation** |
| Argon2id time with the default parameters | Validated: 121 to 151 ms per hash on hearst (2026-11-12) |
| Login (cookie and bearer), `401` without a session, task cancel through the API with the user recorded by the agent | Validated as an ordinary user (2026-11-12) |
| Agent socket under `/var/run/skep` with a root agent and the real `skep` account (`root:skep 0660`; refusal by file permissions and by the peer check); ownership change of `/var/db/skep/agent` from `skep` to root on upgrade | Validated (2026-11-12, see `tests/integration/rcd`) |
| `skep-api` under rc.d as `skep`, binding its port without privileges, generating its key as `skep`, start at boot; `skep-api admin` started as root giving up root | Validated (2026-11-12, same) |
| Task cancel through the API under rc.d: the agent records the user as the actor and uid 1002 (`skep`) as the verified peer | Validated (2026-11-12, same) |
| `skepctl`: first-contact key confirmation on a pseudo-terminal and refusal without one, `--trust-key`, refusal of a changed key with nothing sent, password without echo, `0600` token file in a `0700` directory, read-only commands, logout, a session ended on the server; `skep-api admin disable` and `admin delete` with the last-administrator rule | Validated as an ordinary user, API on loopback (2026-11-12, see `tests/integration/skepctl`) |
| `skepctl` from another host (merrick, Linux build) and on the host, against the `skep_api` rc.d service listening on the management address; back to loopback afterwards | Validated (2026-11-12, see `tests/integration/skepctl`) |
| `skepctl` chain verification of an operator-supplied certificate (`--ca-file`), a renewal with a new key without a new login, refusal of another CA | Validated as an ordinary user (see `tests/integration/skepctl`) |
| Retention under rc.d on UFS: archive directories `root:wheel 0700` and `skep:skep 0700` with `0400` files, seal (write, sync, rename, read back), task pruning, removal of old access audit entries, `audit verify` and `audit export`, the syslog notice of each seal, a restart, an altered segment | Validated (2026-11-12, see `tests/integration/retention`) |
| Retention with the data directory on a ZFS dataset (file-backed test pool): the same checks | Validated (2026-11-13, same) |
| `skep-api` from the FreeBSD build (`make build-freebsd`) serving the web interface under rc.d on the management address, with a scratch state directory: the headers, login (a wrong password first), every page in both themes, a viewer refused Users, a session ended underneath an open page and the return to the same address, task cancel, logout, no CSP violation; the Playwright suite run from merrick against hearst; back to loopback and stopped afterwards | Validated (2026-11-13, `web/e2e`, 5 tests) |
| The rc.d harness `api` phase with the health statuses of #15 (`unavailable` with the agent down) | Validated (2026-11-13, 71 checks, see `tests/integration/rcd`) |
| VM storage on ZFS: datasets and zvols with `volmode=dev`, `volblocksize=16K`, full or no reservation, `skep:` user properties; refusals outside the confinement; deletion of the VM's datasets only | Validated (2026-11-14, see `tests/integration/vm`) |
| Booting a bhyve guest: runner, taps, nmdm console in the browser, a FreeBSD install to a zvol, guest reboot, media detach, stop and `stop_timeout`, force stop (also runner-less), restart, agent restart and `kill -9`, host reboot with drift, console audit, limit, logout and idle, permissions, concurrency, interrupted create, capabilities with `vmm` unloaded, unit tests natively | Validated (2026-11-14, after the nested-KVM workaround; see `tests/integration/vm`). Guest network isolation first failed because the lab firewall routed the guest VLAN to the SERVERS and MGMT VLANs; passed after the firewall fix |
| Storage, phase 7a: inventory, snapshots (running and stopped), rollback with and without `destroy_newer`, linked clones booting with their own lease, disk add, grow, remove, directory pools with a FreeBSD guest installed on a file disk, `/metrics` against `zpool`, `zfs`, `df`, and `ps`, `storage_capacity`, `storage_locked` with an encrypted root and its key unloaded, the viewer refused every write, the clone note in the browser, `skepctl vm snapshot` | Validated (2026-11-14 and 15, see `tests/integration/storage`) |
| ZFS-root host | **Pending FreeBSD validation** (the lab VM is UFS-root) |
| arm64 | Not a target yet |

## Known limitations

- Permissions are global: there is no per-resource scoping and no custom
  roles yet.
- Sessions and users are per node.
- The local node is the only node.
- The node inventory (hardware discovery) is refreshed only at start, not
  periodically.
- **Follow-up: the agent does not check its own files.** `skep-agent`
  runs as root but starts even when its executable, its configuration
  file, or a directory above either is writable by another user (the
  lab's test installation under `/home/aba` was such a case until it moved
  to the package layout on 2026-11-15). It should
  refuse to start, naming the path, owner, and mode: owner uid 0, no
  group or other write bit, for the file and every directory up to `/`;
  a `-dev` build on Linux only warns. Whether `skep-api` checks its own
  binary and `api.json` the same way is part of that change. Logged
  2026-11-15, before phase 7b; `scripts/deploy-test.sh` already installs
  root-owned files and warns about such parent directories.
- Resource monitoring keeps current values only; there is no history and
  no export (Prometheus or otherwise) yet. Per-jail values come with 7b.
- Retention does not shrink the database files: SQLite reuses freed pages,
  so a file stops growing. `VACUUM` is a manual step.
- Archived audit entries are read on the host (`audit export`); the API
  serves the live table only.
- The web interface is desktop first and was checked in Chromium; it has
  no phone layout.
- `os.release` on FreeBSD is the kernel release (`kern.osrelease`). The
  userland patch level can differ and is not reported.
- Memory "available" on FreeBSD is an estimate: free, inactive, and laundry
  pages. It does not account for the ZFS ARC, which the kernel can also
  shrink.
- CPU virtualization support (VT-x, EPT) is not probed while vmm is
  unloaded.
- Discovery searches a fixed list of directories for binaries, not `$PATH`.
- Nested bhyve under KVM with APIC virtual interrupt delivery enabled can
  stop the whole host at a guest's first start (a KVM bug). Skep does not
  detect it yet; the workaround is in the
  [VM guide](../administration/virtual-machines.md). Follow-up: a startup
  check that warns before a start.
