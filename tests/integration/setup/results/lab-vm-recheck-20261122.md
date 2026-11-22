# Lab re-check of PR #52 after the fix round (smith, langrishe), 2026-11-22

Branch feat/m10-installer. Hosts smith (10.79.20.81) and langrishe (10.79.20.82).
hearst not used. Nothing merged.

## Decisions taken unattended

1. **The VMs were not at fresh-151.** Both were exactly in the state the
   previous run left "for the rollback to fresh-151" (results on PR #54):
   smith with `tank` imported on da1 holding `tank/skep/{jail,vm,templates/e0ae4cf9…}`,
   6 old journals in `/var/db/skep/setup`, bridge0 and vmm/nmdm/netgraph/if_epair/if_bridge
   loaded, edk2-bhyve installed; langrishe with the `skep` account (409),
   `zroot/skep/…` from the last run, 5 old journals, edk2-bhyve. Rolling back
   needs Proxmox (not approved), so the run went ahead on these states.
   rc.conf and loader.conf on both were verified to be the bytes recorded
   before the first run (smith 1c8ad786…/75cecdcc…, langrishe fd574992…/a27b0c57…),
   so the byte comparisons still compare against the real originals.
   Every result below that the leftovers could affect says so.

## Part 0: fixes

- e16716c `fix(setup): refuse purge without a key check, non-default data dirs, non-regular loader.conf`
  - a. remove -purge-data refuses when zfs list/zfs get fail for an imported pool
    or the agent's configuration cannot be read; plain remove warns.
  - b. upgrade refuses before stopping anything when skep_agent_data_dir or
    skep_api_data_dir is not the default, naming docs/installation/upgrading.md.
  - c. editTunable: Lstat, non-regular refused, directory synced.
  - backlog: unknown-format / wrong-ID journal still fails every command.
- `make test`: pass, `~/skep-test-logs/20261122T203514Z-make-test.log`; FreeBSD cross-build ok.
- Decision: a **missing** agent.json is not "unreadable": the agent then has its
  defaults (no pools), so there is nothing to check. Pools are read the way the
  agent reads them (skep_agent_config honoured). An agent.json that cannot be read
  or parsed already stopped `remove` before any plan (guest count reads it first),
  with or without -purge-data; that is kept and tested.
- Decision: a pool in agent.json that is not imported gets a warning ("not
  checked"), not a refusal (the prompt names imported pools); a `skep` dataset
  that does not exist (zfs: "dataset does not exist") has nothing to check.

## smith

2. **bridge0 is live on smith** (the last run's bridge, vtnet1 a member). `check
   -create-bridge bridge0` refuses ("exists already"); destroying it is a network
   change that was not approved and smith may not reboot, so S2–S4 use
   `-create-bridge bridge1 -uplink vtnet1` (rc.conf lines only; setup never runs
   ifconfig). Finding: setup calls vtnet1 "the spare uplink" although it is a live
   member of bridge0 (backlog item: the prober reads rc.conf, not live bridges).
3. **`-admin admin` was not planned** (finding, see below), so the mismatch / create
   tests used `-admin skepadmin`.

| Item | Result | Log |
|---|---|---|
| S1 build 0.1.0/0.1.1 from e16716c | PASS (staged sha256 match; skep-0.1.0 13ecb20e…, skep-0.1.1 563f21b9…) | 20261122T204349Z-smith-S1-pkg-create-e16716c.log |
| S1 pkg add 0.1.0 | PASS (created skep 409) | 20261122T204440Z-smith-S1-pkg-add.log |
| S1 zpool create tank da1 | NOT RUN: tank already on da1 from the last run (destroy not approved); used as is | — |
| S2 first apply, umask 027, fake service/sysrc/pw first in PATH | PASS for PATH: no marker file, 15 changes, journal complete. **No administrator planned**: see finding 1 | 20261122T204503Z-smith-S2-apply-mismatch.log |
| S2 `-admin skepadmin`, two different passwords | PASS: exit 3, "the two passwords are not the same", retry command printed, journal complete, next check exit 1 (pending, not refused), no marker | 20261122T205248Z-smith-S2c-apply-admin-mismatch.log |
| (S2 aside) prompt not answered, killed by `timeout -f 400` | same clean failure path: journal complete, retry printed, check not refused | 20261122T204544Z-smith-S2b-apply-admin-mismatch.log |
| S3 matching passwords | PASS: only the administrator planned and created; nothing UID 0 under /var/db/skep/api (api 409:409 0700, files 409 0600); both services running; login as skepadmin with pinned key OK | 20261122T205300Z-smith-S3-apply-admin.log, 20261122T205322Z-smith-S3-login.log |
| S4 third apply | PASS: "nothing to do", journals 10 → 10 | 20261122T205328Z-smith-S4-third-apply.log |

### Findings (smith)

1. **Leftover journals suppress the administrator.** `admin()` treats any journal
   with a finished `admin-create` for the same name as "created" (to keep a second
   apply idempotent). Journals survive `pkg delete` and `remove -purge-data`, so on
   a reinstall the first administrator is never created and the plan shows only
   an `ok` line (ok lines are not printed by apply). On smith three of the last
   run's journals held `admin-create admin`; api.db had been purged since.
   Not fixed (planner behaviour change, not among the approved fixes). Possible
   fixes for Andy: ask the API's database (read-only) whether the user exists
   instead of the journals, or ignore journals older than the current api.db.
| S5 agent.json: extra key / pool list / broken JSON | PASS: ok (20 ok) / conflict, not refused / refused ("cannot be parsed"); sha256 restored each time (15b33a78…) | 20261122T205356Z-smith-S5-agent-json.log |
| S6 -dir-pool aba-owned / symlink / root 0700 | PASS: refused (UID 1001) / refused (symbolic link) / ok | 20261122T205410Z-smith-S6-dir-pool.log |
| S7 empty journal dir; truncated journal.json | PASS: check runs and refuses naming `rollback ID`; rollback clears it (keeps journal.json.unreadable); test dirs then deleted | 20261122T205425Z-smith-S7-journals.log |
| S8 pkg upgrade 0.1.0 → 0.1.1 (local repo) | PASS | 20261122T205443Z-smith-S8a-pkg-upgrade.log |
| S8 -keep 0 | PASS: usage error exit 2, API pid 16532 unchanged | 20261122T205458Z-smith-S8b-upgrade-states.log |
| S8 both services stopped by hand | PASS: health ok, "agent socket: answers as 0.1.1+e16716c" | 20261122T210038Z-smith-S8c-upgrade-states-yes.log |
| S8 only skep_agent stopped | PASS: same | same log |
| S8 SIGINT just after "service skep_agent stop" | PASS: ^C ignored, upgrade completed, health ok, socket answers | same log |
| S8 non-default skep_agent_data_dir | PASS: refused (exit 3) naming docs/installation/upgrading.md, API pid 21042 and agent pid unchanged; rc.conf sha256 restored | 20261122T210050Z-smith-S8d-upgrade-datadir.log |
| S9 remove -revert-host with tank imported | PASS: zfs_enable kept and reported; loader.conf == original bytes; rc.conf minus zfs_enable == original bytes | 20261122T210111Z-smith-S9-remove-revert-rollback.log |
| S9 rollback of that remove | PASS: both byte-identical to before it, root:wheel 0644 unchanged | same log |

Notes from smith:
- My first S8 attempt left out `-yes`, so upgrade only asked and changed nothing
  (log 20261122T205458Z); it left both services stopped, which became the "both
  stopped" state of the rerun. The rerun with `-yes` is the result.
- My pty driver's first S2b attempt did not answer "New password for skepadmin:";
  skep-setup was ended by `timeout -f 400` and failed cleanly (recorded above).
- When the agent is not running, the backup directory is labelled `unknown-<ts>`
  (running build unknown); documented behaviour, noted.
- The data-dir refusal names a repository path (docs/installation/upgrading.md);
  the package installs no docs, so on the host the operator must look it up online.
- Leftover journals also add stale lines to the revert plan: "cloned_interfaces:
  setup's words are gone already" and "ifconfig_bridge0: already gone" came from
  the last run's journals next to this run's `sysrc -x cloned_interfaces`. Harmless,
  but confusing; same root cause as finding 1 (journals outlive the install).

**smith state at the end:** skep 0.1.1 (e16716c) installed, services **stopped**
(the rollback of the remove does not start them), rc.conf c4cfbb6c… and loader.conf
36d2a35e… = the state after S2–S4's apply (Skep lines present, bridge1 lines in
rc.conf, never created live), /usr/local/etc/skep/{agent,api}.json, /var/db/skep with
api/agent data, 3 backups and 12 journals; administrator skepadmin; tank imported with
the last run's tank/skep datasets; bridge0 and the modules still live from the last
run; local repo, fakes and test files removed.

## langrishe

4. L1 and L5 used `-admin skepadmin` / no `-admin`: langrishe's leftover journal
   20261122T192821Z holds `admin-create admin`, and `plan -admin admin` printed
   "ok first administrator admin: created by skep-setup run 20261122T192821Z"
   (finding 1 reproduced on a second host).
5. L5's apply was `-mgmt vtnet0 -create-bridge bridge0 -uplink vtnet1 -pool zroot`
   without `-start`/`-admin`: the item is the byte-identical rollback, and with
   `-admin` finding 1 would hide the admin step anyway. The second approved reboot
   was used before L5: bridge0 (live since L1's boot) refused `-create-bridge bridge0`.

| Item | Result | Log |
|---|---|---|
| L1 pkg add 0.1.0, apply -start -admin skepadmin | PASS: 15 changes, admin created as skep | 20261122T210217Z-langrishe-L1-pkg-add-apply.log |
| L1 reboot | PASS: both services up, bridge0 vlanfilter with vtnet1, vmm/nmdm/if_epair/netgraph/ng_* loaded, kern.racct.enable 1 | 20261122T210228Z-langrishe-L1-reboot.log |
| L1 login, pkgbase template, jail with a DHCP NIC | PASS: pin compared with `skep-api tls fingerprint` (b449452d…); template base151r2 4d0ea3cc… succeeded in 36 s; jail rc2j1 running, skep0 10.79.60.136/24 by DHCP | 20261122T210326Z-langrishe-L1-login.log, 20261122T210358Z-langrishe-L1-template-jail.log |
| L2 pkg delete with the jail running | PASS (as designed for 0.1): warning printed (text below), "DEINSTALL script failed", files deleted, exit 0; jail and old daemons kept running | 20261122T210448Z-langrishe-L2-pkg-delete-readd.log |
| L2 pkg add again, restart services | PASS | same log |
| L3 remove -purge-data -keep-guests, jail running | PASS: refused (exit 3); rc.conf, loader.conf, both configs, /var/db/skep listing, journal count, account unchanged | 20261122T210506Z-langrishe-L3-purge-refusals.log |
| L3 enctest with keylocation file:///var/db/skep/enctest.key | PASS: remove -purge-data refused naming the key; plain remove -plan warns ("--purge-data would refuse"); enctest and key destroyed | same log |
| L4 delete jail; remove -revert-host -purge-data | PASS: rc.conf and loader.conf == original bytes (fd574992…/a27b0c57…, root:wheel 0644); skep user and group gone; /var/db/skep holds only setup/; agent.json, api.json gone; datasets listed, none destroyed | 20261122T210531Z-langrishe-L4-revert-purge.log |
| L5 pkg delete + fresh pkg add, apply, rollback | PASS: rc.conf and loader.conf byte-identical, owner and mode unchanged; created files removed | 20261122T210627Z-langrishe-L5-apply-rollback.log |

L2 warning text (pre-deinstall, pkg 2.9.2):

```
skep: WARNING: Skep guests are running:
skep:   jail skep-d74ca8f6-7342-4f61-a8f2-5532c460b063
skep: pkg deletes the files anyway (it does not stop on this script's failure),
skep: and leaves these guests running with nothing to manage them.
skep: Before pkg delete, stop them (web interface, skepctl, or the API) and run
skep: skep-setup remove, which refuses while Skep guests exist.
pkg: DEINSTALL script failed
```

Self-approved (UI text): 5ec6369 `fix(setup): remove's kept-guests warning says pkg
delete only warns`; the warning said "pkg delete refuses while any of them runs",
which L2 shows is false.

Note: in L3 the plain `remove -plan` (no -keep-guests) is also refused for the
stopped jail that still exists ("delete them first"); the key warning is there as
asked.

**langrishe state at the end:** skep 0.1.0 (e16716c) installed, never applied (L5 was
rolled back), services not running; skep account 409 (from L5's pkg add); rc.conf and
loader.conf the original bytes; zroot/skep with templates 02cb4aff… (last run) and
4d0ea3cc… (this run), no jail; 8 journals in /var/db/skep/setup; no bridge (rebooted
before L5), no Skep modules; `~/skep-pkgs/r2` with both packages; no local repo.

## Differences between real FreeBSD and the unit tests

- The unit tests start from an empty journal directory. Real hosts that had Skep
  before keep every journal (pkg delete and -purge-data keep them), and the
  planner reads them all: the first administrator is skipped (finding 1) and old
  revert lines appear in remove's plan.
- Part 0 a's failure paths (zfs list / zfs get failing for an imported pool) were
  not reproducible on the lab VMs without breaking ZFS; covered by unit tests only.
  The "dataset does not exist" match is on zfs's English message, as the fake
  returns it; on both hosts the skep dataset existed, so that path did not run.
  **Pending FreeBSD validation.**
- Part 0 c: loader.conf was a regular file on both hosts; the append and delete of
  kern.racct.enable worked through the new code (S2, S9, L1, L4, L5); the symlink
  refusal is unit-tested only.
- Upgrade with the agent stopped labels the backup `unknown-<ts>`; the tests cover
  the running case's label.

## CI

- e16716c: run 45067453073 success (Test, vet, build; Web interface; Release build).
- Logs of my own tooling slips, kept for completeness: 20261122T205237Z (pty driver
  syntax error, nothing ran on smith but a read-only check), 20261122T205317Z (pin
  parsed wrongly, login refused by skepctl's pin format check; nothing changed).
- 5ec6369: run 45070265734 success. (The lab packages were built from e16716c;
  5ec6369 changes only the warning text above and was not re-run on the VMs.)

## pgrep check

`pgrep -u aba -af 'scratchpad|\.test'` on merrick: only the pgrep command's own
shell. On smith and langrishe no skep-setup, test script or pty driver running.

## Next

- Andy: finding 1 (journals that outlive the install suppress `-admin` and add
  stale revert lines), the VMs not being at fresh-151, and the items still
  pending FreeBSD validation above. Nothing merged; #52 at 5ec6369, #54 has this file.
