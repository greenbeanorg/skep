# ADR 0004: Privilege boundary between skep-api and skep-agent

- Status: Accepted
- Date: 2026-11-11 (proposed and accepted)
- Revisit: before 1.0, decide whether to split a minimal root helper out of
  the agent (see Decision 2).

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

The seven decisions that were put to review, and their outcomes, are under
[Decisions](#decisions). What is implemented so far is under
[Implementation status](#implementation-status).

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

## Decision

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

Peer credentials are read with `LOCAL_PEERCRED` (`struct xucred`) on
FreeBSD and `SO_PEERCRED` on Linux, both through `golang.org/x/sys/unix`.
This was proven on FreeBSD 15.1-RELEASE-p6 before the ADR was accepted: a
root server allowed UIDs 0 and 1001 and denied 65534, and a `0660` socket
in a `0750` directory refused `connect(2)` from a user outside its group.
The code is `internal/peercred`; the probe and its recorded output are in
`tests/integration/peercred`.

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
6. **Privileged OS calls live in one package behind an interface.** Every
   call that needs root goes through a single internal package (working
   name `internal/hostops`), created with the first privileged operation.
   The rest of the agent depends on its interface, not on the calls. This
   keeps the root-only code small and reviewable, and it is the seam along
   which a minimal root helper can be split out later without redesign
   (Decision 2). Read-only discovery needs no privileges and stays in
   `internal/hardware`.

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
Skep-managed resources", but it is not immunity. Decision 4 records when
this is tightened.

### Identity and audit across the boundary

Every request to the agent carries metadata set by the API: the actor's
user ID and name, the request ID, the source address, and the idempotency
key if any.

- **The actor is mandatory.** The agent rejects any operation that arrives
  without one, and the schema enforces it: `actor_id` and `actor_name` are
  `NOT NULL` and non-empty in both `tasks` and the agent's `audit_log`.
  Work the agent starts itself uses the actor `system`.
- The agent records the actor with each task and in its audit log. It is
  the actor *as asserted by the API*; the agent cannot verify it.
- The agent does not make authorization decisions from it. RBAC lives in
  the API.
- The agent also records the *verified* peer: the UID read from the socket
  (`audit_log.peer_uid`, also mandatory).
- **Multi-node is the trigger for signed tokens.** Trusting an asserted
  identity is acceptable while the only caller is a local process that the
  kernel has identified. As soon as an agent accepts requests that
  originate on another node, the asserted identity is replaced by a signed,
  short-lived token that the agent verifies. That work is part of the
  clustering milestone and must be done before a remote caller exists.

Audit is written on both sides. The API records authentication events and
authorization decisions, including denials that never reach the agent. The
agent records every privileged operation it accepted, in its own
append-only log that the API process cannot write to. ("Append-only"
means the record as a whole since [ADR 0007](0007-retention.md): an entry
is never changed, and leaves the live table only after it is in a sealed,
hash-chained archive segment that the same process keeps.) An attacker who
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

The two schemas are separate migration sets, `migrations/sqlite/agent` and
`migrations/sqlite/api`, each with its own version history. A unit test
(`TestDatabasesDoNotOverlap`) fails if an identity table appears in the
agent's schema or a node-state table appears in both.

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

Steps 1 and 2 change no behaviour visible to an API client. Steps 1 to 3
are Milestone 4, "API split, authentication, and TLS". It comes before the
first frontend, which needs a login, and no VM work happens before it.

## Alternatives considered

**A. One process, as root (what 0.1.0 does).** Simplest. Rejected: it puts
network parsing and root in the same address space.

**B. Unprivileged API and root agent (chosen).**

**C. Three processes: unprivileged API, unprivileged agent, and a minimal
root helper.** The smallest possible root code. Not recommended now: almost
everything the agent does needs root, so the helper would duplicate the
agent's operation set behind a second protocol. The typed operation layer
in B is the same seam, so C remains possible later for specific operations
without redesign. To be revisited before 1.0.

**D. An unprivileged agent using delegated rights**, such as `zfs allow`
for storage and devfs rules for the vmm devices. Attractive, but partial:
bridge, tap, and jail management still need root, so the agent would need a
root path anyway. Better treated as hardening once each delegation is
proven on FreeBSD 15.1. How far unprivileged bhyve goes on 15.1 has not
been verified and is not assumed.

**E. Unprivileged API calling `sudo` or `doas` with an allow-list of
commands.** Rejected. It makes command lines the interface, which is
exactly what the "no shell commands from untrusted input" rule exists to
prevent, and argument injection into an allowed command becomes a root bug.

**F. gRPC or `net/rpc` for the channel.** gRPC adds a large dependency and
code generation for an interface with one client. `net/rpc` has no
versioning story and uses gob. HTTP and JSON over the socket need nothing
outside the standard library, reuse the middleware and tests that already
exist, and can be exercised with `curl --unix-socket`.

## Decisions

Accepted on 2026-11-11 with these outcomes.

1. **Two databases.** Accepted. `agent.db` and `api.db` are separate, and
   migration `0001` was split before the first release, while nothing was
   deployed.
2. **Root agent, for now.** Accepted, with a condition: all privileged OS
   calls stay in one internal package behind an interface, so a minimal
   root helper (alternative C) can be split out later. **Revisit before
   1.0.**
3. **HTTP and JSON over a Unix socket.** Accepted.
4. **Trust the API's asserted identity, for now.** Accepted, with
   conditions: the actor field is mandatory, it is recorded in the agent's
   audit log, and multi-node operation is the trigger for moving to signed
   tokens that the agent verifies.
5. **Audit on both sides.** Accepted.
6. **Two processes in dev mode.** Accepted.
7. **Service account `skep`.** Accepted, for both the user and the group.

Authentication had no milestone when this was proposed. Milestone 4, "API
split, authentication, and TLS", was added to `CLAUDE.md` as part of
accepting this ADR.

## Implementation status

| Part | State |
|---|---|
| Separate agent and API schemas, mandatory actor, `peer_uid` in the agent's audit log | Done (0.1.0) |
| Agent database at `<data_dir>/agent/agent.db`, private directory | Done (0.1.0) |
| Peer credential reading (`internal/peercred`) | Done and validated on FreeBSD; used by the agent socket since Milestone 4 |
| `skep-api` binary, agent Unix socket, `/internal/v1`, mandatory actor headers | Done and validated on FreeBSD (Milestone 4) |
| `skep` account, `/var/run/skep` (`root:skep`, `0750`), rc.d service for the agent | Done and validated on FreeBSD (Milestone 3) |
| rc.d service for `skep-api`, agent socket under `/var/run/skep`, agent as root with no network listener | Done and validated on FreeBSD (Milestone 4) |
| Authentication, sessions, RBAC, TLS | Done (Milestone 4, [ADR 0006](0006-api-split-authentication-tls.md)) |
| First operation that carries a user across the boundary: task cancel, with the actor and the verified peer in the agent's audit log | Done (Milestone 4) |
| Privileged operations package (`internal/hostops`) and host confinement configuration | With the first privileged operation |
| Signed per-request tokens | Clustering milestone, before any remote caller |

The agent no longer has a network listener. `skep-api` serves the public
API and reaches the agent through the socket only.

## Consequences

- Two rc.d services, two configuration files, and one service account to
  create at install time.
- Every node operation costs one local socket round trip. This is
  negligible next to the operations themselves.
- The public API and the internal API are two interfaces to keep in step.
  Shared types in `pkg/models` limit the duplication.
- Milestone 2's task engine and operation audit are built in the agent,
  against `agent.db`. User, role, and session storage are built in the API,
  against `api.db`.
- Two audit logs have to be read together to reconstruct an incident. The
  request ID is recorded in both and links them.

## FreeBSD validation

Validated on FreeBSD 15.1-RELEASE-p6:

- Reading peer credentials from a Unix socket in Go, as an unprivileged
  user and as root.
- A UID allow-list enforced from those credentials.
- Filesystem permissions on the socket and its directory refusing a user
  outside the group.

Validated in Milestone 4 (`tests/integration/rcd`, 2026-11-12):

- The peer-credential and socket permission checks with the dedicated
  `skep` account and a root agent, under `/var/run/skep`.
- Running `skep-api` under rc.d as the `skep` user.

Pending FreeBSD validation:

- A socket under `/var/run` on a host where `/var/run` is really emptied
  at boot (tmpfs). The rc.d script recreates the directory at every start;
  that was validated by removing it, not by a tmpfs.

## Amendment (2026-11-23, Accepted): the agent checks its binary and configuration at start

Status: **Accepted**, 2026-11-23 (Andy). Branch `fix/agent-start-perms`.

### Problem

`skep-agent` runs as root and executes its own binary and reads
`agent.json`, which allow-lists pools, directories, bridges and management
interfaces (the confinement every privileged operation is checked against).
Anyone who can replace the binary, or change `agent.json`, or rename any
directory above either, controls what root runs or what the confinement
allows. The package installs them root-owned (`0555`, `0600`, directories
`0755`), and `skep-setup` checks only `/var/db/skep` and its parents; nothing
stops an agent from starting on a host where a hand install left one of
these writable by another user.

### What is checked

At start, before the socket is created and before any task runs, only when
the process runs as root on FreeBSD (a Linux development agent, which runs
as the developer from the source tree, does not check):

1. **The binary**: the path the kernel reports for the running process
   (`os.Executable`, which on FreeBSD reads `kern.proc.pathname`, so
   symbolic links are already resolved).
2. **The configuration file actually loaded**: `-config PATH`, or
   `/usr/local/etc/skep/agent.json` when it exists. With no configuration
   file there is nothing to check (the built-in defaults are in the binary).
3. **Every parent directory of each, up to `/`.**

Each of these must be **owned by UID 0** and **not writable by its group or
by others** (`mode & 022 == 0`). There are no exceptions: not for the group
`wheel` (its members are not root), and not for the sticky bit (a sticky
world-writable directory above the binary still lets others create entries
next to it).

### Symbolic links

For the configuration path, both chains are checked: the path as given, each
component with `lstat` (so the directories that hold any link are covered:
whoever can write there can point the link elsewhere), and the fully
resolved path (`filepath.EvalSymlinks`) with its parents. A link's own owner
and mode do not matter on FreeBSD (a link cannot be rewritten in place, only
replaced, which needs write access to its directory). The binary's path is
already resolved by the kernel; its parents are checked as resolved.

### What the error says

The agent logs one error and exits with status 1, naming the first path that
fails, why, and the command that fixes it, for example:

```
skep-agent: refusing to start: /usr/local/etc/skep is writable by its group or others (mode 0775, owner root:wheel); a user other than root could change the configuration this root process reads. Fix: chmod go-w /usr/local/etc/skep
skep-agent: refusing to start: /usr/local/sbin/skep-agent is owned by UID 1002 (skep), not root; that user could replace the program root runs. Fix: chown root:wheel /usr/local/sbin/skep-agent
```

Under `daemon(8)` (the rc.d script) the agent is restarted after
`skep_agent_restart_delay` and refuses again, each time with the same line in
`daemon.log`; `skep-setup check` should report the same paths (a follow-up,
so the operator sees it before starting).

### What a hand-installed host sees (hearst)

hearst (skep UID 1002, installed by hand with `scripts/deploy-test.sh` in the
package layout since 2026-11-15) has, by that script and the recorded
layout: `/usr/local/sbin/skep-agent` `root:wheel 0555`, `/usr/local/sbin`
made `root:wheel 0755`, `/usr/local/etc/skep/agent.json` `root:wheel 0600`,
and stock `/usr/local`, `/usr`, `/` (`root:wheel 0755`). It would start
normally. The skep account (UID 1002) owns nothing on these paths; its
directory `/var/db/skep/api` is not checked by this amendment. **Pending
verification:** hearst is powered off tonight; the prediction comes from
the deploy script and the layout record, not from the host. The two lab VMs
(package install) were read: smith has `root:wheel` and `0755`/`0555`/`0600`
on every checked path.

A host where someone ran the agent from a home directory (the pre-2026-11-15
hearst layout under `/home/aba/skep-m6`, removed since) would be refused:
`/home/aba` is owned by `aba`.

### Questions and Andy's answers (2026-11-23)

1. **ACLs.** On ZFS (`nfsv4acls`, langrishe's root) and on UFS mounted with
   `acls`, an ACL entry can let another user write a file or directory whose
   mode bits show `0755`. Mode bits alone miss that. Options: (a) ignore
   ACLs and document it; (b) refuse a path with a non-trivial ACL, detected
   by running `/bin/getfacl -q -n PATH` (fixed arguments, at start only) and
   accepting only the entries that mirror the mode (`owner@`, `group@`,
   `everyone@`, or `user::`, `group::`, `other::`); (c) the same through the
   `__acl_get_file` system call in Go without cgo (more code, no exec).
   Recommendation: (b), simplest to review; (c) later if exec at start is
   unwanted.
2. **Scope.** Also check the data directory (`/var/db/skep`, with the rule
   `skep-setup` already applies) and the socket's directory
   (`/var/run/skep`)? Recommendation: yes for both, in this same check.
3. **Refuse or warn.** The job asked for a refusal; a refusal on an existing
   host stops every running VM's management until fixed (guests keep
   running). Recommendation: refuse, as asked, with `skep-setup check`
   reporting it first.
4. **No override.** Recommendation: no flag to skip the check (it would be
   a silent weakening).

**Answers.**

1. **(b).** Refuse a path with a non-trivial ACL, found with `/bin/getfacl`
   and fixed arguments. If getfacl fails or its output is not understood,
   refuse.
2. **Yes**, also `/var/db/skep` and `/var/run/skep` (not `/var/db/skep/api`).
3. **Refuse.**
4. **No override.**

### As built

- `internal/startcheck` holds the rules behind a small `FS` interface (a fake
  in unit tests, the host in `host_freebsd.go`). `cmd/skep-agent` calls it
  only as root on FreeBSD.
- **Two calls, both earlier than this amendment required.** The binary and
  the configuration file are checked before root parses the file. The data
  directory and the socket's directory are checked right after, before the
  database is opened and before the socket exists. These are the directories
  the loaded configuration names, by default `/var/db/skep` and
  `/var/run/skep`; either may not exist yet, and its existing parents are
  still checked.
- **A trivial ACL** is exactly three entries, one per class, with no write
  for the group or others. POSIX.1e: `user::`, `group::`, `other::`. NFSv4:
  `owner@`, `group@`, `everyone@`, type `allow`, no inheritance flags, and
  for `group@` and `everyone@` none of write, append, delete, delete child,
  write attributes, write extended attributes, write ACL, or write owner.
  Anything else well-formed is an extended ACL; anything else is not
  understood. Both are refused.
- **Also refused:** a path with a `..` component (it cannot be checked
  component by component) and a dangling symbolic link.
- **Symbolic links** are walked one component at a time, as the kernel
  resolves them. At a link the target is read (a relative target resolves
  against the link's directory) and walked the same way, so the directory
  holding every link in a chain is checked. At most 16 links per path: a
  17th, or a loop, is a refusal.
- **One resolver.** `config.ResolvePath` decides which file is read, for both
  the loader and the check. The loader reports the path it read, and the
  agent refuses to start if that differs from the path checked.
- **The database's directory** is checked too when it is not the data
  directory (by default `/var/db/skep/agent`). It may not exist yet.
- **ZFS modes that need deny entries are refused.** A mode that gives a
  class fewer rights than a broader class, for example `0705` (the group has
  less than others), is represented on ZFS with `deny` entries, which are not
  a trivial ACL here. Such a path is refused as an extended ACL even though
  its mode has no group or other write bit (seen on langrishe, 2026-11-23).
  The fix is a mode without that gap, such as `0755` or `0700`; the refusal's
  generic `setfacl -b` hint has not been tested for this case.
- The error names the owner by UID (`owner UID 0`), not by name.
- `skep-setup check` does not report these paths yet: it needs the host
  check, the configuration's path resolution and a plan item, which is more
  than a few lines. See the [backlog](../backlog.md).

### Validation (2026-11-23, lab VMs, FreeBSD 15.1)

- The check passes on smith (UFS, POSIX output) and langrishe (ZFS root,
  NFSv4 output) as installed, for the binary, `agent.json`, `/var/db/skep`,
  `/var/run/skep`, and all their parents.
- Scratch copies of the binary and `agent.json` under `/root`, with the data
  directory and socket also pointed there. On smith, `chmod g+w` on the
  scratch directory: the agent exited 1 naming that directory. On langrishe,
  an added NFSv4 entry granting `write_data` to UID 65534, with the mode still
  `0755`: the agent exited 1 naming the ACL. Neither created a data directory
  or a socket, and the real files and running agents did not change.
- **Pending:** hearst was powered off, so the prediction above for it is not
  verified. A refusal for a POSIX.1e extended ACL (UFS mounted with `acls`)
  and a getfacl failure are unit-tested only.
