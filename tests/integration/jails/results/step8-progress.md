# Step 8 build: progress log

ADR 0010 phase 7b, step 8 (VNET networking, Decision 10; spoofing check
from amendment f), branch `feat/m7b-templates`. Started 2026-11-17 from
`cdc0b06`. hearst runs `57d74bb`.

**Approved on hearst for this run:** `kldload if_epair` (live, not
persistent: no `loader.conf`, no `kld_list`); create and destroy epairs
attached to bridge0 only; deploy this branch with `scripts/deploy-test.sh`
(undo: redeploy `57d74bb`); jails and datasets under skeptest from
minimal-ssh-151; static jail addresses only from an approved range on the
LAB VLAN. **Not approved:** net0, net1, bridge0's own settings, host
routes, pf, rc.conf networking.

## 1. Feasibility probe on hearst: STOPPED (DHCP cannot work as written)

**What was done.** `kldload if_epair` (exit 0; not in `rc.conf` or
`loader.conf`). Then Decision 10 by hand, before any code: a clone of
minimal-ssh-151 in a new dataset `skeptest/step8-probe` (0700), one epair
with the host end described `skep:probe8:0` and added to bridge0, the
jail end renamed `sjprobe8n0` with a fixed `02:` MAC and moved in by
`vnet.interface`, the step 7 baseline, `ifconfig_sjprobe8n0="SYNCDHCP"`
and sshd with a throwaway key in the jail's `rc.conf`. Record:
[step8-probe.txt](step8-probe.txt); script:
[../step8-probe/probe-net.sh](../step8-probe/probe-net.sh).

**Held as specified:** the epair creation, description, renaming, MAC,
bridge membership, and move into the jail; inside, only `lo0` and the
stable name. `jail -r` returns the jail end to the host under its stable
name, and destroying the host end removes both ends and the bridge
member. The host end, bridge0, and vtnet1 have no address (IPv6
IFDISABLED, no IPv4). With IPv6 enabled on its own end, the jail heard
nothing on VLAN 60 and reached no hearst address.

**Found:** DHCP, Decision 10's default, fails. `dhclient` needs
`/dev/bpf`, and devfs ruleset 5 (`devfsrules_jail_vnet`, fixed by
Decision 9) hides it ("Can't find free bpf"; rc then waits 30 s for a
default route). Static addressing works with ruleset 5, but the static
range in the run's approvals was not filled in (`<range>`), so no check
above the link layer could run: internet, isolation, SSH from merrick,
reach to `10.79.20.170`, and the spoofing tests all need an IPv4
address.

**Stopped** under the run's rule ("If the spec can't be built as
written, stop and report the options"). Options are at the end of
step8-probe.txt; I recommend A1 (a Skep-owned devfs ruleset, ruleset 5
plus `bpf*`, used only for DHCP jails, added by the agent at runtime and
checked before every start).

**Other proposals from the probe (small, for the ADR):**

- Production loading of `if_epair`: keep the operator's
  `kld_list="if_epair"` (the ADR's text, consistent with `vmm` in ADR 0009
  Decision 6b), with the capability check `epair_unavailable`; the
  installer (Milestone 10) writes it into its profiles. The agent must
  check `kldstat -m if_epair` before `ifconfig epair create`, because
  that command auto-loads the module.
- Skep sets `inet6 ifdisabled -auto_linklocal` on the host end itself,
  so the host never has an address on a jail's link whatever the host's
  IPv6 configuration (hearst gets this from its rc defaults, a host with
  IPv6 enabled would not).
- Create and describe the host end in one `ifconfig epair create
  description skep:<id>:<slot> up`, so a crash cannot leave an
  undescribed epair that Skep would never clean up.

**Decisions made.** The probe's location (a new skeptest dataset,
destroyed afterwards) and the jail end name `sjprobe8n0` (probe only; the
real naming scheme is part of the build).

**hearst state:** no jails; no epairs; bridge0 has vtnet1 as its only
member; `skeptest/step8-probe` destroyed; `if_epair` **loaded** (live
only); still `57d74bb`; nothing deployed.

## 2. Andy's answers recorded (2026-11-17): amendment h

**Done.** ADR 0010 amendment h, worked into Decisions 9, 10, and 16, the
hearst changes list (item 2: live only on hearst for step 8), and a new
"Step 8" section under the implementation notes:

- A1: a jail with a DHCP interface gets Skep's devfs ruleset (ruleset 5
  plus `bpf*`) under a fixed number, added by the agent at runtime and
  checked before every start; `devfs_ruleset_conflict` if the number
  holds other rules. Static jails keep ruleset 5. I chose the number
  **6001**: well away from the shipped 1 to 5 and the small numbers
  operators usually pick.
- `if_epair`: the operator's `kld_list`; Skep checks before any
  `ifconfig epair create` and refuses with `epair_unavailable`.
- The host end is created, described, and has IPv6 disabled in one
  `ifconfig` call.
- Static addresses are checked on hearst too, from a range on VLAN 60
  outside the Kea pool. **The range in Andy's message came through as
  the literal placeholder `10.79.60.X–10.79.60.Y`**, so the live static
  check waits for the real numbers; the code and its unit tests do not.
- `if_epair` stays loaded on hearst for this step.

## 3. Build (merrick)

**Done.** Commits `f439aed` (hostops), `c6fece1` (service, store,
orphan check), `315f852` (OpenAPI, web types), `1a26e5e` (native writer
test). `go test ./...`, `make lint` (gofmt, vet for Linux and FreeBSD,
staticcheck) and `npm run typecheck` pass on merrick.

- hostops: `JailSpec.NICs`. Before `jail -c`: `if_epair` checked
  (`epair_unavailable`), each bridge checked (allow-listed, no
  management member), ruleset 6001 ensured for a DHCP jail, epairs of an
  earlier run destroyed. One `ifconfig epair create inet6 description
  skep:<id>:<slot> ifdisabled -auto_linklocal up` per NIC; the jail end
  renamed `sj<10 hex>n<slot>` and given its MAC; the host end added to
  the bridge; `vnet.interface` in jail.conf; after `jail -c` the jail
  ends must be gone from the host. Stop and a failed start destroy the
  host ends. A failed create destroys only a new epair with this
  description and logs an undescribed one without destroying it.
  Command forms checked by hand on hearst first
  ([step8/commands.txt](step8/commands.txt)): `inet6` right after
  `create` works; `inet6` after `description` fails ("inet6: bad
  value") *after* the epair is created.
- Service: `nics` (bridge, MAC, IPv4 dhcp or static with an optional
  gateway) and `nameservers` on create and update, stored in
  `container_networks` (agent/0005, no migration) and `config`; rc.conf
  lines per NIC and `resolv.conf` (decision 13 names it) before the
  first start; after it only bridge and MAC may change
  (`container_started`).
- `internal/orphans`: epairs and taps with a `skep:` description whose
  owner is not running; a warning event per interface at agent start
  (after both observations) and the `orphaned_interfaces` health check
  (names no interface; cached 15 s).

**Decisions made (small, easy to undo):** the jail end name
`sj<first 10 hex of the ID>n<slot>`, with a create-time retry if another
container shares the prefix; at most 8 NICs; one gateway per jail;
`nameservers` only without DHCP; a generated MAC is left out of the
idempotency comparison; **no IPv6 addressing** (the schema has the
columns; nothing on VLAN 60 answered IPv6 in the probe, so it could not
be validated); the health check's text names no interface.

## 4. Deploy and the first DHCP jail: STOPPED (ruleset 6001 exposes host devices)

**Native tests** (the hostops test binary as root, from a scratch
dataset `skeptest/step8-scratch`, destroyed afterwards): 35 pass, 2 skip
by design, the new resolv.conf writer test with a planted link
included. [step8/hostops-native.log](step8/hostops-native.log)

**Deploy** of `1a26e5e` ([step8/deploy.txt](step8/deploy.txt)): the
agent logged `orphaned_interface_check found=0` before its socket.

**DHCP jail** `n8dhcp` (s7oper, minimal-ssh-151, one DHCP NIC on
bridge0; [step8/dhcp-jail.txt](step8/dhcp-jail.txt)): the start took one
second; ruleset 6001 was added; the host end `epair0a` was described
`skep:<id>:0`, with IPv6 IFDISABLED, AUTO_LINKLOCAL off, and no address,
in bridge0; the jail end `sj7b6993aad6n0` was gone from the host. Inside,
DHCP from 10.79.60.1 gave 10.79.60.103/24, default route 10.79.60.1,
and resolv.conf from DHCP (10.79.10.250, .249).

**Found:** the jail's `/dev` held almost every host device (`mem`,
`kmem`, `io`, `da0`, `pass0`, `netmap`, ...). devfs(8): "Include
commands in the referenced ruleset are not resolved." Ruleset 5 is only
includes of 1 to 4 plus `path pf unhide`, so `include 5` hides nothing.
Amendment h's ruleset ("ruleset 5 plus bpf*" as `include 5`) cannot be
built as written. Record and assessment:
[step8/devfs-include.txt](step8/devfs-include.txt). My unit test encoded
the same wrong rules, so it passed.

**Done at once:** the jail stopped and deleted, ruleset 6001 removed,
`57d74bb` redeployed (the approved undo), so no build with the bad
ruleset runs.

**Stopped** under the run's rule. Not done: isolation and spoofing
tests, SSH from merrick, stop/delete and kill -9 orphan checks on
hearst, static addressing (range not given).

**Options (for Andy):**

- **A. Ruleset 6001 mirrors ruleset 5's rules, plus bpf (recommended):**
  `100 include 1`, `200 include 2`, `300 include 3`, `400 include 4`,
  `500 path pf unhide`, `600 path bpf* unhide`. The same single level of
  includes as ruleset 5, so it resolves to exactly what ruleset 5 gives
  plus bpf. Checked line for line before every start, as now.
- B. Copy ruleset 5's rules at runtime and add bpf: follows a local
  change to ruleset 5, but 6001's content is then whatever 5 is, which
  is harder to check.
- C. Flatten completely (every `path` rule of 1 to 4 written out): no
  includes at all; long, and tied to 15.1's rulesets.
- D. Static addressing only for now (no bpf, no ruleset 6001).

**Also proposed, whichever option:** after `jail -c`, Skep lists the
jail's `/dev` and removes the jail (`container_start_failed`) if any
entry is outside an allow-list (`null zero crypto random urandom pts fd
stdin stdout stderr ptmx zfs fuse pf`, plus `bpf*` for a DHCP jail). It
applies to every jail and would have caught this. A new Decision 16
check: the `/dev` listing of a DHCP and a static jail.

**hearst state:** no jails, no epairs, bridge0 with vtnet1 only, devfs
rulesets 1 to 5 only, `if_epair` loaded (live), `57d74bb` deployed. The
API users s7admin, s7oper, s7viewer are unchanged.

## 5. Andy's answers recorded (2026-11-17): amendment i

**Done.** ADR 0010 amendment i, worked into Decisions 9, 10, and 16; the
open point under "Step 8" is closed.

- Ruleset 6001 is option A: `include 1` to `include 4`, `path pf
  unhide`, `path bpf* unhide`, checked line by line before every start.
- A `/dev` guard for every jail, run before any process starts in it:
  `jail -c` without `exec.start`, the jail's `/dev` listed on the host and
  compared with an allow-list, the jail refused and removed if anything is
  extra, and only then `/etc/rc` through Skep's own fixed `jexec` step
  (output to console.log, start timeout). How `/etc/rc` runs after the
  guard (`jexec -l -U root ... /bin/sh /etc/rc`) is my reading of
  "only then run exec.start": exec.start can only run inside `jail -c`,
  so a guard between creation and rc needs Skep to run rc itself.
- A new Decision 16 row "Jail devfs": a real DHCP jail's and a real
  static jail's `/dev` on hearst, compared; a refused start for an extra
  entry. The guard's test reads a real jail's `/dev`, not the ruleset
  text.
- Static range: Andy's message held no numbers again ("10.79.60.-
  10.79.60."), so I recorded his stated alternative: the live static
  test is skipped for now, unit tests only.
- The redeploy of the corrected build is approved.

**Not built** (Andy: "Then stop. Don't build.").

## 6. Andy's confirmation recorded, amendment i built and deployed (2026-11-17)

**Recorded** (`ac10276`): Skep runs `/etc/rc` itself with `jexec -l -U
root skep-<id> /bin/sh /etc/rc`; `persist`; `exec.stop` kept; an rc
failure or timeout stops and removes the jail (`container_start_failed`,
output kept in console.log); a half-started jail is handled by the
start-up sweep; the live static test is skipped. My reading, recorded as
such: a guard refusal and a sweep removal run **without** `exec.stop`.

**Built** (`49809c1` hostops, `79620ef` containers): ruleset 6001 as
`include 1` to `include 4`, `path pf unhide`, `path bpf* unhide`; the
jail.conf without `exec.start`; after `jail -c` the parameter check, the
interface check, and the `/dev` guard (the jail root opened without
following links, `dev` beneath it, `fstatfs` devfs mounted at
`<root>/dev`, top-level entries against the allow-list); then rc through
`jexec`, output appended to console.log (a file, not a pipe, so daemons
cannot hold the runner); `DiscardJail` and the start-up sweep of
containers still recorded `starting`. `go test ./...`, `make lint`
(gofmt, vet for Linux and FreeBSD, staticcheck) pass on merrick.

**Approved extras, listed before the deploy** (Andy, 2026-11-17): the two
removals without `exec.stop`; `observe` keeps a recorded stopped detail
(a failed start's reason was lost on the next read); one
`jail_devfs_checked` info line per start; the start task's progress text.

**Deployed** `79620ef` with `scripts/deploy-test.sh`
([step8/deploy-79620ef.txt](step8/deploy-79620ef.txt)); the agent's
start-up line now carries `half_started_removed`.

## 7. Step 8 validation on hearst (2026-11-17, unattended, spoofing excluded)

| Check | Result | Record |
|---|---|---|
| Jail devfs: guard refusal | **Passed.** Native test: the expected 6001 rules changed to 6001 plus `path full unhide`, real `StartJail`, real allow-list; refused with `extra_devices [full]`, removed before rc (empty console.log, jail.conf without exec.*), epair, jail end, ruleset, and dataset gone | [step8/guard-refusal.txt](step8/guard-refusal.txt) |
| Jail devfs: real jails | **Passed.** DHCP jail (6001): `bpf bpf0 fd null pts random stderr stdin stdout urandom zero zfs`; no-NIC jail (5): the same without `bpf bpf0`; host and jail views identical; no extras; the agent's `jail_devfs_checked` lines match | [step8/devfs-check.txt](step8/devfs-check.txt) |
| Clean shutdown | **Passed.** Stop appended rc.shutdown's output to console.log ("Stopping cron."); devfs unmounted | [step8/shutdown.txt](step8/shutdown.txt) |
| rc timeout | **Passed** (twice). `sleep 600` in rc.local: failed after 2m0s with `container_start_failed`; `jail -r` ran rc.shutdown (marker in rc.shutdown.local); jail, devfs, jail end, epair gone; output in console.log | [step8/rc-timeout.txt](step8/rc-timeout.txt) |
| SSH from merrick | **Passed.** root by key to 10.79.60.104 (DHCP); seen from 10.79.20.80; jailed, securelevel 2; password refused | [step8/ssh.txt](step8/ssh.txt) |
| Isolation | **Passed for TCP; one finding.** TCP to hearst (22, 8443), merrick, 10.79.20.1, and 172.16/12 and 192.168/16 samples dropped; internet, DNS (10.79.10.250/.249), NTP (10.79.60.1) work. **ICMP echo from VLAN 60 reaches 10/8 hosts, hearst included** (capture on hearst's vtnet0: the request arrives routed, hearst replies): the LAB rules pass ICMP echo. hearst has no other address a jail can reach; no link-local presence on VLAN 60 | [step8/isolation.txt](step8/isolation.txt) |
| Stop/delete cleanup | **Passed.** Stop destroyed the epair; delete of a jail with an orphan destroyed it (`interfaces_destroyed`) | [step8/cleanup.txt](step8/cleanup.txt) |
| kill -9 mid-start (sweep) | **Passed.** Restarted agent: task `interrupted`, jail removed without exec.stop (no new rc.shutdown marker), epair gone, container stopped with the sweep's detail | [step8/cleanup.txt](step8/cleanup.txt) |
| kill -9 + orphan | **Passed.** Jail removed by hand while the agent was down: at restart a warning `orphaned_interface` (epair0a, owner, slot), `found 1`, `/health` degraded on `orphaned_interfaces`; cleared by the owner's delete | [step8/cleanup.txt](step8/cleanup.txt) |
| Static addressing | **Skipped** (no approved range; unit tests only) | |
| Spoofing | **Not run** (waits for Andy at OPNsense) | |

**Seen on the way, not fixed:**

- While a start runs, a container keeps the previous start's failure as
  its `state_detail` (`MarkStarting` does not clear it). Cosmetic.
- Pre-existing flake: `TestNetworkRefusals` (internal/containers), the
  duplicate-MAC case, failed once in about 240 runs on merrick: the
  second create returned the first container ("a") instead of a refusal.
  The create path, untouched by this work; it also failed 0 of 200 runs
  with and without the amendment i diff. Not fixed (Andy).
- NTP answers to a hand-made query were intermittent for 10.79.60.1 and
  a public server alike (likely rate limiting of back-to-back queries).

**End state of hearst** ([step8/cleanup-end.txt](step8/cleanup-end.txt)):
`79620ef` deployed; no containers, jails, or epairs; bridge0 has vtnet1
only; `/health` ok; devfs rulesets 1 to 5 and Skep's 6001 (the corrected
rules, added by the agent, gone at reboot); `if_epair` loaded (live);
`skeptest/step8-scratch` destroyed; the users s7admin, s7oper, s7viewer
unchanged. **Paused before the spoofing tests.**
