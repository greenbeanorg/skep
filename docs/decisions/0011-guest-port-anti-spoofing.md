# ADR 0011: Anti-spoofing on guest ports (SR-02, Milestone 7c)

- Status: **Accepted** 2026-11-21 by Andy, with the answers recorded in
  [Andy's answers](#andys-answers-2026-11-21) and the
  [additions on acceptance](#additions-on-acceptance). Where an answer
  settles something the draft left open, the decision text below has been
  changed to match; the answers are the authority if anything disagrees.
- Date: 2026-11-21 (proposed and accepted)
- Depends on: [ADR 0003](0003-read-only-discovery.md) (enabling a
  capability is an administrative action), [ADR 0004](0004-privilege-boundary.md)
  (typed operations, no shell), [ADR 0009](0009-first-vm-bhyve.md)
  (Decision 5 bridges and the management-interface rule, Decision 6b
  "Skep loads no modules", Decision 7a the per-VM runner owns the taps),
  [ADR 0010](0010-storage-and-jails.md) (Decision 10 jail networking,
  amendment h, and **amendment y**, which this ADR implements).
- Evidence: [m7c-probe.md](../../tests/integration/jails/results/m7c-probe.md)
  (part 1: pf and ipfw) and
  [m7c-probe-netgraph.md](../../tests/integration/jails/results/m7c-probe-netgraph.md)
  (part 2: netgraph), both run on `hearst` (15.1-RELEASE-p7) on
  2026-11-21. Section references below such as "P1 4.6" or "P2 7.2" point
  into those two files.

## Andy's answers (2026-11-21)

Given for an unattended run, recorded here verbatim in substance.

1. **Mechanism:** netgraph `ng_bpf` per port plus bridge `vlanfilter`.
2. **Talking to netgraph:** `ngctl -f -` with validated values, no shell.
3. **Health check:** 10 s interval. Reaction: event + audit, reinstall,
   `deletem` if the reinstall fails. If the same port fails **3 checks in
   a row**, detach it and stop reinstalling until the guest is restarted.
4. **IPv6:** a jail NIC with `ipv6_mode` `slaac` or `static`, and **every
   VM port**: ethertype 0x86dd allowed, MAC-only, with the RA guard. A
   jail NIC with no IPv6: IPv6 dropped. IPv6 source and neighbour-discovery
   pinning: a later ADR (backlog).
5. **Guards: yes.** Drop UDP source port 67 (handling the IPv4 header
   length and fragments correctly) on **every** port, static jails
   included, and ICMPv6 type 134 wherever IPv6 is allowed. The RA guard is
   documented as best-effort against extension-header evasion.
6. **Unfiltered bridges:** a bridge without `VLANFILTER`, or a port without
   a PVID: the guest starts, and the health check `bridge_vlan_unfiltered`
   reports it at WARNING. Not a start error. The installer sets
   `vlanfilter` by default (Milestone 10 note). Revisit refusing at 1.0.
   `bridge0` on `hearst` may be switched to `vlanfilter` live for the
   validation, with `vtnet1`'s PVID set first and nothing in `rc.conf`.
7. **VMs:** MAC-only. Backlog: an optional pinned address for VM NICs.
8. **Jail root and netgraph:** accepted and documented, including the note
   about VNET jails that Skep does not manage.
9. **Throughput:** accepted; re-measure on bare metal (Milestone 11).
10. **Names approved:** `port_filter_unavailable`, `port_filter_failed`;
    events `port_filter_missing`, `port_filter_restored`,
    `port_filter_detached`; health check `port_filters`. No API field, no
    config key, no migration.
11. **Validation plan approved**, in this order: module-missing first
    (the modules are not loaded on `hearst` now), then load them live,
    deploy, items 1–5 and 7–9 (a VM on `bridge1`, the upgrade case from
    b61e578 with a running jail, a DHCP test jail on `bridge0`, the agent
    restart with test jails running). The reboot/autostart item (10) is
    not run; it is listed for Andy with the exact `kld_list` line.

## Additions on acceptance

**(a) Only completed installs are checked; one lock per port.** A port is
in the health check's scope only once its install has completed, which is
the same as "it is a member of its bridge": `addm` is the last step of
Decision 3, so a port still being installed is never a member. Each port
has a lock, keyed by owner and NIC slot (not by interface name, which the
kernel reuses), held as an `flock(2)` on `port<slot>.lock` in the owner's
runtime directory (`<JailRunDir>/<id>/` for a jail, `<RunDir>/<id>/` for a
VM; both root-only, both gone at reboot). It works across processes,
which the VM runner needs. The lock is held by:

- **jail start**, from `epair create` until the host end is a bridge
  member;
- **the VM runner**, from `CreateTap` until `AddBridgeMember` for each NIC;
- **the health check**, for the check and the reaction on that port. It
  takes the lock without waiting; a port whose lock is held is skipped in
  that round (an install is running).

Under the lock the check reads the port again (exists, same `skep:`
description, member of the recorded bridge), reads the `ng_ether` node's
ID, and from then on addresses the node by ID (`[id]:`), never by name.
A port destroyed and its name reused during the check therefore makes the
check's messages fail with "no such node" instead of reaching the new
interface; that outcome is "port gone", not a failure.

Taps are created **once per VM run** (the runner's boot), not per guest
reboot: the bhyve reboot loop keeps the same taps (`runner.boot`). A VM
stop and start, or a runner that died and a new run, creates new taps
under the lock. (Decision 4's draft said "on every boot loop"; that was
wrong and is corrected below.)

**Who reinstalls.** The agent's port-filter checker owns the check and the
whole reaction (event, audit, reinstall, detach) for **every** port, jail
and VM alike. The runner installs a VM's filters at boot and does nothing
afterwards; it is blocked in the bhyve wait loop and has no audit log.
Jail start installs a jail's. Nothing else touches the filters.

**(b) Residual risk, stated.** A DHCP jail or a VM can still claim
**another IPv4 address** using its own recorded MAC (it can answer ARP for
an address it does not hold, and send from it). The forged traffic carries
the guest's recorded MAC, so it is traceable to the guest from any switch
or neighbour table, but it is not prevented. This is the documented limit
of MAC-only ports (amendment y.6 for DHCP jails; answer 7 for VMs), and it
is in the security guide's known limitations.

**(c) Control frames.** The program is default deny on the ethertype, so it
also drops from guests: STP/RSTP BPDUs and every other 802.3 length-field
(LLC) frame, LLDP (0x88cc), LACP and other slow protocols (0x8809), PAUSE
(0x8808), 802.1X (0x888e), and Q-in-Q tags left in the frame. A guest
cannot take part in the bridge's spanning tree or announce itself with
LLDP. This is intended.

## Summary

Amendment y of ADR 0010 decided **what** must hold on every guest port
(the host end of a jail's epair, a VM's tap), but not **how**:

| | Requirement (amendment y) |
|---|---|
| A | drop frames whose source MAC is not the MAC recorded by the API |
| B | drop IPv4 whose source address is not the configured one (static NICs) |
| C | drop ARP whose sender IP or sender MAC is not the configured one |
| D | pass legitimate traffic, including normal ARP |
| E | DHCP NICs: MAC pinning only; DHCP from 0.0.0.0 passes, a forged MAC is still dropped |
| F | drop IPv6 on a port with no IPv6 configured |

This ADR decides:

1. **netgraph on the host end**: `<port>:lower → ng_bpf "skf_<port>" →
   <port>:upper`, one BPF program per port, generated by the agent from
   the NIC's recorded MAC and addressing. Tested on `hearst`: A to F all
   hold, C included (P2 section 5).
2. **if_bridge VLAN filtering** for the one thing BPF cannot see, a single
   802.1Q tag (P2 section 6).
3. The filter is **installed before the port joins the bridge**, so there
   is no unfiltered window, and a failed install **fails the start**.
4. A **health check** that finds the fail-open states (a removed node or
   hook, a replaced program) and a defined reaction.
5. The modules are **loaded by the operator**, like `vmm` and `if_epair`.

It also records what this costs: every VNET jail's root gains netgraph
sockets (P2 section 9), and the filter costs about 12 % of single-stream
throughput on an epair in a small VM (P2 section 11).

## Context

Today a jail's root can forge source IPv4 within its subnet, take over
unused addresses, change its MAC, and poison other guests' and the
gateway's ARP caches (ADR 0010 section 31 T1–T5; P1 3.1 control run; P2
4.1 control). Static jails need no bpf for the last one: `arp -s <ip>
<mac> pub` forges the ARP sender MAC with the kernel's own ARP code (P1
C5). Amendment y made fixing this a **release blocker** and put the
enforcement on the host, per guest port.

## Decision 1: the mechanism

| Option | What the probes found |
|---|---|
| A. pf `ether` rules in a Skep anchor | **Rejected.** pf `ether in` on a bridge member never sees forwarded unicast: the bridge forwards in `BRIDGE_INPUT()` before `ether_demux()`, where the link pfil hook runs (P1 2.1, 4.2). `ether out` needs `net.link.bridge.ipfw=1` and still cannot name the ingress port (P1 4.3). pf does not parse ARP payloads, so C is impossible (P1 2.2). |
| B. ipfw `layer2` through the bridge's pfil (`recv <port>`) | **Rejected.** Does A, B, D, E, F per port (P1 4.1), but **not C**: ipfw has no ARP parsing (P1 2.3). It needs three global sysctls, one of which turns off pf filtering on every bridge, and a persistent loader tunable, because a plain `kldload ipfw` defaults to deny and cuts the host and every jail (P1 4.7, 6). Rules match by interface name, so a reused epair name inherits another guest's rules (P1 4.6). |
| C. ipfw for A/B/E/F plus netgraph for C | **Rejected.** All the costs of both. |
| D. Hide bpf from jails | **Rejected** (amendment y.2). It breaks `dhclient`, and C5 forges ARP without bpf. With `ng_socket` loaded, a jail can send raw frames without bpf anyway (P2 section 9). |
| E. **netgraph: `ng_ether` + `ng_bpf` on the host end** | **Chosen.** The `lower` hook gets every frame the guest sends **before** `BRIDGE_INPUT()`, on the ingress port only; frames written back on `upper` go to `BRIDGE_INPUT()` as if just received (P2 0.1, 0.2). BPF reads any byte, so the ARP sender fields can be checked. A–F hold (P2 section 5). No global sysctl, no firewall module, nothing bound by name. |

Why E is safe to choose despite being less common: its behaviour comes
from a few hundred lines of `ng_ether.c` and `ng_bpf.c`, read in P2
section 0 and confirmed by test on the target release, including the
failure cases (P2 section 8).

## Decision 2: the filter program

The agent generates one program per port from the NIC record. The
generator is `tests/integration/jails/tools/ngbpfgen` today (test tooling);
the build moves it into an agent package (`internal/portfilter`, name to
be settled), with its interpreter-based unit tests, which run every A–F
case, near-misses, short frames and tags in both modes (P2 section 1).

| NIC | Program on the `jail` hook (match → `bridge`, no match → dropped) |
|---|---|
| Static IPv4 (jail) | Ethernet source = recorded MAC; then ethertype 0x0800 with IPv4 source = configured address (whole header present), **or** 0x0806 with htype 1, ptype 0x0800, hlen 6, plen 4, sender MAC = recorded MAC, sender IP = configured address or 0.0.0.0 (RFC 5227 probe); **everything else dropped**, IPv6 included. 23 instructions. |
| DHCP IPv4 (jail) | The same, except IPv4 passes with any source and the ARP sender IP is not checked. 19 instructions. |
| VM (tap) | Skep records only a MAC for a VM NIC (`vm_networks` has no address), so a VM port gets the **DHCP (MAC-only) program**: A, the ARP sender MAC part of C, E. B and the sender-IP part of C are not enforced for VMs (a documented limitation, as for DHCP jails in amendment y.6). |
| Jail NIC with `ipv6_mode` `slaac` or `static` (answer 4) | As its IPv4 mode, plus ethertype 0x86dd **allowed MAC-only** (the Ethernet source check at the top still applies), except ICMPv6 type 134 (router advertisement) with ICMPv6 as the IPv6 header's next header, which is dropped (RA guard). IPv6 source and neighbour-discovery pinning: later ADR. |
| Jail NIC without IPv6 (`ipv6_mode` `none`) | 0x86dd dropped (F), as in the draft. |
| VM (tap), IPv6 (answer 4) | Every VM port allows 0x86dd MAC-only with the RA guard: VMs have no IPv6 setting, and dropping IPv6 would break guests that use SLAAC today. |

**Guards on every port (answer 5).** Every program, static jails
included, drops IPv4 UDP with **source port 67** (a guest acting as a DHCP
server). The program finds the UDP header from the IPv4 header length
(`IHL`, via `ldx 4*([14]&0xf)`), drops IPv4 whose version is not 4 or
whose IHL is below 5, and checks the port only in a datagram's **first
fragment** (fragment offset 0); later fragments carry no UDP header and
pass on, and without the dropped first fragment they never reassemble. A
first fragment too short to hold the UDP header makes the port load run
past the frame, which drops it. Where IPv6 is allowed, the RA guard above
applies. It is **best-effort**: an RA behind an IPv6 extension header
(hop-by-hop, destination options, fragment) is not recognised and passes.
That is documented as a known limitation.

The program on the `bridge` hook is a single `ret #0xffffffff`. It only
carries frames the host stack itself sends out of the host end, which
Skep's host ends never do (they have no address); traffic the bridge
forwards towards the guest does not pass through the node (P2 0.2, 5;
its counter stayed 0 in every run).

A program must return `0xffffffff` to pass (ng_bpf truncates a frame to
the returned length, P2 0.4), and must stay under `net.bpf.maxinsns`
(512 by default).

## Decision 3: install order and fail-closed install

**Jail start** (`hostops` `createJailEpair`, today: `epair create …
up`, rename the jail end, set its MAC, `addm`):

1. `epair create` (unchanged; the host end is up but has no peer sending
   yet, and is not a bridge member).
2. **Install the filter on the host end**, in this order (one `ngctl -f`
   batch, or the same messages on a netgraph socket):
   1. `mkpeer <host>: bpf lower jail` (from here on every frame from the
      guest is dropped: a new ng_bpf hook drops everything until a program
      is set, P2 0.4 and 8.1);
   2. `name <host>:lower skf_<host>`;
   3. `connect <host>: skf_<host>: upper bridge`;
   4. `setprogram` for `jail`, then for `bridge`.
3. **Verify** (Decision 5's check, once): hooks, node IDs, program.
4. Only then rename the jail end, set its MAC, and **`addm` the host end
   to the bridge**.
5. `jail -c` as today.

Any error in 2 or 3 fails the start with **`port_filter_failed`**
(Decision 10) and the epair is destroyed as on any other start failure;
nothing is retried silently.

**VM start** (the per-VM runner, `runner.boot`: `CreateTap`, then
`AddBridgeMember`): the same steps 2 and 3 between `CreateTap` and
`AddBridgeMember`. A failure ends the run with the VM in `failed` and the
reason, as a tap failure does today.

Evidence: with the filter installed before `addm` and a forged sender
already running, **0** forged frames reached another port; installed 4 s
after `addm`, **5** did (P2 7.2). The ether node exists from
`ether_ifattach()`, so the order is always possible.

Install time is about 0.01 s per port (P2 4.2).

## Decision 4: lifecycle

| Event | What happens |
|---|---|
| Guest start | Decision 3, under the port's lock (addition a). |
| Guest stop or delete | The host end (epair or tap) is destroyed as today; its ether node and the ng_bpf node go with it (P2 7.1, 0.9). No separate removal step. |
| A reused interface name | Starts with **no** filter and a new node ID; nothing is inherited (P2 7.1). Decision 3 installs a fresh one. The node ID, not the name, identifies a port's node in Decision 5. |
| Agent restart | Filters are kernel state and survive (P2 7.3). At start, before the socket is served (where the orphan check runs today), the agent runs Decision 5's check once on every running guest's ports and applies its reaction. |
| Guest reboot inside a VM run | The taps and their filters stay (the bhyve reboot loop reuses them); nothing to do. |
| VM stop/start, or a new run after a runner died | The runner destroys leftover taps and creates new ones, each through Decision 3 under the port lock. |
| Host reboot | Nothing survives (interfaces and nodes are gone). Guests that autostart go through Decision 3. The modules must already be loaded at boot (Decision 6), or every start is refused. |
| Upgrade from a Skep without filters | At the first agent start after the upgrade, running guests' ports are bridge members with no filter. The start-up check finds them and the reaction installs them (`port_filter_missing`, then `port_filter_restored`). If the modules are not loaded, the check reports every port as unfiltered (critical) and installs nothing. |

## Decision 5: detecting the fail-open states

Removing the node or the `lower` hook, or loading a pass-all program,
**fails open**: frames go straight to the bridge (P2 8.1, measured: A1 and
C2 delivered, victim poisoned). Removing `upper`, or a new `jail` hook with
no program, fails closed. Only host root can make these changes; a jail's
root cannot reach host nodes (P2 section 9).

**Health check `port_filters`**, per running guest port, two control
messages (P2 8.2):

1. `listhooks` on `<host>:`: the ether node's ID equals the one recorded
   at install, and exactly two links exist, `lower`↔`skf_<host>:jail` and
   `upper`↔`skf_<host>:bridge`, both to the same ng_bpf node ID recorded
   at install.
2. `getprogram "jail"` on `skf_<host>:` equals the program generated for
   the port, instruction by instruction (and `getprogram "bridge"` is the
   pass-all).

Cost: negligible when batched (P2 8.2: 200 messages below `time`'s
resolution). Interval: **10 seconds** (answer 3), a constant.
netgraph sends no event that a userland process can receive when someone
else removes a node, so polling is the only detection found; the window
is the interval.

**Reaction on a failed check** (answer 3):

1. a critical event `port_filter_missing` naming the guest, NIC slot and
   what was wrong, and an audit entry;
2. **reinstall** the filter at once, on the port as it is (Decision 3's
   step 2; the port stays a bridge member, so frames sent during the gap
   were already forwarded): whatever is left of the old graph on the
   ether node is removed first (`rmhook` of `lower` and `upper`), then
   the install script runs and is verified; success is
   `port_filter_restored` and an audit entry;
3. if the reinstall fails, **remove the port from the bridge** (`deletem`),
   so the guest is cut off rather than unfiltered: event
   `port_filter_detached`, an audit entry, error `port_filter_failed` in
   its details;
4. if the **same port fails 3 checks in a row** (reinstalls succeeded but
   the filter keeps disappearing), it is detached the same way and not
   reinstalled again until the guest is restarted (a new port);
5. `port_filters` stays critical while any port is unfiltered or
   detached. A detached port is recognised without memory, so also after
   an agent restart: a port of a running guest, lock free, not a member
   of its bridge.

The counter of consecutive failures is in the agent's memory, keyed by
the ether node's ID, so a new interface starts at zero; an agent restart
also resets it (the port then gets three more tries).

## Decision 6: the modules

ADR 0009 Decision 6b applies: **Skep loads no modules.** The operator adds
`netgraph ng_socket ng_ether ng_bpf` to `kld_list` (the installer writes it
in Milestone 10, next to `if_epair` and `vmm`). The capability check runs
`kldstat -q -m` for `ng_ether`, `ng_bpf` and `ng_socket` before any guest
start with a NIC, and refuses with **`port_filter_unavailable`** when one
is missing.

The check matters because `mkpeer` of an unloaded node type **loads it as
a side effect** for root (`ng_socket.c` 275–299, P2 8.3). Skep must not
rely on that.

What loading changes, all measured (P2 4.1, 0.3, 0.6):

- `ng_ether` puts a node on every Ethernet interface and bridge, in every
  vnet. With no hooks, a node costs one pointer check per frame and changes
  nothing (control run with the modules loaded: no effect).
- `ng_socket` never unloads (`MOD_UNLOAD` returns `EBUSY`); `ng_ether`
  cannot unload while Ethernet interfaces exist. Only a reboot removes
  them. That is fine for modules loaded from `kld_list`.

## Decision 7: static, DHCP and VM ports

Summarised from Decision 2:

| Port | A | B | C | E | F |
|---|---|---|---|---|---|
| Jail, static IPv4 | yes | yes | yes (sender MAC and IP) | n/a | yes (IPv6 dropped) |
| Jail, DHCP | yes | **no** (amendment y.6) | sender MAC only | yes | yes |
| VM (tap) | yes | **no** (no address recorded) | sender MAC only | yes | IPv6 allowed MAC-only (answer 4) |

Every port also has the DHCP-server guard; every port where IPv6 is
allowed has the RA guard (answer 5).

The two follow-ups from P1 section 7, a guest acting as a **DHCP server**
and a guest sending **router advertisements**, are settled by answer 5:
both are dropped (Decision 2).

## Decision 8: VLANs

`ether_input_internal()` moves a single 802.1Q tag out of the frame into
`M_VLANTAG` before `ng_ether` sees it, so BPF cannot see it (P2 0.5). A
guest can therefore send correctly addressed frames **into a VLAN of its
choosing**; the bridge forwards them tagged (P2 5, V1). On a bridge with a
trunk uplink, that frame leaves on the chosen VLAN. Two tags (Q-in-Q) are
dropped by the program's ethertype check (V3).

**Decided: the bridge does VLAN filtering.** if_bridge on 15.x has
`vlanfilter`: a member with a PVID (`ifuntagged`) accepts untagged frames
into its PVID and drops every tagged frame whose VID is not in its access
list, the PVID itself included; Q-in-Q is dropped unless `qinq` is set.
Tested on `bridge1` (P2 section 6): single tag, tag equal to the PVID, and
Q-in-Q all dropped, untagged traffic passes.

Bridges are the operator's (Skep creates none, ADR 0009 Decision 5). So:

- the operator configures each bridge in `network.bridges` with
  `vlanfilter`, `defuntagged <vid>` (so new members get the PVID), and the
  uplink's `ifuntagged`/`iftagged`;
- the agent **checks**, as the health check **`bridge_vlan_unfiltered`**
  (WARNING, answer 6), that every bridge with a running guest port has
  `VLANFILTER` and that each guest port on it has a PVID. A failure does
  not stop or refuse a guest. The installer sets `vlanfilter` by default
  (Milestone 10). Refusing starts on an unfiltered bridge is to be
  revisited at 1.0.

Caution, measured: `vlanfilter` on a member with **no** PVID drops all
its untagged traffic (P2 6). Turning it on for `bridge0` without first
setting the uplink's PVID would cut every guest on it. This was not tested
on a bridge with a real uplink.

## Decision 9: what jail root can do as a result

With `ng_socket` loaded, `PRIV_NETGRAPH_CONTROL` is granted to the root of
**every VNET jail** (`kern_jail.c`), and no jail parameter or devfs rule
controls it (P2 0.7). Measured from a static jail with no bpf and no raw
sockets (P2 section 9):

- it **can** send arbitrary frames out of its own interface with
  `nghook <if>: lower` (the minimal template ships `ngctl` and `nghook`).
  On a port **without** a filter that is full layer-2 spoofing; it
  poisoned another jail's ARP cache;
- it **can** create nodes of any already loaded type in its own vnet, and
  read global type names and node counts (`ngctl types`);
- it **cannot** see or address any host node, by name or by ID, cannot
  change or remove the host filter, and cannot load a new node type
  (`EPERM`, no `PRIV_KLD_LOAD`).

So the filter on the host end covers this exactly as it covers bpf: on a
Skep port the injected frames were dropped (P2 9, J2). Consequences:

- **every port Skep creates must have its filter**; Decision 3 makes that
  a start condition;
- VNET jails on the same host that **Skep does not manage** gain a raw-frame
  sender. The administration and security guides must say so, and the
  `kld_list` instructions must say why `ng_socket` is there.

## Decision 10: coexistence with an admin's pf or ipfw

- netgraph runs on the guest port before the bridge and before any pfil
  hook, so an admin's pf or ipfw rules are untouched and keep working
  after the filter (frames the filter passes reach the bridge, its pfil
  calls and `ether_demux()` as before). Skep changes no pf or ipfw state
  and sets no sysctl.
- Skep owns the `lower` and `upper` hooks of its own host ends and the node
  names `skf_<host>`. An admin's netgraph graph on Skep's host ends is not
  supported; on a fresh interface those hooks are always free, and the
  install fails (`port_filter_failed`) if they are not.
- The admin's own netgraph use elsewhere (other interfaces, `ng_ether` on
  physical NICs) is unaffected.

## Decision 11: API, configuration, schema and error codes

Each item needs Andy's approval (CLAUDE.md: API contracts, error codes,
persisted data and config format are "ask first").

**API**

- No new endpoint. No request field changes: the filter is derived from
  fields the API already validates (MAC, IPv4 mode and address, IPv6
  mode).
- New error codes, each on the existing start paths:
  - `port_filter_unavailable`: a netgraph module is not loaded. Same HTTP
    status and shape as `epair_unavailable` (task failure detail on start).
  - `port_filter_failed`: install or verification failed; details carry the
    guest ID, slot, host interface and the step.
  - (`bridge_vlan_unfiltered` is **not** an error code: answer 6 made it
    a WARNING health check.)
- New health checks: `port_filters` (critical) and
  `bridge_vlan_unfiltered` (warning, answer 6), in the existing health
  listing. The netgraph modules are reported where `epair_unavailable`'s
  capability is.
- New events: `port_filter_missing`, `port_filter_restored`,
  `port_filter_detached` (the port was removed from the bridge).
- Optional, **not proposed**: a read-only `filter` status per NIC in `GET
  /containers/{id}` and `GET /vms/{id}`. The health check carries the
  information; adding it later is backward compatible.

**Configuration**

- No new key is proposed. Enforcement is not optional (amendment y), so
  there is no switch to turn it off. The check interval is a constant.
- Operator configuration outside Skep: `kld_list` (Decision 6) and the
  bridge VLAN settings (Decision 8), documented in the installation guide.

**Schema**

- No migration. The program is computed from existing columns
  (`container_networks.mac_address`, `ipv4_mode`, `ipv4_address`,
  `ipv6_mode`; `vm_networks.mac_address`). Node IDs recorded at install are
  runtime state, held in memory by the agent and, for VMs, in the
  runner's state file (which already records the taps), and rebuilt by
  the start-up check.

**Behaviour changes visible to users**

- Guests can no longer use addresses or MACs other than the recorded ones
  (the point of SR-02). A static jail whose root changes its `rc.conf`
  address loses connectivity on the new address (ADR 0010 Decision 10
  already calls the record "descriptive" after a change; it becomes
  enforced).
- VMs: IPv6 is allowed, MAC-only, with the RA guard (answer 4). No guest
  can act as a DHCP server on its bridge (answer 5).
- Throughput: about 12 % lower single-stream TCP on an epair in a 4-vCPU
  VM (P2 11); latency differences were within noise.

## Decision 12: validation on `hearst`

Run after the build, with a Proxmox snapshot named in the prompt, in the
order of answer 11: item 6 first (the modules are not loaded on `hearst`
at acceptance time, so no reboot is needed for it), then load the modules
live, deploy, items 1–5 and 7–9. Item 10 is left to Andy.

1. **T1–T5 rerun** (amendment y.7) with `rawping` on `bridge1`, a second
   jail as victim, tcpdump on the host side: no forged frame leaves the
   port. Through the API, with the agent installing the filters.
2. **The probe cases through the agent**: A1, A2, B1, B2, C1–C5, E1–E3, F1,
   T5, V1–V3 for a static and a DHCP jail (P2 section 5 table as the
   expected result); D1/D2 and jail↔host.
3. **Ordering**: a forged sender in the test jail's `rc.local` (the
   earliest point a guest controls) shows 0 forged frames on the victim
   port across a start.
4. **Fail-open detection**: F1/F4 of P2 8.1 by hand on a running test
   jail; the health check turns critical within one interval; the reaction
   (Decision 5) happens and is audited. The reinstall-failure path: after
   removing `lower`, connect a hand-made test node to `<host>:lower` so the
   reinstall's `mkpeer` fails; the port must be detached from the bridge.
5. **Agent restart** with running filtered jails (approved for this run
   with test jails running, answer 11), and an **upgrade** run from
   b61e578 with a running jail (the start-up check installs the missing
   filter).
6. **Module missing**: start refused with `port_filter_unavailable`. Run
   first, while the modules are not loaded (answer 11).
7. **VM**: a VM on `bridge1` with the MAC-only program: A1-style and ARP
   sender MAC tests from inside the guest. Not measured in the probes.
8. **DHCP jail on `bridge0`** with a real lease and renewal through the
   filter (not done in P2, deviation 1).
9. **VLAN**: `vlanfilter` on `bridge1` with Skep's ports, and on `bridge0`
   live only, with `vtnet1`'s PVID set **first** (approved, answer 6);
   nothing in `rc.conf`. Also the `bridge_vlan_unfiltered` warning before
   it is switched on.
10. **Reboot**: needs Andy; not run in the validation run. After a reboot
    with `kld_list` set, an autostart guest comes up filtered.

## Consequences

- Closes SR-02 for jails on static and DHCP NICs, and for VMs except IPv4
  source pinning. Removes the release blocker of amendment y once built
  and validated.
- Adds three always-loaded modules and a kernel-wide netgraph capability
  for VNET jail roots.
- Adds a polling health check with a fail-open window equal to its
  interval.
- Bridge configuration (VLAN filtering) becomes part of the documented
  host setup.
- No new dependency in Go (the agent runs `/usr/sbin/ngctl` with typed,
  validated arguments, or speaks `PF_NETGRAPH` directly; question 2).

## Decisions for Andy (answered 2026-11-21)

The questions as they were put; the answers are in
[Andy's answers](#andys-answers-2026-11-21).

1. **Mechanism**: netgraph `ng_bpf` per port plus bridge `vlanfilter`
   (recommended), or ipfw for A/B/D/E/F with C documented as a limitation,
   or something else?
2. **How the agent talks to netgraph**: run `ngctl -f -` with a script
   built from validated values (recommended first: simplest, matches the
   existing `ifconfig` use), or implement the `PF_NETGRAPH` socket
   protocol in Go (no process per call, more code to review)?
3. **Health check**: interval (proposed 10 s) and reaction (proposed:
   event + audit, reinstall, detach the port from the bridge if the
   reinstall fails). Alternatives: report only; or stop the guest.
4. **IPv6**: for NICs with `ipv6_mode` `slaac` or `static`, what is
   enforced (MAC only for 0x86dd; or also the IPv6 source for static; or
   IPv6 stays unsupported until a later ADR)? And for **VMs**, which have
   no IPv6 setting at all: drop IPv6 (F, may break guests that use SLAAC
   today) or allow it MAC-only?
5. **DHCP server and router-advertisement guard** on guest ports: drop UDP
   source port 67 and ICMPv6 type 134 from guests (cheap), or leave for
   later?
6. **Bridges without VLAN filtering**: refuse guest starts
   (`bridge_vlan_unfiltered`), or start with a warning health check?
   And may `hearst`'s `bridge0` be switched to `vlanfilter` for the
   validation run (needs `vtnet1`'s PVID first)?
7. **VMs**: accept MAC-only for VM ports (no IPv4 source pinning, since
   Skep records no VM address), or add an optional address to VM NICs
   (an API and schema change, not proposed here)?
8. **Jail root and netgraph**: accept that `ng_socket` gives every VNET
   jail's root netgraph sockets (Decision 9), documented, with no
   mitigation beyond the per-port filter?
9. **Throughput cost**: accept about 12 % on epairs (measured on a small
   VM; to be measured again on real hardware in Milestone 11)?
10. **Error codes and events** in Decision 11: approve the names and
    the "no API field, no config key, no migration" scope.
11. **Validation plan** (Decision 12): approve, including which items need
    a reboot or `bridge0` changes, and whether the agent-restart test may
    run with test jails running.
