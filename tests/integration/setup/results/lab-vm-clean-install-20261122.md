# Clean-install check of PR #52 after the finding-1 fix (smith, langrishe), 2026-11-22

Branch feat/m10-installer at 6c40569 (`fix(setup): ask skep-api whether the first
administrator exists`, CI run 45071855517 green). Hosts smith (10.79.20.81, UFS root)
and langrishe (10.79.20.82, ZFS root), rolled back by Andy to Proxmox snapshot
fresh-151. hearst not used. Nothing merged. Logs are in `~/skep-test-logs/` on merrick.

## Decisions taken unattended

1. **`zpool create tank da1` loaded zfs.ko.** smith had no ZFS module loaded; the
   approved `zpool create` loads it itself. No explicit kldload was run.
2. **Login check:** the API's public-key pin was read with `sudo skep-api tls
   fingerprint` on the host and passed to `skepctl login --trust-key`, the password
   fed from the creds file on stdin (never printed). The administrator's password
   for `apply` was typed over a pty by the same helper as before.
3. **Non-root plan:** the fix refuses `-admin` for a non-root plan while api.db
   exists, but on a real host a non-root `check` stops earlier ("open
   /var/db/skep/setup: permission denied … run it as root"), so that refusal is only
   reachable in tests. Left as is.

## Part 1: clean?

| Host | /var/db/skep | skep account | skep pkg | bridge0 | pool tank | Result |
|---|---|---|---|---|---|---|
| smith | absent | absent | absent | absent | absent (ZFS not loaded) | clean |
| langrishe | absent | absent | absent | absent | n/a (zroot only) | clean |

Original files: smith rc.conf 1c8ad786…, loader.conf 75cecdcc…; langrishe rc.conf
fd574992…, loader.conf a27b0c57…

## smith

| Step | Result | Log |
|---|---|---|
| build | skep-0.1.0.pkg from 6c40569, staged sha256 match, pkg sha256 5da7538e… | 20261122T213004Z-lab-build-pkg-6c40569.log |
| 1 pkg add; zpool create tank da1 | PASS | 20261122T213034Z-smith-1-pkg-add-zpool.log |
| 2 apply -mgmt vtnet0 -create-bridge bridge0 -uplink vtnet1 -pool tank -start -admin admin | PASS: administrator planned ("no API database yet") and created; login as admin OK; no file under /var/db/skep/api owned by UID 0 (api 409:409 0700, files 409 0600) | 20261122T213041Z-smith-2-apply.log, 20261122T213050Z-smith-2-login-files.log |
| 3 same apply | PASS: "nothing to do", no new journal; plan: `ok first administrator admin — exists (skep-api admin exists: user "admin" exists (role admin))` | 20261122T213057Z-smith-3-second-apply.log |
| 4 remove -revert-host -purge-data; pkg delete skep | PASS: services stopped, account deleted, only journals left in /var/db/skep, /usr/local/etc/skep gone | 20261122T213116Z-smith-4-remove-pkgdelete.log |
| 5 pkg add; same apply | PASS: journal 20261122T213041Z still records `admin-create admin, done: true`; the administrator was planned and created; login OK; no UID-0 file | 20261122T213136Z-smith-5-reinstall-apply.log |
| 5b (read-only) | `skep-api admin exists`: admin → exit 0, nobody → 1, "Bad Name" → 2; api.db sha256 unchanged; `plan -admin seconduser` → add, "not in the API database (… there is no user "seconduser")" | 20261122T213151Z-smith-5b-exists-paths.log |
| 6 step 4 again | PASS: loader.conf byte-identical; rc.conf differs only by `zfs_enable="YES"` (kept: tank imported) | 20261122T213204Z-smith-6-remove-pkgdelete.log |

Stale revert-plan lines (backlog item): **none appeared** on smith. The remove plans
of steps 4 and 6 are identical (17 actions), although step 6 had the journals of both
installs.

## langrishe

| Step | Result | Log |
|---|---|---|
| 1 pkg add | PASS (skep 409 created) | 20261122T213306Z-langrishe-1-pkg-add.log |
| 2 apply -mgmt vtnet0 -create-bridge bridge0 -uplink vtnet1 -pool zroot -start -admin admin | PASS: administrator planned ("no API database yet") and created; login OK; no UID-0 file under /var/db/skep/api | 20261122T213307Z-langrishe-2-apply.log |
| 3 same apply | PASS: "nothing to do", no new journal; plan: `ok first administrator admin — exists (skep-api admin exists: user "admin" exists (role admin))` | 20261122T213314Z-langrishe-3-second-apply.log |
| 4a remove plan, not confirmed | nothing on zroot touched ("zroot/skep does not exist"); exit 1, nothing changed | 20261122T213321Z-langrishe-4a-remove-plan.log |
| 4 remove -revert-host -purge-data; pkg delete skep | PASS: rc.conf and loader.conf byte-identical to the originals | 20261122T213329Z-langrishe-4-remove-pkgdelete.log |
| 5 pkg add; same apply | PASS: journal 20261122T213307Z records `admin-create admin, done: true`; the administrator was planned and created; login OK; no UID-0 file | 20261122T213341Z-langrishe-5-reinstall-apply.log |
| 6 step 4 again | PASS: rc.conf fd574992… and loader.conf a27b0c57…, byte-identical to the originals; no zroot dataset named skep | 20261122T213352Z-langrishe-6-remove-pkgdelete.log |

Stale revert-plan lines: **none appeared** on langrishe either; the remove plans of
steps 4 and 6 are identical.

## State of the VMs at the end

- **smith:** skep not installed, no skep account, no services, no bridge0.
  `tank` (da1) imported and empty; zfs.ko loaded. rc.conf = original + `zfs_enable="YES"`;
  loader.conf original. /var/db/skep/setup holds the 4 journals of this run.
- **langrishe:** skep not installed, no skep account, no services, no bridge0.
  rc.conf and loader.conf original. /var/db/skep/setup holds the 4 journals of this run.

## Not covered

- In the lab, the "api.db present, user missing" path was exercised only with a read-only
  `plan -admin seconduser` (smith 5b), not with an apply.
- The path where skep-api errors (exit 2) during a plan is covered by unit tests only.
