# Joining hosts into a cluster, and removing them

Every Skep host is a one-node cluster (`skep-cluster`, see
[the architecture page](../architecture/skep-cluster.md)). This page joins
a host to another host's cluster, removes it again, and gives up a join,
with the commands of Milestone 8, steps 8b-1 and 8b-2a, and through the
API and `skepctl` from step 8c-2 ([below](#through-the-api-and-skepctl-step-8c-2)).
**This is a preview:** only a host without VMs and jails can join. Do it
on test hosts.

Both sides exchange **key pins** before the first connection: the new
host's request carries its pin, the member's admission carries the pin of
every member. Nothing in either is secret; what matters is that the
request you admit is really the new host's.

## Before you start

On every host, `skep-setup apply -start` (or `skep-setup upgrade` on an
upgraded host) has initialised, enabled and started `skep_cluster`. By
hand, as root:

```
skep-cluster init                          # once, if not done
sysrc skep_cluster_enable=YES
service skep_cluster start
skep-cluster status
```

The member you admit on needs a listen address (port 8444 on its
management address). On the founding node, once:

```
skep-cluster listen -address 10.79.20.81
```

The new host must have no VMs and no jails, and `skep_agent` must be
running there (the commands ask it).

## 1. On the new host: the request

```
# skep-cluster join-request -address 10.79.20.82
Join request for this host:
  Node ID:      6c0e...
  Node name:    langrishe
  Address:      10.79.20.82:8444
  Node key pin: sha256:3f1c...

skep-join-request:1:eyJub2RlX2lkIjoi...:9b2e4c1a0f6d3e57

On a member of the cluster, compare the pin above with the one this host shows on its own console
("skep-cluster status", line "Node key pin"), then admit it, as root:
  skep-cluster admit skep-join-request:1:eyJub2RlX2lkIjoi...:9b2e4c1a0f6d3e57
The admission it prints is valid for 30 minutes.
```

`join-request` stops and starts `skep_cluster` once (it reads the store
with the service stopped), opens port 8444 on the address, and refuses on
a host that is already clustered or has guests.

## 2. On a member: compare the pin, then admit

**Compare the pin in the request with `skep-cluster status` on the new
host's own console** (not from the same copy you were sent). An
administrator tricked into admitting someone else's request lets that key
join.

```
# skep-cluster admit skep-join-request:1:...
Admitted langrishe (node ID 6c0e..., address 10.79.20.82:8444)
  pin sha256:3f1c...
  until 2026-11-24T19:02:11Z (operation 1d4f...).

Admission (nothing in it is secret):

skep-admission:1:eyJjbHVzdGVyX2lkIjoi...:0c7d5a2b9e14f386

On langrishe, as root:
  skep-cluster join skep-admission:1:...
To withdraw it before it is used:
  skep-cluster admit --withdraw langrishe
```

`admit` refuses a request whose node ID, name, address or pin a member or
another pending admission already has, and refuses on a node without a
listen address. On a member that is not the leader it is passed to the
leader. With too few voters running it is not committed and says so.

The admission expires after 30 minutes. `skep-cluster status` lists the
pending admissions; an expired one disappears from every member at once
(by a log entry the leader writes).

## 3. On the new host: join

```
# skep-cluster join skep-admission:1:...
This host will join the cluster
  Cluster ID: 61d2a433-...
with the members
  smith                e894...  10.79.20.81:8444
                       pin sha256:2a50...
This host has no VMs and no jails. Its own one-node cluster is moved aside.
This host's users (name, role):
  andy admin
The cluster's users replace them: these accounts exist here after the join only if the cluster has them, and every session on this host ends.
Join cluster 61d2a433-...? Type yes to continue: yes
```

Without a terminal (a script), give the cluster ID instead of answering:
`skep-cluster join -cluster-id 61d2a433-... skep-admission:1:...`. It
must equal the admission's. There is no "yes to everything" flag.

`join` stops `skep_cluster`, moves the host's one-node state into
`/var/db/skep/cluster/standalone.<UTC time>/`, calls the cluster, and
starts the service again.

- **If the join fails before the cluster added the host** (wrong or stale
  admission, every member unreachable, refused), it says so, lists each
  attempt, and the one-node cluster is back with the same cluster ID and
  pin:

  ```
  The join failed before the cluster added this node:
    smith (10.79.20.81:8444): TLS handshake with 10.79.20.81:8444 failed: ...
  Nothing was written in the cluster. This host's one-node cluster is back (pin sha256:3f1c...).
  ```

- **Once the cluster has added it**, or when the call was sent and no
  answer came, the join is past its point of no return:

  ```
  The cluster accepted this node (cluster 61d2a433-...).
  The join is past its point of no return: the one-node state stays moved aside (standalone.20261124T183211Z)
  and skep-cluster keeps working with the admission's members until it has joined.
  To give it up: on a member, "skep-cluster remove langrishe"; then here "skep-cluster join --abandon".
  This node has applied its member entry: state joining.
  It is a non-voter until it has caught up and every member has applied its entry;
  "skep-cluster health" on any member names the member being waited for.
  ```

The new host first receives the cluster's data as a non-voter. It becomes
a voter when it has caught up **and every member** has applied its entry.
With a member down, the join waits until that member is back; `skep-cluster
health` on every member says which one. When it is a member, the moved-aside
state is deleted.

## Check

On any member:

```
# skep-cluster status
...
Members (2):
  langrishe  6c0e...  member, voter  10.79.20.82:8444  sha256:3f1c...
  smith      e894...  member, voter  10.79.20.81:8444  sha256:2a50...
Health:        ok
```

## Removing a node

On any member, as root (here: removing `star`):

```
# skep-cluster remove star
Removing star (node ID 2a09...) from cluster ff71... (operation 5c1e...): the first entry is committed.
The leader removes it from the Raft configuration, then removes its member entry; it does that by itself.
star is removed: no member pins its key any more once each has applied the removal.
Every member has applied the removal.
On star, as root, before it can join a cluster again:
  skep-cluster leave --wipe
```

- The removal is three records: an entry that marks the member
  `removing`, its removal from the Raft configuration, and an entry that
  removes its member entry and pin. The command writes the first; the
  leader does the other two, and a new leader finishes what the last one
  left. Nothing needs to be run again.
- **Removing the leader** works the same way: it hands the leadership to
  another voter first (the output starts with "This node was the leader:
  it handed the leadership over first."). If the hand-over fails, nothing
  is written and the command says so.
- The removed node is refused by every member as soon as the
  configuration without it reaches them. It is told (best effort), or
  finds out from its own log, shuts its cluster connections and its
  listener, and `skep-cluster status` and `health` there say:

  ```
  REMOVED:       from cluster ff71..., notice from smith.greenbean.org, confirmed by 2 members refusing this key (2026-11-25T03:10:11Z)
  Run "skep-cluster leave --wipe" as root (after removing this node on a member, if that was not done).
  ```

  A notice is believed only after the node has checked it: it asks the
  other members, with its own key, and acts only if at least one answers
  and none still accepts the key (otherwise it drops the notice after
  three tries, 2 s apart). A node that was joining is told to run
  `skep-cluster join --abandon` instead, which puts its one-node cluster
  back.

  Its guests keep running. It cannot join again until it is wiped.
- **Refusals:** a name that is not a member (`not_found`); a member that is
  already being removed; any member while **another** member is joining
  (remove the joining member, or wait for its join); and a removal after
  which too few of the remaining voters answer:

  ```
  skep-cluster: voters_unanswered: without star the cluster has 2 voters and needs 2 of them for every change,
  but only 1 answered now (no answer from langrishe.greenbean.org): after this removal the cluster would accept
  no changes until they return. Nothing was written. To remove it all the same, run the command again with
  -allow-unanswered and confirm the cluster ID
  ```

  With `-allow-unanswered` the command asks for the cluster ID on the
  terminal (or takes `-cluster-id ID`; there is no "yes" flag). The
  removal then starts, but it **cannot finish** while the member that does
  not answer is down: the configuration without the removed node needs
  it. `skep-cluster health` says what it waits for, and the removal
  finishes by itself when the member returns.
- **A removal while a member is down.** When the removal finishes with a
  member down, the command and `skep-cluster health` on every member name
  the members that have not yet applied it:

  ```
  Members that have not yet applied the removal: langrishe.greenbean.org (no answer since this node started).
  ```

  A member is named as "no answer since this node started" or "applied
  N, the removal is M". A member that has once answered with the
  removal applied is not named again, even when it is down later (each
  node remembers this until it restarts).

  **Rule:** if you removed the node because it is compromised, wipe every
  member that was down during the removal (`skep-cluster leave --wipe` on
  it) before it returns to the network, and join it again. Until it has
  caught up it still trusts the removed node's key (ADR 0013 amendment 1,
  point 6).
- A pending admission is withdrawn when a removal starts: admit the host
  again afterwards. **No admission while a member is being removed:**
  `admit` is refused (`refused`, naming the member being removed) until
  the removal has finished, because the admission would list that member
  and the joining host would accept its key.

## Wiping a node: leave --wipe

On the node itself, as root. Remove it on a member first if it is still
a member (the command reminds you).

```
# skep-cluster leave --wipe
This wipes the cluster state of this node (star, node ID 2a09...):
  cluster ff71..., node key pin sha256:8b32...
It becomes a one-node cluster with a new cluster ID and a new node key, and no listener.
Wipe this node's cluster state?
Type the cluster ID (ff71...) to confirm: ff71...
This node is a one-node cluster again.
  Cluster ID:   4d0c...
  Node key pin: sha256:91ae... (new)
  Listener:     none (no network port)
If it has not been removed on a member of its old cluster yet, remove it there, as root:
  skep-cluster remove star
To join a cluster again: skep-cluster join-request -address ADDR
```

Without a terminal give `-cluster-id ID` (the current cluster ID). The
command asks for the confirmation **before** it stops `skep_cluster`, so a
wrong or missing cluster ID stops nothing; it then stops and starts
`skep_cluster` itself. The exception: when the files cannot tell the
cluster ID (only the Raft store can), `skep_cluster` is stopped first, the ID
is read from the store, and a wrong or missing confirmation then starts
it again. It keeps the node ID and
makes a **new key**, so the old pin is never valid again. If it is
interrupted (a crash, a reboot), `skep_cluster` refuses to start until you
run `skep-cluster leave --wipe` again; the second run finishes the wipe
without asking.

## Giving up a join: join --abandon

A join that is past its point of no return (the cluster added the node,
or the outcome is unknown) is not undone by the join command. To give it
up:

1. On a member, remove the node if it is listed (`skep-cluster status`
   there): `skep-cluster remove langrishe`.
2. On the node, as root:

   ```
   # skep-cluster join --abandon
     smith.greenbean.org (e894..., 10.79.20.81:8444): answers and refuses this key (...)
     star (2a09..., 10.79.20.83:8444): answers and refuses this key (...)
   The join is given up: this host's one-node cluster is back (pin sha256:3f1c...).
   ```

It asks every node of the admission and of the cluster's answer whether
it still accepts this host's key, and puts the one-node state back only if
at least one answers and none accepts it. Otherwise it changes nothing:

```
Refused: smith.greenbean.org still accept(s) this host's key, so the cluster still counts this host.
On smith.greenbean.org, as root, remove this host first:
  skep-cluster remove langrishe
then run "skep-cluster join --abandon" here again.
```

or, when nobody answers, "Run it again when at least one member is
reachable." On a host without a join in progress it refuses ("this host
has no join in progress"). When `join.json` names no moved-aside one-node
state that still exists, it refuses and changes nothing: the one-node
state is gone, and `skep-cluster leave --wipe` is the way out. These
refusals come before `skep_cluster` is stopped: it keeps running. If it is interrupted after it decided,
`skep_cluster` refuses to start until you run it again; the second run
finishes without asking the members.

If members rotated their keys after this host's admission, nobody answers
the abandon's probes (the host only knows the old pins); `skep-cluster
leave --wipe` is then the way out.

## Closing the listener: listen -none

A host that ran `skep-cluster listen` or `join-request` and is still alone
can close port 8444 again, as root:

```
# skep-cluster listen -none
skep-cluster no longer listens on 10.79.20.83:8444: no network port is open, and "skep-cluster admit" is refused.
To listen again: skep-cluster listen -address ADDR
```

It is refused on a node that is not alone in its cluster, that has a
pending admission (withdraw it first), that is joining, or that was
removed; when the running service already shows that, the refusal comes
without stopping `skep_cluster`. Otherwise it stops and starts
`skep_cluster`.

## Rotating a node key: key rotate

Every Skep node in the cluster must run a build with key rotation (0.2,
step 8b-2b or later) before the first rotation: an older build stops its
state machine at the new entries. On the node whose key is to change, as
root:

```
# skep-cluster key rotate
Rotating this node's key:
  old pin: sha256:2a50...
  new pin: sha256:7c41...
The leader added the new pin (log index 131). Every member now accepts both keys as this node.
Done: this node's pin is sha256:7c41... on its applied membership; the old pin is no longer accepted.
```

Nothing is stopped. The node switches to its new key once every member
has applied the new pin (and only once the switch is on disk: the data
directory's fsync must succeed), and the leader drops the old pin once it
has seen the node use the new key; a later connection with the old key
does not undo that. The command waits up to 2 minutes
(`-wait D`) and exits 0 when the old pin is gone; it exits 3 when the
wait ends first and says what the rotation waits for. During a rotation,
`skep-cluster status` shows the member's `next pin` and which key it was
last seen using, and `skep-cluster health` on every member shows
`key_rotation` (a warning). One rotation runs in the cluster at a time;
none is started while an admission is pending or a member is joining, and
`admit`, `join-request`, `join` and `listen -none` are refused while one
runs.

**A member is down.** The rotation waits for it (it must apply the new
pin before the node switches) and finishes by itself when it returns. If
it will not return, remove it (`skep-cluster remove NAME`): a removal is
allowed during a rotation, and the rotation then finishes. Or abort:

```
# skep-cluster key rotate --abort
```

An abort drops the next pin and deletes the next key; the pin stays. It
is refused once `node.key` on disk has the new key, or `node.key.next` is
gone (`409 refused`): the rotation then ends by itself.

**The leader refuses the rotation.** `key rotate` prints the refusal and
its reason, and the rotation is aborted: the node asks the leader to end
it keeping the old key, and deletes the next key only once the leader has
confirmed (it asks again every 10 s while the leader cannot be reached;
`skep-cluster status` shows phase `aborting`). Nothing is deleted on the
refusal alone, because after a change of leader an earlier attempt's
entry may still apply. `skep-cluster key rotate --api` (skep-api's
node-to-node key) arrives with forwarding (step 8e).

If a node's key may have been **stolen**, do not rely on a rotation:
remove the node, or wipe it (`leave --wipe`) and join it again.

## Reading the cluster audit

Each node records every replicated entry it applies (or refuses at
apply), with its actor, operation ID and outcome, and its own local steps
(init, join, abandon, removal, wipe, listen address, key rotation), in
`/var/db/skep/cluster/audit/`, as hash-chained JSON lines, one segment per
month. A node's audit of a cluster starts at its join; the members that
were there before have the earlier history. To check it, as root, with
the service running or stopped:

```
# skep-cluster audit verify
Cluster audit:   /var/db/skep/cluster/audit
Records:         57 in 1 segment(s)
Sealed segments: none (a segment is sealed when the next one exists)
Open segment:    audit-00000001.jsonl
First record:    seq 1, 2026-11-25T14:02:11.120Z, audit.started (epoch 1, index 121)
Last record:     seq 57, 2026-11-25T15:40:03.511Z, entry (epoch 1, index 168) member.key_done applied
Highest entry:   epoch 1, index 168
Every hash and every link checks out.
```

It exits 0 when everything checks out and 1 with `BROKEN: <file> line
<n>: <reason>` otherwise (2 for a usage error). `-dir DIR` checks a copy.
A segment that is a symbolic link, a FIFO or anything but a regular file
is a break ("not a regular file"); it is not followed or read.
The audit is written by `skep-cluster` itself, so a compromised node can
rewrite its own audit; compare with another member's. Its first record,
`audit.started`, counts the whole local log at that start, an
uncommitted tail included.

### A broken audit

Nothing more is written to a broken audit, and `skep_cluster` refuses to
start; its error names the break and this procedure. As root:

1. `service skep_cluster stop`
2. `skep-cluster audit verify`: see where it is broken.
3. Move it aside and keep it (use `mv`, which keeps the files' owner):
   `mv /var/db/skep/cluster/audit /var/db/skep/cluster/audit.broken.$(date -u +%Y%m%dT%H%M%SZ)`.
   `skep-cluster audit verify -dir /var/db/skep/cluster/audit.broken.<UTC time>`
   still reads it.
4. `service skep_cluster start`: a new audit begins with `audit.started`.

Compare the broken audit with another member's audit before you discard
anything. `leave --wipe` keeps `audit.broken.*` directories; nothing in
Skep deletes them.

## Clocks

`skep-cluster status` lists each other member's clock offset from this
node (± half the round trip). `skep-cluster health` warns when a member's
offset, less the uncertainty, is over 5 s and reports critical over
15 s; a member running an older build, or not answering, is "not known",
which is no warning. Nothing is refused because of a clock yet. Fix NTP
on the node named.

## A node whose key every member refuses: key_refused

A node that was removed while it was down does not learn it from Raft.
When, three status rounds in a row, every member that answers refuses
its key, `skep-cluster health` on that node shows `key_refused` (a
warning): "every member that answers refuses this node's key; it was
probably removed while it was down". Check with `skep-cluster status` on
a member whether the node is still listed; if it is not, run
`skep-cluster leave --wipe` on the node (or `skep-cluster join --abandon`
if it was joining) and join it again. The warning never shuts anything by
itself.

## What you cannot do yet

- Join a host that has VMs or jails, or remove one that owns guests with
  `--forget-guests` (step 8d).
- Admit or remove through the web interface (`skepctl` and the API can,
  from step 8c-2).
- Change the address of a clustered node.
- Limit a node without quorum to reads and to stopping and starting its
  own guests (ADR 0013 B9): this comes with the guest inventory (step 8d).

## Through the API and skepctl (step 8c-2)

Admitting a host, withdrawing an admission and removing a member are
open to the public API with the permission `cluster:manage` (only the
built-in `admin` role has it). The cluster view needs `system:read`.
`skepctl` uses the same API:

```
skepctl cluster show                       # GET /api/v1/cluster
skepctl cluster admit 'skep-join-request:...'
skepctl cluster withdraw wu
skepctl cluster remove wu                  # shows wu, asks for the cluster ID
skepctl cluster remove wu --cluster-id ID  # without a terminal
```

- **admit** takes the line `skep-cluster join-request` printed on the new
  host (at most 2 KiB) and prints the host's name, node ID, address and
  pin, then the admission and the `skep-cluster join` command to run on
  that host. **Compare the pin with the one the host shows on its own
  console** (`skep-cluster status` there) before you join; if it differs,
  withdraw the admission. The node you send it to needs a listen address:
  without one the API answers `409 cluster_refused` naming `skep-cluster
  listen -address ADDR`, which only root on that host can run.
- **remove** needs the cluster ID typed back (`cluster_id` in the body).
  It is compared on the node that receives the request and again on the
  leader: a different one is `409 cluster_id_mismatch` and nothing is
  written. When too few of the remaining voters answer, `409
  voters_unanswered`, nothing written: the API offers no override; root's
  `skep-cluster remove -allow-unanswered` on the host stays the only one.
  The answer can take up to about 90 s. Removing the node whose API you
  use hands the leadership over first (`handed_over`); that API then
  answers `503 no_quorum`, because the node is removed.
- Nothing can be committed without quorum: `503 no_quorum` with
  `details.outcome` `not_written` (no leader, `skep-cluster` not
  reachable, joining, removed) or `unknown` (sent and no answer: check
  `skepctl cluster show`). skep-api never repeats a member operation.
- Each one is in the access audit (`cluster.admit`,
  `cluster.admit.withdraw`, `cluster.member.remove`, with the node name,
  node ID, operation ID, and on failure the error code and outcome), and
  in every node's cluster audit with the actor `user:NAME`.

## Users in a cluster (step 8c-1)

The users are the cluster's: every node's `skep-api` keeps a copy of them
and sends every change through `skep-cluster`, the local `skep-api admin`
commands included. A change therefore needs the cluster's quorum. Without
it, a change answers `503 no_quorum`, only an enabled administrator may
log in (with a degraded session of at most one hour), and a password
changed on the majority still works on the cut-off node until it catches
up; its sessions for that user end then.

A host upgraded from 0.1 keeps its own users until they are imported, and
refuses user changes until then (`503 users_import_pending`). Upgrade
every node first (an older `skep-cluster` stops at the first user entry),
then, on one node only:

```sh
skep-api admin import-users
```

On every other node the cluster has users already: the command lists the
node's own users, says that the cluster's replace them, and does it with
`--yes`. A joining host takes the cluster's users (its confirmation lists
its own); `skep-cluster join` refuses a host whose import is still
pending (run `skep-api admin import-users` first, so that its users are
not lost), and goes on, saying so, when it cannot read that state. After
`leave --wipe` a host has no users until `skep-api admin create` runs.

A node that has lost its leader still applies what its own state machine
holds (a committed part of the log): a user disabled or a password
changed before the leader went away takes effect there, and that user's
sessions end. Changes still answer `503 no_quorum`. If the copy cannot be
applied at all, the `cluster_users` health check says so, and the node
counts as having no quorum until it can.
