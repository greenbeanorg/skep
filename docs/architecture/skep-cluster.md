# skep-cluster (Milestone 8, step 8a)

`skep-cluster` is the third Skep service, next to `skep-agent` and
`skep-api`. It holds the node's cluster key, the cluster membership and
the replicated store, and runs as its own unprivileged account. The design
is [ADR 0013](../decisions/0013-clustering.md); this page describes what is
built.

**Step 8a builds a one-node cluster on every host and nothing more.**
There is no join, no second node, no Raft traffic between nodes, and no
replicated users. `skep-api`, `skep-agent` and `skep-setup` do not use
`skep-cluster` yet and are unchanged; the public API is unchanged.

**Validated on FreeBSD** on the lab VM `smith` (15.1-RELEASE-p7, UFS
root) on 2026-11-24, build `67396a5`:
[record](../../tests/integration/cluster/results/8a-67396a5-20261124-smith.md).
Not validated: a boot with the service enabled, ZFS-root hosts, hardware.

## What exists after 8a

| Piece | Where | Notes |
|---|---|---|
| Service account | `skep-cluster`, UID and GID **410** | Created by the package's pre-install script, as the `skep` account (409) is. Not the `skep` account: `skep-api` must not be able to read the node key |
| Service | `/usr/local/sbin/skep-cluster`, rc.d `skep_cluster` | Refuses to run as root. Not enabled by the package or by `skep-setup` |
| Configuration | `/usr/local/etc/skep/cluster.json`, `root:skep-cluster`, `0640` | Written by the root-run commands; the service reads it and cannot change it |
| Data directory | `/var/db/skep/cluster`, `skep-cluster:skep-cluster`, `0700` | `node.key` (`0600`), `node.crt`, `raft.db` (`0600`), `snapshots/` |
| Local API | Unix socket `/var/run/skep-cluster/cluster.sock` | Peer credentials checked; see the table below |
| Node-to-node listener | TCP **8444** on the management address | Open only after `skep-cluster listen -address` (or `init -address`). A single host opens no port |

## The node's identity

- **Node ID.** One ID per node: the UUID `skep-agent` created in
  `agent.db`. `skep-cluster init`, run by root, reads it from the agent's
  read-only system operation (`GET /internal/v1/system` on the agent
  socket, as peer UID 0, actor `skep-cluster-init`) and writes it to
  `cluster.json`. The service never connects to the agent.
- **Node key.** ECDSA P-256, generated at `init` by the part of the
  command that runs as the service account. Other nodes will trust it by
  its **pin**, the SHA-256 of the public key (`sha256:<hex>`, the format
  `skepctl` uses for `skep-api`'s key).
- **Certificate.** Self-signed, valid 397 days, renewed with the same key
  30 days before it expires (checked at start and daily), so the pin does
  not change. A failed renewal is a health warning.

## Commands

```
skep-cluster init [-address ADDR] [-name NAME]   root; creates the one-node cluster
skep-cluster listen -address ADDR                root; opens 8444 on ADDR
skep-cluster status [-json]                      root or skep
skep-cluster health [-json]                      root or skep; exit 1 unless ok
skep-cluster version
```

**`init`** does its work in two parts. As root: it refuses if the host is
already initialised, reads the node ID from the agent, creates the data
directory for the service account when it is missing, and writes
`cluster.json`. Then it gives up root for the service account
(`internal/privdrop`) and, as that account, creates the key, the
certificate and the store with this node as its only member. Root creates
nothing inside the data directory, and `init` checks at the end that no
root-owned file is there.

Without `-address`, the member entry has no address and the service opens
**no network listener**.

**`listen -address ADDR`** is for the founding node of a cluster, before
its first `admit` (step 8b). It refuses unless the Raft configuration is
this node alone (asked from the running service over the socket), and
unless `ADDR` is one specific address of this host. It writes
`listen_address` to `cluster.json` and restarts `skep_cluster`
(`service skep_cluster restart`; `-no-restart` leaves that to the
administrator). At start the service then updates its own member entry
and its own server entry in the Raft configuration, and opens the
listener. In 8a `listen` changes `cluster.json` only; `skep-api`'s
node-to-node listener (`api.json`, port 8445) comes with step 8e.

## The local API and who may call it

The socket's directory belongs to the service account with the group
`skep`, mode `0750`; the socket has mode `0660` and, on FreeBSD, inherits
the directory's group. So the file permissions admit the service account,
the `skep` group and root. The service then reads each connection's peer
credentials from the kernel and **closes any connection whose UID is not
root or `skep`** before reading a byte. That includes the service account
itself and every other user.

Each operation names which of the two may call it. A caller that connects
but is not named gets `403 forbidden`.

| Operation | Request | root | skep |
|---|---|---|---|
| `health` | `GET /cluster/v1/health` | yes | yes |
| `status` | `GET /cluster/v1/status` | yes | yes |
| `raft-configuration` | `GET /cluster/v1/raft/configuration` | yes | no |

All three are reads. `status` returns the node ID and name, the cluster
ID, the node key's pin, the certificate's expiry, the listener's state,
the Raft state and the members: IDs, names, addresses and pins, all
public within a cluster. `raft-configuration` is what `listen` checks;
`skep-api` has no use for it. The API is internal, like the agent's, and
makes no compatibility promise.

## The node-to-node listener

Open only when `cluster.json` has a `listen_address`, on that address and
port 8444 only (never a wildcard address).

- **TLS 1.3 only.** A TLS 1.2 handshake is refused.
- **A client certificate is required, and accepted by its pin alone**:
  the key must be pinned in a member entry of the membership the node has
  applied at that moment. The check runs at every handshake; there are no
  session tickets. An expired certificate is refused even when its key is
  pinned. No CA is involved.
- **With one member, the only key that passes is the node's own, and
  nothing is served to it:** the connection is closed after the
  handshake. The inbound Raft RPC handler, the join operation and pending
  keys are step 8b.

## The store

- `hashicorp/raft` v1.8.0 with `raft-boltdb/v2` v2.4.2 (bbolt) for the
  log and stable store, and the library's file snapshots; pure Go,
  `CGO_ENABLED=0`, cross-compiled for FreeBSD like every binary. The
  versions are pinned in `go.mod`. Library default timeouts, pre-vote on.
- Only `internal/cluster/store` imports the Raft library.
- In 8a the store uses the library's in-memory transport, which reaches
  no other process: a single voter elects itself. The network transport
  with per-RPC identity is step 8b.
- **Every Skep log entry has an envelope** with a `type`, a `version`, an
  operation ID (`op_id`: applying the same ID twice is a no-op), the
  actor, the origin node and an optional request ID. 8a writes two types,
  both at version 1: `cluster.init` (the cluster ID and the first member)
  and `member.address`.
- **An entry this build cannot apply stops the state machine** (unknown
  type, a newer version, or data it cannot read). Nothing after it is
  applied, health reports it, and the listener accepts no key. It never
  skips.

## Health

`skep-cluster health` (and `GET /cluster/v1/health`) reports: `raft` (a
leader exists), `state_machine` (not stopped), `membership` (this node's
member entry exists and pins the key on disk), `address` (the member
entry and the Raft configuration match `cluster.json`), `listener` (open
when configured), `certificate` (renewal). The first five are critical.
`skep-api`'s health does not include it yet.

## Limits of 8a

- One node only. `join-request`, `admit`, `join`, removal, `recover`,
  backup and key rotation do not exist yet.
- `skep-setup` does not know the service: it does not enable, restart or
  remove it. After a package upgrade, restart it by hand
  (`service skep_cluster restart`).
- The operation IDs applied so far are kept in the replicated state
  without a bound; a bound comes before writes become frequent (step 8c).
- There is no cluster audit yet (step 8b). Refused socket callers and
  refused handshakes are logged by the service.
