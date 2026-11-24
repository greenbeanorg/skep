# ADR 0013: Clustering (Milestone 8)

- Status: **Proposed.** Rewritten on 2026-11-24 as the Milestone 8 design,
  after Andy chose the direction (Raft inside Skep) with the review chat on
  2026-11-24. Part A records those decisions in direction. Part B is
  the proposals, revised on 2026-11-24 after Andy's review. Part C holds
  his answers and the questions still open. Not to be merged while
  Proposed.
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

Each item was proposed for review. Andy's answers (Part C) are worked
into the text; items marked "proposed, not decided" still have an open
question in Part C or are new in this revision.

### B1. Join and trust (proposed; answered in Part C)

**Options for who vouches for a node:**

| Option | How | For | Against |
|---|---|---|---|
| a. Cluster CA held by one node | The founding node's `skep-cluster` holds a CA key and signs node certificates | Standard X.509 | A CA key on a node is a cluster-wide secret; revocation needs CRLs or short lifetimes; losing that node loses the CA |
| b. Cluster CA key replicated | Every node can sign | No single holder | Any one compromised node mints identities forever |
| c. Operator's offline CA | Skep never holds the CA key | Strongest custody | Every join and renewal needs the operator's CA; heavy for 0.2 |
| d. **Pinned node keys in the membership** | Each node generates its own key and self-signed certificate. The replicated membership stores, per node: ID, name, address, and the SHA-256 of its public key (SubjectPublicKeyInfo). A peer is accepted only if its key's hash is in the membership the receiver has applied | No CA key anywhere; removal takes effect as soon as a node applies it; same model as `skepctl`'s pinning (ADR 0006) | Custom verification code; a node that is behind does not yet know a newly added peer |

**Options for how a node joins:**

| Option | How | Outcome |
|---|---|---|
| i. Secret join token | A member issues a single-use secret; the new host presents it and is added as a non-voter | **Rejected.** A non-voter receives the whole replicated store, password hashes included, before anyone has confirmed which host it is; and every member must accept a connection from a key it does not know yet, to let the token be presented |
| ii. **Two-way fingerprint exchange** | Both sides learn each other's key pins out of band, before the first connection | **Recommended** (below) |

**Recommendation: d, with the fingerprint exchange ii.**

- **Key and certificate:** ECDSA P-256, generated by `skep-cluster` in its
  own directory (`/var/db/skep/cluster`, owned by the `skep-cluster`
  account, `0700`, key file `0600`) at `init` or `join-request`. The
  certificate is renewed with the same key, so the pin does not change. A
  new key only on `skep-cluster key rotate` (root, local): the new pin is
  added to the node's membership entry, then the old one removed, both
  through the log.
- **Lifetime and renewal:** the certificate's validity does not carry the
  trust (the pin does), so a long validity (397 days, as `skep-api`) and
  automatic renewal with the same key. Go's verifier refuses an expired
  certificate even when the pin matches, so renewal is checked daily and
  its failure is a health warning.
- **How a node joins, in three steps:**
  1. **On the new host, as root:** `skep-cluster join-request` creates the
     node key and prints a *request*: node name, address, key pin. Nothing
     in it is secret. **Conflict with answer 10, recorded rather than
     resolved:** every 0.2 host is already a one-node cluster, so it
     already has a node key from `init`. As written, `join-request`
     would create a second key. The proposal is that it reuses the
     existing key and only creates one on a host without (a fresh install
     before `init`); a new key stays `key rotate`'s job. To be confirmed
     (open question 11).
  2. **On a member** (root locally, or through the API with the new
     `cluster:manage` permission): `skep-cluster admit <request>` commits a
     **pending** membership entry (ID, name, address, pin). It expires
     unused after 30 minutes, and its creation, use and expiry are
     audited (B10). It prints an *admission*: the cluster ID and the
     address and pin of every current member. Nothing in it is secret.
  3. **On the new host, as root:** `skep-cluster join <admission>`. Both
     sides are now pinned before the first connection. Two Raft logs
     cannot be merged, so the host's own one-node cluster state (answer
     10) is exported (B3: users and roles to a root-only file) and
     discarded, and the host joins with an empty log; its guests are not
     touched and are put back into the inventory by reconciliation (B8). The joiner calls
     the join operation on a member; a member that is not the leader
     answers with the leader's address, from the admission's list. The
     leader adds the node as a **non-voter**, replicates to it, and
     **promotes it to voter once it has caught up** (its applied index
     within the leader's commit index by less than one snapshot
     threshold; `hashicorp/raft` has no automatic promotion, so the
     leader asks the non-voter for its applied index over Skep's own
     RPC and promotes it with `AddVoter`).
- **What the listener accepts:** a key that is a member, or a key in an
  unexpired pending entry. Every other key is refused in the handshake. A
  pending key may call the join operation **and nothing else**: no Raft
  RPC, no read. It receives no replicated data before the leader has
  added it as a non-voter. A member that has not yet applied the pending
  entry refuses the joiner like any unknown key; the joiner tries the
  next member in its admission.
- **What a stolen request or admission gives an attacker:** nothing on its
  own. Both contain only public data. Using either needs the private key
  behind the pin, which never leaves the host that made it. The real risk
  is substitution: an attacker who gets an administrator to admit **their**
  request (their key, a plausible name) joins. The administrator must
  compare the pin in the request with `skep-cluster status` on the new
  host's console before admitting it.
- **What a wrong paste does:**
  - A request admitted with a typing error or for the wrong host: the
    pending entry names a key nobody presents; it expires after 30
    minutes and is audited as expired. Nothing joins.
  - A wrong or stale admission on the new host: the joiner pins keys the
    members do not hold (or members that do not know it), every handshake
    fails, and nothing is sent. `join` reports the first refusal and
    names the member.
  - An admission from another cluster (for example a test cluster): the
    host would join that cluster, and under B3 its users would be
    replaced by that cluster's. `join` therefore prints the cluster ID and
    the member names and addresses from the admission, and asks for
    confirmation on `/dev/tty` before it connects. (The cluster's users
    cannot be shown at that point: the admission does not carry them, and
    the joiner receives no data before it is added.) B3's own listing of
    the host's users and guests, and its confirmation, come next, before
    the join operation is called.
- **A removed node:** removing it from the membership removes its pin;
  every node that applies the change refuses it from then on. The removed
  node is not told by Raft (measured, evidence §11), so removal also sends
  it a best-effort notice; its `skep-cluster` stops when it sees itself
  absent from its applied configuration, and it must be wiped
  (`skep-cluster leave --wipe`, root) before it can join again. Removing
  a node that still owns guests is B13.
- **Per-RPC identity:** the receiver checks that each RPC's claimed sender
  is the node whose pinned key presented the connection, from the address
  in its membership entry. `hashicorp/raft`'s `NetworkTransport` does not
  expose the connection to the RPC consumer, so Skep serves inbound RPCs
  itself, as the prototype did, adapting the library's MPL-2.0 handler.
  **The library version is pinned** in `go.mod`, and at every update of
  it the adapted handler is compared with upstream `net_transport.go`
  (wire format, RPC types, heartbeat fast path) before the update is
  merged. The comparison is a checklist item in the update's PR.
- **The service account:** `skep-cluster` runs as its own account, not as
  `skep`: the API runs as `skep`, and the same UID could read the node key,
  which A3 keeps out of the API. A fixed UID and GID, chosen as ADR 0012
  chose 409 (checked against the ports `UIDs`/`GIDs` files).
- **The API's node-to-node key in the membership:** each node's entry
  also pins a key that `skep-api` uses only for node-to-node forwarding
  (B8). It is a separate key from the user-facing certificate's, because
  an operator-supplied user-facing certificate (ADR 0006 decision 6, C)
  may change key at the operator's renewal, which would break the pin.
- **Where `skep-cluster` listens:** one TCP address and port, the node's
  management address and port 8444 by default (a proposal; like 8443 it
  is not registered with IANA). There is no default address: `init` and
  `join-request` require `-address`, and the address in the membership
  entry must equal the listen address. Configuration file
  `/usr/local/etc/skep/cluster.json`, `root:skep-cluster`, `0640`, as
  `api.json` is `root:skep` `0640`: the service can read it and not
  change it.
- **Clocks:** the signed envelopes of B8 carry an expiry, so clocks must
  agree. `skep-cluster` exchanges wall-clock time with each peer on its
  own status RPC and estimates the offset from the round trip. A peer
  more than **5 s** away is a health warning naming it; more than **15 s**
  is a health error, and forwarded requests to and from that peer are
  refused with `503 node_clock_skew` (B8) rather than risking expired or
  over-long envelopes.

**Risks:** custom verification code is security-critical and must be
small and tested; the admission step depends on the administrator
comparing a pin; a node that is far behind refuses a newly admitted
joiner until it catches up from the others; the adapted handler can
drift from upstream if the update checklist is skipped.

**Tests:** repeat every identity test of the prototype (other key, valid
key of another node, non-member, header claiming another node, impostor
at a member's address) against pins; a pending key's Raft RPC and read
refused, its join accepted; an unadmitted key refused in the handshake;
an expired pending entry refused and audited; a wrong admission sends
nothing; promotion happens only after catch-up; a removed node refused by
each member within one apply; key rotation without a refused RPC; clock
offset of 6 s and 16 s gives the warning and the error (unit tests with a
fake clock; changing a lab VM's clock is not pre-approved).

### B2. What one compromised node can do to the others (proposed; answered in Part C)

The plain statement, for review:

- **Root on one node** has that node's cluster key and a voter's full copy
  of the replicated store (including password hashes). It can propose any
  entry, and as a voter it can become leader. So it can create an
  administrator, change roles, or edit the guest inventory, and through
  those control Skep-managed guests, volumes and jails on **every** node:
  create, stop, delete.
- That includes **the guests' data**. Through ordinary Skep operations on
  another node, run as an administrator it created, it can read and
  change the disks of every Skep-managed guest on every node: clone a
  guest's disk or snapshot into a guest of its own (ADR 0010 decision 3),
  attach install media and boot a guest into it, or use the serial
  console (ADR 0009). No root shell on that node is needed for any of
  this.
- It holds **every user's password hash** (the replicated store is on
  every voter, B6), so it can attack all passwords offline. Argon2id
  (64 MiB, 3 passes, ADR 0006 decision 2) slows that down; it does not
  prevent it for weak passwords.
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
(c) defeats "one login". What the signed per-request envelopes (ADR 0004, as amended) buy in
this design: proof of which node a forwarded request came from, its
expiry, and its target, recorded at the receiver (B8). They do not contain
a compromised node or API, because the originating node is the one that
authenticated the user.

**Risks:** an operator may assume nodes are isolated from each other; the
security overview must say they are not, for Skep-managed resources and
the data inside them, and that a compromised node means every password
should be changed.

**Tests:** from one node's `skep-cluster`, an entry that widens another
node's confinement has no effect (the agent ignores it); a compromised
member removed by the majority is refused at once.

### B3. Joining a host that already has guests and users (proposed; answered in Part C)

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
- **Single-node 0.1 installs upgrading to 0.2:** every host is a
  one-node cluster (Part C answer 10); its users and roles move from
  `api.db` into the replicated store once, by migration.

**Risks:** an operator loses a local account they relied on (mitigated by
the listing, the confirmation and the export); the guest pre-check and
the inventory write race a guest created during the join (the join takes
an upgrade-style drain, B7, for its duration).

**Tests:** join langrishe (with guests and users) to a cluster of smith:
guests listed and owned by langrishe, users replaced after confirmation,
export file `0600 root`; a name conflict refuses the join and changes
nothing; guests keep running (their PIDs unchanged).

### B4. Permanent loss of quorum (proposed; answered in Part C)

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
  type, the highest version it can apply. **The label is on every Skep
  entry from the first replicated one** (the node and pending-membership
  entries of 8a and 8b; Raft's own configuration entries are typed by the
  library): a log written before the label existed would have to be told
  apart by guessing. Only the feature-level operation below waits for
  step 8g; until then every build writes version 1 of each type.
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

**Forwarding between nodes on different builds.** During a rolling
upgrade, one node's API may forward a request (B8) to a node on an older
or newer build. The public API refuses unknown JSON fields
(`decodeBody` uses `DisallowUnknownFields`), so a new field forwarded to
an old build already fails, but as a confusing `400`.

| Option | How | For | Against |
|---|---|---|---|
| a. Refuse every forward between different builds | Each node's build is in its membership entry | Simplest | One upgraded node makes every other node's guests unmanageable from it until all are upgraded |
| b. **A versioned forwarding protocol** | The node-to-node endpoints (B8) have their own version, like `/internal/v1`. Each node publishes, in its membership entry, the range of forwarding versions it speaks and the set of public operations its build serves. The forwarding node uses the highest common version, and forwards an operation only if the target's build serves it | A mixed cluster keeps working for everything both builds know | A table of operations per build to maintain |
| c. Forward blindly | – | – | Errors from the old build look like user mistakes |

**Recommendation: b.** A forward that cannot be made (no common
forwarding version, or an operation or field the target's build does not
serve) is refused by the **forwarding** node, before anything is sent,
with `503 node_version_mismatch`, `details` naming the target node and
both builds. It is a 503 because it ends when the upgrade finishes, as
`agent_version_mismatch` does on one node today. The interface shows the
node as "upgrading".

**Options rejected:** upgrading all nodes at once (needs downtime of the
whole cluster), and best-effort skipping of unknown entries (silent
divergence).

**Risks:** the library's wire format changes in a dependency update;
feature-level raise forgotten (the interface shows when it can be raised);
the per-build operation table drifting from the code (it is generated
from the route table, not written by hand).

**Tests:** a cluster on build N, upgrade one node to N+1 (with a new
entry type): no new entry written until the level is raised; raise
refused while a node is on N; a node on N refused after the raise; every
entry in a fresh log carries a type and version; a request using an
operation only N+1 serves, sent to N+1's API for a guest on N, is
refused with `node_version_mismatch` and nothing reaches N.

### B6. Backup and restore of the cluster store (proposed, not decided)

**Proposal:**

- `skep-cluster db backup` writes a consistent copy (a Raft snapshot of
  the applied state plus the membership) next to the other two backups.
  `skep-setup upgrade` backs up three databases instead of two, in the
  same step.
- **The copy holds every user's password hash**, like `api.db`'s backup.
  It is written as the other two are today: by a root process, into
  `/var/db/skep/backup/<label>/` (`0700`), file `0600`, with its
  `.sha256` (`internal/dbbackup`). `skep-cluster db backup` runs as root
  for this, reading the store read-only, as `skep-api db backup` does.
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

### B7. Drain (proposed; answered in Part C)

**One refusal rule in the agent, two sources, two scopes.** In
`tasks.Engine.Submit`, in the same locked step that refuses
`shutting_down` today, the engine refuses a new task when the node is
drained:

| Source | Refuses | Allows |
|---|---|---|
| Upgrade drain | **Every** new task | – |
| Maintenance mode | Every new task **except** guest stop and force-stop: `vm.stop`, `vm.force_stop`, `container.stop` (jails have no force-stop task today) | Those three types |

In both, **cancellation stays allowed**: it is `tasks.Engine.Cancel`, not
a submission, so the rule does not touch it. When both sources are
active, the upgrade drain's scope applies. One error code with a reason:

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

**Risks:** the exception list must name task types, not actions, so a
new stop-like task type is refused in maintenance until it is added
deliberately; an old API in front of a
draining agent passes `node_draining` through unchanged, so the message
itself must say what is happening; the root-only check must be a test,
not a comment.

**Tests:** drain as root works, as `skep` is refused; expiry ends a drain
when setup is killed (`timeout -f` on FreeBSD); a submission racing the
drain is either accepted before it or refused after it, never half; reads
and cancel work while drained; in maintenance `vm.stop`, `vm.force_stop`
and `container.stop` are accepted and `vm.start`, `vm.restart`,
`container.restart` and creates are refused; during an upgrade drain the
stops are refused too; maintenance survives an agent restart and an API
stop; the upgrade path on a lab VM with a running task.

### B8. How a request reaches another node, and who the caller is (proposed, not decided)

**Path:** client → `skep-api` on any node → if the resource's owner (from
the replicated inventory) is another node, the API forwards the request
over **mutual TLS 1.3 to the owner's `skep-api`**, which talks to its own
agent over the socket as today. No agent gets a network listener (ADR
0004). Console streams are forwarded the same way.

**Two listeners in `skep-api`:**

| | User-facing listener (today's) | Node-to-node listener (new) |
|---|---|---|
| Address and port | `listen_address` in `api.json`, default `127.0.0.1:8443` (ADR 0006 decision 7) | `node_listen_address` in `api.json`, the management address and port **8445** by default (a proposal); no default address: written by `skep-cluster init`/`join`, which run as root |
| Client certificate | **Never requested** (as today) | **Required**, and accepted only if its key's pin is a member's node-to-node API key in the applied membership (B1) |
| Server key | Generated or operator-supplied (ADR 0006) | The node-to-node key, pinned in the membership; never operator-supplied |
| Serves | The public API and the web interface; **not** the forwarding endpoints (they are not registered on its mux) | **Only** the forwarding endpoints; no web interface, no login, no session |

Keeping them apart means a browser is never asked for a certificate, a
user-facing request can never reach a forwarding endpoint, and a
node's forwarding identity does not depend on the operator's certificate.

**Identity:** each API presents its node-to-node key, pinned in the
membership (B1). The forwarded request carries a **signed envelope**, signed with the
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
   envelope, which the agent stores in its audit entry (`audit_log.details`,
   which already exists as a JSON column; no schema change).

**Where the envelope is verified (Andy, 2026-11-24, Part C answer 5):** in
the receiving node's API, and recorded in its agent's audit, where it can
be checked later. Verifying in the agent too would need the agent to hold
the cluster's pins, from `skep-cluster` (which A3 keeps away from the
agent) or from its own API (which, compromised, controls the agent
anyway). This changes ADR 0004; the amendment is drafted there
(Proposed).

**What the inventory is for:** routing and cluster-wide name uniqueness.
The owning node's agent remains the truth about its guests (ADR 0004). A
create writes the inventory entry first (quorum; state `creating`, with the
request's idempotency key, which the API supplies when the client sent
none), then submits to the owner's agent, and records
the task ID in the entry.

**Reconciliation, by the owning node, automatically.** The owner's
`skep-api` compares the inventory entries it owns with its agent's guests:

- **when:** when a create's task reaches a terminal state; at every start
  of `skep-api`; and every 60 s;
- **how:** an entry whose guest exists in the agent becomes `present`
  (and takes the agent's ID); an entry in `creating` whose task ended
  `failed`, `cancelled` or `interrupted` and whose guest the agent does
  not have is **removed**, and its name is free again; an entry in
  `creating` with no task ID (the API stopped between the two steps) is
  removed after 5 minutes if the agent still has no such guest, or the
  submission is replayed with the same idempotency key, which returns the
  task if it was created; a guest the agent has and the inventory lacks
  (created before the join, or written by an older build) is added;
- **only with quorum:** every change is a replicated write; without
  quorum the owner waits and retries; the agent is not touched;
- **audited:** each removal or addition is a cluster audit event (B10)
  with the actor `system` and the owner as origin.

No operator is needed, and no other node ever removes an entry it does
not own (except through B13, removing a node).

**Tasks:** a task lives on the node that runs it. Every task already
stores its `node_id`; the API looks a task up on that node, and a lookup
by ID alone asks each member.

**Unreachable owner:** `503 node_unreachable`, naming the node. No quorum
is needed to forward a read or a stop/start of a guest to a reachable
owner (A5).

**Clock skew:** forwards to or from a peer whose clock is more than 15 s
away are refused with `503 node_clock_skew` (B1, "Clocks").

**Risks:** the replay cache and clock skew between nodes (the expiry
allows for skew; clocks run NTP; B1's health warning shows drift before it
matters); forwarding loops if two nodes disagree
on the owner (the receiver refuses a request for a resource it does not
own, with the owner it knows).

**Tests:** a request on smith for a VM on langrishe runs on langrishe with
the user recorded on both sides; a replayed or expired envelope refused; a
role removed on the majority refuses a forwarded request even when the
originating node is stale; the owner down gives `node_unreachable`; the
user-facing listener does not request a client certificate and answers
404 for a forwarding path; the node-to-node listener refuses a
connection without a member's pinned key and serves no public route; a
create whose task fails leaves no inventory entry after reconciliation; an
API killed between the inventory write and the submission leaves no entry
after 5 minutes.

### B9. Sessions and login without quorum (proposed; answered in Part C)

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

### B10. Audit of cluster-level changes (proposed; answered in Part C)

**Options:** (a) the originating API's access log only; (b) the Raft log
itself (lost on compaction); (c) **each node's `skep-cluster` writes an
audit entry when it applies a change**, in its own database, sealed and
hash-chained like the other two logs (ADR 0007).

**Recommendation: c**, plus (a) at the originating node. Every replicated
entry carries the actor, the origin node and the request ID. Each node
records it on apply, so one node's compromise cannot erase the record on
the others. Membership changes, admissions (pending entry created, used, expired),
promotions, recovery, feature-level raises and maintenance mode are cluster
audit events. `skep-cluster audit verify` matches the other two.

**Risks:** a third log to read during an incident; the request ID links
them, as it links the two today.

**Tests:** a user created on smith appears in the cluster audit of all
three nodes with the same request ID; `audit verify` detects an altered
segment.

### B11. Library, stores and timeouts (proposed; answered in Part C)

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
  helper. Accepted for 0.2 (Part C answer 16), with a backlog row to
  follow it.
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
leader, so a witness runs the full service (deferred to the witness
design, Part C answer 19).

**Tests:** the prototype's thirteen measurements repeated against the
product service, with real partitions where Andy allows them (B12).

### B12. Build order and lab plan (proposed; answered in Part C)

Sub-milestones small enough for one run each. Each ends with a PR, CI
green, and the lab test listed, on smith (UFS), langrishe (ZFS) and star
(ZFS, on another Proxmox host). Never on hearst.

| Step | Builds | Lab test |
|---|---|---|
| 8a | `skep-cluster` skeleton: the `skep-cluster` account (fixed UID/GID), rc.d service, `/var/db/skep/cluster`, `cluster.json` (`root:skep-cluster`, `0640`), listen address required, port 8444; node key; `init` (every host a one-node cluster, Part C answer 10); the Raft store with the library version pinned; type-and-version labels on every Skep entry (B5); a Unix socket for `skep-api` with peer-credential check (`skep` only); health | smith: init, restart, health; the socket refuses other UIDs; no listener without `-address`; `skep_agent`/`skep_api` untouched |
| 8b | Join and trust (B1): `join-request`, `admit` (pending entries, 30-minute expiry), `join` with the cluster confirmation, the listener's member-or-pending rule, the inbound handler with per-RPC identity and the upstream-comparison checklist, non-voter then automatic promotion once caught up, remove, `leave --wipe`, removal of a node that owns guests (B13), clock-offset health (B1), cluster audit (B10) | Three nodes; every identity test from the prototype; pending key limited to join; wrong and expired admissions; removal refused within one apply; removal with guests refused without `--forget-guests` |
| 8c | Replicated identity: users and roles in the store, the API's read copy, credential epochs, degraded login without quorum (B9); the 0.1 migration of users and roles | Login on any node; password change revokes sessions on all; a cut-off node's login rules |
| 8d | Guest inventory and owner (A7): the owner's API publishes its guests; cluster-wide names; join with guests and user replacement (B3); automatic reconciliation by the owner (B8) | langrishe joins with guests; names unique; guests keep running; a failed create leaves no entry |
| 8e | Forwarding (B8): the node-to-node listener on port 8445 with the pinned node-to-node keys, signed envelope verified in the receiving API and recorded in the agent's audit, re-authorization, `node_unreachable`, `node_clock_skew`, the versioned forwarding protocol and `node_version_mismatch` (B5); one view in the interface | A VM on langrishe managed from smith's interface; replay refused; the user-facing listener requests no client certificate and serves no forwarding path |
| 8f | Drain (B7): the agent's refusal rule with both scopes, root-only upgrade drain with expiry, maintenance mode persisted in `agent.db`; `skep-setup upgrade` backs up the cluster store (root, `0600`) and drains | Upgrade a lab VM with a running task; kill setup mid-drain, drain expires; stops accepted in maintenance, refused in an upgrade drain |
| 8g | Recovery (B4), the feature-level operation (B5), backup and restore (B6), and the partition suite with pf | Recovery of one survivor; old-epoch node refused; full restore; the prototype's partition cases with real cuts |

**Partition testing with pf (Part C answer 21).** Real cuts with pf on
smith, langrishe and star only, under Andy's conditions:

- rules match **only the cluster port** (8444) between the three lab
  addresses (10.79.20.81–83), in both directions or one, as the case
  needs;
- **never a default-deny rule**; everything else passes;
- a timer that disables pf again is started **before** the rules load
  (time-bounded and killed at the end of the step, as CLAUDE.md requires
  for background processes);
- nothing persistent: no `rc.conf` change (`pf_enable` stays unset), no
  `/etc/pf.conf` written;
- never on hearst.

Two practical points the run must handle, not decided here: loading
rules with `pfctl -f` replaces the main ruleset, so the run first checks
`pfctl -s info` and refuses if pf is already enabled with rules of its
own, and uses an anchor rather than the main ruleset otherwise; and pf
must be loaded (`kldload pf`) if it is not, which the answer is read to
include, but without any `loader.conf` change.

**What the conditions do not cover:** cutting the node-to-node API port
(8445). With rules on 8444 only, a partition stops Raft but not
forwarding between the APIs, so forwarding *during* a partition (B8, A5)
cannot be tested with real cuts under these conditions. Part C lists it
as open.

### B13. Removing a node that still owns guests (proposed, not decided)

A node can be removed (B1) while the inventory still lists guests it
owns: an operator retires a host, or a host is lost for good (B4). There
is no migration in 0.2 (A6), so its guests cannot move.

| Option | How | For | Against |
|---|---|---|---|
| a. Refuse while it owns guests | The operator deletes the guests first (through the owner, while it is reachable) | Nothing is ever lost from the inventory | A lost host can never be removed; deleting guests to retire a host is often not wanted |
| b. Remove and drop its entries | The inventory forgets the guests | Simple | Silent: an operator may not know guests are still running on the removed host |
| c. **Refuse by default; an explicit `--forget-guests`** | Removal lists the guests it owns and refuses. With `--forget-guests` (and the list typed back as a count), the entries are removed in the same log entry as the membership change, each audited | A lost host can be removed; nothing is dropped without being named | Two paths to test |
| d. Keep the entries as orphans | Entries stay, marked "owner removed", read-only, until deleted by hand | Visible | Names stay reserved by guests no node manages; reconciliation (B8) has no owner to ask |

**Recommendation: c.**

- What happens on the removed host: nothing. Its guests keep running
  (unreachable is not dead), its `agent.db` still has them, and its
  `skep-cluster` stops when it sees itself removed (B1). After `leave
  --wipe` it is a one-node cluster again (Part C answer 10), and its own
  reconciliation (B8) puts its guests back into its own inventory.
- If that host later joins again, B3's name check applies: a name taken
  meanwhile by another guest refuses the join.
- `--forget-guests` needs quorum, like every membership change; on a
  cluster without quorum it comes after `recover` (B4), which then names
  the node as dropped.

**Risks:** an operator forgets guests that still run on a host they no
longer watch; the listing, the count typed back and the audit entry name
each one. A host removed while merely cut off (not lost) keeps running
its guests unmanaged until it is wiped or rejoined.

**Tests:** remove langrishe while it owns a VM: refused, listing the VM;
with `--forget-guests` and the count: removed, entries gone, audited on
every node, the VM still running on langrishe; `leave --wipe` on
langrishe: its VM is back in its own inventory; rejoin with a meanwhile
taken name: refused.

## Part C — Answers (Andy, 2026-11-24)

Given with the review chat on 2026-11-24, to the 22 questions of the
previous draft (head `ff265b5`). Part B has been revised to match. The
ADR stays Proposed until the questions still open (below) are answered
and it is accepted.

| # | Question (short) | Answer |
|---|---|---|
| 1 | Trust: pinned node keys or a cluster CA | **Pinned keys** (B1 d) |
| 2 | Joiner a non-voter until its fingerprint is confirmed | **Replaced by the two-way fingerprint exchange** (B1); **automatic promotion once caught up** |
| 3 | Join token lifetime and binding | **Replaced** by the fingerprint exchange; the token is a rejected option in B1 |
| 4 | Accept that root on one node controls Skep-managed resources on every node | **Yes**, with the fuller statement (B2: guests' disks, password hashes) |
| 5 | Where the forwarded request's envelope is verified | **In the receiving API; recorded in the agent's audit** (B8; ADR 0004 amendment, Proposed) |
| 6 | The node's key belongs to `skep-cluster`, not the agent | **Yes** (ADR 0004 amendment, Proposed) |
| 7 | `skep-cluster` as its own account with a fixed UID/GID | **Yes** |
| 8 | Joining a host with users | **Replace them after listing and confirmation** (B3) |
| 9 | Guest names unique across the cluster | **Yes** |
| 10 | Every 0.2 node runs `skep-cluster` | **Yes, every host is a one-node cluster** |
| 11 | Without quorum only admins log in, degraded sessions of one hour | **Yes** (B9) |
| 12 | One error code `node_draining` with a reason | **Yes** (B7) |
| 13 | Upgrade drain expiry 60 s, renewed by setup | **Yes** |
| 14 | Maintenance refuses guest stops too | **No: maintenance allows guest stops** (B7, two scopes) |
| 15 | Library default timeouts and pre-vote on | **Yes** (B11) |
| 16 | `raft-boltdb/v2` with the archived `boltdb/bolt` v1 | **Accept `raft-boltdb/v2`**; a backlog row follows the archived dependency |
| 17 | `skep-cluster recover` as proposed | **Yes** (B4) |
| 18 | Cluster audit in `skep-cluster`'s own database, sealed | **Yes** (B10) |
| 19 | A witness holding the full store and able to lead | **Deferred to the witness design; not in 0.2** |
| 20 | Feature level raised explicitly or automatically | **Explicitly** (B5) |
| 21 | Real partitions in the lab | **Yes, real cuts with pf** on smith, langrishe and star only: rules match only the cluster port between the three lab addresses; never a default-deny rule; a timer set before the rules load disables pf again; nothing persistent in `rc.conf`; never on hearst (B12) |
| 22 | Build order 8a–8g | **Yes** |

### Questions still open after these answers

New in this revision, or left open by it. Each can be answered in a
line.

1. Ports: `skep-cluster` on **8444** and the node-to-node API listener on **8445** (B1, B8), both unregistered like 8443: acceptable?
2. New API contract items: error codes `node_unreachable`, `node_version_mismatch` and `node_clock_skew` (all 503), and the permission `cluster:manage`, given to the built-in `admin` role only: approved?
3. Clock offset thresholds: health warning above **5 s**, error and forwarding refused above **15 s** (B1, B8)?
4. Forwarding between builds: the versioned forwarding protocol, refused at the forwarding node with `node_version_mismatch` (B5 b)?
5. Removing a node that owns guests: refuse by default, `--forget-guests` with the count typed back (B13 c)?
6. Reconciliation timings: every 60 s and at API start; a `creating` entry without a task ID removed after 5 minutes (B8)?
7. The pf conditions allow rules on the cluster port only. Forwarding between APIs (8445) during a partition then cannot be tested with real cuts. Allow rules on 8445 under the same conditions, or keep that case to unit tests?
8. Answer 21 read as including a live `kldload pf` where pf is not loaded (no `loader.conf` change), and an anchor rather than the main ruleset: correct?
9. The fixed UID/GID number for the `skep-cluster` account, chosen and checked against the ports `UIDs`/`GIDs` files in step 8a: may the 8a run pick it, as 409 was, and record it?
10. The `join` confirmation on `/dev/tty` showing the cluster ID and members from the admission before connecting, then B3's listing of the host's users and guests (B1, B3): agreed?
11. `join-request` on a host that is already a one-node cluster reuses its existing node key instead of creating one (B1, step 1): agreed?

## Consequences

- A third service and a third account on every node; three databases to
  back up on upgrade; two new listening ports per node (8444, 8445).
- ADR 0004 changes, after answers 5 and 6: the node key's owner, and where
  forwarded identity is verified. The amendment is drafted in ADR 0004
  (Proposed) and is accepted with this ADR. The agent gains its first
  root-only internal operation (B7).
- The security overview gains a cluster section: the trust model and the
  join exchange, the compromised-node statement (B2, guests' data and
  password hashes included), the staleness window without quorum (B9);
  its open item on signed per-request tokens is reworded per the ADR 0004
  amendment.
- New error codes (`node_draining`, approved; `node_unreachable`,
  `node_version_mismatch`, `node_clock_skew`, open) and a permission
  (`cluster:manage`, open) change the API contract.
- `agent.db` gains the persisted maintenance flag (a migration, B7).
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
