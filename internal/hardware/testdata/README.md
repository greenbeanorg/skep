# Discovery fixtures

| File | Origin |
|---|---|
| `freebsd/sysctl-15.1-hearst.txt` | Real `sysctl(8)` output captured from `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, vmm not loaded, zfs loaded). |
| `freebsd/zpool-list.txt` | **Synthetic.** Written from the documented `zpool list -H -p` format; `hearst` has no pools. Replace with a real capture once a test pool exists. |
| `linux/debian13/` | Real procfs and os-release files captured from `merrick` (Debian 13). |
| `qemu/*.txt` | **Synthetic.** Written from the documented `--version` output format; QEMU is not installed on either lab machine. |

Synthetic fixtures prove the parser handles the documented format. They do not
prove the real tools produce that format on FreeBSD 15.1; that is pending
FreeBSD validation.
