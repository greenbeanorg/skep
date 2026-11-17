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
