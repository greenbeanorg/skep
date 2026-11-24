# ADR 0013: Clustering (Milestone 8)

- Status: **Proposed.** Rewritten on 2026-11-24 as the Milestone 8 design,
  after Andy chose the direction (Raft inside Skep) with the review chat on
  2026-11-24. Part A records those decisions in direction. Part B is
  proposals for review, none decided. Part C is the questions for Andy.
  Not to be merged while Proposed.
- Date: 2026-11-23 (evaluation draft), 2026-11-24 (rewritten as the design)
- Depends on: [ADR 0004](0004-privilege-boundary.md) (privilege boundary),
  [ADR 0005](0005-agent-task-engine.md) (task engine),
  [ADR 0006](0006-api-split-authentication-tls.md) (API split,
  authentication, TLS), [ADR 0007](0007-retention.md) (audit sealing),
  [ADR 0010](0010-storage-and-jails.md) (allow-lists),
  [ADR 0012](0012-installer.md) (installer, upgrade, UID 409),
  [agent drain proposal](../proposals/agent-drain.md).
- Evidence:
  - [Raft prototype measurements, 2026-11-24](0013-evidence/raft-prototype-20261124.md)
    (`hashicorp/raft` on smith, langrishe and star; branch `probe/m8-raft`).
  - Package queries on langrishe for the rejected options, 2026-11-23
    ([0013-evidence/](0013-evidence/), [appendix](#appendix-evidence-for-the-rejected-options)).

## Summary

Skep 0.2 joins hosts into one cluster with one login and one view. Each
host keeps running its own guests; a request about a guest goes to the node
that owns it. There is no migration, shared storage or HA in 0.2.

A new unprivileged service, `skep-cluster`, runs on every node. It embeds a
Raft library, holds the cluster membership and a small replicated store
(users and roles, membership, the guest inventory with each guest's owner,
cluster-wide settings) and talks to other nodes' `skep-cluster` over mutual
TLS. It never talks to the agent. `skep-api` reads and writes the
replicated store through `skep-cluster` on its own node, and forwards
requests to the API of the owning node. Each node's agent stays the
authority for its own guests and tasks.

The prototype measured `hashicorp/raft` v1.8.0 with pure-Go stores on
three FreeBSD 15.1 VMs: leader failover in 1.7–2.5 s with the library's
default timeouts, a median commit of 5.6–8.2 ms, recovery without help
after a full stop, correct refusals without quorum, and peer identity
checked per RPC. It also showed four things the design must handle: a
write that returns an error may still commit, a removed node is never told,
a witness voter can become leader, and the library's network transport
does not let the receiver check who sent an RPC.

## Context: what exists today

Checked against the code on 2026-11-24 (main at `4bd0c6a`):

- **Two processes, two databases** (ADR 0004). `skep-api` runs as `skep`
  (UID 409, ADR 0012), is the only network listener, and owns `api.db`
  (users, roles, sessions, access audit). `skep-agent` runs as root, listens
  only on `/var/run/skep/agent.sock`, and owns `agent.db` (node identity,
  VMs, storage, jails, tasks, operation audit).
- **The socket admits UIDs 0 and `skep`** (`internalapi.ListenOptions.AllowedUIDs`,
  set in `cmd/skep-agent`). No internal operation is restricted to root
  today; every route is open to both allowed UIDs.
- **Every agent request carries an actor** asserted by the API
  (`Skep-Actor-Id`, `Skep-Actor-Name`); the agent records it and the
  kernel-verified peer UID, and makes no authorization decision (ADR 0004
  decision 4: "multi-node operation is the trigger for moving to signed
  tokens that the agent verifies").
- **Task submission** (`tasks.Engine.Submit`) refuses under the engine's
  lock: `shutting_down` when the engine is not started, `queue_full` when
  the queue is full (an idempotent replay is still answered), and
  `resource_busy` / `idempotency_key_conflict` from the store. Every
  refusal is audited. Tasks are not replicated and are never resumed after
  a restart (ADR 0005). Each task row already stores its `node_id`.
- **Node identity** is a random UUID created in `agent.db` at first start
  (`database.EnsureLocalNode`), independent of the hostname.
- **Guests:** `virtual_machines` and `containers` rows have a `node_id`
  owner column. VM and jail names are each `UNIQUE` within one node's
  `agent.db`.
- **Sessions** are opaque tokens stored hashed in `api.db`; changing a
  password, disabling a user, changing a role, or deleting a user deletes
  that user's sessions in the same transaction (`apidb.UserStore`). Roles
  are one global role per user, built-in `admin`, `operator`, `viewer`
  (ADR 0006 decision 5).
- **TLS:** `skep-api` generates its own ECDSA P-256 key and self-signed
  certificate, renews with the same key, and clients pin the SHA-256 of the
  public key (ADR 0006 decision 6). There is no cluster CA, no node key and
  no node-to-node channel.
- **Confinement is local:** `agent.json` (root-owned, `0600`) allow-lists
  pools, directories, bridges and the management interface, and is not
  editable through the API (ADR 0004 rule 4, ADR 0010).
- **Upgrades** (`internal/setup/upgrade.go`): refuse if the agent has
  pending or running tasks; stop `skep_api`; wait up to `-wait` for tasks;
  back up both databases (`skep-agent db backup`, `skep-api db backup`);
  restart the agent, then the API, each verified.
- **Audit:** two logs, each sealed monthly into a hash-chained archive by
  the process that owns it (ADR 0007).

## Part A — Decided in direction by Andy, 2026-11-24

These are decisions of direction. Their details are in Part B.

### A1. Raft compiled into Skep

The cluster uses a Raft library compiled into a Skep binary. **Corosync,
Pacemaker and an external store (etcd, rqlite) are rejected.**

Reason: Corosync provides membership and messaging but does not replicate
application state, so Skep would still need its own replicated store on
top; it needs C libraries (libqb, kronosnet, nss) reached through cgo or a
helper, and a root daemon with its own keys outside Skep's identity; and
its two-node quorum device (`corosync-qdevice`/`corosync-qnetd`) is not
packaged on FreeBSD (appendix). Pacemaker would overlap Skep's own
controller and pulls Python, bash and glib. An external store is another
daemon to install, secure, upgrade and back up, whose failure is Skep's.

### A2. Pure Go stays a hard requirement

No cgo in any cluster component.

Reason: `GOOS=freebsd` cross-compilation from the Linux development and CI
machines must keep working, as it does for every Skep binary. The
prototype was built with `CGO_ENABLED=0`, including its stores.

### A3. A new unprivileged service, `skep-cluster`

`skep-cluster` holds the membership and the replicated log. It runs
unprivileged, and it **never talks to the agent**.

Reason: `skep-api` is stopped during upgrades (`skep-setup upgrade` stops
it first), and an API stop must not take the node out of the cluster; and
the node's cluster key must stay out of the process that parses browser
traffic. Keeping it away from the agent keeps the agent's single caller
(`skep-api`) and ADR 0004's boundary unchanged.

### A4. Two-node clusters: "no changes while one node is down"

A two-node cluster is supported. While either node is down, nothing that
needs the replicated store changes. A witness voter comes later.

Reason: with two voters Raft needs both for every commit (measured: three
runs, no write committed with one down). The alternative, letting one
survivor decide, is split brain without fencing.

### A5. What a node without quorum may do

A node that cannot reach a majority may:

- serve reads, marked as possibly stale (with the time it last heard from
  the leader);
- let an administrator log in;
- stop or start guests it already owns.

It may **not** create, delete or edit anything.

Reason: an unreachable node is not dead (CLAUDE.md), and its operator must
still be able to see and stop its guests; but anything that changes shared
state would diverge from the majority. **This start rule tightens in
Milestone 9:** a guest under HA management will not be started by a node
without quorum, because the majority may have started it elsewhere.

### A6. Scope of 0.2

One login, one view, many hosts, with each request forwarded to the node
that owns the resource. **No migration, no shared storage, no HA.**

Reason: the smallest step that gives operators one management plane, and
it needs none of the fencing that HA does.

### A7. What is replicated and what is not

Replicated through the log: **users and roles, the membership, the guest
inventory with its owner, and cluster-wide settings.**

Not replicated through the log: **task status** (each node's agent stays
the authority for its own tasks; other nodes ask it) **and sessions.**

Reason: tasks change state many times a second during work and are already
owned by the agent that runs them (ADR 0005); replicating them would put
the agent's hot path behind quorum. Sessions are per node: a login is a
local event, and a replicated session table would be a cluster-wide
credential store.

## Part B — Proposals for review

Each item is **proposed, not decided.**

### B1. Join and trust (proposed, not decided)

**Options for who vouches for a node:**

| Option | How | For | Against |
|---|---|---|---|
| a. Cluster CA held by one node | The founding node's `skep-cluster` holds a CA key and signs node certificates | Standard X.509 | A CA key on a node is a cluster-wide secret; revocation needs CRLs or short lifetimes; losing that node loses the CA |
| b. Cluster CA key replicated | Every node can sign | No single holder | Any one compromised node mints identities forever |
| c. Operator's offline CA | Skep never holds the CA key | Strongest custody | Every join and renewal needs the operator's CA; heavy for 0.2 |
| d. **Pinned node keys in the membership** | Each node generates its own key and self-signed certificate. The replicated membership stores, per node: ID, address, and the SHA-256 of its public key (SubjectPublicKeyInfo). A peer is accepted only if its key's hash is in the membership the receiver has applied | No CA key anywhere; removal takes effect as soon as a node applies it; same model as `skepctl`'s pinning (ADR 0006) | Custom verification code; a node that is behind does not yet know a newly added peer |

**Recommendation: d.**

- **Key and certificate:** ECDSA P-256, generated by `skep-cluster` at
  `init` or `join`, in its own directory (`/var/db/skep/cluster`, owned by
  the `skep-cluster` account, `0700`, key file `0600`). The certificate is
  renewed with the same key, so the pin does not change. A new key only on
  `skep-cluster key rotate` (root, local): the new pin is added to the
  node's membership entry, then the old one removed, both through the log.
- **Lifetime and renewal:** the certificate's validity does not carry the
  trust (the pin does), so a long validity (397 days, as `skep-api`) and
  automatic renewal with the same key; an expired certificate is still
  refused by Go's verifier, so renewal is checked daily and its failure is
  a health warning.
- **How a node joins:** an administrator creates a join token on a member
  (`skep-cluster join-token`, root, local; or through the API with a new
  `cluster:manage` permission). The token holds the cluster ID, one
  member's address and pin (so the joiner verifies the cluster it joins),
  the expected node name, and a 256-bit random secret. The secret is stored
  only as its hash in the replicated store, is single-use, and expires
  after 30 minutes. On the new host, as root: `skep-cluster join <token>`.
  The joiner connects with its own key, proves the secret, and the leader
  commits a membership entry with the joiner's ID, address and pin **as a
  non-voter**. An administrator then confirms the joiner's fingerprint
  (shown by `skep-cluster status` on the joiner and in the web interface)
  to promote it to voter.
- **A removed node:** removing it from the membership removes its pin;
  every node that applies the change refuses it from then on. The removed
  node is not told by Raft (measured, evidence §11), so removal also sends
  it a best-effort notice; its `skep-cluster` stops when it sees itself
  absent from its applied configuration, and it must be wiped (`skep-cluster
  leave --wipe`, root) before it can join again.
- **Per-RPC identity:** the receiver checks that each RPC's claimed sender
  is the node whose pinned key presented the connection, from the address
  in its membership entry. `hashicorp/raft`'s `NetworkTransport` does not
  expose the connection to the RPC consumer, so Skep serves inbound RPCs
  itself (as the prototype did, adapting the library's MPL-2.0 handler).
- **The service account:** `skep-cluster` runs as its own account, not as
  `skep`: the API runs as `skep`, and the same UID could read the node key,
  which A3 keeps out of the API. A fixed UID and GID, chosen as ADR 0012
  chose 409 (checked against the ports `UIDs`/`GIDs` files).
- **The API's key in the membership:** each node's entry also pins its
  `skep-api` public key, for API-to-API forwarding (B8).

**Risks:** custom verification code is security-critical and must be
small and tested; a leaked join token admits a host as a non-voter until
it expires (mitigated by single use, expiry, the expected name, and the
fingerprint confirmation before it votes); a node that is far behind
refuses a newly joined peer until it catches up from the others.

**Tests:** repeat every identity test of the prototype (other key, valid
key of another node, non-member, header claiming another node, impostor
at a member's address) against pins; a removed node refused by each member
within one apply; a replayed, expired, or second use of a join token
refused; promotion refused without confirmation; key rotation without a
refused RPC.

### B2. What one compromised node can do to the others (proposed, not decided)

The plain statement, for review:

- **Root on one node** has that node's cluster key and a voter's full copy
  of the replicated store (including password hashes). It can propose any
  entry, and as a voter it can become leader. So it can create an
  administrator, change roles, or edit the guest inventory, and through
  those control Skep-managed guests, volumes and jails on **every** node:
  create, stop, delete.
- It **cannot** get a root shell or run commands on another node, touch
  anything outside another node's `agent.json` allow-lists (they are local
  and never replicated), cut another node's management interface, or erase
  another node's agent audit log or cluster audit (each node keeps its
  own, B10).
- **A compromised `skep-api` on one node** (not root) can act as any user
  on every node: it authenticates users, so anything it asks for in a
  user's name is believed, locally and when forwarded. It cannot vote or
  append to the log (it has no cluster key), and it cannot reach any agent
  but its own except through another node's API, which re-authorizes.

**Options:** (a) accept the cluster as one trust domain for Skep-managed
resources, state it, and keep the local confinement; (b) require
security-relevant entries (users, roles, membership) to carry a signature
from an administrator's own key, verified by every node; (c) per-node
administrators who cannot act on other nodes.

**Recommendation: (a) for 0.2**, with (b) recorded as possible hardening.
(b) needs client-side keys for administrators, which Skep does not have;
(c) defeats "one login". What ADR 0004's signed per-request tokens buy in
this design: proof of which node a forwarded request came from, its
expiry, and its target, recorded at the receiver (B8). They do not contain
a compromised node or API, because the originating node is the one that
authenticated the user.

**Risks:** an operator may assume nodes are isolated from each other; the
security overview must say they are not, for Skep-managed resources.

**Tests:** from one node's `skep-cluster`, an entry that widens another
node's confinement has no effect (the agent ignores it); a compromised
member removed by the majority is refused at once.

### B3. Joining a host that already has guests and users (proposed, not decided)

**Options:**

| Option | Guests | Users and roles |
|---|---|---|
| a. Only empty hosts join | Refused if any exist | Refused if any beyond the first administrator |
| b. **Import guests, replace users** | Imported into the inventory, owned by the joining node | The cluster's replace the host's, after listing them and a confirmation |
| c. Merge everything | Imported | Merged; name conflicts refused |

**Recommendation: b.**

- **Guests:** the join pre-check lists the host's VMs and jails. Guest
  names become unique across the cluster (today `virtual_machines.name` and
  `containers.name` are unique per node only); a conflict refuses the join and names both guests,
  so the operator renames one first. IDs are UUIDs and do not collide.
  Guests keep running throughout; the join changes no host state.
- **Users and roles:** the host's users, roles and sessions are exported
  to a root-only file, listed by `skep-cluster join`, and replaced by the
  cluster's after the operator confirms. Merging is not proposed: a weak
  local administrator password would silently become a cluster
  administrator.
- **Single-node 0.1 installs upgrading to 0.2:** see Part C (every node a
  one-node cluster is recommended; its users and roles then move from
  `api.db` into the replicated store once, by migration).

**Risks:** an operator loses a local account they relied on (mitigated by
the listing, the confirmation and the export); the guest pre-check and
the inventory write race a guest created during the join (the join takes
an upgrade-style drain, B7, for its duration).

**Tests:** join langrishe (with guests and users) to a cluster of smith:
guests listed and owned by langrishe, users replaced after confirmation,
export file `0600 root`; a name conflict refuses the join and changes
nothing; guests keep running (their PIDs unchanged).

### B4. Permanent loss of quorum (proposed, not decided)

When a majority is gone for good (hosts lost), the survivors cannot commit
anything, including the removal of the lost nodes.

**Options:** (a) none: rebuild the cluster by hand; (b) a root-only, local
`skep-cluster recover` that rewrites the membership on stopped survivors
(the library supports this: `raft.RecoverCluster`); (c) automatic
shrinking after a timeout.

**Recommendation: b.** `skep-cluster recover --keep <node>,... --drop
<node>,...`, run as root on each survivor while `skep-cluster` is stopped:

- every member is named, either kept or dropped; nothing is inferred;
- it asks for the cluster ID to be typed back;
- it writes an audit entry (cluster audit and syslog `daemon.notice`)
  before changing anything, naming the operator UID and both lists;
- it raises a **membership epoch**; nodes of an older epoch are refused,
  so a dropped node that comes back cannot rejoin with its old log and
  must be wiped and joined again;
- (c) is rejected: unreachable is not dead.

**Risks:** two operators recovering both halves of a partition create two
clusters (split brain). Skep cannot prevent this; the epoch makes the
halves refuse each other afterwards, and the documentation must say to
recover only when the dropped nodes are known to be gone.

**Tests:** on three nodes, stop two for good, recover the third alone, it
commits; restart a dropped node: refused (old epoch); wipe and rejoin it.

### B5. Mixed versions during a rolling upgrade (proposed, not decided)

**Proposal:**

- Every log entry has a type and a version. Each build declares, per
  type, the highest version it can apply.
- The cluster's **feature level** is stored in the log. Writers produce
  only entries at or below it. It is raised by an explicit operation
  (`skep-cluster feature-level raise`, or from the interface) that the
  leader accepts only when every member reports support for it.
- **A node that is too old** (it cannot apply the current feature level)
  is refused when it joins or rejoins, with an error naming both levels.
- **A node that is too new** works: it writes at the cluster's level.
- **A node that meets an entry it cannot apply** stops applying and
  serving cluster reads and reports unhealthy, rather than skipping the
  entry. Feature-level gating should make this impossible; this is the
  backstop.
- Upgrades go one node at a time. `skep-cluster` is upgraded and
  restarted on its own (A3), so a node leaves the quorum only for that
  restart. The Raft library's own wire protocol version is pinned and
  changed only with a release note.

**Options rejected:** upgrading all nodes at once (needs downtime of the
whole cluster), and best-effort skipping of unknown entries (silent
divergence).

**Risks:** the library's wire format changes in a dependency update;
feature-level raise forgotten (the interface shows when it can be raised).

**Tests:** a cluster on build N, upgrade one node to N+1 (with a new
entry type): no new entry written until the level is raised; raise
refused while a node is on N; a node on N refused after the raise.

### B6. Backup and restore of the cluster store (proposed, not decided)

**Proposal:**

- `skep-cluster db backup` writes a consistent copy (a Raft snapshot of
  the applied state plus the membership) next to the other two backups.
  `skep-setup upgrade` backs up three databases instead of two, in the
  same step.
- **Restore is disaster recovery, not an undo.** Restoring an old copy on
  one node of a running cluster is pointless: the leader overwrites it.
  A cluster restore is: stop every node, restore the copy on one, `recover`
  it alone (B4), wipe and rejoin the others. This is documented and
  audited like `recover`.
- Each node's agent database stays the authority for its guests and
  tasks, so a restored cluster store that lists a guest the agent does not
  have (or misses one it has) is reconciled from the agents, never the
  other way round.

**Risks:** an operator expects a restore to roll back a user change; the
documentation must say it does not on a live cluster.

**Tests:** back up, change users, full restore on one node, rejoin the
others: the old users are back everywhere and the agents' guests are
reconciled into the inventory.

### B7. Drain (proposed, not decided)

**One refusal rule in the agent, two sources.** In `tasks.Engine.Submit`,
in the same locked step that refuses `shutting_down` today, the engine
refuses a new task when the node is drained. One error code with a reason:

`503 node_draining`, `details.reason` = `upgrade` | `maintenance`, plus
who set it and, for `upgrade`, when it expires.

- **Source 1, the local upgrade drain** ([agent drain proposal](../proposals/agent-drain.md),
  option B): an internal operation allowed **only for peer UID 0** (the
  first root-only operation on the socket), held **in memory**, with an
  **expiry** (60 s proposed) that `skep-setup` renews while it waits and
  backs up. An expired drain ends by itself and is audited; an agent
  restart ends it. It needs **no quorum** and does not involve
  `skep-cluster`. `skep-setup upgrade` drains instead of stopping
  `skep_api`, keeping today's API stop as the fallback for an agent
  without the operation.
- **Source 2, node maintenance mode:** set and cleared through the
  cluster (a replicated per-node flag, persistent until an operator clears
  it). `skep-api` applies it to its own agent with a second internal
  operation (peer `skep`, actor required, audited); the agent persists it
  in `agent.db` so it survives restarts of either process. Setting or
  clearing it needs quorum, like every replicated change (A5).
- **What stays available while drained:** reads, running tasks to their
  end, cancellation, idempotent replays of existing tasks (as with
  `queue_full`). The agent's own scheduled work (retention) is skipped and
  logged while drained, so an upgrade wait can end.
- `docs/proposals/agent-drain.md` points here.

**Options considered:** two mechanisms with separate codes (rejected:
the proposal's review asked for one rule); maintenance in memory only
(rejected: a restart would silently end it).

**Risks:** maintenance refuses all new tasks, including a guest stop an
operator wants during maintenance (see Part C); an old API in front of a
draining agent passes `node_draining` through unchanged, so the message
itself must say what is happening; the root-only check must be a test,
not a comment.

**Tests:** drain as root works, as `skep` is refused; expiry ends a drain
when setup is killed (`timeout -f` on FreeBSD); a submission racing the
drain is either accepted before it or refused after it, never half; reads
and cancel work while drained; maintenance survives an agent restart and
an API stop; the upgrade path on a lab VM with a running task.

### B8. How a request reaches another node, and who the caller is (proposed, not decided)

**Path:** client → `skep-api` on any node → if the resource's owner (from
the replicated inventory) is another node, the API forwards the request
over **mutual TLS 1.3 to the owner's `skep-api`**, which talks to its own
agent over the socket as today. No agent gets a network listener (ADR
0004). Console streams are forwarded the same way.

**Identity:** each API presents its own key, pinned in the membership
(B1). The forwarded request carries a **signed envelope**, signed with the
originating API's key: user ID, the user's credential epoch (B9), request
ID, idempotency key, target node, method, path, a hash of the body, and an
expiry of 30 s, plus a nonce. The receiving API:

1. checks the signature against the origin's pinned API key, the target
   (itself), the expiry, and the nonce (a replay cache for the expiry
   window);
2. loads the user from its own copy of the replicated store and refuses a
   disabled user or a stale credential epoch;
3. **authorizes again** with its own copy of the roles: permissions travel
   by reference (the user), not by value;
4. sends the request to its agent with the actor, the origin node, and the
   envelope, which the agent stores in its audit entry.

**Whether the agent verifies the envelope:** ADR 0004 decision 4 says the
agent verifies signed tokens once requests originate on another node.
Verifying in the agent needs the agent to hold the cluster's pins, from
`skep-cluster` (which A3 keeps away from the agent) or from its own API
(which, compromised, controls the agent anyway). The proposal is to verify
in the receiving API and record the envelope in the agent's audit, where
it can be checked later. This changes ADR 0004 decision 4 and is put to
Andy (Part C).

**What the inventory is for:** routing and cluster-wide name uniqueness.
The owning node's agent remains the truth about its guests (ADR 0004): a
create writes the inventory entry (quorum), then submits to the owner's
agent; a failed create marks the entry failed and an operator removes it.

**Tasks:** a task lives on the node that runs it. Every task already
stores its `node_id`; the API looks a task up on that node, and a lookup
by ID alone asks each member.

**Unreachable owner:** `503 node_unreachable`, naming the node. No quorum
is needed to forward a read or a stop/start of a guest to a reachable
owner (A5).

**Risks:** the replay cache and clock skew between nodes (the expiry
allows for skew; clocks run NTP); forwarding loops if two nodes disagree
on the owner (the receiver refuses a request for a resource it does not
own, with the owner it knows).

**Tests:** a request on smith for a VM on langrishe runs on langrishe with
the user recorded on both sides; a replayed or expired envelope refused; a
role removed on the majority refuses a forwarded request even when the
originating node is stale; the owner down gives `node_unreachable`.

### B9. Sessions and login without quorum (proposed, not decided)

Sessions stay local (A7). Logins check the password against the node's
copy of the replicated users, which may be stale on a node without quorum.

**Proposal:**

- `skep-api` keeps a read copy of the replicated users and roles in
  `api.db`, with the log index it reflects, so a login works while
  `skep-cluster` restarts.
- Every replicated user carries a **credential epoch**, raised by a
  password change, a disable, a role change or a delete. A session stores
  the epoch at login. When a node applies an entry that raises a user's
  epoch, it deletes that user's sessions (ADR 0006's rule, applied on
  catch-up as well as locally).
- **Without quorum** (no leader contact for longer than the election
  timeout): only users whose last applied role is `admin` may log in
  (A5). Such sessions are marked degraded, limited to reads and stop/start
  of owned guests, and expire after at most one hour. Existing sessions
  keep working within the same limits.
- **What a missed change means**, stated plainly: if a password was
  changed or an administrator removed on the majority while this node was
  cut off, the old password or the removed administrator still works **on
  this node only**, for reads and stop/start of its own guests, until the
  node catches up; then the sessions are deleted. The host's root can
  always use the local `skep-api admin` command.

**Options rejected:** refuse all logins without quorum (an operator cannot
stop guests on an isolated node through Skep); replicate sessions (A7).

**Risks:** the window above; it is bounded by the reduced action set and
the one-hour limit, not closed.

**Tests:** on a node cut off from the other two: admin login works and is
degraded, operator login refused, create refused, stop/start of an owned
guest works; change the admin's password on the majority, heal: the
minority's sessions for that user are gone.

### B10. Audit of cluster-level changes (proposed, not decided)

**Options:** (a) the originating API's access log only; (b) the Raft log
itself (lost on compaction); (c) **each node's `skep-cluster` writes an
audit entry when it applies a change**, in its own database, sealed and
hash-chained like the other two logs (ADR 0007).

**Recommendation: c**, plus (a) at the originating node. Every replicated
entry carries the actor, the origin node and the request ID. Each node
records it on apply, so one node's compromise cannot erase the record on
the others. Membership changes, join tokens (created, used, expired),
promotions, recovery, feature-level raises and maintenance mode are cluster
audit events. `skep-cluster audit verify` matches the other two.

**Risks:** a third log to read during an incident; the request ID links
them, as it links the two today.

**Tests:** a user created on smith appears in the cluster audit of all
three nodes with the same request ID; `audit verify` detects an altered
segment.

### B11. Library, stores and timeouts (proposed, not decided)

**Recommendation, from the measurements:**

- **Library: `github.com/hashicorp/raft` v1.8.0** (MPL-2.0). Pure Go, it met
  every requirement tested: elections, commits, snapshots, membership,
  recovery after a full stop, pre-vote. It brings MIT and MPL-2.0
  dependencies (evidence §1). MPL-2.0 is file-level: unmodified use in a
  BSD-licensed project is fine; Skep's adapted inbound handler is an
  MPL-2.0 file and must say so. `go.etcd.io/raft` was not prototyped.
- **Stores:** `raft-boltdb/v2` (bbolt) for the log and stable store, the
  library's file snapshot store. Both pure Go. Note: `raft-boltdb/v2`
  still links the archived `github.com/boltdb/bolt` v1 for its migration
  helper (Part C asks whether to accept it or use another store).
- **Timeouts: keep the library defaults for 0.2**: heartbeat and election
  timeout 1 s, leader lease 500 ms, commit timeout 50 ms. Measured with
  them: failover 1.7–2.5 s, a frozen or isolated leader stepping down
  within 0.45–0.51 s of losing contact, full-cluster restart 1.1–1.7 s.
  0.2 has no HA, so nothing needs faster failover, and round trips of
  0.3–0.65 ms leave a wide margin against spurious elections on loaded
  hosts. Revisit in Milestone 9 under guest load.
- **Pre-vote on** (the default). Measured with it off: a node returning
  from a 20 s cut had raised its term by 12–13 and deposed a healthy
  leader for 1.2–2.3 s, every time.
- **Writes are idempotent by key.** A write that returns an error may
  still have committed (evidence §7, three of three runs). Every
  replicated write carries an operation ID, applying the same ID twice is
  a no-op, and an error is reported as "outcome unknown", like ADR 0005's
  idempotency keys.
- **A returning node may take up to about 10 s to be served** (the
  leader's replication back-off, evidence §10). Acceptable for 0.2.

**Risks:** HashiCorp's licence for future versions (the library is MPL-2.0
today; a change would mean pinning or forking); the witness can become
leader, so a witness runs the full service (Part C).

**Tests:** the prototype's thirteen measurements repeated against the
product service, with real partitions where Andy allows them (B12).

### B12. Build order and lab plan (proposed, not decided)

Sub-milestones small enough for one run each. Each ends with a PR, CI
green, and the lab test listed, on smith (UFS), langrishe (ZFS) and star
(ZFS, on another Proxmox host).

| Step | Builds | Lab test |
|---|---|---|
| 8a | `skep-cluster` skeleton: account, rc.d service, `/var/db/skep/cluster`, node key, `init` (a one-node cluster), the Raft store, a Unix socket for `skep-api` with peer-credential check (`skep` only), health; nothing replicated yet | smith: init, restart, health; the socket refuses other UIDs; `skep_agent`/`skep_api` untouched |
| 8b | Join and trust (B1): join tokens, pinned membership, the inbound handler with per-RPC identity, non-voter then promote, remove, `leave --wipe`, cluster audit (B10) | Three nodes; every identity test from the prototype; removal refused within one apply |
| 8c | Replicated identity: users and roles in the store, the API's read copy, credential epochs, degraded login without quorum (B9); the 0.1 migration | Login on any node; password change revokes sessions on all; a cut-off node's login rules |
| 8d | Guest inventory and owner (A7): the owner's API publishes its guests; cluster-wide names; join with guests (B3) | langrishe joins with guests; names unique; guests keep running |
| 8e | Forwarding (B8): API-to-API mutual TLS, signed envelope, re-authorization, origin in the agent's audit, `node_unreachable`; one view in the interface | A VM on langrishe managed from smith's interface; replay refused |
| 8f | Drain (B7): the agent's refusal rule, root-only upgrade drain with expiry, maintenance mode; `skep-setup upgrade` backs up the cluster store and drains | Upgrade a lab VM with a running task; kill setup mid-drain, drain expires |
| 8g | Recovery (B4), feature levels (B5), backup and restore (B6), and the partition suite | Recovery of one survivor; old-epoch node refused; full restore |

**Partition testing:** the prototype simulated partitions inside the
process. The product tests should use real cuts. That needs Andy's
approval for a method on the lab VMs (pf rules or a test interface; never
`vtnet0`), or must stay simulated and say so.

## Part C — Open questions for Andy

Each can be answered in a line.

1. Trust: pinned node keys in the replicated membership (B1 d, recommended), or a cluster CA?
2. Joins: a new node stays a non-voter until an administrator confirms its fingerprint (recommended), or promote automatically after it catches up?
3. Join tokens: single-use, 30-minute expiry, bound to an expected node name: acceptable?
4. Accept, and state in the security overview, that root on one node can control Skep-managed resources on every node (B2 a)?
5. ADR 0004 decision 4: verify forwarded requests' signed envelopes in the receiving API and record them in the agent's audit (recommended), or also verify in the agent?
6. Amend ADR 0004's "the node's mutual-TLS key belongs to the agent" to "belongs to `skep-cluster`", as A3 implies?
7. `skep-cluster` as its own account with a fixed UID/GID (chosen like 409): agreed?
8. Joining a host with users: replace them by the cluster's after a listing and confirmation (recommended), or allow only empty hosts to join?
9. Guest names unique across the whole cluster (join refused on a conflict)?
10. Every 0.2 node runs `skep-cluster`, a single host being a one-node cluster (recommended), or keep a separate no-cluster mode?
11. Without quorum: only users whose last known role is `admin` may log in, with degraded sessions limited to one hour (B9)?
12. One error code `node_draining` (503) with `reason` `upgrade` or `maintenance`, replacing the proposal's `agent_draining`?
13. Upgrade drain expiry of 60 s, renewed by `skep-setup` while it works: acceptable?
14. Maintenance mode refuses every new task, guest stops included (recommended for one simple rule), or should it allow guest stops?
15. Keep the library's default timeouts (1 s / 1 s / 500 ms) and pre-vote on for 0.2?
16. Store: accept `raft-boltdb/v2` with its archived `boltdb/bolt` v1 dependency, or use `raft-wal` or a small Skep store instead (to be measured)?
17. `skep-cluster recover`: every member named as kept or dropped, cluster ID typed back, dropped nodes must be wiped to rejoin: acceptable?
18. Cluster audit in `skep-cluster`'s own database with ADR 0007 sealing (B10 c)?
19. A future witness holds the full replicated store, password hashes included, and can become leader: acceptable, or should the witness wait for a design that keeps it from leading?
20. Feature level raised by an explicit operator action (recommended), or automatically once every member supports it?
21. May the product's partition tests use pf rules or a test interface on smith, langrishe and star, or stay simulated?
22. The build order 8a–8g, starting with 8a?

## Consequences

- A third service and a third account on every node; three databases to
  back up on upgrade.
- ADR 0004 needs an amendment once Part C questions 5 and 6 are
  answered: the node key's owner, and where forwarded identity is
  verified. The agent gains its first root-only internal operation (B7).
- The security overview gains a cluster section: the trust model, the
  compromised-node statement (B2), the staleness window without quorum
  (B9).
- New error codes (`node_draining`, `node_unreachable`) and a permission
  (`cluster:manage`) change the API contract and need Andy's approval with
  this ADR.
- Replicated writes need operation IDs and an "outcome unknown" answer.

## Appendix: evidence for the rejected options

Kept from the 2026-11-23 evaluation draft. Package queries on langrishe
(FreeBSD 15.1-RELEASE-p7, amd64, `quarterly` repository, 2026-11-23 UTC),
logs in [0013-evidence/](0013-evidence/). Nothing was installed.

| Package **[pkg]** | Version | Origin | Licence (port metadata) | Depends on |
|---|---|---|---|---|
| corosync3 | 3.1.10 | net/corosync3 | BSD3CLAUSE | kronosnet, libqb, nspr, nss |
| corosync2 | 2.4.6_2 | net/corosync2 | – | (older major) |
| kronosnet | 1.26_2 | devel/kronosnet | GPLv2 | gcc14 (runtime libs), liblz4, libqb, libxml2, lzo2, nspr, nss, zstd |
| libqb | 2.0.10 | devel/libqb | – | – |
| pacemaker2 | 2.1.11 | net/pacemaker2 | GPLv2 | bash, corosync3, glib, gnutls, libxml2, libxslt, python312, resource-agents, … |
| crmsh | 4.6.0_6 | net-mgmt/crmsh | – | – |
| fence-agents | 4.12.1_1 | net-mgmt/fence-agents | – | python312, py312-pexpect, py312-pycurl, py312-requests |
| resource-agents | 4.13.0_2 | net-mgmt/resource-agents | – | – |

**Not packaged [pkg]:** `pcs`, `sbd` (watchdog-based fencing),
`corosync-qdevice` / `corosync-qnetd` (the quorum device for two-node
clusters), `booth`, `sanlock`.

Packaged Raft-based systems **[pkg]**: `coreos-etcd36` 3.6.14_3
(APACHE20), `coreos-etcd35`, `coreos-etcd34`, `consul` 2.0.4_3 (MPL20),
`nomad` 1.9.6_15, `rqlite` 10.3.6_3 (MIT). They showed that Go Raft
implementations build on FreeBSD 15; none is used.

FreeBSD base facilities noted for Milestone 9 **[base]**: `/dev/fido` and
`watchdogd(8)` exist on langrishe (whether a Proxmox VM's watchdog resets
the machine is untested); CARP is in base.
