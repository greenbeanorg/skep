# Draft: zvol created with volmode=dev sometimes gets no /dev/zvol node (FreeBSD 15.1)

Status: **draft, not filed.** Andy files it (OpenZFS issue tracker,
<https://github.com/openzfs/zfs/issues>, with a pointer from FreeBSD
Bugzilla if wanted). Before filing: paste the real `zfs version` and
`uname -a` output, and the numbers from the reproducer below run on a
fresh host. The figures here come from the 2026-11-22 lab-VM run of
Skep's installer (finding 5 in
`tests/integration/setup/results/lab-vm-validation-20261122.md`). The
fuller probe (clone, rollback, encryption, open node) has not run yet.
Nothing below mentions Skep. It is not needed to reproduce.

---

**Title:** FreeBSD: `zfs create -V … -o volmode=dev` intermittently creates no `/dev/zvol` device node; it never appears later

### System information

| | |
|---|---|
| Distribution | FreeBSD 15.1-RELEASE-p7, amd64 |
| OpenZFS | zfs-2.4.2 as shipped in base (paste `zfs version`) |
| Hardware | KVM guest on Proxmox VE (CPU type `host`), 4 vCPUs; pools on virtio-scsi disks (`da1`, and `zroot` on `da0p4`) |
| `vfs.zfs.vol.mode` | default (1, geom) |

### Describe the problem

A zvol created with `volmode=dev` should get a character device at
`/dev/zvol/<pool>/<name>` and nothing else (no GEOM provider). On two
fresh 15.1-p7 installs (one UFS-root with a data pool on a second disk,
one ZFS-root using `zroot`), the node exists only some of the time:

- five creates in a row: 2 of 5 had a node, on each host;
- one create at a time, with pauses: 1 of 6;
- a missing node never appears later, not after 30 s, minutes, or
  `zfs list`/`zfs get` on the volume.

The volume itself is fine (`zfs get volmode,volsize` show the requested
values). Any consumer that opens `/dev/zvol/...` fails with ENOENT, for
example bhyve: "Could not open backing file: No such file or directory".

**Workaround.** `zfs set volmode=none <vol>` followed by
`zfs set volmode=dev <vol>` created the node in 5 of 5 cases, without
creating a GEOM provider. Switching through `volmode=geom` also works,
but it exposes the volume to GEOM tasting, which `dev` is meant to avoid.

### How to reproduce

Run as root on a scratch pool. Adjust `POOL` to an existing test pool, or
create a file-backed one as shown. (Whether a file-backed pool reproduces
it as readily as the disk-backed pools above is still to be confirmed.)

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
roughly 60 % missing.

### Include any warning/errors/backtraces from the system logs

Nothing in `dmesg` or `/var/log/messages` when a node is missing (paste
anything seen on the filing run).

### Notes

- Not yet checked: whether `zfs clone -o volmode=dev` and `zfs rollback`
  of a `dev` zvol show the same, and whether an encrypted parent matters.
- Hosts that need a guaranteed node can work around it today with the
  none→dev toggle while nothing has the volume open.
