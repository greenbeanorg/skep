# Storage

Skep keeps VM disks in the pools an administrator allow-lists: ZFS pools,
and directory pools for hosts without ZFS. On ZFS it takes snapshots of a
VM, rolls a VM back, and makes linked clones. It reports the current space
and load of everything it uses, with two health checks. The design is
[ADR 0010](../decisions/0010-storage-and-jails.md), phase 7a. Jail
templates (phase 7b) have [their own guide](jail-templates.md); jails
themselves are not in this release yet.

> **Validation status.** Verified on FreeBSD 15.1-RELEASE (amd64,
> UFS-root, a file-backed test pool and a directory pool on the root file
> system). Every check of
> [tests/integration/storage](../../tests/integration/storage/README.md)
> passed. A ZFS-root host and real disks are still pending.

## What Skep manages and what it does not

| Skep does | Skep does not |
|---|---|
| Create, grow, and destroy the datasets and zvols of its VMs under `<pool>/<dataset_root>` | Create, import, export, scrub, or destroy a pool |
| Snapshot, roll back, and clone VMs on ZFS | Touch any dataset outside `<pool>/<dataset_root>` |
| List everything under its root, flagging what it does not own | Remove anything it does not own, ever, automatically |
| Inherit encryption, compression, and quotas set on `<pool>/<dataset_root>` | Set them, or hold encryption keys |
| Report current space, disk use, and bhyve CPU and memory | Keep history: there are no time series yet |

Preparing pools is in the [VM guide](virtual-machines.md#preparing-the-host),
with [directory pools](virtual-machines.md#a-host-without-zfs-directory-pools)
for hosts without ZFS.

## Space, load, and health

The **Storage** section of the web interface shows each allow-listed pool
with its health, size, allocation, and available space, and a capacity bar
coloured by the thresholds below. Underneath are the disk space each VM
holds (snapshots included), what its disks reference now, and, while it
runs, the CPU time and resident memory of its bhyve process. Last comes the
dataset inventory.

The same values come from `GET /api/v1/metrics`, together with each
jail's root dataset space and, while a jail runs on a host with RACCT,
its CPU, resident memory, process count, and descriptor-table slots from
`rctl -u` (see [Jails](jails.md#limits)). That endpoint needs
`system:read`, `vm:read`, `storage:read`, and `container:read`, because
it names pools, VMs, and jails. The agent samples every 30 seconds and never answers with a sample
older than a minute; `observed_at` says when the values were read.

| Value | Read from |
|---|---|
| ZFS pool size, allocated, health; capacity as allocated / size | `zpool list -p` |
| Directory pool size and space; capacity as used / (used + available), as `df` shows it | `statfs(2)` |
| Available space of a ZFS pool | `zfs get available` |
| A VM's disk use on ZFS: its dataset's `used` (snapshots included), each zvol's `used` and `referenced` | `zfs list` of `<pool>/<root>` |
| A VM's disk use in a directory pool: each file's allocated blocks | `lstat(2)` |
| bhyve CPU time (user and system) and resident memory | `ps -o rss= -o time= -o comm=` on the PID the watcher observed, only after `kern.proc.args` shows it is `bhyve: skep-<vm-id>` (a recycled PID is not reported as the VM) |

### Health checks

Two checks join the health document (`GET /api/v1/health`, the dashboard,
`skepctl system`). Neither is critical, so a problem makes the status
`degraded` while the service keeps serving.

| Check | Degraded when | Configuration |
|---|---|---|
| `storage_capacity` | An allow-listed pool or directory pool is at or past `capacity_warn_percent` (a warning) or `capacity_fail_percent` (a failure) of its space in use, or cannot be read | `storage.capacity_warn_percent` (default 80), `storage.capacity_fail_percent` (default 90); 1 ≤ warn < fail ≤ 100 |
| `storage_locked` | A dataset under a pool's dataset root has its encryption key unloaded | none |

The health document has a single `degraded` status. The check's `error`
names the pool, how full it is, and which threshold it passed, for
example `pool tank is 91.2% full, past the 90% failure threshold`.
`/metrics` gives each pool's `capacity` as `ok`, `warning`, `failing`, or
`unavailable`.

## The dataset inventory

`GET /api/v1/storage/datasets` (`storage:read`), and the Datasets table of
the Storage section, list every filesystem, volume, and snapshot under
`<pool>/<dataset_root>` of each ZFS pool. Each entry has its owner:

| Owner | Meaning |
|---|---|
| `vm` | A VM's dataset, zvol, or snapshot, where the tree, the `skep:vm_id` property, and the database all agree |
| `skep` | The datasets that only hold others: `<root>`, `<root>/vm`, `<root>/jail`, `<root>/templates` |
| `unowned` | Anything else under the root, with the reason: an interrupted create, a dataset made by hand, a snapshot Skep did not take |

Unowned entries are listed and never removed automatically. A snapshot
made by hand under a VM's dataset also blocks a rollback past it, because
it is not Skep's to destroy.

## Snapshots

A snapshot of a VM is one atomic `zfs snapshot -r` of all its disks,
taken with a copy of its UEFI variable store. It can be taken while the VM
runs: it is then **crash-consistent**, the state a power cut would leave
on disk, which a journaling file system recovers from. Taken while the VM
is stopped, it is **consistent**. There is no guest agent, so nothing is
called "application-consistent".

- Names are unique per VM and use the VM name's characters. At most
  `storage.max_snapshots_per_resource` (default 64) per VM.
- A VM with disks in two pools, or with a disk in a directory pool, cannot
  be snapshotted (`storage_feature_unsupported`). ZFS snapshots are atomic
  within one pool only.
- There is no schedule and no automatic pruning. Both come with the
  backup milestone.

### Rollback

A rollback needs the VM stopped. It returns every disk, and the UEFI
variables, to the snapshot; everything written since is lost.

- ZFS rolls back to the latest snapshot only. Going back further needs
  `"destroy_newer": true` (the confirmation box in the interface,
  `--destroy-newer` in `skepctl`). That destroys every newer snapshot, and
  the task result and the audit entry name each one. Without the flag the
  request is refused with `snapshot_not_latest`, listing them.
- A newer snapshot that a clone depends on blocks it (`snapshot_has_clones`).
- A snapshot taken before a disk was added cannot be rolled back to while
  that disk exists (`snapshot_incomplete`). Remove the disks added since
  first.
- A disk's size follows ZFS: after a rollback past a growth, the disk has
  its old size.

### Clones

A clone is a new VM whose disks are linked ZFS clones of one snapshot of
the source. It is instant and shares blocks with the snapshot. Without a
snapshot named, Skep takes one first, named after the clone, and keeps it.

**A clone is the same guest.** Skep gives it a new ID (and so a new SMBIOS
UUID), name, and MAC addresses. Its hostname, SSH host keys, and machine
ID (`/etc/hostid`, `/etc/machine-id`, the Windows SID) are the source's
until you change them inside the clone. The clone form, the clone's page,
and `skepctl vm clone` all say so.

- The origin snapshot, and the source VM, cannot be deleted while a clone
  exists (`snapshot_has_clones`, naming the clones).
- A clone's disks are thin whatever the source's were, and it has no
  install media.
- Clones stay in the source's pool. Full, independent copies come with the
  backup milestone.

## Encryption

Skep encrypts nothing itself and holds no key. To encrypt everything Skep
creates, make the dataset root an encrypted dataset before first use and
load its key at boot your own way:

```sh
zfs create -o encryption=on -o keyformat=passphrase -o keylocation=prompt -o canmount=off tank/skep
```

Every dataset Skep creates inherits it. While the key is not loaded, the
`storage_locked` check is degraded and names the datasets, and the
inventory shows `keystatus: unavailable`. Creating a VM there, starting
one, snapshotting, rolling back, cloning, and adding or growing a disk are
all refused with `storage_locked`. Start is refused before a task exists;
the others fail their task before changing anything. Load the key
(`zfs load-key`) and try again.

## Command line

```sh
skepctl vm snapshot list web1
skepctl vm snapshot create web1 before-upgrade --description "pkg upgrade" --wait
skepctl vm snapshot rollback web1 before-upgrade --wait          # VM stopped
skepctl vm snapshot rollback web1 first --destroy-newer --wait   # destroys newer snapshots
skepctl vm snapshot delete web1 before-upgrade --wait
skepctl vm clone web1 web2 --snapshot before-upgrade --wait
```

See [skepctl](skepctl.md).

## Permissions

| Permission | Allows | operator | viewer |
|---|---|---|---|
| `storage:read` | Pools, media, the dataset inventory | yes | yes |
| `vm:snapshot` | Take, roll back, and delete VM snapshots | yes | no |
| `vm:write` and `vm:snapshot` | Clone a VM | yes | no |
| `vm:delete` as well as `vm:write` | Remove a disk from a VM (it destroys the zvol) | yes | no |
| `system:read`, `vm:read`, `storage:read`, and `container:read` | `GET /metrics` | yes | yes |
| `storage:write` | Nothing yet in this release (jail templates, removing unowned datasets: phase 7b) | no | no |

Every refusal is in the access audit log; every task, with what it
destroyed, is in both audit logs.

## Troubleshooting

| Symptom | Look at |
|---|---|
| `storage_capacity` degraded | `/metrics` or the Storage section: which pool, and whether it is a warning or a failure. Free space, or raise the thresholds in `agent.json` and restart the agent |
| `storage_locked` degraded | `zfs get keystatus -r <pool>/<root>`; load the key with `zfs load-key` |
| A running VM shows no CPU or memory | The VM stopped since the last sample, or bhyve could not be read; the agent's log (`daemon.log`) has a `monitoring:` line with the reason |
| `storage_feature_unsupported` on a snapshot | The VM has a disk in a directory pool, or disks in two pools |
| `snapshot_not_latest` | Roll back with `destroy_newer`, or clone the older snapshot instead |
| `snapshot_has_clones` | Delete the clones it names first |
| An `unowned` dataset | Find out where it came from; Skep leaves it alone. Remove it by hand if it is not wanted |
