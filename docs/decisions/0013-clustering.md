# ADR 0013: Clustering (Milestone 8)

- Status: **Accepted** (Andy, 2026-11-24). Rewritten on 2026-11-24 as the
  Milestone 8 design, after Andy chose the direction (Raft inside Skep)
  with the review chat on 2026-11-24. Part A records those decisions in
  direction. Part B is the design as proposed and then revised by his
  answers and the final review. Part C holds the answers and the final
  review; no question is open.
- **Amendment 1 (2026-11-24)** is **Accepted** (Andy, 2026-11-24). It is
  at the end of this document and is worked into B1, B2, B3, B4, B6 and
  B13, each changed passage marked "Amended (amendment 1)". Its open
  question (what "has applied" means after a restart) was answered with
  option a: the durable applied-membership file.
- **Amendment 2 (2026-11-25)** is **Proposed** (Andy accepts it by
  approving the merge of the step 8b-2a PR). It is at the end of this
  document and records what the build of removal, `leave --wipe`, `join
  --abandon` and `listen -none` changed or added to the accepted text;
  the passages it changes are marked "Amended (amendment 2)".
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
TLS 1.3, on clustered nodes only. It never talks to the agent. `skep-api` reads and writes the
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

Each item was proposed for review. Andy's answers and the final review
(Part C) are worked into the text.

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
  account, `0700`, key file `0600`) at `init`. `join-request` on a host
  that is already a one-node cluster **reuses that key** (Part C,
  follow-up answer 11) and creates one only on a host without. A wiped
  node (`leave --wipe`) gets a **new** key, so its old pin is never valid
  again. The certificate is renewed with the same key, so the pin does
  not change. Otherwise a new key only on `skep-cluster key rotate`
  (root, local): the new pin is added to the node's membership entry,
  then the old one removed, both through the log. **Amended (amendment
  1):** the old pin is removed only when **every member** in the Skep
  membership has applied the entry that added the new one (asked over
  Skep's own status RPC, as for a promotion, below). Until then both
  pins are accepted and health on every member names the member being
  waited for. With a member down, a key rotation cannot finish until
  that member returns or is removed.
- **Lifetime and renewal:** the certificate's validity does not carry the
  trust (the pin does), so a long validity (397 days, as `skep-api`) and
  automatic renewal with the same key. Go's verifier refuses an expired
  certificate even when the pin matches, so renewal is checked daily and
  its failure is a health warning.
- **The node ID:** one ID per node, the UUID the agent created in
  `agent.db` at first start (`database.EnsureLocalNode`), the same one
  guest and task rows carry as `node_id`. The service never talks to the
  agent (A3), so the root-run `skep-cluster init` reads it over the agent
  socket as root, as `skep-setup` reads the agent, and writes it to
  `cluster.json`. A wipe does not change it.
- **Two records, not one.** Skep's membership entries (ID, name, address,
  the two pins, a state) are ordinary Skep log entries applied by Skep's
  state machine. Raft's configuration (which servers exist and which
  vote) is a separate kind of log entry, written by the library's
  `AddNonvoter`, `AddVoter` and `RemoveServer`. The library accepts one
  configuration change at a time and no application data in it (checked
  in v1.8.0, `raft.go` `configurationChangeChIfStable` and
  `configuration.go`), so the two cannot be one entry. Their order and
  every crash point are below ("Order and crash points").
- **How a node joins, in three steps:**
  1. **On the new host, as root:** `skep-cluster join-request -address
     <addr>` prints a *request*: node ID, node name, address, the node
     key's pin, and the pin of `skep-api`'s node-to-node key (below).
     Nothing in it is secret. It refuses unless the local Raft
     configuration is this node alone, and it opens the listeners (below,
     "Where `skep-cluster` listens").
  2. **On a member** (root locally, or through the API with the
     `cluster:manage` permission; B2 says why the API keeps it):
     `skep-cluster admit <request>` commits a **pending** entry (ID,
     name, address, both pins, and an expiry time 30 minutes after the
     leader's clock at the proposal). It prints an *admission*: the
     cluster ID and the address and pin of every current member. Nothing
     in it is secret.
     - `admit` **refuses** an ID, name, address or pin that a member or
       another pending entry already has, and refuses on a node that has
       no listen address (the founding node must be given one first;
       below).
     - **Expiry is a log entry.** When the leader's clock passes a
       pending entry's expiry time it proposes an entry that removes it;
       the replicated state changes only by that entry, so every node's
       state stays the same. A listener may refuse a pending key
       **earlier**, by its own clock, once the expiry time has passed
       there; it never accepts one after the expiry entry is applied.
     - **Withdrawal:** an administrator can withdraw a pending entry
       before it is used (`skep-cluster admit --withdraw <name>`, root
       locally or `cluster:manage` through the API).
     - Creation, use, expiry and withdrawal are audited (B10).
  3. **On the new host, as root:** `skep-cluster join <admission>`. Both
     sides are now pinned before the first connection. In order:
     1. It refuses unless the local Raft configuration is this node
        alone.
     2. It prints the cluster ID and the member names and addresses from
        the admission, then the host's own users and guests (B3), and
        asks for confirmation on `/dev/tty` (follow-up answer 10).
     3. The **command** (root), not the service, takes a drain with the
        reason `join` on the local agent (B7) and reads the guest list
        (ID, name, kind) over the agent socket as root.
     4. Two Raft logs cannot be merged. The host's own one-node state
        (answer 10) is **moved aside, not deleted**
        (`/var/db/skep/cluster/standalone.<UTC time>/`, still owned by
        the `skep-cluster` account); the users and roles in it are also
        written to a root-only file (B3). **Amended (amendment 1):** the
        applied-membership file moves aside with it. **Amended
        (amendment 2):** the store, the snapshots and that file are in
        one directory, `/var/db/skep/cluster/state/`, and moving them
        aside is one rename of `state/` to `standalone.<UTC time>`
        (putting it back is one rename back). The service starts
        with an empty log, no applied-membership file and the
        admission's pins.
     5. The service calls the join operation on a member, **sending the
        guest list**; a member that is not the leader answers with the
        leader's address, from the admission's list. The leader checks
        the guest names against the inventory (B3) and refuses on a
        conflict before anything is written. Otherwise it follows "Order
        and crash points" below. **Amended (amendment 1):** the join
        call's answer carries the address and pins of **every current
        member entry** (`joining` ones included; public data, as in an
        admission), so a joiner whose admission is older than a newer
        member knows that member. The joiner accepts those pins with
        the admission's.
     6. When the node has applied a membership that lists it as a
        member, the admission's pins are dropped, every local session is
        deleted (B3), the moved-aside state is deleted, and the drain is
        released. The guests are already in the inventory, written with
        the member entry.
     7. **Amended (amendment 1): the join has a point of no return**,
        the moment the leader applies join step 1 (below).
        - **Before it**, a failed join puts the one-node state back,
          restarts the service on it and releases the drain. "Before"
          means the command **knows** step 1 was not applied: every
          handshake failed and nothing was sent, or the leader answered
          with a refusal.
        - **After it**, and whenever the outcome of the join call is
          unknown (B11), the `join` command **never puts the one-node
          state back by itself**. The node keeps retrying with its
          admission pins and its (possibly partial) log, and the
          command says so and how to give up.
        - **To give up**, an administrator first removes the node on a
          member (B13), then runs **`skep-cluster join --abandon`**
          (root) on the host. It restores the moved-aside state only
          if **at least one member of the admission answers and none
          that answers accepts this node's key**; otherwise it refuses
          and changes nothing. **Amended (amendment 2):** it asks the
          nodes of the stored join answer too, writes the phase
          `abandoning` to `join.json` before any file step, and a run
          after a crash finishes without asking again.
        - **Why:** a host that restored its one-node state while the
          cluster still counts it as a voter would never vote again. A
          cluster of two members and that voter would need two of
          three votes for every commit, including the one that removes
          it: one more member down and it is without quorum for good,
          and a one-member cluster that had promoted it is without
          quorum at once.
- **Order and crash points.** The leader does every step; a new leader
  finishes what the last one left. It checks when it gains leadership
  and every 10 s after.

  *Join:*

  | Step | Record | If the leader stops after it |
  |---|---|---|
  | 1 | **One Skep entry** that consumes the pending entry and creates the member entry in state `joining`, with the node's guests (B3). Applying it checks again that the pending entry exists and the names are free; if not it changes nothing and the join is refused | The joiner repeats the call with the same operation ID (B11): a no-op if it was applied. The new leader finds a `joining` member that is not in the Raft configuration and continues at step 2 |
  | 2 | `AddNonvoter` (Raft configuration). The leader now replicates to the joiner; this is the first replicated data it receives | The new leader finds a `joining` non-voter and continues at step 3 |
  | 3 | **Amended (amendment 1).** Catch-up and the promotion rule: the leader asks the non-voter for its applied index (the state machine's own, never the library's `AppliedIndex`; below, "Per-RPC identity") over Skep's own status RPC until it is within one snapshot threshold of the commit index (`hashicorp/raft` has no automatic promotion), **and** asks every member in the Skep membership (`joining` and `removing` ones included) the same way until each has applied the step 1 entry that created this member entry. Only then `AddVoter`. Until then the node is a working non-voter, and health on every member names the member being waited for | The new leader repeats both checks and the `AddVoter`; on a server that already votes it changes nothing |
  | 4 | A Skep entry that sets the member's state to `member` | The new leader finds a `joining` voter and proposes it |

  **Amended (amendment 1):** a new leader continues a join **only for a
  member in state `joining`**, never for one in state `removing`
  (below). Before, an interrupted removal of a joining member would
  have been taken for an unfinished join and the node added again.

  A joiner that stops part way starts again with the admission's pins
  and its (possibly partial) log; the leader keeps replicating to it.

  **Amended (amendment 1): no member ever meets a leader it does not
  know.** A non-voter never stands for election (checked in v1.8.0,
  `raft.go` `runFollower`: a node without a vote in its latest
  configuration does not become a candidate), so a node can lead only
  after step 3, and step 3 waits until every member has applied the
  entry that carries its pins. **The consequence: with a member down, a
  join cannot finish** (the node stays a working non-voter) **until
  that member returns or is removed.** "Has applied" is durable across
  a restart: it is the applied-membership file (amendment 1, "Applied
  membership on disk"), and the applied index the leader asks for is
  the state machine's own (below, "Per-RPC identity").

  *Removal:*

  | Step | Record | If the leader stops after it |
  |---|---|---|
  | 0 | **Amended (amendment 1).** **One Skep entry** that sets the member's state to `removing` and records, when `--forget-guests` was given, the guest entries named (B13). It does **not** change which keys are accepted: the node is a member like before until step 1. Applying it checks again that the member exists and, without `--forget-guests`, that it owns no guest entries; if not it changes nothing and the removal is refused | Before it committed: nothing has changed, the administrator's command reports "outcome unknown" (B11) and is run again with the same operation ID. After: the new leader finds a `removing` member and continues at step 1 |
  | 1 | `RemoveServer` (Raft configuration). From the moment a node has this entry it refuses the removed node's RPCs (below, "Per-RPC identity"). A leader that finds **itself** `removing` first hands leadership to another voter (the library's `LeadershipTransfer`) and the new leader does the removal | The new leader finds a `removing` member; `RemoveServer` for a server already absent changes nothing (checked in v1.8.0, `configuration.go` `nextConfiguration`). It continues at step 2 |
  | 2 | **One Skep entry** that removes the member entry and both pins and the guest entries that step 0 recorded (B13) | **Amended (amendment 1).** The new leader finds a `removing` member that is absent from the committed Raft configuration and proposes this entry itself, guests included: the administrator's choice is in the log since step 0. Nothing is run again and no health report asks for it |

  Between steps 1 and 2 the node's pins are still in the Skep
  membership, and it is refused all the same, because acceptance needs
  both records (next bullet). **Amended (amendment 1):** nodes dropped
  by `recover` (B4) are absent from the Raft configuration without a
  `removing` entry; B4 says how their removal is finished.
- **What the listener accepts:** TLS 1.3 only, with a client certificate
  whose key is pinned in a member entry (`joining`, `member` or, since
  amendment 1, `removing`) or in a pending entry. Every other key is refused in the handshake. A
  pending key may call the join operation **and nothing else**: no Raft
  RPC, no read. It receives no replicated data before step 2 above. A
  member that has not yet applied the pending entry refuses the joiner
  like any unknown key; the joiner tries the next member in its
  admission. **When a node applies a removal, a withdrawal or a pending
  entry's expiry, it closes that key's open connections.** **Amended
  (amendment 1):** "the membership the node has applied" is, after a
  restart, the applied-membership file until the state machine has
  caught up with it (below, "Per-RPC identity"). The file holds no
  pending entries, so **after a restart a pending key is refused until
  the node has caught up**; the joiner tries the next member in its
  admission, as for a member that has not yet applied the pending
  entry.
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
    minutes and is audited as expired, or is withdrawn. Nothing joins.
  - A wrong or stale admission on the new host: the joiner pins keys the
    members do not hold (or members that do not know it), every handshake
    fails, and nothing is sent. `join` reports the first refusal, names
    the member, and puts the one-node state back.
  - An admission from another cluster (for example a test cluster): the
    host would join that cluster, and under B3 its users would be
    replaced by that cluster's. That is why `join` prints the cluster ID
    and the member names and addresses from the admission and asks for
    confirmation before it connects. (The cluster's users cannot be shown
    at that point: the admission does not carry them, and the joiner
    receives no data before it is added.)
- **A removed node:** removal is the steps above (three since amendment
  1); every node that has the `RemoveServer` entry refuses it from then
  on, and the final Skep entry then removes its pins. The removed node is not told by Raft
  (measured, evidence §11), so removal also sends it a best-effort
  notice; its `skep-cluster` stops when it sees itself absent from its
  applied configuration (amended, amendment 2: it writes that down
  durably, shuts its Raft instance and its listener, and keeps only its
  local socket for `status` and `health`), and it must be wiped (`skep-cluster leave
  --wipe`, root), which gives it a new key, before it can join again.
  Removing a node that still owns guests is B13.
- **Per-RPC identity: the pin is the identity.** Each RPC carries a
  header with the claimed sender's node ID. The receiver looks up the
  membership entry with that ID, and the entry's pin must equal the key
  that presented the connection. This is checked on **every RPC**,
  against the membership the receiver has applied at that moment, not
  once per connection. The TCP source address is **not** compared: a
  multi-homed host may connect from an address other than the one in
  its entry. A **Raft RPC** is accepted only if both hold:
  1. the key is pinned in a member entry of the applied Skep membership;
  2. that node is a server in the receiver's latest Raft configuration
     (the latest it has in its log, committed or not; this is what the
     library itself uses).

  A **joining node** has neither record yet: it accepts the pins in its
  admission until it has applied a membership that lists it, and the
  normal rule from then on. **Amended (amendment 2):** once a join answer
  has been accepted, the answer's nodes **replace** the admission's; in
  the phase `refused` the joining node accepts no inbound key.

  **Amended (amendment 1): the applied membership is durable.** The
  library does not store the commit index; at start a node restores
  its state machine from its latest snapshot and applies the entries
  after it only when a leader tells it the commit index (checked in
  v1.8.0, `api.go` `NewRaft`). So "the membership the receiver has
  applied" is kept on disk:
  - **The file:** `/var/db/skep/cluster/state/membership.json`
    (amended, amendment 2: inside `state/`), written by
    the service as the `skep-cluster` account, mode `0600`,
    atomically: a temporary file in the same directory, `fsync`,
    `rename`, `fsync` of the directory. Content: the cluster ID, the
    membership epoch (B4), the log index it reflects, and every member
    entry (ID, address, both pins, state). **Pending entries are not in
    it.**
  - **When:** whenever an applied entry changes the membership, the
    file is written **before** that index is reported as applied on the
    status RPC and **before** the listener uses the new membership. A
    write that fails stops the state machine at that entry (it is not
    reported as applied) and is a health error.
  - **At start** the node accepts keys by the file until its state
    machine's applied index has reached the file's index, and by the
    state machine from then on. The service never writes the file back
    to a lower (epoch, index); the only root commands that lower or
    remove it are a restore (B6) and `leave --wipe`, both with the
    service stopped.
  - **"Applied index"** on the status RPC, for catch-up and for the
    promotion rule (join step 3) and key rotation, is the **state
    machine's own** index, not the library's `AppliedIndex` (which can
    run ahead of the state machine).
  - **Who else writes it:** `recover` (B4) writes the file for the new
    membership; a restore (B6) replaces it with the one from the
    restored store; `leave --wipe` deletes it; `join` moves it aside
    with the one-node state (`standalone.<UTC time>/`), and putting the
    one-node state back (a failed join before the point of no return,
    or `join --abandon`) brings it back with that state.
  - This also closes the case of **a full stop of a young cluster**:
    before, a node that restarted before its first snapshot had applied
    no membership and accepted no peer; now it accepts the members in
    its file.

  `hashicorp/raft`'s `NetworkTransport` does not
  expose the connection to the RPC consumer, so Skep serves inbound RPCs
  itself, as the prototype did, adapting the library's MPL-2.0 handler.
  **The library version is pinned** in `go.mod`, and at every update of
  it the adapted handler is compared with upstream `net_transport.go`
  (wire format, RPC types, heartbeat fast path) before the update is
  merged. The comparison is a checklist item in the update's PR.
- **The service account:** `skep-cluster` runs as its own account, not as
  `skep`: the API runs as `skep`, and the same UID could read the node key,
  which A3 keeps out of the API. A fixed UID and GID, proposed by the 8a
  run with ADR 0012's method (checked against the ports `UIDs`/`GIDs`
  files) and recorded in the 8a PR (follow-up answer 9).
- **The API's node-to-node key in the membership:** each node's entry
  also pins a key that `skep-api` uses only for node-to-node forwarding
  (B8). It is a separate key from the user-facing certificate's, because
  an operator-supplied user-facing certificate (ADR 0006 decision 6, C)
  may change key at the operator's renewal, which would break the pin.
  `skep-api` creates it; the root-run `join-request` (or `listen`,
  below) reads its pin and puts it in the request, so it is admitted
  with the node. **Who may change it later:** only root on that node,
  with `skep-cluster key rotate --api` (the new pin added, then the old
  one removed, through the log, as for the node key). `skep-api` cannot
  change its own pin through `skep-cluster`'s socket, and no API
  operation changes any pin.
- **Where `skep-cluster` listens, and when.** Only a clustered node
  listens on the network.
  - `skep-cluster init` without `-address` (the default for a single
    host) starts **no network listener**: the one-node cluster works
    over its Unix socket only, and `skep-api` opens no node-to-node
    listener.
  - The listeners open, on the node's management address, port **8444**
    for `skep-cluster` and port **8445** for `skep-api`'s node-to-node
    listener (B8), when the host is about to be clustered: on a joining
    host by `skep-cluster join-request -address <addr>`, and on the
    founding node, before its first `admit`, by the root-only local
    command **`skep-cluster listen -address <addr>`**. Both refuse
    unless the Raft configuration is this node alone; they write the
    address to `cluster.json` and `node_listen_address` to `api.json`
    and restart the two services. A one-node cluster that starts with a
    new address updates its own membership entry and its own server
    entry in the Raft configuration (`AddVoter` with an existing voter's
    ID replaces its address; checked in v1.8.0 `configuration.go`).
  - `admit` refuses on a node without a listen address.
  - There is no default address, and the address in the membership
    entry must equal the listen address. Both ports are unregistered
    with IANA, like 8443 (follow-up answer 1).
  - Configuration file `/usr/local/etc/skep/cluster.json`,
    `root:skep-cluster`, `0640`, as `api.json` is `root:skep` `0640`:
    the service can read it and not change it.
- **Clocks:** the signed envelopes of B8 carry an expiry, so clocks must
  agree. `skep-cluster` exchanges wall-clock time with each peer on its
  own status RPC and estimates the offset from the round trip. A peer
  more than **5 s** away is a health warning naming it; more than **15 s**
  is a health error, and forwarded requests to and from that peer are
  refused with `503 node_clock_skew` (B8) rather than risking expired or
  over-long envelopes (follow-up answer 3).

**Risks:** custom verification code is security-critical and must be
small and tested; the admission step depends on the administrator
comparing a pin; the adapted handler can drift from upstream if the
update checklist is skipped. **Amended (amendment 1):** the risk
recorded here before (a member that was down while a node joined
refuses that node when it leads) is replaced by the promotion rule of
join step 3: no member meets a leader it does not know. What it costs
instead: **with a member down, a join or a key rotation cannot finish
until that member returns or is removed.** What a member "has applied"
after it restarts is the applied-membership file (amendment 1).
**Amended (amendment 1): a member that was down during a removal still
pins the removed node** until it has caught up (B2 states the risk and
the operator's rule; Skep 0.2 does not prevent it).

**Tests:** repeat every identity test of the prototype (other key, valid
key of another node, non-member, header claiming another node, impostor
at a member's address) against pins; a member connecting from a second
address accepted; a pending key's Raft RPC and read
refused, its join accepted; an unadmitted key refused in the handshake;
TLS 1.2 refused; an expired pending entry refused and audited, with the
same state on every node; a listener whose clock is ahead refuses
before the expiry entry; `admit` refused for a duplicate ID, name,
address and pin, and on a node without a listen address; a withdrawn
entry refused and its connection closed; `join-request` and `join`
refused on a node whose Raft configuration has two servers; a wrong
admission sends nothing and the one-node state is back; the leader
stopped after each join step and each removal step (step 0 included),
and the new leader finishes it; **amended (amendment 1):** an
interrupted removal of a node with guests is finished by the new leader
itself, guests included, with nothing run again; an interrupted removal
of a `joining` member is finished as a removal and the node is not
added again; a `removing` member's key is still accepted until the
`RemoveServer` entry; promotion happens only after catch-up **and**
after every member has applied the member entry: with one member
stopped the joiner stays a working non-voter, health on every member
names the stopped one, and the promotion follows its return or its
removal; a joiner whose admission is older than a newer member accepts
that member from the join call's answer; a join that fails before the
leader applied step 1 puts the one-node state back; one that fails
after it, or with an unknown outcome, does not, keeps retrying and says
so; `join --abandon` refused while a member accepts the key and while
no member answers, and restoring the one-node state after the removal;
a
removed node refused by each member as soon as it has the
`RemoveServer` entry, before the Skep entry, and its open connection
closed; a single host after `init` has no listener on 8444 or 8445; key
rotation without a refused RPC, the old pin kept while one member is
stopped and removed after it has applied the new one (amended,
amendment 1); clock
offset of 6 s and 16 s gives the warning and the error (unit tests with a
fake clock; changing a lab VM's clock is not pre-approved). **Amended
(amendment 1):** the test "a member kept down across a join, with the
new node as leader" is replaced by the promotion tests above: the new
node cannot lead while that member is down. **Amended (amendment 1),
the applied-membership file:** a member restarted after reporting the
joiner's member entry as applied, before its next snapshot, accepts the
joiner by its file, and the joiner as leader brings it up to date; a
full stop of a three-node cluster before its first snapshot, then a
start, elects a leader; the file is `0600`, owned by `skep-cluster`,
holds no pending entry, and is written before the status RPC reports
the index (a fault injected between the write and the report leaves
the file ahead, never behind); a pending key is refused after a restart
until the node has caught up; the file never goes back to a lower
(epoch, index) across restarts; the status RPC reports the state
machine's index, not the library's; `recover` rewrites it, a restore
replaces it, `leave --wipe` deletes it, a join moves it aside and a
failed join brings it back. **Removal while a member is down (B2):**
health on every member names the members that have not yet applied a
removal, and stops naming each one once it has.

### B2. What one compromised node can do to the others (proposed; answered in Part C)

The plain statement, for review:

- **Root on one node** has that node's cluster key and a member's full copy
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
  every member, voter or not, B6), so it can attack all passwords offline. Argon2id
  (64 MiB, 3 passes, ADR 0006 decision 2) slows that down; it does not
  prevent it for weak passwords.
- It **cannot** get a root shell or run commands on another node, touch
  anything outside another node's `agent.json` allow-lists (they are local
  and never replicated), cut another node's management interface, or erase
  another node's agent audit log or cluster audit (each node keeps its
  own, B10).
- **A compromised `skep-api` on one node** (not root) can act as any user
  on every node: it authenticates users, so anything it asks for in a
  user's name is believed, locally and when forwarded. It can also make
  **every replicated write that `skep-cluster`'s socket offers** to the
  API: create, change and delete users and roles, set maintenance mode,
  write its node's inventory entries, and admit a node (next bullet).
  And it holds **every user's password hash**, in its read copy in
  `api.db` (B9), for offline attack. It **cannot** vote, send Raft RPCs
  or write a log entry the socket does not offer (it has no cluster
  key), cannot change any pin (B1), and cannot reach any agent but its
  own except through another node's API, which re-authorizes.
- **`admit` stays available through the API**, with `cluster:manage`
  (built-in `admin` role only). Reason: a cluster is administered from
  the interface and `skepctl`, and an administrator should not need a
  root shell on a member to add a host. Making it root-only would not
  contain a compromised API, which can already create an administrator
  and change every role. What `admit` adds for such an attacker is a
  node of their own with a cluster key and a vote, which is the
  position of root on one node, described above. Every admission is
  audited on every node (B10), and a pending entry can be withdrawn
  (B1).

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

**Amended (amendment 1), a risk Skep 0.2 does not prevent: a removed
node and a member that was down.** A member that was down while another
node was removed still pins that node until it has caught up (its
applied membership, the file of B1, still lists it). A removed node that
is hostile can reach that member first, give it a forged log at a higher
term, and that member could then win an election among the real
members. Preventing this would need membership entries signed by a
quorum of members (a form of hardening option b, below; backlog). **The
rule for operators:** after removing a node because it is compromised,
**wipe every member that was down at that time** (`leave --wipe`) before
it returns, and join it again. To make that possible, **health on every
member names the members that have not yet applied a removal** (learned
over the status RPC, B1), until each has.

**Tests:** from one node's `skep-cluster`, an entry that widens another
node's confinement has no effect (the agent ignores it); a compromised
member removed by the majority is refused at once; **amended
(amendment 1):** with one member stopped, a node removed: health on
every running member names the stopped member as not having applied the
removal, and stops naming it once it has returned and caught up.

### B3. Joining a host that already has guests and users (proposed; answered in Part C)

**Options:**

| Option | Guests | Users and roles |
|---|---|---|
| a. Only empty hosts join | Refused if any exist | Refused if any beyond the first administrator |
| b. **Import guests, replace users** | Imported into the inventory, owned by the joining node | The cluster's replace the host's, after listing them and a confirmation |
| c. Merge everything | Imported | Merged; name conflicts refused |

**Recommendation: b.**

- **Guests:** the root-run `skep-cluster join` command reads the host's
  VMs and jails (ID, name, kind) over the agent socket as root and lists
  them. Guest names become unique across the cluster (today
  `virtual_machines.name` and `containers.name` are unique per node
  only). **The check is atomic:** the joiner sends its guest list in the
  join call; the leader refuses on a conflict before adding the node,
  and otherwise writes the guests in the member entry itself (B1, join
  step 1), so no guest can take a name between the check and the write.
  A refusal names both guests so the operator can rename one; it
  therefore **shows the pending key one cluster guest name** per
  attempt (the first conflict only), which is the only cluster data a
  pending key ever sees. IDs are UUIDs and do not collide. Guests keep
  running throughout; the join changes no host state.
- **Which process touches the agent.** The `skep-cluster` **service**
  never connects to the agent socket (A3). The `skep-cluster join`
  **command**, run by root, does: it takes the drain (reason `join`, B7)
  and reads the guest list, as peer UID 0, the way `skep-setup` already
  talks to the agent.
- **Only a one-node host joins.** `join-request` and `join` refuse
  unless the local Raft configuration is this node alone. The host's
  one-node state is moved aside, not deleted, until the node has joined
  and caught up. **Amended (amendment 1):** a join that fails before
  the leader has applied join step 1 puts it back; after that point the
  command never does so by itself, and giving up is `skep-cluster join
  --abandon` after a removal on a member (B1, step 3.7).
- **Users and roles:** the host's users and roles are exported
  to a root-only file, listed by `skep-cluster join`, and replaced by the
  cluster's after the operator confirms. Merging is not proposed: a weak
  local administrator password would silently become a cluster
  administrator. **Every local session is deleted at the join**; sessions
  are not exported.
- **Single-node 0.1 installs upgrading to 0.2:** every host is a
  one-node cluster (Part C answer 10); its users and roles move from
  `api.db` into the replicated store once, by migration (step 8c):
  - **Who and when:** `skep-setup upgrade` runs it, after the `api.db`
    backup it already takes and after `skep-cluster` is initialised and
    healthy, before the new `skep-api` starts.
  - **What moves:** every user and role, with user IDs, password hashes
    and their Argon2 parameters **unchanged**, so every password keeps
    working. Each user starts at the same credential epoch (B9).
  - **Sessions are kept:** they stay in `api.db`, refer to the unchanged
    user IDs, and are stamped with that starting epoch.
  - **Idempotent:** the whole set is one replicated write with one
    operation ID (B11), stored in `api.db` before the write. When it has
    committed, a marker in `api.db` records it. It refuses if the
    replicated store already holds users from anything but this
    operation.
  - **A crash part way** leaves one of two states. Before the write
    committed: no marker, nothing replicated; the next run writes it.
    After it committed, before the marker: the next run repeats the
    write with the same operation ID, which is a no-op, and sets the
    marker. Until the marker is set, `skep-api` does not start serving
    and `skep-setup` reports the upgrade as failed.
  - **Rollback** is the previous package plus the `api.db` backup taken
    before the migration. User and role changes made since the upgrade
    are lost, because 0.1 does not read the replicated store.

**Risks:** an operator loses a local account they relied on (mitigated by
the listing, the confirmation and the export); a guest created on the
host during the join would miss the name check (the join command holds a
drain, B7, from before it reads the guest list until the join has ended
either way).

**Tests:** join langrishe (with guests and users) to a cluster of smith:
guests listed and owned by langrishe, users replaced after confirmation,
export file `0600 root`, no session left; a name conflict refuses the
join, names one cluster guest only, changes nothing and puts the
one-node state back; a guest created on a member between the pre-check
and the join call still refuses the join; a create on the joining host
during the join is refused with `node_draining`, reason `join`; guests
keep running (their PIDs unchanged); the `skep-cluster` service's UID
is refused by the agent socket; **amended (amendment 1):** a join
interrupted after the leader applied step 1 leaves the moved-aside
state in place, the drain is released only when the join has ended
either way (completed, or abandoned with `join --abandon`), and the
host's guests keep running meanwhile. Migration: a 0.1 host upgraded keeps
every password and session; the migration killed before and after the
write and run again gives one copy; rollback with the previous package
and the backup.

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
- **Amended (amendment 1):** the dropped nodes' member entries are
  removed afterwards by the removal steps of B1. `recover` rewrites the
  Raft configuration while the service is stopped and writes no Skep
  entry, so after it a dropped node is a member in state `member` that
  is absent from the Raft configuration, the only way that combination
  arises. The first leader writes step 0 (`removing`) for each such
  node that owns no guest entries itself, actor `system`, and finishes
  the removal. For a dropped node that owns guest entries it writes
  nothing: health reports it on every member, and the administrator
  runs the removal with `--forget-guests` (B13), which writes step 0
  with the guests named; from there any leader finishes it. A leader
  still never forgets guests that no administrator named;
- **Amended (amendment 1):** it writes the applied-membership file
  (B1) for the new membership and the raised epoch, as the
  `skep-cluster` account, before the service starts again;
- **it leaves no root-owned file** in `/var/db/skep/cluster`: the part
  that opens the store runs as the `skep-cluster` account (B6);
- (c) is rejected: unreachable is not dead.

**Risks:** two operators recovering both halves of a partition create two
clusters (split brain). Skep cannot prevent this; the epoch makes the
halves refuse each other afterwards, and the documentation must say to
recover only when the dropped nodes are known to be gone.

**Tests:** on three nodes, stop two for good, recover the third alone, it
commits; restart a dropped node: refused (old epoch); wipe and rejoin it;
**amended (amendment 1):** after the recovery a dropped node without
guests is set to `removing` and removed by the leader itself; one with
guests stays, reported in health, until it is removed with
`--forget-guests`; the survivor's applied-membership file names the new
membership and epoch, owned by `skep-cluster`.

### B5. Mixed versions during a rolling upgrade (proposed; answered in Part C)

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
forwarding version, or an operation the target's build does not
serve) is refused by the **forwarding** node, before anything is sent,
with `503 node_version_mismatch`, `details` naming the target node and
both builds. It is a 503 because it ends when the upgrade finishes, as
`agent_version_mismatch` does on one node today. The interface shows the
node as "upgrading" (follow-up answer 4). Detection is **per operation,
not per field**: a new field on an operation both builds serve is not
detected by the forwarding node; the older target refuses it with its
own `400`, which is passed back naming the target node. Field-level
detection is in the backlog.

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

### B6. Backup and restore of the cluster store (proposed; answered in Part C)

**Proposal:**

- `skep-cluster db backup` writes a consistent copy (a Raft snapshot of
  the applied state plus the membership) next to the other two backups.
  `skep-setup upgrade` backs up three databases instead of two, in the
  same step.
- **How the copy is taken.** A second process cannot open the store
  while the service runs: bbolt takes an exclusive `flock` on the file
  for a read-write open, and a read-only open asks for a shared lock,
  which the exclusive one blocks (checked in `go.etcd.io/bbolt` v1.4.1,
  `bolt_unix.go` and `db.go`; `raft-boltdb/v2` v2.4.2 opens it
  read-write). So:
  - **while the service runs** (the normal case, and the one
    `skep-setup upgrade` uses): the service takes a snapshot (the
    library's `Raft.Snapshot`) and **streams it over its Unix socket**
    to the root command, which writes the file. This operation is
    allowed for **peer UID 0 only**; `skep` cannot call it.
  - **with the service stopped** (disaster recovery): the command opens
    the store itself, read-only, as the `skep-cluster` account.
- **The copy holds every user's password hash**, like `api.db`'s backup.
  It is written as the other two are today: by a root process, into
  `/var/db/skep/backup/<label>/` (`0700`), file `0600`, with its
  `.sha256` (`internal/dbbackup`).
- **The node key is not in the backup.** A backup holds pins, never a
  private key.
- **No root-owned files in the store's directory.** The root-run
  commands that open anything under `/var/db/skep/cluster` (`recover`,
  `db backup` with the service stopped, `audit verify`) do that part as
  the `skep-cluster` account (`internal/privdrop`), so a later start of
  the service never meets a file it cannot write.
- **Restoring onto a host with a different key** (a rebuilt host, a new
  key after a wipe) is supported only together with a one-node `recover`
  (B4) of that host, which writes that host's **current** pin into its
  member entry. Any other restore onto a different key is refused: the
  store would name a pin nobody holds.
- **Restore is disaster recovery, not an undo.** Restoring an old copy on
  one node of a running cluster is pointless: the leader overwrites it.
  A cluster restore is: stop every node, restore the copy on one, `recover`
  it alone (B4), wipe and rejoin the others. This is documented and
  audited like `recover`.
- **Amended (amendment 1):** a restore **replaces** the
  applied-membership file (B1) with one written from the restored
  store's membership, as the `skep-cluster` account, while the service
  is stopped; it is the one case where the file may go back to a lower
  index. The `recover` that follows writes it again.
- Each node's agent database stays the authority for its guests and
  tasks, so a restored cluster store that lists a guest the agent does not
  have (or misses one it has) is reconciled from the agents, never the
  other way round.

**Risks:** an operator expects a restore to roll back a user change; the
documentation must say it does not on a live cluster.

**Tests:** back up, change users, full restore on one node, rejoin the
others: the old users are back everywhere and the agents' guests are
reconciled into the inventory; a backup with the service running, asked
for as `skep`, refused; the backup contains no private key; after
`recover`, `db backup` and `audit verify` as root, no file under
`/var/db/skep/cluster` is owned by root; a restore on a host with a new
key works with a one-node `recover` and is refused without; **amended
(amendment 1):** after a restore the applied-membership file matches
the restored store's membership.

### B7. Drain (proposed; answered in Part C)

**One refusal rule in the agent, three sources, two scopes.** In
`tasks.Engine.Submit`, in the same locked step that refuses
`shutting_down` today, the engine refuses a new task when the node is
drained:

| Source | Refuses | Allows |
|---|---|---|
| Upgrade drain | **Every** new task | – |
| Join drain (B3) | **Every** new task | – |
| Maintenance mode | Every new task **except** guest stop and force-stop: `vm.stop`, `vm.force_stop`, `container.stop` (jails have no force-stop task today) | Those three types |

In both, **cancellation stays allowed**: it is `tasks.Engine.Cancel`, not
a submission, so the rule does not touch it. When maintenance and
another source are active, the wider scope (every new task) applies.
One error code with a reason:

`503 node_draining`, `details.reason` = `upgrade` | `maintenance` |
`join`, plus who set it and, for `upgrade` and `join`, when it expires.

- **Source 1, the local upgrade drain** ([agent drain proposal](../proposals/agent-drain.md),
  option B): an internal operation allowed **only for peer UID 0** (the
  first root-only operation on the socket), held **in memory**, with an
  **expiry** (60 s proposed) that `skep-setup` renews while it waits and
  backs up. An expired drain ends by itself and is audited; an agent
  restart ends it. It needs **no quorum** and does not involve
  `skep-cluster`. `skep-setup upgrade` drains instead of stopping
  `skep_api`, keeping today's API stop as the fallback for an agent
  without the operation.
- **Source 1b, the join drain:** the same root-only, in-memory, expiring
  operation, with the reason `join`. The root-run `skep-cluster join`
  **command** takes it and renews it for the length of the join (B3);
  the `skep-cluster` service never connects to the agent socket.
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

**Tests:** drain as root works, as `skep` is refused, with each of the
reasons `upgrade` and `join`; expiry ends a drain
when setup is killed (`timeout -f` on FreeBSD); a submission racing the
drain is either accepted before it or refused after it, never half; reads
and cancel work while drained; in maintenance `vm.stop`, `vm.force_stop`
and `container.stop` are accepted and `vm.start`, `vm.restart`,
`container.restart` and creates are refused; during an upgrade drain the
stops are refused too; maintenance survives an agent restart and an API
stop; the upgrade path on a lab VM with a running task.

### B8. How a request reaches another node, and who the caller is (proposed; answered in Part C)

**Path:** client → `skep-api` on any node → if the resource's owner (from
the replicated inventory) is another node, the API forwards the request
over **mutual TLS 1.3 to the owner's `skep-api`**, which talks to its own
agent over the socket as today. No agent gets a network listener (ADR
0004). Console streams are forwarded the same way.

**Two listeners in `skep-api`:**

| | User-facing listener (today's) | Node-to-node listener (new) |
|---|---|---|
| Address and port | `listen_address` in `api.json`, default `127.0.0.1:8443` (ADR 0006 decision 7) | `node_listen_address` in `api.json`, the management address and port **8445**; no default address, and **absent on a host that is not clustered**: written by `skep-cluster listen` or `join-request` (B1), which run as root |
| Client certificate | **Never requested** (as today) | **Required**, and accepted only if its key's pin is the node-to-node API key of a node that is a member in the applied membership **and** a server in the Raft configuration (B1's rule; `skep-cluster` gives its API only such members) |
| Server key | Generated or operator-supplied (ADR 0006) | The node-to-node key, pinned in the membership; never operator-supplied |
| Serves | The public API and the web interface; **not** the forwarding endpoints (they are not registered on its mux) | **Only** the forwarding endpoints; no web interface, no login, no session |

Keeping them apart means a browser is never asked for a certificate, a
user-facing request can never reach a forwarding endpoint, and a
node's forwarding identity does not depend on the operator's certificate.

**Identity:** each API presents its node-to-node key, pinned in the
membership (B1). The forwarded request carries a **signed envelope**, signed with the
originating API's key: user ID, the user's credential epoch (B9), request
ID, idempotency key, target node, method, **path including the query
string**, a hash of the body, the time it was **issued**, and an expiry
30 s later, plus a nonce. The receiving API:

1. checks the signature against the origin's pinned API key, the target
   (itself), the expiry, and the nonce (a replay cache for the expiry
   window). The replay cache is in memory, so the receiver also
   **refuses any envelope issued before its own start**: after a
   restart it cannot know which nonces it had seen. Forwards in flight
   across a restart of the receiving API are refused and retried by the
   caller;
2. loads the user from its own copy of the replicated store, refuses a
   disabled user, and compares the envelope's credential epoch with its
   own for that user:
   - **equal:** accepted;
   - **the envelope's is higher:** the receiver is behind. It answers
     `503`, and the request can be retried once the receiver has caught
     up;
   - **the envelope's is lower:** the origin is behind (the user's
     password, role or state has changed since). Refused;
3. **authorizes again** with its own copy of the roles: permissions travel
   by reference (the user), not by value;
4. **applies its own quorum state** (A5): a receiver without quorum
   serves a forwarded read or a stop/start of a guest it owns and
   refuses everything else, whatever the origin's state;
5. sends the request to its agent with the actor, the origin node, and the
   envelope, which the agent stores in its audit entry (`audit_log.details`,
   which already exists as a JSON column; no schema change). The stored
   envelope has a size limit (ADR 0004 amendment).

**Console streams** are forwarded with the same envelope at the start.
They are long-lived, so **both nodes close a forwarded stream when the
user's credential epoch rises** in their own copy, as each closes its
local sessions (B9).

**Where the envelope is verified (Andy, 2026-11-24, Part C answer 5):** in
the receiving node's API, and recorded in its agent's audit, where it can
be checked later. Verifying in the agent too would need the agent to hold
the cluster's pins, from `skep-cluster` (which A3 keeps away from the
agent) or from its own API (which, compromised, controls the agent
anyway). This changes ADR 0004; its amendment of 2026-11-24 was
accepted with this ADR.

**What the inventory is for:** routing and cluster-wide name uniqueness.
The owning node's agent remains the truth about its guests (ADR 0004). A
create writes the inventory entry first (quorum; state `creating`, with the
request's idempotency key, which the API supplies when the client sent
none), then the request reaches the owner's API, which submits to its
agent and **itself writes the task ID** into the entry (the owner's API
does this also when the create was forwarded from another node). A
delete goes the other way: **agent first, inventory second**. The entry
is removed when the delete task has succeeded, so an entry never
disappears while the guest exists.

**Reconciliation, by the owning node, automatically.** The owner's
`skep-api` compares the inventory entries it owns with its agent's guests:

- **when:** when a create's or delete's task reaches a terminal state;
  at every start of `skep-api`; and every 60 s (follow-up answer 6);
- **how:**
  - an entry whose guest exists in the agent becomes `present` (and
    takes the agent's ID);
  - an entry in `creating` whose task ended `failed`, `cancelled` or
    `interrupted` and whose guest the agent does not have is
    **removed**, and its name is free again;
  - an entry in `creating` **with no task ID** (the API stopped between
    the two steps) is resolved **by a read, never by submitting again**:
    the owner's API asks its agent for the task with that idempotency
    key (a new read-only internal operation). If there is one, its ID
    is written into the entry and the rules above apply. If there is no
    such task 5 minutes after the entry was written, and the agent has
    no such guest, the entry is removed;
  - an entry `present` whose guest the agent **no longer has**, with no
    running task for that guest, is **removed** (a delete that ended
    between its two steps, or a guest removed outside Skep);
  - a guest the agent has and the inventory lacks (written by an older
    build, or after a restore, B6) is added. **If its name is already
    taken** in the cluster, it is added all the same, **flagged as a
    name conflict**; reconciliation never refuses a guest that exists
    and never renames one. The flag shows in the interface and in
    health until an operator renames either guest;
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
own, with the owner it knows); a receiver that refuses envelopes issued
before its start also refuses, for up to the tolerated clock offset,
some issued just after it by a node whose clock is behind (the caller
retries).

**Tests:** a request on smith for a VM on langrishe runs on langrishe with
the user recorded on both sides; a replayed or expired envelope refused; a
role removed on the majority refuses a forwarded request even when the
originating node is stale; the owner down gives `node_unreachable`; the
user-facing listener does not request a client certificate and answers
404 for a forwarding path; the node-to-node listener refuses a
connection without a member's pinned key and serves no public route; a
create whose task fails leaves no inventory entry after reconciliation; an
API killed between the inventory write and the submission leaves no entry
after 5 minutes, and one killed between the submission and writing the
task ID gets the task ID by the read, with exactly one task in the
agent; a delete interrupted after the agent finished leaves no entry;
a guest whose name is taken is added with the flag; an envelope with a
changed query string, one issued before the receiver's start, and one
with a lower epoch refused; a higher epoch gives 503 and works after
catch-up; a receiver without quorum refuses a forwarded create and
serves a forwarded stop; a forwarded console closes on both nodes when
the user's password changes.

### B9. Sessions and login without quorum (proposed; answered in Part C)

Sessions stay local (A7). Logins check the password against the node's
copy of the replicated users, which may be stale on a node without quorum.

**Proposal:**

- `skep-api` keeps a read copy of the replicated users and roles in
  `api.db`, with the log index it reflects, so a login works while
  `skep-cluster` restarts.
- **`skep-cluster` unreachable on its socket is treated by the API as no
  quorum**, on a single host too: the rules below apply until it
  answers again. A single host whose `skep-cluster` is stopped therefore
  serves reads, administrator logins and stop/start of its guests, and
  refuses every create, edit and delete.
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
  node catches up; then the sessions are deleted.
- **What root can do.** In 0.2 the local `skep-api admin` commands
  (create, disable, delete a user) write through the cluster like every
  other user change, so they **need quorum**. On an isolated node root
  cannot create or change a user until quorum returns. If it will not
  return, the way out is `skep-cluster recover` (B4), not a local
  account.

**Options rejected:** refuse all logins without quorum (an operator cannot
stop guests on an isolated node through Skep); replicate sessions (A7).

**Risks:** the window above; it is bounded by the reduced action set and
the one-hour limit, not closed.

**Tests:** on a node cut off from the other two: admin login works and is
degraded, operator login refused, create refused, stop/start of an owned
guest works; change the admin's password on the majority, heal: the
minority's sessions for that user are gone; with `skep-cluster` stopped
on a single host, a create is refused, an admin login is degraded and a
guest stop works; `skep-api admin create` without quorum is refused and
says why.

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

- **Keyed by log index.** Each audit entry is keyed by the index of the
  log entry it records (with the membership epoch, which `recover`
  raises, so an index used again after a restore, B6, does not
  collide). After a restart the library replays the log from the last
  snapshot; an entry whose key is already in the audit is not written
  again.
- **A node's cluster audit starts at its join.** A joining node
  receives a snapshot of the state, not the entries that led to it, so
  its audit has nothing from before; the older members have that
  history.

**Risks:** a third log to read during an incident; the request ID links
them, as it links the two today.

**Tests:** a user created on smith appears in the cluster audit of all
three nodes with the same request ID; `audit verify` detects an altered
segment; a restart that replays the log adds no audit entry; a node
joined later has no entry older than its join.

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
| 8a | `skep-cluster` skeleton: the `skep-cluster` account (fixed UID/GID, proposed by the run with ADR 0012's method and recorded in the PR), rc.d service, `/var/db/skep/cluster`, `cluster.json` (`root:skep-cluster`, `0640`); node key; node ID read from the agent by the root-run `init`; `init` without `-address` (every host a one-node cluster, Part C answer 10, with **no network listener**); `skep-cluster listen -address` opening port 8444 (B1); the Raft store with the library version pinned; type-and-version labels on every Skep entry (B5); a Unix socket with peer-credential check (`skep` and root, each operation naming which; the `skep-cluster` commands root runs use it); health | smith: init, restart, health; the socket refuses other UIDs; after `init` nothing listens on 8444; after `listen -address` it does, on that address only; `skep_agent`/`skep_api` untouched |
| 8b | Join and trust (B1): `join-request`, `admit` (pending entries, expiry by log entry, withdrawal), `join` with the cluster confirmation, the one-node state moved aside and put back, the two records and their crash points, the listener's member-or-pending rule, the inbound handler with per-RPC identity and the upstream-comparison checklist, non-voter then automatic promotion once caught up and applied by every member, the applied-membership file and the state machine's applied index on the status RPC (amendment 1), remove with its `removing` step, `leave --wipe`, removal of a node that owns guests (B13), clock-offset health (B1), cluster audit (B10) | Three nodes; every identity test from the prototype; pending key limited to join; wrong and expired admissions; removal refused within one apply; removal with guests refused without `--forget-guests` |
| 8c | Replicated identity: users and roles in the store, the API's read copy, credential epochs, degraded login without quorum or without `skep-cluster` (B9); `skep-api admin` through the cluster; the 0.1 migration of users and roles by `skep-setup upgrade` (B3) | Login on any node; password change revokes sessions on all; a cut-off node's login rules |
| 8d | Guest inventory and owner (A7): the owner's API publishes its guests; cluster-wide names; the agent's root-only, expiring drain operation with the reason `join` (B7, moved here from 8f because the join needs it); join with guests, the atomic name check and user replacement (B3); the agent's read of a task by idempotency key; automatic reconciliation by the owner (B8); a new guest entry owned by a `removing` member refused (amendment 1) | langrishe joins with guests; names unique; guests keep running; a failed create leaves no entry |
| 8e | Forwarding (B8): the node-to-node listener on port 8445 with the pinned node-to-node keys, signed envelope verified in the receiving API and recorded in the agent's audit, re-authorization, `node_unreachable`, `node_clock_skew`, the versioned forwarding protocol and `node_version_mismatch` (B5); one view in the interface | A VM on langrishe managed from smith's interface; replay refused; the user-facing listener requests no client certificate and serves no forwarding path |
| 8f | Drain (B7): the agent's refusal rule with both scopes, the upgrade drain (the root-only operation exists since 8d, here with the reason `upgrade` and the full refusal scopes), maintenance mode persisted in `agent.db`; `skep-setup upgrade` backs up the cluster store (root, `0600`) and drains | Upgrade a lab VM with a running task; kill setup mid-drain, drain expires; stops accepted in maintenance, refused in an upgrade drain |
| 8g | Recovery (B4), the feature-level operation (B5), backup and restore (B6), and the partition suite with pf | Recovery of one survivor; old-epoch node refused; full restore; the prototype's partition cases with real cuts |

**Partition testing with pf (Part C answer 21, follow-up answers 7 and
8).** Real cuts with pf on smith, langrishe and star only, under Andy's
conditions:

- rules match **only the cluster port (8444) and the node-to-node API
  port (8445)** between the three lab addresses (10.79.20.81–83), in
  both directions or one, either port or both, as the case needs. So
  forwarding between APIs during a partition (B8, A5) is tested with
  real cuts too;
- **never a default-deny rule**; everything else passes;
- a timer that disables pf again is started **before** the rules load
  (time-bounded and killed at the end of the step, as CLAUDE.md requires
  for background processes);
- pf may be loaded live (`kldload pf`) where it is not loaded, and the
  rules go in **an anchor**, not the main ruleset. The run first checks
  `pfctl -s info` and refuses if pf is already enabled with rules of
  its own;
- nothing persistent: no `rc.conf` change (`pf_enable` stays unset), no
  `loader.conf` change, no `/etc/pf.conf` written;
- never on hearst.

### B13. Removing a node that still owns guests (proposed; answered in Part C)

A node can be removed (B1) while the inventory still lists guests it
owns: an operator retires a host, or a host is lost for good (B4). There
is no migration in 0.2 (A6), so its guests cannot move.

| Option | How | For | Against |
|---|---|---|---|
| a. Refuse while it owns guests | The operator deletes the guests first (through the owner, while it is reachable) | Nothing is ever lost from the inventory | A lost host can never be removed; deleting guests to retire a host is often not wanted |
| b. Remove and drop its entries | The inventory forgets the guests | Simple | Silent: an operator may not know guests are still running on the removed host |
| c. **Refuse by default; an explicit `--forget-guests`** | Removal lists the guests it owns and refuses. With `--forget-guests` (and the list typed back as a count), the guest entries are named in the Skep entry that starts the removal and removed by the Skep entry that removes the node's pins, which follows the Raft `RemoveServer` (B1; amended, amendment 1), each audited | A lost host can be removed; nothing is dropped without being named | Two paths to test |
| d. Keep the entries as orphans | Entries stay, marked "owner removed", read-only, until deleted by hand | Visible | Names stay reserved by guests no node manages; reconciliation (B8) has no owner to ask |

**Recommendation: c.**

- What happens on the removed host: nothing. Its guests keep running
  (unreachable is not dead), its `agent.db` still has them, and its
  `skep-cluster` stops when it sees itself removed (B1). After `leave
  --wipe` it is a one-node cluster again (Part C answer 10), and its own
  reconciliation (B8) puts its guests back into its own inventory.
- **The order** is B1's. **Amended (amendment 1):** step 0, one Skep
  entry that sets the member to `removing` and records the guest
  entries named with `--forget-guests`; then `RemoveServer`; then one
  Skep entry that removes the pins and those guest entries. If the
  leader stops anywhere after step 0, the new leader finishes every
  remaining step itself, guests included: the administrator's choice is
  in the log, so forgetting them is not the leader's own decision. The
  administrator does not run the removal again, and health has nothing
  to report for it. A leader still never forgets guests that no
  administrator named (B4).
- **Amended (amendment 1): no new guests on a node being removed.**
  From step 0 on, the state machine refuses a new guest entry owned by a
  member in state `removing`, so the guest entries step 0 checked or
  named are all the node owns until it is gone. Built in step 8d, with
  the guest inventory.
- If that host later joins again, B3's name check applies: a name taken
  meanwhile by another guest refuses the join.
- `--forget-guests` needs quorum, like every membership change; on a
  cluster without quorum it comes after `recover` (B4), which then names
  the node as dropped.

**Risks:** an operator forgets guests that still run on a host they no
longer watch; the listing, the count typed back and the audit entry name
each one. A host removed while merely cut off (not lost) keeps running
its guests unmanaged until it is wiped or rejoined. **A removed host
that is not wiped keeps its last copy of the users:** `skep-cluster`
stops there, so its API is without quorum (B9), and the passwords as
they were at the removal, including any changed or revoked in the
cluster since, still open a degraded administrator session on that
host (reads, stop/start of its guests) until it is wiped.

**Tests:** remove langrishe while it owns a VM: refused, listing the VM;
with `--forget-guests` and the count: removed, entries gone, audited on
every node, the VM still running on langrishe; `leave --wipe` on
langrishe: its VM is back in its own inventory; rejoin with a meanwhile
taken name: refused; before the wipe, an administrator password changed
in the cluster after the removal still gives a degraded login on
langrishe, and none after the wipe; **amended (amendment 1):** the
leader stopped after step 0 and after `RemoveServer` of a removal with
`--forget-guests`: the new leader removes the node and exactly the
guest entries step 0 named, with no second command; a new guest entry
owned by a `removing` member is refused (step 8d).

## Part C — Answers (Andy, 2026-11-24)

Given with the review chat on 2026-11-24, to the 22 questions of the
previous draft (head `ff265b5`). Part B has been revised to match.

| # | Question (short) | Answer |
|---|---|---|
| 1 | Trust: pinned node keys or a cluster CA | **Pinned keys** (B1 d) |
| 2 | Joiner a non-voter until its fingerprint is confirmed | **Replaced by the two-way fingerprint exchange** (B1); **automatic promotion once caught up** |
| 3 | Join token lifetime and binding | **Replaced** by the fingerprint exchange; the token is a rejected option in B1 |
| 4 | Accept that root on one node controls Skep-managed resources on every node | **Yes**, with the fuller statement (B2: guests' disks, password hashes) |
| 5 | Where the forwarded request's envelope is verified | **In the receiving API; recorded in the agent's audit** (B8; ADR 0004 amendment, accepted with this ADR) |
| 6 | The node's key belongs to `skep-cluster`, not the agent | **Yes** (ADR 0004 amendment, accepted with this ADR) |
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

### Follow-up answers (Andy, 2026-11-24)

To the 11 questions the revision of `6071127` left open. Part B has been
revised to match.

| # | Question (short) | Answer |
|---|---|---|
| 1 | Ports: `skep-cluster` on 8444, the node-to-node API listener on 8445 | **Yes.** Open on clustered nodes only (B1, final review F7) |
| 2 | Error codes `node_unreachable`, `node_version_mismatch`, `node_clock_skew` (all 503); permission `cluster:manage` for the built-in `admin` role only | **Approved** |
| 3 | Clock offset: health warning above 5 s, error and forwarding refused above 15 s | **Yes** (B1, B8) |
| 4 | Versioned forwarding protocol, refused at the forwarding node with `node_version_mismatch` | **Yes** (B5 b) |
| 5 | Removing a node that owns guests: refuse by default, `--forget-guests` with the count typed back | **Yes** (B13 c) |
| 6 | Reconciliation every 60 s and at API start; a `creating` entry without a task ID removed after 5 minutes | **Yes**, as changed by the final review: resolved by a read, never by submitting again (B8) |
| 7 | pf rules on 8445 as well as 8444 | **Yes**, under the same conditions (B12) |
| 8 | A live `kldload pf` and an anchor | **Allowed** on smith, langrishe and star; no `loader.conf` or `rc.conf` change (B12) |
| 9 | The fixed UID/GID of the `skep-cluster` account | **The 8a run proposes it** with ADR 0012's method and records it in the 8a PR |
| 10 | The `join` confirmation: cluster ID and members from the admission, then the host's own users and guests | **Agreed** (B1, B3) |
| 11 | `join-request` on a one-node cluster reuses its node key | **Yes; a wiped node gets a new key** (B1) |

### Final review, 2026-11-24

Andy accepted the ADR and the ADR 0004 amendment with these changes, all
worked into the text. No question is open.

| # | Where | Change |
|---|---|---|
| F1 | B1, B13 | Skep's membership entries and Raft's configuration are two records; their order and every crash point; a Raft RPC needs both; removal is `RemoveServer` first, then the Skep entry |
| F2 | B1 | The pin is the identity, checked on every RPC against the applied membership; the source address is not compared; connections closed on removal and expiry; TLS 1.3 on 8444 |
| F3 | B1, B3 | Only a one-node host joins; its state is moved aside and put back if the join fails; local sessions deleted, not exported |
| F4 | B3, B7, ADR 0004 | The `skep-cluster` service never touches the agent; the root-run `join` command takes the drain and reads the guests; `node_draining` reason `join` |
| F5 | B3, B8 | The name check is atomic in the join call; a refusal shows one cluster guest name; reconciliation flags a name conflict and never refuses or renames |
| F6 | B1 | Pending entries expire by a log entry; `admit` refuses duplicates; withdrawal; the request carries the node ID and the API's pin; who may change the API pin |
| F7 | B1, B8, B12 | Listeners only on clustered nodes; `skep-cluster listen -address`; `admit` refused without a listen address |
| F8 | B2 | What a compromised API can do in a cluster; why `admit` stays in the API |
| F9 | B8 | Reconciliation by a read; a vanished guest's entry removed; delete is agent first; the owner's API writes the task ID; envelope issued-at, query, epoch rules, receiver's quorum state, console streams |
| F10 | B9 | `skep-cluster` unreachable is no quorum, on a single host too; `skep-api admin` needs quorum |
| F11 | B3, 8c | The 0.1 migration of users and roles |
| F12 | B4, B6 | The backup is streamed by the running service (bbolt holds an exclusive lock); no root-owned files in the store's directory; no key in the backup; restore onto another key only with a one-node `recover` |
| F13 | B10 | Audit keyed by log index; a node's audit starts at its join |
| F14 | ADR 0004 amendment, B2 | "Every member"; what stays of the databases and of the compromised-API statement; the stored envelope's size limit |
| F15 | B5, B13 | Forwarding detects operations, not fields; a removed host that is not wiped keeps its last user copy |

Written while applying them, for the record: the build order moves the
agent's root-only drain operation from 8f to 8d (the join needs it); a
new leader does not forget a removed node's guests on its own (the
removal is run again); the 4 KiB limit for the stored envelope; and one
risk found in B1 (a member that was down across a join can refuse a new
leader), which is in the backlog. (Amendment 1 changes two of these:
a new leader finishes a removal that step 0 recorded, guests included,
and the B1 risk is replaced by the promotion rule.)

## Consequences

- A third service and a third account on every node; three databases to
  back up on upgrade; two new listening ports (8444, 8445) **on
  clustered nodes** only. A single host opens neither.
- **A single host now depends on `skep-cluster`:** while that service is
  down, the API behaves as without quorum (B9), so nothing can be
  created, edited or deleted, and `skep-api admin` cannot change users.
- ADR 0004 changes, after answers 5 and 6: the node key's owner, and where
  forwarded identity is verified. The amendment in ADR 0004 was accepted
  with this ADR and its replacements are applied there. The agent gains its first
  root-only internal operation (B7).
- The security overview gains a cluster section: the trust model and the
  join exchange, the compromised-node statement (B2, guests' data and
  password hashes included), the staleness window without quorum (B9);
  its open item on signed per-request tokens is reworded per the ADR 0004
  amendment.
- New error codes (`node_draining`, with the reasons `upgrade`,
  `maintenance` and `join`; `node_unreachable`, `node_version_mismatch`,
  `node_clock_skew`; all approved) and a permission (`cluster:manage`,
  built-in `admin` role only, approved) change the API contract. The
  codes for a refusal without quorum (A5) and for a receiver that is
  behind (B8, the 503) are not named here; like every error code they
  are proposed in the PR that adds them (8c, 8e) and need Andy's
  approval there.
- `agent.db` gains the persisted maintenance flag (a migration, B7). The
  agent's internal API gains a read of a task by idempotency key (B8).
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

## Amendment 1 (2026-11-24, Accepted)

From the review of the accepted text. **Accepted by Andy on
2026-11-24.** Points 1 to 4 were accepted first; checking point 1
against `hashicorp/raft` v1.8.0 found a condition it did not state,
which Andy answered with point 5 (option a, the applied membership on
disk). Point 6 and the additions to points 2 and 4 came with the
acceptance. They are worked into B1, B2, B3, B4, B6 and B13 and their
test lists, each changed passage marked "Amended (amendment 1)".
Everything here is built in step 8b (the guest-entry refusal of point
2 in step 8d); step 8a (one node, no join, no removal) does not depend
on it.

### 1. No member ever meets a leader it does not know

- The leader promotes a joining non-voter (`AddVoter`) only when it has
  caught up **and** every member in the Skep membership has applied the
  entry that created its member entry. The leader asks each member over
  Skep's own status RPC, as it does for catch-up.
- Until then the node is a working non-voter, and health on every
  member names the member being waited for.
- The same rule in key rotation: the old pin is removed only when every
  member has applied the entry that added the new one.
- The join call's answer carries the current members' addresses and
  pins (public data, as in an admission), so a joiner whose admission is
  older than a newer member knows that member.
- **The consequence:** with a member down, a join or a key rotation
  cannot finish until that member returns or is removed.
- This replaces the B1 risk paragraph ("A member that was down while a
  node joined...") and its test. The backlog row is closed as solved by
  design.

Checked in v1.8.0: a non-voter does not stand for election (`raft.go`
`runFollower`), so a node cannot lead before its promotion; `AddVoter`
on a non-voter promotes it (`configuration.go` `nextConfiguration`).

### 2. Removal starts with a Skep entry

- New step 0: one Skep entry sets the member's state to `removing` and
  records, when `--forget-guests` was given, the guest entries named. It
  does not change which keys are accepted.
- Then `RemoveServer`, then the final Skep entry, as before.
- A new leader that finds a `removing` member finishes every remaining
  step itself, guests included, because the administrator's choice is in
  the log. This replaces "the administrator runs the removal again" and
  its health report.
- A new leader continues a join only for a member in state `joining`,
  never for one in `removing`. (Before, an interrupted removal of a
  joining member would have been added again.)
- From step 0 on, the state machine **refuses a new guest entry owned by
  a member in state `removing`** (B13; built in step 8d, with the guest
  inventory).

Checked in v1.8.0: `RemoveServer` for a server that is already absent
leaves the configuration unchanged (`nextConfiguration`), so a new
leader may repeat step 1.

Two places where the accepted text had to be carried over to the new
steps, written the way the point implies and confirmed with the
amendment's acceptance:

- **A leader that is itself `removing`** hands leadership over first,
  as the accepted text already said for a leader asked to remove itself.
- **Nodes dropped by `recover`** (B4) have no `removing` entry, because
  `recover` writes no Skep entry. The leader writes step 0 itself for a
  dropped node without guest entries (the accepted text already let it
  remove those on its own); for one with guest entries the
  administrator's removal with `--forget-guests` writes step 0.

### 3. Join's point of no return

- Until the leader has applied join step 1, a failed join puts the
  one-node state back, as before.
- After it, the `join` command never puts it back by itself: the node
  keeps retrying with its admission pins and partial log, and the
  command says so.
- To give up, an administrator removes the node on a member, then runs
  `skep-cluster join --abandon` (root) on the host, which restores the
  moved-aside state only if at least one member of the admission answers
  and none accepts its key.
- **Why:** a restored host that the cluster counts as a voter leaves a
  two-node cluster without quorum for good.

Written with one reading made explicit: when the outcome of the join
call is **unknown** (sent, no answer; B11), the command cannot tell
whether step 1 was applied, so it behaves as after the point of no
return.

### 4. Wording

- ADR 0004 amendment, "What does not change": the root-run
  `skep-cluster` commands `init` (reads the node ID) and `join` talk to
  the agent socket as peer UID 0; the service never.
- CLAUDE.md: the statement that the cluster technology is not decided is
  replaced by a pointer to this ADR (Raft in an unprivileged
  `skep-cluster` service), and the Milestone 8 line no longer says to
  finish the Raft vs Corosync evaluation first.

### 5. Applied membership on disk (the open question, answered: option a)

Checking point 1 against `hashicorp/raft` v1.8.0 found that it did not
hold across a restart, and the amendment was left Proposed with one
open question. **Andy answered it on 2026-11-24 with option a.** The
decision is worked into B1 ("What the listener accepts", "Per-RPC
identity", join step 3 and the join command's step 4), B4 and B6.

**Why it was needed** (v1.8.0):

- The commit index is not stored. At start (`api.go` `NewRaft`) a node
  restores its state machine from its latest snapshot and reads the log
  entries after it **for Raft configuration changes only**. Skep entries
  after the snapshot are applied again only when a leader tells the node
  the commit index, or when the node itself wins an election.
- Snapshots are rare by default: after 8192 new entries, checked every
  120 s.
- The optional `RestoreCommittedLogs` does not close this: it needs a
  log store that implements `CommitTrackingLogStore`, which
  `raft-boltdb/v2` v2.4.2 does not, and the commit index it stores is
  written with the *next* log append, so it can be behind the index a
  member has already reported.

So, without a durable record: (1) member M reports that it has applied
the entry with joiner J's pins, J is promoted, M restarts before its
next snapshot, its applied membership is the snapshot's without J, and
if J leads, M refuses the only node that can tell it the commit index;
(2) a node that restarts before its first snapshot has applied **no**
membership and accepts no peer, so after a full stop of a young cluster
no node would accept another.

**The decision:**

- **The file.** The service writes the applied membership to
  `/var/db/skep/cluster/state/membership.json` (amended, amendment 2:
  inside `state/`), as the `skep-cluster` account, mode `0600`,
  atomically: a temporary file in the same
  directory, `fsync`, `rename`, `fsync` of the directory. Content: the
  cluster ID, the membership epoch (B4), the log index it reflects, and
  every member entry (ID, address, both pins, state). **Pending entries
  are not in it:** after a restart a pending key is refused until the
  node has caught up.
- **When.** Whenever an applied entry changes the membership, **before**
  that index is reported as applied on the status RPC and **before** the
  listener uses the new membership.
- **At start** the node accepts keys by the file until its state machine
  has reached the file's index, then by the state machine. The file
  never goes back to a lower (epoch, index).
- **"Applied index"** on the status RPC, for catch-up and for the
  promotion rule, is the **state machine's own** index, not the
  library's `AppliedIndex`.
- **Root commands:** `recover` writes the file for the new membership; a
  restore replaces it with the restored store's; `leave --wipe` deletes
  it; a join moves it aside with the one-node state (and putting that
  state back brings it back). Read together with the previous bullet: a
  restore and `leave --wipe` are the only ways the file goes back or
  goes away, both root-run with the service stopped; the service itself
  never lowers it.
- **It also closes consequence 2 above**, a full stop of a young
  cluster: each node accepts the members in its file.
- **What it costs:** a second copy of the pins on disk (public data, in
  the same directory and with the same owner as the log).

Options considered and not chosen: (b) a Raft snapshot after every
membership entry before reporting it (a snapshot of the whole store per
change on every member, slower joins); (c) reading membership entries
from the log at start, committed or not (could accept a pin from an
entry the cluster later discards); (d) keeping the risk for restarts
(point 1's headline would not be true, and consequence 2 would stay).

Step 8a is not affected: a one-node cluster elects itself at start and
applies its whole log before it serves. The file is built in step 8b.

### 6. A removed node and a member that was down (risk, not prevented)

Added with the acceptance (Andy, 2026-11-24) and worked into B1's risks
and B2: a member that was down while another node was removed still
pins that node until it has caught up. A removed node that is hostile
can reach it first, give it a forged log at a higher term, and that
member could then win an election among the real members. Skep 0.2 does
not prevent this; it would need membership entries signed by a quorum
(B2's hardening option b; backlog row). The rule for operators: after
removing a node because it is compromised, wipe every member that was
down at that time before it returns. Health on every member names the
members that have not yet applied a removal.

## Amendment 2 (2026-11-25, Proposed)

Written with the build of step 8b-2a (removal, `leave --wipe`, `join
--abandon`, a way back to no listener, crash-safe file steps), from the
fixed decisions D1–D14 the review chat gave that run. **Status:
Proposed.** Andy accepts it by approving the merge of the step 8b-2a
PR. It changes the accepted text only where it says so; the changed
passages are marked "Amended (amendment 2)".

### 1. One state directory, one rename (D1)

- The Raft store (`raft.db`), the snapshots and the applied-membership
  file are in **one directory, `/var/db/skep/cluster/state/`** (`0700`,
  the service account). The node key, its certificate and `join.json`
  stay in `/var/db/skep/cluster`.
- A join moves the one-node state aside with **one** `rename(state,
  standalone.<UTC time>)`; putting it back is one rename back. Every
  rename, create and delete in the data directory that a later start
  depends on is followed by an `fsync` of the parent directory.
- **No migration code.** A service that finds `raft.db` directly in the
  data directory refuses to start and names the three paths to move
  into `state/` (0.2 is unreleased; the lab was moved by hand).

### 2. The start rule (D2)

Before anything is opened: a wipe marker (point 5) → refuse; `raft.db` in
the data directory → refuse (point 1); `join.json` in phase `abandoning`
(point 6) → refuse; `state/` present → start; `state/` absent and
`join.json` present → the join decides; `state/` absent, no `join.json`,
exactly one `standalone.*` → rename it back with a warning; several →
refuse and name them. Wherever both a `standalone.*` and `join.json` are
deleted, the `standalone.*` goes first; a node whose own state is
`member`, that is not joining and that still finds a `standalone.*`
deletes it with a log line.

### 3. The leader's membership work is tied to the term (D5)

- The leader repeats its barrier whenever the Raft term differs from the
  term of its last successful barrier (not only when a "was leader" flag
  flips), and does nothing else until that barrier has succeeded.
- **One leader loop** makes every configuration change (`AddNonvoter`,
  `AddVoter`, `RemoveServer`, and a one-node cluster's own address) and
  proposes every membership entry of the leader's own, after a barrier
  in the current term, from a view read after it, **one step per
  round**.
- It never continues a join for a member in state `removing`. Removal
  step 1 runs whenever a `removing` member is still a server in the
  latest Raft configuration, voter or not; the final entry is proposed
  only when the configuration without it is committed.
- **Ghost sweep:** a server in the Raft configuration with no member
  entry is removed (`RemoveServer`) and logged.
- At apply: a promotion of a member that is not `joining`, a `removing`
  entry for the last member that is not `removing`, and a final entry
  for a member that is not `removing` are refused.
- Removal is `skep-cluster remove NAME`, root-only on the local socket in
  this step (the API gets it with `cluster:manage` in 8c). New entry
  types `member.removing` and `member.removed`, version 1. The
  `removing` entry carries the guest-list field of B13, always empty in
  version 1; step 8d writes guests in version 2.

### 4. Removal guards (D7)

- Removing a member is refused while **another** member is `joining`;
  removing the joining member itself is allowed (how a stuck join is
  cleared).
- Applying the `removing` entry **withdraws every pending admission** in
  the same apply: an admission issued before a removal is void.
- Removal is refused when, without the member, fewer than a majority of
  the remaining voters answer a status round made at the request (the
  cluster would then accept no changes until they return). The override
  is `-allow-unanswered` with the cluster ID typed back on `/dev/tty` or
  given as `-cluster-id`; there is no `-yes`.

### 5. `leave --wipe` (D8)

Root, local; it stops the service itself and confirms the cluster ID. The
result is a one-node cluster with a **new cluster ID and a new node
key**, the same node ID, **no listen address and no open port**. It
writes a marker (`wipe.json`) first and removes it last; while the
marker exists the service refuses to start, and running the command
again finishes the wipe without asking again. It deletes every
`standalone.*` and `join.json` before `state/`. It prints that the node
must also be removed on a member, with the command.

### 6. `join --abandon` (D9)

Only with a `join.json`, root, local, the service stopped by the command.
It asks every node of the admission **and of the stored join answer**.
In TLS 1.3 the listener's first byte after the handshake (the accepted
marker) is how "accepts this host's key" is known. It restores the
one-node state only if at least one node answers and none accepts the
key; before any file step it writes the phase **`abandoning`** to
`join.json`, with which the service refuses to start and a rerun
finishes without asking again. Otherwise it changes nothing and names
the node that still accepts the key and the command to run there.

### 7. What a joining node accepts (D10)

Once a join answer has been accepted, the answer's nodes **replace** the
admission's (before, they were added). In phase `refused` the joining
node accepts no inbound key. The answer's addresses get the same port
check as the admission's.

### 8. The removed node (D11)

- Members close its connections when the configuration change without it
  takes effect on them (its key stays accepted in the handshake until
  the final entry, as amendment 1 says), and refuse it in the handshake
  after the final entry. The heartbeat fast path's copy of the Raft
  configuration (at most 250 ms old) bounds how long a removed node's
  heartbeat can still be accepted after the change took effect; the
  in-process test measures it.
- A member sends the removed node a best-effort notice. A node learns it
  was removed by that notice (from a key it accepts, naming it and its
  cluster) or from its own log (its entry `removing` and itself absent
  from its latest Raft configuration, or its entry gone). It writes that
  down durably (`state/removed.json`), shuts its Raft instance and its
  listener, keeps its local socket for `status` and `health` (critical,
  naming the cluster and saying to run `leave --wipe`), and does the
  same after a restart.

### 9. Health after a removal (D12)

Every member's health names the members that have not yet applied the
last removal (from a status round every node makes every 10 s and at
once after a membership change), until all have. `skep-cluster remove`
prints the same list and, when it is not empty, the operator rule of B2.

### 10. Back to no listener (D13)

`skep-cluster listen -none` (root, local) is refused unless the node is
alone in its cluster with nothing pending (read from the store with the
service stopped); afterwards no port is open and `admit` is refused as
on a fresh host.

### What this does not change

The order and crash points of B1, the promotion rule, the
applied-membership file's rules, the trust model of B2 and the risk of
amendment 1 point 6 (a member that was down during a removal still pins
the removed node until it catches up; Skep 0.2 does not prevent it).

