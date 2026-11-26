# skep-cluster (Milestone 8, steps 8a to 8d-1)

`skep-cluster` is the third Skep service, next to `skep-agent` and
`skep-api`. It holds the node's cluster key, the cluster membership and
the replicated store, and runs as its own unprivileged account. The design
is [ADR 0013](../decisions/0013-clustering.md) with its amendments 1, 2
and 3 (amendment 3 Accepted with the 8b-2b PR); this page describes
what is built.

**Built so far:** every host is a one-node cluster (8a), and a host
**without guests** can join another cluster (8b-1): `join-request`,
`admit`, `admit --withdraw`, `join`, Raft over TLS 1.3 on port 8444 with
the sender checked on every RPC, pending entries with expiry and
withdrawal, the join's four steps and their crash points, promotion once
caught up and applied by every member, and the applied-membership file.
Step 8b-2a adds **removal** (`remove`, three records driven by the leader
loop), **`leave --wipe`**, **`join --abandon`**, **`listen -none`**, the
`state/` directory with crash-safe file steps, and health after a
removal. Step 8b-2b adds **node key rotation** (`key rotate`, `key
rotate --abort`), the **cannot-say byte** after the handshake, a
**per-node connection limit**, **clock-offset health**, the **cluster
audit** with `audit verify`, and the `key_refused` warning (see
[Step 8b-2b](#step-8b-2b-key-rotation-cannot-say-limits-clock-audit)).
**Step 8c-1** (replicated users) puts the users in the log: `skep-api`
keeps a read copy of them in `api.db` and sends every user change through
`skep-cluster` (see [Step 8c-1](#step-8c-1-users-in-the-replicated-log)).
**Not built yet**: `skep-setup` initialising and starting `skep_cluster`,
and admit, withdraw and remove in the public API (8c-2); joining a host
with guests (8d); forwarding (8e). `skep-agent` does not use
`skep-cluster`.

**Validated on FreeBSD:** step 8a on `smith` (2026-11-24, build `67396a5`,
[record](../../tests/integration/cluster/results/8a-67396a5-20261124-smith.md)).
Step 8b-1 on `smith`, `langrishe` and `star` (2026-11-24, build `2de184b`,
[record](../../tests/integration/cluster/results/8b1-2de184b-20261124.md)):
three hosts joined into one cluster, the identity and refusal cases, a
full stop and an expiry. What the record lists as in-process only (crash
points, a member down during a join, unknown outcomes, the file's fault
cases) is tested on several nodes over real TLS on loopback, not on
FreeBSD.
Step 8b-2a on the same three (2026-11-25, builds `067a840` and `25d5fa2`,
[record](../../tests/integration/cluster/results/8b2a-25d5fa2-20261125.md)):
the hand move to `state/`, removal of a follower and of the leader, `leave
--wipe`, `listen -none`, rejoining, a removal while a member is down
(refused, then with the override waiting for the member and finishing
1.7 s after its return), the refusals and a full stop. `join --abandon`'s
success path, the leader stopped after each removal record, the D5 term
case, the ghost sweep, D10, the heartbeat bound and every crash point are
in-process only.

## What exists

| Piece | Where | Notes |
|---|---|---|
| Service account | `skep-cluster`, UID and GID **410** | Created by the package's pre-install script, as the `skep` account (409) is. Not the `skep` account: `skep-api` must not be able to read the node key |
| Service | `/usr/local/sbin/skep-cluster`, rc.d `skep_cluster` | Refuses to run as root. Not enabled by the package or by `skep-setup` |
| Configuration | `/usr/local/etc/skep/cluster.json`, `root:skep-cluster`, `0640` | Written by the root-run commands (`init`, `listen`, `join-request`); the service reads it and cannot change it |
| Data directory | `/var/db/skep/cluster`, `skep-cluster:skep-cluster`, `0700` | `node.key` (`0600`), `node.crt`, `audit/` (`0700`, the cluster audit, segments `0600`), and `state/` (`0700`) holding `raft.db` (`0600`), `snapshots/`, `membership.json` (`0600`) and, on a removed node, `removed.json` (`0600`); while joining also `join.json` (`0600`) and `standalone.<UTC time>/` (`0700`, a renamed `state/`); while `leave --wipe` runs, `wipe.json` (`0600`); while the node rotates its key, `node.key.next` (`0600`) and `rotate.json` (`0600`). Nothing in it belongs to root |
| Local API | Unix socket `/var/run/skep-cluster/cluster.sock` | Peer credentials checked; see the table below |
| Node-to-node listener | TCP **8444** on the management address | Open only after `listen -address`, `join-request -address` or `init -address`. A single host opens no port |

## The node's identity

- **Node ID.** One ID per node: the UUID `skep-agent` created in
  `agent.db`. `skep-cluster init`, run by root, reads it from the agent's
  read-only system operation (`GET /internal/v1/system` on the agent
  socket, as peer UID 0, actor `skep-cluster-init`) and writes it to
  `cluster.json`. The service never connects to the agent.
- **Node key.** ECDSA P-256, generated at `init` by the part of the
  command that runs as the service account. Other nodes trust it by its
  **pin**, the SHA-256 of the public key (`sha256:<64 hex>`, the format
  `skepctl` uses for `skep-api`'s key). `join-request` on a one-node
  cluster reuses the key.
- **Certificate.** Self-signed, valid 397 days, renewed with the same key
  30 days before it expires (checked at start and daily), so the pin does
  not change. A failed renewal is a health warning.

## Commands

```
skep-cluster init [-address ADDR] [-name NAME]   root; creates the one-node cluster
skep-cluster listen -address ADDR                root; opens 8444 on ADDR
skep-cluster join-request -address ADDR          root; on the new host: prints its request
skep-cluster admit REQUEST                       root; on a member: admits, prints the admission
skep-cluster admit -withdraw NAME                root; on a member: withdraws a pending admission
skep-cluster join [-cluster-id ID] ADMISSION     root; on the new host: joins
skep-cluster join --abandon                      root; gives up a join past its point of no return
skep-cluster remove [-allow-unanswered] NAME     root; on a member: removes a member
skep-cluster leave --wipe [-cluster-id ID]       root; a new one-node cluster, new key, no listener
skep-cluster listen -none                        root; closes the listener of a node that is alone
skep-cluster status [-json]                      root or skep
skep-cluster health [-json]                      root or skep; exit 1 unless ok
skep-cluster version
```

The procedures, as the commands print them, are in
[Joining hosts into a cluster](../administration/cluster.md).

### The data directory and the start rule (amendment 2, D1 and D2)

The store, the snapshots, the applied-membership file and the removal
record are in **`state/`**. A join moves the one-node state aside with one
`rename(state, standalone.<UTC time>)` and puts it back with one rename;
every rename, create and delete that a later start depends on is followed
by an `fsync` of the parent directory (`internal/cluster/layout`). There
is no migration from the layout of 8a and 8b-1: a service that finds
`raft.db` directly in the data directory refuses to start and names the
three paths to move into `state/`.

**The start rule** (`service.StartRule`), before anything is opened:

| Found | The service |
|---|---|
| `wipe.json` | refuses: "`leave --wipe` did not finish; run it again" |
| `raft.db` directly in the data directory | refuses and names the paths to move |
| `join.json` in phase `abandoning` | refuses: "`join --abandon` did not finish; run it again" |
| `node.key.next` without `rotate.json` | deletes it, with a warning (8b-2b) |
| `rotate.json` and `node.key.next` | goes on with the rotation |
| `rotate.json`, no `node.key.next`, `node.key` with the new pin | switched: goes on at step 5 |
| `rotate.json`, no `node.key.next`, the old pin, phase `aborting` | deletes `rotate.json` (the abort's last delete) |
| `rotate.json`, no `node.key.next`, the old pin, phase `rotating` | refuses and names the three files |
| `state/` | starts normally (a `removed.json` in it: only the local socket, below) |
| no `state/`, `join.json` | starts on an empty `state/`: the join decides |
| no `state/`, no `join.json`, one `standalone.*` | renames it back to `state/`, with a warning |
| no `state/`, no `join.json`, several `standalone.*` | refuses and names them |

Wherever a `standalone.*` and `join.json` are both deleted (a finished
join, a wipe), the `standalone.*` goes first. A node whose own state is
`member`, that is not joining, and that still finds a `standalone.*`
deletes it with a log line. A message saying "the next start of
skep_cluster puts it back" is printed only when the rule above would.

**`init`** does its work in two parts. As root: it refuses if the host is
already initialised, reads the node ID from the agent, creates the data
directory for the service account when it is missing, and writes
`cluster.json`. Then it gives up root for the service account
(`internal/privdrop`) and, as that account, creates the key, the
certificate and the store with this node as its only member. Without
`-address` the member entry has no address and the service opens **no
network listener**.

**`listen -address ADDR`** is for the founding node of a cluster, before
its first `admit`. It refuses unless the Raft configuration is this node
alone (asked from the running service) and unless `ADDR` is one specific
address of this host, writes `listen_address` to `cluster.json` and
restarts `skep_cluster`. At start the service updates its own member entry
and its own server entry in the Raft configuration, and opens the
listener. `skep-api`'s node-to-node listener (port 8445) comes with step
8e.

### What runs as root, and what it believes

Root never creates, writes or moves anything inside the data directory.
Every root-run command that works on the store starts **this same
executable as the service account** (`internal-stage`, an exec with the
account's UID and GID, no supplementary groups, a fixed environment), and
every root-run command ends by checking that no file in the data directory
belongs to root.

- **`join-request -address ADDR`**, as root: checks the address and
  `cluster.json`; reads the host's VMs and jails from the agent (actor
  `skep-cluster-join`) and **refuses if there is one or if the agent does
  not answer**; stops `skep_cluster`; the stage, as the service account,
  reads the Raft configuration **from the store with the service stopped**
  and the key's pin; refuses unless the node is alone; root writes
  `listen_address` to `cluster.json` and starts the service; it waits for
  the listener and the member entry's address, checks that the running
  service shows the pin the stage read, and prints the request.
- **`admit`** and **`admit --withdraw`** call the local API (root only, D2
  of the run). The service passes them to the leader, see below. `admit`
  refuses a request whose address has a port other than **8444**, in the
  command and again in the leader's side of the service's `Admit` (also
  when a member passed it to the leader), as `invalid_request`.
- **`join ADMISSION`**, as root: decodes the admission and refuses one for
  another host (ID, name, address), and one in which any address (the
  admitted node's or a member's) has a port other than **8444**; refuses a host with guests or without
  an answering agent; prints the cluster ID and every member (name, ID,
  address, pin) and asks for `yes` on `/dev/tty`, or, with no terminal,
  needs `-cluster-id` equal to the admission's (there is no `-yes`); stops
  `skep_cluster`; starts the stage, which **makes the join call**:
  1. it checks that the admission names this key, and reads "is this node
     alone" from the store;
  2. it renames `state/` to `standalone.<UTC time>` and writes
     `join.json` (phase `calling`, a new operation ID, the admission);
  3. it calls the join operation on the admission's members (below);
  4. **only when it knows join step 1 was not applied** (every handshake
     failed and nothing was sent, or a leader refused, or no member had a
     leader) it **removes `join.json` first** (with the directory fsync)
     and **then** puts the one-node state back; after an accepted call
     whose answer passed the check (below) it records the answer (phase
     `accepted`), and after an unknown outcome (sent, no answer, or an
     answer that failed the check) phase `unknown`.

  The start rule (above) decides what the next start does with what a
  stopped stage left.

  Root then starts `skep_cluster` in every case and runs the root-file
  check. **What root believes from the stage** is only its outcome, for
  what it prints; no root effect depends on it (the service is started
  whatever the outcome). After `accepted` or `unknown` the command says
  the join is past its point of no return and how to give it up
  (`remove` on a member, then `join --abandon`), then waits (default
  90 s) for the node to apply its own member entry.
- **`remove NAME`** calls the local API (root only). The service passes it
  to the leader, which checks it (below, "Removal") and proposes the first
  entry; the command then waits (default 2 min) until the member entry is
  gone, prints the members that have not yet applied the removal and, when
  the list is not empty, the operator rule of ADR 0013 B2. No root effect
  depends on the service's answers. `-allow-unanswered` needs the cluster
  ID typed back on `/dev/tty` or given as `-cluster-id`; the service
  checks it again.
- **`leave --wipe`**, as root: loads `cluster.json`; the stage
  `wipe-info-files` (service account, the service still running) reads
  the cluster ID from `wipe.json`, `state/removed.json`,
  `state/membership.json` or `join.json`, and whether a wipe is under way;
  root asks for the cluster ID on `/dev/tty` or checks `-cluster-id` (not
  when a wipe is under way) **before any stop**, so a wrong or missing
  confirmation stops nothing (F2). Only when the files do not tell the ID
  (the store must), or for a resumed wipe, is `skep_cluster` stopped
  first and the stage `wipe-info` (which also reads the store) asked
  before the confirmation. Otherwise root stops `skep_cluster` after the
  confirmation and `wipe-info` must give the same ID; the stage
  `wipe-begin` writes `wipe.json` (the new cluster ID, the old pin)
  unless it exists; **root removes `listen_address` from `cluster.json`**;
  the stage `wipe-finish` deletes every `standalone.*`, then `join.json`,
  then `state/`, replaces the node key if it still has the old pin, makes
  a one-node cluster with the marker's cluster ID and no address, and
  removes `wipe.json` last; root starts the service and runs the root-file
  check. Each step can be repeated, so the command run again after a crash
  finishes the wipe without asking.
- **`join --abandon`**, as root: first the stage `abandon-check`
  (service account, the service still running, it writes nothing)
  refuses without any stop (F2) when there is no `join.json`, when
  `join.json` names no `standalone.*` that exists (F3: the one-node state
  is gone; `leave --wipe` is the way out), or when a node accepts the key
  or none answers. Only then root stops `skep_cluster`; the stage
  `abandon` (service account, it holds the key) checks the moved-aside
  state again and asks every node of the admission and
  of the stored join answer whether it accepts this host's key
  (`raftnet.ProbeKey`: a TCP connection and a handshake with that node's
  pin, then the listener's first byte: the accepted marker, or a refusal).
  Only if at least one answers and none accepts, it writes phase
  `abandoning` to `join.json`, deletes `state/` (the joining node's log),
  renames `standalone.<UTC time>` to `state/` and removes `join.json`
  last; with phase `abandoning` already there it finishes these steps
  without asking again. Root starts the service and runs the root-file
  check; what it prints comes from the stage's outcome.
- **`listen -none`**, as root: refused unless `cluster.json` has a listen
  address; asks the running service's status and refuses **without a
  stop** (F2) when it shows more than one member, a pending admission, a
  join in progress or a removed node (believing the socket for a refusal
  has no root effect); when it shows the node alone with nothing pending,
  or the socket does not answer, stops `skep_cluster`; the stage `read-state` rebuilds the state
  from the store with the service stopped (`store.ReadState`: snapshot
  plus every stored entry, valid on a node that is alone) and refuses
  unless the node is alone, the only member, nothing is pending, and it is
  not joining or removed; root removes `listen_address` from
  `cluster.json` and starts the service, which updates its own member
  entry and Raft server entry to "no address" and opens no port; `admit`
  is then refused (`no_listen_address`) as on a fresh host.

### The join call and what a joining node accepts

- The joining node dials **only the members of its admission**, and the
  members of the join call's answer once it has one. A redirect to the
  leader (`not_leader`) is followed only when it names a node of that list
  at the address the list gives (D8 of the run).
- **The join call's operation ID** is a lower-case UUID (a new one per
  join, the same on every repeat); the leader refuses anything else as
  `invalid_request`. The `member.join` entry's operation ID is
  `join:<node ID>:<that UUID>`, so a joining key can never match another
  entry's operation ID. After the write, **also when it was a duplicate**,
  the leader answers accepted only if a member entry for that node with
  that pin exists; otherwise it answers `refused`.
- **The joining side checks the answer before it uses or stores it**: its
  cluster ID is the admission's, it lists 1 to 64 nodes, and every node
  passes the validation an admission's nodes pass (ID, name, address,
  pin, none twice). An answer that fails is not stored and makes the
  outcome **unknown** (the request was sent and may be applied): the
  one-node state is never put back for it. The service's repeat of the
  call checks the same.
- A refusal from a leader, a handshake failure (nothing sent) and "no
  leader" are each certain to have applied nothing. A request sent without
  an answer, a leader's `outcome_unknown`, or an answer that fails the
  check makes the outcome unknown, and a later refusal in the same command
  does not undo that.
- The service starts on an empty store when `join.json` exists. Until its
  accepted membership lists this node it accepts **only the admission's
  keys, or, once a join answer has been accepted, only the answer's**
  (the answer replaces the admission's list; amendment 2, D10), as
  members, and applies no Raft-configuration rule. In phase `refused` it
  accepts **no** inbound key. The answer's addresses get the same port
  check (8444) as the admission's. Phase `unknown` (or `calling`, found at start) makes it repeat the
  call with the same operation ID every 10 s; a repeat after step 1 is
  answered as accepted (and refused once the member is `removing`). A
  refusal after an unknown outcome is phase `refused`: health is
  critical, the one-node state stays aside, and health says how to give
  the join up.
- When the node has applied a membership that lists it in state `member`,
  the service deletes `standalone.*/` and `join.json`.

### Request and admission

One line each: `<kind>:1:<payload>:<check>`, `kind` being
`skep-join-request` or `skep-admission`, `payload` the unpadded base64url
of a JSON object, `check` the first 16 hex digits of its SHA-256. A line
is decoded strictly: at most 2 KiB (request) or 32 KiB (admission), the
kind and version, the check (a truncated or mistyped paste is reported as
damaged), no unknown JSON field, every value validated, no duplicate ID,
name, address or pin. Nothing in either is secret.

- Request: node ID, name, address (`ip:8444`), node-key pin. `admit` and
  `join` refuse any address whose port is not 8444; the state machine's
  address check does not fix the port (so that the multi-node tests run on
  loopback), and skep-cluster itself always writes 8444.
- **Terminal output.** Every string that comes from another node or from
  the stage (error messages, attempt lists, names and fields in answers
  and in `status`) has its control characters replaced by `?` before
  `join`, `join-request`, `admit`, `status` or `health` prints it; an
  error of several lines keeps its line breaks and each line is cleaned.
- Admission: cluster ID, the admitted node (as in its request), every
  current member entry (ID, name, address, pin; `joining` ones included),
  the pending entry's expiry. No API pin in either: `skep-api`'s
  node-to-node key and port 8445 come with step 8e (D3 of the run).

## The local API and who may call it

The socket's directory belongs to the service account with the group
`skep`, mode `0750`; the socket has mode `0660`. The service reads each
connection's peer credentials from the kernel and **closes any connection
whose UID is not root or `skep`** before reading a byte. Each operation
names which of the two may call it; any other caller gets `403
forbidden`.

| Operation | Request | root | skep |
|---|---|---|---|
| `health` | `GET /cluster/v1/health` | yes | yes |
| `status` | `GET /cluster/v1/status` | yes | yes |
| `raft-configuration` | `GET /cluster/v1/raft/configuration` | yes | no |
| `admit` | `POST /cluster/v1/admit` | yes | yes |
| `admit-withdraw` | `POST /cluster/v1/admit/withdraw` | yes | yes |
| `remove` | `POST /cluster/v1/remove` | yes | yes |
| `key-rotate` | `POST /cluster/v1/key/rotate` | yes | no |
| `key-abort` | `POST /cluster/v1/key/abort` | yes | no |
| `users-read` | `GET /cluster/v1/users` | no | yes |
| `users-write` | `POST /cluster/v1/users/write` | no | yes |
| `guests-read` | `GET /cluster/v1/guests` | no | yes |
| `guests-write` | `POST /cluster/v1/guests/write` | no | yes |

`key-rotate` and `key-abort` (step 8b-2b) are root-only and local: no
operation of `skep-api` changes a pin; `key rotate --api` arrives with
forwarding (8e). Their own errors: `409 rotation_in_progress` (this node
or another member is rotating), `409 no_rotation` (abort without
`rotate.json`), `409 refused` (abort once `node.key` has the new pin or
`node.key.next` is gone; the leader or the state machine refused the add,
after which the rotation is aborted in the background). The remove operation has
its own time limit of 60 s (every other operation 15 s), and the
command's client waits above it.

`admit`, `admit-withdraw` and `remove` are open to root's commands and,
from step 8c-2, to `skep-api` for a user with `cluster:manage` (ADR 0013
amendment 5, D22–D24). For the API account the body must name the user
as `actor` (`user:NAME`, the `user:` form of the users-write rule) and
carry a `request_id` (logged on this node; the node-to-node message is
unchanged, so it stays here); the entry's actor is that user. Root names
neither: a root request with an actor or a request ID is `400`, and its
actor is `root`. `allow_unanswered` is refused for the API account (`403
forbidden`), and its removal must carry `cluster_id`. A `cluster_id`
given with any removal is compared on the node that receives it and again
on the leader: `409 cluster_id_mismatch`, nothing written; on a node
that does not know its cluster ID yet, `503 unavailable` ("this node is
not ready"). Their errors: `400
invalid_request`, `409 no_listen_address`, `409 joining`, `409 refused`
(the leader or the state machine refused it, with the reason), `409
removed` (this node was removed), `409 voters_unanswered` (remove: too
few of the remaining voters answer), `404 not_found` (withdraw, remove),
`503 handover_failed` (remove of the leader), `503 not_committed` /
`no_leader` / `outcome_unknown`. A member that is
not the leader passes the operation to the leader as a typed Skep RPC;
the leader checks it again and the state machine checks it again at apply
on every node. The actor recorded is `root` (the verified peer) or the
API's user; the entry's origin is the node where the command ran. The
messages are package `memberapi` (standard library and `clusterstate`
only), which `skep-api` uses without linking the Raft library.

`status` returns the node and cluster IDs, the pin, the listener, the Raft
state, the state machine's index (below), the members with their Raft role,
the pending admissions, where the accepted keys come from (`state
machine`, `membership file`, `admission`) and, while joining, the join's
phase.

## The node-to-node listener and transport

- **TLS 1.3 only**; a client certificate is required and accepted by its
  pin alone: pinned in a member entry (`joining`, `member` or `removing`)
  or in a pending entry of the membership the node has applied. A pending key
  whose expiry time has passed **by this node's clock** is refused
  earlier; none is accepted after the expiry entry. Every other key is
  refused in the handshake. No session tickets; an expired certificate is
  refused even when pinned.
- After a successful handshake the listener sends one byte (`0x01`): in
  TLS 1.3 the client's handshake ends before the server has checked the
  client's key, and this byte is how a client knows its key was accepted
  before it sends anything.
- One port carries both protocols: a connection whose first byte is one of
  the library's RPC types carries Raft RPCs; one that starts with `S`
  carries one Skep RPC (4-byte length, JSON, answered the same way).
- **A key that may not make Raft RPCs (a pending key) is refused before
  any RPC is read or decoded**: when the first byte is a Raft RPC type, the
  receiver looks the key up, and if its kind may not make Raft RPCs it
  logs `raft connection refused before any rpc was read` and closes the
  connection. The per-RPC check below stays as it is.
- **On every RPC** (ADR 0013 B1, "Per-RPC identity"): the header's node ID
  has an entry whose pin is this connection's key; that kind of key may
  call the operation (table below); for a Raft RPC also the header's
  address is that node's address, the protocol version is 3, the
  deprecated `Leader`/`Candidate` fields (if set) equal the header's, and
  the node is a server in the receiver's **latest** Raft configuration
  (read from the library for every RPC but the heartbeat fast path, which
  uses a copy at most 250 ms old so that it never waits for the Raft main
  loop). The TCP source address is not compared.
- **Outbound**, the library's `NetworkTransport` dials through Skep's
  stream layer, which finds the node by the address the library gives
  (addresses are unique among accepted keys) and accepts only that node's
  pin. A Skep RPC client names the pin it expects.
- When an applied entry changes the membership or the pending entries,
  and every second (for clock-based expiry and for configuration
  changes), the listener closes every open connection whose key is no
  longer accepted, and every connection of a `removing` member that is no
  longer a server in the latest Raft configuration (amendment 2, D11).

| Operation on 8444 | pending key | joining key | member key | removing key | Also |
|---|---|---|---|---|---|
| `raft` (AppendEntries with its heartbeat fast path, RequestVote, RequestPreVote, InstallSnapshot, TimeoutNow) | no | yes | yes | yes | a server in the latest Raft configuration |
| `join` | yes | yes | no | no | |
| `status` | no | yes | yes | yes | |
| `admit` (passed to the leader) | no | no | yes | no | |
| `withdraw` (passed to the leader) | no | no | yes | no | |
| `remove` (passed to the leader) | no | no | yes | no | |
| `removed` (the notice to a removed node) | no | no | yes | no | |
| `clock` (the node's time, for the clock check) | no | yes | yes | yes | |
| `key-add` (passed to the leader) | no | no | yes | no | only for the caller's own node |
| `key-abort` (passed to the leader) | no | no | yes | no | only for the caller's own node |
| `users-write` (passed to the leader, step 8c-1) | no | no | yes | no | the leader answers only after its barrier of the current term |
| `guests-write` (passed to the leader, step 8d-1) | no | no | yes | no | the leader answers only after its barrier of the current term; the entry's owner is the node of the calling key, never the body |

The table is `raftnet.Operations`; the tests are driven from it. On a
joining node that its own membership does not list yet, the admission's
and the answer's keys are member keys.

**Bounds.** Handshake 10 s, at most 64 handshakes in progress, at most
256 accepted connections; an accepted connection may idle 5 min between
RPCs; one RPC is read within 30 s and at most 32 MiB; an InstallSnapshot
may stream at most 256 MiB, with a deadline scaled like the sender's (10 s
per 256 KiB); the decoder allocates at most 64 KiB up front for a declared
collection; a Skep RPC is at most 256 KiB each way and 30 s; a dial and
handshake 10 s; a status RPC of the promotion check 5 s; the library's
pool keeps 3 connections per peer.

## The replicated state

- `hashicorp/raft` v1.8.0 with `raft-boltdb/v2` v2.4.2 (bbolt) and the
  library's file snapshots; pure Go, `CGO_ENABLED=0`. Versions pinned in
  `go.mod`. Library default timeouts, pre-vote on. Only
  `internal/cluster/store` and `internal/cluster/raftnet` import the
  library. A node without a listen address uses the in-memory transport
  and opens no port.
- **Every Skep log entry has an envelope**: `type`, `version`, `op_id`
  (applying the same ID twice is a no-op), actor, origin node, optional
  request ID. Every type is at version 1:

| Type | Data | Applying it |
|---|---|---|
| `cluster.init` | cluster ID, first member | creates the cluster, epoch 1 |
| `member.address` | node ID, address | only while that node is the only member and nothing is pending |
| `pending.create` | node ID, name, address, pin, `expires_at` (leader's clock + 30 min) | creates a pending entry; refused while any member is `removing` (F4: the admission would list the member being removed, and the joining node would accept its key) |
| `pending.withdraw` | node ID, name | removes it |
| `pending.expire` | node ID, `expires_at`, `at` (leader's clock) | removes it when `at` ≥ `expires_at` |
| `member.join` (join step 1) | node ID, pin, `at` (leader's clock) | consumes the pending entry, creates the member entry in state `joining` with the entry's index (`joined_index`) and op ID |
| `member.promoted` (join step 4) | node ID | `joining` → `member` |
| `member.removing` (removal step 0) | node ID, `forget_guests` (always empty in version 1) | `joining` or `member` → `removing`; refused for a member already `removing`, while **another** member is `joining`, for the last member that is not `removing`, with a non-empty guest list (step 8d-2 writes guests in version 2), and (step 8d-1, amendment 6) while the node owns a guest or a name reservation; **withdraws every pending entry** in the same apply |
| `member.removed` (removal step 2) | node ID | removes a `removing` member entry and its pins; records the entry's index as the last removal |

- **Apply is deterministic**: no wall clock (every time is in the entry,
  written by the proposing leader), no local file content, no map order
  (members and pending entries are walked sorted by ID). **An entry of an
  unknown type or a newer version, or one that cannot be decoded, stops
  the machine** (B5): nothing after it is applied, health reports it, no
  key is accepted and no snapshot is taken.
- **A decodable entry with invalid values is refused and changes
  nothing**, the same on every node, checked at apply and not only at the
  proposer: an invalid node ID, name, pin or address (one specific unicast
  IP, canonical `ip:port`, port not 0), an API pin (none before 8e), a
  missing expiry, a duplicate ID, name, address or pin among members and
  pending entries, a pending entry while a member has no address, a
  withdrawal or expiry naming no such pending entry (or another expiry
  time), an expiry not yet due, a join without a pending entry, with
  another pin or at or after the expiry, a promotion of a node that is not
  `joining`, an address change with more than one member or a pending
  entry, a second `cluster.init`, a cluster ID that is not a UUID, and the
  removal refusals in the table above.
- Each membership event is **one log line** on every node that applies it
  (`cluster membership: pending created | pending used | pending expired |
  pending withdrawn | member joining | member | member removing | member
  removed`) with actor, operation ID and origin; the leader logs
  `non-voter added`, `promoted`, `server removed` and `ghost removed`
  (actor `system`). The cluster audit (B10) is step 8b-2b.
- **Step 8d-2** adds the joining host's guest list to `member.join` as its
  version 2 (ID, name, kind per guest), so that the name check and the
  inventory write stay in join step 1 (ADR 0013 B3); version 1 means a host
  without guests, which is all this build lets join.
- The user entries (step 8c-1) and the guest entries (step 8d-1) are in
  their own sections below.

**Snapshot formats.** Every earlier format is read; a node writes the
oldest format that holds its state, so a build of an earlier step in a
cluster being upgraded still reads it:

| Format | Step | Adds | Written when |
|---|---|---|---|
| 1 | 8a | – | never again (read only) |
| 2 | 8b-1 | pending entries, epoch | no member was ever removed |
| 3 | 8b-2a | the last removal's index | a member was removed |
| 4 | 8b-2b | a member's next pin | a member rotates its key |
| 5 | 8c-1 | users, users index, forgotten operation IDs | the state holds a user or forgot an ID |
| 6 | 8d-1 | guests, reservations, guests index | a guest or a reservation exists, or the guests index is above 0 |

### The applied-membership file

`membership.json` (ADR 0013 amendment 1, point 5): the cluster ID, the
epoch, the log index it reflects, and every member entry (ID, name,
address, pin, API pin, state), **no pending entry**; mode `0600`, written
by the service as the service account through a temporary file in the same
directory, `fsync`, `rename`, `fsync` of the directory.

- Written **inside the apply of every entry that changes the membership**,
  before the machine's applied index moves (so before the status RPC
  reports it) and before the listener sees the new membership. A failed
  write **stops the machine at that entry**, which is then not applied.
- A snapshot restore (at start or by InstallSnapshot) changes the
  membership too and writes the file first by the same rules; a failed
  write fails the restore.
- **Never written back to a lower (epoch, index)**: a lower or equal one
  changes nothing. The service never lowers or removes it; a join moves it
  aside with the one-node state and putting that state back brings it back.
- **At start** the node accepts keys by the file until its state machine's
  applied index has reached the file's index, and by the state machine from
  then on. So after a restart a pending key is refused until the node has
  caught up, and a full stop of a young cluster (no snapshot yet) elects a
  leader again.

### Applied index and promotion

- **The state machine's applied index** is the index of the last Skep entry
  it applied (or refused, or found a duplicate); the library's own entries
  (a no-op at each term, configuration changes, barriers) never reach it.
  The status RPC and the promotion rule use it, never the library's
  `AppliedIndex`, and a stopped machine never counts as caught up.
- `skep-cluster status` no longer shows "applied 7 of 10" as a lag: when
  the machine is not stopped, the library has applied its commit index and
  every entry between the two indexes is one of the library's own, it
  prints "up to date, last Skep entry 7 (entries 8-10 are Raft's own)".
- **The leader's work** (amendment 2, D5): **one leader loop** makes every
  configuration change (`AddNonvoter`, `AddVoter`, `RemoveServer`, and a
  one-node cluster's own address) and proposes every membership entry of
  the leader's own. Whenever the Raft term differs from the term of its
  last successful barrier it makes a barrier first (retried every second
  until one succeeds in the current term), and reads the state after it.
  Until that barrier has succeeded, the leader answers the **peer
  operations** (join, removal, key rotation) `unavailable` (amendment 3,
  point 9; `notLeader` in `service/peer.go`): its view may lack entries
  of earlier terms, and a join refused from a stale view records phase
  `refused`, which sticks; `unavailable` makes the caller ask again. The
  leader's local paths are unchanged.
  Then **one step per round** (the next round follows at the next tick
  after a step, otherwise after 10 s), in this order: a leader that is
  itself `removing` hands the leadership over (`LeadershipTransfer`);
  nothing while its latest configuration is not committed; a one-node
  cluster's address; the **ghost sweep** (a Raft server without a member
  entry is removed and logged); the next removal step (below); the expiry
  of every pending entry its clock has passed; the next join step for a
  member in state `joining` (never `removing`): not a Raft server → join
  step 2 (`AddNonvoter`); a non-voter → step 3 when the joiner's applied
  index is within one snapshot threshold (8192) of the leader's commit
  index **and** every member in the Skep membership (the joiner and the
  leader included) answers the status RPC with an applied index at or past
  the member's `joined_index` and a running machine; a voter → step 4.
  Every step can be repeated: a new leader finishes what the last one
  left. Skep bounds the wait for a configuration change at 30 s (the
  library's own has none); a change still in flight is read again next
  round.
- Every node (not only the leader) asks every member the same way every
  10 s while a join is in progress, so **health on every member, the joiner
  included, names the member being waited for** (check `join`, a warning).
- No case was found where the machine's own index makes catch-up fail: the
  gap between it and the commit index is the library's own entries after
  the last Skep entry, a handful against a threshold of 8192.

### Removal (amendment 2, D3–D7, D11, D12)

- **`remove NAME`** reaches the leader (a member that is not the leader
  passes it as a typed Skep RPC). The leader refuses: an unknown name
  (`not_found`), a member already `removing`, a removal while **another**
  member is `joining` (removing the joining member itself is allowed: it
  is how a stuck join is cleared), a wrong cluster ID with the override,
  and, **without `-allow-unanswered`**, a removal after which fewer than a
  majority of the remaining voters answered a status round made at once
  (`voters_unanswered`: the cluster would accept no changes until they
  return). When the node to remove **is the leader**, it first hands the
  leadership over and passes the removal to the new leader; if the
  hand-over fails, nothing is written (`handover_failed`).
- Then **step 0** (`member.removing`, the operation ID a new UUID, actor
  `root`, origin the node where the command ran). The leader loop does
  **step 1** (`RemoveServer`) while the member is a server in the latest
  configuration, voter or not, and **step 2** (`member.removed`,
  operation ID `removed:<node ID>:<joined index>`) once the configuration
  without it is committed, then sends the removed node a best-effort
  notice (`removed`).
- **Members close the removed node's connections** when the configuration
  without it takes effect on them, and refuse its key in the handshake
  after step 2. The heartbeat fast path's copy of the Raft configuration
  (at most 250 ms old) bounds how long a removed node's heartbeat can
  still be accepted after that: the in-process test measured 15 ms on
  loopback (`TestRemovedHeartbeatBound`).
- **No admission during a removal** (F4): while any member is
  `removing`, the leader refuses `admit` before it proposes (`409
  refused`, naming the member), and `pending.create` is refused at apply.
- **The removed node** learns it from its own log (its entry `removing`
  and itself absent from its latest Raft configuration while it is not
  the leader, or its entry gone once its state machine has caught up), or
  from the notice **once checked** (F1). A notice from a member key it
  accepts, naming it and its cluster, only starts a check in its own
  goroutine (one at a time; a notice during a check is dropped): the node
  probes with its own key (`raftnet.ProbeKey`) every other node it
  accepts now (the members of its view; on a joining node the nodes of
  its admission or answer), the sender included, and believes the notice
  only if at least one answers and none that answers accepts its key. Up
  to three rounds, 2 s apart (the other members apply the final entry a
  moment after the leader), each probe bounded by 3 s; then the notice is
  dropped with a log line. `how` in the record reads "notice from NAME,
  confirmed by N members refusing this key". It writes
  `state/removed.json`, shuts its Raft instance and its listener, and
  keeps its local socket: `status` says REMOVED, health is critical
  (`removed`, naming the cluster and saying to run `leave --wipe`, or
  `join --abandon` when `join.json` exists, F7), `admit` and `remove`
  answer `removed`. At every later start it runs only its local socket.
- **Health after a removal** (D12): every node runs a status round every
  10 s and at once after a membership change, in its own goroutine and
  one at a time, so that a member that does not answer does not hold the
  once-a-second work (F6). Each node remembers, in memory, the highest
  applied index each member has ever answered with since it started
  (F5). Health (`removal_applied`, a warning) names, until all have
  applied it, with the operator rule: a member never heard from since
  this node started ("no answer since this node started") and one whose
  highest is below the last `member.removed` entry ("applied N, the
  removal is M"); a member at or past it is not named, whether or not it
  answers now. While a removal runs, `removal` (a warning) says what
  it waits for: the leader's `RemoveServer`, the configuration without
  the node to commit (naming the voters that do not answer), or the final
  entry.

## Health

`skep-cluster health` (and `GET /cluster/v1/health`) reports: `raft` (a
leader exists), `state_machine` (not stopped), `membership` (this node's
member entry exists and pins the key on disk; not checked while joining),
`address` (the member entry and the Raft configuration match
`cluster.json`; a clustered node's address is not changed this way),
`listener` (open when configured), `certificate` (renewal, warning),
`join` (a join in progress and whom its promotion waits for, warning),
`removal` (a removal in progress and what it waits for, warning),
`removal_applied` (members that have not yet applied the last removal,
warning; nothing until the first status round since the start has
ended), `key_refused` (every member that answers refuses this node's key,
three rounds in a row, warning), `key_rotation` (what a rotation waits
for, warning), `clock` (offsets over 5 s warning, over 15 s critical),
`joining` (this node's own join: critical when it was refused after an
unknown outcome), and on a removed node only `removed` (critical). `skep-api`'s health does not include it yet.

## hashicorp/raft v1.8.0: facts the design relies on

Checked in the module source (`github.com/hashicorp/raft@v1.8.0`) on
2026-11-24 for step 8b-1.

| Fact | Where |
|---|---|
| A node started with an empty log and no configuration never stands for election | `raft.go` `runFollower`: on heartbeat timeout, `configurations.latestIndex == 0` logs "no known peers, aborting election" and stays follower |
| A non-voter never stands for election | `raft.go` `runFollower`: `!hasVote(r.configurations.latest, r.localID)` → no candidate |
| Such a node accepts AppendEntries and InstallSnapshot from a leader | `raft.go` `runFollower` serves `rpcCh` with `processRPC` → `appendEntries` / `installSnapshot` in any configuration |
| Every RPC of protocol 3 carries the sender's ID | `commands.go`: every request type embeds `RPCHeader{ProtocolVersion, ID, Addr}`; `raft.go` `getRPCHeader` fills `ID` from `LocalID`. Holds for AppendEntries (and the heartbeat fast path, which is an AppendEntries), RequestVote, RequestPreVote, InstallSnapshot, TimeoutNow. The library accepts protocol ≥ configured − 1 (`checkRPCHeader`) and still has the deprecated `Leader`/`Candidate` fields; Skep's handler requires protocol 3 and a non-empty ID |
| The library uses the header ID as the leader's identity | `raft.go` `appendEntries` and `installSnapshot`: `setLeader(DecodePeer(a.Addr), ServerID(a.ID))` |
| `NetworkTransport` outbound: the dialler sees only an address | `net_transport.go` `genericRPC` / `AppendEntriesPipeline` / `InstallSnapshot` → `getConnFromAddressProvider(id, target)` → `getConn(address)` → `StreamLayer.Dial(address, timeout)`. The dialled peer can be verified only in `StreamLayer.Dial`, by address (a `ServerAddressProvider` can rewrite the address but not pass the ID to `Dial`) |
| `NetworkTransport` does not give the consumer the connection | `net_transport.go` `handleConn` / `handleCommand` build `RPC{Command, Reader, RespChan}` only; hence Skep's own inbound handler |
| A non-voter does not count for commit or votes | `commitment.go` `newCommitment` and `setConfiguration`: only `Suffrage == Voter` in `matchIndexes`; `recalculate` takes the quorum over those. Votes: `raft.go` `electSelf` / `quorumSize` count voters |
| A snapshot restore goes through the state machine's `Restore` | At start: `api.go` `NewRaft` → `restoreSnapshot` → `tryRestoreSingleSnapshot` → `fsmRestoreAndMeasure` → `FSM.Restore` (skipped only with `Config.NoSnapshotRestoreOnStart`, which Skep leaves false). InstallSnapshot: `raft.go` `installSnapshot` → `restoreFuture` on `fsmRestoreCh` → `fsm.go` `runFSM` `restore` → `fsmRestoreAndMeasure` → `FSM.Restore` |
| Only `LogCommand` entries reach `FSM.Apply` | `fsm.go` `runFSM` `commitSingle`; configuration entries go to an optional `ConfigurationStore`, no-ops and barriers nowhere. Hence the state machine's own applied index |

None of these fails; no finding against the ADR.

### Removal, leadership and configuration changes (checked for step 8b-2a)

Checked in the same module source on 2026-11-25 for removal, `leave
--wipe` and `join --abandon` (decisions D3, D5, D6, D11 of the run).

| Question | Fact | Where |
|---|---|---|
| What `RemoveServer` does when the leader removes itself | The leader appends the new configuration without itself and uses it at once (it keeps leading and replicating). When that configuration **commits**, the leader sees it has no vote in the committed configuration and steps down; with `Config.ShutdownOnRemove` (default **true**, which Skep keeps) it calls `Shutdown()`, otherwise it becomes a follower. So a leader that removes itself shuts its Raft instance once the change commits; Skep therefore hands leadership over first (D6) | `api.go` `RemoveServer` → `raft.go` `requestConfigChange` → `leaderLoop` → `appendConfigurationEntry`; step-down in `leaderLoop` `case <-r.leaderState.commitCh` (`!hasVote(r.configurations.committed, r.localID)` → `stepDown`, then `ShutdownOnRemove`); default in `config.go` `DefaultConfig` |
| When a configuration change **takes effect** | On the leader at append: `appendConfigurationEntry` calls `setLatestConfiguration` and `commitment.setConfiguration` before the entry is replicated, so commit counting and replication (`startStopReplication`) use the new configuration at once. On a follower when the entry is **stored**, before it is committed: `appendEntries` → `processConfigurationLogEntry` → `setLatestConfiguration` | `raft.go` `appendConfigurationEntry`, `appendEntries`, `processConfigurationLogEntry` |
| When it is **committed** | On the leader when the commit index passes it (`leaderLoop`, `commitCh`: `setCommittedConfiguration`); the `IndexFuture` of `AddNonvoter`/`AddVoter`/`RemoveServer` is answered then (or with `ErrLeadershipLost` when leadership is lost first). On a follower when a leader's `LeaderCommitIndex` reaches it (`appendEntries`: `setCommittedConfiguration` when `latestIndex <= idx`). The enqueue timeout of these calls bounds only the hand-off to the main loop; the wait for the commit has no timeout of its own | `raft.go` `leaderLoop`, `appendEntries`, `requestConfigChange` (`ErrEnqueueTimeout`); `ErrLeadershipLost` in `runLeader`'s deferred clean-up |
| Only one change at a time | A leader takes a new configuration change only when its latest configuration is committed and it has committed an entry of its own term | `raft.go` `configurationChangeChIfStable` |
| Reading the latest configuration, and whether it is committed | `GetConfiguration()` returns the **latest** configuration (committed or not) from a copy kept outside the main loop; in v1.8.0 its `Index()` is **always 0** (the future is filled with `latest` only), so it cannot tell whether that configuration is committed. Skep reads that from the log instead: the latest configuration is committed when no configuration entry is stored above `CommitIndex()` (`store.Node.Configuration`). *Corrected during the lab run of step 8b-2a: the first version of this table said `Index()` gives the log index, and the health text built on it was wrong (lab case 4.5)* | `api.go` `GetConfiguration` (`configurations{latest: r.getLatestConfiguration()}`), `raft.go` `getLatestConfiguration` |
| What a removed server does afterwards | The leader stops replicating to it after a best-effort send of the log up to the change (`startStopReplication`: `repl.stopCh <- lastIdx`; `replicate`: `replicateTo(s, maxIndex)`). It never learns that the change committed (no later `LeaderCommitIndex` reaches it). A removed **follower** that stored the change has a latest configuration without its own vote and does not stand for election ("not part of a stable configuration or a non-voter, not triggering a leader election"). One that did **not** receive it still thinks it votes and stands for election when its heartbeat times out | `raft.go` `startStopReplication`, `runFollower`; `replication.go` `replicate` |
| Whether pre-vote stops it from disturbing the cluster | Yes for members that have the change: `requestPreVote` refuses a candidate that is not in the latest configuration, or not a voter, and refuses any candidate while the member has a leader. A pre-vote does not raise the term of the candidate or of the member (`preElectSelf` proposes `term+1` without storing it; a refused pre-vote leaves the member's term unchanged). Skep adds its own refusal: a Raft RPC from a node that is not a server in the receiver's latest configuration is refused before the library sees it | `raft.go` `requestPreVote`, `preElectSelf`, `runCandidate`; `internal/cluster/raftnet` per-RPC check |
| How `LeadershipTransfer` fails | It returns an error future when: the protocol version is below 3 (`ErrUnsupportedProtocol`); a transfer is already in progress (`ErrLeadershipTransferInProgress`); there is no other voter to pick ("cannot find peer"); there is no replication state for the target; catching the target up fails ("replication failed"); the `TimeoutNow` RPC fails; or the leader is still leader one `ElectionTimeout` (1 s) after the start or after `TimeoutNow` ("leadership transfer timeout"). It succeeds (nil) when the leader leaves its leader loop. While it runs, the leader refuses `Apply`, configuration changes and barriers with `ErrLeadershipTransferInProgress` | `api.go` `LeadershipTransfer`; `raft.go` `leaderLoop` (`case future := <-r.leadershipTransferCh`), `leadershipTransfer`, `pickServer` |
| Reading the current term and learning of leadership changes | `CurrentTerm()` reads the term; `State()` and `LeaderWithID()` the role and the leader. `LeaderCh()` signals gains and losses but **drops** signals a slow reader misses (two `true` in a row mean the leadership was lost and gained in between), and `RegisterObserver` delivers `LeaderObservation`/`RaftState` without a guarantee either. Skep therefore compares the term (D5a) and does not rely on either channel | `api.go` `CurrentTerm`, `LeaderCh` (its comment), `State`, `LeaderWithID`; `observer.go` `RegisterObserver` |
| Whether the FSM is told of a committed configuration | Only through the optional `ConfigurationStore` interface: `runFSM` calls `StoreConfiguration(index, configuration)` for each **committed** `LogConfiguration` entry, in log order with the commands. Without it the FSM is told nothing. Skep's FSM does not implement it: the membership decisions read the configuration from the library (`GetConfiguration`), and the leader's are made after a barrier of its current term | `fsm.go` `runFSM` (`configStore, configStoreEnabled := r.fsm.(ConfigurationStore)`) |
| A barrier commits every earlier entry | A barrier is an entry of the leader's current term; when it is applied, every entry before it, configuration changes included, is committed. So after a barrier in the current term, with no change sent since, the latest configuration is the committed one | `api.go` `Barrier`; `raft.go` `leaderLoop` (`commitCh`) |

None of these makes D3, D5, D6 or D11 unworkable. Two consequences are
built in: a configuration change's wait is bounded by Skep (the library's
has no timeout), and a removed follower learns of its removal from its own
log (its entry `removing` and itself absent from its latest configuration)
or from the member's notice once its own key is refused (F1), never from
a commit.

## Comparing the adapted handler with upstream

`internal/cluster/raftnet/net_handler.go` is adapted from the library's
`net_transport.go` (`handleConn`, `handleCommand`) and stays under
MPL-2.0: its header is upstream's header unchanged (`Copyright IBM Corp.
2013, 2025` and `SPDX-License-Identifier: MPL-2.0`, as in v1.8.0's
`net_transport.go`), followed by Skep's adaptation note. **At every update of `hashicorp/raft`**,
before the update is merged, compare it with upstream and fill in this
checklist in the update's PR:

| # | Check | v1.8.0 (2026-11-24, step 8b-1) |
|---|---|---|
| 1 | RPC type bytes (`rpcAppendEntries` … `rpcRequestPreVote`) unchanged, none added | Same five, 0–4. Skep's `peerRPCMagic` (`S`, 0x53) is outside them |
| 2 | Each request type decoded, and the response written as an error string then the response object | Same; `respErr` then `resp.Response` |
| 3 | Heartbeat fast path condition (`Term != 0`, leader address set, `PrevLogEntry == 0`, `PrevLogTerm == 0`, no entries, `LeaderCommitIndex == 0`) | Same, copied |
| 4 | InstallSnapshot: `rpc.Reader = io.LimitReader(r, req.Size)` on the same buffered reader | Same; Skep bounds `Size` and the deadline |
| 5 | Msgpack handles: decoder default, encoder `TimeNotBuiltin = !MsgpackUseNewTimeFormat` | Same, with `MsgpackUseNewTimeFormat` false (Skep does not set it); Skep's decoder sets `MaxInitLen` |
| 6 | Every request type still embeds `RPCHeader` with `ID` and `Addr`; protocol version still 3 by default (`ProtocolVersionMax`) | Yes (`commands.go`, `config.go`) |
| 7 | Deprecated fields (`Leader` in AppendEntries and InstallSnapshot, `Candidate` in RequestVote) still only fallbacks | Yes; Skep refuses them when they differ from the header |
| 8 | `StreamLayer.Dial` still receives the address only; `NetworkTransport` outbound unchanged | Yes |
| 9 | The library's `consumeCh` handshake: unbuffered `RPC` with a buffered `RespChan` of 1 | Same |
| 10 | Anything new in `net_transport.go` that the adapted copy lacks (read the whole diff) | v1.8.0 is the base |

## Step 8b-2b: key rotation, cannot-say, limits, clock, audit

ADR 0013 amendment 3 is the decision record; this is how it is built.

### Node key rotation (D1)

**Entries.** `member.key_add {node_id, next_pin, at}` and
`member.key_done {node_id, keep, add_index}` (version 1; an older build's
state machine stops at them: upgrade every node before the first
rotation). A member entry carries `next_pin` and `key_add_index` while its
node rotates. `pending.create` and the leader's `admit` refuse while any
member has a next pin (`409 rotation_in_progress`); `member.key_add`
refuses while an admission is pending, a member is joining or another
member has a next pin. `member.removed` takes both pins.

**Files on the rotating node.** `node.key.next` (the next key, `0600`,
written with `O_EXCL`, fsync, fsync of the directory; no certificate until
the switch) and `rotate.json` (`{format, old_pin, new_pin, op_id, started,
phase}`, phase `rotating` or `aborting`). The order: next key, record,
`key-add` to the leader (a refusal records phase `aborting` and runs the
abort: nothing is deleted on a refusal alone, review fix F2); the switch
(rename of `node.key.next` to `node.key`, directory fsync, and only once
that fsync succeeded (F3) a new `node.crt` through the "certificate is
not for this key" repair, `nodekey.Key.Adopt` so that the listener, the
diallers and the transport, which hold the same key object, use the new
key from their next handshake, and `Listener.CloseAll`); `rotate.json`
deleted once this node applies its `member.key_done`. Every step can be
repeated after a crash; the start rule table above says what a start does
with each combination of the files. Crash points of steps 1–5 and of the
abort are tested in-process (`TestRotationCrashPoints`), and so are the
refusal path's (`TestRefusedKeyAddCrashPoints`). A rename lost in a crash
after the leader dropped the old pin (`rotateStep`, `keptNewPin`: the
accepted membership, the machine's or the applied-membership file's,
keeps only the new pin while `node.key` has the old one) is done at the
next round (`TestRotationLostRename`). `rotationEnded` reads `node.key`'s
pin from the disk and never deletes `node.key.next` while it may be the
only copy of a pinned key; `AbortRotation` also decides from the disk.

**Who decides.** The rotating node asks; the leader proposes. `key-add`
and `key-abort` are member-key peer operations, and the leader takes the
node from the key (`from.NodeID`), so a member can add or abort a next pin
only for itself. `key-add` repeats with the same operation ID until the
answer is known; a repeat whose next pin is already present is answered
with its add index. `key-abort` makes a barrier first, so that "the add is
not applied" means it never will be. The leader proposes `member.key_done`
keep new in its loop (after removals, before expiries) once every member's
highest answered applied index is at or past the add index and the node
has presented the new pin on any accepted connection to or from the
leader since the leader's service started (`raftnet.PinObserver`:
inbound, the listener's handshake pin; outbound, the pin the dialler
accepted; `Service.used` keeps every pin seen per node, F4, so a later
connection with the old key does not undo it). The leader's refusal of a
`key-abort` sent with the new key compares `raftnet.Peer.ConnPin`, the pin
the connection used, which the listener side fills for a Skep RPC (F5:
`Peer.Pin` is the member entry's pin whichever key was used). For the leader's own rotation it is its own
key. The rotating node switches by its own memory of the same indexes.

**Where a pin is compared.** `ByPin` (and so the listener's handshake, the
per-RPC check and `connGroup`), `ByAddress` and the Raft dialler, the Skep
RPC caller (`CallPins`) and the probes (`ProbeKeyPins`) all take either
pin of a rotating node. Status prints both pins and which one each node
was last seen using; health `key_rotation` (warning) says what a rotation
waits for, on every member.

### The cannot-say byte (D2) and the per-node limit (D5)

After the handshake the listener's first byte is 0x01 (accepted), or 0x02
("cannot say") followed by a close, or — for a key it refuses while it is
up to date — the TLS handshake refusal as before. 0x02 is sent when the
node does not accept the key and `cannotSay()` holds (state machine
stopped, behind `membership.json`, or a joining node in phase `refused`),
or when the key is over its connection limit. For an unaccepted key the
handshake then completes so that the byte can be sent; that is the stated
cost: any key that completes a TLS handshake can learn that the node is
not up to date. `raftnet.KeyAnswer` classifies a call's error the same way
as `ProbeKey` (accepted, refused, cannot say, no answer) without a second
connection; `HandshakeError.Answered` says the TLS handshake with the
expected key completed.

Mixed builds (facts file, point 2): an 8b-2a dialler treats 0x02 as a
failed connection; an 8b-2a probe reads 0x02 as "refuses", as it read the
silent close it replaces; an 8b-2b node reads an 8b-2a listener's silent
close as "refuses". No protocol version change.

The listener counts accepted connections per node (`Options.Group`): 16
per member, joining or removing node (by node ID, so both pins of a
rotating node are one node), 4 per pending key; the 17th gets 0x02. The
256 total and 64 handshakes stay. Measured normal use: at most 2 open
connections of one node on another's listener (three nodes, in-process;
`TestPoolUseUnderPerNodeLimit`).

### key_refused (D6)

Every status round now records what each member's listener answered to
this node's key. After three consecutive rounds with at least one real
refusal and no acceptance, health `key_refused` warns that the node was
probably removed while it was down and names `leave --wipe` (`join
--abandon` with `join.json`). It never puts the node in removed mode. A
round asks the members of `membership.json` while the state machine has
none yet.

### Clock health (D3)

`clock` is a peer operation (every key that may call `status`). The
status round asks each member that answered `status` for its clock after
its status; the offset is the remote time minus the midpoint of send and
receive, the uncertainty half the round trip, and a sample with a round
trip over 2 s is dropped (the last sample is kept, the reason shown).
Health `clock`: warning over 5 s, critical over 15 s (|offset| minus the
uncertainty). Status lists every member's offset, uncertainty and round
trip, or "not known (older build or refused)" / "no answer" / "no sample
yet". An 8b-2a node answers `clock` with `forbidden` (an operation it does
not know); that cannot be told from a real refusal, so every failure reads
"not known", and the status RPC alone decides "answers". Nothing is
refused because of a clock.

### The cluster audit (D4)

Package `internal/cluster/audit`, directory `/var/db/skep/cluster/audit`
(`0700`; segments `audit-NNNNNNNN.jsonl`, `0600`; a lock file `.lock`).
The format, the chain and the keys are in amendment 3, point 6.

- **Before applied.** `clusterstate.Machine` calls its `AuditWriter` for
  every entry it handles — applied, refused at apply (with the reason) or
  a duplicate — after the applied-membership file and before the index is
  reported as applied; an error stalls the machine at that entry. `Restore`
  calls it for a snapshot past the audited key.
- **Start.** `serve` opens the audit before the store. An empty audit gets
  `audit.started` at the last index of the local log (`store.LastIndex`,
  which also counts a snapshot), so the replay of that log writes nothing.
- **Keys per cluster** and the join's skip: amendment 3, point 6.
  `init`, `wipe-finish` and the abandon stage write their mark with the
  last index of the new or restored log (`service.StageMark`); the join
  stage writes `node.join.started` with this node's ID.
- **Local events** (`service.event`, `service.stageEvent`) are written
  after the step they record; a failed write is logged (the service) or
  printed on stderr (a stage) and changes nothing.
- **One writer, the argument.** `audit.Open` takes an exclusive,
  non-blocking `flock` on `audit/.lock` and fails with "another process is
  writing this node's cluster audit" if it is held. The running service
  opens it at start and holds it until it stops. The commands that write
  (the stages `join`, `abandon`, `wipe-begin`, `wipe-finish`, and `init`
  after it has given up root) run only after the root command has stopped
  `skep_cluster` (`stopForStore`, which waits for the store's lock to be
  free), or, for `init`, before the service was ever started; the stages
  that run while the service runs (`abandon-check`, `wipe-info-files`)
  write nothing. So at most one process writes at a time by construction,
  and the lock turns a mistake into an error instead of two chains.
- **Tail repair.** A last line without a newline (a write a crash cut off,
  never reported as written) is removed at the next open, and
  `audit.tail_repaired` records how many bytes went.
- **A failed append** (F6): a failed write or fsync to an existing segment
  truncates the file back to its size before the append and syncs it; if
  that fails too, the log is marked failed and every later append fails
  until the next `Open`, which repairs the tail.
- **A broken audit** (F7): `Open` fails with `audit.BrokenError`, whose
  message gives the way out (stop, `audit verify`, move `audit/` to
  `audit.broken.<UTC time>/` and keep it, start: a new audit begins). The
  start rule, the root-owned-file check and `leave --wipe` look only at
  names they know, so an `audit.broken.*` directory is kept.
- `skep-cluster audit verify [-dir DIR]` reads with `audit.Verify`: no
  write, no create, no lock. `Verify`, `ReadAll` and the tail repair open
  every segment with `O_NOFOLLOW|O_NONBLOCK` and check it with `fstat`
  (F8): a segment that is not a regular file is a break.
- The membership log lines stay; the audit is the record.

### A rejoining node (found on the lab)

A joining node switches from its admission's or answer's keys to its
state machine's membership only once that membership lists it **for this
join**: a member entry whose `join_op` is this join's `member.join`
operation ID, with the current key as pin or next pin (review fix F1;
`Service.listsSelf`, `joinStep`). Its join ends only then. While the
machine is behind the applied-membership file the node is not listed. A
node that was a member before meets its older entries in the log's
history (after `join --abandon`, which keeps the key, even with its
current pin); trusting that membership stopped a rejoin on the lab once
other members had rotated their keys (record `8b2b-6a3268a-20261125.md`,
case 9.7b). In-process the leader sends the whole log before the joiner
applies it, so the race is tested on the decision itself
(`TestJoinStepEndsOnlyAtThisJoinsEntry`) as well as end to end
(`TestRejoinAfterAbandonWithTheSameKey`).

### Library facts (checked for step 8b-2b)

- `LeadershipTransferToServer` does not check the target's suffrage
  (`raft.go` `leaderLoop`); the hand-over picks a voter in state `member`
  itself. The transfer is bounded by one election timeout and allowed
  while a configuration change is uncommitted.
- A joining node receives the log's entries, not a snapshot, while the
  log is shorter than the snapshot threshold; hence the join's skip in the
  audit.

## Step 8d-1: the guest inventory

ADR 0013 amendment 6 (Accepted 2026-11-26, Andy); facts in
[0013-evidence/8d1-facts-20261126.md](../decisions/0013-evidence/8d1-facts-20261126.md).

**Entries**, all version 1:

| Type | Data | Applying it |
|---|---|---|
| `guest.reserve` | kind, name, key, owner, `at` | holds kind and name for the owner; refused when the owner is not a member in state `member` (`owner_not_member`), for invalid values (`invalid`: kind `vm` or `container`; a name of 1 to 255 bytes of valid UTF-8 with no byte below 0x20 and no 0x7f; a key of `[A-Za-z0-9._:-]{1,128}`; a time), at 10000 guests and reservations (`too_many_guests`), and when a guest or reservation of the kind and name exists (`name_taken`, naming the holder's node); **the same owner and key as that reservation or guest is a success that changes nothing** |
| `guest.release` | kind, name, key, owner | removes the reservation when all four match; otherwise nothing changes (a success) |
| `guest.add` | ID, kind, name, task ID, owner, `at` | adds the guest (owner a member in state `member`, the 10000 limit); the same ID and owner changes nothing, another owner is `not_owner`; consumes this owner's reservation of the kind and name whatever its key, and takes its key; **never refused for its name** |
| `guest.remove` | ID, owner | removes the guest; no such guest changes nothing; another owner's is `not_owner` |

The state holds guests by ID (ID, kind, name, owner, key, task ID, the
index that added it, time), reservations by kind and name, and a guests
index (the last entry that changed either). No configuration. **The flag
is computed when read, never stored**: among guests of a kind and name,
every one but the one with the lowest added index is flagged. The cluster
audit records the four types with `guest_id`, `guest_kind`, `guest_name`
and `guest_owner`, each omitted when empty.

**Socket operations** (`guests-read`, `guests-write`, the API account
only). `guests-read` answers the cluster ID, the guests index, the guests
(with the flag and the owner's node name), the reservations, and whether
a leader is known, the machine is stalled, the node is joining or
removed; a joining or removed node answers no guests. `guests-write`
takes one entry (type, operation ID, actor `user:NAME` or
`reconciliation`, request ID, data); **the data has no owner field**
(strict decoding refuses one): the owner is this node, or, when a member
passes the write to the leader as the peer operation `guests-write`, the
node of the calling key. The answer is the index, the cluster ID and the
outcome `applied`, `duplicate` or `unchanged`.

**`skep-api`.** A create or clone reserves its name (the key is the
client's `Idempotency-Key` or a new UUID passed to the agent), then calls
the agent; after an accepted call it reads the guest and adds it (or
releases the name if the guest is not there); after a refusal, or when
nothing was sent, it releases; after anything else (no answer, a timeout)
it writes nothing more. A failed reserve answers `409 vm_name_taken` /
`container_name_taken` (naming the node), `409 too_many_guests`, `409
cluster_refused` or `503 no_quorum`, and the agent is not called. Delete
is unchanged. Reconciliation (one goroutine in `skep-api`, at start,
every 60 s, every 5 s while an accepted create, clone or delete runs)
settles the rest; see ADR 0013 amendment 6, section 3.

**Mixed builds.** A build before 8d-1 stops at the first guest entry, and
reconciliation writes guest entries by itself: **every node is upgraded
while no node of the cluster has a guest.**

## Step 8c-1: users in the replicated log

ADR 0013 amendment 4 (Proposed); facts in
[0013-evidence/8c1-facts-20261125.md](../decisions/0013-evidence/8c1-facts-20261125.md).

**Entries.** `user.create`, `user.update`, `user.password`, `user.rehash`,
`user.delete` and `users.import`, version 1. The state machine checks
every value and is the only authority: the ID a lower-case UUID, the
username rule of `internal/auth` (a test keeps them equal), a display name
of at most 128 characters without control characters, one of the three
built-in role IDs (roles are not replicated), an `$argon2id$` hash of at
most 512 bytes, unique usernames, at most 2000 users, and never an entry
that leaves no enabled administrator (`last_admin`). A refusal carries a
code (`username_taken`, `last_admin`, `unknown_role`, `not_found`,
`users_exist`, `hash_changed`, `too_many_users`, `invalid`). Each user has
a credential epoch, raised by a password change, a role change and a
change of the disabled flag. Times are the proposing leader's, except an
import's users' own. Applied operation IDs are forgotten 100000 indexes
after the entry that applied them, decided from the index alone.
Snapshot format 5 (users, users index, "forgot") is written only once
the state holds a user or forgot an ID. **An 8b-2b build stalls at the
first user entry: every node is upgraded before the first user write.**

**Socket operations** (`users-read`, `users-write`, the API account only).
`users-read` is a long poll: with `after` (a users index) and `wait` (at
most 5 s) it answers once the users index differs, or when the wait ends,
with the cluster ID, the users index, the users with their hashes and
epochs, and whether a leader is known, the machine is stalled, the node is
joining or removed. A joining or removed node answers no users.
`users-write` takes one entry (type, operation ID, actor, request ID,
data); the service builds it (origin this node, the leader's time). A
member that is not the leader passes it on as the peer operation
`users-write`, which the leader answers only after its barrier of the
current term. The answer is the entry's index (the first one, for a
duplicate), the cluster ID, and the user without its hash.

**`skep-api`'s copy.** The `users` table of `api.db` is the copy, with
the cluster ID it belongs to and its users index (table `cluster_copy`).
A long poll applies, in one transaction, every answer that has a cluster
ID and does not come from a joining or removed node, with or without a
leader and stalled or not: what the node's own state machine holds is a
committed prefix of the log (whether there is quorum is a separate
question, below). `apidb.ApplyAnswer` is the only writer of the
replicated columns: it adopts the first cluster, replaces the users and deletes
every session for another cluster ID, never goes back to a lower users
index for the same cluster, and ends every session of a user whose epoch
changed or who is gone, each audited as `session.revoke` by `system`. A
session is valid only while its epoch equals its user's. A failed apply
is logged once per change of error, shown by `cluster_users`, and counts
as no quorum while it lasts. A write waits up
to 10 s for the copy to reach its index, repeating with the same
operation ID while the outcome is unknown.

**Without quorum** (the socket unreachable or answering an error, no
answer for 15 s, no leader, stalled, joining or removed, or a copy that
cannot be applied): user writes answer `503 no_quorum`
with `details.outcome`; the password is checked first, then an enabled
administrator gets a degraded session of at most one hour and anyone else
`503 no_quorum`; an empty copy answers `no_quorum`, not `setup_required`.
A degraded session is refused on the three membership operations of the
API before anything is sent (`503 no_quorum`, `not_written`), even after
quorum is back; on user writes it is refused only while there is no
quorum (8c-2 review, F3).
The hash upgrade at login is a `user.rehash` with a 2 s limit, not sent
without quorum. `skep-api`'s health has the non-critical check
`cluster_users`. The writer treats the codes `no_leader`, `unavailable`,
`not_committed`, `removed`, `joining` and `forbidden` (the leader refusing
a member being removed) as transient: it repeats the write, then answers
`503 no_quorum`, never 403. A cluster at 2000 users answers `409
too_many_users`.

A node without quorum is not yet limited to reads and to stopping and
starting its own guests (ADR 0013 B9): that is built in step 8d with the
guest inventory.

**Import.** Migration `api/0006` marks a host that had users `pending`:
logins use its own rows, user writes answer `503 users_import_pending`.
`skep-api admin import-users` records an operation ID, sends one
`users.import` (at most 192 KiB), and repeats it while the outcome is
unknown. It records `done` in one statement with the answer's cluster ID
and a users index one below the import entry's, so that the copy's
same-cluster rule drops every `users-read` answer taken before the import
(one with no users would otherwise delete the host's users and sessions);
an answer without a cluster ID leaves the import pending. Only one of the
eight refusal codes drops the recorded operation ID; after any other
error it is kept for a rerun. A cluster that has users refuses the import
(`users_exist`), and with `--yes` the host takes the cluster's users
(`superseded`). `skep-cluster join` refuses a host whose import is
pending (it reads the state with `skep-api admin list --import-state`,
read-only, as the API account) and goes on, saying so, when the state
cannot be read.

**Leader barrier (D12).** A new leader makes the barrier of its term as
soon as the library reports the leadership, in its own goroutine that
waits on the library's leader channel, and records the term only if the
node is still the leader in that term; so it answers the peer operations
without waiting for the background step, which synchronous status calls
can hold up for seconds. The leader's work stays in the step, one step
per round; the step skips its own barrier once the term is recorded.

**Tests use a real `skep-cluster`**: package `clustertest` runs a one-node
cluster in the test's process, and the browser test's stack runs the
`skep_testcluster` build, whose `test-init` creates a cluster without
root (never in a shipped build).

## Limits of 8b-2a and 8b-2b

- Only a host **without VMs or jails** joins, and only a one-node host
  (`join-request` and `join` refuse otherwise). Since step 8c-1 the
  cluster's users replace the host's (the join's confirmation lists
  them); no drain is taken (8d). `remove` has no
  `--forget-guests` and no guest refusal (8d; the entry's guest list is
  always empty).
- A key rotation waits for every member: a member that stays down blocks
  it until it returns, is removed, or the rotation is aborted (only while
  the rotating node still uses its old key). One rotation in the cluster
  at a time, and none while an admission is pending or a member joins.
  `skep-api`'s node-to-node key (`--api`) arrives with 8e.
- The cluster audit can be rewritten from the start by a compromised
  `skep-cluster` on its own node, and a cut-off tail cannot be detected;
  the other members' audits are the cross-check (no external anchor in
  0.2).
- The clock check only reports; `node_clock_skew` arrives with 8e.
- With a member down, a join cannot finish (by design, amendment 1), and a
  removal that leaves too few answering voters is refused unless
  `-allow-unanswered`; such a removal may not finish until the member
  returns, because the configuration without the removed node needs it.
- `admit` and `remove` are root-only on a member; through the API from 8c.
- A clustered node's address cannot be changed; `listen` and `listen
  -none` refuse on a node that is not alone.
- The operation IDs applied so far are kept without a bound (step 8c).
- `skep-setup` does not know the service: after a package upgrade, restart
  it by hand (`service skep_cluster restart`).
- A member that was down while a node was removed still pins it until it
  has caught up (ADR 0013 amendment 1, point 6); health names such
  members, and the operator rule is to wipe them if the removed node was
  compromised.
- A snapshot written after a removal has format 3, which an 8b-1 build
  cannot read: in a rolling upgrade from 8b-1, upgrade every node before
  removing one (the new entry types stop an 8b-1 state machine anyway).
