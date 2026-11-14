# Storage integration checks (Milestone 7, phase 7a)

The named checks of [ADR 0010 decision 16](../../../docs/decisions/0010-storage-and-jails.md#decision-16-what-verified-means-and-what-is-not-in-this-milestone)
for phase 7a, run on `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, UFS-root,
the file-backed pool `skeptest`) with the FreeBSD build of the binaries,
against the test installation in `/home/aba/skep-m6`. The output of every
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
| Disk add, grow, remove | pending (step 3) | |
| Directory pool | pending (step 4) | |
| Monitoring, health checks | pending (step 5) | |
| Clone identity note in the UI (browser test) | pending (step 5) | |
| Permissions | partly: a viewer refused a clone (`vm:write`); the full matrix with step 5 | [step2-checks.txt](results/step2-checks.txt) |
| `storage_locked` with an encrypted `<pool>/<root>` | pending | |
| Unit tests natively (`api`, `database`, `hostops`, `hostfake`, `internalapi`, `storage`, `vm`) | **passed** (step 2 build) | [step2-checks.txt](results/step2-checks.txt) |

Not verifiable on this host and pending regardless: a ZFS-root host; a
host installed from distribution sets.
