# M7c probe, part 2: netgraph `ng_bpf` on the host end (SR-02)

Probe only: no Skep code change and no ADR. Part 1 (pf, ipfw), the A–F
criteria, the tools and the setup are in [m7c-probe.md](m7c-probe.md).

**Status (2026-11-21): done.** Step 0 and the generator were done first
(sections 0 and 1). After Andy answered section 3, the run plan (section 2)
ran on hearst on 2026-11-21 from 21:44 to 22:00 UTC, unattended, with
Proxmox snapshot `p7-plus-b61e578`. Results are in sections 4 to 13, the
recommendation is in section 12. Run record:
[m7c-probe-netgraph-hearst.txt](m7c-probe-netgraph-hearst.txt) (every
command and its output, the harness, the filter files and the injected
frames). Full logs: `~/skep-test-logs/20261121T214311Z-hearst-m7c-ng-*.log`.

**Short answer.** A per-port `ng_ether lower → ng_bpf → upper` chain on the
host end **enforces all of A to F on hearst**, C included, per ingress port,
with no global sysctl and no firewall module. It is fail-closed while
being installed, and a reused interface name inherits nothing. It has three
real costs: (1) removing the node or the `lower` hook, or loading a
pass-all program, **fails open**, so the agent needs a health check; (2)
`ng_socket`, which cannot be unloaded, gives **every VNET jail's root a
raw-frame sender** through its own interface (the minimal template already
ships `ngctl` and `nghook`), so every port Skep enforces on must have its
filter; (3) about **12 % less single-stream TCP throughput** on an epair
in this VM. The **VLAN gap** (a single 802.1Q tag is invisible to BPF) is
closed by if_bridge `vlanfilter` with `ifuntagged` on each member, tested on
`bridge1`.

## 0. Step 0: what the source says (read before testing)

hearst's `/usr/src` is `REVISION="15.1"`, `BRANCH="RELEASE-p7"`, the same as
the running kernel (`releng/15.1-n283696-bb03631fe3b7`). Paths are relative
to `/usr/src/sys`. The modules `netgraph.ko`, `ng_socket.ko`, `ng_ether.ko`
and `ng_bpf.ko` and `/usr/sbin/ngctl` are present. None is loaded.

### 0.1 Order in `ether_input_internal()` (`net/if_ethersubr.c`)

For a frame received on a bridge member (the host end of a guest's epair,
or a tap):

1. line 585: `ETHER_BPF_MTAP(ifp, m)`. tcpdump on the host end sees every
   frame the guest sent, **before** any filter.
2. lines 611–634: **a single 802.1Q or Q-in-Q tag is taken out of the frame
   data** into `m->m_pkthdr.ether_vtag`, and `M_VLANTAG` is set. Only the
   outer tag moves. An inner tag stays in the data.
3. lines 641–651: **ng_ether claims the frame** (`ng_ether_input_p`) if the
   interface has a node.
4. lines 664–666: `BRIDGE_INPUT()`.
5. line 702: `ether_demux()` (the link pfil head; part 1, 2.1).

So the netgraph hook runs **before the bridge**, on the ingress port only.
That is what part 1 could not get from pf or ipfw.

### 0.2 ng_ether hooks (`netgraph/ng_ether.c`, ng_ether(4))

- **`lower`**: `ng_ether_input()` (245–256) sends every received frame
  (from the guest) out of `lower`, and the frame is gone from the normal
  path. Data **written into** `lower` goes to `ng_ether_rcv_lower()`
  (664–706), which **transmits** it out of the interface with
  `ether_output_frame()` (towards the guest).
- **`upper`**: data **written into** `upper` goes to `ng_ether_rcv_upper()`
  (712–741): `rcvif` is set to the interface, and **if the interface is a
  bridge member the frame goes to `BRIDGE_INPUT()`** (735), else to
  `ether_demux()`. **A frame written back after filtering does reach
  if_bridge, as if it had arrived on the port.** The bridge's learning and
  forwarding see the real ingress port.
  - Frames the **host stack** sends out of the interface through
    `ether_output()` are diverted to `upper` (`ng_ether_output()`,
    281–297, called at `if_ethersubr.c` 421–431). This is **not** the path
    bridge forwarding takes: `bridge_enqueue()` calls `if_transmit()`
    directly (part 1, 2.1), and `ether_output()` on a member hands off to
    `BRIDGE_OUTPUT()` at line 410, before the ng_ether call. So traffic the
    bridge sends **towards the guest never passes through the node**. Only
    traffic the host addresses to the member interface itself does (Skep's
    host ends have no address).
  - Connecting `upper` sets the interface's `if_hwassist` to 0 (482) and
    restores it on disconnect. That affects only host-stack output on that
    interface.
- **`orphan`**: frames `ether_demux()` would discard (`if_ethersubr.c`
  937–953). Not used.
- `autoSrcAddr` and `promisc` start at 0 (`M_ZERO`), so frames written to
  `lower` keep their source MAC and the interface is not made promiscuous.

### 0.3 Loading ng_ether changes nothing for interfaces with no hooks

- On load, `vnet_ng_ether_init()` (861–877) creates a node for every
  existing `IFT_ETHER`, `IFT_L2VLAN` and `IFT_BRIDGE` interface, in every
  vnet. On hearst: `vtnet0`, `vtnet1` and `bridge0` in the host vnet. New
  interfaces get one at `ether_ifattach()`.
- With no hooks connected: `ng_ether_input()` returns at once when `lower`
  is NULL (252) and the frame continues unchanged; `ng_ether_output()`
  returns 0 when `upper` is NULL (288); an orphan frame is freed, exactly
  what `ether_demux()` does without netgraph (`m_freem`). `if_hwassist` and
  promiscuous mode change only when `upper` is connected or a message asks.
  **So a node with no hooks costs one pointer check per frame and changes
  nothing.**

### 0.4 ng_bpf (`netgraph/ng_bpf.c`, ng_bpf(4))

- **A new hook drops everything** until a program is set
  (`ng_bpf_default_prog`, 217–223: `ret #0`, and no match hook).
  Connecting the hooks before loading the program is therefore
  **fail-closed**.
- `bpf_filter()` runs over the **mbuf data only** (471/473). Out-of-bounds
  loads return 0. A non-zero return is a match, and the frame is
  **truncated to the returned length**. The program must return
  `0xffffffff`, not a small "accept" value.
- `setprogram` checks `bpf_validate()` and `net.bpf.maxinsns` (512 on
  hearst) and returns `EINVAL` if either fails (560–563). The **old program
  stays in place** when the new one is rejected.
- The node removes itself when its last hook is disconnected
  (`ng_bpf_disconnect()`, 520–545).

### 0.5 The VLAN gap: BPF cannot see a single 802.1Q tag

Because of 0.1 step 2, a frame the guest sends with one 802.1Q tag reaches
ng_bpf **with the tag already removed from the data** (`M_VLANTAG`). The
program sees the inner ethertype. **A tagged frame with the correct MAC and
IPv4 source passes the filter**, and `BRIDGE_INPUT()` then forwards it
**tagged** to the other members unless the bridge does VLAN filtering
(`net/if_bridge.c` `bridge_vfilter_in()` 3280–3335: without
`IFBRF_VLANFILTER` "pass everything"). The forged-address cases stay
covered, because the inner frame is still checked. What is not covered is
"drop every VLAN-tagged frame": the guest can choose which VLAN its own,
correctly addressed traffic goes into. A Q-in-Q frame's inner `0x8100` is
still in the data and is dropped by the default-deny ethertype check.

Options, not tested yet:

- **if_bridge VLAN filtering** (15.x, ifconfig(8) `vlanfilter`,
  `ifuntagged`, `iftagged`): with `vlanfilter` on, a tagged frame from a
  member whose access list is empty is dropped. It is per bridge, not per
  port, so on `bridge0` the uplink `vtnet1` needs an `ifuntagged` too, and
  every member needs the same untagged VLAN. That is a Skep bridge-config
  decision. It is allowed on `bridge1` under the standing approval and can
  be tested there.
- **ng_vlan** does **not** help: a frame with `M_VLANTAG` and an
  unconfigured VID leaves on `nomatch` with the tag still set (`ng_vlan.c`
  537–581). It is also not approved for this probe.
- Plan: measure it (rawl2 now has `-vlan`) and record the result.

### 0.6 Unloading: `ng_socket` never, `ng_ether` not while an Ethernet interface exists

- `ng_socket.c` `MOD_UNLOAD` (1181–1191) returns `EBUSY` **unconditionally**
  ("can't do this yet"). Once loaded, `ng_socket` stays until reboot. So
  does `netgraph`, which it depends on.
- `ng_base.c` 3113–3115: a node type cannot be unloaded while nodes of that
  type exist (`type->refs > 1` → `EBUSY`). ng_ether's own comment (834–837)
  says it "won't try to unload us until all nodes have been removed, and
  that can't happen until all Ethernet interfaces are removed". `vtnet0`,
  `vtnet1` and `bridge0` will have nodes. The only way to remove them
  without a reboot is `ngctl msg vtnet0: detach` (`NGM_ETHER_DETACH`,
  which calls `ng_ether_detach()`), and the same for `vtnet1` and `bridge0`.
  That is a control message to the management interfaces' nodes, which this
  probe's limits did not foresee.
- `ng_bpf` can be unloaded once its nodes are gone.

**Consequence:** nothing survives a reboot (no `loader.conf` change), but
part 1's "kldstat as before the run" cannot be met. Without a reboot,
`netgraph`, `ng_socket` and `ng_ether` stay loaded after the probe, with
hook-less nodes on `vtnet0`, `vtnet1` and `bridge0` (0.3: no effect on
their traffic).

### 0.7 Side effect on jails: `ng_socket` gives every VNET jail's root netgraph

- `ng_socket.c` line 199: opening a netgraph socket needs
  `PRIV_NETGRAPH_CONTROL`. `kern/kern_jail.c` 4268 lists it among the
  privileges granted to a jail's root **when the jail has its own vnet**
  (`PR_VNET`, 4302–4303). No `allow.*` parameter controls it, and devfs
  rulesets do not apply (it is a socket, not a device).
- Netgraph names and nodes are per vnet (`ng_base.c` 186–188,
  `VNET_DEFINE_STATIC ... ng_name_hash`), and ng_ether creates the jail-side
  node in the jail's vnet when the epair end is moved in
  (`ether_reassign()`, `if_ethersubr.c` 1047–1063). So a jail's root
  **cannot see or touch the host-side filter node**.
- It **can** connect to the ng_ether node of its own interface (`skep0`)
  and write arbitrary frames into `lower`. That is the raw-frame capability
  part 1 found static jails do *not* have (no bpf, devfs ruleset 5). It
  needs an `ngctl`/`nghook` binary, or any program using `PF_NETGRAPH`
  sockets, inside the jail. The jail's root can copy one in. **Correction
  from the run (section 9):** nothing has to be copied in. The
  `minimal-151` template already contains `/usr/sbin/ngctl`,
  `/usr/sbin/nghook` and `libnetgraph.so.4`.
- With the host-side filter in place those frames are filtered like any
  other (0.1), so A–F would still hold. But **on any port without the
  filter, loading `ng_socket` turns a static jail into a raw-frame sender
  until reboot**. It also exposes netgraph node types (whatever modules are
  loaded) to jail roots, which adds kernel attack surface. For Skep this
  means: if the agent uses netgraph, every enforcing port must have its
  filter, and the ADR has to record the wider jail capability. The agent
  could also do its netgraph control through a different path; see 3.3.
- hearst has **no jails running** now (`jls` empty), so loading it today
  affects only the probe's own test jails.

### 0.8 VMs (tap)

`net/if_tuntap.c`: a tap is `IFT_ETHER` and calls `ether_ifattach()`
(1031–1056), so it gets an ng_ether node. Frames bhyve writes go through
`tunwrite_l2()` → `if_input` → `ether_input_internal()` (1958). That is the
same path as an epair host end, so the same `lower → ng_bpf → upper` layout
applies (not measured).

### 0.9 Lifecycle, from source

- Destroying the interface: `ether_ifdetach()` → `ng_ether_detach()`
  (353–368) removes the node "REALLY" and disconnects its hooks. ng_bpf then
  loses both hooks and removes itself (0.4). **Nothing is bound by name**,
  so a reused name starts with no filter. That is the opposite of part 1's
  pf/ipfw finding (part 1, 4.6).
- Failure mode to design for: if the ng_bpf node is shut down or a hook
  disconnected (`ngctl shutdown skf_epair0a:` by host root), `lower` becomes
  unconnected and frames go **straight to the bridge, unfiltered**. Removal
  fails **open**. Only host root can do it (0.7), but the agent's health
  check has to verify the hooks and programs.

## 1. The filter and its generator

[`tools/ngbpfgen`](../tools/ngbpfgen/) prints the ngctl script for one port
from (interface, MAC, IPv4, mode). Layout:

```
<if>:lower -> [jail] ng_bpf "skf_<if>" [bridge] -> <if>:upper
```

- Hook `jail`: the generated program, match → `bridge`, no-match → dropped.
- Hook `bridge`: `ret #0xffffffff` (pass all) → `jail`. It carries only
  host-stack output on `<if>` (0.2).
- Order: `mkpeer lower` first (from then on frames from the guest are
  dropped, 0.4), then `connect upper`, then the two `setprogram`s.

Static mode program for `02:5e:00:0a:00:10` / `192.0.2.10` (23
instructions):

```
(000) ld   [6]                          ; Ethernet source, bytes 0-3
(001) jeq  #0x25e000a   jt 2   jf 22
(002) ldh  [10]                         ; Ethernet source, bytes 4-5
(003) jeq  #0x10        jt 4   jf 22
(004) ldh  [12]                         ; ethertype
(005) jeq  #0x800       jt 7   jf 6
(006) jeq  #0x806       jt 10  jf 22    ; anything else: drop
(007) ld   [30]                         ; whole IPv4 header present
(008) ld   [26]                         ; IPv4 source
(009) jeq  #0xc000020a  jt 21  jf 22
(010) ld   [14]                         ; ARP htype 1, ptype 0x0800
(011) jeq  #0x10800     jt 12  jf 22
(012) ldh  [18]                         ; hlen 6, plen 4
(013) jeq  #0x604       jt 14  jf 22
(014) ld   [22]                         ; ARP sender MAC
(015) jeq  #0x25e000a   jt 16  jf 22
(016) ldh  [26]
(017) jeq  #0x10        jt 18  jf 22
(018) ld   [28]                         ; ARP sender IP
(019) jeq  #0xc000020a  jt 21  jf 20
(020) jeq  #0x0         jt 21  jf 22    ; RFC 5227 probe
(021) ret  #4294967295
(022) ret  #0
```

DHCP mode is the same, except that IPv4 passes after the header-length load
and the ARP sender IP is not checked (19 instructions).

Unit tests run every A–F case, plus near-miss MACs, a bad ARP `hlen`, an
in-data VLAN tag, Q-in-Q, other ethertypes and short frames, in both modes,
through an interpreter that follows `bpf_filter()`'s out-of-bounds rule.
They pass. `~/skep-test-logs/20261121T212727Z-go-test-ngbpfgen.log`. vet is
clean for linux and freebsd/amd64. (The first run caught a DHCP-mode program
that passed a 20-byte truncated IPv4 frame, which is why instruction 7 is
there.)

rawl2 gained `-vlan VID[,VID]` (802.1Q tags, outermost first) for 0.5;
`~/skep-test-logs/20261121T212558Z-go-test-rawl2.log`, 4/4 pass, FreeBSD vet
clean.

## 2. Run plan (run 2026-11-21; deviations in 4.1)

Same setup as part 1 (bridge1 192.0.2.0/24, attacker 192.0.2.10 on
`epair0a`, victim 192.0.2.20 on `epair1a`, tcpdump on `epair1a` and
`bridge1`, plus `epair0a` for what was sent). With the filter on
`epair0a`, frames dropped by ng_bpf never reach `BRIDGE_INPUT()`, so the
`bridge1` capture is meaningful evidence this time (unlike part 1, where
the bridge's bpf ran before pfil).

0. Safety: verify `jls` empty apart from test jails, new SSH session and
   API `/health` after each kldload. Record `ngctl list` before any hook.
1. A–F and C5, T5, plus V1/V2: tagged frames (one tag, valid inner / forged
   inner) and V3: two tags. Pass = absent on `epair1a` and `bridge1`.
   `ngctl msg skf_epair0a: getstats jail` before and after each test.
2. D in depth: jail↔jail, jail↔host (a temporary address on `bridge1`),
   broadcast, ARP both ways; confirm `bridge` hook counters stay 0 (the
   direction towards the jail does not pass through the node).
3. E with a DHCP test jail on `bridge0`, MAC-only filter on its host end:
   lease obtained and renewed (shortened with `dhclient` restart), forged
   MAC and forged ARP sender MAC dropped.
4. Lifecycle: destroy the epair, `ngctl list` shows the node gone; jail
   restart reusing the name starts unfiltered (proving no inheritance);
   timing of the pre-install window, measured with a sender started before
   the jail's interface is up.
5. Failure: `setprogram` with an invalid program (`EINVAL`, old program
   kept); `kldunload ng_bpf` with a node present (`EBUSY` expected); start
   with `ng_bpf` absent (`mkpeer` fails: unknown type; **wrong**, see 8.3:
   ng_socket autoloads the type for host root); agent restart with
   the filter present (counters continue).
6. In-jail netgraph (0.7): copy `ngctl`/`nghook` into the attacker jail,
   list its nodes (only its own), try to reach `skf_epair0a:` (should
   fail), inject a forged frame through `skep0:lower` (should be dropped by
   the host filter).
7. Tap (read-only): see 3.2.

## 3. Questions for Andy before the run (answered, 3.1)

1. **Snapshot name.** The prompt had the `<snapshot name>` placeholder.
2. **Modules that will not unload (0.6).** Choose one:
   - (a) leave `netgraph`, `ng_socket` and `ng_ether` loaded until the
     next reboot you schedule, with hook-less nodes on `vtnet0`, `vtnet1`
     and `bridge0` (no effect on their traffic, 0.3). `ng_bpf` is unloaded.
     **Recommended**: it sends nothing to the management interfaces' nodes.
   - (b) at cleanup, send `detach` to the ng_ether nodes of `vtnet0`,
     `vtnet1` and `bridge0` so `ng_ether` can unload. `ng_socket` and
     `netgraph` still stay. Touches the management interfaces' nodes.
   - (c) you reboot hearst after the run, which restores everything.
3. **The jail-side effect (0.7)** holds until reboot. No jails run on
   hearst now. OK to accept for the probe? For the ADR it is a real design
   point. Can the agent avoid leaving `ng_socket` loaded? No, `ng_socket`
   cannot be unloaded at all. So a Skep design built on netgraph means every
   VNET jail's root has netgraph sockets. That is acceptable only if every
   enforcing port has its filter.
4. **Tap for test 6.** No tap exists on hearst. OK to `ifconfig tap create`
   one bare tap (no bridge, no VM), run `ngctl list` / `ngctl show
   tapN:`, and destroy it? No hooks would be connected.
5. **VLAN gap (0.5).** OK to test `vlanfilter` on `bridge1` only (test
   bridge, standing approval), as a candidate fix?

### 3.1 Andy's answers (run prompt, 2026-11-21)

1. Snapshot `p7-plus-b61e578` (taken on swearengen with `qm snapshot 102`,
   crash-consistent).
2. Option (a): leave `netgraph`, `ng_socket` and `ng_ether` loaded; unload
   `ng_bpf`; remove every node and hook the probe created. Andy reboots
   hearst later.
3. The jail effect of `ng_socket` is accepted for the probe. Measure it from
   a static test jail and confirm it cannot see or alter the host-side
   filter node (section 9).
4. One bare tap: create, inspect with ngctl, destroy, no hooks (section 10).
5. `vlanfilter` on `bridge1` only; record the exact config and whether
   single-tagged frames are dropped (section 6).
6. Also test the fail-open case and work out a cheap health check
   (section 8).

## 4. Setup and run

### 4.1 Setup, and deviations from the plan

- As part 1: `bridge1` (no uplink) created and added to
  `network.bridges` in `agent.json` (backup, sha256 checked), agent and
  API restarted with no jail running (text inodes 3044883/3044885,
  `b61e578`, `/health` ok). Two static jails from `minimal-151` through
  the API as `s7oper`: **attacker** `m7c-attacker` (192.0.2.10/24, MAC
  `02:5e:00:0a:00:10`, host end `epair0a`) and **victim** `m7c-victim`
  (192.0.2.20/24, MAC `02:5e:00:0a:00:20`, host end `epair1a`).
- `rawping` and `rawl2` (rebuilt from e2682db, so with `-vlan`) in the
  attacker only; bpf unhidden in the attacker's own `/dev`, as in part 1,
  to model a DHCP jail's root (ruleset 6001 has `bpf*`).
- **Deviation 1, test 3 (DHCP lease on `bridge0`) not run.** The run rules
  allowed no interface other than test epairs, `bridge1` and one tap. A
  DHCP jail on `bridge0` adds its epair to `bridge0`. E was tested with the
  DHCP (MAC-only) program on the attacker's port on `bridge1` instead (E1
  DISCOVER from 0.0.0.0 with the real MAC, E2 the same from a forged MAC,
  E3 a forged ARP sender MAC). Not covered: a real lease and renewal
  through the filter.
- **Deviation 2, test 6 ran in the victim, not the attacker.** The attacker
  had bpf unhidden. The victim kept ruleset 5 (no bpf) and
  `allow.noraw_sockets`, so it shows exactly what netgraph adds for a
  static jail. Its filter case used a second filter on `epair1a`.
- **Deviation 3, agent restart with a filter** used a hand-made epair
  (`epair2a`, in `bridge1`) after both jails were stopped, because the
  standing approval allows agent restarts only with no test jail running.
- V4 (priority tag, VID 0) was not sent: `rawl2 -vlan` accepts 1–4094
  only. if_bridge treats VID 0 as untagged (`VLANTAGOF() == 0`), so it is
  the same case as an untagged frame for `vlanfilter`. Not measured.
- Loading: `kldload netgraph`, `ng_socket`, `ng_ether`, `ng_bpf` one at a
  time, each followed by a new SSH session and `/health` (all ok).
  `ngctl list` then showed nodes for `vtnet0`, `vtnet1`, `bridge0`,
  `bridge1`, `epair0a`, `epair1a` (no hooks); `ngctl types` counted 8
  ether nodes, the 6 host ones plus one `skep0` in each jail's vnet.
- **Control with the modules loaded and no hooks** (run 11: D1 A1 B1 C2 F1
  V1): everything delivered, C2 poisoned the victim (`192.0.2.10 at
  02:5e:00:00:00:01`), as in part 1's control. Loading the modules changed
  nothing (0.3 confirmed).

### 4.2 Filter install

`ngbpfgen -if epair0a -mac 02:5e:00:0a:00:10 -ip 192.0.2.10 | ngctl -f -`
(section 1's script; 0.01 s). Afterwards:

```
epair0a  (ether, ID 5):  upper -> skf_epair0a:bridge   lower -> skf_epair0a:jail
skf_epair0a (bpf):       bridge -> epair0a:upper        jail -> epair0a:lower
```

`getprogram "jail"` returned the 23-instruction program, `getprogram
"bridge"` the 1-instruction pass-all. `epair0a`'s flags and options were
unchanged (connecting `upper` changes only `if_hwassist`, 0.2).

## 5. Results: A to F

Frames of the forged traffic seen on `epair1a` (delivered to the victim)
and on `bridge1` (bridge input). The jail-hook counters are from the
repeat run with counters (17); the first static run (12) had the same
frame counts.

| Test | Requirement | static program | DHCP (MAC-only) program |
|---|---|---|---|
| D1, D2 | D | 6/6 delivered, ping OK both ways | D1 6/6, ping OK |
| A1 rawl2 forged Ethernet source | A | **0** (jail hook +2 recv, +0 match) | **0** |
| A2 `ifconfig skep0 ether <forged>` + ping | A | **only the restore's ARP from the real MAC (1)**; ping 100 % lost | same (1) |
| B1 rawping from 192.0.2.50 | B | only the ARP for it (2); the echo **dropped** | **delivered** (6): MAC-only by design |
| B2 rawping from 198.51.100.10 | B | **0** | delivered (2), by design |
| C1 ARP reply, sender IP .50 | C | **0**, victim table clean | not run (no IP check in DHCP mode) |
| C2 ARP reply, sender MAC forged in the payload | C | **0**, victim keeps `.10 at 02:5e:00:0a:00:10` | **0**, victim clean |
| C3 alias .50/32 + `ping -S` | C+B | its gratuitous ARP for .50 and the echo **dropped**; the normal ARP for .20 passes (2) | not run |
| C4 broadcast ARP claiming .20 | C | **0** | not run |
| C5 `arp -s .50 <forged> pub` | C | the forged `Reply .50 is-at 02:5e:00:00:00:01` **dropped**; only the victim's own request seen (1) | not run |
| T5 alias .60/24 + `ping -S` | B+C | as C3: gratuitous ARP and echo from .60 dropped | not run |
| E1 DHCP DISCOVER 0.0.0.0, real MAC | E | 0 (static mode drops it, correct) | **delivered (2)** |
| E2 the same from a forged MAC | E | **0** | **0** |
| E3 ARP, forged sender MAC (real Ethernet source) | E | (= C2) | **0** |
| F1 ICMPv6 to ff02::1 | F | **0** | **0** |
| V1 one 802.1Q tag (VID 5), real addresses | VLAN | **delivered (2)**: the gap of 0.5 | delivered (2) |
| V2 one tag, forged MAC | VLAN+A | **0** | **0** |
| V3 two tags (5, 7), real addresses | VLAN | **0** (inner 0x8100 fails the ethertype check) | not run |

**Every forged case is dropped before the bridge (`bridge1` 0 as well),
and every legitimate one passes.** Unlike part 1 (pf `ether` and ipfw), C1,
C2, C4 and C5 no longer poison the victim, C5 included, which needs no bpf.
The only thing that passes that should not is V1, closed separately in
section 6.

Note on V1: tcpdump on `epair1a` shows the delivered frames *untagged*,
because if_epair taps with `BPF_MTAP()`, not `ETHER_BPF_MTAP()`
(`if_epair.c` 314/371), so the `M_VLANTAG` tag kept in the mbuf header is
not printed. The `bridge1` capture shows `vlan 5`, `bridge_enqueue()`
strips the tag only when it equals the egress port's PVID
(`if_bridge.c` 2416–2420; no PVID here), and the victim did not answer
(no VLAN interface). On `bridge0` the tag would leave through `vtnet1`.

## 6. VLAN: if_bridge `vlanfilter` on `bridge1` (run 18)

| Config on `bridge1` | D1 (untagged) | V1 (VID 5) | V5 (VID 1) | V3 (5,7) | V2 / C2 / A1 |
|---|---|---|---|---|---|
| `vlanfilter`, nothing else | **dropped** (0 on `bridge1`/`epair1a`) | 0 | – | – | – |
| `vlanfilter` + `ifuntagged epair0a 1 ifuntagged epair1a 1`, ng_bpf on `epair0a` | 6/6, ping OK (D2 too) | **0** | **0** | **0** | 0 |
| the same with the ng_bpf node shut down (vlanfilter alone) | 6/6 | **0** | **0** | **0** | V2 0 (tagged, so dropped by vlanfilter) |
| `-vlanfilter`, untagged cleared, ng_bpf back | 6/6 | 2 (gap back) | – | – | – |

Exact commands:

```
ifconfig bridge1 vlanfilter
ifconfig bridge1 ifuntagged epair0a 1 ifuntagged epair1a 1
# new members get the PVID automatically with:
ifconfig bridge1 defuntagged 1     # epair2a added afterwards showed "untagged 1"
# undo:
ifconfig bridge1 -vlanfilter -ifuntagged epair0a -ifuntagged epair1a -defuntagged
```

- `vlanfilter` drops **untagged** frames on a member with no PVID
  (`bridge_vfilter_in()`, `if_bridge.c` 3316–3322), so it must never be
  turned on before every member, uplink included, has `ifuntagged` (or
  `defuntagged` is set before the members are added).
- With a PVID, a frame tagged with **any** VID not in the member's access
  list is dropped, including the PVID itself (V5, VID 1; the source says so
  at 3326–3330). Q-in-Q is dropped unless `qinq <member>` (3303–3313).
- With `vlanfilter` on, the `bridge1` capture showed nothing for dropped
  frames, so the bridge's bpf tap runs after `bridge_vfilter_in()`.
- **Answer for the VLAN gap:** `vlanfilter` plus a PVID on every member
  drops single-tagged frames from a guest. ng_bpf is not needed for it and
  cannot do it (0.5). It is per bridge: on `bridge0`, `vtnet1` needs a PVID
  too, and if the uplink carries tagged VLANs, `iftagged vtnet1 <list>`.
  **Not tested on `bridge0`** (run rules). That is a bridge-config decision
  for the ADR.

## 7. Lifecycle and install order

### 7.1 Destroy and reuse (run 22, API stop/start of the attacker)

- After the stop, `epair0a`, its ether node (ID 5) and `skf_epair0a` were
  gone from `ngctl list`. Nothing was left behind.
- After the start, the attacker's host end was `epair0a` again (description
  `skep:d9f783dc…:0`), with a **new ether node, ID 0x304, and no hooks**:
  it starts unfiltered and inherited nothing. Node IDs are not reused, so
  a check keyed by node ID notices a replaced interface even when the name
  is the same. (Part 1's pf/ipfw rules, bound by name, were inherited by
  the wrong jail.)

### 7.2 Install order and the unfiltered window (run 21, hand-made epairs)

A forged sender (`rawl2` on the epair's `b` end in the host vnet, 1 frame/s,
forged MAC, source .50) ran across the install:

- **X2, wrong order:** `addm` to `bridge1` at 21:58:18.205, filter at
  21:58:22.438. **5 forged frames reached `epair1a`**, the last at
  :22.317; none afterwards.
- **X3, right order:** sender started first, filter installed on the new
  `epair2a` at 21:58:33.721, then `addm` and `up` at :33.738. **0 forged
  frames reached `epair1a`**; 6 hit the filter and were dropped, and the 2
  legitimate frames from the right MAC/IP were delivered.

So an ng_ether node exists as soon as the interface exists
(`ether_ifattach()`), and **installing the filter before the host end joins
the bridge leaves no unfiltered window.** In Skep's start path that means:
create the epair, install on the host end, then `addm`, then move the `b`
end into the jail.

### 7.3 Agent restart (run 23)

`epair2a` with its filter, both jails stopped. Agent and API restarted
(inodes, `-version`, status, `/health` ok). The node kept its ID (0x2ff),
hooks and counters (8/2 before and after), and still dropped forged and
passed legitimate frames afterwards (12/4). The agent's start-up orphan
check found 0 (the hand-made epair has no `skep:` description). Filters are
kernel state: they survive an agent restart and are gone after a reboot,
with the interfaces.

## 8. Failure modes and the health check

### 8.1 Fail-open and fail-closed (run 14, attacker running)

| Change made by host root | Result | Evidence |
|---|---|---|
| `ngctl rmhook epair0a: lower` | **fail-open**: A1 4/4 and C2 delivered, victim poisoned; D1 OK | node left with only `bridge`↔`upper`; `getprogram "jail"`: ENOENT |
| reconnect `lower` to a new `jail` hook, no program yet | fail-closed: D1 100 % lost, 0 on `bridge1` | new hook has the drop-all default (0.4); counters `recvFrames=2`, no match |
| `setprogram` pass-all on `jail` | **fail-open**: A1, C2 delivered, victim poisoned | `recvMatchFrames == recvFrames` |
| `ngctl rmhook epair0a: upper` | fail-closed: D1 lost, A1 0 | matched frames have nowhere to go |
| `ngctl shutdown skf_epair0a:` | **fail-open**: A1, C2, D1 all delivered | `epair0a` has 0 hooks |
| interface destroyed and recreated (7.1) | unfiltered until reinstalled | new node ID, 0 hooks |

Only host root can make these changes (section 9). A jail root cannot.

### 8.2 What a cheap health check looks at (run 15)

Per enforcing port, two control messages on a netgraph socket:

1. `listhooks` to `<if>:` (the ether node, by name), one reply with both
   links: `ourhook="lower" peerhook="jail"` and `ourhook="upper"
   peerhook="bridge"`, both peers `type="bpf"` with the same node ID and
   name `skf_<if>`. Also record the ether node's ID: a different ID means the
   interface was replaced (7.1), and `getifindex`/`getifname` tie it to the
   port the agent created.
2. `getprogram "jail"` to `skf_<if>:`, compared instruction by instruction
   with the program the agent generated for that port. That catches a
   replaced or pass-all program (8.1).

Optional: `getstats "jail"`. `recvMatchFrames == recvFrames` over a long
period suggests a pass-all program, but it is not proof; the program
comparison is. Cost: 100 ports at two `ngctl` processes each took 0.88 s;
the same 200 messages batched in one `ngctl -f` were below `time`'s
resolution. An agent talking to a `PF_NETGRAPH` socket directly would be
the batched case.

The check finds a fail-open state only after the fact; it cannot prevent
one. The window is the check interval. I found no netgraph notification a
userland process receives when someone else removes a hook or shuts down a
node it is not connected to, so polling is the only option found (not
exhaustively researched).

### 8.3 Other failure cases (runs 21, 30)

- `setprogram` with an out-of-range jump, or with an invalid opcode:
  **EINVAL, the old 23-instruction program stays** (`getprogram`).
- `setprogram` for a hook that does not exist: ENOENT.
- `kldunload ng_bpf`, `ng_ether`, `ng_socket` while in use: **EBUSY** each.
- **`mkpeer` with `ng_bpf` not loaded loads it** for host root
  (`ng_socket.c` 275–299 calls `kern_kldload("ng_bpf")`). Step 0 predicted
  a failure; that was wrong. So an agent running as root does not have to
  load ng_bpf itself, but the ADR should still load it explicitly, so that
  a missing module is a clear start-up error rather than a side effect.

## 9. What a jail's root can do with netgraph (run 19)

From the **static victim jail** (devfs ruleset 5: no bpf;
`allow.noraw_sockets`; securelevel 2), as its root, with the template's
own `/usr/sbin/ngctl` and `nghook` (nothing copied in):

| Action | Result |
|---|---|
| `ngctl list` | 2 nodes: its own `skep0` (ether, ID 1, in the jail's vnet) and its ngctl socket |
| `ngctl types` | `bpf 1`, `ether 8`, `socket 1`: type names and **live node counts are global**, so the jail sees how many ether and bpf nodes exist host-wide (a small information leak) |
| `ngctl show skep0:`, `msg skep0: getifname/getenaddr/getifindex` | works: its own interface |
| `ngctl show skf_epair0a:`, `epair0a:`, `epair1a:`, `vtnet0:` | **ENOENT** |
| `ngctl show [5]:` (host ID of `epair0a`) | **ENOENT**; `[1]:` is its own `skep0` (IDs are per vnet) |
| `setprogram` pass-all to `skf_epair0a:`, `shutdown skf_epair0a:` | **ENOENT**; the host node was untouched (host view during the test: hooks and node ID unchanged) |
| `mkpeer skep0: tee …`, `mkpeer skep0: ksocket …` (types not loaded) | **EPERM**: the autoload needs `PRIV_KLD_LOAD` (`kern_linker.c` 1210) |
| `mkpeer . bpf x jail` (type loaded) | succeeds: a jail root can create nodes of any **loaded** type in its own vnet |
| `nghook skep0: lower < frame` | **sends any frame it likes out of `skep0`** |

Injection (two frames built on merrick: ICMP victim→attacker with a forged
Ethernet source, and an ARP reply `192.0.2.20 is-at 02:5e:00:00:00:01`):

- **J1, no filter on `epair1a`:** both frames reached `bridge1` and
  `epair0a`; the attacker answered the forged-source echo (to the victim's
  real MAC, from its ARP cache), and the forged ARP reply then changed that
  cache to **`192.0.2.20 at 02:5e:00:00:00:01`**. A
  static jail with no bpf and no raw sockets became a raw-frame sender.
- **J2, static filter on `epair1a`:** both frames seen on `epair1a`, **0 on
  `bridge1` and `epair0a`**, attacker cache clean; `skf_epair1a` jail hook
  4 received, 2 matched (the attacker's legitimate ping).

So the 0.7 reading holds: the jail cannot see or alter the host filter, but
**with `ng_socket` loaded, any VNET jail on a port without a filter can send
arbitrary frames**. That applies to every VNET jail on the host, not only
Skep's, until reboot.

## 10. Tap (run 20)

`ifconfig tap create` → `tap0` (if_tuntap is in GENERIC, nothing loaded). It
got an ether node at once (`tap0`, ID 0x2ea, 0 hooks); `getifname`,
`getifindex` (12) and `getenaddr` worked. `ifconfig tap0 destroy` removed
the node. No hooks were connected and no traffic was sent: VM filtering
through a tap is still **not measured**, only the node's existence and
lifecycle, which match the epair's.

## 11. Costs

- **Throughput** (TCP, one stream, 1000 MiB attacker→victim through
  `bridge1`, `dd | nc`, 3 runs each): with the filter 1.205–1.278 GB/s
  (six runs, mean 1.25), without 1.410–1.458 GB/s (mean 1.43): **about
  12–13 % lower** on this 4-vCPU VM. The epair path is CPU-bound here, so
  this is an upper bound for the per-frame cost, not a NIC-speed figure.
- **Latency** (500 pings at 100/s): avg 0.261 ms with, 0.200 ms without;
  the stddev (~0.1 ms) is larger than the difference.
- **Memory:** `netgraph.ko` 44 KB, `ng_socket.ko` 15 KB, `ng_ether.ko`
  13 KB, `ng_bpf.ko` 9 KB of kernel text; `netgraph_node` 2.8 KB for 14
  nodes.
- **Install:** one `ngctl -f` of 5 lines, 0.01 s.
- **Instructions:** 23 (static), 19 (DHCP), limit 512 (`net.bpf.maxinsns`).
- **Side effects of loading:** ether nodes on every Ethernet interface and
  bridge, no hooks (no traffic effect, run 11); netgraph for every VNET
  jail's root (section 9); modules stay until reboot (0.6).

## 12. Recommendation

**Use netgraph `ng_bpf` on the host end for everything it can do (A, B, C,
E, F), and if_bridge `vlanfilter` with a PVID per member for the VLAN
gap.** Do not build on ipfw, and do not accept C as a limitation.

Reasons:

1. It is the only mechanism tested that does **all of A–F, C included**,
   per ingress port, with no global sysctl, no loader tunable and no
   firewall module, so an admin's own pf or ipfw is untouched.
2. Nothing is bound by name: the filter dies with the interface and a reused
   name starts clean (7.1). ipfw's `recv <name>` has part 1's inheritance
   hazard.
3. Install is fail-closed (drop-all until the program is set), and
   installing before `addm` leaves no window (7.2).
4. ipfw would still leave C open, needs three global sysctls (one of which
   switches off pf's bridge filtering) and a persistent tunable to load
   safely.

Conditions the ADR must accept or design for:

- **Fail-open on removal** (8.1). Mitigation: a periodic health check per
  port (8.2) that compares the hook layout, the node IDs and the program,
  and a defined reaction (see the ADR).
- **`ng_socket` gives every VNET jail's root netgraph** (section 9), and it
  cannot be unloaded. Every port Skep enforces on must have its filter;
  ports Skep does not enforce on (other VNET jails on the host, if any) gain
  raw-frame sending. The health check covers Skep's ports only. Userland
  has no way to configure netgraph without `ng_socket`, so this cost comes
  with choosing netgraph.
- **VLAN** needs `vlanfilter` on every Skep bridge, with PVIDs on all
  members, uplinks included. That is a bridge-config change, untested on a
  bridge with a real uplink.
- **VMs** (tap) are not measured beyond the node lifecycle.
- **Throughput** cost of roughly 12 % on an epair in this VM.

"Something else" considered and not better: `ng_vlan` does not help with
the tag (0.5); doing A/B/F in ipfw and only C in netgraph combines the
costs of both.

## 13. Restore and verify (run 30)

- Test jails stopped and deleted through the API (tasks succeeded, `GET`
  404, container list empty); session logged out (204).
- Every node and hook the probe created removed: `skf_epair0a` went with
  the attacker's epair, `skf_epair1a` shut down, `skf_epair2a` shut down,
  hand-made `epair2` destroyed, the X4 epair destroyed, `tap0` destroyed.
  `bridge1` destroyed (no members left).
- `ng_bpf` unloaded (and again after the X4 autoload). Per option (a),
  **`netgraph`, `ng_socket` and `ng_ether` stay loaded** until Andy's
  reboot. `ngctl list`: `vtnet0`, `vtnet1`, `bridge0`, all with 0 hooks;
  `ngctl types`: `ether 3`, `socket 1`.
- `agent.json` restored with no jail running: sha256
  **`490bc08738a2dd835952e19c4f59e869857fd15a1a6ce8e8c0b9df1d1f3cb134`**
  (matches). Agent and API restarted: text inodes 3044883/3044885 match the
  installed files, `b61e578`, both running, `/health` ok. Backup removed.
- Final state: no jails; interfaces `vtnet0 vtnet1 lo0 bridge0`; `bridge0`
  members `vtnet1` only, flags unchanged; `net.link.bridge.*` as in the
  pre-check; `skeptest/skep/jail` empty; `/var/run/skep/jails` empty; no
  RCTL rules; only the host's devfs mounted; `/tmp/m7c-ng` removed; no
  stray tcpdump, rawl2 or nghook; `/` at 18 %. `/etc/rc.conf`,
  `/boot/loader.conf`, `/etc/sysctl.conf` mtimes Nov 15–16. New SSH session
  ok.
- SSH was never lost. `vtnet0`, `vtnet1` and `bridge0` were not touched
  (their ether nodes were created by loading `ng_ether` and got no hooks
  or messages other than `ngctl show`/`list`).

**Owed: Andy reboots hearst** to unload `netgraph`, `ng_socket` and
`ng_ether` (and to end the jail-side netgraph capability).

## 14. Changes on hearst during the run

Modules `netgraph`, `ng_socket`, `ng_ether` (still loaded) and `ng_bpf`
(unloaded; loaded twice, once by the X4 autoload); ether nodes on every
Ethernet interface (no hooks left); `bridge1` with a temporary address
192.0.2.1/24 (run 13) and `vlanfilter`/`ifuntagged`/`defuntagged`
(run 18), all undone, then destroyed; the `agent.json` edit and its backup;
three agent and API restarts; two test jails (rawping and rawl2 in the
attacker, two frame files in the victim, bpf unhidden in the attacker's
`/dev`); filters on `epair0a`, `epair1a`, hand-made `epair2a`; three
hand-made epairs; one tap; `/tmp/m7c-ng`.
