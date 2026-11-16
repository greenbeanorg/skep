# Jail integration checks (Milestone 7, phase 7b)

The named checks of [ADR 0010 decision 16](../../../docs/decisions/0010-storage-and-jails.md#decision-16-what-verified-means-and-what-is-not-in-this-milestone)
for phase 7b, run on `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, UFS-root,
pkgbase, the file-backed pool `skeptest`) with the FreeBSD build of the
binaries, against the test installation in the package layout
(`/usr/local/sbin`, `/usr/local/etc/skep`, `/var/db/skep`). The output of
every run is in [results/](results/). A check without a recorded passing
run is **pending FreeBSD validation**, whatever CI says.

## Status

| Check | Status | Record |
|---|---|---|
| hearst change 3: `kern.racct.enable=1` in `loader.conf` and the reboot (1 of 2); after it, `hw.vmm.vmx.cap.virtual_interrupt_delivery` is `0` before any `vm.start`; pool, services, and bridge back | **done** (2026-11-16) | [step6-reboot.txt](results/step6-reboot.txt) |
| How `pkg --rootdir` runs scripts and triggers (pkg 2.9.2 source, and a traced hand-run build of `FreeBSD-set-minimal`); host `/etc`, `/var/db/pkg`, `/usr/share/certs` unchanged by that build | **recorded**: shell scripts run unchrooted on the host (raised with Andy; see the record); host state identical | [step6-pkg-trace.txt](results/step6-pkg-trace.txt) |
| Template (pkgbase): `container.template.create` builds a 15.1 userland; release recorded `15.1-RELEASE-pN` | pending: not implemented until the question above is answered | |
| Template (dist): a mismatched `base.txz` refused, a matching one builds | pending | |
| Template host safety, by a Skep task: host hashes identical, `pkg` environment fixed (`procstat -e`), `var/cache/pkg` empty in `@skep-base` | pending (the hand-run build passed the hash part; see above) | |
| Jail lifecycle, networking, baseline, root parent, orphaned epairs, `started_once`, limits, clone identity, symlink rule, monitoring, permissions (`operator` refused `storage:write` with a real `container.template.create` request) | pending (steps 7 to 10) | |

Not verifiable on this host and pending regardless: a ZFS-root host; a host
installed from distribution sets (the `dist` source is checked, the host
shape is not).

## The scripts

- [pkgbase-trace.sh](pkgbase-trace.sh): the traced build, as run on
  2026-11-16 (`sudo sh pkgbase-trace.sh` on hearst). It creates
  `skeptest/trace-7b`, refuses to run if it exists, and leaves it for
  review.
- [pkgbase-trace-analyze.sh](pkgbase-trace-analyze.sh): decodes the ktrace
  and lists the execs, the writes outside the root and work area, the host
  paths the script children read, the deferred triggers, and the files
  changed on the UFS root during the build.

Clean-up once the record has been reviewed:
`sudo zfs destroy -r skeptest/trace-7b; sudo rm -r /var/tmp/skep-trace7b`.
