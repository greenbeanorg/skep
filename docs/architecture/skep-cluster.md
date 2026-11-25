# skep-cluster (Milestone 8, steps 8a and 8b-1)

`skep-cluster` is the third Skep service, next to `skep-agent` and
`skep-api`. It holds the node's cluster key, the cluster membership and
the replicated store, and runs as its own unprivileged account. The design
is [ADR 0013](../decisions/0013-clustering.md) with its amendment 1; this
page describes what is built.

**Built so far:** every host is a one-node cluster (8a), and a host
**without guests** can join another cluster (8b-1): `join-request`,
`admit`, `admit --withdraw`, `join`, Raft over TLS 1.3 on port 8444 with
the sender checked on every RPC, pending entries with expiry and
withdrawal, the join's four steps and their crash points, promotion once
caught up and applied by every member, and the applied-membership file.
**Not built yet** (the second 8b run and later steps): removal and
`leave --wipe`, `join --abandon`, key rotation, clock-offset health, the
cluster audit (each membership event is a log line for now), joining a
host with guests (8d), replicated users (8c). `skep-api`, `skep-agent`
and `skep-setup` do not use `skep-cluster` yet and are unchanged; the
public API is unchanged.

**Validated on FreeBSD:** step 8a on `smith` (2026-11-24, build `67396a5`,
[record](../../tests/integration/cluster/results/8a-67396a5-20261124-smith.md)).
Step 8b-1 on `smith`, `langrishe` and `star` (2026-11-24, build `2de184b`,
[record](../../tests/integration/cluster/results/8b1-2de184b-20261124.md)):
three hosts joined into one cluster, the identity and refusal cases, a
full stop and an expiry. What the record lists as in-process only (crash
points, a member down during a join, unknown outcomes, the file's fault
cases) is tested on several nodes over real TLS on loopback, not on
FreeBSD.

## What exists

| Piece | Where | Notes |
|---|---|---|
| Service account | `skep-cluster`, UID and GID **410** | Created by the package's pre-install script, as the `skep` account (409) is. Not the `skep` account: `skep-api` must not be able to read the node key |
| Service | `/usr/local/sbin/skep-cluster`, rc.d `skep_cluster` | Refuses to run as root. Not enabled by the package or by `skep-setup` |
| Configuration | `/usr/local/etc/skep/cluster.json`, `root:skep-cluster`, `0640` | Written by the root-run commands (`init`, `listen`, `join-request`); the service reads it and cannot change it |
| Data directory | `/var/db/skep/cluster`, `skep-cluster:skep-cluster`, `0700` | `node.key` (`0600`), `node.crt`, `raft.db` (`0600`), `snapshots/`, `membership.json` (`0600`); while joining also `join.json` (`0600`) and `standalone.<UTC time>/` (`0700`). Nothing in it belongs to root |
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
skep-cluster status [-json]                      root or skep
skep-cluster health [-json]                      root or skep; exit 1 unless ok
skep-cluster version
```

The procedure, as the commands print it, is in
[Joining hosts into a cluster](../administration/cluster.md).

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
  of the run). The service passes them to the leader, see below.
- **`join ADMISSION`**, as root: decodes the admission and refuses one for
  another host (ID, name, address); refuses a host with guests or without
  an answering agent; prints the cluster ID and every member (name, ID,
  address, pin) and asks for `yes` on `/dev/tty`, or, with no terminal,
  needs `-cluster-id` equal to the admission's (there is no `-yes`); stops
  `skep_cluster`; starts the stage, which **makes the join call**:
  1. it checks that the admission names this key, and reads "is this node
     alone" from the store;
  2. it moves `raft.db`, `snapshots/` and `membership.json` into
     `standalone.<UTC time>/` and writes `join.json` (phase `calling`, a
     new operation ID, the admission);
  3. it calls the join operation on the admission's members (below);
  4. **only when it knows join step 1 was not applied** (every handshake
     failed and nothing was sent, or a leader refused, or no member had a
     leader) it puts the one-node state back and removes `join.json`;
     after an accepted call it records the answer (phase `accepted`), and
     after an unknown outcome (sent, no answer) phase `unknown`.

  Root then starts `skep_cluster` in every case and runs the root-file
  check. **What root believes from the stage** is only its outcome, for
  what it prints; no root effect depends on it (the service is started
  whatever the outcome). After `accepted` or `unknown` the command says
  the join is past its point of no return and that this build has no
  command to give up a join, then waits (default 90 s) for the node to
  apply its own member entry.

### The join call and what a joining node accepts

- The joining node dials **only the members of its admission**, and the
  members of the join call's answer once it has one. A redirect to the
  leader (`not_leader`) is followed only when it names a node of that list
  at the address the list gives (D8 of the run).
- A refusal from a leader, a handshake failure (nothing sent) and "no
  leader" are each certain to have applied nothing. A request sent without
  an answer, or a leader's `outcome_unknown`, makes the outcome unknown,
  and a later refusal in the same command does not undo that.
- The service starts on an empty store when `join.json` exists. Until its
  accepted membership lists this node it accepts **only the admission's
  and the answer's keys** (as members) and applies no Raft-configuration
  rule. Phase `unknown` (or `calling`, found at start) makes it repeat the
  call with the same operation ID every 10 s; a repeat after step 1 is
  answered as accepted. A refusal after an unknown outcome is phase
  `refused`: health is critical, and the one-node state stays aside.
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

- Request: node ID, name, address (`ip:8444`), node-key pin.
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
| `admit` | `POST /cluster/v1/admit` | yes | no |
| `admit-withdraw` | `POST /cluster/v1/admit/withdraw` | yes | no |

`admit` and `admit-withdraw` are root-only in this step (`skep-api` gets
them with `cluster:manage` in 8c). Their errors: `400 invalid_request`,
`409 no_listen_address`, `409 joining`, `409 refused` (the leader or the
state machine refused it, with the reason), `404 not_found` (withdraw),
`503 not_committed` / `no_leader` / `outcome_unknown`. A member that is
not the leader passes the operation to the leader as a typed Skep RPC;
the leader checks it again and the state machine checks it again at apply
on every node. The actor recorded is `root` (the verified peer); the
entry's origin is the node where the command ran.

`status` returns the node and cluster IDs, the pin, the listener, the Raft
state, the state machine's index (below), the members with their Raft role,
the pending admissions, where the accepted keys come from (`state
machine`, `membership file`, `admission`) and, while joining, the join's
phase.

## The node-to-node listener and transport

- **TLS 1.3 only**; a client certificate is required and accepted by its
  pin alone: pinned in a member entry (`joining` or `member`) or in a
  pending entry of the membership the node has applied. A pending key
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
  and every second (for clock-based expiry), the listener closes every
  open connection whose key is no longer accepted.

| Operation on 8444 | pending key | joining key | member key | Also |
|---|---|---|---|---|
| `raft` (AppendEntries with its heartbeat fast path, RequestVote, RequestPreVote, InstallSnapshot, TimeoutNow) | no | yes | yes | a server in the latest Raft configuration |
| `join` | yes | yes | no | |
| `status` | no | yes | yes | |
| `admit` (passed to the leader) | no | no | yes | |
| `withdraw` (passed to the leader) | no | no | yes | |

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
| `pending.create` | node ID, name, address, pin, `expires_at` (leader's clock + 30 min) | creates a pending entry |
| `pending.withdraw` | node ID, name | removes it |
| `pending.expire` | node ID, `expires_at`, `at` (leader's clock) | removes it when `at` ≥ `expires_at` |
| `member.join` (join step 1) | node ID, pin, `at` (leader's clock) | consumes the pending entry, creates the member entry in state `joining` with the entry's index (`joined_index`) and op ID |
| `member.promoted` (join step 4) | node ID | `joining` → `member` |

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
  entry, a second `cluster.init`, or a cluster ID that is not a UUID.
- Each membership event is **one log line** on every node that applies it
  (`cluster membership: pending created | pending used | pending expired |
  pending withdrawn | member joining | member`) with actor, operation ID
  and origin; the leader logs `non-voter added` and `promoted` (actor
  `system`, the join's op ID). The cluster audit (B10) is the next run.
- **Step 8d** adds the joining host's guest list to `member.join` as its
  version 2 (ID, name, kind per guest), so that the name check and the
  inventory write stay in join step 1 (ADR 0013 B3); version 1 means a host
  without guests, which is all this build lets join.

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
- **The leader's work**, when it gains leadership (after a barrier) and
  every 10 s: propose the expiry of every pending entry its clock has
  passed, and for every member in state `joining`: not a Raft server → join
  step 2 (`AddNonvoter`); a non-voter → step 3 when the joiner's applied
  index is within one snapshot threshold (8192) of the leader's commit
  index **and** every member in the Skep membership (the joiner and the
  leader included) answers the status RPC with an applied index at or past
  the member's `joined_index` and a running machine; a voter → step 4.
  Every step can be repeated: a new leader finishes what the last one left.
- Every node (not only the leader) asks every member the same way every
  10 s while a join is in progress, so **health on every member, the joiner
  included, names the member being waited for** (check `join`, a warning).
- No case was found where the machine's own index makes catch-up fail: the
  gap between it and the commit index is the library's own entries after
  the last Skep entry, a handful against a threshold of 8192.

## Health

`skep-cluster health` (and `GET /cluster/v1/health`) reports: `raft` (a
leader exists), `state_machine` (not stopped), `membership` (this node's
member entry exists and pins the key on disk; not checked while joining),
`address` (the member entry and the Raft configuration match
`cluster.json`; a clustered node's address is not changed this way),
`listener` (open when configured), `certificate` (renewal, warning),
`join` (a join in progress and whom its promotion waits for, warning),
`joining` (this node's own join: critical when it was refused after an
unknown outcome). `skep-api`'s health does not include it yet.

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

## Comparing the adapted handler with upstream

`internal/cluster/raftnet/net_handler.go` is adapted from the library's
`net_transport.go` (`handleConn`, `handleCommand`) and stays under
MPL-2.0 (its header says so). **At every update of `hashicorp/raft`**,
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

## Limits of 8b-1

- Only a host **without VMs or jails** joins, and only a one-node host
  (`join-request` and `join` refuse otherwise). The host's users are not
  replaced (8c) and no drain is taken (8d).
- **No removal, `leave --wipe`, `join --abandon` or key rotation.** A join
  past its point of no return cannot be given up with this build; a member
  cannot leave. Test clusters are reset by hand (stop the service, delete
  the data directory and `cluster.json`, `init` again).
- With a member down, a join cannot finish (by design, amendment 1); the
  joiner stays a working non-voter and health names the member.
- `admit` is root-only on a member; through the API from 8c.
- No clock-offset health and no cluster audit yet (each membership event
  is a log line).
- A clustered node's address cannot be changed; `listen` refuses on a node
  that is not alone, and no command closes the listener again (backlog).
- The operation IDs applied so far are kept without a bound (step 8c).
- `skep-setup` does not know the service: after a package upgrade, restart
  it by hand (`service skep_cluster restart`).
