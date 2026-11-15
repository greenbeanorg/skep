# Storage integration checks (Milestone 7, phase 7a)

The named checks of [ADR 0010 decision 16](../../../docs/decisions/0010-storage-and-jails.md#decision-16-what-verified-means-and-what-is-not-in-this-milestone)
for phase 7a, run on `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, UFS-root,
the file-backed pool `skeptest`) with the FreeBSD build of the binaries,
against the test installation, which was in `/home/aba/skep-m6` (binaries,
`agent.json`) and `/home/aba/skep-m5/state` (skep-api's data) until
2026-11-15 and is now in the package layout (`/usr/local/sbin`,
`/usr/local/etc/skep`, `/var/db/skep`; see
[layout-move.txt](results/layout-move.txt)). The output of every
run is in [results/](results/). A check without a recorded passing run is
**pending FreeBSD validation**, whatever CI says.

## Status

| Check | Status | Record |
|---|---|---|
| Inventory: the M6 VM's tree with owners; a hand-made dataset under `skeptest/skep` listed as unowned and left alone | **passed** | [step2-checks.txt](results/step2-checks.txt) |
| Snapshot: taken while running (crash-consistent) and while stopped (consistent); the VM's two disks share one snapshot (one transaction group); the variable store copy exists | **passed** | [step2-checks.txt](results/step2-checks.txt) |
| Rollback: a guest that wrote a marker after the snapshot loses it and boots (its UFS journal replayed); an older snapshot refused without `destroy_newer`, and with it exactly the newer snapshot destroyed on every dataset and named in the audit entry; disk contents checked on the host; refused while running | **passed** | [step2-checks.txt](results/step2-checks.txt) |
| Clone: boots with a new MAC and its own DHCP lease while the source runs; origin snapshot and source deletes refused with `snapshot_has_clones`; `volmode=dev`, owner properties, no reservation on the clone; clone without a snapshot ID leaves its snapshot | **passed** | [step2-checks.txt](results/step2-checks.txt) |
| A snapshot made by hand under a VM's dataset blocks `destroy_newer` and is listed as unowned | **passed** | [step2-checks.txt](results/step2-checks.txt) |
| Disk add, grow, remove: a second disk appears in the guest (`vtbd1`) and a rollback past the add is refused; grow refused while running and for a shrink, then grown with `gpart resize` and `growfs` in the guest, which reboots clean; removal refused with `disk_has_snapshots`, then with `destroy_snapshots` the snapshots containing the disk gone from every dataset and the older ones intact; rollback after the removal restores the old size and boots | **passed** (build 732b5b9); the `operator`-without-`vm:delete` refusal is unit-tested only (no such role can be made on hearst) | [step3-checks.txt](results/step3-checks.txt) |
| Directory pool: FreeBSD 15.1 installed onto and booted from a sparse file disk in `/var/tmp/skep/dirpool` on hearst's UFS root; snapshot and clone refused with `storage_feature_unsupported`; grow, preallocated add, remove, delete on real files; a 0755 pool reported and refused | **passed** (build ca7a609) | [step4-checks.txt](results/step4-checks.txt) |
| Monitoring: `/metrics` reports each pool's size, allocation, and capacity as `zpool list` and `df` do, each VM's disk use as `zfs get` does, and a running VM's bhyve CPU time and resident memory as `ps` does | **passed** (build c7c2f7a, after a fix found here) | [step5-checks.txt](results/step5-checks.txt) |
| Health: `storage_capacity` warns and fails when the thresholds are lowered below current use, and is `ok` again when restored | **passed** (build c7c2f7a) | [step5-checks.txt](results/step5-checks.txt) |
| `storage_locked` with an encrypted `<pool>/<root>` (`skeptest/skep-enc`) and its key unloaded: the health check and `/metrics` name the datasets, the inventory shows `unavailable`; snapshot, disk growth, and create fail with `storage_locked`; start refused with `storage_locked` before a task exists, audited | **passed** (build 1296f78, after a fix found here: start had failed in bhyve instead) | [step5-checks.txt](results/step5-checks.txt) |
| Clone identity note in the UI (browser test): the clone form and the clone's page say a clone keeps the source's hostname, SSH host keys, and machine ID | **passed** (Playwright against hearst; also in the local browser tests) | [step5-checks.txt](results/step5-checks.txt) |
| Permissions: a `viewer` reads every 7a endpoint and is refused every write (9 routes), each refusal audited with the permission; the `operator` role has no `storage:write` | **passed** (no 7a route requires `storage:write`, so its refusal is shown by the role and the route table test, not a request) | [step5-checks.txt](results/step5-checks.txt) |
| `skepctl vm snapshot` and `vm clone` natively on hearst, through the public API | **passed** | [step5-checks.txt](results/step5-checks.txt) |
| Unit tests natively (`api`, `database`, `hostops`, `hostfake`, `internalapi`, `storage`, `vm`; `monitoring`, `config` from step 5) | **passed** (steps 2 to 5; `config`'s two tests that read the source tree's sample files skipped on hearst) | [step2-checks.txt](results/step2-checks.txt), [step5-checks.txt](results/step5-checks.txt) |

Not verifiable on this host and pending regardless: a ZFS-root host; a
host installed from distribution sets.

The browser check against hearst is `web/e2e/clone-note.spec.ts`, run with
`SKEP_E2E_EXTERNAL=1 SKEP_E2E_LISTEN=10.79.20.170:8443` and the
`SKEP_E2E_CLONE_*` variables it documents; it makes a clone through the
interface and deletes it again.
