# M7c probe, part 2: netgraph `ng_bpf` on the host end (SR-02)

Probe only: no Skep code change and no ADR. Part 1 (pf, ipfw), the A–F
criteria, the tools and the setup are in [m7c-probe.md](m7c-probe.md).

**Status (2026-11-21): Step 0 and the filter generator are done. Nothing
has been loaded on hearst and no test has run.** Two reasons:

1. The run prompt said `Proxmox snapshot: <snapshot name>`. The placeholder
   was never filled in, and CLAUDE.md allows an unattended hearst run only
   when the prompt names the snapshot.
2. Step 0 found that the approved modules **cannot all be unloaded again
   without a reboot** (0.6), and that loading `ng_socket` changes what
   every VNET jail's root can do until that reboot (0.7). Andy needs to
   decide on both before anything is loaded (section 3).

The only thing done on hearst was reading: `uname`, `kldstat`, `ifconfig
-l`, `jls` (no jails), `sysctl net.bpf.maxinsns`, and copying source files
from `/usr/src` to merrick.

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
  sockets, inside the jail. The jail's root can copy one in.
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

## 2. Run plan (ready, not started)

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
   with `ng_bpf` absent (`mkpeer` fails: unknown type); agent restart with
   the filter present (counters continue).
6. In-jail netgraph (0.7): copy `ngctl`/`nghook` into the attacker jail,
   list its nodes (only its own), try to reach `skf_epair0a:` (should
   fail), inject a forged frame through `skep0:lower` (should be dropped by
   the host filter).
7. Tap (read-only): see 3.2.

## 3. Questions for Andy before the run

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
