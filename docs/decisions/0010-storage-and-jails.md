# ADR 0010: Storage management and jails (Milestone 7)

- Status: **Accepted**; phase 7a implemented and validated on `hearst`
  (pull request #26); phase 7b started 2026-11-16 (step 6: the RACCT
  reboot is done, the `pkg --rootdir` trace is recorded, and the decision
  on package scripts is accepted, see
  [Step 6](#step-6-racct-reboot-and-the-pkg-trace); a probe on 2026-11-16
  found the specification not buildable as written, and Andy's answers are
  recorded under
  [Resolved](#resolved-the-probe-of-the-specification-2026-11-16); the
  builder, the template store, tasks and routes are built, and the builder
  is validated on `hearst`, see
  [Built](#built-the-template-builder-2026-11-16); step 7, jails, started
  with a probe whose findings are amendments f and g, see
  [Step 7](#step-7-jails-decisions-8-9-13)).
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
| 9 | Jail parameters | A fixed hardened baseline (`enforce_statfs=2`, `children.max=0`, no `allow.*` beyond the defaults `allow.suser` and `allow.reserved_ports`, `securelevel=2`); two typed opt-ins, `mlock` only with an RCTL locked-memory limit (amendment j); the parent directory of every jail root is `root:wheel 0700`; never `exec.prestart`, `exec.poststart`, or any host-side command from a request |
| 10 | Jail networking | VNET only, one `epair` per NIC, the host end in an allow-listed bridge; addresses by DHCP or static, written into the jail before its first start; no shared-IP and no `inherit` |
| 11 | Jail resource limits | RCTL limits (memory, processes, CPU percent, open files) when `kern.racct.enable=1`, refused with a clear code when not; disk by ZFS `refquota`, which needs nothing |
| 12 | Getting into a jail | No interactive console in this milestone; an optional SSH public key and `sshd_enable` at create, only from a template that has sshd; templates hold no SSH host keys. `jexec` to a shell is never exposed |
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
  configuration. (Amendment i: the jail.conf has no `exec.start`; Skep
  runs `/etc/rc` itself with `jexec` after the `/dev` guard. `exec.stop`
  is unchanged.)
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
(`devfsrules_jail_vnet`, which every Skep jail is; a jail with a DHCP
interface gets Skep's ruleset 6001 instead, ruleset 5's rules plus
`bpf*`, amendments h and i), `mount.devfs` checked against an allow-list
before any process runs in the jail (amendment i), no
`mount.fstab`, every `allow.*` that grants extra privileges off, with
`allow.suser` and `allow.reserved_ports` left at their default, on
(amendments e and f), `exec.clean`, `exec.consolelog` to
`<RunDir>/jails/<id>/console.log` (root-only, served to `container:read`
holders as a bounded tail). Opt-ins: `sysvipc` (`sysvshm`, `sysvsem`,
`sysvmsg` = `new`) and `mlock`. (`allow_raw_sockets` was dropped by
amendment f: a VNET jail has raw sockets on its own stack whatever
`allow.raw_sockets` says.) `mlock`
is accepted only together with an RCTL `memorylocked` limit (Decision 11;
amendment j, which replaced the `memoryuse` limit here),
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

**Amended 2026-11-17 (Andy, from the step 8 probe; amendment h).**

- **DHCP needs bpf.** `dhclient` opens `/dev/bpf`, which ruleset 5
  hides. A jail with at least one `dhcp` interface gets Skep's own devfs
  ruleset **6001**: ruleset 5's own rules plus bpf, written out as
  `100 include 1`, `200 include 2`, `300 include 3`, `400 include 4`,
  `500 path pf unhide`, `600 path bpf* unhide` (amendment i: devfs does
  not resolve the includes of an included ruleset, so `include 5` would
  hide nothing).
  A jail with only static addressing (or no interface) keeps plain
  ruleset 5. The agent adds ruleset 6001 to the kernel at runtime with
  `devfs rule -s 6001 add ...` when it is absent (no file; it is gone at
  reboot and added again by the next DHCP start), and checks its rules
  before every start of a DHCP jail; if 6001 holds any other rules, the
  start is refused with `devfs_ruleset_conflict` and Skep changes
  nothing. bpf in a VNET jail sees only the jail's own interfaces (what
  the bridge delivers to its port) and can write any frame on its epair,
  which is what a VM can do on its tap; amendment f already accepts
  spoofing on the epair.
- **`if_epair` in production**: the operator sets `kld_list="if_epair"`
  (the installer writes it into its profiles in Milestone 10), as for
  `vmm`. Skep checks `kldstat -q -m if_epair` before any `ifconfig epair
  create`, which would otherwise load the module itself, and refuses with
  `epair_unavailable`.
- **The host end never has an address.** Skep creates and describes the
  host end in one call, `ifconfig epair create description
  skep:<id>:<slot> inet6 ifdisabled -auto_linklocal up`, so a crash cannot
  leave an undescribed epair (which Skep would never destroy) and the host
  never gets an IPv6 link-local address on a jail's link, whatever the
  host's IPv6 configuration.
- **Static addresses are checked on `hearst`** as well as DHCP, from a
  range on VLAN 60 outside the Kea pool.

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

**Amended 2026-11-16 (Andy, from the step 7 probe; amendment g).**
`FreeBSD-set-minimal` has no sshd. The SSH key is accepted only when the
jail's template has `/usr/sbin/sshd`; otherwise the create or update is
refused with `container_ssh_unavailable` (422), naming the template.
`FreeBSD-ssh` joins the default `storage.template_packages` (it has no
package scripts and no trigger files on 15.1, so the allowlist does not
change). **A template holds no SSH host keys**: the build fails if
`etc/ssh/ssh_host_*` exists in it before `@skep-base`. Each jail
generates its own at its first start (`rc.d/sshd`), so two jails from
one template never share a host key; the check of Decision 16 confirms
it. FreeBSD's sshd defaults to `PermitRootLogin no`; Skep sets
`prohibit-password` through `rc.conf` (`sshd_flags`), without editing
`sshd_config`.

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
| `container:snapshot` | Create, roll back, delete jail snapshots (a rollback also needs `container:write`, amendment n) | yes | no |
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
| Jail devfs | Before any process runs in the jail, its `/dev` is listed and compared with the allow-list; on `hearst`, the `/dev` of a real DHCP jail (ruleset 6001) and a real static jail (ruleset 5) are listed and compared: they differ only by `bpf` entries, and neither holds anything outside the allow-list; a jail whose `/dev` holds an extra entry is refused and removed before `/etc/rc` runs (amendment i). The test reads a real jail's `/dev`, not the ruleset text |
| Jail networking | DHCP address on VLAN 60, internet reachable, `10.79.20.0/24` not reachable; a static address works the same way (amendment h; on `hearst` deferred, unit tests only, until a range is given: amendment i); the host epair has the `skep:` description and no address, and is gone after stop and delete; a DHCP jail has devfs ruleset 6001 and a static one ruleset 5, and a start with a foreign ruleset 6001 is refused with `devfs_ruleset_conflict`; without `if_epair` a start is refused with `epair_unavailable`; a jail that forges its source IPv4 address and its source MAC on its epair reaches nothing it could not reach without (amendment f) |
| Jail baseline | Inside the jail: `sysctl security.jail.param` shows the baseline; `mount` and `kldload` fail; a port below 1024 can be bound; whether raw sockets work is recorded, not expected to fail (amendment f); `mlock` without a memory limit is refused with `mlock_requires_memory_limit` |
| Jail SSH | A key works on a template with sshd and is refused with `container_ssh_unavailable` on one without; the template has no `etc/ssh/ssh_host_*`; two jails from one template have different host keys (amendment g) |
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
   VNET (Decision 10). **Changed at step 8 (Andy, 2026-11-17):** on
   `hearst` it is loaded live only, not persisted, for step 8; how
   production loads it is amendment h.

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
- **e. `allow.suser` stays on in ordinary jails** (Decision 9; Andy,
  2026-11-16, from the step 6 probe). An ordinary jail keeps `allow.suser`
  at its default, on; every `allow.*` that grants extra privileges stays
  off. The reason: `allow.suser` is itself an `allow.*` parameter, and with
  it off, root inside the jail has no privileges (on `hearst`, `jexec -U
  root` failed with `initgroups: Operation not permitted`), so `/etc/rc`
  and the services a jail exists to run fail. Only the template build jail
  of step 6 runs with `allow.suser` off, because its fixed commands need
  nothing beyond owning the files they write.
- **f. `allow.reserved_ports` stays on, and the raw-socket opt-in is
  dropped** (Decision 9; Andy, 2026-11-16, from the step 7 probe).
  - With `allow.reserved_ports` off, a jail's root cannot bind a port
    below 1024 (`nc -l 22`: Permission denied), so sshd, web, and DNS
    servers fail. It stays at its default, on, like `allow.suser`. Every
    other `allow.*` is off, the kernel defaults `allow.set_hostname` and
    `allow.unprivileged_proc_debug` included.
  - `allow.raw_sockets` has no effect in a VNET jail: the 15.1 kernel
    (`prison_priv_check` in `sys/kern/kern_jail.c`) grants
    `PRIV_NETINET_RAW` to every jail with its own network stack before it
    looks at `allow.raw_sockets`. Every Skep jail is VNET (Decision 10),
    so the `allow_raw_sockets` opt-in is dropped, and the baseline check
    records whether raw sockets work instead of expecting a refusal.
  - **Consequence for networking.** A VNET jail's root can craft and
    spoof packets on its own epair: any source IPv4 or IPv6 address, and
    any source MAC, as a VM can on its NIC. The VLAN isolation test of
    step 8 must therefore include source-IP and source-MAC spoofing from
    a jail (Decision 16, "Jail networking").
- **g. SSH keys and host keys** (Decision 12; Andy, 2026-11-16, from the
  step 7 probe). The key only for a template with sshd
  (`container_ssh_unavailable` otherwise); `FreeBSD-ssh` in the default
  template packages; no SSH host keys in a template; each jail makes its
  own at first start, and two jails from one template are checked to
  differ.
- **h. Jail networking** (Decisions 9 and 10; Andy, 2026-11-17, from the
  step 8 probe). A jail with a DHCP interface gets Skep's devfs ruleset
  6001 (ruleset 5 plus `bpf*`), added by the agent at runtime and checked
  before every start (`devfs_ruleset_conflict` if 6001 holds other
  rules); static jails keep ruleset 5. `if_epair` is the operator's
  (`kld_list`), checked before any `ifconfig epair create`
  (`epair_unavailable`). The host end is created and described in one
  `ifconfig` call, with IPv6 disabled on it by Skep. Static addresses are
  checked on `hearst` as well as DHCP.
- **i. Ruleset 6001 corrected, and a `/dev` guard for every jail**
  (Decisions 9, 10, 16; Andy, 2026-11-17, from the step 8 validation).
  - Ruleset 6001 is ruleset 5's rules written out plus bpf: `100 include
    1`, `200 include 2`, `300 include 3`, `400 include 4`, `500 path pf
    unhide`, `600 path bpf* unhide`, the same single level of includes as
    ruleset 5. The agent adds it when absent and checks it line by line
    before every start (`devfs_ruleset_conflict` otherwise), as in
    amendment h.
  - **The `/dev` guard runs before any process starts in the jail.**
    `jail -c` creates the jail with its baseline, `mount.devfs`, and its
    epairs but **without `exec.start`**, so nothing runs in it. Skep then
    lists the jail's `/dev` on the host (its devfs mount under the jail
    root; no process of the jail has existed, so nothing in it can have
    been planted) and compares every entry with a short allow-list: `fd`,
    `null`, `zero`, `crypto`, `random`, `urandom`, `pts`, `ptmx`, `stdin`,
    `stdout`, `stderr`, `zfs`, `fuse`, `pf`, and for a DHCP jail `bpf`
    and `bpf<N>`. The list says what may be present, not what must be:
    an entry on it that is absent (on `hearst`, `crypto`, `ptmx`, `fuse`,
    and `pf` with `pf` not loaded) is not an error. Any entry not on the
    list, a `dev` that is a link or not a devfs mounted at `<root>/dev`,
    or a devfs without `null` refuses the start
    (`container_start_failed`, naming the extra entries) and removes the
    jail with `jail -r` before any of its code has run. Only after the
    guard passes does Skep run the jail's `/etc/rc`, as its own fixed
    step: `jexec -l -U root skep-<id> /bin/sh /etc/rc` with the clean
    environment `exec.clean` gave before, output appended to the jail's
    `console.log`, bounded by `containers.start_timeout_seconds`. `jexec`
    stays internal; it is never exposed through the API (Decision 12).
    `exec.stop` (`/etc/rc.shutdown`) is unchanged. The guard applies to
    every jail, ruleset 5 included.
  - **Its check** (Decision 16, "Jail devfs") reads the `/dev` of a real
    DHCP jail and a real static jail on `hearst`, compares them with each
    other and with the allow-list, and shows a refused start for a jail
    with an extra entry; a test of the ruleset text alone is not enough.
  - **Static addressing on `hearst`**: no range was given, so the live
    static check is skipped for now; static addressing is covered by
    unit tests only until a range is approved.
  - Redeploying the corrected build on `hearst` is approved.
  - **Skep running rc itself is confirmed** (Andy, 2026-11-17): `jexec
    -l -U root skep-<id> /bin/sh /etc/rc`, with these requirements.
    - The jail is created with `persist`, so it survives with no
      processes while the guard runs.
    - `exec.stop` stays configured, so `jail -r` still runs
      `/etc/rc.shutdown`; verified on `hearst`.
    - If `/etc/rc` fails or hits the start timeout, the jail is stopped
      and removed (`jail -r`, which runs `/etc/rc.shutdown`), its epairs
      destroyed, the result is `container_start_failed`, and rc's output
      stays in `console.log`.
    - A jail left half-started by an agent crash (the container still
      recorded `starting` at agent start, its jail present) is handled by
      the start-up sweep: removed before the socket is served, its epairs
      destroyed, and the container recorded `stopped` with a detail that
      says why.
    - My reading of two cases the answer leaves open: a jail refused by
      the guard, and a half-started jail found by the sweep, are removed
      **without** `exec.stop`. In the first, none of the jail's code may
      run with the devices it should not have; in the second, Skep cannot
      know whether the guard passed. Their jail.conf is written for the
      removal without `exec.stop`; `jail -r` then sends `SIGTERM` and
      `SIGKILL` as usual and unmounts devfs.
    - Static range: the live static test is skipped; unit tests only.
- **j. Limits, `mlock`, clone permission, jail metrics** (Decisions 9,
  11, 14, 15, 16; Andy, 2026-11-17, from the step 9 validation, Q1 to Q6
  in [step9-progress.md](../../tests/integration/jails/results/step9-progress.md)).
  - **`limits.memory` stays `memoryuse` deny, documented as soft**
    (Q4, option A). On `hearst` a jail held twice its `memoryuse` limit
    with nothing refused. Before the docs are written, one longer run
    (about 3 minutes holding twice the limit, `rctl -u` and swap watched)
    records what the limit does on FreeBSD, and the API docs and the UI
    label it soft: a resident-memory target, not a cap at allocation.
    Decision 11's check is met by recording that behaviour, not by a
    refusal.
  - **No address-space limit (`vmemoryuse`) in 7b**; it is in the
    backlog.
  - **`mlock` is tied to `memorylocked`, not to the memory limit**
    (Decision 9 as amended): the opt-in is accepted only together with
    an RCTL `memorylocked` deny limit (`limits.locked_memory`) and with
    RACCT on; otherwise `mlock_requires_memory_limit` as before (the code
    keeps its name: a locked-memory limit is the memory limit `mlock`
    needs).
  - **A jail clone needs `container:write` and `container:snapshot`**
    (Q1, option B), as a VM clone needs `vm:write` and `vm:snapshot`.
    Decision 14's table is read that way.
  - **Jail usage in `/metrics`** (Decision 15) is built in step 9 if it
    is a read-only exposure of `rctl -u`; if it is bigger, it goes to the
    backlog after 7b (Q2; the assessment is in step9-progress.md).
  - **No reboots for the RACCT-off check** (Q3): it stays on the
    pending-FreeBSD-validation list, covered by unit tests.
  - Docs also say: `refquota` counts the data the jail root references
    from its template (about 78 MiB for `minimal-151`), so the room left
    for the jail's own data is the quota less that; `pcpu` is
    approximate (RACCT samples and averages, so a busy jail swings above
    and below the limit around its mean).
- **k. Fixed in-jail interface names, clone addressing, host keys**
  (Decisions 3, 10, 13, 14, 15; Andy, 2026-11-17, from the step 9 stop,
  Q7 in [step9-progress.md](../../tests/integration/jails/results/step9-progress.md)).
  - **In-jail interface names are fixed by slot: `skep0`, `skep1`, ...**
    (Q7, option C), for every jail, not only clones. Skep's `rc.conf`
    lines in a jail's tree name `skep<slot>`, so a jail's network
    configuration does not depend on its container ID, and a clone's
    copied tree works unchanged (Decisions 3 and 13 hold: Skep still does
    not rewrite a started tree). After `vnet.interface` has moved the jail
    end into the jail, and before the `/dev` guard and `/etc/rc`, Skep
    renames it inside the jail with `ifconfig -j skep-<id> <name> name
    skep<slot>`: the host's ifconfig, fixed arguments, and none of the
    jail's code runs. The host end's name and its
    `skep:<container-id>:<slot>` description stay ID-based, and orphan
    detection keys on the host end.
  - **On a normal stop**, each jail end is renamed back to its ID-based
    name (or its epair destroyed) before the jail is removed, so no two
    interfaces named `skep0` return to the host.
  - **Probe first** (before any of this is built): on `hearst`, what
    15.1 does when a jail is removed abnormally and two returned
    interfaces are both named `skep0` (whether the rename or the return
    fails, and what is left), and whether orphan detection still
    identifies them by their host ends. If that is unsafe, the build
    stops and the findings go to Andy.
  - **Static addresses on clones:** a clone request must give new
    addressing for every NIC of the source that has a static address;
    without it the request is refused (422). Separately, a start is
    refused when the jail's static IPv4 address is already held by a
    running Skep jail on the same bridge.
  - **`GET /metrics` requires `container:read`** as well (confirmed): the
    values name jails, as they name VMs and pools.
  - **SSH host keys:** a clone has its source's `/etc/ssh` host keys
    (Decision 3); the docs say so and say to regenerate them inside the
    clone. An opt-in host-key reset on clone is in the backlog.
  - The `internal/templates` flake (`TestCreateAndDeleteTemplate`
    asserting `provisioned` in the create response) gets the Q5 fix.
- **l. Static clones, clone defaults, stop order, leftover devfs, the
  duplicate-address test** (Decisions 3, 8, 10, 13, 16; Andy, 2026-11-17,
  from the step 9 validation, Q8 and section 14 of
  [step9-progress.md](../../tests/integration/jails/results/step9-progress.md)).
  - **Q8: a clone of a jail with a static address that has started is
    refused in 7b** (`clone_addressing_required` without new addressing,
    `container_started` with it): Skep does not write into a tree that has
    run code. Backlog: an opt-in that writes the clone's addressing into a
    Skep-owned file under `/etc/rc.conf.d`, through the
    `O_RESOLVE_BENEATH` in-jail writer.
  - **Clone defaults (approved):** a clone's hostname defaults to the
    clone's name, as on a create; the source's nameservers are dropped
    when no NIC of the clone keeps a static address.
  - **Stop order (recorded):** on a normal stop, and on every removal Skep
    makes, the jail's epairs are destroyed before `jail -r`, so no jail end
    returns to the host; the jail's `/etc/rc.shutdown` runs without its
    NICs.
  - **Leftover devfs (approved fix):** a removal outside Skep can leave a
    devfs mounted on `<root>/dev`, which blocks the jail's delete. Skep
    finds such a mount with getfsstat(2), only where `f_mntonname` is
    exactly `<root>/dev` and `f_fstypename` is `devfs`, and unmounts it by
    its fsid (`MNT_BYFSID`), never by a path it resolved. This runs before
    a start, at delete, and in the agent's start-up sweep, only while no
    jail of that name runs. A test shows that a jail root whose `dev` is a
    symbolic link leaves the host untouched. The live test is the cleanup
    of `s9dhcp` and `c9dhcp` on `hearst` (no hand changes).
  - **Duplicate static IP at start, live test (approved):** on `hearst`,
    `ifconfig bridge1 create` (no members, no uplink, nothing in
    `rc.conf`), `bridge1` added to `network.bridges` in `agent.json`,
    addresses from 192.0.2.0/24. Undo right after the test: `bridge1`
    removed from `agent.json`, the agent restarted, `ifconfig bridge1
    destroy`.
  - **Security review (step 10) gets two items:** duplicate host
    interface names after an abnormal removal (two `skep0`); a jail's root
    can rename its interface to a host-like name (`vtnet0`), which returns
    to the host under that name after a removal outside Skep.
- **m. Templates report whether they have sshd** (Decision 12, amendment
  g; Andy, 2026-11-17, from the step 10 UI stop, Q1 in
  [step10-progress.md](../../tests/integration/jails/results/step10-progress.md)).
  Template responses (`GET /storage/templates`, `GET
  /storage/templates/{id}`, and the template in a create response) carry
  `has_sshd`, computed by the same check the agent uses to refuse a key
  with `container_ssh_unavailable` (a regular file `usr/sbin/sshd` in the
  template), so there is one source of truth. The web UI shows the SSH key
  field only for a template with `has_sshd` true. Bridges for jails stay in
  `GET /system`. Backlog: whether an empty `nameservers` list on a clone
  clears the source's (today an empty or omitted list keeps them).
  Also (Andy, 2026-11-17, from the step 10 sysvipc answer): the template
  build jail of step 6 sets `sysvshm`, `sysvsem`, and `sysvmsg` to
  `disable` explicitly instead of leaving them to the kernel's default, and
  `checkBuildJail` compares them with the kernel after `jail -c`; a
  mismatch refuses the build and removes the build jail. The web UI's edit
  dialog asks for confirmation before a save that removes a limit.
- **n. Triage of the M7b security review** (Andy, 2026-11-18, from
  [security-review-m7b.md](../../tests/integration/jails/results/security-review-m7b.md);
  dispositions per finding are in that file's Triage section).
  - **Fixed now:**
    - SR-03: a NIC MAC is accepted only if it is unicast (multicast bit
      clear), locally administered (bit `0x02` set), and not all zeros; the
      API refuses anything else with 422, in create, edit, and clone, and the
      web UI checks the same rules. At start the agent also refuses a MAC
      that is already present on the target bridge, on its member
      interfaces, or on any other Skep container.
    - SR-04: locked memory is never unbounded. With the `mlock` option on,
      `limits.locked_memory` is required (422; the UI requires it).
      Whenever `locked_memory` is set, it is applied as its own RCTL
      `memorylocked` rule and read back like the others, whether or not
      `memory` is set.
    - SR-06: a jail whose `/dev` guard (or parameter check) refused it and
      whose removal then failed is recorded in a failed, quarantined state
      with its own error code and hint. It is never shown as running, Skep
      never runs `exec.stop` or `/etc/rc.shutdown` in it, start and stop are
      refused, and delete and the start-up sweep retry the removal without
      `exec.stop`.
    - SR-09 (the FIFO): the in-jail writer opens with `O_NONBLOCK` as well
      as `O_RESOLVE_BENEATH`/`O_NOFOLLOW`, `fstat`s the descriptor, and
      refuses anything that is not a regular file before any read or write.
    - SR-11 (rollback permission): rollback needs `container:write` and
      `container:snapshot`, the same as clone.
    - SR-05, documentation only: the jail docs list what Skep's limits do
      not cover (swap, threads, shared memory, ptys, disk I/O, kernel
      memory) and that a `container:write` user can create a jail with no
      limits. Default limits, or limits required per role, go to the
      backlog.
  - **SR-01** (console.log): probe and proposal only, not built in this
    run; the fix is decided after the comparison of options.
  - **SR-02** (layer-2/3 anti-spoofing): deferred until the spoofing tests
    with Andy at OPNsense; no code.
  - **Backlog:** SR-07, SR-08, SR-10, SR-I1, SR-I2, SR-I4, SR-I5, SR-I6.
  - **Hearst validation list:** SR-I3 (jail root cannot use `/dev/zfs` to
    list, set properties on, or snapshot datasets).
- **o. Decisions on the security review fix run** (Andy, 2026-11-18, from
  section 13 of
  [step10-progress.md](../../tests/integration/jails/results/step10-progress.md)).
  - **Approved as built:** the seven changes outside the request
    (section 13.7: the learnt address table and `hwaddr` in the bridge
    MAC check; Skep VMs in the start-time MAC check; a pre-amendment MAC
    refused at start; the locally-administered rule in hostops as well;
    the half-started sweep failure quarantined and a stop or restart of a
    container still recorded `starting` refused; the delete check order;
    the renamed summary log field). The web UI gets no MAC field.
    `readSkepFile` does not refuse `nlink > 1`.
  - **SR-01, console.log: option (a).** At each start the agent truncates
    `console.log` and gives `/etc/rc` (through jexec) a pipe, which the
    agent drains into the file: the first 256 KiB are written, the rest is
    counted and discarded, and when the start ends a
    `N bytes discarded` marker and the last 64 KiB of the output are
    written. The agent closes its end of the pipe when the start ends,
    whether it succeeded, failed, or timed out, and never waits for
    end-of-file. A process rc leaves running that writes to its stdout or
    stderr afterwards gets `SIGPIPE` (or `EPIPE` if it ignores the
    signal); this is documented. `exec.consolelog` is dropped from the
    jail.conf, so `jail -r` gets `/etc/rc.shutdown`'s output on its own
    stdout and stderr, through the output-capped runner; output beyond
    the cap is discarded with a marker and never turns a removal into an
    error (the runner's strict mode is not used for this call, or a
    capped mode is added).
  - **SR-04, locked memory: a host total.** A new setting,
    `containers.max_locked_memory_bytes`, is the total `locked_memory`
    of all running jails, defaulting to 25 % of physical memory
    (`hw.physmem`, read at agent start). The API refuses a single
    `locked_memory` above the total (422). At start, the agent refuses
    when this jail's `locked_memory` plus that of the jails already
    running exceeds the total, with a new `container_*` code and a UI
    hint. The docs say that bhyve VMs with wired memory draw from the
    same system pool (`vm.max_user_wired`).
  - **Hearst validation (section 13.9) approved**, on these conditions:
    it runs after Andy's Proxmox snapshot; `skep-api` is stopped during
    the `sqlite3` edit; the hand-made epair and all test data are removed
    afterwards; every change made on hearst is recorded in the progress
    log.
- **p. Decisions on the amendment o build** (Andy, 2026-11-18, from
  section 15 of
  [step10-progress.md](../../tests/integration/jails/results/step10-progress.md)).
  - **Approved as built:** the ten changes outside the request (section
    15.3).
  - **rc.shutdown's output is kept** (15.4 point 1). The output `jail -r`
    returns is appended to `console.log` after a `--- shutdown ---`
    marker line, with its own budget: the first 32 KiB, a
    `N bytes discarded` marker, and the last 32 KiB. The agent writes it
    from memory after `jail -r` returns; a failure to write it never
    fails the removal.
  - **A failed `jail -r` reports the last line of its stderr** (15.4
    point 2), not the first: control characters stripped, at most 200
    bytes, and labelled as jail output in the error and in the log.
  - **The locked-memory total stays out of `GET /system` in 7b** (15.4
    point 3). Backlog: expose it in `GET /system` so the UI can check a
    `locked_memory` value before sending it.
- **q. Decisions on the amendment p build** (Andy, 2026-11-18, from
  section 17 of
  [step10-progress.md](../../tests/integration/jails/results/step10-progress.md)).
  - **Approved as built:** the nine changes outside the request (section
    17.3), and the final hearst list for SR-01 and SR-04 (section 17.5).
  - **rc.shutdown's stderr is kept too** (17.4 point 1). After the stdout
    section, the agent appends a `--- shutdown stderr ---` marker line and
    the last 32 KiB of what `jail -r` wrote to stderr, preceded by the
    `N bytes discarded` marker when it was cut. The same failure rules
    apply: a failure to write it never fails the removal.
  - **A failed `jail -r` still appends nothing** (17.4 point 2): kept as
    built until hearst test 10 (section 17.5) shows whether a failing
    `rc.shutdown` fails `jail -r`.
  - **Unicode format characters are stripped** (17.4 point 3): category
    Cf, including the bidi overrides U+202A–U+202E and U+2066–U+2069, is
    removed wherever Skep reports or logs jail output, alongside the
    existing control-character cleanup.

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
- **The question this raised** (Decision 7a's rule: an unchrooted script
  is raised before the template feature is enabled). Shell scripts run
  unchrooted, as root, on the host, and read host paths. Their writes
  stay in the root only because their text uses `${PKG_ROOTDIR}`. Three
  options were put to Andy on 2026-11-16: **A**, accept it, document it,
  and check the host files on every build; **B**, turn package scripts
  and triggers off and have Skep run the known commands itself; **C**,
  re-run the scripts chrooted in a second pass, which pkg has no command
  for, so in practice it becomes B.

#### Decision on package scripts (accepted 2026-11-16)

Andy chose **B, with four changes**, and kept A's host-file check. This
is the specification of the pkgbase build. It supersedes the "Rules for
running `pkg`" in Decision 7a where they differ, and keeps every rule
there that it does not change (cleared environment, fixed arguments,
package names only from `storage.template_packages`, `PKG_CACHEDIR` and
`PKG_DBDIR` given with `-o`, cache emptied before `@skep-base`).

1. **No package code runs on the host.** `pkg` runs with
   `-o RUN_SCRIPTS=false -o PKG_TRIGGERS_ENABLE=false`. With triggers off,
   pkg neither runs nor saves (defers) triggers: `triggers_enabled()` in
   pkg 2.9.2's `triggers.c` checks both options.
2. **The known commands run inside a throwaway build jail on the new
   template, not in a plain chroot** (Andy's change 1). Its properties:
   - Name `skep-build-<template-id>`, `path=<template root>`, created
     only for the build and removed afterwards (`jail -r`) on success,
     failure, cancel, or timeout. The agent removes any leftover
     `skep-build-*` jail at start, before it serves its socket, and logs
     an event when it does.
   - **No network:** `ip4=disable`, `ip6=disable`, no `vnet`, no
     interfaces.
   - **No devfs, or the minimum:** first try without `mount.devfs`. If a
     command needs `/dev/null` or similar (record which one and why),
     mount devfs with a ruleset that unhides only `null`, `zero`,
     `random`, and `urandom`, and nothing else.
   - The hardened parameters of Decision 9 that apply: `enforce_statfs=2`,
     `children.max=0`, every `allow.*` off, `securelevel=2` unless a
     command needs less (record it), `exec.clean`. In the build jail
     "every `allow.*`" includes `allow.suser`, unlike an ordinary jail
     (amendment e); the probe showed no command needs it.
   - Started with `persist` so the commands can run one by one with
     `jexec -l` (cleared login environment). It never runs `/etc/rc`
     (no `exec.start`).
   - Same parent rule as the template: the parent of the template root is
     `root:wheel 0700`, checked before the jail starts
     (`jail_parent_insecure`).
   - The jail.conf for it is generated from typed values like Decision
     8's, or the parameters are passed as a fixed argument list to
     `jail -c`. Nothing in it comes from a request.
3. **The commands, fixed in Skep's code, run as root inside the build
   jail with the template's own binaries** (paths relative to the jail
   root, which is `/` inside it). They are the package scripts' commands
   with `${PKG_ROOTDIR}` empty and `PKG_METALOG` unset:

   | Package (its script) | Command run in the build jail |
   |---|---|
   | `FreeBSD-caroot` (post-install) | `/usr/sbin/certctl rehash` |
   | `FreeBSD-runtime` (post-install) | `/usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd` |
   | `FreeBSD-runtime` (post-install) | `/usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services` |
   | `FreeBSD-utilities` (post-install) | `/usr/bin/cap_mkdb -l /etc/login.conf` |
   | mandoc trigger (Andy's change 4) | `/usr/bin/makewhatis /usr/share/man` and, if it exists, `/usr/bin/makewhatis /usr/share/openssl/man` |

   About `services_mkdb`: the package script passes `${PKG_ROOTDIR}/`
   (the root directory) as the input file. Inside the jail, Skep passes
   `/etc/services`, the input services_mkdb(8) documents as its default.
   When this is first built, check that the resulting
   `/var/db/services.db` is identical to the one the traced build
   produced, which is kept on `hearst` in `skeptest/trace-7b` until then.
   (The probe showed it cannot be: the package script's database is
   empty. Skep keeps `/etc/services`; see
   [Resolved](#resolved-the-probe-of-the-specification-2026-11-16).)
   A command that fails fails the build, and its first line of stderr
   goes in the task error.
4. **Allowlist by package name and script hash** (Andy's change 2). After
   the install and before the build jail, Skep reads every installed
   package's scripts from the template's own database with a fixed
   command: `pkg --rootdir <root> -o PKG_DBDIR=<root>/var/db/pkg info
   --raw --raw-format json-compact -a`. For each package it takes every
   entry of `scripts` and `lua_scripts`, in every phase (install,
   deinstall, pre and post, upgrade), and computes the SHA-256 of the
   script body as that JSON gives it (the UTF-8 string, no trailing
   newline added). Every (package name, script kind, phase, SHA-256) must
   be on the allowlist. Anything not on it fails the build with
   `template_script_unknown`. The error names the package and phase and
   includes the script text, so the operator sees what changed. The
   allowlist is compiled into Skep, keyed by release major.minor; it is
   not configuration, because it is a security boundary. The 15.1 entries,
   from the traced build (the package versions are shown for reference;
   the key is the name and hash):

   | Package | Kind, phase | SHA-256 of the body | Version seen |
   |---|---|---|---|
   | `FreeBSD-caroot` | scripts, post-install | `5976bb9bdaf0cbb34807cdd7eb9b1b30e4913f92f4c2422c96caea0b381c8099` | 15.1p6 |
   | `FreeBSD-caroot` | scripts, post-deinstall | `5976bb9bdaf0cbb34807cdd7eb9b1b30e4913f92f4c2422c96caea0b381c8099` | 15.1p6 |
   | `FreeBSD-runtime` | scripts, post-install | `900cf7d3a93efcb3e894d1517d9d1c913932041ee97ad339adeed6f4ae9ce0f9` | 15.1p6 |
   | `FreeBSD-utilities` | scripts, post-install | `8620cabb4dfe8249fc0fe50ff62fc6e0cd4df957ac46b112098baa4ee63c698f` | 15.1p4 |

   The bodies hashed are, exactly:
   `/usr/sbin/certctl -D${PKG_ROOTDIR}/ ${PKG_METALOG:+-U -M $PKG_METALOG} rehash`;
   `pwd_mkdb -i -p -d  ${PKG_ROOTDIR}/etc ${PKG_ROOTDIR}/etc/master.passwd\n    services_mkdb -l -q -o ${PKG_ROOTDIR}/var/db/services.db ${PKG_ROOTDIR}/`
   (two spaces after `-d`; `\n` is a newline followed by four spaces);
   `cap_mkdb -l ${PKG_ROOTDIR}/etc/login.conf`. A unit test pins these
   hashes against these strings. A release without allowlist entries has
   no pkgbase source in the capabilities.

   **Trigger definition files are allowlisted too** (Andy, 2026-11-16).
   Triggers are off during the install, so a trigger whose work Skep does
   not reproduce would silently leave the template unfinished. After the
   install, Skep lists every regular file in the template's trigger
   directories, `<root>/usr/share/pkg/triggers/` and
   `<root>/usr/local/share/pkg/triggers/` (pkg's default
   `PKG_TRIGGERS_DIR`), reading them through the root's directory
   descriptor without following links. A symlink or any other non-regular
   entry there fails the build. Each file's (file name, SHA-256 of its
   bytes) must be on the same compiled, per-release allowlist. Anything
   else fails the build with `template_script_unknown`; the error names
   the file and includes its content. For 15.1 there is one entry:

   | Trigger file | SHA-256 | From package |
   |---|---|---|
   | `usr/share/pkg/triggers/mandoc.ucl` | `c6cc94249864d251d0a143b62c21019d5f4ab593299c55dccdb5f0574fdb1590` | `FreeBSD-mandoc` 15.1 |

   Each allowlisted trigger maps to the commands Skep runs for it in the
   build jail. `mandoc.ucl` maps to the `makewhatis` row of item 3.
5. **No deferred trigger is left in the template** (Andy's change 4).
   Before `@skep-base`, `<root>/var/db/pkg/triggers/` must be absent or
   empty; if it holds anything, the build fails (`template_trigger_left`)
   and the error lists the files. With triggers off it should never have
   any, and the check proves it. The mandoc trigger's work is done by the
   `makewhatis` row above.
6. **The release is never newer than the host's** (Andy's change 3).
   After the install, `freebsd-version -u` of the template (run inside
   the build jail, or read from `<root>/bin/freebsd-version` without
   running host code on it) must have the host's `freebsd-version -k`
   major.minor (Decision 7b) and must not be newer than the host's
   release. A newer one fails the build with `release_unsupported` and
   the template is destroyed. The same rule applies to the `dist` source.
   **Only major.minor is compared** (Andy, 2026-11-16): a template of
   `15.1-RELEASE-p7` on a host at `15.1-RELEASE-p6` builds; `15.2` or
   `16.0` on that host is refused. The template's full release, patch
   level included, is recorded in `observed_release`, and the UI shows it
   beside the host's. When the template's patch level differs from the
   host kernel's, in either direction, the UI flags it, the same way
   Decision 7b flags jails older than the host.
7. **A's host-file check is kept, at run time, on every build.** Before
   `pkg` starts and after the build jail is removed, the agent records,
   for every regular file and symlink under the host's `/etc`,
   `/var/db/pkg`, and `/usr/share/certs` except the exclusions below, its type, mode, owner, size,
   mtime, and the SHA-256 of regular files (about 1,500 entries on
   `hearst`, a few seconds). Any difference fails the build
   (`template_host_changed`), lists the changed paths in the task error
   and the audit entry, and emits an event. The template is destroyed;
   the host change cannot be undone and is reported, not repaired. The
   Decision 16 check repeats this from outside, as the trace did.

   **Exclusions (Andy, 2026-11-16).** The check leaves out the paths on an
   explicit list compiled into Skep, each one an exact absolute path. There
   are no wildcards and no directory exclusions: a listed path that is a
   directory is still walked. The list is not configuration, for the same
   reason as the allowlist. A path is added only with its reason recorded
   here. The list starts with one entry:

   | Path | Why it is left out |
   |---|---|
   | `/var/db/pkg/vuln.xml` | The vulnerability database. `periodic daily` (03:01 from `/etc/crontab`) runs `405.pkg-base-audit` and `410.pkg-audit`, which run `pkg audit -F` and download a new copy whenever it is older than two days (`security_status_*audit_expiry`). That rewrite has nothing to do with a build, and a build that overlapped it would fail with `template_host_changed`. Nothing in a build writes it: pkg runs with `PKG_DBDIR` inside the root. |

   What this costs: a build that did write an excluded path would not be
   caught by this check. Each entry must be a file the build has no
   reason to touch and that something else on the host is known to
   rewrite.
8. **The trace's housekeeping.** `TMPDIR=<work area>/tmp` is part of the
   fixed environment of `pkg` (the trace showed pkg writing its catalogue
   download temporaries to the host's `/tmp` without it).
   `PKG_CACHEDIR=<work area>/cache`, outside the root. `PKG_DBDIR=<root>/var/db/pkg`.
   Before `@skep-base`, Skep removes `<root>/var/db/pkg/repos/` (copies of
   every host repository's catalogue, ports included) and checks that
   `<root>/var/cache/pkg` is empty or absent. The work area is
   `<pool>/<root>/templates/.work-<template-id>` or a directory beside the
   template root under the same `0700` parent, and is removed after the
   build.

The build sequence, in order: check the parent directory; record the
host hashes; create the template dataset and work area; run `pkg install`
(scripts and triggers off, fixed environment and options); read and check
the scripts against the allowlist; check the release; start the build
jail, run the commands, remove the jail; check for deferred triggers;
remove the catalogues and check the cache; record the host hashes again
and compare; remove the work area; snapshot `@skep-base`; record the
release, packages, and size. A failure at any step removes the build jail
and work area and destroys the template dataset. Cancel and timeout (30
minutes) do the same.

New error codes: `template_script_unknown` (422), `template_trigger_left`
(500), and `template_host_changed` (500); `release_unsupported` and
`jail_parent_insecure` already exist in the text. The administration
guide and the security overview document all of this when it is built.

Three follow-up points, settled on 2026-11-16 (Andy answered the first
two and left the third to the implementation):

- **Patch level:** only major.minor is compared, and the patch level is
  shown in the UI (item 6).
- **Trigger definition files:** allowlisted by name and hash (item 4).
- **The build jail's shape: one persistent jail, with each command run
  through `jexec`.** The jail is created once with `jail -c` (the
  parameters of item 2 plus `persist`, and no `command`, `exec.start`,
  or `exec.stop`). Each command of item 3 runs as
  `/usr/sbin/jexec -l skep-build-<template-id> <command> <args>` (without
  `-U root`; see [Resolved](#resolved-the-probe-of-the-specification-2026-11-16)),
  in a fixed order: certctl, pwd_mkdb, services_mkdb, cap_mkdb,
  makewhatis. Then the jail is removed with `jail -r`. The removal sits in
  a deferred cleanup that uses its own short context, so a cancelled or
  timed-out task still removes the jail. Removal is then confirmed with
  `jls -j`, and a jail that will not go fails the task and is reported.
  Why one jail rather than one `jail -c ... command=` per command:
  - The parameters (no network, devfs or not, securelevel) are set and
    checked once, and every command runs in the identical environment.
    With five jails, a difference between them would be a bug that is
    hard to see.
  - There is one jail to clean up, under one known name, and one place
    that guarantees its removal. The agent's start-up sweep of leftover
    `skep-build-*` jails covers a crash at any point. Five short-lived
    jails mean five create and remove cycles, each one a chance to leave
    something behind or to collide with a half-removed predecessor of the
    same name.
  - Errors map cleanly. A failing `jexec` is one command's failure, with
    its own exit status and stderr, and the jail itself is unaffected. A
    failing `jail -c ... command=` mixes jail-setup failures with command
    failures in one exit status.
  - `jexec -l` gives each command a clean login environment, which
    matches `exec.clean` and the fixed-environment rule.

  What it costs: `persist` means the jail outlives a crashed agent until
  the start-up sweep removes it. Per-command jails would vanish on their
  own when their command ended. The sweep, and the fact that a build
  jail has no network, no `exec.start`, and nothing running between
  commands, make that acceptable.

The pkgbase source is built in step 6 to this specification, together
with the template store, the `dist` source, the
`container.template.create` and delete tasks, and the routes.

#### Resolved: the probe of the specification (2026-11-16)

A hand-run probe on `hearst` before building
([step6-probe.txt](../../tests/integration/jails/results/step6-probe.txt))
found the specification not buildable as written. Andy answered on
2026-11-16; the text above is amended accordingly.

- **What failed.** `jexec -l -U root` failed before `pwd_mkdb` had run
  (`getpwnam: root`): the template has no `pwd.db` yet, and FreeBSD's
  `files` passwd source reads nothing else. It also failed after that
  (`initgroups: Operation not permitted`), because item 2 turns every
  `allow.*` off, `allow.suser` included.
- **Decision: option A.** Every command runs as
  `/usr/sbin/jexec -l skep-build-<template-id> <command>`, without `-U`,
  in the fixed order, with every `allow.*` off in the build jail,
  `allow.suser` included. The command then runs with the agent's own
  credentials, uid 0 and gid 0 (what `-U root` would give), and gets root's
  login-class environment from `-l`. In a jail with `allow.suser` off that
  root has no privilege beyond owning the files. The probe ran all six
  commands this way with no devfs and `securelevel=2`.
- **Decision: `services_mkdb` keeps `/etc/services` as its input.** The
  `FreeBSD-runtime` 15.1 post-install script passes `${PKG_ROOTDIR}/`, a
  directory, as the input file; services_mkdb reads no entries from it and
  writes an empty database (2097920 bytes, 285 of them non-zero). The trace
  has that empty database, and so does `hearst`'s own base system. Skep's
  command builds the populated one, so the template's `services.db`
  differs from the trace's on purpose. **The empty database breaks no
  lookup on `hearst`** (checked read-only): `getent services ssh` gives
  `ssh 22/tcp` and `getent services domain` gives `domain 53/tcp`, because
  `nsswitch.conf` has `services: compat`, and in libc
  (`lib/libc/net/getservent.c`, releng/15.1) the `files` and `compat`
  sources read the `/etc/services` text; only the `db` source opens
  `/var/db/services.db`. A host configured with `services: db` would find
  nothing. An upstream report is Andy's decision, later.
- **Decision: Decision 9 is amended for step 7** (amendment e): ordinary
  jails keep `allow.suser` on; only the build jail turns it off.
- **Held as specified:** scripts and triggers off (nothing ran, nothing
  deferred); `TMPDIR`; every allowlist hash (the four script entries and
  `mandoc.ucl`); no devfs needed; removal confirmed with `jls -j`. Also
  learned: `pkg info --raw --raw-format json-compact -a` prints one JSON
  array, not one object per line.

#### Built: the template builder (2026-11-16)

Built to the specification above with the answers of "Resolved". Record:
[step6-build.txt](../../tests/integration/jails/results/step6-build.txt);
the choices the text left open are listed with their reasons in
[step6-progress.md](../../tests/integration/jails/results/step6-progress.md).
The ones that shape behaviour:

- **Layout.** `<root>/templates` is a mounted dataset (unlike
  `<root>/vm`), whose directory is the `root:wheel 0700` parent. The work
  area is a directory `<templates>/.work-<id>` in it, the second form item
  8 allows. A template is set `readonly=on` after `@skep-base`.
- **The build jail's `allow.*`.** The 19 `allow.*` parameters of the
  15.1 GENERIC kernel that are neither `allow.mount.*` nor added by a
  module are set to 0 explicitly (`allow.mount=0` covers the
  `allow.mount.*`; `allow.vmm` and the like default to off);
  `allow.reserved_ports`
  and `allow.unprivileged_proc_debug` default to on. After `jail -c`, every
  `allow.*` that `jls --libxo=json -n` reports must be `false`, together
  with the name, path, `ip4`/`ip6=disable`, `vnet=inherit`,
  `enforce_statfs=2`, `children.max=0`, `securelevel=2`. A jail that fails
  the check is removed and the build fails. No command needed devfs.
- **Which commands run.** Each allowlist entry maps to its commands (the
  table of item 3); a command runs only if an allowed script or trigger in
  the template asks for it, always in the fixed order.
- **Codes beyond the three.** `template_build_failed` (a pkg, tar, or
  build-jail command failed; the first line of its stderr, as item 3
  asks), `template_name_taken`, `template_not_provisioned`,
  `template_archive_mismatch` (the dist check). A source the host cannot
  use is `storage_feature_unsupported`. A symlink in a trigger directory
  is `template_script_unknown`. A failed build that also changed the host
  is `template_host_changed`, with the build's error in the details.
- **Events.** The agent has no event stream yet. A host change is in the
  task error, the audit details, and an error log line
  (`event=template_host_changed`). The start-up sweep logs a warning per
  jail removed (`event=build_jail_removed`).
- **Routes and permissions.** `GET /storage/template-sources` (what
  "capabilities" means for templates), `GET|POST /storage/templates`,
  `GET|DELETE /storage/templates/{id}`. Reads are `storage:read`, writes
  `storage:write`, which is now declared and enforced (admin only).
- **Not built in this step.** Templates in a **directory pool** are
  refused with `storage_feature_unsupported`: a build there needs its own
  layout and has no snapshot. The `dist` source is built and unit tested
  but not run on `hearst` (no `base.txz` there).

Validated on `hearst` through the builder's FreeBSD test binary with the
real host operations, not yet through a deployed agent:
`FreeBSD-set-minimal` 15.1-RELEASE-p6 in 36 s; host files identical by
the builder (764 entries) and from outside (the trace's method); pkg's
environment exactly `PATH`, `ASSUME_ALWAYS_YES`, `TMPDIR`; no build jail,
deferred trigger, catalogue, cache, or work area left. Against
`skeptest/trace-7b`, every difference has a named cause: two `mandoc.db`
files more (makewhatis ran), the catalogues and the deferred trigger fewer,
`services.db` populated instead of empty (intended), and `local.sqlite`
differing only in `packages.time`. A `mandoc.ucl` changed after the
install was refused with `template_script_unknown` and left nothing
behind.

Validated through the deployed agent and API on 2026-11-16
([step6-deployed.txt](../../tests/integration/jails/results/step6-deployed.txt)):
an admin's `POST /api/v1/storage/templates` built `FreeBSD-set-minimal`
in 33 s, comparing 763 host files (`vuln.xml` excluded), with the host
identical from outside. Its tree is byte-identical to the test binary's
template apart from `local.sqlite`'s install times. An operator's request
was refused 403 for `storage:write` and audited as `authz.deny`. A restart
with no build jail logs `build_jail_sweep found=0` before the socket is
served; every sweep now logs that line, so a clean one is visible.

### Step 7: jails (Decisions 8, 9, 13)

Started 2026-11-16. A hand-run probe on `hearst` first
([step7-probe.txt](../../tests/integration/jails/results/step7-probe.txt))
confirmed that a clone of a read-only template is writable and that
`/etc/rc` starts cleanly in a jail with the baseline, and found three
places where the text could not hold: `allow.reserved_ports`,
`allow.raw_sockets` in a VNET jail, and the missing sshd. Andy chose the
recommended options on 2026-11-16; they are amendments f and g above and
are worked into Decisions 9, 12, and 16. Build notes and the validation
follow in
[step7-progress.md](../../tests/integration/jails/results/step7-progress.md).

#### Built: jails (2026-11-16)

Built to Decisions 8, 9, and 13 with amendments e, f, and g. The choices
the text left open are listed with their reasons in
[step7-progress.md](../../tests/integration/jails/results/step7-progress.md).
The ones that shape behaviour:

- **Layout.** `<root>/jail` is a mounted dataset whose directory is the
  `root:wheel 0700` parent of every jail root, like `<root>/templates`. A
  jail root is `zfs clone` of `@skep-base` with `skep:container_id` and
  `readonly=off` set explicitly. The jail.conf and console.log live in
  `/var/run/skep/jails/<id>/` (the agent's `RunDir` is the VM directory),
  checked to be the agent's own and not writable by others.
- **The parameters are checked after `jail -c`**, as for the build jail:
  name, path, hostname, `vnet=new`, `persist`, `enforce_statfs`,
  `children.max`, `securelevel`, `devfs_ruleset=5`, `sysv*`, and every
  `allow.*` the kernel reports (suser and reserved_ports on, mlock as
  asked, the rest off). A mismatch removes the jail
  (`container_start_failed`).
- **The writer** opens one component at a time with `O_RESOLVE_BENEATH |
  O_NOFOLLOW` relative to the jail root's descriptor, replaces files by a
  new file and `renameat`, and touches only Skep's own `etc/rc.conf`
  (recognised by its header) and `root/.ssh/authorized_keys`. It uses
  `golang.org/x/sys/unix`, already a dependency; no new one.
- **`started_once`** is set before `jail -c`. Hostname and options are jail
  parameters and can change whenever the jail is stopped; the SSH key is a
  write into the tree and is `container_started` after the first start.
- **`mlock`** is refused (`mlock_requires_memory_limit`) until limits exist
  (step 9). `container:snapshot` stays undeclared until its routes exist.
- **New codes** besides the ADR's: `container_name_taken`,
  `container_not_stopped`, `container_not_running`,
  `container_not_provisioned`, `container_started`,
  `container_start_failed` (all 409).
- **No NIC.** A step 7 jail is VNET with only `lo0`; epairs are step 8.

Validated on `hearst` through the deployed agent and API (`57d74bb`;
[step7-validation.txt](../../tests/integration/jails/results/step7-validation.txt)):
a clone of the read-only template is writable; `/etc/rc` starts cleanly;
the kernel reports the baseline; root binds port 22; raw sockets work and
are recorded (amendment f); mount, kldload, sysctl writes, sethostname,
lowering the securelevel, and clearing `schg` fail; an SSH key works on a
template with sshd (login over the jail's `lo0`) and is refused with
`container_ssh_unavailable` on one without; two jails from one template
have different host keys; a key change after the first start is
`container_started`; the jail and template parents are `root:wheel 0700`
and an unprivileged host user cannot reach a setuid file in a running
jail; the operator's and viewer's allowed and refused requests match
Decision 14, with every refusal audited; jails survive an agent restart
and `kill -9`; stop leaves no jail, mount, or interface behind and delete
no dataset. The writer's native tests pass, including planted links.
Not run on `hearst`: the `chmod 755` refusal of the parent (it changes a
host directory's mode, outside the run's approvals; unit tested), and
the epair clean-up (step 8).

### Step 8: jail networking (Decision 10)

Started 2026-11-17. A hand-run probe on `hearst` first
([step8-probe.txt](../../tests/integration/jails/results/step8-probe.txt))
confirmed the epair creation, description, renaming, `vnet.interface`,
and teardown as written, and found that DHCP cannot work with ruleset 5:
`dhclient` needs `/dev/bpf` ("Can't find free bpf"). Andy chose option A1
on 2026-11-17 and approved the two smaller proposals; they are amendment
h above and are worked into Decisions 9, 10, and 16. Build notes and the
validation follow in
[step8-progress.md](../../tests/integration/jails/results/step8-progress.md).

**Resolved (2026-11-17, from the step 8 validation; amendment i).**
Ruleset 6001 as first written in amendment h (`include 5` plus `bpf*`)
exposed host devices: devfs does not resolve the includes of an included
ruleset, and ruleset 5 is only includes plus `path pf unhide`. Found on
`hearst` with the first DHCP jail (its `/dev` held `mem`, `kmem`, `io`,
`da0`, ...), which was stopped and deleted at once; the ruleset was
removed and the previous build redeployed
([step8/devfs-include.txt](../../tests/integration/jails/results/step8/devfs-include.txt)).
Andy's answers are amendment i below the list of amendments, worked into
Decisions 9, 10, and 16.

**Validated on `hearst` (2026-11-17, `79620ef`)**, spoofing excluded: the
"Jail devfs" check of Decision 16 (a real DHCP jail and a no-NIC jail,
which differ only by `bpf` and `bpf0` and hold nothing outside the
allow-list; a refusal triggered on a real jail), rc.shutdown on stop and
after an rc timeout, SSH from `merrick`, isolation (TCP into the private
ranges dropped; ICMP echo into 10/8 passes the LAB rules), epair cleanup,
the start-up sweep after `kill -9`, and an orphaned epair reported and
cleaned. Static addressing on `hearst` stays skipped. Results in
[step8-progress.md](../../tests/integration/jails/results/step8-progress.md),
section 7.

### Step 9: limits, quotas, snapshots, clones (Decisions 11, 2, 3)

Started 2026-11-17. **Built:** the RCTL limits and the disk quota of
Decision 11 (`memoryuse`, `pcpu`, `maxproc`, `openfiles` deny rules on
`jail:skep-<id>`, added after `jail -c` and read back before the `/dev`
guard and `/etc/rc`, removed at every removal; `refquota` on the jail
root; `rctl_unavailable`; `mlock` with a memory limit), and, after
amendment k, the fixed in-jail names, jail snapshots and clones, and the
static-address rules (below).

**`memoryuse` deny is not a hard limit** (the check of Decision 11, on
`hearst`, `a3ef267`): a jail with `memoryuse` 256 MiB held 514 MiB
resident for 19 s with nothing refused or paged out, the host having
free memory. `vmemoryuse` deny refuses at allocation but counts address
space (about 3x resident for `dd`). The memory limit is therefore not
verified as a cap; Andy chose to keep it as a documented soft limit
(amendment j, Q4 in
[step9-progress.md](../../tests/integration/jails/results/step9-progress.md)). `pcpu`, `maxproc`, `openfiles`, and `refquota` were verified
on `hearst`.

**After amendment j (`4c7d8e0`, on `hearst`).** `mlock` needs
`limits.locked_memory` (RCTL `memorylocked`); the rule is applied and read
back, and refusing an `mlock` past it is pending live validation (no
program in the minimal template calls mlock(2)). A longer `memoryuse`
run (256 MiB limit, 512 MiB held for 3 minutes) was never refused; after
about 80 s the jail's resident memory fell about 0.85 MiB/s (to 425 MiB at
185 s), with no swap used and no pageouts: vm_daemon moves the pages of a
jail over its limit to the inactive queue, so they are reclaimed first
under memory pressure. That is what the docs call soft. RCTL `openfiles`
counts descriptor-table slots, not open descriptors (each process is
charged 40, 128, 256, ... as its table grows, and Capsicum helpers such
as `tail`'s count too), which is why 59 of 300 opens succeeded under 256.

**Amendment k, built (2026-11-17).** The probe on `hearst` came first
([step9-progress.md](../../tests/integration/jails/results/step9-progress.md)
section 12): an abnormal removal of two jails returns two host interfaces
both named `skep0`, which 15.1 allows; host ends keep their descriptions,
destroying a host end removes only its own peer, and `jail -r` with
`vnet.interface` runs no `-vnet` command, so a renamed interface never
fails a removal. Built: `finishStart` renames each jail end to
`skep<slot>` with `ifconfig -j` after the move-in check and before the
`/dev` guard (a failed rename removes the jail without running its code);
Skep's `rc.conf` lines name `skep<slot>`; the API's `nics[].interface` is
the in-jail name. Of amendment k's two options for a normal stop, every
removal Skep makes **destroys the epairs before `jail -r`** rather than
renaming back: it does not depend on the jail's interface list, which the
jail's root controls, and no jail end returns to the host. The jail's
`/etc/rc.shutdown` runs without its NICs (a rename-back would have taken
`skep0` away at the same point).

**Jail snapshots and clones (Decisions 2, 3, 13; amendments j, k).**
`container.snapshot.create/rollback/delete` reuse the VM snapshot calls
with the container owner; `container.clone` uses `hostops.CloneJail`
(`zfs clone -o canmount=on -o readonly=off -o skep:container_id=<new>`,
mount checked) and sets the disk quota again. A clone inherits its
source's `started_once`; a rollback never resets it. Clone needs
`container:write` and `container:snapshot`. Static addresses: a clone must
give new addressing for each static NIC of the source
(`clone_addressing_required`, 422); new addressing is written into the
clone's tree only when the source never started, otherwise
`container_started`, so **a started jail with a static address cannot be
cloned** (Q8 in the progress log). A start is refused with
`container_address_in_use` when a running Skep jail on the same bridge
holds one of its static addresses (as Skep wrote them).


**Amendment l, the leftover devfs, built (2026-11-17).**
`hostops.removeLeftoverDevfs` reads the mount table with getfsstat(2)
(`MNT_NOWAIT`, read again if the buffer filled), keeps only entries whose
`f_mntonname` is byte for byte `<root>/dev` (root the jail's expected
mountpoint, a clean absolute path) and whose `f_fstypename` is `devfs`,
and unmounts each, newest first, with `unmount("FSID:<a>:<b>",
MNT_BYFSID)`; an entry with a zero fsid is refused. `StartJail` calls it
after the jail.conf is written and before any epair or `jail -c`;
`DestroyJailRoot` after the ownership and clone checks, before `zfs
destroy` (a failed unmount stops the delete); `Jails.RemoveLeftoverDevfs`
(refused while the jail runs) is called by `containers.ObserveAll` for
every provisioned jail that does not run. Each unmount is logged as
`orphaned_devfs_removed`. The symbolic-link requirement is tested twice:
a unit test with a fake table, and a native test on `hearst` that reads
the real table with a root whose `dev` links to `/dev` and to `/`, with
an unmount that fails the test if called.

**Amendment o, built (2026-11-18, merrick only).** SR-01: `/etc/rc` gets
a pipe (`EnvCommand.ConsoleLog`, `hostops.runToConsoleLog` in
`exec_unix.go`); `console.log` is truncated when rc is about to run, the
first 256 KiB are written as they arrive, the rest is counted with the
last 64 KiB kept, and `skep: N bytes discarded` plus the tail are written
when rc ends. The read end is read for at most 100 ms after jexec exits
and then closed. The jail.conf has no `exec.consolelog`; `jail -r` runs
with `EnvCommand.MarkOverflow` (4 MiB, a marker, never an error for its
size), and its output is not kept. SR-04:
`containers.max_locked_memory_bytes` (0 = `hw.physmem`/4, read at agent
start); `container_locked_memory_above_max` (422) at create, update, and
start; `container_locked_memory_exhausted` (409) at submission and in the
start task, which reserves its share in memory until the jail runs or
the start fails. Details and the hearst checks are in section 15 of
[step10-progress.md](../../tests/integration/jails/results/step10-progress.md).

**Amendment p, built (2026-11-18, merrick only).** `jail -r` runs with
`MaxOutput` 32 KiB (`ShutdownLogPart`) and `MarkOverflow`, which now keeps
the first and the last `MaxOutput` bytes of standard output in memory,
with `skep: N bytes discarded` between them (the console log's layout,
through the same `consoleLog` writer over a buffer). When the jail.conf
had `exec.stop` (a stop, or the removal after a failed rc; not a discard),
`removeJail` appends `--- shutdown ---` (on a line of its own) and that
output to `console.log`: `O_APPEND|O_CREATE|O_NOFOLLOW|O_NONBLOCK`, 0600,
regular files only; a failure is logged as `jail_shutdown_log_failed` and
never fails the removal. Only standard output is appended: the runner
returns no stderr on success. With the new `EnvCommand.StderrLastLine`,
the runner keeps the last 64 KiB of stderr and a failure's `CommandError`
carries its last non-empty line, with C0, DEL, and C1 controls and
non-UTF-8 bytes removed, cut to 200 bytes on a character boundary;
`removeJail` logs it as `jail_output` (`jail_remove_failed`) and returns
the error with the line as `jail output, last line of stderr: "..."`, or
`no jail output on stderr`. Details in section 17 of
[step10-progress.md](../../tests/integration/jails/results/step10-progress.md).

**Amendment q, built (2026-11-21, merrick only).** `jail -r` also runs
with the new `EnvCommand.StderrTail`: on success the runner writes the
last `MaxOutput` (32 KiB) bytes of stderr to it, preceded by `skep: N
bytes discarded` (as the first line, without the leading empty line) when
more was written; nothing on a failure, and a failed write to it is not
an error. `removeJail` appends, after the stdout section, `--- shutdown
stderr ---` (on a line of its own) and that tail, in the same single
write and under the same failure rules; both marker lines are written
even when their part is empty. A failed `jail -r` still appends nothing.
`cleanLine` also removes category Cf (`unicode.Cf`), so the stderr line
of a failed `jail -r` in the task error and in `jail_remove_failed` has
no bidi overrides or isolates. The console log itself is not cleaned:
both of its shutdown sections keep the jail's bytes, like rc's output
before them. Details in section 19 of
[step10-progress.md](../../tests/integration/jails/results/step10-progress.md).
