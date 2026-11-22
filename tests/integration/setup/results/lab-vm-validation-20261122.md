# Lab-VM validation of the Milestone 10 installer (PR #52), 2026-11-22

Run unattended against [packaging.md](../../../../docs/installation/packaging.md)
§3–4 and Andy's nine-part checklist. Raw output, one file per step:
[lab-vm-20261122/](lab-vm-20261122/) (copied from `~/skep-test-logs`).

| Host | What | Root | Install |
|---|---|---|---|
| `smith` 10.79.20.81 | FreeBSD 15.1-RELEASE-p7 amd64, 2 CPUs, 4 GiB | UFS on da0 (MBR, BIOS) | distribution sets; spare da1 → pool `tank` |
| `langrishe` 10.79.20.82 | FreeBSD 15.1-RELEASE-p7 amd64, 2 CPUs, 4 GiB | ZFS `zroot` (GPT, UEFI) | pkgbase |

Both: pkg 2.9.2, zfs-2.4.2-1-FreeBSD_g6330a45b0, `vtnet0` management,
`vtnet1` guest uplink on VLAN 60 (DHCP from OPNsense),
`hw.vmm.vmx.cap.virtual_interrupt_delivery="0"` in loader.conf.

Head under test: e8836a8 at the start; fixes made during the run are
bd406aa, 20c8731 and 67fa267 (all on #52, CI green). smith ran parts 2–8
on e8836a8 → 20c8731 (part 8's revert with a 67fa267 test binary);
langrishe ran parts 2–9 on 67fa267.

## Result

| Part | smith | langrishe |
|---|---|---|
| 1 Build packages (`pkg create`) | PASS | (built on smith) |
| 2 check/plan before install | PASS | PASS |
| 3 `pkg add` | PASS | PASS |
| 4 check → plan → apply, reboot | PASS after fix 1 | PASS |
| 5 Smoke test (jail + VM on the new bridge, filtered) | jail PASS; **VM FAIL** (zvol node), passes after a manual zvol step | PASS |
| 6 Second apply is a no-op | PASS after fix 2 | PASS |
| 7 Upgrade (PKG_UPGRADE, refusals, 503, success) | PASS | PASS |
| 8 `pkg delete` refusal; remove; rollback; revert+purge | **`pkg delete` refusal FAIL** (not enforced); rest PASS after fix 3 | **`pkg delete` refusal FAIL**; rest PASS |
| 9 Fresh apply + rollback byte-identical | — | PASS |

## Packages

`make stage VERSION=…` on merrick, `pkg create -M +MANIFEST -p plist -r
stage -o out` on smith. Staged files always matched `SHA256SUMS.stage`.
`pkg info -F`: name `skep`, origin `sysutils/skep`, ABI
`FreeBSD:15:amd64`, the ten files, both scripts embedded.

| Build | skep-0.1.0.pkg | skep-0.1.1.pkg |
|---|---|---|
| e8836a8 | d6dd2e7724f93b98a189995017e9197395b728b46b889f433a3487f84d80c3dd | 0a432ab96d67b8f2561f9e8c9f2923a128757f1dadc08bffef74245ede7648e8 |
| bd406aa | 21b611be76905e10c48ec93ec97fe52256b4ce0e27bcf7212c6b83f01bbf8932 | cb3ee6c85db3121312c22a997c74b49405e8def7fab3967c33cf62052a3cab94 |
| 20c8731 | 8cb77163f13ee974a692ea130e0fc0b8fdb25ec9388e3d252a033c4e98a4f399 | 65db3826e633c501eed5024c618f44bb3b921a3713c966fa79fceea9149de992 |
| 67fa267 | eaa321ee187f4f52233768eff75a38d990a4101aa91834f81d407730fb63a9ed | 1604aacb8dde857f86a5da97545409d96355b317914d5674b36843f4f0b508d2 |

Also 0.1.2 from 20c8731 (e8978fb7…cbc81) for a second upgrade on smith,
and a probe pair `skep-pkgprobe` 1.0/1.1 whose pre-deinstall writes its
environment to `/var/tmp`, to record `PKG_UPGRADE` without altering the
real package.

## PKG_UPGRADE (answer 14)

Measured on both hosts, pkg 2.9.2:

| Operation | Old package's pre-deinstall | `PKG_UPGRADE` |
|---|---|---|
| `pkg upgrade -r <repo> skep-pkgprobe` (1.0 → 1.1) | runs | **`true`** |
| `pkg delete skep-pkgprobe` | runs | unset |
| `pkg add skep-pkgprobe-1.1.pkg` over 1.0 | **does not run** ("already installed, forced install") | — |

Also set for the script: `PKG_NAME`, `PKG_PREFIX=/usr/local`,
`PKG_ROOTDIR=/`, `PKG_MSGFD=4`. The real `skep` package upgraded with
`pkg upgrade -r skeplocal skep` (a temporary local file:// repository)
with Skep jails running: not refused, guests and their filters untouched,
the old daemons kept running until `skep-setup upgrade`.

## How real FreeBSD differed from the Linux tests

1. **sysrc refuses dotted names.** `sysrc -f /boot/loader.conf
   kern.racct.enable=1` (and `-n`, `-c`, `-x` on such a name) fails with
   "name contains characters not allowed in shell" (sysrc 7.2). The first
   apply on smith stopped there. Fix 1.
2. **`sysrc NAME-="…"` leaves `NAME=""`** when it removes every word, so
   `remove -revert-host` did not return rc.conf to its bytes
   (`kld_list=""`, `cloned_interfaces=""`). The fake sysrc did the same,
   but the test compared values, not bytes. Fix 3.
3. **A second apply with `-start -admin` was not a no-op**: it planned the
   service starts and the administrator again, wrote a journal, and failed
   at "user admin already exists", leaving an incomplete journal that
   blocked the next plan. The fixtures never had running services or an
   existing user. Fix 2.
4. **pkg does not enforce a failing pre-deinstall.** With a Skep jail
   running, `pkg delete skep` printed the script's refusal and the jail,
   then "pkg: DEINSTALL script failed", **deleted the files and exited 0**
   (both hosts). Only `pkg -o DEVELOPER_MODE=yes delete` stops (exit 3);
   the default is `DEVELOPER_MODE = false`. A failing **pre-install** does
   stop `pkg add` (UID 409 held by a temporary user: "PRE-INSTALL script
   failed", exit 1, nothing installed). Not fixed; see proposals.
5. **zvols created with `volmode=dev` get their `/dev/zvol` node only
   sometimes** (zfs-2.4.2 on 15.1-p7, both hosts). Skep's exact `zfs
   create -s -V 2G -o volblocksize=16K -o volmode=dev …` five times in a
   row gave 2/5 nodes on each host; one at a time, 1/6; a missing node
   never appears later. smith's VM disk had none, so bhyve failed ("Could
   not open backing file … No such file or directory", status 4);
   langrishe's happened to get one. `zfs set volmode=none` then
   `volmode=dev` restored 5/5 nodes without ever exposing the zvol to GEOM
   (geom → dev also works but lets GEOM taste the disk). Not fixed; see
   proposals.
6. **FreeBSD `timeout(1)` reaps and kills every descendant** when it
   fires, and waits for them: `sudo timeout 120 skep-setup upgrade`
   finished the upgrade in seconds, then hung until 120 s and killed both
   daemons that setup had started. A harness issue, not Skep's; the run
   used `timeout -f` afterwards. Recorded for test scripts.
7. **An agent restart during a jail stop leaves the jail half stopped**:
   the stop destroys the jail's epairs first, then runs `jail -r`; the
   agent's shutdown killed `jail -r` during `/etc/rc.shutdown`, leaving
   the jail running with only `lo0`, `desired_state` stopped, and a stale
   `host_interface`. The next stop completed it. This is what
   `skep-setup upgrade -force` does to a running stop. Seen once (smith,
   a test task injected too late, see part 7).

Paths validated here that were "pending FreeBSD validation" elsewhere: the
dist template source (smith, 6 s), a pkgbase template through a deployed
agent on a ZFS-root host (langrishe, 44 s), and the package scripts.

## Fixes on #52 (pushed, CI green)

| Commit | Fix |
|---|---|
| bd406aa | Loader tunables with a dot are appended or deleted by setup itself (atomic, mode and owner kept, read back); the plan prints `append kern.racct.enable="1" to /boot/loader.conf`; the fake sysrc refuses dotted names. `TestEditTunable` |
| 20c8731 | The prober records `service skep_agent|skep_api status`; running services and an administrator a journal shows created (rolled back or not) are `ok`. `TestSecondApplyWithStartAndAdminIsANoOp` |
| 67fa267 | A list the apply created (absent from that apply's backup) holding only setup's words, or nothing, is removed with `sysrc -x`. `TestRevertHostRestoresTheBytes` |
| 3752a00 | Docs: validation status, the pkg finding, the 0.1 acceptance of the 503 after a late refusal, agent drain mode in the backlog |

## Proposals (not built; need Andy)

- **Pre-deinstall refusal (finding 4).** pkg will not enforce it. Options:
  (a) keep the script as a loud warning, and document `skep-setup remove`
  as the only guard (it refuses with guests, running or stopped); (b) have
  `skep-setup apply` run `pkg lock skep` and `remove` unlock it (a locked
  package cannot be deleted or upgraded without `pkg unlock`, so the
  upgrade flow would need an unlock step); (c) ask upstream pkg for a
  documented way for a deinstall script to abort. Recommendation: (a) for
  0.1 (docs already say so), (b) evaluated after 0.1.
- **zvol node (finding 5).** Before a VM start (and after creating or
  cloning a disk), if `/dev/zvol/<disk>` is missing, run `zfs set
  volmode=none` then `volmode=dev` and wait a few seconds for the node;
  fail with a clear error if it still is missing. Never via `geom`. A
  hostops change outside the installer PR, so a separate PR; until then
  VMs on 15.1-p7 fail to start at random. Also worth an upstream OpenZFS
  report.
- **Operator-edited agent.json blocks setup.** On smith, adding
  `storage.dist_dir` to setup's agent.json (the documented way to enable
  the dist source) made every later `check`/`plan`/`apply` refuse
  (`refuse config agent.json: exists and was not written by skep-setup`).
  Normal administration of agent.json then blocks setup entirely. Propose:
  treat a changed agent.json that setup once wrote as `ok, edited by the
  administrator` and compare only the allow-lists setup owns. The run
  restored setup's bytes after building the template.
- **pkgbase availability disagrees.** On smith (FreeBSD-base repository
  not enabled) setup says "FreeBSD-base repository: disabled", while the
  agent's `/storage/template-sources` says `pkgbase: available: true`.
  One of the two checks is wrong; not tested further (smith used dist).
- Nits: manual `db backup` directories are `<ver>-<commit>-<ts>`,
  setup's `<ver>+<commit>-<ts>`; the revert plan lists
  `skep_agent_enable`/`skep_api_enable` `ok` twice.

## Per part

### 1. Packages (smith) — PASS
See the table above. Logs `*-stage-packages.log`, `*-smith-part1-*`.

### 2. check/plan before install — PASS (both)
Staged `skep-setup` with `-mgmt vtnet0 -create-bridge bridge0 -uplink
vtnet1 -pool tank|zroot -start -admin admin`: exit 3; refused `account
skep` ("the package's pre-install creates it"), and on smith also `pool
tank` (not imported yet). `apply` exit 3, "nothing was changed",
rc.conf/loader.conf sha256 unchanged, no `/var/db/skep`.

### 3. pkg add — PASS (both)
`skep:*:409:409::0:0:Skep service account:/nonexistent:/usr/sbin/nologin`,
group `skep:*:409:`; binaries and rc.d `0555 root:wheel`, samples `0644`,
licenses `0444`; no agent.json or api.json; no process; rc.conf
unchanged; `pkg check -s` clean. A second `pkg add` and `pkg add -f` with
the account present changed nothing. `pkg delete` with no guest kept the
account and `/var/db/skep` (§4 item 6).

### 4. check → plan → apply, reboot — PASS (smith after fix 1)
smith: `zpool create tank da1`. Apply: loader.conf `vmm_load`,
`nmdm_load`, `kern.racct.enable`; rc.conf `kld_list`, `zfs_enable`
(smith), `cloned_interfaces`, `ifconfig_vtnet1="up"`, `ifconfig_bridge0=
"addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`, both
`_enable`; agent.json `0600 root:wheel`, api.json `0640 root:skep`,
`/var/db/skep/media` `0700`; services started; administrator created
through the `/dev/tty` prompt over a pty (password from a 0600 file on
merrick; 0 occurrences in any log). Before the reboot the agent ran
without bridge0 or vmm. edk2-bhyve installed (setup's "manual" item).
After the reboot: both services running (root `service status`),
`/api/v1/health` `ok`, `port_filters` `ok`, `bridge_vlan_unfiltered` `ok`,
`hw.vmm.vmx.cap.virtual_interrupt_delivery: 0`, `kern.racct.enable: 1`,
vmm nmdm if_epair netgraph ng_socket ng_ether ng_bpf loaded, bridge0
`VLANFILTER defuntagged=1`, vtnet1 `untagged 1`. TLS pin from `skep-api
tls fingerprint` matched the presented key on both.

The failed first apply on smith also exercised the failure path: the
incomplete journal made the next plan refuse ("an earlier run did not
finish"), and `rollback` restored loader.conf to the original bytes.

### 5. Smoke test — langrishe PASS; smith jail PASS, VM after a manual step
Templates: smith dist (base.txz 15.1-RELEASE, `storage.dist_dir` added to
agent.json temporarily, then setup's bytes restored and verified);
langrishe pkgbase. Each host: a jail with a DHCP NIC on bridge0 (leases
10.79.60.122 and .128 through the filter) and a VM (1 CPU, 512M, thin 2G
zvol, NIC on bridge0). Running: `skf_epair0a` and `skf_tap0` in `ngctl
list`, every bridge member `untagged 1`, health `ok`. Both stopped and
deleted. smith's VM start failed on the missing zvol node (finding 5);
`volmode` geom → dev by hand on its fresh, empty disk, then the start
succeeded.

### 6. Idempotency — PASS (smith after fix 2)
Second apply: `20 ok`/`21 ok, 0 to add`, "nothing to do", no journal,
rc.conf, loader.conf, agent.json and api.json unchanged. On smith run
before part 5 (because of the temporary agent.json edit) and again after.

### 7. Upgrade — PASS (both)
- `skep-agent db backup` / `skep-api db backup`: directory `0700`, files
  `0600 root:wheel` with `.sha256`.
- PKG_UPGRADE: above. Real `pkg upgrade` with jails running: not refused.
- Early refusal (a jail stop slowed by `sleep 90` in the jail's
  `rc.shutdown.local`): exit 3, "0 task(s) pending and 1 running … nothing
  was changed"; the API kept running.
- Late refusal: a test helper submitted the slow stop on the agent socket
  the moment `service skep_api stop` ran (after setup's first look):
  "waiting up to 10s", exit 3, `skep_api pid … runs
  /usr/local/sbin/skep-api at <new>`, "service skep_api start (after the
  failure)", no backup, the agent still the old build. While mismatched:
  `/api/v1/health` **503** `unavailable` ("skep-agent and skep-api are
  different builds"), every agent-backed call **503
  `agent_version_mismatch`** ("skep-agent runs 0.1.1+20c8731 and skep-api
  runs 0.1.2+20c8731 …"). Sessions survived the API restart.
- Success, after the task finished: `skep_api` stop → both backups in
  `/var/db/skep/backup/<old>+<commit>-<ts>/` → `skep_agent` stop and start
  → pid and installed file verified at the new build for the agent, then
  the API → health `ok`, exit 0. The running jail kept its filter node
  through every restart.
- smith's first attempt (0.1.0 → 0.1.1) injected the task 37 ms too late
  and ran the success path instead, with the injected stop interrupted
  (finding 7); the late refusal was then done with 0.1.1 → 0.1.2.

### 8. Delete and remove — see findings 2 and 4
- `pkg delete` with a running jail: refusal printed, files deleted, exit 0
  (both hosts). Restored with `pkg add` of the same package.
- `skep-setup remove` with the jail stopped: refused (exit 3) because a
  guest exists, as documented. The jail was deleted.
- `remove` (default): services stopped, both `_enable="NO"`, everything
  else kept. `rollback` of it: rc.conf back to the exact pre-remove bytes;
  services not started.
- `remove -revert-host -purge-data` (+ `yes` at the purge prompt):
  loader.conf original bytes; `/var/db/skep` reduced to `setup/`;
  agent.json and api.json deleted (copies in the journal); the skep user
  and group deleted; datasets under the pool's `skep` listed, none
  destroyed. rc.conf: on smith (20c8731) `kld_list=""` and
  `cloned_interfaces=""` stayed (finding 2), removed by the fixed build's
  `remove -revert-host` → **original bytes**; on langrishe (67fa267)
  original bytes directly, with the operator's own `zfs_enable` kept.
- `pkg delete` then succeeded on both.

### 9. Fresh apply + rollback (langrishe) — PASS
Apply (12 changes, journal 20261122T193925Z), `rollback
20261122T193925Z`: rc.conf `fd574992…` and loader.conf `a27b0c57…`,
byte-identical to before; agent.json, api.json and the media directory
removed. (A first try was refused because bridge0 still existed live
after part 8's revert, which the plan reports; the test script then
rolled back the newest journal, which was part 8's remove. The lines
were reverted again with `remove -revert-host` and langrishe rebooted
before the retry.)

## Decisions taken unattended

- Whole run on smith first, then langrishe (rather than alternating per
  part), so fixes found on smith could be built before langrishe.
- Fixed three setup bugs on #52 during the run (each with a test, pushed,
  CI green) instead of stopping; the hosts then ran the fixed packages.
- Installed `edk2-bhyve` with `pkg install` on both (setup's own "manual"
  item; needed for the VM smoke test).
- smith's template used the dist source, which needs `storage.dist_dir`:
  agent.json was edited temporarily and restored to setup's bytes
  (sha256 checked) right after the build; idempotency checked before and
  after.
- Jail NICs used DHCP (static addresses on VLAN 60 are not approved).
- The VM disk on smith got its device node by hand (volmode geom → dev
  on the new, empty zvol) to finish the filter check; not built into
  Skep.
- `PKG_UPGRADE` recorded with a separate probe package, not by changing
  the real package's script.
- The late upgrade refusal needs a task that appears after setup's first
  look; it was injected through the agent socket as root by a small test
  helper (not committed).
- After `pkg delete` ignored the refusal, the same package was reinstalled
  and the services restarted to put the host back, then the run went on.
- `skep-setup remove` refused with a stopped guest: the guest was deleted
  (not `-keep-guests`).
- A 0.1.2 package from the same commit was built for a second upgrade on
  smith.
- langrishe was rebooted once more in part 9 to drop the live bridge0
  before the fresh apply.

## State left for the rollback to fresh-151

- **smith:** no skep package or account; rc.conf and loader.conf at their
  original bytes; edk2-bhyve installed; pool `tank` on da1 with
  `tank/skep/{jail,vm,templates/e0ae4cf9…}` (template 372M; `zfs_enable`
  removed, so not imported at the next boot); `/var/db/skep/setup` (6
  journals); bridge0, vmm, nmdm, netgraph and if_epair live until a
  reboot; `~/skep-pkgs`.
- **langrishe:** no skep package; the `skep` account (409) from part 9's
  `pkg add` remains; rc.conf and loader.conf at their original bytes;
  edk2-bhyve installed; `zroot/skep/{jail,vm,templates/02cb4aff…}`
  (template 98M); `/var/db/skep/setup` (5 journals); no bridge or Skep
  modules (rebooted); `~/skep-pkgs`.
- Both: the local repository and its pkg configuration removed; no jail,
  VM, or Skep process.
