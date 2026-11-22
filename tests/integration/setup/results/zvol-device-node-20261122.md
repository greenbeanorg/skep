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

Logs: 20261122T231318Z-zvol-part1-clean-check.log,
20261122T*-zvol-smith-zpool-create.log.

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
and runs a repair on it. Log: 20261122T*-zvol-part1b-a-zfs-list.log.

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

## Fixes made during the validation

1. `toggleVolMode`: a failed `volmode=none` with `volmode=dev` set back is
   `disk_device_missing`/`repair_failed` with the error in details (Part 1b c, asked for).
