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

A third binary, `skep-cluster` ([ADR 0013](../decisions/0013-clustering.md)),
holds the node's cluster key and the Raft store as its own unprivileged
account. Since Milestone 8, step 8a it exists and runs, and since step
8b-1 a host can join another's cluster (with its guests since step 8d-2), but the other two
do not use it yet; it is described in
[skep-cluster](skep-cluster.md) and left out of the picture below.

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

### The internal API's operations (step 8d-1)

The agent's socket admits two peers by the kernel's credentials: root and
the API account (`skep`). Every operation of the internal API is in one
table (`internalapi.Operations`) that says which of the two may call it;
every route is registered from it, and a route not in it does not exist.
One function (`authorize`) checks the caller of an operation by the peer
UID, root first, before the handler runs. No header takes part in the
decision; the actor headers are recorded, never trusted for access. A
test checks this table against the code.

| Operation | Method and path | Root | API |
|---|---|---|---|
| `health` | `GET /internal/v1/health` | yes | yes |
| `system` | `GET /internal/v1/system` | yes | yes |
| `node-hardware` | `GET /internal/v1/nodes/{id}/hardware` | yes | yes |
| `tasks-list` | `GET /internal/v1/tasks` | yes | yes |
| `task-get` | `GET /internal/v1/tasks/{id}` | yes | yes |
| `task-cancel` | `POST /internal/v1/tasks/{id}/cancel` | yes | yes |
| `task-by-key` | `GET /internal/v1/tasks/by-key/{key}` | yes | yes |
| `drain-get` | `GET /internal/v1/drain` | yes | yes |
| `drain-set` | `POST /internal/v1/drain` | yes | no |
| `drain-clear` | `DELETE /internal/v1/drain` | yes | no |
| `vms-list` | `GET /internal/v1/vms` | yes | yes |
| `vm-create` | `POST /internal/v1/vms` | yes | yes |
| `vm-get` | `GET /internal/v1/vms/{id}` | yes | yes |
| `vm-update` | `PUT /internal/v1/vms/{id}` | yes | yes |
| `vm-delete` | `DELETE /internal/v1/vms/{id}` | yes | yes |
| `vm-action` | `POST /internal/v1/vms/{id}/actions` | yes | yes |
| `vm-console` | `GET /internal/v1/vms/{id}/console/serial` | yes | yes |
| `vm-snapshots-list` | `GET /internal/v1/vms/{id}/snapshots` | yes | yes |
| `vm-snapshot-create` | `POST /internal/v1/vms/{id}/snapshots` | yes | yes |
| `vm-snapshot-get` | `GET /internal/v1/vms/{id}/snapshots/{sid}` | yes | yes |
| `vm-snapshot-delete` | `DELETE /internal/v1/vms/{id}/snapshots/{sid}` | yes | yes |
| `vm-snapshot-rollback` | `POST /internal/v1/vms/{id}/snapshots/{sid}/rollback` | yes | yes |
| `vm-clone` | `POST /internal/v1/vms/{id}/clone` | yes | yes |
| `storage-pools` | `GET /internal/v1/storage/pools` | yes | yes |
| `storage-media` | `GET /internal/v1/storage/media` | yes | yes |
| `storage-datasets` | `GET /internal/v1/storage/datasets` | yes | yes |
| `template-sources` | `GET /internal/v1/storage/template-sources` | yes | yes |
| `templates-list` | `GET /internal/v1/storage/templates` | yes | yes |
| `template-create` | `POST /internal/v1/storage/templates` | yes | yes |
| `template-get` | `GET /internal/v1/storage/templates/{id}` | yes | yes |
| `template-delete` | `DELETE /internal/v1/storage/templates/{id}` | yes | yes |
| `containers-list` | `GET /internal/v1/containers` | yes | yes |
| `container-create` | `POST /internal/v1/containers` | yes | yes |
| `container-get` | `GET /internal/v1/containers/{id}` | yes | yes |
| `container-update` | `PUT /internal/v1/containers/{id}` | yes | yes |
| `container-delete` | `DELETE /internal/v1/containers/{id}` | yes | yes |
| `container-action` | `POST /internal/v1/containers/{id}/actions` | yes | yes |
| `container-console-log` | `GET /internal/v1/containers/{id}/console-log` | yes | yes |
| `container-snapshots-list` | `GET /internal/v1/containers/{id}/snapshots` | yes | yes |
| `container-snapshot-create` | `POST /internal/v1/containers/{id}/snapshots` | yes | yes |
| `container-snapshot-get` | `GET /internal/v1/containers/{id}/snapshots/{sid}` | yes | yes |
| `container-snapshot-delete` | `DELETE /internal/v1/containers/{id}/snapshots/{sid}` | yes | yes |
| `container-snapshot-rollback` | `POST /internal/v1/containers/{id}/snapshots/{sid}/rollback` | yes | yes |
| `container-clone` | `POST /internal/v1/containers/{id}/clone` | yes | yes |
| `metrics` | `GET /internal/v1/metrics` | yes | yes |

### The drain (step 8d-1, ADR 0013 amendment 6)

`POST /internal/v1/drain` with `{"reason":"upgrade"|"join"}` (root only)
sets a drain for 60 seconds from now, or renews the drain of the same
reason; it answers the reason, the expiry and the number of unfinished
tasks. Another reason while a drain is active is `409 drain_active` and
changes nothing. `DELETE /internal/v1/drain` (root only) clears it, and
answers the same 200 without one. `GET /internal/v1/drain` is open to root
and skep-api.

While a drain is active the task engine refuses every new task with
`503 node_draining` (details: the reason and the expiry), recorded as a
refused submission; a replay by idempotency key, which creates nothing,
is answered as it is with a full queue. Cancelling and reading are not
affected. The drain lives in the engine's memory: it expires by the
engine's clock unless renewed, and an agent restart clears it. Setting
it (not each renewal), clearing it, and the first time the engine notices
it expired each write one agent audit entry (`node.drain.set`,
`node.drain.cleared`, `node.drain.expired`) with the actor and the peer
UID. While it is active the health document has a non-critical `drain`
check with the reason and the expiry. Nothing in step 8d-1 sets a drain
by itself: `skep-setup upgrade` and `skep-cluster join` use it in later
steps; the reason `maintenance` comes with step 8f.

`GET /internal/v1/tasks/by-key/{key}` returns the task created with an
idempotency key (404 without one). skep-api's guest reconciliation reads
the task of a name reservation with it; it is not in the public API.

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

## Guest-port anti-spoofing

Milestone 7c ([ADR 0011](../decisions/0011-guest-port-anti-spoofing.md)):

```
skep-agent
   internal/portfilter  the ng_bpf program per NIC (static, MAC-only, IPv6
                        with the RA guard; DHCP-server guard everywhere),
                        the ngctl install script (by node ID), and the
                        parsers of ngctl show / getprogram; no OS calls
   internal/hostops     PortFilters: kldstat checks, ngctl -f - with the
                        script on stdin, read-back, reinstall by node ID,
                        deletem, bridge VLAN state, the per-port flock
                        (port<slot>.lock in the owner's run directory)
   internal/portcheck   the 10 s check of every running guest's ports and
                        the reaction (audit, reinstall, detach), the
                        port_filters and bridge_vlan_unfiltered checks
   jail start / runner  install under the port lock between creating the
                        epair or tap and addm
```

- **One owner of repair.** Jail start and the VM runner install; only the
  agent's checker reinstalls or detaches, for VM ports too. The runner, a
  separate process, shares the port lock through `flock(2)`.
- **No API field, no configuration key, no migration.** The program is
  derived from the NIC as recorded; node IDs are read, not stored.

## FreeBSD validation status

Validated means it ran on FreeBSD 15.1-RELEASE amd64 on a lab virtual
machine nested under KVM (Proxmox, Intel VT-x) and the output was checked.
Unless a row names another host, that is `hearst` (UFS root, a file-backed
test pool; 15.1-RELEASE-p6, later p7). `smith` (UFS root)
and `langrishe` (ZFS root) are lab VMs of the same kind. Nothing has been
validated on bare metal or on an AMD host
([known issues](../release/0.1/known-issues.md)). Everything else is
**pending FreeBSD validation**, even though it cross-compiles and passes
unit tests on Linux. A row records the build and date of its run; a later
build is not revalidated unless the row says so.

The first validation run, on 2026-11-11, used two host states: first with
vmm not loaded and no pools; then with vmm loaded and the file-backed pool
`skeptest` imported, both as an unprivileged user and as root.

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
| A tmpfs `/var/run` | **Verified for service start** on smith (UFS root, FreeBSD 15.1-RELEASE-p7, build 2323afd, 2026-11-23): both services start at boot, their rc.d scripts create `/var/run/skep` as `root:skep 0750`, the agent's start check passes, health `ok`. **Still pending:** running a VM or jail with `/var/run` on tmpfs, and a ZFS-root host |
| Agent on a Unix socket only, API through the socket, peer UID check, mandatory actor: both processes as one ordinary user | Validated (2026-11-12) |
| Unit tests of the new packages run natively (`internalapi`, `agentclient`, `apitls`, `auth`, `apidb`, `api`, `privdrop`, `sysaccount`, `tests/e2e`, both commands) | Validated (2026-11-12) |
| TLS: certificate generation, file modes, TLS 1.3 only, renewal with the same key on a live server, `SIGHUP` reload, `tls regenerate`, `tls fingerprint`, operator certificates | Validated as an ordinary user (2026-11-12, see `tests/integration/apitls`) |
| TLS: a new pair swapped in without a mismatched pair ever being loaded: a reload between the renames, a crash between them, a mismatched pending pair refused, 4272 reads during 200 regenerations | Validated as an ordinary user (2026-11-13, `internal/apitls` unit tests on `hearst`, 50 repeats). The 2026-11-13 design let concurrent readers finish the swap and raced (CI run 44960295215); since `fix/apitls-swap` the swap and reads are serialized with a directory `flock(2)`, revalidated 2026-11-21 on `hearst` (suite ×20, concurrency tests ×200) |
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
| Booting a bhyve guest: runner, taps, nmdm console in the browser, a FreeBSD install to a zvol, guest reboot, media detach, stop and `stop_timeout`, force stop (also runner-less), restart, agent restart and `kill -9`, host reboot with drift, console audit, limit, logout and idle, permissions, concurrency, interrupted create, capabilities with `vmm` unloaded, unit tests natively | Validated (2026-11-14, build 44a8784, after the nested-KVM workaround; `tests/integration/vm/results/step6-checks.txt`). Guest network isolation first failed because the lab firewall routed the guest VLAN to the SERVERS and MGMT VLANs; passed after the firewall fix. Not rerun since: later builds started VMs at the bhyve level only, with no console, guest boot or login inspected (on `hearst` f73f4fd and dd7a8b6 on 2026-11-22, `tests/integration/jails/results/m7c-validation.md` and `validation-2026-11-22.md`, and ce1d910 on 2026-11-23, recorded in `docs/backlog.md` only; on `smith` and `langrishe`, VMs with empty disks on 2026-11-22 and 23, `tests/integration/setup/results/lab-vm-validation-20261122.md`, `zvol-device-node-20261122.md`, `task-error-details-20261123.md`). On the 0.1 build (installed package from c8ce17b), on `smith` (UFS root) on 2026-11-24: create, start, stop, force-stop and delete of a VM with one zvol disk and no NIC, and a boot from the FreeBSD 15.1 bootonly ISO to the installer's first prompt on the serial console (`tests/integration/vm/results/smoke-0.1-c8ce17b-20261124-smith.md`); no guest install, installed guest's boot or guest networking on that build |
| Storage, phase 7a: inventory, snapshots (running and stopped), rollback with and without `destroy_newer`, linked clones booting with their own lease, disk add, grow, remove, directory pools with a FreeBSD guest installed on a file disk, `/metrics` against `zpool`, `zfs`, `df`, and `ps`, `storage_capacity`, `storage_locked` with an encrypted root and its key unloaded, the viewer refused every write, the clone note in the browser, `skepctl vm snapshot` | Validated (2026-11-14 and 15, see `tests/integration/storage`) |
| Guest-port anti-spoofing (ADR 0011): module check, install before `addm` for jails and VMs, programs identical to the generated ones, every probe case for static, DHCP and VM ports, ordering, fail-open detection, reinstall, detach, three-in-a-row, agent restart, upgrade from b61e578, a real DHCP lease, VLAN filtering on both bridges | Validated (2026-11-22, `tests/integration/jails/results/m7c-validation.md`; the VM program's hostile-frame cases emulated on a hand-made epair). Reboot with `kld_list` (Decision 12 item 10): validated 2026-11-22 (`tests/integration/jails/results/validation-2026-11-22.md`) |
| `skep-setup check` and `plan` against a real host (read-only, `hearst`) | Validated (2026-11-22, `tests/integration/setup/results/hearst-check-plan-20261122.txt`) |
| The package (`pkg create`, the pre-install and pre-deinstall scripts, `PKG_UPGRADE`), `skep-setup apply`, `rollback`, `upgrade`, `remove`, `db backup` on live databases, the version handshake between deployed programs (`503 agent_version_mismatch`) | Validated on `smith` (UFS root) and `langrishe` (ZFS root), 2026-11-22, builds e8836a8 to 67fa267 (`tests/integration/setup/results/lab-vm-validation-20261122.md`). pkg deletes the files even when the pre-deinstall script refuses; `skep-setup remove` is the guard. An install on hardware is pending |
| Jail templates (7b): the pkgbase builder with the real host operations, through its test binary and through a deployed agent; the `dist` source | Validated: pkgbase on `hearst` (`tests/integration/jails/results/step6-build.txt`, `step6-deployed.txt`); on 2026-11-22 the `dist` source on `smith` and a pkgbase build through the installed agent on `langrishe` (`tests/integration/setup/results/lab-vm-validation-20261122.md`) |
| Jails (7b, steps 7 to 9): lifecycle, the `jail.conf` baseline checked against the kernel, `/dev` allow-list, SSH key before the first start, VNET epairs with DHCP and static IPv4, RCTL limits, `refquota`, jail usage in `/metrics`, snapshots, rollback, clones; the web interface's Containers section | Validated on `hearst` (`tests/integration/jails/README.md`, the step 7 to 10 progress logs; the Containers section on 2026-11-21, section 26 of `step10-progress.md`). Since then: two jails starting at once with the same static address (`hearst`, dd7a8b6, `tests/integration/jails/results/validation-2026-11-22.md`); a host without RACCT (`langrishe`, eadd643, 2026-11-24, `racct-off-guest-20261124-langrishe.md`); the start-up sweeps' audit entries (`langrishe`, eadd643, `sweep-audit-entries-20261124-langrishe.md`) |
| Jails: a start refused for an RCTL limit without RACCT; the refusal to clone a started static-address jail (ADR 0010 amendment ab); leftover devfs removal by the start and delete paths | **Pending FreeBSD validation** (unit-tested; for amendment ab the backlog notes a live check on `langrishe` at e05b380 that no results file records) |
| ZFS-root host | Partly validated on `langrishe` (ZFS root): the package and `skep-setup` (2026-11-22), a pkgbase template build, jails without RACCT and the start-up sweeps (2026-11-24), and a VM with an empty disk started at the bhyve level (2026-11-22 and 23). A guest operating system, the storage checks of phase 7a and the services' tmpfs `/var/run` start are not validated on a ZFS-root host |
| arm64 | Not a target yet |

## Known limitations

- **Guest-port anti-spoofing is MAC-only on DHCP jail NICs and all VM
  NICs**: such a guest can still use another IPv4 address with its own
  MAC. IPv6 source and neighbour discovery are not pinned, the RA guard is
  best-effort, and detection of a filter removed by host root is polling
  (10 s). See [Guest-port anti-spoofing](../administration/guest-port-filtering.md#known-limitations).
- Permissions are global: there is no per-resource scoping and no custom
  roles yet.
- Sessions and users are per node.
- The local node is the only node.
- The node inventory (hardware discovery) is refreshed only at start, not
  periodically.
- **The agent checks its own files; `skep-api` does not.** As root on
  FreeBSD, `skep-agent` refuses to start when its binary, configuration,
  data or socket directory, or any parent, is not root-owned, is group-
  or other-writable, or has a non-trivial ACL, and prints the fix; there
  is no override (ADR 0004 amendment of 2026-11-23, `internal/startcheck`).
  `skep-api` runs unprivileged and has no such check.
- Resource monitoring keeps current values only; there is no history and
  no export (Prometheus or otherwise) yet.
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
  stop the whole host at a guest's first start (a KVM bug). The agent
  warns at start and reports a degraded `nested_interrupts` health check
  when it sees this, but does not prevent a start; the workaround is in the
  [VM guide](../administration/virtual-machines.md).
