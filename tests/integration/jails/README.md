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
| How `pkg --rootdir` runs scripts and triggers (pkg 2.9.2 source, and a traced hand-run build of `FreeBSD-set-minimal`); host `/etc`, `/var/db/pkg`, `/usr/share/certs` unchanged by that build | **recorded**: shell scripts run unchrooted on the host; host state identical. Andy's decision (B with changes, 2026-11-16) is in ADR 0010's step 6 notes | [step6-pkg-trace.txt](results/step6-pkg-trace.txt) |
| Feasibility of the step 6 build specification, hand-run before building it | **resolved**: Andy chose option A (`jexec -l`, no `-U`), `/etc/services` kept, Decision 9 amended (ADR 0010 step 6, "Resolved") | [step6-probe.txt](results/step6-probe.txt), [step6-progress.md](results/step6-progress.md) |
| Template (pkgbase): the step 6 builder builds a 15.1 userland; release recorded `15.1-RELEASE-pN` | **passed** (2026-11-16) for the builder with the real host operations, run by its FreeBSD test binary: `15.1-RELEASE-p6`, 66 packages, every difference from the trace explained (services.db intended). Through the deployed agent's task and API (`a34749d`/`c8b0f06`): **passed** (2026-11-16), byte-identical to the test binary's template apart from `local.sqlite`'s install times | [step6-build.txt](results/step6-build.txt), [step6-deployed.txt](results/step6-deployed.txt) |
| Template refusal: a changed trigger file fails with `template_script_unknown` and leaves nothing | **passed** (2026-11-16) | [step6-build.txt](results/step6-build.txt) |
| Template (dist): a mismatched `base.txz` refused, a matching one builds | pending | |
| Template host safety: host hashes identical, `pkg` environment fixed (`procstat -e`), `var/cache/pkg` empty in `@skep-base`, no build jail left, no deferred trigger | **passed** (2026-11-16) by the builder's own check and from outside, for the test binary and for the deployed agent (763 files with `/var/db/pkg/vuln.xml` excluded, ADR 0010 item 7) | [step6-build.txt](results/step6-build.txt), [step6-deployed.txt](results/step6-deployed.txt) |
| Permissions: `operator` refused `storage:write` with a real `container.template.create` request, and audited | **passed** (2026-11-16): 403 `forbidden`, `permission: storage:write`, `authz.deny` in the access log | [step6-deployed.txt](results/step6-deployed.txt) |
| Agent start-up sweep of `skep-build-*` jails, with none present | **passed** (2026-11-16): `build_jail_sweep found=0` before the socket is served | [step6-deployed.txt](results/step6-deployed.txt) |
| Jail lifecycle, networking, baseline, root parent, orphaned epairs, `started_once`, limits, clone identity, symlink rule, monitoring, permissions | pending (steps 7 to 10) | |

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

- [step6-probe/](step6-probe/): the hand-run probe of the specification
  before it was built ([results/step6-probe.txt](results/step6-probe.txt)).
- [step6-build/](step6-build/): the native build, the refusal, and the
  comparison with the trace (`run-build.sh`, `run-refusal.sh`,
  `compare.sh`), driving `internal/templates/integration_freebsd_test.go`.
- [step6-deployed/](step6-deployed/): the deployed agent's run
  (`hashhost.sh`, the host files from outside with nothing excluded;
  `compare.sh`, two template trees entry by entry).

`skeptest/trace-7b`, `/var/tmp/skep-trace7b`, `skeptest/step6` and
`skeptest/step6-scratch` were destroyed on 2026-11-16 with Andy's approval
([results/step6-progress.md](results/step6-progress.md), sections 8 and 11).
