# ADR 0013: Clustering — native Go Raft or Corosync (Milestone 8 evaluation)

- Status: **Proposed** (draft, 2026-11-23). Written in an unattended run
  while Andy was away. **No decision is made here.** It gathers what a Skep
  cluster must do, what each option offers on FreeBSD 15.1, how each fits the
  privilege boundary, the failure cases, and what must be measured in the lab
  before a decision. Not to be merged while Proposed.
- Date: 2026-11-23
- Depends on: [ADR 0004](0004-privilege-boundary.md) (privilege boundary),
  [ADR 0005](0005-agent-task-engine.md) (task engine),
  [ADR 0006](0006-api-split-authentication-tls.md) (API split, TLS),
  [ADR 0012](0012-installer.md) (fixed UID 409).
- Evidence: package queries on langrishe (FreeBSD 15.1-RELEASE-p7, amd64,
  `quarterly` repository, 2026-11-23 UTC), logs in
  [0013-evidence/](0013-evidence/). Nothing was installed.

## How to read this

Each fact carries its source: **[pkg]** the FreeBSD package repository as
langrishe saw it (evidence logs); **[repo]** this repository's ADRs and
code; **[base]** FreeBSD 15.1 base system as seen on the lab VMs;
**[upstream, unverified]** what the project documents say as far as known
when this was written, not checked in this run (no web access was used).
Unverified items are listed again under "What must be lab-tested".

## 1. What a Skep cluster must do for 0.2

From CLAUDE.md and the ADRs **[repo]**:

1. **Membership.** Nodes join and leave explicitly; a node that cannot be
   reached is *unreachable*, never *dead* (CLAUDE.md: "Never treat an
   unreachable node as dead").
2. **Quorum.** A majority decides. The initial HA target is three nodes.
   Two-node clusters need a rule (section 4).
3. **Replicated configuration and desired state.** VM and jail
   definitions, storage and network allow-lists that are cluster-wide,
   users, roles, and sessions (ADR 0006: "sessions and users are per node.
   A cluster needs shared identity"), and the cluster's own membership.
   ADR 0005 keeps task persistence behind a store so the cluster layer can
   replace it.
4. **Replicated task state.** Which node owns a task, its status
   (`pending` … `interrupted`), so that the UI on any node shows it and a
   task is never run twice.
5. **Ownership.** Exactly one node owns a running guest. Ownership changes
   only with quorum.
6. **What Milestone 9 (HA) will need from it, not built in 0.2:** fencing
   (proof that a lost node no longer runs a guest before its guest starts
   elsewhere), a lease or epoch that a partitioned node can check locally
   to stop acting, and watchdog integration. CLAUDE.md: "Never auto-restart a
   VM elsewhere without fencing and ownership validation."
7. **Security.** Mutual TLS between nodes with peer identity checked (ADR
   0004, "Clusters"); signed short-lived tokens before any remote caller
   reaches an agent (ADR 0004); no plaintext secrets; the agent gets no
   network listener until that exists.

Not required for 0.2: live migration, shared storage, automatic recovery.

## 2. What exists on FreeBSD 15.1 for each option

### Option A: Corosync (with or without Pacemaker)

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

What Corosync provides **[upstream, unverified]**: membership and messaging
(totem, over the kronosnet transport in 3.x), a quorum service
(`votequorum`, with `two_node`, `wait_for_all`, `last_man_standing`), a
small replicated key-value store (`cmap`), and a C API (`cpg`, `quorum`,
`cmap`) through libqb IPC. It does **not** replicate application state:
Skep's configuration and task state would still need a replicated store on
top (CPG-ordered messages into each node's SQLite, or another store).
Pacemaker adds a resource manager and fencing (STONITH) framework that
would overlap with Skep's own controller.

Go integration **[repo, upstream unverified]**: Corosync's APIs are C
libraries; using them from Go needs cgo or a helper process. CLAUDE.md
asks for pure-Go dependencies and a justification for any cgo, so that
cross-compilation from Linux keeps working.

### Option B: native Go Raft inside Skep

Nothing to install from packages: a Raft library is a Go module compiled
into a Skep binary. Candidate libraries **[upstream, unverified]**:
`github.com/hashicorp/raft` (MPL-2.0; used by Consul and Nomad) and
`go.etcd.io/raft/v3` (Apache-2.0; used by etcd; leaves transport and
storage to the caller). Both are pure Go as far as known, which would keep
`GOOS=freebsd` cross-compilation. Either is a new dependency and needs the
stated reason CLAUDE.md asks for.

FreeBSD packages of Raft-based systems exist **[pkg]**: `coreos-etcd36`
3.6.14_3 (APACHE20), `coreos-etcd35`, `coreos-etcd34`, `consul` 2.0.4_3
(MPL20), `nomad` 1.9.6_15, `rqlite` 10.3.6_3 (MIT, Raft over SQLite). They
show the Go Raft implementations build and are packaged on FreeBSD 15; they
are not proposed as dependencies here (an external store is a third
option, C, listed for completeness).

Raft provides **[upstream, unverified]**: leader election, a replicated log
applied in order on every node, membership changes through the log, and
majority quorum. Skep's configuration and task state would be the state
machine; snapshots would cover log compaction. Raft does not provide
fencing; leases would be built on top (for example, a leader lease bounded
by election timeouts and clock-drift assumptions).

### Option C (for completeness): an external Raft store

etcd or rqlite as a separate service, Skep as its client. Fewer lines in
Skep, but another daemon to install, secure (its own TLS and auth), upgrade,
and back up; and its failure is Skep's. Listed because it is packaged
**[pkg]**; not evaluated further unless Andy wants it.

### FreeBSD base facilities relevant to either **[base]**

- A hardware watchdog interface exists: `/dev/fido` and `watchdogd(8)` are
  present on langrishe. Whether the Proxmox VMs (and real hardware Skep
  targets) expose a watchdog device that actually resets the machine is
  untested.
- CARP is in base (`ucarp` is also packaged **[pkg]**); a floating
  management address is a possible M9 helper, not a cluster store.

## 3. Fit with the privilege boundary (ADR 0004, ADR 0006)

ADR 0004 **[repo]**: `skep-api` runs unprivileged and talks to the root
agent over a Unix socket with peer-credential checks; the agent has no
network listener until mutual TLS with peer identity exists; multi-node is
the trigger for signed per-request tokens.

| Question | Corosync | Go Raft in Skep |
|---|---|---|
| Which process joins the cluster? | `corosync` runs as its own daemon (as root in the usual setup **[upstream, unverified]**); Skep talks to it over libqb IPC. A fourth Skep-relevant daemon on every node | A Skep process. Natural home: a new unprivileged cluster process, or `skep-api` (unprivileged, already networked). Not the root agent, which ADR 0004 keeps off the network |
| Node-to-node transport security | kronosnet has its own crypto (nss) and keys **[upstream, unverified]**; separate from Skep's TLS and identity | Skep's own mutual TLS with node certificates (the cluster CA ADR 0006 deferred); one identity system |
| Does replicated state reach the root agent? | Through whatever Skep builds on top of CPG/cmap | The agent keeps acting only on requests over its socket; a replicated desired state is turned into agent requests by an unprivileged controller, so the boundary stays where ADR 0004 put it |
| Privileged code added | `corosync` as root (and Pacemaker, if used: as root, with Python agents) | None required in the agent for 0.2; fencing (M9) would add privileged operations either way |
| Audit | Corosync logs are separate from Skep's audit | Log entries can carry Skep's actor and request IDs |

## 4. Failure cases

Assumptions: each node runs guests locally; no shared storage in 0.2.

### Three nodes (the target)

| Case | Corosync (votequorum) **[upstream, unverified]** | Raft |
|---|---|---|
| One node down or cut off | Two nodes keep quorum; the lone node loses it | Two nodes keep a leader; the lone node cannot commit |
| Partition 2 / 1 | The pair continues; the single node is inquorate | The same |
| Leader / representative lost | Membership recomputed by totem | New election after the election timeout |
| All three partitioned from each other | Nobody is quorate | Nobody can commit |
| Two nodes down | Survivor inquorate (unless `last_man_standing` was configured and the losses were sequential) | Survivor cannot commit; manual recovery needed |

In every case the minority side must stop *changing* cluster state but must
**not** stop or kill its running guests on its own (CLAUDE.md:
unreachable is not dead). What it may still do locally (stop a guest on an
operator's request?) is an open question (section 6).

### Two nodes

Neither option has a safe automatic answer with two votes. Corosync's
`two_node: 1` lets the survivor stay quorate, which relies on fencing to
avoid split brain **[upstream, unverified]**; the usual third vote is
`corosync-qnetd`, **not packaged on FreeBSD [pkg]**. Raft with two voters
needs both for every commit: either node down stops all changes. For both:
a two-node Skep cluster needs a third voter (a small witness node or a
tie-breaker service) or must accept "no changes while one node is down".

## 5. What must be lab-tested before a decision

On FreeBSD 15.1 lab VMs (three are needed; smith and langrishe are two),
installing only with Andy's approval:

1. **Corosync 3 on FreeBSD:** `pkg install corosync3`, a three-node ring
   over kronosnet, `corosync-quorumtool` through each failure case in
   section 4 (pf or `ifconfig down` on a test interface, never vtnet0), knet
   crypto on, time to detect a lost node, and whether it runs as non-root.
2. **Corosync from Go:** whether a pure-Go client of libqb IPC is feasible,
   or cgo/a helper is unavoidable (cross-compilation cost measured).
3. **Go Raft prototype (not product code):** a throwaway three-node program
   with `hashicorp/raft` and with `etcd-io/raft` over mutual TLS: election
   time, commit latency, behaviour in each section 4 case, snapshot and
   restore, membership change, and binary size; cross-compiled from merrick.
4. **Watchdog:** whether `watchdogd` with `/dev/fido` resets a Proxmox VM
   (and which watchdog device the VM needs), as groundwork for M9 fencing.
5. **Clock behaviour:** drift between lab VMs over a day, since lease-based
   fencing in M9 depends on it.
6. **Licences:** confirm each candidate's licence from upstream (port
   metadata lists kronosnet and pacemaker2 as GPLv2; Skep's own licence
   compatibility to be checked).
7. **Upstream facts marked unverified** above, checked against current
   upstream documentation.

## 6. Open questions for Andy

1. Is Pacemaker in scope at all, or would Skep's own controller own
   resource management? (Pacemaker on FreeBSD pulls Python, bash, and
   glib, and overlaps the controller.)
2. Is cgo acceptable for a cluster component if Corosync wins, or is
   pure Go a hard requirement?
3. Two-node clusters: support them with a witness (which?), support them
   with "no changes while one is down", or not at all in 0.2?
4. Which process holds cluster membership: a new unprivileged
   `skep-cluster`, or `skep-api`?
5. What may an inquorate node still do locally on an operator's request
   (stop a guest, read state)?
6. Is an external store (option C, etcd or rqlite) worth a lab test, or
   ruled out because it adds a daemon to secure and upgrade?
7. Approval for the section 5 lab tests: packages on the lab VMs, a third
   lab VM, and test-interface partitions.

## 7. What the evidence supports so far

Only this, and no more: both a Corosync stack and Go Raft implementations
are available and packaged for FreeBSD 15.1 amd64 **[pkg]**; the Corosync
two-node quorum device is not **[pkg]**; Corosync needs C libraries and a
separate root daemon, while a Go Raft library would live inside a Skep
process and could use Skep's own TLS identity **[repo, upstream
unverified]**. Neither option has been run on FreeBSD by this project.
A recommendation waits for section 5.
