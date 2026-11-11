# ADR 0004: Privilege boundary between skep-api and skep-agent

- Status: **Proposed, awaiting review.** No code implements this yet.
- Date: 2026-11-11

## Summary

Split the 0.1.0 agent into two processes before any state-changing endpoint
exists:

- **`skep-api`** runs as an unprivileged user. It is the only process that
  listens on the network. It authenticates, authorizes, validates, and
  audits, and it cannot change the host.
- **`skep-agent`** runs as root. It has no network listener. It accepts a
  closed set of typed operations over a Unix domain socket and executes
  them.

The boundary is the socket. Only typed operations cross it. There is no
operation that takes a command, a shell string, or an arbitrary path.

Seven decisions need your answer. They are listed under
[Decisions for review](#decisions-for-review), each with a recommendation.

## Context

In 0.1.0, `skep-agent` serves the public API itself, on loopback, with no
authentication. That is acceptable only because every endpoint is read-only
and discovery needs no privileges; the agent was validated on FreeBSD as an
unprivileged user.

That stops being true with the first real operation. Starting a bhyve VM,
creating a zvol, building a bridge, and creating a jail all need root. If
the process that parses HTTP from the network is also the process that holds
root, then every bug in request parsing, session handling, or a dependency
is a root bug.

The standing requirements say the same thing: do not run the web
application as root, and keep unprivileged API services separate from
privileged host operations with a clearly defined boundary. The API must
never expose arbitrary command execution or a root shell.

The code already has the seam. `internal/api` depends only on the
`api.Backend` interface, not on the agent or on discovery.

## Threat model

What the boundary is meant to hold against:

1. **A remote attacker who compromises `skep-api`**, through a parsing bug,
   an authentication bypass, or a vulnerable dependency. This is the main
   case. The API is the largest and most exposed attack surface.
2. **A local unprivileged user** on the host who tries to talk to the agent
   directly.
3. **An authenticated but malicious or mistaken API user** who sends
   hostile values in otherwise valid requests.

What it is not meant to hold against: an attacker who already has root on
the host, and a compromised agent. Both own the node by definition.

## Proposal

### Processes

```
                network (TLS)
                     |
        +------------v-------------+
        | skep-api   user: skep    |   authN, sessions, RBAC, validation,
        |                          |   rate limiting, request audit
        | owns: api.db, TLS key    |   no host access
        +------------+-------------+
                     |  Unix socket /var/run/skep/agent.sock
                     |  typed operations, peer credentials checked
        +------------v-------------+
        | skep-agent  user: root   |   re-validates, confines, executes,
        |                          |   records tasks and operation audit
        | owns: agent.db           |   no network listener
        +------------+-------------+
                     |
          bhyve, ZFS, jails, networking
```

| | `skep-api` | `skep-agent` |
|---|---|---|
| Runs as | `skep` (unprivileged, no login shell) | `root` |
| Listens on | TCP with TLS | Unix socket only |
| Decides | Who the caller is and whether they may do this | Whether this operation is safe and valid on this host |
| Can execute host commands | No | Yes, typed operations only |
| Survives the other being down | Yes: still authenticates, returns 503 for node operations | Yes: VMs keep running, tasks keep executing |

`skepctl` and the web UI talk to `skep-api` only. Nothing else talks to the
agent socket. `skep-controller`, when it is built, would be a second
unprivileged client of the same socket; it is not designed here.

### The channel

- A Unix domain socket at `/var/run/skep/agent.sock`.
- The directory is `root:skep`, mode `0750`. The socket is `root:skep`,
  mode `0660`. Filesystem permissions are the first check.
- The agent also reads the peer's credentials from the kernel for every
  connection and accepts only an allow-list of UIDs (`root` and `skep`).
  This is the second check, and it means a permissions mistake on the
  directory does not open the agent to every local user.
- The protocol is HTTP and JSON over the socket, under `/internal/v1`. It is
  an internal interface with its own version. It is not the public API and
  makes no compatibility promise to third parties.
- The agent limits request size, concurrency, and time per request.

Peer credentials are read with `LOCAL_PEERCRED` on FreeBSD and
`SO_PEERCRED` on Linux, both through `golang.org/x/sys/unix`. I have not yet
run this on FreeBSD from Go. It is the first thing to prove on `hearst`, and
the design depends on it.

### What may cross the boundary

**Only typed operations from a closed set.** Each operation is a Go struct
with named, typed, validated fields, for example "start VM `<id>`" or
"create volume of `<size>` in pool `<id>`".

Rules for every operation:

1. **No generic primitives.** No operation runs a command, reads or writes
   an arbitrary file, or loads an arbitrary module. Adding an operation is a
   code change in the agent, reviewed as a change to the privileged surface.
2. **Resources are named by ID, never by path.** The agent derives dataset
   names, device paths, and file paths from resource IDs and its own
   configuration. A path, device node, or dataset name supplied by the
   caller is never used directly.
3. **The agent re-validates everything.** Validation in the API is for good
   error messages. Validation in the agent is the security control. The
   agent does not assume the API checked anything.
4. **The agent enforces host confinement that the API cannot change.** The
   agent's root-owned configuration lists what Skep may touch: which pools
   and dataset roots, which bridges and physical interfaces, which
   management interface is protected. An operation outside that set fails,
   whoever asks. Raw device passthrough and host path mounts are off unless
   allow-listed there. That configuration is not editable through the API.
5. **Commands are built from typed values**, with `exec.Command` and
   explicit argument lists, as discovery already does.

### What a compromised API can and cannot do

This is the honest statement of what the boundary buys.

The agent trusts the API's decision that a caller is authenticated and
authorized. So an attacker who fully controls `skep-api` can invoke any
operation, as any claimed user. They can stop, delete, or create
Skep-managed VMs, jails, and volumes.

They cannot get a root shell, run arbitrary commands, read or write
arbitrary host files, touch pools, datasets, interfaces, or devices outside
the agent's configured set, cut the protected management interface, or
erase the agent's record of what was done.

That is a real reduction, from "root on the host" to "control of
Skep-managed resources", but it is not immunity. Decision 4 covers whether
to go further.

### Identity and audit across the boundary

Every request to the agent carries metadata set by the API: the actor's
user ID and name, the request ID, the source address, and the idempotency
key if any.

- The agent records this with each task. It labels the actor as *asserted
  by the API*, because the agent cannot verify it.
- The agent does not make authorization decisions from it. RBAC lives in
  the API.
- The agent also records the *verified* peer: the UID read from the socket.

Audit is written on both sides. The API records authentication events and
authorization decisions, including denials that never reach the agent. The
agent records every privileged operation it accepted, in its own
append-only log that the API process cannot write to. An attacker who
controls the API can add false entries to the API's log but cannot remove
the agent's record of the operations they ran.

### State ownership

Each process owns one SQLite database that the other cannot open.

| Database | Owner | Mode | Holds |
|---|---|---|---|
| `/var/db/skep/agent/agent.db` | `root` | `0600` in a `0700` directory | Node identity, VMs, disks, networks, containers, storage, interfaces, tasks, task events, operation audit |
| `/var/db/skep/api/api.db` | `skep` | `0600` in a `0700` directory | Users, roles, sessions, authentication and authorization audit |

The agent's database is the source of truth for the node. This follows from
the requirement that the agent works without the API or controller.

The API reads node state by asking the agent. It never opens `agent.db`.
Sharing one SQLite file between a root process and an unprivileged one
would need a group-writable directory for the journal files and would let a
compromised API rewrite desired state directly, bypassing every check in
the agent.

**Consequence for existing work:** migration `0001` puts `users`, `roles`,
and `audit_log` in the same file as node state. Under this proposal they
move to a separate migration set for `api.db`. See Decision 1.

### Tasks

1. The API validates and authorizes a request, then sends the operation to
   the agent.
2. The agent validates it again, writes a `pending` task to `agent.db`, and
   returns the task ID. A repeated idempotency key returns the existing
   task.
3. The agent executes the task and appends task events.
4. The API reads task status and events from the agent on behalf of
   callers.

If the agent restarts, tasks that were `running` become `interrupted`. If
the API restarts, nothing happens to tasks.

### Secrets

- The API's TLS private key and session secrets are readable by `skep`
  only. The agent never sees them.
- Password hashes live in `api.db`. The agent never sees them.
- The node's future mutual-TLS key belongs to the agent and is readable by
  root only. The API never sees it.

### Clusters

Not designed here; the cluster technology is undecided. Two constraints
carry forward. The internal API is independent of its transport, so a
remote agent can be reached later without changing operations. And any
network path to an agent will require mutual TLS with peer identity
checked, the network equivalent of the peer-credential check above. The
agent gets no network listener until that exists.

### Migration from 0.1.0

1. Add `skep-api`. It serves the public API and implements `api.Backend`
   with a client for the agent socket. The handlers in `internal/api` do
   not change.
2. Move the agent's listener from loopback TCP to the Unix socket and move
   its read-only routes under `/internal/v1`.
3. Add authentication and TLS to `skep-api`.
4. Only then add the first state-changing operation.

Steps 1 and 2 change no behaviour visible to an API client.

## Alternatives considered

**A. One process, as root (what 0.1.0 does).** Simplest. Rejected: it puts
network parsing and root in the same address space.

**B. Unprivileged API and root agent (this proposal).**

**C. Three processes: unprivileged API, unprivileged agent, and a minimal
root helper.** The smallest possible root code. Not recommended now: almost
everything the agent does needs root, so the helper would duplicate the
agent's operation set behind a second protocol. The typed operation layer
in B is the same seam, so C remains possible later for specific operations
without redesign.

**D. An unprivileged agent using delegated rights**, such as `zfs allow`
for storage and devfs rules for the vmm devices. Attractive, but partial:
bridge, tap, and jail management still need root, so the agent would need a
root path anyway. Better treated as hardening once each delegation is
proven on FreeBSD 15.1. I have not verified how far unprivileged bhyve goes
on 15.1 and am not assuming it.

**E. Unprivileged API calling `sudo` or `doas` with an allow-list of
commands.** Rejected. It makes command lines the interface, which is
exactly what the "no shell commands from untrusted input" rule exists to
prevent, and argument injection into an allowed command becomes a root bug.

**F. gRPC or `net/rpc` for the channel.** gRPC adds a large dependency and
code generation for an interface with one client. `net/rpc` has no
versioning story and uses gob. HTTP and JSON over the socket need nothing
outside the standard library, reuse the middleware and tests that already
exist, and can be exercised with `curl --unix-socket`.

## Decisions for review

1. **Split the database in two, and restructure migration `0001` now?**
   Recommended: yes. Nothing is deployed and PR 1 is not merged, so the
   migration can be split into `migrations/sqlite/agent` and
   `migrations/sqlite/api` without breaking the "never edit an applied
   migration" rule for anyone.
2. **Agent as root, or go straight to a smaller root helper (C)?**
   Recommended: root agent now. Revisit per operation once the operation
   set is known.
3. **HTTP and JSON over a Unix socket, or something else (F)?**
   Recommended: HTTP and JSON.
4. **Should the agent trust the API's asserted identity?** Recommended:
   yes for now, with the confinement rules above as the limit on damage.
   The stronger option is for the agent to verify a signed, short-lived
   token per request, so that a compromised API cannot invent actors. That
   moves key management and part of authorization into the root process,
   and I would rather add it deliberately later than half-build it now.
5. **Audit on both sides, as described?** Recommended: yes. The
   alternative is a single log in the API, which a compromised API could
   rewrite.
6. **Dev mode: two processes, or one combined process?** Recommended: two,
   started together by `make run-dev`, with the socket under `.skep-dev/`.
   A combined mode would be more convenient, but it would mean the boundary
   is never exercised in daily development.
7. **Service account name.** Recommended: user and group `skep`.

One related point that is not part of this ADR but that you should decide:
**no milestone currently contains authentication.** Milestone 2 is
persistence, tasks, and audit; the first state-changing endpoint arrives
with Milestone 5. I recommend steps 1 to 3 of the migration above land
between Milestones 2 and 5, and that the milestone list in `CLAUDE.md` say
so.

## Consequences if accepted

- Two rc.d services, two configuration files, and one service account to
  create at install time.
- Every node operation costs one local socket round trip. This is
  negligible next to the operations themselves.
- The public API and the internal API are two interfaces to keep in step.
  Shared types in `pkg/models` limit the duplication.
- Milestone 2's task engine and operation audit are built in the agent,
  against `agent.db`. User, role, and session storage are built in the API,
  against `api.db`.
- `docs/security/overview.md` and `docs/architecture/overview.md` are
  updated when this is accepted, not before.

## To verify on FreeBSD before building on this

All pending FreeBSD validation:

- Reading peer credentials from a Unix socket in Go on FreeBSD 15.1.
- Socket and directory permissions behaving as described under
  `/var/run`, including after a reboot clears it.
- Running `skep-api` under rc.d as the `skep` user.
