# Discovery fixtures

| File | Origin |
|---|---|
| `freebsd/sysctl-15.1-hearst.txt` | Real `sysctl(8)` output captured from `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, vmm not loaded, zfs loaded, no pools). |
| `freebsd/sysctl-15.1-hearst-vmm.txt` | Real `sysctl(8)` output captured from `hearst` with vmm loaded. |
| `freebsd/zpool-list-hearst.txt` | Real `zpool list -H -p` output captured from `hearst` with the file-backed test pool `skeptest` imported. |
| `freebsd/zpool-list-synthetic.txt` | **Synthetic.** Two pools, one degraded and one reporting `-` for fragmentation. Kept for the cases the lab pool does not show. |
| `linux/debian13/` | Real procfs and os-release files captured from `merrick` (Debian 13). |
| `qemu/*.txt` | **Synthetic.** Written from the documented `--version` output format; QEMU is not installed on either lab machine. |

Synthetic fixtures prove the parser handles the documented format. They do not
prove the real tools produce that format on FreeBSD 15.1. The single-pool
`zpool list` format is confirmed by the real capture; a degraded pool, a `-`
fragmentation value, and QEMU version output are pending FreeBSD validation.
