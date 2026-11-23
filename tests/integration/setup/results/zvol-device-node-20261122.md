# zvol device-node fix (ADR 0010 amendment z): FreeBSD validation, 2026-11-22

Branch `fix/zvol-device-node` (PR #55). Hosts: smith (10.79.20.81, UFS root,
data pool `tank` on da1) and langrishe (10.79.20.82, ZFS root, `zroot`), both
rolled back by Andy to Proxmox snapshot `fresh-151`, FreeBSD 15.1-RELEASE-p7.
hearst was powered off and not contacted. Andy away; run unattended. Logs are in
`~/skep-test-logs/` on merrick. Nothing merged.

(An earlier run the same day could not validate anything: the VMs were not at
`fresh-151` and hearst was unreachable. This file replaces its record.)

**Result.** The raw `zfs` probe passed every check on both VMs: the miss rate is
90 % and 86 %, none→dev restored every missing node (plain, clone, encrypted), and
nothing reached GEOM. The agent's hostops code, driven on real ZFS by a harness, ended
every one of 50 creates, 20 clones, and 20 rollbacks with a node, and refused both
cases it must refuse. One code fix (Part 1b c).

**Through the API (Part 4, a second unattended run the same night, with
`edk2-bhyve` installed and vmm loaded): passed on both VMs.** With the late wait
cut to 1 s, 51 `vm.create`, 20 disk adds, 20 `vm.clone`, and 20 rollbacks of a
GPT disk all succeeded and all 91 volumes ended with a node; none was left to the
next start, and starting all 71 VMs found nothing to repair. No `p` node and nothing
in `kern.geom.conftxt` after the clones or the rollbacks. A real `vm.start` repaired
a missing node and bhyve ran; `vm.start` of a disk set to `volmode=none` by hand
failed with `disk_device_missing` and the exact command. One gap: the task's error
carries code and message only, so `reason: volmode_none` is in the agent's log, not
in the API (question for Andy, Part 4).

## Decisions taken unattended

1. **zfs.ko loaded by `zpool create tank da1`** on smith, as in the clean-install run;
   no explicit kldload.
2. **`skep-setup apply` with `-no-racct` and no bridge.** The approval was to install
   with skep-setup; apply also writes `vmm_load` and `nmdm_load` to
   `/boot/loader.conf` and `kld_list+=` to `rc.conf`, which take effect only at a boot
   (none happened). `-no-racct` and no `-create-bridge` kept those changes to the
   minimum apply makes. Nothing was kldloaded and no sysctl was set.
3. **No `edk2-bhyve`, no `kldload vmm`.** Package installs other than Skep's were not
   in the approval, and kld changes were excluded, so the API part of Part 3 stopped
   at the first `vm.create` (`bhyve_unavailable`).
4. **A hostops harness instead.** To still measure the repair code on FreeBSD, a
   cross-compiled test program ran the agent's hostops under `<pool>/zvolprobe` (the
   approved scratch name), destroyed at the end. It is not a substitute for the API
   run and is not reported as one.
5. **Probe (c) run three times.** The first run's reader status (124 under
   `timeout`) did not fit; the second had a wrong `ps` format; the third (dd started
   directly, SIGINFO, `procstat`) is the one reported.
6. **Rollback probe on zvols that had a node.** (b) asked whether none→dev restores the
   node after a rollback; no rollback lost one (0/20 on each host), so the toggle was
   also tried on a rolled-back zvol that had its node, and it came back.

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

## Part 4: through the API (second run, 2026-11-23 UTC)

A second unattended run, Andy away. The VMs were **not** rolled back: both were
as Part 3 left them (Skep from 64de929 running, no VMs, `tank` on smith). hearst
was off and not contacted. Logs and the driver scripts are in
[zvol-device-node-20261122/](zvol-device-node-20261122/) (files `20261123T*`,
`zapi.py`, `zloop.py`, `zcleanup.py`).

**Decisions taken unattended (Part 4).**

1. **Same-version reinstall.** The rebuilt package is still 0.1.0, so it went on
   with `pkg add -f` (no guests existed, so the pre-deinstall script had nothing to
   warn about), then `skep-setup upgrade -yes` under `timeout -f`. Upgrade exited 3,
   "health is degraded", because vmm and the netgraph modules were not loaded yet;
   both services were restarted at 0ec6fec, as checked with `-version` and by the
   upgrade itself.
2. **Modules.** Besides vmm and nmdm, the kld_list modules (`if_epair netgraph
   ng_socket ng_ether ng_bpf`) were kldloaded live, as approved. Nothing persistent.
3. **Disk adds:** 20 (the first 20 create VMs, one 64 MiB thin disk each, through
   `PUT /vms/{id}` `add_disks`); the job named no number.
4. **Start path:** every VM (71 per host) was started once through the API and
   force-stopped, so that a node left missing by an earlier path would show up there.
5. **Before each rollback** 1 MiB of random data was written at offset 8 MiB of the
   GPT disk, and its sha256 compared with the image after the rollback.
6. **A missing node for the real start.** `zfs rename` there and back kept the node
   5 times out of 5, so the stopped VM's zvol was destroyed and created again by hand
   with the properties hostops gives it (`zfs create -s -V 64M -o volblocksize=16K
   -o volmode=dev -o skep:vm_id=… -o skep:disk_id=…`); its node was missing at the
   first try. Done on smith only.
7. **The task error has no details.** Not changed: the `Task.error` schema is
   `{code, message}`, and changing it is an API contract change.

**Step 1, state.** Both as described: `skep-agent 0.1.0 (commit 64de929)`, both
services running (`sudo service … status`), `tank` ONLINE on smith, `zroot` on
langrishe, no VM. vmm was **not** loaded although `/boot/loader.conf` had
`vmm_load="YES"`: apply wrote it at 18:40, after the 18:11 boot. The tunable
`hw.vmm.vmx.cap.virtual_interrupt_delivery="0"` was in the kernel environment
(`kenv` read 0).

**Step 2, the wait.** `deviceLateWait` 5 s → 1 s (`fix(hostops)`, 0ec6fec). `go vet`
and `go test ./...` passed on merrick (log `20261123T000806Z-job1-go-test.log`). `make
stage`, `pkg create` on smith (staged sha256 matched `SHA256SUMS.stage`, package
sha256 `e29e7684…82bd`), installed on both as in decision 1.

**Step 3, what setup and the agent said before.** `skep-setup check -mgmt vtnet0`
on both:

```
  manual   packages                           edk2-bhyve
                                                UEFI firmware for VMs is not installed; jails do not need it
                                                $ pkg install edk2-bhyve
```

`check` said **nothing about vmm**: it reads `loader.conf`, which had `vmm_load`, and
does not look at whether the module is loaded. The agent did: `bhyve.available`
false, "the vmm kernel module is not loaded … the UEFI firmware (BHYVE_UEFI.fd …",
`firmware_found` false, capability `bhyve` unavailable. Then `pkg install -y
edk2-bhyve` (g202508_3) and kldload of vmm, nmdm and the kld_list modules on both.
**Hard rule 1:** `sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery` read **0** on
both before any VM start. The agent then reported bhyve available ("vmm is loaded,
the UEFI firmware is installed, and nmdm is available").

**Step 4, through the API.** VMs with 1 vCPU, 256 MiB, one 64 MiB thin disk, no
NIC, driven from merrick (`zloop.py`, pinned key). Repairs per path are counted
from each task's events ("repaired the device node of a disk"; "the device node
of a disk is missing; the next start …" is a node left to the next start), and at
start also from the agent's log ("bhyve: repaired a disk's device node before the
start").

| Path | smith (`tank`): tasks / repaired / left to the next start | langrishe (`zroot`) |
|---|---|---|
| `vm.create` (50 + the GPT VM) | 51 / **36** / 0 | 51 / **41** / 0 |
| disk add (`add_disks`) | 20 / **18** / 0 | 20 / **12** / 0 |
| `vm.clone` of the GPT VM's snapshot | 20 / **2** / 0 | 20 / **4** / 0 |
| rollback of the GPT disk | 20 / **0** / 0 | 20 / **0** / 0 |
| `vm.start` of every VM | 71 / **0** (log: 0) / – | 71 / **0** (log: 0) / – |
| tasks failed | 0 | 0 |
| volumes with a node before the starts | **91/91**, all `volmode=dev` | **91/91**, all `volmode=dev` |
| VMs running after start | **71/71** | **71/71** |
| create time, with a repair: median / max | 1.161 s / 1.201 s | 1.166 s / 1.222 s |
| create time, node there at once (15 / 10 creates) | 0.097–0.125 s | 0.103–0.146 s |
| GPT image on the disk: sha256 equal after the write | yes | yes |
| rollbacks after which the disk's sha256 equalled the image | 20/20 | 20/20 |
| after the clones: `p` nodes / `kern.geom.conftxt` lines with `zvol` | 0 / 0 | 0 / 0 |
| after the rollbacks: the same | 0 / 0 | 0 / 0 |

A create with a repair costs about 1.05 s more than one without: the 1 s late wait
and one none→dev round. Through the API, creates missed less often (36/51, 41/51)
than bare `zfs create` in Part 2 (45/50, 43/50), and disk adds on smith more (18/20);
the reason was not investigated.

**A real `vm.start` repaired a missing node (smith).** VM `zc-50`
(86a3b481-…), stopped, its zvol made again by hand (decision 6): no node. `vm.start`
through the API, task `14e333bc-…`, **succeeded** in 1.16 s. The agent's log:
`hostops: zvol device node repaired … "rounds":1,"duration":34178430` and `bhyve:
repaired a disk's device node before the start`. Afterwards the node existed,
`/dev/vmm/skep-86a3b481-…` existed, the VM was observed `running` with pid 11880,
and `pgrep` showed `11880 bhyve: skep-86a3b481-…`. Force-stopped. (Log
`*-job1-start-repair-smith.log`.)

**`vm.start` refused a disk at `volmode=none` (smith).** VM `zc-49`
(e8189ac9-…), stopped; `zfs set volmode=none` on its disk by hand (node gone).
`vm.start` task `ebd38466-…` **failed**:

```
"code": "disk_device_missing",
"message": "tank/skep/vm/e8189ac9-dc92-48ad-8359-b0791e75d9ad/disk0 has volmode=none, so it has no device node; Skep does not change it on its own. An administrator who wants this disk usable again runs: zfs set volmode=dev tank/skep/vm/e8189ac9-dc92-48ad-8359-b0791e75d9ad/disk0"
```

The agent's log, at WARN: `hostops: zvol device repair refused … "reason":"volmode_none"`.
After the refusal `volmode` was still `none` and no vmm instance existed; running
the printed command brought the node back. **Gap:** the task's `error` in the API
has only `code` and `message` (the `Task.error` schema), so the error's details
(`reason`, `dataset`, `command`) are not visible through the API; the command is in
the message, the reason only in the agent's log. Question for Andy: should a task
error carry the error's details? (An API contract change; not made.)

**Cleanup.** All 71 test VMs on each host deleted through the API (clones first),
0 failures; `<pool>/skep/vm` empty, no snapshots, no `/dev/vmm`.

## Fixes made during the validation

1. `toggleVolMode`: a failed `volmode=none` with `volmode=dev` set back is
   `disk_device_missing`/`repair_failed` with the error in details (Part 1b c, asked for).

## Not validated

- The real start repair and the `volmode=none` refusal ran on smith only (the loops
  ran on both).
- The miss rate on a file-backed pool (hearst's `skeptest`; hearst was off).
- What sends SIGTERM to a reader when `volmode=none` is set under it.
- Through the API, a VM with a NIC (not in scope: the VMs had none).

## End state

- **smith:** `tank` on da1, empty but for the agent's `tank/skep/vm`; skep 0.1.0 (from
  64de929) installed and running; apply journal `/var/db/skep/setup/20261122T234046Z`;
  `/boot/loader.conf` + `vmm_load`, `nmdm_load`; `rc.conf` + `kld_list+=…`,
  `zfs_enable`, `skep_agent_enable`, `skep_api_enable`. No zvolprobe, no key file;
  `/var/tmp/probe.sh` and the GPT image removed.
- **langrishe:** the same on `zroot` (journal `20261122T234048Z`), without the
  `zfs_enable` line (it was already there).
- No VM, no bridge, no probe dataset on either; `kern.geom.conftxt` has no `zvol` line.

**After Part 4 (both VMs):** skep 0.1.0 from 0ec6fec installed and running (backups
of both databases from 64de929 under `/var/db/skep/backup/0.1.0+64de929-…`);
`edk2-bhyve` g202508_3 installed; vmm, nmdm, if_epair, netgraph, ng_socket, ng_ether,
ng_bpf loaded live (nothing persistent beyond what apply had written); no VM, no
zvol, no snapshot, no bridge.
