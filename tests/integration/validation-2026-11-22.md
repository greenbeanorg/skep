# Validation runbook for the next hearst session (written 2026-11-22)

The steps, in order, for Andy's next session on `hearst` (10.79.20.170,
FreeBSD 15.1-RELEASE-p7). Each step says who does it: **Andy** or
**Claude Code**. Claude Code does nothing on hearst before step 1 is done
and Andy has started the session (and, for an unattended part, named the
Proxmox snapshot).

What it validates on a real host, from main:

- **C1**: a VM NIC whose MAC address a Skep jail or another Skep VM holds
  is refused with 409 `vm_mac_in_use` (PR #37, merged as 3dc3d7e).
- **C4**: nameservers on a jail without a NIC are refused with 422
  `nameservers_require_nic`, at create, update and clone (PR #40).
- **SR-07**: two jails with the same static address on one bridge,
  started concurrently: exactly one starts, the other is refused with
  `container_address_in_use` (PR #44).
- **M7c item 10** (ADR 0011 Decision 12): after a reboot with `kld_list`,
  the netgraph modules are back and the next guest start is filtered.

Host state when this was written (read-only checks, 2026-11-22 ~07:00Z):
skep-agent and skep-api at b61e578; `/usr/local/etc/skep/agent.json`
sha256 `490bc08738a2dd835952e19c4f59e869857fd15a1a6ce8e8c0b9df1d1f3cb134`
(`network.bridges: ["bridge0"]`); no `kld_list` in `/etc/rc.conf`;
`ifconfig_bridge0="addm vtnet1 up"` (no `vlanfilter`); `if_epair`,
`netgraph`, `ng_socket`, `ng_ether`, `ng_bpf` loaded live only (they go
at the next reboot); `vmm` loaded from `loader.conf` with
`hw.vmm.vmx.cap.virtual_interrupt_delivery="0"`; no jails running, no
VMs running; API key pin
`sha256:779cc5ec8daa3b04cefd14dc6dc6c9ac1ed989a409831561a47ec7d05cc069dd`.
Re-check these at the start of step 2; if any differs, stop and tell Andy.

Conventions:

- `$S` is a fresh session directory on merrick:
  `S=~/skep-test-logs/$(date -u +%Y%m%dT%H%M%SZ)-validation; mkdir -p $S`.
  Every command's output goes there with `tee`.
- API calls use `tests/integration/jails/m7c-validation/api.py`, which
  trusts only the pinned certificate and reads passwords from
  `~/.skep-test-creds` without printing them. **Never print, cat, grep or
  log that file**, redacted or not. `api.py USER METHOD PATH [JSON] [--wait]`.
  The write calls below use the admin test user (`s7admin` in earlier
  runs; use whichever of the three test users has the admin role).
- Test objects are named `rb-*` and are deleted in step 7.
- Stop immediately if any security property fails, and report.

---

## Step 1 — rotate the three test passwords (Andy)

The test passwords were printed into a session on 2026-11-21 and are due
for rotation. Andy changes the passwords of the three test users (web
interface Users page, or `skep-api` on hearst), and writes the new ones
into `~/.skep-test-creds` on merrick in the existing format (one line per
user: `user role password`, mode 0600). Claude Code does not read, check
or touch the file; `api.py` reads it only inside the script.

Done when Andy says so. Claude Code then logs in once per user and checks
only the HTTP status:

```sh
cd ~/git/skep
node scripts/e2e-trust.mjs fetch 10.79.20.170:8443 \
  sha256:779cc5ec8daa3b04cefd14dc6dc6c9ac1ed989a409831561a47ec7d05cc069dd \
  tests/integration/jails/hearst-api.pem
rm -rf tests/integration/jails/m7c-validation/.tokens   # old sessions
for u in s7admin s7oper s7viewer; do    # the three test users
  python3 tests/integration/jails/m7c-validation/api.py $u GET /system >/dev/null && echo "$u ok"
done 2>&1 | tee $S/01-logins.txt
```

`login <user>: HTTP 200 (token not shown)` for each. A 401 means the file
and the host disagree: stop and ask Andy; do not retry in a loop (the
login limiter).

## Step 2 — back up and deploy main (Claude Code)

Pre-approved (CLAUDE.md, standing approvals): deploy with
`scripts/deploy-test.sh`, backups first.

```sh
# 2a. merrick: main, clean, built with the web interface
cd ~/git/skep && git checkout main && git pull --ff-only
git status --porcelain            # must be empty
git rev-parse --short HEAD | tee $S/02-commit.txt
gh run list --branch main --limit 1   # main's CI on this commit: green, or stop
make build-freebsd 2>&1 | tail -5 | tee $S/02-build.txt
sha256sum bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api bin/freebsd-amd64/skepctl | tee -a $S/02-build.txt

# 2b. hearst: the state above, unchanged
ssh hearst 'uname -r; jls -N name; sudo ngctl list | grep -c skf_;
  /usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-api -version;
  sudo sha256 -q /usr/local/etc/skep/agent.json' 2>&1 | tee $S/02-before.txt
# expect: no jails, 0 skf_ nodes, b61e578 twice,
# 490bc08738a2dd835952e19c4f59e869857fd15a1a6ce8e8c0b9df1d1f3cb134

# 2c. hearst: backups (services running: sqlite .backup is consistent)
B=/var/tmp/skep-backup-$(date -u +%Y%m%dT%H%M%SZ)
ssh hearst "set -e; sudo mkdir -m 0700 $B
  sudo sqlite3 /var/db/skep/agent/agent.db '.timeout 5000' \".backup $B/agent.db\"
  sudo sqlite3 /var/db/skep/api/api.db '.timeout 5000' \".backup $B/api.db\"
  sudo cp -p /usr/local/etc/skep/agent.json /usr/local/sbin/skep-agent /usr/local/sbin/skep-api /usr/local/bin/skepctl $B/
  sudo sha256 $B/*" 2>&1 | tee $S/02-backup.txt
echo "$B" > $S/02-backup-dir.txt

# 2d. deploy: installs, restarts API then agent in order, verifies the
# running text's inode, -version (the commit of 2a) and /health
scripts/deploy-test.sh hearst 2>&1 | tee $S/02-deploy.txt

# 2e. config unchanged, health
ssh hearst 'sudo sha256 -q /usr/local/etc/skep/agent.json' | tee $S/02-after.txt
python3 tests/integration/jails/m7c-validation/api.py s7admin GET /health | tee $S/02-health.json
```

Pass: `deploy-test.sh` ends with both services verified at the commit of
2a; `agent.json` still `490bc087…` (main has no config, schema or rc.d
change against b61e578, see `docs/installation/upgrading.md`; no
migration should run, and the agent's log shows none); health `ok`, or
`degraded` only by `bridge_vlan_unfiltered` (non-critical, no guest ports
yet). If the deploy fails, reinstall the binaries from `$B` with
`scripts/deploy-test.sh hearst /usr/local/sbin …` and stop.

## Step 3 — C1: a MAC held by a jail or a VM (Claude Code)

All objects are created, never started, so no NIC module is involved and
no guest runs.

```sh
A="python3 tests/integration/jails/m7c-validation/api.py s7admin"
$A GET /storage/templates | tee $S/03-templates.json     # pick a provisioned template id: $TPL
tasks() { ssh hearst "sudo sqlite3 -readonly -cmd '.timeout 5000' /var/db/skep/agent/agent.db 'SELECT count(*) FROM tasks'"; }

# 3a. a test jail with one NIC on bridge0 (its MAC is generated)
$A POST /containers "{\"name\":\"rb-c1-jail\",\"template_id\":\"$TPL\",\"nics\":[{\"bridge\":\"bridge0\"}]}" --wait | tee $S/03a-jail.json
JMAC=...   # .body.container.nics[0].mac_address from 03a (or GET /containers/{id})

# 3b. a test VM with one NIC, no disk (its MAC is generated)
$A POST /vms '{"name":"rb-c1-vm","cpu_count":1,"memory":"256M","nics":[{"bridge":"bridge0"}]}' --wait | tee $S/03b-vm.json
VMAC=...   # its NIC's mac_address

# 3c. a VM with the jail's MAC
N0=$(tasks)
$A POST /vms "{\"name\":\"rb-c1-x1\",\"cpu_count\":1,\"memory\":\"256M\",\"nics\":[{\"bridge\":\"bridge0\",\"mac_address\":\"$JMAC\"}]}" | tee $S/03c.json
N1=$(tasks)

# 3d. a VM with the other VM's MAC
$A POST /vms "{\"name\":\"rb-c1-x2\",\"cpu_count\":1,\"memory\":\"256M\",\"nics\":[{\"bridge\":\"bridge0\",\"mac_address\":\"$VMAC\"}]}" | tee $S/03d.json
N2=$(tasks)
echo "tasks: $N0 $N1 $N2" | tee $S/03-tasks.txt

# 3e. the refusals in the agent's audit log
ssh hearst "sudo sqlite3 -readonly -cmd '.timeout 5000' /var/db/skep/agent/agent.db \
  \"SELECT occurred_at, action, outcome, json_extract(details, '\$.error_code') FROM audit_log WHERE outcome = 'denied' ORDER BY id DESC LIMIT 2\"" | tee $S/03e-audit.txt
$A GET /vms | tee $S/03-vms.json      # rb-c1-x1 and rb-c1-x2 absent
```

Pass:

- 3c: HTTP 409, `error.code` `vm_mac_in_use`, details `held_by_kind`
  `container`, `held_by_id` the jail's ID, `held_by_name` `rb-c1-jail`.
- 3d: HTTP 409 `vm_mac_in_use`, `held_by_kind` `vm`, `held_by_name`
  `rb-c1-vm`.
- No task created: `N0 = N1 = N2`.
- Two audit entries, outcome `denied`, error code `vm_mac_in_use`, and no
  VM named `rb-c1-x1` or `rb-c1-x2`.

## Step 4 — C4: nameservers on a jail without a NIC (Claude Code)

Needs main at or after PR #40's merge (check `git log --oneline | grep
nameservers` in step 2a). If #40 is not on main, skip this step and say so.

```sh
# 4a. create: no NIC, nameservers -> 422, nothing created
N0=$(tasks)
$A POST /containers "{\"name\":\"rb-c4-x\",\"template_id\":\"$TPL\",\"nameservers\":[\"192.0.2.53\"]}" | tee $S/04a.json
N1=$(tasks)

# 4b. update: a NIC-less jail, then nameservers -> 422
$A POST /containers "{\"name\":\"rb-c4-src\",\"template_id\":\"$TPL\"}" --wait | tee $S/04b-src.json
SRC=...    # its id
$A PUT /containers/$SRC '{"nameservers":["192.0.2.53"]}' | tee $S/04b.json

# 4c. clone of the NIC-less jail, with nameservers -> 422
N2=$(tasks)
$A POST /containers/$SRC/clone '{"name":"rb-c4-clone","nameservers":["192.0.2.53"]}' | tee $S/04c.json
N3=$(tasks)
echo "tasks: $N0 $N1 / $N2 $N3" | tee $S/04-tasks.txt

# 4d. controls: the same requests without nameservers are accepted
$A POST /containers/$SRC/clone '{"name":"rb-c4-clone"}' --wait | tee $S/04d.json
$A GET /containers | tee $S/04-containers.json   # no rb-c4-x
```

Pass: 4a, 4b and 4c each HTTP 422, `error.code` `nameservers_require_nic`;
`N0 = N1` and `N2 = N3`; `rb-c4-x` does not exist; `rb-c4-src` keeps no
nameservers (GET it); 4d succeeds. If the clone endpoint needs the source
snapshotted first, the 422 must still come before any task (the check is
in `normalizeNICs`, shared by create, update and clone).

## Step 4b — SR-07: two starts of jails with the same address (Claude Code)

Needs main at or after PR #44's merge (check `git log --oneline | grep
SR-07` in step 2a). If #44 is not on main, skip this step and say so.
It must finish, with both jails deleted, before step 5: step 5b assumes no
guest ports on bridge0.

Two jails with the same static address on bridge0 (TEST-NET-1, which
nothing on VLAN 60 uses; bridge0 is the only bridge in `agent.json`, so
no config change is needed), started at the same moment. Whichever
reaches `MarkStarting` second is refused, either at submission (the other
is already running: HTTP 409) or when its start is recorded (the other is
starting: the task fails). Both refusals carry `container_address_in_use`.

```sh
# 4b-1. two NIC'd jails, same address, created (never refused at create)
for n in rb-sr07-a rb-sr07-b; do
  $A POST /containers "{\"name\":\"$n\",\"template_id\":\"$TPL\",\"nics\":[{\"bridge\":\"bridge0\",\"ipv4\":{\"mode\":\"static\",\"address\":\"192.0.2.77/24\"}}]}" --wait | tee $S/04b-1-$n.json
done
JA=...; JB=...   # their ids
$A GET /containers > /dev/null    # log in once, so the two calls below share the cached token

# 4b-2. both starts at once
$A POST /containers/$JA/actions '{"action":"start"}' --wait > $S/04b-2-a.json 2>&1 &
$A POST /containers/$JB/actions '{"action":"start"}' --wait > $S/04b-2-b.json 2>&1 &
wait
cat $S/04b-2-a.json $S/04b-2-b.json

# 4b-3. the result on the host and in the API
$A GET /containers/$JA | tee $S/04b-3-a.json
$A GET /containers/$JB | tee $S/04b-3-b.json
ssh hearst "jls -N name ip4.addr" | tee $S/04b-3-jls.txt   # read-only; VNET jails show no ip4.addr
ssh hearst "sudo sqlite3 -readonly -cmd '.timeout 5000' /var/db/skep/agent/agent.db \
  \"SELECT name, observed_state, started_once FROM containers WHERE name LIKE 'rb-sr07-%'\"" | tee $S/04b-3-db.txt

# 4b-4. clean up: stop the one that runs, delete both
$A POST /containers/<the running one>/actions '{"action":"stop"}' --wait | tee $S/04b-4-stop.json
for id in $JA $JB; do $A DELETE /containers/$id --wait; done | tee $S/04b-4-delete.txt
$A GET /containers | tee $S/04b-4-containers.json   # no rb-sr07-*
ssh hearst "ifconfig -l; ngctl list | grep -c skf_ || true" | tee $S/04b-4-host.txt   # no epair left, 0 skf_ nodes
```

Pass:

- Exactly one of the two jails is `running` (API and `containers` row);
  the other is `stopped` with `started_once` 0.
- The other one's refusal is either HTTP 409 or a task that `failed`,
  and in both cases `error.code` `container_address_in_use`, details
  `address` `192.0.2.77/24`, `bridge` `bridge0`, `held_by_name` the jail
  that runs.
- After 4b-4: no `rb-sr07-*` jail, no epair and no `skf_` node left on
  hearst.

If both jails run, that is a security property failing (two guests with
one address on one bridge): stop both, delete them, stop the session and
report.

## Step 5 — kld_list, bridge0 vlanfilter, reboot (Andy)

Andy, as root. The order is the one in `docs/installation/upgrading.md`,
"Before the upgrade", step 3. On hearst the management address
(10.79.20.170) is on `vtnet0`, which is **not** a member of bridge0
(members: `vtnet1` only, no address), so a wrong order here cuts guests,
not SSH; the Proxmox console is still the safer place for the reboot.

```sh
# 5a. modules, appended to any existing kld_list (+= cannot replace one)
sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"
sysrc -n kld_list            # must list all five (and anything there before)

# 5b. bridge0 VLAN filtering, live, uplink first (no guest ports on bridge0 now)
ifconfig bridge0 ifuntagged vtnet1 1
ifconfig bridge0 defuntagged 1
ifconfig bridge0 vlanfilter
ifconfig bridge0 | head -12  # bridge flags=1<VLANFILTER>; vtnet1 untagged 1
# 5c. and persistent
sysrc ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"

# 5d. reboot
shutdown -r now
```

After the reboot, Andy (or Claude Code, read-only, once Andy says hearst
is back):

```sh
ssh hearst 'sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery;   # must be 0
  kldstat -m vmm -m if_epair -m netgraph -m ng_socket -m ng_ether -m ng_bpf;
  ifconfig bridge0 | head -8;      # bridge flags=1<VLANFILTER>, vtnet1 untagged 1
  sudo service skep_agent status; sudo service skep_api status' 2>&1 | tee $S/05-after-reboot.txt
```

Pass: the tunable prints 0 (the nested-KVM workaround, `docs/administration/virtual-machines.md`;
a `VMLAUNCH` with it at 1 stops the whole hearst VM); all six modules
listed; `VLANFILTER` on bridge0 with `vtnet1` untagged 1; both services
running (`service status` needs sudo on hearst; unprivileged it lies).
If a service does not start or a module is missing, Andy fixes it from
the console; if bridge0 came up without `vtnet1` untagged 1, run
`ifconfig bridge0 -vlanfilter` and check the `rc.conf` line.

## Step 6 — M7c item 10: the next guest start is filtered (Claude Code)

Skep has no autostart (ADR 0009 decision 2c), so item 10 checks that the
first start after the reboot is filtered, for a jail and a VM.

```sh
# 6a. health before any guest
$A GET /health | tee $S/06a-health.json          # port_filters ok, bridge_vlan_unfiltered ok

# 6b. start the step-3 jail and VM (both NICs on bridge0)
$A POST /containers/$JID/actions '{"action":"start"}' --wait | tee $S/06b-jail.json
$A POST /vms/$VID/actions '{"action":"start"}' --wait | tee $S/06b-vm.json
```

The VM has no disk, so it boots to the UEFI shell; that is enough for its
tap to exist and be filtered. If the start is refused for the missing
disk, give it no media and note it, or skip the VM half and say so.

```sh
# 6c. filters present: one skf_ node per guest port, each ng_bpf with the program
ssh hearst 'sudo ngctl list | grep skf_; ifconfig bridge0 | grep -E "member|untagged"' | tee $S/06c-ngctl.txt
for p in $(ssh hearst "sudo ngctl list | awk '/skf_/ {print \$2}'"); do
  ssh hearst "sudo ngctl show $p:" ; done | tee -a $S/06c-ngctl.txt

# 6d. health after one check round (wait 15 s)
sleep 15; $A GET /health | tee $S/06d-health.json
ssh hearst "sudo sqlite3 -readonly -cmd '.timeout 5000' /var/db/skep/agent/agent.db \
  \"SELECT occurred_at, action, outcome FROM audit_log WHERE action LIKE 'port_filter.%' ORDER BY id DESC LIMIT 5\"" | tee $S/06d-audit.txt
```

Pass: both starts succeed (no `port_filter_unavailable`, no
`epair_unavailable`); one `skf_` node per guest port (jail epair and VM
tap) with `lower` and `upper` hooks; both ports on bridge0 untagged 1
(from `defuntagged`); health `port_filters` `ok` and
`bridge_vlan_unfiltered` `ok`; no `port_filter.missing` /
`port_filter.detached` audit entries after the starts. Then stop both:

```sh
$A POST /containers/$JID/actions '{"action":"stop"}' --wait | tee $S/06e-stop-jail.json
$A POST /vms/$VID/actions '{"action":"force_stop"}' --wait | tee $S/06e-stop-vm.json
ssh hearst 'sudo ngctl list | grep -c skf_' | tee $S/06e-ngctl.txt   # 0
```

## Step 7 — clean up and record (Claude Code)

```sh
for id in <rb-c1-jail> <rb-c4-src> <rb-c4-clone>; do $A DELETE /containers/$id --wait; done | tee $S/07-delete.txt
$A DELETE /vms/<rb-c1-vm> --wait | tee -a $S/07-delete.txt
$A GET /containers; $A GET /vms                # no rb-* left
ssh hearst "sudo rm -rf $(cat $S/02-backup-dir.txt)"   # only after Andy agrees the deploy stays
rm -rf tests/integration/jails/m7c-validation/.tokens
```

The binaries from main stay deployed (that is the point of step 2). The
`kld_list` and `ifconfig_bridge0` lines stay (Andy's persistent changes).
Record the results in `tests/integration/jails/results/` (C1, C4,
SR-07, item 10), update the "pending" notes in `README.md`,
`docs/architecture/overview.md`, `docs/installation/upgrading.md` ("Status
of this procedure") and `tests/integration/jails/results/m7c-validation.md`
item 10, and write the stop report to `~/stop-report.md`.
