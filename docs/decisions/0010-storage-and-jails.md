# ADR 0010: Storage management and jails (Milestone 7)

- Status: **Accepted**; phase 7a implemented and validated on `hearst`
  (pull request #26); phase 7b started 2026-11-16 (step 6: the RACCT
  reboot is done, the `pkg --rootdir` trace is recorded, and one question
  from it is open, see [Step 6](#step-6-racct-reboot-and-the-pkg-trace)).
  Every recommendation was
  accepted; the review answers and the amendments made at acceptance are
  under [Decisions](#decisions) and are worked into the text. The changes
  `hearst` needs are under [Changes hearst needs](#changes-hearst-needs),
  all approved.
- Date: 2026-11-14 (proposed and accepted)
- Depends on: [ADR 0003](0003-read-only-discovery.md) (discovery never
  changes the host), [ADR 0004](0004-privilege-boundary.md) (typed
  operations only, host confinement), [ADR 0005](0005-agent-task-engine.md)
  (every host operation is a task, nothing is resumed),
  [ADR 0006](0006-api-split-authentication-tls.md) (permissions),
  [ADR 0009](0009-first-vm-bhyve.md) (`internal/hostops`, the confinement
  configuration, the VM dataset layout, the acceptance-check convention).

## Summary

Milestone 7 turns the storage Skep already uses for VM disks into storage
an operator can manage, and adds the second workload type, FreeBSD jails.
It covers: snapshots, rollback, and clones of VMs; adding and growing VM
disks after creation; an inventory of what Skep owns on each pool;
storage for hosts without ZFS; jail templates, lifecycle, VNET
networking, resource limits, and snapshots; and current-value resource
monitoring of VMs, jails, and pools.

It is too large for one pull request, so it is built in two phases, each
validated on `hearst` before the next starts: **7a, storage** (Decisions 1
to 6) and **7b, jails** (Decisions 7 to 13). Decisions 14 to 16 apply to
both.

| # | Decision | Recommendation |
|---|---|---|
| 1 | What "ZFS management" means | Skep manages datasets, zvols, and snapshots inside `<pool>/<root>` of allow-listed pools only; it never creates, imports, exports, or destroys a pool, and never touches a dataset outside its root |
| 2 | Snapshots | Disk-only, atomic recursive snapshots of a VM's (or jail's) dataset, allowed while running and labelled crash-consistent; rollback only while stopped, to the latest snapshot unless the caller explicitly allows destroying newer ones; the UEFI variable store is saved with each VM snapshot |
| 3 | Clones | Linked clones (`zfs clone`) of a whole VM from one of its snapshots; the origin snapshot cannot be deleted while a clone depends on it; full copies wait for send/receive in the backup milestone |
| 4 | Disk changes after creation | Add a disk and grow a disk while stopped; never shrink; removing a disk destroys its zvol and needs `vm:delete`; with `destroy_snapshots` it destroys every snapshot containing the disk whole, never partially |
| 5 | Encryption, quotas, compression | Skep sets none of them on VM storage; it honours what the operator set on `<pool>/<root>` and refuses to use a dataset whose key is not loaded; a jail gets a `refquota` |
| 6 | Hosts without ZFS (UFS-root equivalence) | A `directory` pool type: an allow-listed directory, sparse raw files for VM disks, plain directories for jail roots; snapshots and clones are reported unsupported on it, never emulated |
| 7 | Jail userland and templates | A template is a ZFS dataset snapshot (or a directory) of a FreeBSD userland built by a task from the host's own pkgbase repository configuration, or from an operator-placed `base.txz`; the release must equal the host's; no URL from a request; `pkg` runs with a cleared environment and fixed arguments, its cache is emptied before `@skep-base`, and the host's `/etc`, `/var/db/pkg`, and `/usr/share/certs` are checked unchanged |
| 8 | How jails run | A generated per-jail `jail.conf` under the run directory and `jail(8)` with explicit arguments; no runner process; observed with `jls --libxo=json`; never `rc.conf`'s `jail_list` |
| 9 | Jail parameters | A fixed hardened baseline (`enforce_statfs=2`, `children.max=0`, no `allow.*`, `securelevel=2`); three typed opt-ins, `mlock` only with an RCTL memory limit; the parent directory of every jail root is `root:wheel 0700`; never `exec.prestart`, `exec.poststart`, or any host-side command from a request |
| 10 | Jail networking | VNET only, one `epair` per NIC, the host end in an allow-listed bridge; addresses by DHCP or static, written into the jail before its first start; no shared-IP and no `inherit` |
| 11 | Jail resource limits | RCTL limits (memory, processes, CPU percent, open files) when `kern.racct.enable=1`, refused with a clear code when not; disk by ZFS `refquota`, which needs nothing |
| 12 | Getting into a jail | No interactive console in this milestone; an optional SSH public key and `sshd_enable` at create. `jexec` to a shell is never exposed |
| 13 | Writing into a jail's filesystem | Only before its first start, or through `O_RESOLVE_BENEATH`/`O_NOFOLLOW` opens; never by path after the jail has run |
| 14 | Permissions | `vm:snapshot`; `container:read`, `container:write`, `container:delete`, `container:power`, `container:snapshot`; `storage:write` (templates, directory-pool housekeeping) for `admin` only |
| 15 | Resource monitoring | Current values only (no history): pool capacity with 80 % and 90 % health warnings, per-VM and per-jail disk use, per-jail CPU and memory from RCTL, per-VM CPU time and resident memory of the bhyve process |
| 16 | What "verified" means, and what is not in this milestone | ADR 0009's convention, under `tests/integration/storage` and `tests/integration/jails`; the not-in-scope list is at the end |

## Context

### What exists

- VM disks are zvols under `<pool>/skep/vm/<vm-id>/disk<slot>`, created by
  `vm.create` and destroyed by `vm.delete` through `internal/hostops`. Every
  dataset carries `skep:vm_id` (and `skep:disk_id` on a zvol).
- `storage.pools` in `agent.json` is the allow-list; `storage_pools` is a
  cache of what discovery observes for it. `GET /storage/pools` and
  `GET /storage/media` exist; `GET /storage/datasets` (in the standing
  endpoint list) does not yet.
- The `containers` table was sketched in `agent/0001` (kind `jail`,
  `root_volume_id`, `vnet`, `config`, desired and observed state,
  `observed_jid`) and has never held a row. As with the VM tables in
  ADR 0009, it is treated as a draft to amend, not as a decision.
  `storage_pools.type` already allows `directory`; `storage_volumes.kind`
  already allows `dataset` and `file`.
- ADR 0009 deferred to "the storage milestone": disk changes after
  creation, snapshots and clones, encryption, and `directory` pools for
  UFS-root hosts. Those deferrals are why Decisions 2 to 6 exist.
- The UEFI variable store of a VM lives in its persistent directory under
  `StateDir`, not in ZFS. A disk rollback that ignored it would leave the
  firmware's boot entries out of step with the disk (Decision 2).

### What hearst looks like today (read-only survey, 2026-11-14)

| Fact | Observed |
|---|---|
| Release | FreeBSD 15.1-RELEASE-p6, GENERIC, amd64, UFS-root, `kern.osreldate` 1501000 |
| Base system | Installed from **pkgbase**: repositories `FreeBSD-base` (`base_release_1`, signature-checked with `/usr/share/keys/pkgbase-15`), `FreeBSD-ports`, `FreeBSD-ports-kmods`; `FreeBSD-set-minimal 15.1` and `FreeBSD-jail 15.1p2` installed; no `/usr/freebsd-dist` |
| ZFS | OpenZFS 2.4.2; `skeptest` (15.9 GiB file-backed, `ONLINE`), `compression=on`, `encryption=off`, `feature@encryption` and `feature@block_cloning` enabled; one VM dataset from Milestone 6 |
| Jails | `jls` empty; `/etc/jail.conf.d` empty; `jls --libxo=json` works; `security.jail.param.allow.mount.zfs` 0 |
| VIMAGE | `kern.features.vimage=1` (in GENERIC); `if_epair.ko` present, **not loaded**; `bridge0` with `vtnet1` on VLAN 60 from Milestone 6 |
| RACCT/RCTL | Compiled in (`kern.features.racct=1`, `kern.features.rctl=1`) but **disabled**: `kern.racct.enable=0`. It is a boot-time tunable; enabling it needs `loader.conf` and a reboot |
| devfs rulesets | `devfsrules_jail=4`, `devfsrules_jail_vnet=5` in `/etc/defaults/devfs.rules` |
| bhyve memory snapshots | `bhyve(8)` documents `-r file`, but the base `bhyvectl` has no `--checkpoint`: the snapshot feature is not built into the release |
| Skep | Milestone 6 build under rc.d from `/home/aba/skep-m6` |

Two facts shape the decisions. The lab host is pkgbase, which gives Skep
a signature-checked source for jail userlands that the operator already
trusts (Decision 7). And RACCT is off, so jail memory and CPU limits
cannot be validated on `hearst` without a boot configuration change that
needs Andy's approval (Decision 11, [Changes hearst needs](#changes-hearst-needs)).

## Phase 7a: storage

## Decision 1: what "ZFS management" means

| Option | Notes |
|---|---|
| A. Datasets, zvols, and snapshots inside `<pool>/<root>` of allow-listed pools; pools are the operator's | Extends ADR 0009's confinement without widening it. Pool creation needs disk inventory, a choice of redundancy, and the means to destroy data on a disk by mistake; that is the installer milestone's, with its own safeguards. |
| B. Also create and destroy pools from whole disks | A single typo in a device name destroys a disk. Needs a disk-ownership model Skep does not have. Rejected for this milestone. |
| C. Manage any dataset in an allow-listed pool | An operator's own datasets beside `<pool>/skep` become reachable through the API. Rejected. |

**Recommendation: A.** Rules:

- Every name is derived: `<pool>/<root>/vm/<vm-id>`,
  `<pool>/<root>/jail/<container-id>`, `<pool>/<root>/templates/<template-id>`,
  and snapshot names `@skep-<snapshot-id>`. Nothing in a request is ever
  part of a dataset or snapshot name.
- `hostops` refuses any destroy or rollback of a dataset that does not
  carry the matching `skep:` user property, as `DestroyVM` already does.
  New properties: `skep:container_id`, `skep:template_id`,
  `skep:snapshot_id` (on snapshots, which take user properties).
- `GET /api/v1/storage/datasets` (`storage:read`) lists what Skep owns:
  every dataset and snapshot under `<pool>/<root>` with its type, owner
  (VM, jail, template, or **unowned**), used, referenced, and, for zvols,
  volsize and reservation. An unowned dataset under the root (an
  interrupted create, a hand-made dataset) is listed and flagged, never
  removed automatically. Cursor-paginated like the other collections.
- Pool scrub, trim, and `zpool status` detail are read-only in this
  milestone: health and the last scrub's result are shown; starting a
  scrub is not offered (it is cheap to add later, and on a file-backed
  lab pool it proves nothing).

## Decision 2: snapshots

### 2a. What a snapshot is

| Option | Notes |
|---|---|
| A. Disk-only: one recursive `zfs snapshot -r` of the VM's dataset, atomic across all its disks; allowed while running, recorded as `crash_consistent`, and as `consistent` when taken while stopped | A running VM's snapshot is what a power cut would leave on disk: every journaling filesystem recovers from it. Recording which kind it is lets the UI say so. |
| B. Disk and memory (a live checkpoint) | bhyve's snapshot support is a build option the release does not include (survey). Not available. |
| C. Only while stopped | Safe and simple, but forces a guest shutdown for every snapshot. Rejected as the only mode; A includes it. |

**Recommendation: A.** No guest agent exists to freeze filesystems, so
there is no "application-consistent" kind and none is claimed.

### 2b. The UEFI variable store

| Option | Notes |
|---|---|
| A. Each VM snapshot also copies `uefi-vars.fd` to `<StateDir>/<vm-id>/snapshots/<snapshot-id>.fd`; rollback restores it; deleting the snapshot deletes the copy | Keeps ADR 0009's layout. The copy is 128 KiB and written durably (temporary file, fsync, rename, directory fsync, as every runtime write is). |
| B. Move the variable store into the VM's ZFS dataset so the snapshot captures it | One mechanism, but it ties a VM's runtime to the dataset being mounted, and does nothing for `directory` pools, which then need A anyway. |
| C. Ignore it | A rollback past an OS reinstall leaves a boot entry for a disk layout that no longer exists. Rejected. |

**Recommendation: A.** The copy is taken while the VM is stopped or, when
running, from the file as bhyve last wrote it; the variable store is only
written when the guest changes a variable, so the race is the same one a
power cut has, and the snapshot is already labelled crash-consistent.

### 2c. Rollback

| Option | Notes |
|---|---|
| A. Only while stopped; to the latest snapshot; to an older one only with `"destroy_newer": true`, which names the snapshots it will destroy in the task result and the audit entry | ZFS can roll back only to the most recent snapshot, or with `-r` destroy everything newer. Making the destruction explicit is the point. |
| B. Allow while running (stop, roll back, start) | Hides a power-off inside a rollback. Rejected. |
| C. Never destroy newer snapshots; offer clone-from-snapshot instead | Safe, but leaves no way to go back two steps without a clone. A covers this case through the flag. |

**Recommendation: A.** A snapshot with linked clones (Decision 3) blocks
`destroy_newer` with `snapshot_has_clones`; Skep never uses `-R`.

### 2d. Schema and limits

Migration `agent/0005` adds a `snapshots` table: `id`, `resource_kind`
(`vm`, `container`), `resource_id`, `name` (the user's label, unique per
resource, the same character set as VM names), `description`,
`consistency` (`consistent`, `crash_consistent`), `created_at`,
`observed_exists`, `observed_used_bytes`. Snapshot IDs are UUIDs like
every other resource. A configurable cap per resource
(`storage.max_snapshots_per_resource`, default 64) refuses more with
`snapshot_limit`. There is no automatic snapshot schedule and no
automatic pruning: both belong to the backup milestone, where retention
is designed together with replication.

Tasks: `vm.snapshot.create` (cancellable until the ZFS call, 1 min),
`vm.snapshot.rollback` (not cancellable, 5 min), `vm.snapshot.delete`
(not cancellable, 5 min). Each holds the VM, so a snapshot cannot race a
start or a delete.

## Decision 3: clones

| Option | Notes |
|---|---|
| A. Linked clone: a new VM whose disks are `zfs clone`s of one snapshot of the source; new UUID, new name, new MAC addresses, the variable store copied from the snapshot's copy | Instant and space-efficient. The dependency on the origin snapshot is real and must be visible: deleting that snapshot, or the source VM, is refused with `snapshot_has_clones`, naming the clones. |
| B. Full copy: `zfs send | zfs receive` inside the pool | Independent of the source, but it is the replication machinery the backup milestone needs, with progress and interruption to design. |
| C. Both now | B alone is a large piece of the backup milestone. |

**Recommendation: A**, with B in the backup milestone. `zfs promote` is
not exposed: it moves snapshots between datasets with different owners,
which the ownership properties would then have to follow. A clone can be
made only from a snapshot (clone-from-running is "snapshot, then clone"
in one task, `vm.clone`, and leaves the snapshot behind, named after the
clone). A clone records `cloned_from_snapshot_id`. Same-pool only:
`zfs clone` cannot cross pools.

**A clone is the same guest.** Skep changes the VM's UUID (and so the
SMBIOS UUID), name, and MAC addresses, but not what is inside the disks:
the guest's hostname, its SSH host keys, and its machine ID
(`/etc/hostid` on FreeBSD, `/etc/machine-id` on Linux, the SID on
Windows) are those of the source. The UI says so on the clone form and
on the clone's page, and the API documentation says it for `vm.clone`
and `container.clone`. For a jail clone the same is true of its
`rc.conf` hostname, `/etc/ssh` host keys, and `/etc/hostid`; Skep does
not rewrite them, because a clone has been started before (Decision 13).

## Decision 4: disk changes after creation

| Change | Recommendation |
|---|---|
| Add a disk | `vm.update` while stopped, with the same rules as at create (slot, bus, size, thin, block size); `vm:write` |
| Grow a disk | `vm.update` while stopped; the new size must be larger and a multiple of the block size; a full-reservation disk checks `available` space first, as at create; the guest grows its own partition and filesystem afterwards, which the UI says |
| Shrink a disk | Never. ZFS would allow it and the guest's filesystem would be cut off |
| Remove a disk | `vm.update` while stopped, with `vm:delete` as well as `vm:write`, because it destroys the zvol; refused while the VM has snapshots that contain the disk unless the request also says `"destroy_snapshots": true`; the audit entry names the dataset and size. See below for what `destroy_snapshots` destroys |
| Change bus, block size, or thin/full | Not offered. Block size cannot change; changing thin to full is a reservation change that can fail for lack of space and is not worth its edge cases yet |

**`destroy_snapshots` destroys whole snapshots.** A snapshot is one
recursive `zfs snapshot -r` across all of a VM's disks (Decision 2a).
Removing only the removed disk's part would leave snapshots that roll
back to a VM with a disk missing, which is not a state the VM was ever
in. So every snapshot that contains the disk is destroyed entirely, on
every dataset of the VM, together with its variable store copy and its
row; the task result and the audit entry list each snapshot by ID and
name. A snapshot taken after the disk was added contains it; one taken
before does not and is kept. A snapshot with linked clones blocks the
removal with `snapshot_has_clones`, whatever the flag says.

Hot-plug of anything stays out: bhyve has no hot-plug for these devices.

## Decision 5: encryption, quotas, compression

| Option | Notes |
|---|---|
| A. Skep sets none of them on VM disks; it inherits what the operator put on `<pool>/<root>`, reports it in the dataset inventory, and refuses (`storage_locked`) to create in, start from, or snapshot a dataset whose `keystatus` is `unavailable` | Encryption of everything Skep creates is one `zfs create -o encryption=on -o keyformat=passphrase -o keylocation=prompt <pool>/skep` by the operator before first use, with the key loaded at boot the operator's way. Skep holds no key. |
| B. Skep creates encrypted datasets with keys it stores | A key store in the root agent, unlock at boot, rotation, and backup of the keys: a design of its own under "no plaintext secrets". Not this milestone. |
| C. Per-VM encryption choice in the API | Needs B. |

**Recommendation: A.** Quotas: a VM's size is the sum of its zvols, so no
quota is set on VM datasets. A jail's root dataset gets a `refquota` from
the request (Decision 11), which works without RACCT. Compression: the
inventory shows the effective value and ratio; Skep sets nothing.

## Decision 6: hosts without ZFS

The standing instructions require equivalent functionality on UFS-root
installs. A UFS-root host may still have a ZFS pool on another disk, as
`hearst` has; that case is already covered. The remaining case is a host
with no pool at all.

| Option | Notes |
|---|---|
| A. A `directory` pool type: an allow-listed absolute directory (`storage.directories`), root-owned `0700`; VM disks are sparse raw files `<dir>/<root>/vm/<vm-id>/disk<slot>.img` attached with bhyve's file backend; jail roots are directories `<dir>/<root>/jail/<id>/root`; capabilities report `snapshots: false`, `clones: false`, `quotas: false` and the API refuses those operations with `storage_feature_unsupported` | Every workload works; the features that need copy-on-write say plainly that they are not there. |
| B. Require ZFS, and tell UFS-root operators to make a file-backed pool, as `hearst` does | Works, but a file-backed pool on UFS is double copy-on-write layering nobody should run in production. |
| C. Emulate snapshots on directory pools by copying files | A "snapshot" that takes minutes and doubles space, with no atomicity across disks. Rejected. |

**Recommendation: A** (accepted; the UFS-root installer profile in
Milestone 10 depends on it). Rules: a directory pool must be on a local
filesystem (UFS or ZFS; checked with `statfs`, `nfs` and `tmpfs`
refused); free space is `statfs` available space; disk files are created
sparse with a fixed size (`truncate`-equivalent through `os.File`, no
shell) unless `thin: false`, which preallocates by writing zeros in a
cancellable task; growing a file disk is the same as growing a zvol.
Jail templates on directory pools are directory trees copied by a Go
walker that copies regular files, directories, and symbolic links (never
following them), refuses device nodes, sockets, and FIFOs, and preserves
ownership, modes, and file flags (`schg` is set on some base files, so
flags are applied through `hostops` after the contents are written).

## Phase 7b: jails

## Decision 7: jail userland and templates

### 7a. Where the userland comes from

| Option | Notes |
|---|---|
| A. pkgbase: `pkg --rootdir <template root> install FreeBSD-set-minimal` (plus a fixed list of extra base packages from configuration), using the host's own repository configuration, so the release and the signing key are what the host already trusts | `hearst` and FreeBSD 15 hosts installed from pkgbase have this today. The packages are signature-checked by `pkg`. The root agent makes outbound connections, but only to repositories the operator configured, never to an address from a request. |
| B. An operator-placed `base.txz` (and optionally `lib32.txz`) in a root-owned dist directory, with its `MANIFEST`; Skep checks the SHA-256 against the `MANIFEST` and extracts with `tar` into the template | Works offline and on hosts installed from distribution sets. The trust in the archive is the operator's, as with ISO media in ADR 0009. |
| C. Skep downloads `base.txz` from a URL in the request | The root agent fetches from an address a user typed. Rejected, as in ADR 0009 Decision 4. |
| D. Copy the host's own base system into the jail | Copies host configuration and secrets (`/etc/ssh` host keys, `master.passwd`). Rejected. |

**Recommendation: A and B**, chosen per template by `source: pkgbase` or
`source: dist`. A host that is not pkgbase simply has no `pkgbase` source
in its capabilities. The packages come from the operator's signed
repository, which is the trust the host already places in its own
updates.

Rules for running `pkg` (amendment at acceptance):

- **Cleared environment, fixed arguments.** `/usr/sbin/pkg` (the
  bootstrap, which execs `/usr/local/sbin/pkg`) is run by `hostops` with
  an environment containing only fixed values (`PATH=/sbin:/bin:/usr/sbin:/usr/bin`,
  `ASSUME_ALWAYS_YES=yes`, `IGNORE_OSVERSION` unset), never the agent's
  inherited environment, and with a fixed argument list: `--rootdir
  <template root>`, explicit `-o` options for the cache and database
  locations (below), `install -y`, and the package names, each validated
  against `^FreeBSD-[a-z0-9][a-z0-9-]*$` and taken only from
  `storage.template_packages` in `agent.json`, never from a request.
- **Scripts and triggers are verified, not assumed.** How `pkg --rootdir`
  runs package shell scripts, Lua scripts, and triggers on the 15.1
  host, whether chrooted into the root directory or run on the host with
  only `PKG_ROOTDIR` set, is established at step 6 from the `pkg` source
  of the version installed and from a traced build on `hearst`, and
  documented in the administration guide and the security overview with
  what was observed. If any script runs unchrooted against host paths,
  that is raised before the template feature is enabled, not worked
  around.
- **Host state is checked unchanged.** The template check (Decision 16)
  hashes every file under the host's `/etc`, `/var/db/pkg`, and
  `/usr/share/certs` before and after a build, and the build passes only
  if the hashes are identical. The `-o PKG_CACHEDIR=` and
  `-o PKG_DBDIR=` options point inside the template's work area for
  exactly this reason; if the first traced build shows `pkg` still
  writing a host path, the fix is another fixed option, never dropping
  the check.
- **The cache does not ship.** The downloaded packages are deleted from
  the template (`<template root>/var/cache/pkg` and any other
  `PKG_CACHEDIR` used) before the `@skep-base` snapshot is taken, and the
  check confirms the directory is empty in the snapshot.

### 7b. Templates

A template is built once by the task `container.template.create`
(`storage:write`, cancellable, 30 min) into
`<pool>/<root>/templates/<template-id>`, then snapshotted as
`@skep-base`; it is read-only from then on. Its row records source,
release (`freebsd-version -u` of the result), package list or archive
checksum, and size. A jail is created by `zfs clone` of that snapshot
(instant, shares unchanged blocks); on a directory pool, by copying the
template directory. Templates are per pool, because clones cannot cross
pools.

**Release rule.** A template's release must equal the host's
`freebsd-version -k` major.minor; a newer userland than the kernel is
refused (`release_unsupported`), an older one is not offered in this
milestone. When the host is upgraded, existing jails keep their userland
and the UI flags them as older than the host; updating a jail's userland
(`pkg --rootdir ... upgrade`) is a later milestone.

Thick jails only. Thin jails (a shared read-only base by `nullfs`) save
space that ZFS clones already save, and add mount management. Not in
this milestone.

## Decision 8: how jails run

| Option | Notes |
|---|---|
| A. The agent writes one `jail.conf` per jail to `<RunDir>/jails/<id>/jail.conf` (root-only, durable write) and runs `/usr/sbin/jail -f <file> -c skep-<id>`, `-r`, and `-rc`; observes with `/usr/sbin/jls --libxo=json -n` | `jail(8)` is the supported interface and does the mounts, VNET setup, and `exec.*` sequencing correctly. A generated file from typed values keeps the argument list fixed. |
| B. Call `jail_set(2)`/`jail_remove(2)` directly from Go | Needs `golang.org/x/sys/unix` or hand-written syscalls, and re-implements `jail(8)`'s ordering of mounts, interfaces, and `exec.start`. No gain. |
| C. Add jails to `/etc/jail.conf.d` and `jail_list` in `rc.conf` | Writes host boot configuration and makes `service jail` start Skep's jails at boot behind the agent's back (which is autostart, deferred). Rejected. |

**Recommendation: A.** Details:

- The jail name is `skep-<uuid>` (jail names allow `-`; a name of digits
  only is reserved for JIDs, so the prefix also avoids that). The jail's
  `host.hostname` is the request's hostname, validated as a DNS label
  sequence.
- No runner: the kernel holds a jail; `persist` is set so a jail with no
  processes stays until stopped. A jail outlives an agent restart with no
  extra mechanism.
- `exec.start = "/bin/sh /etc/rc"`, `exec.stop = "/bin/sh /etc/rc.shutdown jail"`,
  fixed by Skep. `exec.timeout` and `stop.timeout` come from typed
  configuration.
- **Stopping escalates, unlike a VM.** `jail -r` runs `exec.stop`, then
  sends `SIGTERM` and, after `stop.timeout` (default 10 s; request
  `timeout` up to 300 s), `SIGKILL` to whatever is left; the kernel then
  removes the jail. There is no way for a jail to refuse, so there is no
  `stop_timeout` and no `force_stop` for jails. The UI wording says so.
- Desired and observed state follow ADR 0009 Decision 2c: no
  reconciliation, no autostart; after a host reboot every jail is
  observed `stopped` and drift is shown.
- Migration `agent/0005` amends `containers`: drop nothing, add
  `template_id`, `provisioned`, `storage_pool_id`, `started_once`
  (Decision 13), `cloned_from_snapshot_id`, and a `container_networks`
  table mirroring `vm_networks` (slot, MAC, bridge, address mode, static
  address and gateway, `observed_epair`).

## Decision 9: jail parameters

| Option | Notes |
|---|---|
| A. A fixed hardened baseline plus a short typed list of opt-ins | The parameters that weaken a jail are few and well known; each opt-in is a boolean with a documented consequence. |
| B. Pass arbitrary `jail.conf` parameters through | `exec.prestart` and `exec.poststart` run on the **host** as root: arbitrary command execution through the API. `allow.mount`, `enforce_statfs=0`, `devfs_ruleset=0` each break isolation. Rejected. |
| C. Baseline only, no opt-ins | Breaks ordinary workloads (PostgreSQL needs SysV IPC; `ping` needs raw sockets). |

**Recommendation: A.** Baseline, not changeable through the API:
`enforce_statfs=2`, `children.max=0`, `securelevel=2`, `devfs_ruleset=5`
(`devfsrules_jail_vnet`, which every Skep jail is), `mount.devfs`, no
`mount.fstab`, every `allow.*` off, `exec.clean`, `exec.consolelog` to
`<RunDir>/jails/<id>/console.log` (root-only, served to `container:read`
holders as a bounded tail). Opt-ins: `allow_raw_sockets`,
`sysvipc` (`sysvshm`, `sysvsem`, `sysvmsg` = `new`), and `mlock`. `mlock`
is accepted only together with an RCTL `memoryuse` limit (Decision 11),
because locked memory cannot be paged out and an unlimited jail could pin
the host's memory; without the limit, or with RACCT disabled, the request
is refused with `mlock_requires_memory_limit`. `allow.chflags` was
proposed and dropped at acceptance: at `securelevel=2` it cannot clear
system flags, so it does nothing useful. Not offered: `allow.mount.*`, ZFS dataset delegation
(`jailed=on`), `allow.vmm` (bhyve in a jail), and Linux ABI; each is its
own decision later.

`securelevel=2` has a cost worth naming: inside the jail, `schg` flags
cannot be cleared, so updating the base system from inside the jail
(`pkg upgrade` of `FreeBSD-*` packages) fails. That is intended while
userland updates are not designed (Decision 7b); packages from ports
install and upgrade normally. Kept at acceptance.

**The parent of every jail root is closed to the host's users.** A jail's
tree holds setuid binaries whose owner the jail controls; a host user who
can reach one can run it with the jail root's IDs on the host. The
directory that contains every jail root, the mountpoint of
`<pool>/<root>/jail` and on a directory pool `<dir>/<root>/jail`, is
`root:wheel 0700`, and so is the directory of templates
(`<pool>/<root>/templates`, `<dir>/<root>/templates`), which hold the same
binaries. `hostops` sets the mode when it creates the directory and
checks it, with its owner, before every jail start and template build,
refusing with `jail_parent_insecure` if it has changed. A check
(Decision 16) runs as an unprivileged host user and confirms that a
setuid file inside a running jail cannot be reached.

## Decision 10: jail networking

| Option | Notes |
|---|---|
| A. VNET only: one `epair` per NIC; the host end (`epairNa`) gets a `skep:<id>:<slot>` description and joins an allow-listed bridge; the jail end gets a stable name and moves into the jail; a fixed locally administered MAC as for VMs | The jail has its own network stack, routing table, and firewall, isolated as a VM's is. It reuses the bridge allow-list and the management-interface rule from ADR 0009. |
| B. Shared IP: an alias on a host interface (`ip4.addr`) | Adds addresses to host interfaces, possibly the management one; the jail shares the host's stack. Rejected. |
| C. `ip4=inherit` | No network isolation. Rejected. |

**Recommendation: A.** The kernel numbers epairs at creation, so the
jail end is renamed on the host, before it moves into the jail, to a
name derived from the container and slot that fits `IFNAMSIZ` (15
characters) and is unique on the host; the jail's `rc.conf` refers to
that name, so it is the same on every start. The epair creation and
renaming are Skep's own fixed steps in `hostops`, run before `jail -c`,
not `exec.prestart` text from anywhere. Addressing is written into the jail's `rc.conf`
before its first start (Decision 13): `dhcp` (default) or a static IPv4
address with prefix and gateway, plus an optional IPv6 address (SLAAC or
static). A change of addressing after the first start is the jail's own
business (its `rc.conf`); Skep's record of it becomes descriptive and the
API says so. `if_epair` must be loaded by the operator
(`kld_list="if_epair"`); `ifconfig epair create` would auto-load it, and
Skep loads no modules (ADR 0009 Decision 6b), so the capability check
refuses a start with `epair_unavailable` when the module is absent. On
`hearst`, jails join `bridge0` on VLAN 60, and the isolation check of ADR
0009 is repeated for a jail.

**Orphaned epairs.** If the agent dies between `jail -r` and destroying
the host ends of a jail's epairs, they stay on the host. At every agent
start, before the socket is served, the agent lists interfaces whose
description starts with `skep:` (epairs and VM taps alike) and whose
owner is not running. Each is reported: a warning event naming the
interface and its owner, and the health check `orphaned_interfaces`
(warning) until it is gone. It is destroyed by the next start, stop, or
delete of its owner, or reported indefinitely if the owner no longer
exists; Skep never destroys an interface without a `skep:` description,
and never one whose owner is running.

## Decision 11: jail resource limits

| Option | Notes |
|---|---|
| A. RCTL rules on `jail:skep-<id>` for `memoryuse` (deny), `maxproc` (deny), `pcpu` (deny, which throttles), `openfiles` (deny), set at start and removed at stop; `refquota` on the root dataset | The standard FreeBSD mechanism. Needs `kern.racct.enable=1`, a boot tunable, off by default and off on `hearst`. |
| B. `cpuset` pinning instead of a CPU limit, and no memory limit | Works without RACCT, but no memory limit means one jail can push the host into swap. Could be offered beside A later. |
| C. No limits | Not acceptable on a shared host. |

**Recommendation: A.** When RACCT is disabled, capabilities say so and a
create or update that asks for a CPU, memory, process, or file limit is
refused with `rctl_unavailable`; a jail without such limits still runs,
and the UI shows "no limits" plainly. The disk quota works regardless.
RACCT costs a little on every process on the host; the administration
guide says so, and enabling it is the operator's choice in `loader.conf`.

**`memoryuse` deny is verified, not assumed** (amendment at acceptance).
The limit check (Decision 16) must show that a process in a jail with a
`memoryuse` deny limit really cannot allocate and touch memory past it.
If it can (resident memory is accounted after the fact, so the kernel
may page the jail out rather than refuse the allocation), that is
recorded in the implementation notes with the observed behaviour, and
`vmemoryuse` deny (which refuses address-space growth at the allocation)
is evaluated as the memory limit, or as a second limit beside it, before
the feature is called verified.

## Decision 12: getting into a jail

| Option | Notes |
|---|---|
| A. A shell through `jexec` over the console WebSocket | Hands every `container:console` holder a root shell, with no credentials, on a system sharing the host kernel. Violates "never expose a root shell". Rejected. |
| B. `login(1)` on a pty inside the jail, brokered like the VM serial console | Needs credentials inside the jail. A fresh jail's root has no password and `pts` devices are not secure ttys, so root cannot log in; the console is useless until someone has set up a user by other means. Useful later. |
| C. No console in this milestone; an optional SSH public key at create, installed for root with `sshd_enable` and `PermitRootLogin prohibit-password` | Reaching the jail over its own network is the usual way. A public key is not a secret. |

**Recommendation: C**, with B as a follow-up once there is a way to set a
user inside a jail without passing a password through Skep. `jexec` is
never exposed through the API, in any form.

## Decision 13: writing into a jail's filesystem

A root process that writes into a directory a jail controls can be
redirected by a symlink the jail planted (`/etc/rc.conf -> /../../etc/master.passwd`
resolves on the host). Rules:

- Skep writes configuration into a jail (`rc.conf` lines, the SSH key,
  `resolv.conf` for static addressing) only while `started_once = 0`,
  that is, into a tree that came from a read-only template and has never
  run code.
- Every such write uses `openat` with `O_RESOLVE_BENEATH` relative to the
  jail root's directory descriptor and `O_NOFOLLOW` on the last component,
  so even a planted link cannot leave the root.
- After the first start, Skep never writes into the jail's tree. Updates
  that would need it are refused with `container_started`.
- `started_once` never goes back to 0: not after a rollback, even to a
  snapshot taken before the first start, and not on a clone, which
  inherits 1 from its source if the source had ever started, and 1 in any
  case if the clone was taken from a running jail. Whether the tree on
  disk has run code is not something Skep can tell after the fact, so it
  assumes it has.

## Phase-wide decisions

## Decision 14: permissions

| Permission | Covers | `operator` | `viewer` |
|---|---|---|---|
| `vm:snapshot` | Create, roll back, delete VM snapshots; clone needs `vm:write` as well | yes | no |
| `container:read` | List and read jails, their snapshots, the console log tail | yes | yes |
| `container:write` | Create (from a template), update, clone | yes | no |
| `container:delete` | Delete a jail and its dataset | yes | no |
| `container:power` | Start, stop, restart | yes | no |
| `container:snapshot` | Create, roll back, delete jail snapshots | yes | no |
| `storage:write` | Create and delete templates; remove unowned datasets flagged in the inventory | no (`admin` only) | no |

Removing a VM disk needs `vm:delete` (Decision 4). `storage:write` is
admin-only because building a template makes the root agent fetch and
install packages. Migration `api/0005` updates the built-in roles as
`api/0004` did. Every task writes `requested` and `finished` audit
entries in both logs; destructive tasks (rollback with `destroy_newer`,
disk removal, deletes) list every dataset and snapshot destroyed.

## Decision 15: resource monitoring

| Option | Notes |
|---|---|
| A. Current values only, refreshed by the existing watcher: pool size, allocated, available, health, and capacity; per-VM disk used and referenced; per-VM CPU time and resident memory of the bhyve process (`kern.proc`); per-jail CPU, memory, and process count from `rctl -u` when RACCT is on, and disk used always | What the UI needs to show load and space now, without a storage engine for history. |
| B. Time series with history | Needs retention, downsampling, and storage sizing: the monitoring work, which also has to decide between storing locally and exporting (Prometheus). Not this milestone. |

**Recommendation: A.** `GET /api/v1/metrics` returns these values for the
node. Two health checks join ADR 0007's: `storage_capacity` warns at 80 %
and fails at 90 % of an allow-listed pool or directory pool (the
thresholds are configuration), and `storage_locked` fails when a dataset
Skep uses has its key unloaded.

## Decision 16: what "verified" means, and what is not in this milestone

ADR 0009 Decision 11's convention holds: a feature is verified when its
named check has run on `hearst` with the FreeBSD build and its output is
recorded, under `tests/integration/storage/` for 7a and
`tests/integration/jails/` for 7b. Checks planned:

| Feature | Check |
|---|---|
| Inventory | `GET /storage/datasets` lists the M6 VM's tree with owners; a hand-made dataset under `skeptest/skep` is listed as unowned and left alone |
| Snapshot | Taken while running (crash-consistent) and while stopped (consistent); the VM's two disks share one snapshot; the variable store copy exists |
| Rollback | Rollback of a guest that wrote a marker file after the snapshot removes the marker and the guest boots; rollback to an older snapshot is refused without `destroy_newer` and destroys exactly the listed snapshots with it |
| Clone | A linked clone boots with new MACs and its own DHCP lease; deleting the origin snapshot or the source VM is refused with `snapshot_has_clones` |
| Disk add, grow, remove | A second disk appears in the guest; a grown disk is grown with `gpart resize` and `growfs` in the guest; removal is refused for an `operator` without `vm:delete`; with `destroy_snapshots`, the snapshots taken after the disk was added are gone from every dataset of the VM and the older ones are intact |
| Directory pool | A VM installs and boots from a file disk in a directory pool on `hearst`'s UFS root; snapshot and clone are refused with `storage_feature_unsupported` |
| Template (pkgbase) | `container.template.create` builds a 15.1 userland from `FreeBSD-base`; the release recorded is `15.1-RELEASE-pN` |
| Template (dist) | A `base.txz` whose SHA-256 does not match `MANIFEST` is refused; a matching one builds |
| Template host safety | Before and after a pkgbase build, the hashes of every file under the host's `/etc`, `/var/db/pkg`, and `/usr/share/certs` are identical; `pkg` ran with the fixed environment (shown by `procstat -e` during the build); `var/cache/pkg` is empty in `@skep-base`; how scripts and triggers ran (chrooted or not) is recorded with the trace |
| Jail lifecycle | Create, start, stop, restart, delete; `jls` shows it under `skep-<id>`; it survives `service skep_agent restart` and `kill -9` of the agent |
| Jail networking | DHCP address on VLAN 60, internet reachable, `10.79.20.0/24` not reachable; the host epair has the `skep:` description and is gone after stop |
| Jail baseline | Inside the jail: `sysctl security.jail.param` shows the baseline; `mount`, `kldload`, and raw sockets fail; with `allow_raw_sockets`, `ping` works; `mlock` without a memory limit is refused with `mlock_requires_memory_limit` |
| Jail root parent | The jail and template parent directories are `root:wheel 0700`; as an unprivileged host user, a setuid file inside a running jail cannot be reached or executed; a start after `chmod 755` of the parent is refused with `jail_parent_insecure` |
| Orphaned epairs | Killing the agent between `jail -r` and the epair destroy leaves an epair that the restarted agent reports (event and `orphaned_interfaces`) and the next start of the jail removes |
| `started_once` | After a rollback of a started jail to a snapshot taken before its first start, an update that would write into the tree is still refused with `container_started` |
| Limits | With RACCT on: a process in a jail with `memoryuse` 256 MiB cannot allocate and touch memory past it (if it can, the observed behaviour is recorded and `vmemoryuse` is evaluated, Decision 11); `pcpu` 50 throttles a busy loop; with RACCT off, a limit is refused with `rctl_unavailable` |
| Clone identity | The clone form and page in the UI carry the note that a clone shares the source's hostname, SSH host keys, and machine ID (browser test) |
| Symlink rule | A jail that was started cannot be updated in a way that writes into its tree; an `O_RESOLVE_BENEATH` test with a planted link fails safely (unit test natively on `hearst`) |
| Monitoring | `/metrics` reports pool capacity and jail usage; `storage_capacity` warns when the threshold is lowered below the current use |
| Permissions | `viewer` reads and is refused every write, each audited; `operator` is refused `storage:write` |

Not verifiable on `hearst`, recorded as pending: a ZFS-root host; an
encrypted `<pool>/skep` (possible on `skeptest`; listed as optional in
the hearst changes); a host installed from distribution sets rather than
pkgbase (the `dist` source is checked, the host shape is not).

**Not in this milestone**, so nobody looks for them: pool creation or
destruction; scrub and trim actions; replication, send/receive, full
clones, scheduled snapshots, snapshot pruning (backup milestone);
Skep-managed encryption keys; memory snapshots of VMs; thin jails;
updating a jail's userland; jails of an older release than the host;
Linux jails; ZFS dataset delegation into jails; `allow.vmm`; a jail
console; templates for VMs; metric history; NAT and bridge management
(networking milestone); autostart.

## Consequences

- Skep can now destroy data on request in new ways (rollback, disk
  removal, snapshot deletion). Each is a task, each needs a delete-class
  permission or an explicit flag, and each lists what it destroyed in the
  audit log.
- A second workload type runs with the host's kernel. Its isolation rests
  on the fixed parameter baseline (Decision 9), VNET (Decision 10), and
  the write rule (Decision 13); none of these is configurable through the
  API.
- The root agent makes outbound connections for the first time, only to
  the operator's configured `pkg` repositories, only when an admin builds
  a template.
- `hostops` gains snapshot, clone, rollback, property, and file-pool
  operations, a `Jails` interface (`jail`, `jls`, `rctl`, `epair`), and
  the `openat`-based writer; the Linux fake gains the same.
- New configuration in `agent.json`: `storage.directories`,
  `storage.dist_dir`, `storage.template_packages` (names validated as `FreeBSD-*`),
  `storage.max_snapshots_per_resource`, `storage.capacity_warn_percent`,
  `storage.capacity_fail_percent`, and `containers.stop_timeout_seconds`.
- No new Go or frontend dependency is planned. If `O_RESOLVE_BENEATH` or
  `jail` attachment turns out to need `golang.org/x/sys/unix`, that will
  be raised as an implementation note with its reason, not added
  silently.
- The UI's Storage and Containers sections stop being placeholders.

## Suggested implementation order

Each step is one reviewable change that leaves the tree working.

**7a, storage**

1. Migration `agent/0005` (snapshots, the container amendments) and
   `api/0005`; `hostops` snapshot, rollback, clone, and property calls
   with the fake; `GET /storage/datasets` (Decisions 1, 5, 14).
2. VM snapshot, rollback, and clone tasks and routes; the variable store
   copies (Decisions 2, 3). First validation on `hearst`.
3. Disk add, grow, remove (Decision 4).
4. Directory pools (Decision 6). Second validation.
5. Monitoring values and the two health checks (Decision 15); the UI's
   Storage section and VM snapshot tab; `skepctl vm snapshot ...`. The 7a
   check table, docs, and implementation notes. **7a merges here.**

**7b, jails**

6. The `kern.racct.enable` reboot on `hearst` (hearst change 3), then
   templates from pkgbase and dist (Decision 7), with the trace of how
   `pkg --rootdir` runs scripts and triggers. Validation: a template
   builds on `hearst` and the host-state hashes are unchanged.
7. `hostops` Jails interface, the conf generator, the baseline, the
   `openat` writer; `container.create`, `start`, `stop`, `restart`,
   `delete` (Decisions 8, 9, 13).
8. VNET networking (Decision 10). Validation: DHCP and isolation.
9. RCTL limits and quotas (Decision 11); jail snapshots and clones,
   reusing step 2.
10. The UI's Containers section and `skepctl container ...`; the 7b check
    table, docs, and implementation notes. **7b merges here.**

## Changes hearst needs

All approved at acceptance (review answer 12). They are applied in this
order, and nothing else on `hearst` changes without asking.

**Before any change**

1. The Proxmox snapshot `pre-m7` of `hearst`, taken before the first
   change of this list and before any Milestone 7 build is deployed.
   Skep will not rely on it.

**On hearst, modules, live and persistent** (from step 8, no reboot)

2. `kldload if_epair` now, and `kld_list="if_epair"` in `/etc/rc.conf`
   (added to any existing list) so it loads at the next boot. For jail
   VNET (Decision 10).

**On hearst, `/boot/loader.conf`** (boot configuration)

3. `kern.racct.enable="1"`, then a reboot, at the **start of 7b** (step
   6), so the template, jail, limit, and monitoring checks all run with
   RACCT on. For Decisions 11 and 15.

**On hearst, files and datasets**

4. A directory pool at `/var/tmp/skep/dirpool` (`root:wheel 0700`), on
   the UFS root beside the existing `skeptest.img`, listed in
   `storage.directories` of the test installation. About 10 GiB of sparse
   files at most; 93 GiB are free. From step 4.
5. An encrypted test dataset root on `skeptest` (`skeptest/skep-enc`,
   `keyformat=passphrase`), to check the `storage_locked` refusal with
   the key unloaded.
6. Outbound HTTPS from `hearst` to `pkg.FreeBSD.org` during template
   builds (already used by `pkg` on the host). If Pi-hole blocks it, the
   build stops, Andy is told, and the `dist` source with a copied
   `base.txz` is used; the block is not worked around.

**Reboots**

- The reboot budget for Milestone 7 is **two**. One is item 3; the other
  is held for a check or a recovery that turns out to need it, and is
  used only after saying why.
- After **every** reboot of `hearst`, before any `vm.start`, confirm that
  `sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery` is `0` (the
  nested-KVM workaround from ADR 0009's implementation notes; without it
  the first `VMLAUNCH` stops the whole host). If it is not `0`, no VM is
  started until it is.

## Decisions

Accepted on 2026-11-14 with these answers to the twelve review
questions.

1. **Two phases**, 7a storage and 7b jails, each merged after its own
   validation.
2. **Snapshots while running are allowed**, labelled crash-consistent
   (Decision 2a).
3. **Rollback to an older snapshot only with `destroy_newer`**
   (Decision 2c).
4. **Linked clones only**; full copies with the backup milestone
   (Decision 3).
5. **Encryption left to the operator** at `<pool>/<root>`, no Skep key
   store (Decision 5).
6. **Directory pools**, with snapshots and clones reported unsupported
   (Decision 6). Needed for the UFS-root installer later.
7. **pkgbase and `base.txz` template sources, release pinned to the
   host's** (Decision 7), with amendment a.
8. **The hardened baseline with `securelevel=2`** (Decision 9), with
   amendment b.
9. **No jail console** (option C, Decision 12); the `login(1)` console is
   a follow-up.
10. **The permissions as proposed**, `storage:write` admin-only
    (Decision 14).
11. **Monitoring as current values only** (Decision 15).
12. **Every hearst change approved**, with amendment c on how and when.

Amendments made at acceptance, worked into the text:

- **a. `pkg` runs confined and is checked** (Decision 7a, the template
  host safety check of Decision 16). The exact way `pkg --rootdir` runs
  shell scripts, Lua scripts, and triggers (chrooted, or with only
  `PKG_ROOTDIR` set) is verified on `hearst` and documented. `pkg` runs
  with a cleared environment and fixed arguments. The hashes of the
  host's `/etc`, `/var/db/pkg`, and `/usr/share/certs` must be unchanged
  after a build. The package cache is emptied out of the template before
  `@skep-base`.
- **b. Jail parameters** (Decision 9). `allow_chflags` is dropped: at
  `securelevel=2` it cannot clear system flags. `mlock` is accepted only
  together with an RCTL memory limit. The parent directory of every jail
  root (and of the templates) is `root:wheel 0700`, checked before use,
  with a check that an unprivileged host user cannot reach a setuid file
  inside a jail.
- **c. How the hearst changes are made** (Changes hearst needs).
  `pre-m7` is taken before any change, not only before step 2.
  `if_epair` is loaded live with `kldload` and persisted with `kld_list`,
  without a reboot. The `kern.racct.enable` reboot happens at the start
  of 7b. The reboot budget for Milestone 7 is two. After every reboot,
  `hw.vmm.vmx.cap.virtual_interrupt_delivery` is confirmed `0` before any
  `vm.start`.
- **d. Clarifications requested at review:**
  - Removing a disk with `destroy_snapshots` destroys each snapshot that
    contains the disk whole, on every dataset, never leaving it partial
    (Decision 4).
  - Epairs (and taps) orphaned by an agent crash are reported at agent
    start, as an event and the `orphaned_interfaces` health warning, and
    removed by the owner's next start, stop, or delete (Decision 10).
  - `started_once` stays 1 after a rollback, even to a snapshot taken
    before the first start (Decision 13).
  - The memory-limit check must show that `memoryuse` deny really stops
    allocation; if it does not, that is recorded and `vmemoryuse` is
    considered (Decision 11).
  - The UI notes that a clone shares the source guest's hostname, SSH
    host keys, and machine ID (Decision 3).

## Implementation notes

Recorded during the build of 7a (from 2026-11-14). The decisions above
stand; these are the places where the implementation had to choose
something the text left open, or departs from it.

### Step 1: schema, hostops, inventory

- **`agent/0005` also creates `container_templates`**, because the
  container amendment's `template_id` needs a table to reference; its
  columns are those Decision 7b lists (source, packages or archive
  checksum, release, size). `snapshots` gains a `provisioned` flag like
  VMs, so a row left by an interrupted snapshot task is recognisable.
  Snapshot rows are deleted with their resource by trigger; a clone's
  `cloned_from_snapshot_id` is `ON DELETE RESTRICT`, so the database
  refuses to delete an origin snapshot, or its VM, while a clone exists,
  independently of the ZFS check.
- **Permissions are granted in `api/0005` and declared when enforced.**
  `api/0005` gives the built-in roles every permission of Decision 14 at
  once, as the ADR orders. `internal/auth` declares each one only when a
  route requires it (`vm:snapshot` with step 2, the `container:*` ones in
  7b), because the route table test requires every declared permission
  to be enforced. Until then the string sits in the role and
  `Role.Allows` denies it, as it denies every undeclared permission.
- **Two error codes the text did not name.** A rollback to a snapshot
  that is not the latest, without `destroy_newer`, is
  `snapshot_not_latest` (409), with the newer snapshot IDs in the
  details. A rollback or clone of a snapshot that lacks a dataset the
  resource has now (a disk added after the snapshot) is
  `snapshot_incomplete` (409). The ADR does not cover the second case;
  using the snapshot would give a VM a disk with post-snapshot contents,
  a state it was never in (the same reasoning as Decision 4's), so it is
  refused before anything changes, and the message says to remove the
  disks added since. A `snapshot_has_clones` also comes from `DestroyVM`,
  which before this milestone would have failed in `zfs destroy` with an
  unnamed error.
- **Snapshots Skep did not take block a rollback past them.** A newer
  snapshot without a matching `skep:snapshot_id` (made by hand under a
  VM's dataset) is not Skep's to destroy, so `destroy_newer` refuses with
  `snapshot_not_latest` and names it, instead of letting `zfs rollback -r`
  take it.
- **Rollback is one `zfs rollback` per dataset.** ZFS has no recursive
  rollback (`-r` destroys newer snapshots; it does not descend). Every
  precondition is checked for every dataset first, so a refusal changes
  nothing; a failure of the second or later command (I/O error, pool
  suspended) leaves the earlier datasets rolled back. The task reports
  that as a failure naming the dataset.
- **Clone properties are set, not inherited.** A ZFS clone does not
  copy the origin's local properties. `CloneVM` sets `skep:vm_id`,
  `skep:disk_id`, `canmount=off`, and `volmode=dev` explicitly. A linked
  clone has no reservation whatever the source had: it is thin.
- **The inventory's owner kinds** are `vm`, `container`, `template`,
  `skep` (the datasets that only hold others: `<root>`, `<root>/vm`,
  `<root>/jail`, `<root>/templates`), and `unowned` with a reason. A
  dataset is owned only when its place in the tree, its owner property,
  and a database row agree; a snapshot is judged by its dataset and is
  unowned if Skep did not take it. Pages are 100 entries by default, 500
  at most, because a pool with snapshots has many more datasets than VMs.
  A pool that cannot be read is reported in `unavailable_pools`.

### Step 2: snapshots, rollback, clones

- **Snapshots go through hostops directly, not the hypervisor adapter.**
  They are storage operations, the same for any engine; the adapter is
  asked only whether the VM runs (for the consistency label and the
  rollback refusal).
- **A VM with disks in more than one pool cannot be snapshotted**
  (`storage_feature_unsupported`). ZFS creates snapshots atomically only
  within one pool, so such a VM cannot have the "one atomic snapshot of
  every disk" of Decision 2a, and a weaker one is not offered.
- **`vm.clone` holds the source VM** as its task resource, so the source
  cannot be started, rolled back, or deleted while it is being cloned. The
  clone is a new, unprovisioned VM inserted with the task; deleting it is
  refused with `resource_busy` while its clone task runs. A clone has the
  source's CPU, memory, and NICs (new MAC addresses), thin disks, and no
  install media.
- **Two more codes**: `snapshot_name_taken` (409) for a label the VM
  already uses, and `snapshot_not_provisioned` (409) for a snapshot whose
  task never finished, which can only be deleted.
- **Size follows ZFS after a rollback or clone.** A zvol rolled back, or
  cloned, from a snapshot taken before the disk grew has the snapshot's
  size; the disk's recorded size is set to the zvol's after either.
- The snapshot of a VM also carries `skep:vm_id`: user properties of a
  dataset show on its snapshots (seen on `hearst`), though the inventory
  judges a snapshot by its dataset in any case.

### Step 3: disk changes

- **The request shape**: `add_disks` (disk specs as at create),
  `resize_disks` (`slot`, `size`), `remove_disks` (slots), and
  `destroy_snapshots` on `PUT /vms/{id}`. New disks take the lowest slot
  no disk or medium uses; a slot freed by the same request is not reused
  by it. A removal is refused with `disk_has_snapshots` (409) while
  snapshots contain the disk and the flag is not set, and also when the
  disk has a snapshot Skep did not take.
- **`vm:delete` is checked by skep-api from the body**: an update with
  `remove_disks` needs it as well as the route's `vm:write`, refused and
  audited like a route denial.
- **Steps are recorded as they are done.** The task destroys the
  snapshots, then removes, grows, and adds, writing each change to the
  database as soon as it is done on the host, so a failure part way leaves
  the database describing the disks that exist; the task fails naming the
  step. `vm.update`'s time limit went from 1 to 5 minutes for this.
- **Space for an update** is checked by the service, not by the adapter's
  create-time check (which skips provisioned VMs): only the new
  reservations count, that is full-reservation disks added and the growth
  of full-reservation disks.

### Step 4: directory pools

- **`storage.directories` is a list of `{"name", "path"}`**, not of bare
  paths: a disk names its pool in a request as it names a ZFS pool, so a
  directory pool needs a name, and names are one namespace with
  `storage.pools` (a repeat is refused at load, as are relative, unclean,
  duplicate, and nested paths). The checks of the directory itself (not
  a symbolic link, `root`, mode `0700`, `statfs` type `ufs` or `zfs`) run
  on every use, not at load, so a pool that goes missing reports
  `storage_unavailable` with the reason instead of stopping the agent.
  The file system types are an allow-list (`ufs`, `zfs`) rather than a
  deny-list of `nfs` and `tmpfs`.
- **Every file operation goes through `os.Root`** (Go 1.24+, standard
  library) opened on the pool's directory, so no derived name can resolve
  outside it, even through a planted link; disk files are also opened
  with `O_NOFOLLOW` and checked with `lstat` as regular files. No new
  dependency.
- **One router, `hostops.Disks`**, sends create, grow, remove, delete,
  listing, and pool reads to ZFS or to the directory implementation by the
  pool's kind; the bhyve adapter and the VM service use it. Snapshots,
  rollback, and clones stay ZFS calls; the service refuses them first
  with `storage_feature_unsupported` for a VM with a disk in a directory
  pool (a VM with disks in both kinds was already refused as multi-pool).
  `GET /storage/datasets?pool=<directory pool>` is refused the same way;
  the inventory is of ZFS datasets.
- **A file disk has no ownership marker.** A zvol carries `skep:vm_id`
  and `skep:disk_id`; extended attributes could do the same for a file,
  but the path is already derived from the VM's ID inside a directory
  only root can open, so the path is the ownership. A VM's directory is
  removed only when it holds nothing but `disk<N>.img` regular files;
  anything else fails the delete and is left in place.
- **The database records the pool's kind.** `storage_pools.type` (already
  allowing `directory`) is set from the configuration, and a volume in a
  directory pool is `kind = 'file'` named `<root>/vm/<id>/disk<slot>.img`
  relative to the pool, with no `desired_volblocksize`. A pool name whose
  row has the other type is refused (`storage_unavailable`), and so is a
  VM disk recorded in one kind whose pool name is now configured as the
  other: a disk is never looked for in the wrong place. A disk shows
  `pool_type` and, in a directory pool, `file` instead of `dataset`;
  `GET /storage/pools` shows `type` and `capabilities`. No migration was
  needed.
- **Preallocation and its limits.** A disk that is not thin is checked
  against `statfs` available space, then written with zeros in 1 MiB
  chunks, checking cancellation between chunks; a failed or cancelled
  create removes the file, and a failed growth truncates it back to its
  old size. On a ZFS file system with compression the zeros are not
  stored, so the space is not truly held; this is documented, not worked
  around. `volblocksize` is refused for a directory pool rather than
  ignored.

### Step 5: monitoring, health checks, interface, CLI

- **The sampler has its own 30-second loop**, not the VM watcher's
  2 seconds: a `zfs list` of every pool that often would cost more than
  the values are worth. `GET /metrics` and both health probes read the
  latest sample, which is never more than a minute old (an older one is
  replaced before answering), so a health request runs no ZFS command.
  Per-jail values come with 7b.
- **bhyve's CPU time and memory come from `ps(1)`** (`/bin/ps -o rss=
  -o time= -o comm= -p <pid>`), not from decoding `struct kinfo_proc`:
  `golang.org/x/sys` has no FreeBSD binding for it and its layout is not
  an interface. Before `ps` runs, `kern.proc.args` of the PID the watcher
  observed must be `bhyve: skep-<vm-id>` (bhyve's `setproctitle(3)`
  title, seen on `hearst`), so a recycled PID is never reported as the
  VM. On `hearst` the first version, with `-o rss=,time=,comm=`, failed:
  FreeBSD's `ps` takes everything after `=` as the header, commas
  included. A FreeBSD-only test now runs the real `ps`.
- **Capacity** is allocated / size for a ZFS pool, as `zpool list` shows
  it, and used / (used + available) for a directory pool, as `df` shows
  it (the root reserve is not available to Skep's files either).
- **One `degraded` level for both thresholds.** The health document has
  no "warning" status, and making `storage_capacity` critical would make
  `skep-api` answer 503 for a full pool, taking a working service away
  from a load balancer. So a pool past either threshold makes the check
  `degraded`; its message names the threshold, and `/metrics` gives
  `capacity` as `ok`, `warning`, `failing`, or `unavailable`. A pool that
  cannot be read also degrades `storage_capacity`.
- **`GET /metrics` needs `system:read`, `vm:read`, and `storage:read`**:
  it names VMs and pools.
- **Start is refused with `storage_locked` too.** Steps 1 to 3 refused
  create, snapshot, rollback, clone, and disk changes in a locked
  dataset, but not start; on `hearst` a VM whose key was unloaded failed
  in bhyve ("Device emulation initialization error"). The start check
  now reads the VM's ZFS datasets and refuses before a task exists, and
  again when the task runs.
- **The `storage_locked` check used `skeptest/skep-enc` as the dataset
  root** (hearst change 5). The test installation's `dataset_root` was
  switched to it for the check and back afterwards, because the check is
  of an encrypted `<pool>/<root>` and a sibling dataset is outside the
  root.
- **`storage:write` is still undeclared.** No 7a route needs it (step 1's
  rule: a permission is declared when a route enforces it), so "operator
  is refused `storage:write`" is shown by the role and the route table
  test until 7b adds templates. **7b must verify it with a real request**:
  an `operator`'s `container.template.create` refused with `403`
  (`storage:write`) and audited, on `hearst` (Andy, 2026-11-15).
- **The interface**: the Storage section replaces its placeholder (pools
  with capacity against the thresholds, per-VM use, the paged inventory
  with owners); the VM page gains a Snapshots card. A rollback to an
  older snapshot asks for confirmation naming the snapshots it destroys
  and sends `destroy_newer` only then. The clone form and a clone's page
  carry the identity note of decision 3. `skepctl vm clone` prints it
  too, on standard error with `--json` so the JSON stays parseable.
- **The browser check against a real host** is
  `web/e2e/clone-note.spec.ts`, skipped unless pointed at one; the local
  browser suite covers the same note against the simulated host.
- **`/var/tmp/skep` on `hearst` is now `root:wheel`** (approved for this
  step). The device-and-inode comparison of step 4 stays: it is what
  protects a directory pool whose parent is not root's.

### Step 6: RACCT reboot and the pkg trace

Recorded from 2026-11-16 for phase 7b. Evidence in
[tests/integration/jails/](../../tests/integration/jails/README.md).

- **hearst change 3 is applied.** `kern.racct.enable="1"` is in
  `/boot/loader.conf` (backup `loader.conf.pre-racct-20261116`), and
  `hearst` was rebooted on 2026-11-16: reboot 1 of the budget of two. After
  the reboot, before anything else, `hw.vmm.vmx.cap.virtual_interrupt_delivery`
  was `0` (it is set in `loader.conf` since ADR 0009), `kern.racct.enable`
  was `1`, and the pool, both services, and `bridge0` came back. No VM was
  started.
- **How `pkg --rootdir` runs scripts (pkg 2.9.2), from its source and a
  traced build.** Shell scripts are spawned as the host's `/bin/sh`,
  **not chrooted**, with `PKG_ROOTDIR` set. They use the host's binaries
  and write where their text points. Lua package scripts are forked
  unchrooted, with file access redirected to the root descriptor (no
  `RESOLVE_BENEATH`) and `pkg.exec` free to run host binaries. Triggers
  marked `sandbox: false` are **deferred** under `--rootdir`: saved into
  `<dbdir>/triggers/` for `pkg triggers` inside the target, never run on
  the host. Sandboxed triggers run under Capsicum. Repository
  configuration comes from the host. `pkg.conf`, `PKG_DBDIR`, and
  `PKG_CACHEDIR` default to paths inside the root. Trust anchors are
  looked up in the root first, then on the host: safe only because a
  template root is always empty when Skep creates it.
- **On the 15.1 minimal set** (66 packages), three packages have
  scripts, all shell: `FreeBSD-caroot` (`certctl -D<root> rehash`),
  `FreeBSD-runtime` (`pwd_mkdb -d <root>/etc`, `services_mkdb -o
  <root>/...`), and `FreeBSD-utilities` (`cap_mkdb <root>/etc/login.conf`).
  The one trigger, mandoc's, is unsandboxed and would run `makewhatis
  /usr/share/man` on the host; it was deferred. The traced build read
  `/etc/nsswitch.conf`, `/etc/shells`, and `/etc/ssl/openssl.cnf` from the
  host and wrote only into the root, apart from pkg's catalogue download
  temporaries in the host's `/tmp`. Every file under the host's `/etc`,
  `/var/db/pkg`, and `/usr/share/certs` was identical afterwards (type,
  mode, owner, size, mtime, SHA-256). A `find -newer` over the UFS root
  found nothing else changed.
- **Fixed options the build needs, from the trace.** `TMPDIR` set to the
  work area (it is part of the fixed environment), so nothing lands in the
  host's `/tmp`. `PKG_CACHEDIR` in the work area, outside the root, so the
  cache never enters the template. `PKG_DBDIR` inside the root, so the
  jail's own `pkg` knows the base packages and runs the deferred trigger.
  The catalogue copies this leaves in `<root>/var/db/pkg/repos/` (every
  host repository's, ports included) are removed before `@skep-base`.
- **Open question, raised before the template feature is enabled**
  (Decision 7a's rule). Shell scripts run unchrooted, as root, on the
  host, and they read host paths. Their writes stay in the root only
  because their text uses `${PKG_ROOTDIR}`. That text comes from packages
  signed by the repository the host already trusts for its own updates.
  The options:
  - **A.** Accept it as observed and document it in the administration
    guide and the security overview. Every build runs the host-state hash
    check at run time as well (about 1,500 files, seconds), so a build
    that changed the host fails and raises an event; it cannot undo the
    change. Repeat the trace for each new FreeBSD release before Skep
    offers it.
  - **B.** Run `pkg` with `RUN_SCRIPTS=false` and
    `PKG_TRIGGERS_ENABLE=false`, and have Skep run the four commands
    itself with fixed arguments. Refuse the build if the installed
    packages carry any script beyond the four known ones. No package code
    runs on the host. The cost is that Skep must track the base set's
    scripts for every release.
  - **C.** Two phases: `--rootdir` with scripts off, then the scripts
    run inside a `chroot` of the new root, using the root's own binaries.
    That is the strongest confinement, but it needs pkg internals pkg
    does not expose (`pkg` has no "run the scripts of installed packages"
    command), so in practice it becomes B with the commands chrooted.

  Recommendation: **B, with the commands run chrooted in the new root**
  (`chroot <root> /usr/sbin/pwd_mkdb -d /etc /etc/master.passwd` and so on,
  using the root's binaries, which the release rule makes the host's
  version anyway), plus A's run-time hash check. No package-supplied code
  runs on the host, and an unknown script stops the build instead of
  running. The pkgbase source is not implemented until Andy answers. The
  rest of step 6 (the template store, the `dist` source, the task and the
  routes) does not depend on the answer.

