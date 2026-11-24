# Raft prototype measurements for ADR 0013 (2026-11-24)

Evidence for [ADR 0013](../0013-clustering.md). A throwaway prototype, not
product code. Every number below was measured in this run; nothing is
estimated. Raw output is in [raft-20261124/](raft-20261124/).

## What ran, where

| | |
|---|---|
| Date | 2026-11-24, 18:05–18:31 UTC |
| Prototype | `raftprobe`, branch `probe/m8-raft`, commit `b76e2a9`, directory `probes/m8-raft/` with its own `go.mod` (the main module is unchanged) |
| Library | `github.com/hashicorp/raft` v1.8.0 |
| Log and stable store | `github.com/hashicorp/raft-boltdb/v2` v2.4.2 over `go.etcd.io/bbolt` v1.4.1, fsync on (the default) |
| Snapshot store | `raft.FileSnapshotStore` (library), 2 retained |
| State machine | a string map; snapshots are its JSON |
| Build | merrick (Debian 13), go1.27.4, `CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -trimpath`; build info records `CGO_ENABLED=0` |
| Nodes | smith 10.79.20.81 (UFS root), langrishe 10.79.20.82 (ZFS root), star 10.79.20.83 (ZFS root, on another Proxmox host); all FreeBSD 15.1-RELEASE-p7 amd64, 2 vCPU, 4 GiB |
| Process | as `aba`, never root, from `/var/tmp/raftprobe`, under `timeout 14400`, listening on `<10.79.20.x>:17400` only |
| Driver | `raftprobe drive` / `ctl` on merrick 10.79.20.80; every interval below is measured on merrick's clock unless it says otherwise |
| Round trip | merrick–star 0.38/0.59/0.76 ms (min/avg/max, 20 pings); smith–langrishe 0.09/0.27/0.80 ms; smith–star 0.38/0.65/0.98 ms |
| Raft settings | library defaults unless stated: HeartbeatTimeout 1 s, ElectionTimeout 1 s, LeaderLeaseTimeout 500 ms, CommitTimeout 50 ms; pre-vote on |

smith and langrishe kept running Skep 0.1.0 (c8ce17b) throughout; their
`skep_agent` and `skep_api` PIDs were the same at the start and the end and
`/api/v1/health` answered 200 both times. Nothing was written under
`/usr/local` or `/var/db/skep`. `hw.vmm.vmx.cap.virtual_interrupt_delivery`
read 0 on all three VMs at both ends.

## How the prototype is built

- **Transport.** Mutual TLS 1.3 with a throwaway ECDSA P-256 CA generated
  on merrick in a scratch directory outside the repository (deleted after
  the run). Each node certificate names exactly one node (one DNS SAN).
  Outbound: `hashicorp/raft`'s `NetworkTransport` over a TLS stream layer
  that sets `ServerName` to the node being dialled, so the server's
  certificate must name that node; dials are bound to the node's own
  address. Inbound: served by the probe, not by `NetworkTransport`, because
  `NetworkTransport` hands decoded RPCs to Raft without the connection they
  came on, so there is no place to check who sent them. The probe's handler
  (adapted from `net_transport.go`, MPL-2.0) refuses a connection whose
  certificate does not name a configured node or whose source address is
  not that node's, and refuses any RPC whose header (`RPCHeader.ID` and
  `Addr`) is not the certificate's node.
- **Control.** The driver uses the same port with a separate client
  certificate (`ctl`); those connections are never filtered.
- **Partitions are simulated, not real.** `block` makes one node hold the
  bytes it sends to (`out`) and/or receives from (`in`) a named peer, in a
  filter below TLS. Held bytes flow again on `heal` unless the connection
  timed out first (the transport's timeout is 10 s), roughly like TCP over
  a link that loses everything for a while. The kernel still completes TCP
  handshakes, and nothing is dropped at the IP layer. A real cut (pf, or a
  link down) was not used: both are excluded on these VMs.
- **Reads.** `stale` reads the local map. `linear` calls `VerifyLeader()`
  first (a heartbeat round to a majority), then reads.

## Results table (three runs each)

| # | Measurement | Run 1 | Run 2 | Run 3 |
|---|---|---|---|---|
| 1 | Binary size (FreeBSD amd64, not stripped) | 12,371,509 B | (same build) | (same build) |
| 1 | Idle RSS, KiB, smith / langrishe / star | 14,044 / 14,132 / 14,744 | 15,500 / 11,164 / 15,392 | 14,960 / 13,932 / 14,024 |
| 2 | Bootstrap of three nodes: first leader seen / first write committed, ms | 1,183 / 1,189 | 1,087 / 1,098 | 1,046 / 1,052 |
| 3 | 1000 sequential writes, leader smith: median / p99 / worst, µs | 5,558 / 9,585 / 21,045 | 7,019 / 9,694 / 14,652 | 6,808 / 9,776 / 13,542 |
| 3 | Same, leader star (other Proxmox host) | 8,228 / 14,775 / 22,203 | 6,076 / 9,520 / 16,908 | 5,770 / 9,321 / 16,364 |
| 4 | Leader SIGKILL: new leader seen / write accepted, ms | 2,445 / 2,450 | 1,719 / 1,726 | 1,662 / 1,666 |
| 5 | Leader SIGSTOP 30 s: new leader's write accepted, ms | 1,990 | 2,277 | 2,507 |
| 5 | Old leader after SIGCONT: steps down | ≤ 1 ms after it resumed | already follower at first sample | ≤ 1 ms |
| 6 | One follower cut 20 s, pre-vote on: term change / leader disturbed | 0 / no | 0 / no | 0 / no |
| 6 | Same, pre-vote off: term rise / leader deposed / time without leader | +13 / yes / 2.3 s | +13 / yes / 1.2 s | +12 / yes / 2.0 s |
| 7 | 2/1 partition, leader alone: it steps down after, ms | 491 | 507 | 446 |
| 7 | Write sent to it inside that window: reply / later committed? | error after 507 ms / **yes** | error after 471 ms / **yes** | error after 469 ms / **yes** |
| 8 | All three cut off, or two nodes down: survivor commits a write | no | no | no |
| 9 | One-way cut leader↔follower (both directions tried): leader disturbed | no | no | no |
| 10 | Snapshot time / InstallSnapshot size / returning node caught up, ms | 26 ms / 150,463 B / 9,026 | 10 ms / 193,353 B / 1,185 | 22 ms / 236,243 B / 8,317 |
| 11 | Remove / add non-voter / promote, ms (each) | 7.4 / 8.2 / 7.2 | 7.7 / 7.5 / 7.5 | 7.2 / 7.2 / 7.0 |
| 12 | All three stopped and started: write accepted after start, ms, SIGTERM | 1,443 | 1,469 | 1,714 |
| 12 | Same after SIGKILL | 1,644 | 1,217 | 1,103 |
| 13 | Two voters, one down: survivor commits a write | no | no | no |
| 13 | Three voters incl. witness star, leader SIGKILL: write accepted, ms | 2,015 | 2,047 | 1,838 (4th run: 2,494) |

## Details per measurement

### 1. Size, memory, dependencies

The binary is 12,371,509 bytes (not stripped). Idle RSS was read with
`ps -o rss` three times: 76 s after a fresh bootstrap, after all scenarios
(about 12,000 keys had been written by then; the map then held 21 keys
after a wipe), and 60 s after fresh processes. Range 11,164–15,500 KiB.

Modules linked into the FreeBSD binary (`go list -deps`), licence read
from each module's LICENSE file in the module cache:

| Module | Version | Licence (from LICENSE) |
|---|---|---|
| github.com/hashicorp/raft | v1.8.0 | MPL-2.0 |
| github.com/hashicorp/raft-boltdb/v2 | v2.4.2 | MPL-2.0 |
| go.etcd.io/bbolt | v1.4.1 | MIT |
| github.com/boltdb/bolt | v1.3.1 | MIT (archived upstream; linked by raft-boltdb/v2 for its v1-to-v2 migration helper) |
| github.com/hashicorp/go-msgpack/v2 | v2.1.5 | MIT |
| github.com/hashicorp/go-hclog | v1.6.3 | MIT |
| github.com/hashicorp/go-metrics | v0.7.0 | MIT |
| github.com/hashicorp/go-immutable-radix | v1.3.1 | MPL-2.0 |
| github.com/hashicorp/golang-lru | v1.0.2 | MPL-2.0 |
| github.com/fatih/color | v1.19.0 | MIT |
| github.com/mattn/go-colorable | v0.1.15 | MIT |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| golang.org/x/sys | v0.48.0 | BSD-3-Clause |

All pure Go: the build ran with `CGO_ENABLED=0` and succeeded.

### 2. Bootstrap

Nodes started with `-hold`; the driver opened its control connections,
then sent `go` to all three at once and polled until a node reported
`Leader` and accepted a write. Leader terms were 2 each time. Most of the
~1.1 s is the 1 s election timeout.

### 3. Commit latency

`Apply().Error()` timed inside the leader process, 1000 writes one after
another, no batching. With round trips of 0.3–0.65 ms the median of
5.6–8.2 ms is dominated by the disk sync of the bbolt store on the leader
and a follower (an inference from the round trip, not separately
measured). Star as leader made no consistent difference.

### 4. Leader killed

The driver sent the leader a control request that makes it `SIGKILL`
itself, then polled the two others every 5 ms until one led and committed
a write. Restarted nodes rejoined and caught up each time (digests
compared).

### 5. Leader frozen

The leader `SIGSTOP`ped itself; the driver measured the new leader as in
4, and 500 ms before the 30 s were up opened four control requests to the
frozen node (status, a write, a stale read, a linear read), which the
kernel queued. It then sent `SIGCONT` over ssh (134–214 ms per ssh call).

- While frozen the old leader did nothing at all (it was stopped).
- After waking, its own records (its clock): the pause was detected at
  30.14–30.21 s, and it was a follower in the same millisecond, still at
  its old term. It stepped down on its own lease check, not because of a
  message from the new leader; the new term reached it 5–11 ms later.
- All four queued requests were answered after the step-down: the write
  and the linear read were refused with "node is not the leader"; the
  stale read answered from old data (run 1: the key, written by the new
  leader during the freeze, was "not found").
- It never answered anything as leader after waking, in three runs. The
  ordering between the lease check and request handling is the
  scheduler's; safety does not depend on it (a write cannot commit
  without a majority, and a linear read needs one), only the staleness of
  a stale read does.

### 6. One follower cut off and rejoined

The follower held both directions to both peers for 20 s, then healed.

- **Pre-vote on:** its term did not move, it sat as a (pre-vote)
  candidate, the leader and the term stayed the same, and it was caught up
  within 26–34 ms of the heal (first sample).
- **Pre-vote off** (all three restarted with it off): its term rose by
  12–13 in 20 s; on heal its higher term deposed the healthy leader each
  time and the cluster had no leader for 1.2–2.3 s before the same leader
  was elected again (the cut-off node's log was behind).

### 7. Leader alone on the minority side

- The isolated leader stepped down after 446–507 ms (LeaderLeaseTimeout
  500 ms). Afterwards it refused writes and linear reads at once ("node is
  not the leader"); stale reads returned its old data and its
  `last_contact` time, which a caller can use to mark the data as possibly
  stale.
- The majority elected a leader (term +1), committed a write (5–11 ms) and
  served a linear read of it.
- After the heal the minority node rejoined without raising the term
  (pre-vote) and had the majority's data; nothing it accepted was kept.
- **Write inside the lease window.** In three more runs a write was sent to
  the leader right after the cut. Each time the reply was an error,
  "leadership lost while committing log", after 469–507 ms, **and each
  time the key was present on all three nodes after the heal**. The heal
  came before the followers' election timeout, so the held entry reached
  them and the next leader committed it. A write that returns an error is
  therefore of unknown outcome, not "not done": Skep's replicated writes
  need idempotency keys and the client must be told the outcome is
  unknown.

### 8. All isolated; two nodes down

With every node cut off from the other two, all three became candidates at
the same term, refused writes and linear reads, and served stale reads
with their `last_contact`. With two nodes stopped (SIGTERM), the survivor
did the same. Its term did not rise during 15 s (pre-vote). After the
heal, or the restart of the others, a leader was elected and all nodes
converged, three runs each.

### 9. One direction only

`out` (the leader's bytes to the follower held) and `in` (the follower's
bytes to the leader held), three runs each, 15 s. In both cases the
follower stopped applying and became a (pre-vote) candidate after
0.14–1.5 s; the leader, its term and its commits were not disturbed; the
follower caught up on heal. Raft RPCs are request–response, so with either
direction gone the leader's next heartbeat waits for the lost reply (up to
the 10 s transport timeout): to the follower, a one-way cut looks like a
full cut.

### 10. Snapshot, truncation, restore

Nodes ran with `TrailingLogs` 100 and automatic snapshots effectively off.
A follower was stopped, 2000 writes were made, the leader took a
snapshot, and its log was truncated to the last 100 entries (for example
`[1, 8084]` to `[7985, 8084]`). On restart the follower was sent the
snapshot (`Installed remote snapshot` in its log) and its map digest
matched the leader's. The 9.0 s and 8.3 s catch-up times are the leader's
replication back-off towards a peer that had been down: its log shows
retries to langrishe 5 s and then 10 s apart before the snapshot was sent.
The leader was then restarted, restored from its own snapshot plus the
trailing log, and matched the others.

### 11. Membership

Remove a follower, write with two voters (committed), restart the removed
node, add it as a non-voter, then promote it. Each change committed in
about 7–8 ms (control round trip included), and the digests matched.
**The removed node never applied its own removal**: the leader stops
replicating to a node once its removal commits, so that node kept running
as a follower at the removal index minus one. Its configuration (entries
take effect when appended) already lacked itself, and it did not start
elections. A product must stop the removed node's service and revoke its
identity explicitly; Raft does not tell it.

### 12. Full restart

All three stopped (SIGTERM, then separately SIGKILL), restarted with
`-hold` and released together. Each time a leader was elected and a write
committed 1.1–1.7 s after release, with the data unchanged (digest) and no
operator action.

### 13. Two voters, then a witness

- smith and langrishe bootstrapped as the only voters. In three runs one
  was killed (twice the leader, once the follower): the survivor refused
  writes ("node is not the leader") and linear reads, served stale reads,
  and stayed a candidate at the same term. When the other came back, a
  leader was elected and the data matched.
- star was then added as a third voter (7.6 ms). With smith or langrishe
  down, the other plus star commit writes.
- **The witness can become leader.** star won the election in two of four
  leader kills, and led while langrishe was down. `hashicorp/raft` has no
  voter that is ineligible to lead. A witness therefore runs the full
  service and holds the full replicated state.

## Transport identity tests

All against smith while the cluster ran; refusals recorded by smith's (or
the leader's) transport.

| Test | Result |
|---|---|
| Client certificate from another CA (rogue CA, name "langrishe"), presented | Refused in the handshake: `x509: certificate signed by unknown authority` |
| Valid certificate for star, from merrick, RPC claiming langrishe | Refused: certificate names "star" but the connection comes from 10.79.20.80 |
| Valid certificate for a non-member ("mallory") | Refused: not a cluster node |
| On star, star's own address and certificate, RPC header claiming langrishe | Refused at the RPC: header claims "langrishe", certificate names "star" |
| Positive control: on star, header claiming star, stale term 1 | Answered (`granted=false term=14`), so the refusals above are the checks, not a broken path |
| Outbound: langrishe's node stopped, an impostor at 10.79.20.82:17400 presenting star's valid certificate | The leader refused every dial: `certificate is valid for star, not langrishe` |
| Outbound: impostor presenting a rogue-CA certificate named "langrishe" | Refused: `certificate signed by unknown authority` |

The first other-CA attempt sent no certificate at all (Go's client only
offers a certificate whose issuer the server lists) and was refused as
"didn't provide a certificate"; the test was repeated with the certificate
forced, as in the table. smith's term did not change during these tests.

## Limits of this evidence

- Simulated partitions (above). No pf rule, no link down.
- One small state machine; no load besides the probe; no guest running
  on the VMs.
- Library default timeouts only; faster settings were not measured.
- `go.etcd.io/raft` was not prototyped (the run's rules: one library; a
  second only if the first failed a requirement, and it did not).
- A rerun script killed star instead of smith in one witness run (`drive
  fail` always kills the leader); this is recorded in the m13 log and the
  run was repeated.
- Idle RSS readings in the m2 log's first block are empty (a script error);
  the values above come from the three readings recorded after it.
