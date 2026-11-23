# Draft: zvol created with volmode=dev sometimes gets no /dev/zvol node (FreeBSD 15.1)

Status: **draft, not filed.** Andy files it (OpenZFS issue tracker,
<https://github.com/openzfs/zfs/issues>, with a pointer from FreeBSD
Bugzilla if wanted). The versions and figures below are real: two fresh
FreeBSD 15.1-p7 VMs, 2026-11-22 (Skep's record:
`tests/integration/setup/results/zvol-device-node-20261122.md`, with the
probe script and logs). The file-backed runs are recorded below. Still to
do before filing: paste anything `dmesg` shows on the filing run.
Nothing below mentions Skep. It is not needed to reproduce.

---

**Title:** FreeBSD: `zfs create -V … -o volmode=dev` intermittently creates no `/dev/zvol` device node; it never appears later

### System information

| | |
|---|---|
| Distribution | FreeBSD 15.1-RELEASE-p7, amd64 (`FreeBSD smith.greenbean.org 15.1-RELEASE-p7 FreeBSD 15.1-RELEASE-p7 releng/15.1-n283696-bb03631fe3b7 GENERIC amd64`; the second host identical but for the name) |
| OpenZFS | `zfs-2.4.2-1-FreeBSD_g6330a45b0`, `zfs-kmod-2.4.2-1-FreeBSD_g6330a45b0` (base) |
| Hardware | KVM guest on Proxmox VE (CPU type `host`), 4 vCPUs; pools on virtio-scsi disks (`da1`, and `zroot` on `da0p4`) |
| `vfs.zfs.vol.mode` | default (`vfs.zfs.vol.mode: 1`, geom) |

### Describe the problem

A zvol created with `volmode=dev` should get a character device at
`/dev/zvol/<pool>/<name>` and nothing else (no GEOM provider). On two
fresh 15.1-p7 installs (one UFS-root with a data pool on a second disk,
one ZFS-root using `zroot`), the node is usually missing:

| `zfs create -s -V 1G -o volblocksize=16K -o volmode=dev`, one per second | Host A (pool on `da1`) | Host B (`zroot`) |
|---|---|---|
| no node 1 s after create, of 50 | **45** | **43** |
| of those, appeared within 30 s | 0 | 0 |
| `zfs clone -o volmode=dev` of a zvol snapshot: no node, of 20 | 17 | 20 |
| zvols in an encrypted dataset (aes-256-gcm, key loaded): no node, of 20 | 19 | 14 |
| `zfs rollback` of a zvol that had a node (1 MiB written in between): no node after, of 20 | 0 | 0 |
| `zpool export` + `zpool import` with 50 zvols, all with nodes: nodes after import (3 times) | 50, 50, 50 | not run |

A missing node never appears later, not after 30 s or minutes. Import
creates every node, so the problem seems limited to the create and clone
paths.

The volume itself is fine (`zfs get volmode,volsize` show the requested
values). Any consumer that opens `/dev/zvol/...` fails with ENOENT, for
example bhyve: "Could not open backing file: No such file or directory".

**Workaround.** `zfs set volmode=none <vol>` followed by
`zfs set volmode=dev <vol>` created the node every time it was tried: 179
missing nodes on the two hosts (plain, cloned, and encrypted zvols), each
visible at once. It created no GEOM provider: for a zvol holding a GPT,
no `…p1` node and nothing in `kern.geom.conftxt`. Switching through
`volmode=geom` would also work, but it exposes the volume to GEOM
tasting, which `dev` is meant to avoid.

**Caution with the workaround:** with a process reading the node (`dd`),
`zfs set volmode=none` still succeeds (exit 0); the node disappears and
the reader is terminated at once (SIGTERM, exit status 143, no read
error). So the toggle is only safe while nothing has the volume open.

### How to reproduce

Run as root on a scratch pool. Adjust `POOL` to an existing test pool, or
create a file-backed one as shown. File-backed pools have given different
results on different hosts; see "File-backed pools" below.

```sh
#!/bin/sh
# Reproducer: count zvols created with volmode=dev that get no device node.
POOL=zvoltest
truncate -s 8G /var/tmp/zvoltest.img
zpool create "$POOL" /var/tmp/zvoltest.img

n=50 i=0 miss=0
while [ "$i" -lt "$n" ]; do
	zfs create -s -V 1G -o volblocksize=16K -o volmode=dev "$POOL/v$i"
	sleep 1
	if [ ! -c "/dev/zvol/$POOL/v$i" ]; then
		miss=$((miss + 1))
		echo "missing: $POOL/v$i"
	fi
	i=$((i + 1))
done
echo "no node for $miss of $n zvols"

# A missing node does not appear later.
sleep 30
for v in $(zfs list -H -o name -t volume -r "$POOL"); do
	[ -c "/dev/zvol/$v" ] || echo "still missing after 30 s: $v"
done

# Workaround, for one missing volume:
#   zfs set volmode=none $POOL/vN; zfs set volmode=dev $POOL/vN; sleep 2
#   ls -l /dev/zvol/$POOL/vN
# Confirm nothing reached GEOM:
sysctl -n kern.geom.conftxt | grep zvol || echo "no zvol in GEOM (expected)"

zpool destroy "$POOL"
rm -f /var/tmp/zvoltest.img
```

Expected: `no node for 0 of 50 zvols`. Observed (disk-backed pools):
45 and 43 of 50 missing (90 % and 86 %). For file-backed pools, see
"File-backed pools" below.

### Include any warning/errors/backtraces from the system logs

Nothing in `dmesg` or `/var/log/messages` when a node is missing (paste
anything seen on the filing run).

### Notes

- File-backed pools: reproduced on Host A, did not reproduce on another
  host (see "File-backed pools" below). The cause of the difference is not
  known.
- Hosts that need a guaranteed node can work around it today with the
  none→dev toggle while nothing has the volume open.

### File-backed pools

#### Configuration where it did not reproduce

With the same probe and pacing on FreeBSD 15.1-RELEASE-p7, on a pool backed
by a file on UFS, 0/100 creates and 0/100 clones were missing a node at 0, 1
and 5 s. This was one run on one host. That host differs from the reproducing
ones in more than the pool's backing, so no cause is claimed.

#### Configuration where it did reproduce

On Host A (2026-11-23, 2 vCPUs, booted minutes before, same OpenZFS version), one run
of each, the disk-backed pool first. Both used `-s -V 1G -o volblocksize=16K
-o volmode=dev` for creates and `zfs clone -o volmode=dev` of one zvol
snapshot for clones, one per second. Each node was checked right after the
command, 1 s later and 5 to 6 s later.

| No node, at 0 s / 1 s / 5 s | Disk-backed pool (`da1`, 15.5 GiB) | Pool on a 4 GiB file on the UFS root |
|---|---|---|
| creates | 46 / 46 / 46 of 50 | 98 / 98 / 98 of 100 |
| clones | 12 / 12 / 12 of 20 | 70 / 70 / 70 of 100 |

No node that was missing at 0 s appeared by 5 s. `dmesg` gained no lines
during either run.

This shows that a pool backed by a file on UFS is not enough to avoid the
problem: the two file-backed results disagree, so file backing does not
explain the host where it did not reproduce. It does not show why that host
behaved differently, and it does not compare the two backings on Host A: the
counts differ, the pools differ in size, and each is one run in a fixed order.
No cause is claimed.
