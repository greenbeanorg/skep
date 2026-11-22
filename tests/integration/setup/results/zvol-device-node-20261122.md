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
