# Joining hosts into a cluster

Every Skep host is a one-node cluster (`skep-cluster`, see
[the architecture page](../architecture/skep-cluster.md)). This page joins
a host to another host's cluster with the commands of Milestone 8, step
8b-1. **This step is a preview:** only a host without VMs and jails can
join, a joined host cannot leave yet, and users are not shared yet (each
host keeps its own). Do it on test hosts.

Both sides exchange **key pins** before the first connection: the new
host's request carries its pin, the member's admission carries the pin of
every member. Nothing in either is secret; what matters is that the
request you admit is really the new host's.

## Before you start

On every host, as root:

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
This host has no VMs and no jails. Its own one-node cluster is moved aside; its users are not changed in this build.
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
  This build has no command to give up a join.
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

## What you cannot do yet

- Join a host that has VMs or jails (step 8d).
- Remove a node, or make a host leave (`leave --wipe`), or give up a join
  (`join --abandon`): the next run of step 8b. Until then a test cluster is
  reset by hand on each host: `service skep_cluster stop`, delete
  `/var/db/skep/cluster` and `/usr/local/etc/skep/cluster.json`,
  `skep-cluster init` again (a new cluster, the same key only if
  `node.key` is kept; deleting the directory makes a new key).
- Admit through the web interface or `skepctl` (step 8c).
