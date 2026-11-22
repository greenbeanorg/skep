# zvol device-node fix (ADR 0010 amendment z): FreeBSD validation, 2026-11-22

Branch `fix/zvol-device-node` (PR #55). Hosts: smith (10.79.20.81, UFS root,
data pool `tank` on da1) and langrishe (10.79.20.82, ZFS root, `zroot`), both
rolled back by Andy to Proxmox snapshot `fresh-151`, FreeBSD 15.1-RELEASE-p7.
hearst was powered off and not contacted. Andy away; run unattended. Logs are in
`~/skep-test-logs/` on merrick. Nothing merged.

(An earlier run the same day could not validate anything: the VMs were not at
`fresh-151` and hearst was unreachable. This file replaces its record.)

## Part 1: clean?

| Check | smith | langrishe |
|---|---|---|
| /var/db/skep | absent | absent |
| skep account | absent | absent |
| skep package | absent | absent |
| Skep bridge | absent (vtnet0 vtnet1 lo0) | absent (vtnet0 vtnet1 lo0) |
| pools | none (zfs.ko not loaded) | zroot only |
| rc.conf / loader.conf | no Skep lines, no `zfs_enable` | no Skep lines |
| uptime | 2 min (booted 18:11 local) | 1 min |
| Result | **clean** | **clean** |

Then, on smith only: `zpool create tank da1` (da1 blank: `gpart: No such geom`).
As in the clean-install run, this loaded zfs.ko itself; no explicit kldload.
`zfs version` on both: `zfs-2.4.2-1-FreeBSD_g6330a45b0`,
`zfs-kmod-2.4.2-1-FreeBSD_g6330a45b0`.

Logs (copies in [zvol-device-node-20261122/](zvol-device-node-20261122/)): 20261122T231318Z-zvol-part1-clean-check.log,
20261122T231332Z-zvol-smith-zpool-create.log.

## Part 1b: code checks before the probe

**a) What `ownedVolume` parses (langrishe, real output).** It runs
`zfs list -H -p -t filesystem,volume -o <21 columns>` (not `zfs get`). For an
unencrypted zvol `zroot/zvolprobe/plain0` (`-s -V 1G -o volblocksize=16K -o volmode=dev`):

```
zroot/zvolprobe/plain0	volume	1073741824	57344	57344	0	dev	16384	669	-	-	off	-	on	1.00	probe	d0	-	-	-	-
```

keystatus is `-`, which `dash()` turns into `""`; VolMode is `"dev"`. The repair
check (`KeyStatus != "" && KeyStatus != "available"`) does not refuse it, so there is
**no storage_locked refusal and nothing to fix.** Added anyway:
`TestEnsureVolumeDeviceRepairsRealUnencryptedOutput`, which parses exactly this line
and runs a repair on it. Log: 20261122T231422Z-zvol-part1b-a-zfs-list.log.

**b) Host state before `ensureDevices` in bhyve `Adapter.Start`.** Before it:
`unavailable` (capabilities), `Validate`, `Inspect` (reads the run state file, looks
for the runner and the vmm instance), `spec` (bridge lookup, media path), and
`PortFilterAvailable` (`kldstat -q -m` only). **None creates host state.** The run
directory, run files, and the runner, which creates taps and port filters, all come
after `ensureDevices`. Start **never removes a leftover vmm instance:** `Inspect`
reports one as `failed`/active ("force-stop the VM to destroy it") and Start refuses
with `vm_not_stopped`. **Nothing changed.**

**c) `toggleVolMode` when `volmode=none` failed and `volmode=dev` was set back.** It
returned a plain wrapped error (no code). It now returns `disk_device_missing` with
reason `repair_failed` and the `zfs` error in details `error`.
`TestEnsureVolumeDeviceRestoresDevWhenNoneFails` checks the code, the reason, the
dataset, and the error. (Fix 1 below.)

**d) `hv.Inspect` for a clone target that is not yet provisioned.** `hv` is the bhyve
adapter. The target has no run state file (`ReadRunState` returns not found, not an
error), no runner, and no vmm instance, so `Inspect` returns `State: stopped,
Active: false`. The stopped check passes, and the clone's zvols carry the target's
`skep:vm_id` and `skep:disk_id` (hostops `CloneVM`), so `ownedVolume` accepts them:
**the post-clone repair can run.** Part 3's clone loop measures it.

Tests: `go test -race ./internal/hostops ./internal/vm ./internal/hypervisor/bhyve -run
'EnsureVolumeDevice|EnsureDevice|DeviceNode|Start'` PASS,
20261122T231518Z-go-test-hostops-zvoldev.log.

## Part 2: raw zfs probe

Script [probe.sh](zvol-device-node-20261122/probe.sh), run as root in
`tank/zvolprobe` (smith) and `zroot/zvolprobe` (langrishe). Every zvol was created
like the agent creates one: `zfs create -s -V 1G -o volblocksize=16K -o volmode=dev`.
"Toggle" means `zfs set volmode=none`, then `zfs set volmode=dev`, then watching for
the node for up to 10 s. Nothing ever wrote `volmode=geom`. `vfs.zfs.vol.mode` was the
default (1). After each part, `kern.geom.conftxt` had **0** lines with `zvol` on both.

| Probe | smith (`tank`, da1) | langrishe (`zroot`) |
|---|---|---|
| a) 50 creates, one per second: no node 1 s after create | **45/50** (also 45 at 0 s) | **43/50** (also 43 at 0 s) |
| a) missing nodes that appeared within 30 s (≥ 31 s after create) | **0 of 45** | **0 of 43** |
| b1) toggle, plain zvols missing a node | 45/45 node back | 43/43 node back |
| b2) 20 `zfs clone -o volmode=dev` of a zvol snapshot: no node | **17/20**; toggle 17/17 back | **20/20**; toggle 20/20 back |
| b3) 20 rollbacks (snapshot, 1 MiB written through the node, `zfs rollback`) of zvols that had a node: no node after | **0/20**; toggle on a present one: back | **0/20**; toggle on a present one: back |
| b4) 20 zvols in an encrypted dataset (aes-256-gcm, raw key, `keystatus` available): no node | **19/20**; toggle 19/19 back | **14/20**; toggle 14/14 back |
| node visible after `volmode=dev` (all toggles) | first look (< 0.1 s), 82/82 | first look, 78/78 |
| c) toggle with a reader (dd) holding the node open | see below | same as smith |
| d) GPT zvol after the toggle: `…/g0p1`, `kern.geom.conftxt` | **no p1, 0 lines**, `gpart: No such geom` | **no p1, 0 lines**, `gpart: No such geom` |
| e) `zpool export tank; zpool import tank` with 50 zvols, all with nodes | 3 repeats: **50/50, 50/50, 50/50** nodes at 0 s, 10 s, and 30 s after import (0 after export) | not run (smith only) |

Under `volmode=none` the node was gone every time, as expected.

**c) The toggle with the node held open.** It does **not fail**: `zfs set
volmode=none` returns 0 with dd reading the node, the node disappears, and the reader
is gone at once (its file descriptor too). With dd started directly it ends with exit
status 143 (SIGTERM) and prints no read error; under `timeout(1)` the status was 124.
What sends the signal was not identified. `volmode=dev` then brings the node back
(rc 0). So the toggle **pulls the node away from a running consumer**; a repair while
a VM runs would take the disk away from bhyve. That is what the stopped check and the
vmm-instance check of rule 3 prevent. Runs: probe.sh `c`, then
[probe-c3.sh](zvol-device-node-20261122/probe-c3.sh) (dd started directly, SIGINFO
before and after, `procstat -f`); the middle run's `ps` format was wrong and is kept
only for the record.

**d)** A 64 MiB GPT image (one 20 MiB freebsd-ufs partition, built with `sfdisk` on a
file on merrick) was written to `g0` through its node (sha256 identical), then the
toggle: no `/dev/zvol/<pool>/zvolprobe/g0p1`, nothing naming `g0` or `zvol` in
`kern.geom.conftxt`. (`g0` itself had no node after create on both hosts and was
toggled first.)

**What it means for the fix.** None→dev brought the node back in every case
measured (179 missing nodes on the two hosts, plus 3 present ones), never exposed a
zvol to GEOM, and the node was there at once, well inside the 10 s the code waits.
The miss rate on fresh hosts is far higher than the 2/5 seen before (90 % and 86 %).
Rollback did not lose a node, and import creates all of them. Cleanup:
`zfs destroy -r <pool>/zvolprobe` on both, the key file removed
(20261122T233746Z-zvol-part2-cleanup.log).

## Part 3: validation through the agent

**Install: done on both.** `make stage` at 64de929, `pkg create` on smith (staged
sha256 matched `SHA256SUMS.stage`; `skep-0.1.0.pkg` sha256 `4e64d49d…0208`), copied
to langrishe. `pkg add`, then `skep-setup apply -mgmt vtnet0 -pool tank|zroot
-no-racct -start -admin admin -yes` under `timeout -f` (password typed over a pty from
the creds file, never printed). Both services running; `skepctl login` with the pin
from `skep-api tls fingerprint`. Logs: *-zvol-build-pkg.log,
*-zvol-smith-install-apply.log, *-zvol-langrishe-install-apply.log.

**Through the API: blocked.** The first `vm.create` (one 64 MiB thin disk, no NIC)
failed: `bhyve_unavailable: the UEFI variable store template BHYVE_UEFI_VARS.fd is not
in the firmware directory`. That comes from the `edk2-bhyve` package, which
`fresh-151` doesn't have and this run was not approved to install. A VM start also
needs `vmm.ko`, and kld changes were excluded. The failed create was undone (no VM
row, no zvol). So **none of the API steps ran:** the 50/20/20 loops through
`vm.create`, `vm.clone`, and `vm.snapshot.rollback`, the real VM started from a
disk whose node was missing, and `vm.start` refused with `disk_device_missing`.
They stay **pending FreeBSD validation** and need `pkg install edk2-bhyve` and
`kldload vmm` (or `vmm_load` and a reboot) on the lab VMs.

**What ran instead: the agent's hostops code on real ZFS.**
[tests/integration/storage/zvoldev](../../storage/zvoldev/main.go), cross-compiled and
run as root on both VMs. It builds the real FreeBSD `hostops` (the agent's own code,
`/sbin/zfs` through its runner) with `DatasetRoot: zvolprobe`, so everything was under
`<pool>/zvolprobe`, destroyed at the end. It calls `Disks.Create`, `ZFS.CloneVM`,
`ZFS.Snapshot`, and `ZFS.Rollback` as the VM service does, each followed by
`Disks.EnsureDevice` with a stopped check that says stopped, as the VM service calls
it after those operations. It does **not** exercise the API, the VM service's task
code, bhyve's `Adapter.Start`, or a guest. The clone and rollback source was a
64 MiB disk holding the GPT image (sha256 identical after the write, then
snapshotted); before every rollback, 1 MiB of random data was written into it.

| Through hostops | smith (`tank`) | langrishe (`zroot`) |
|---|---|---|
| 50 disk creates: node at the end / needed a repair | **50/50** / 47 | **50/50** / 45 |
| 20 clones of the GPT disk: node at the end / needed a repair | **20/20** / 4 | **20/20** / 7 |
| 20 rollbacks of the GPT disk: node at the end / needed a repair | **20/20** / 0 | **20/20** / 0 |
| repairs: rounds, time (incl. the 5 s wait for a late node) | all 1 round; 5.06–5.16 s | all 1 round; 5.05–5.14 s |
| check (d) after the clones and after the rollbacks: `p1` nodes / `kern.geom.conftxt` lines with `zvol` | 0 / 0, both times | 0 / 0, both times |
| refusal, VM not stopped (stopped check says running) on a disk with no node | `disk_device_missing`, reason `vm_not_stopped`; volmode still `dev`, node still missing (no `zfs set`) | same |
| refusal, volmode set to `none` by hand on a stopped VM's disk | `disk_device_missing`, reason `volmode_none`, `command: zfs set volmode=dev tank/zvolprobe/vm/da3bf3fa-…/disk0`; volmode still `none` | same, `zroot/zvolprobe/vm/30ee8c6c-…/disk0` |
| after running that command by hand | volmode `dev`, node back | same |

Every repair and refusal was in hostops' log (52 "repaired" lines on smith = 47 + 4 +
the source; 53 on langrishe), the refusals at WARN with the reason. A clone made
through `CloneVM` (a filesystem clone, then the volume clone) missed its node far less
often (4/20, 7/20) than a bare `zfs clone` of a zvol in Part 2 (17/20, 20/20); the
reason was not investigated.

## Fixes made during the validation

1. `toggleVolMode`: a failed `volmode=none` with `volmode=dev` set back is
   `disk_device_missing`/`repair_failed` with the error in details (Part 1b c, asked for).
