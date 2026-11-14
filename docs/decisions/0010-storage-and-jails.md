# ADR 0010: Storage management and jails (Milestone 7)

- Status: **Proposed**. Waiting for Andy's answers to the
  [questions for review](#questions-for-review). Not to be merged until
  they are recorded here and the status is Accepted.
- Date: 2026-11-14 (proposed)
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
| 4 | Disk changes after creation | Add a disk and grow a disk while stopped; never shrink; removing a disk destroys its zvol and needs `vm:delete` |
| 5 | Encryption, quotas, compression | Skep sets none of them on VM storage; it honours what the operator set on `<pool>/<root>` and refuses to use a dataset whose key is not loaded; a jail gets a `refquota` |
| 6 | Hosts without ZFS (UFS-root equivalence) | A `directory` pool type: an allow-listed directory, sparse raw files for VM disks, plain directories for jail roots; snapshots and clones are reported unsupported on it, never emulated |
| 7 | Jail userland and templates | A template is a ZFS dataset snapshot (or a directory) of a FreeBSD userland built by a task from the host's own pkgbase repository configuration, or from an operator-placed `base.txz`; the release must equal the host's; no URL from a request |
| 8 | How jails run | A generated per-jail `jail.conf` under the run directory and `jail(8)` with explicit arguments; no runner process; observed with `jls --libxo=json`; never `rc.conf`'s `jail_list` |
| 9 | Jail parameters | A fixed hardened baseline (`enforce_statfs=2`, `children.max=0`, no `allow.*`, `securelevel=2`); a short typed list of opt-ins; never `exec.prestart`, `exec.poststart`, or any host-side command from a request |
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

## Decision 4: disk changes after creation

| Change | Recommendation |
|---|---|
| Add a disk | `vm.update` while stopped, with the same rules as at create (slot, bus, size, thin, block size); `vm:write` |
| Grow a disk | `vm.update` while stopped; the new size must be larger and a multiple of the block size; a full-reservation disk checks `available` space first, as at create; the guest grows its own partition and filesystem afterwards, which the UI says |
| Shrink a disk | Never. ZFS would allow it and the guest's filesystem would be cut off |
| Remove a disk | `vm.update` while stopped, with `vm:delete` as well as `vm:write`, because it destroys the zvol; refused while the VM has snapshots that contain the disk unless the request also says `"destroy_snapshots": true`; the audit entry names the dataset and size |
| Change bus, block size, or thin/full | Not offered. Block size cannot change; changing thin to full is a reservation change that can fail for lack of space and is not worth its edge cases yet |

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

**Recommendation: A.** Rules: a directory pool must be on a local
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
in its capabilities. Package scripts run inside `pkg --rootdir`; the
packages come from the operator's signed repository, which is the trust
the host already places in its own updates.

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
`sysvipc` (`sysvshm`, `sysvsem`, `sysvmsg` = `new`), `allow_chflags`,
`mlock`. Not offered: `allow.mount.*`, ZFS dataset delegation
(`jailed=on`), `allow.vmm` (bhyve in a jail), and Linux ABI; each is its
own decision later.

`securelevel=2` has a cost worth naming: inside the jail, `schg` flags
cannot be cleared, so updating the base system from inside the jail
(`pkg upgrade` of `FreeBSD-*` packages) fails. That is intended while
userland updates are not designed (Decision 7b); packages from ports
install and upgrade normally. Review question 8 asks whether to keep it.

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
| Disk add, grow, remove | A second disk appears in the guest; a grown disk is grown with `gpart resize` and `growfs` in the guest; removal is refused for an `operator` without `vm:delete` |
| Directory pool | A VM installs and boots from a file disk in a directory pool on `hearst`'s UFS root; snapshot and clone are refused with `storage_feature_unsupported` |
| Template (pkgbase) | `container.template.create` builds a 15.1 userland from `FreeBSD-base`; the release recorded is `15.1-RELEASE-pN` |
| Template (dist) | A `base.txz` whose SHA-256 does not match `MANIFEST` is refused; a matching one builds |
| Jail lifecycle | Create, start, stop, restart, delete; `jls` shows it under `skep-<id>`; it survives `service skep_agent restart` and `kill -9` of the agent |
| Jail networking | DHCP address on VLAN 60, internet reachable, `10.79.20.0/24` not reachable; the host epair has the `skep:` description and is gone after stop |
| Jail baseline | Inside the jail: `sysctl security.jail.param` shows the baseline; `mount`, `kldload`, and raw sockets fail; with `allow_raw_sockets`, `ping` works |
| Limits | With RACCT on: a jail with `memoryuse` 256 MiB cannot allocate past it; `pcpu` 50 throttles a busy loop; with RACCT off, a limit is refused with `rctl_unavailable` |
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
  `storage.dist_dir`, `storage.template_packages`,
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

6. Templates from pkgbase and dist (Decision 7). Validation: a template
   builds on `hearst`.
7. `hostops` Jails interface, the conf generator, the baseline, the
   `openat` writer; `container.create`, `start`, `stop`, `restart`,
   `delete` (Decisions 8, 9, 13).
8. VNET networking (Decision 10). Validation: DHCP and isolation.
9. RCTL limits and quotas (Decision 11); jail snapshots and clones,
   reusing step 2.
10. The UI's Containers section and `skepctl container ...`; the 7b check
    table, docs, and implementation notes. **7b merges here.**

## Changes hearst needs

None of these is applied before acceptance. Each needs Andy's approval;
they are listed in the order the build needs them.

**On hearst, `/etc/rc.conf`** (persistent)

1. `kld_list="if_epair"` (add to any existing list), for jail VNET
   (Decision 10). Needed from step 8.

**On hearst, `/boot/loader.conf`** (boot configuration; the standing
instructions say never to touch it without approval)

2. `kern.racct.enable="1"`, then a reboot, for RCTL limits and jail
   usage (Decisions 11, 15). Needed from step 9. Without it, the limit
   checks are recorded as pending and only the `rctl_unavailable`
   refusal is verified.

**On hearst, files and datasets**

3. A directory pool for Decision 6 at `/var/tmp/skep/dirpool`
   (`root:wheel 0700`), on the UFS root beside the existing
   `skeptest.img`, listed in `storage.directories` of the test
   installation. About 10 GiB of sparse files at most; 93 GiB are free.
   Needed from step 4.
4. Optional: an encrypted test dataset root on `skeptest`
   (`skeptest/skep-enc`, `keyformat=passphrase`), to check the
   `storage_locked` refusal with the key unloaded. Inside `skeptest`, so
   within the pool Skep may touch, but a key and a second dataset root
   are new state, so it is listed here.
5. Outbound HTTPS from `hearst` to `pkg.FreeBSD.org` during template
   builds (already used by `pkg` on the host; if Pi-hole blocks it, the
   build stops and the `dist` source with a copied `base.txz` is used).

**On the Proxmox host**

6. A Proxmox snapshot `pre-m7` of `hearst` before step 2 (Skep will not
   rely on it).

## Questions for review

1. **Two phases** (7a storage, 7b jails), each merged after its own
   validation: agreed?
2. **Snapshots while running**, labelled crash-consistent (Decision 2a),
   or only while stopped?
3. **Rollback to an older snapshot** only with an explicit
   `destroy_newer` (Decision 2c): agreed?
4. **Linked clones only** in this milestone, full copies with the backup
   milestone (Decision 3)?
5. **Encryption left to the operator** at `<pool>/skep`, with no Skep key
   store yet (Decision 5)?
6. **Directory pools** for hosts without ZFS, with snapshots and clones
   reported unsupported (Decision 6), or defer UFS-only hosts again?
7. **Jail userland sources**: pkgbase from the host's repositories and an
   operator-placed `base.txz`, with the release pinned to the host's
   (Decision 7). Is the root agent fetching from the host's own `pkg`
   repositories acceptable?
8. **The jail parameter baseline and the four opt-ins** (Decision 9):
   anything to add or remove?
9. **No jail console**, SSH key at create instead (Decision 12), or do
   you want the `login(1)` console in this milestone?
10. **Permissions** (Decision 14), in particular `storage:write` as
    admin-only.
11. **Monitoring as current values only** (Decision 15)?
12. **The hearst changes** above: which are approved, especially
    `kern.racct.enable` in `loader.conf` with a reboot, and the
    directory pool on the UFS root?
