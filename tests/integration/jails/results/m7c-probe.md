# M7c probe: host-side anti-spoofing mechanisms (SR-02, ADR 0010 amendment y)

hearst, 2026-11-21, 20:30 to 20:57 UTC. Probe only: no Skep code change and
no ADR yet. Proxmox snapshot `p7-plus-b61e578` taken by Andy beforehand.
Andy approved temporary, non-persistent use of pf and ipfw (kldload, enable,
rules) for this probe only. Nothing went into `loader.conf`, `rc.conf`,
`sysctl.conf` or `pf.conf`.

Run record: [m7c-probe-hearst.txt](m7c-probe-hearst.txt) (every command and
its output, the harness, and the rule files). Full logs:
`~/skep-test-logs/20261121T203035Z-hearst-m7c-probe-*.log`.

## 1. Question and answer

Which mechanism can enforce A to F on the host end of a guest port that is a
bridge member, and leave every other interface alone?

| | Requirement |
|---|---|
| A | drop frames whose source MAC is not the recorded MAC |
| B | drop IPv4 whose source address is not the configured one |
| C | drop ARP whose sender IP or sender MAC is not the configured one |
| D | pass legitimate traffic, including normal ARP |
| E | MAC-only mode for DHCP ports: DHCP from 0.0.0.0 passes, a forged MAC is still dropped |
| F | drop IPv6 on a port with no IPv6 configured |

**Short answer.** No pf or ipfw configuration does all six. **C is out of
reach for both**: neither parses the ARP payload (source in 2.3 and 2.4,
confirmed by test). **ipfw layer 2 on the bridge path does A, B, D, E and F
per ingress port**, but only with three global sysctls and a loader tunable
(section 5). **pf cannot filter per ingress port on a bridge**: `ether`
rules never see forwarded unicast frames on the member, and pf's layer-3
rules (with `pfil_member`) do only B and F. Combining pf and ipfw adds
nothing for C. The recommendation (section 6) is a follow-up probe of
netgraph `ng_bpf` on the host end. Source reading says it could do all six,
C included, per interface and with no global sysctl. It needs Andy's
approval first because it loads netgraph modules.

## 2. Step 0: what the source says (read before testing)

hearst's `/usr/src` is `REVISION="15.1"`, `BRANCH="RELEASE-p7"`, the same as
the running kernel (`releng/15.1-n283696-bb03631fe3b7`). Paths are relative
to `/usr/src/sys`.

### 2.1 Where the link-layer hooks run on a bridge member

- `net/if_ethersubr.c` `ether_input_internal()`: a frame received on a
  bridge member goes to `BRIDGE_INPUT()` at line 666, **before**
  `ether_demux()` at line 702. The link pfil head (where pf `ether` rules and
  ipfw `layer2` rules hook) runs IN only in `ether_demux()` (lines 865–869),
  and OUT only in `ether_output_frame()` (lines 472–478).
- `net/if_bridge.c` `bridge_enqueue()` sends a forwarded frame with
  `dst_ifp->if_transmit()` (line 2448), not `ether_output_frame()`. So **a
  frame the bridge forwards passes neither link-layer hook**, in either
  direction.
- The one exception is `bridge_pfil()` lines 4030–4038: when
  `net.link.bridge.ipfw=1`, it runs the link pfil head **OUT on the
  destination member**, with the Ethernet header still on. That call is made
  only if something hooks the link head (`PFIL_HOOKED_OUT(V_link_pfil_head)`)
  and only if the inet or inet6 heads are hooked too (`PFIL_HOOKED_IN_46`
  guards the call in `bridge_forward()`, lines 2796 and 2828).
- `bridge_pfil()` returns at once unless `pfil_bridge`, `pfil_member` or
  `pfil_ipfw` is set (line 3963). ARP and REVARP are passed without filtering
  unless `net.link.bridge.ipfw_arp=1` (lines 4004–4007). Other non-IP types
  are dropped when `pfil_onlyip=1` (lines 4021–4026). With `pfil_member=1`
  the inet and inet6 heads run on the member, IN for the ingress port and OUT
  for the egress port (lines 4093–4098 and 4145–4150), with the Ethernet
  header stripped.
- Setting `net.link.bridge.ipfw=1` forces `pfil_onlyip`, `pfil_bridge` and
  `pfil_member` to 0 (`sysctl_pfil_ipfw()`, lines 773–776). These are VNET
  globals that affect every bridge, not one port.
- bridge(4) describes `pfil_member`, `pfil_bridge`, `pfil_onlyip`, `ipfw`
  and `ipfw_arp` the same way: "ARP and REVARP packets are forwarded without
  being filtered".

### 2.2 pf

- `netpfil/pf/pf_ioctl.c` lines 6918–6942: pf hooks its `ether` checks into
  the link pfil head IN and OUT whenever pf is loaded.
- `netpfil/pf/pf.c` `pf_test_eth_rule()` (line 5308) reads layer-3
  addresses only for `ETHERTYPE_IP` and `ETHERTYPE_IPV6` (lines 5363–5390).
  For ARP, `src` and `dst` stay NULL, and the `l3` checks are **skipped**:
  `else if (src != NULL && PF_MISMATCHAW(...))` (lines 5425–5436). An `ether
  ... proto 0x0806 l3 from X` rule therefore matches **every** ARP frame,
  whatever its addresses. The ARP sender fields cannot be matched.
- pf.conf(5) on 15.1 lists `l3` only in the grammar (`ether-rule = ...
  [ "l3" hosts ]`). The prose for layer-2 rules matches MACs only (`from
  <source> to <dest>` "with the specified source and destination MAC
  addresses"). It has no option like "received on", so an `out` rule cannot
  name the ingress port.

### 2.3 ipfw

- `netpfil/ipfw/ip_fw2.c` contains no ARP parsing at all (a grep for "arp"
  finds only `IPPROTO_CARP`). For a layer-2 frame, IPv4 fields are parsed
  only when `ether_type == ETHERTYPE_IP` (lines 1801–1803). So ipfw can match
  `mac-type arp` and the Ethernet MACs (`O_MACADDR2`, line 2023), but never
  the ARP sender fields.
- On the OUT path `iif = m_rcvif(m)` and `oif = args->ifp` (lines
  1905–1910), so a `recv <port>` match on the bridge's OUT call identifies
  the **ingress port**. `iface_match()` matches by name (`strncmp` with
  `if_xname`, or `fnmatch` for globs) or through an interface table.
- `netpfil/ipfw/ip_fw_pfil.c`: ipfw links its layer-2 hook into the link head
  only when `net.link.ether.ipfw=1` (lines 80–108, 660, 684–685). Without it,
  `net.link.bridge.ipfw=1` alone gives ipfw no layer-2 call.
- `default_to_accept` (`ip_fw2.c` lines 135–137, 224: `CTLFLAG_RDTUN`) sets
  the default rule's opcode in `vnet_ipfw_init()` (line 3740), a
  `VNET_SYSINIT` that runs for **every** existing vnet, the jails' included.
  `kern/kern_linker.c` registers a module's sysctls, which loads
  `CTLFLAG_TUN` values from kenv, at line 497, before the module's SYSINITs
  at line 501. So `kenv net.inet.ip.fw.default_to_accept=1` before `kldload
  ipfw` takes effect for the host and every jail.

### 2.4 A third candidate for C (not tested)

`netgraph/ng_ether.c`: `ng_ether_input_p` claims a frame in
`ether_input_internal()` before `BRIDGE_INPUT()` (if_ethersubr.c around line
640), and `ng_ether_rcv_upper()` hands frames coming back on the `upper` hook
to `BRIDGE_INPUT()` again. A host-end `lower → ng_bpf → upper` chain would
therefore see every frame from the guest before the bridge does. `ng_bpf`
runs BPF programs that can read any byte, ARP sender fields included. The
modules exist on hearst (`/boot/kernel/ng_ether.ko`, `ng_bpf.ko`). This
probe's approval did not cover netgraph, so it was not loaded.

## 3. Setup

- `bridge1` (no uplink, no address), standing approval. `bridge1` was added
  to `network.bridges` in `agent.json` (backup, sha256 checked), and the
  agent and API were restarted with no jail running (inode, `-version` and
  `/health` verified).
- Two static jails from `minimal-151`, made through the API as `s7oper`:
  **attacker** `m7c-attacker` (192.0.2.10/24, MAC `02:5e:00:0a:00:10`, host
  end `epair0a`) and **victim** `m7c-victim` (192.0.2.20/24, MAC
  `02:5e:00:0a:00:20`, host end `epair1a`). Skep sets `description
  skep:<uuid>:<slot>` on each host end.
- Forged MAC `02:5e:00:00:00:01`. Forged sources 192.0.2.50 (unused,
  on-subnet) and 198.51.100.10 (off-subnet).
- **Static jails have no bpf.** They use devfs ruleset 5. Only DHCP jails get
  6001, which unhides `bpf*` for dhclient. A static jail's root can still
  forge IPv4 (raw sockets), change its MAC (T4), add aliases (T5) and
  publish proxy ARP (C5 below), but cannot write raw frames. To test what a
  **DHCP** jail's root can send, bpf was unhidden in the attacker jail's own
  `/dev` only (`devfs -m <root>/dev rule apply path 'bpf*' unhide`). This is
  recorded as a deviation; it went away with the jail.
- Senders, inside the attacker jail only: `rawping` (forged IPv4, raw socket)
  and the new **`tools/rawl2`** (bpf frame sender: ARP with any payload
  addresses, ICMP and DHCP DISCOVER with any source MAC, ICMPv6 to ff02::1;
  unit tests 3/3 passed, `~/skep-test-logs/20261121T203611Z-go-test-rawl2.log`;
  FreeBSD cross-vet clean; `make lint` passed).
- Evidence per test: `tcpdump -n -e` on `epair0a` (what the attacker sent),
  on `bridge1` (bpf runs before the bridge's pfil call, so this is **not**
  proof of delivery), and on `epair1a` (**what the bridge delivered to the
  victim**), then the victim's `arp -an`. tcpdump timestamps are hearst's
  local time (UTC−5).

### 3.1 Tests

| Test | What the attacker does | Requirement |
|---|---|---|
| D1 / D2 | normal ping attacker→victim / victim→attacker, ARP caches cleared | D |
| A1 | rawl2: ICMP from 192.0.2.10 with Ethernet source = forged MAC | A |
| A2 | T4: `ifconfig skep0 ether <forged>`, ping, restore | A |
| B1 | T1: rawping from 192.0.2.50 (unused, on-subnet) | B |
| B2 | rawping from 198.51.100.10 (off-subnet) | B |
| C1 | rawl2: ARP reply to the victim, sender 192.0.2.50 / real MAC | C (sender IP) |
| C2 | rawl2: ARP reply to the victim, sender 192.0.2.10 / **forged MAC in the payload**, real Ethernet source | C (sender MAC) |
| C3 | T5: alias 192.0.2.50/32, `ping -S 192.0.2.50`, alias removed | C + B |
| C4 | rawl2: broadcast ARP claiming the victim's address 192.0.2.20 | C |
| C5 | **no bpf needed**: `arp -s 192.0.2.50 <forged> pub` in the attacker; victim pings .50 | C (sender MAC) |
| E1 | rawl2: DHCP DISCOVER 0.0.0.0→255.255.255.255 from the real MAC | E (pass in DHCP mode) |
| E2 | the same from the forged MAC | E (drop) |
| F1 | rawl2: ICMPv6 echo fe80::…→ff02::1 | F |

**Control (no filter, run 03):** every frame of every test reached the
victim. C1 left `192.0.2.50 at 02:5e:00:0a:00:10` in the victim's ARP
table. C2 changed it to **`192.0.2.10 at 02:5e:00:00:00:01`**, the
poisoning. In C5 the attacker's kernel answered `who-has 192.0.2.50` with
`Reply 192.0.2.50 is-at 02:5e:00:00:00:01` from its real Ethernet source,
and the victim stored the forged MAC. So **a static jail can forge the ARP
sender MAC without bpf**.

## 4. Results

### 4.1 Table: A to F per mechanism

Each cell: frames of the forged traffic seen on `epair1a` (delivered to the
victim), then the verdict. "Per port" means the rule names the attacker's
ingress port.

| | pf `ether` on the port (in) | pf `ether out` on the egress port, `bridge.ipfw=1` | pf L3, `pfil_member=1` | ipfw `layer2`, `bridge.ipfw=1` + `ether.ipfw=1` + `ipfw_arp=1` |
|---|---|---|---|---|
| **A** | A1 and A2 delivered (A1: 2 forged echo + 2 replies; A2: 8/8). **Fail** | A1: 0 delivered. Drops, but only as a blacklist of one MAC on the egress port; cannot tell the ingress port. **Not per port** | A1 and A2 delivered. **Fail** | A1: 0. A2: only the restore's ARP from the real MAC (1 frame); the forged-MAC ARP and echo were dropped. **Pass** |
| **B** | B1, B2 delivered. **Fail** | B1: only ARP (2); B2: 0. **Not per port** (same reason) | B1: only ARP (2); B2: 0. **Pass** | B1: only ARP (2); B2: 0. **Pass** |
| **C** | C1–C5 delivered; victim poisoned (C2: `.10 at 02:5e:00:00:00:01`). **Fail** | C2 delivered, victim poisoned. **Fail** | C1, C2, C4, C5 delivered, victim poisoned; C3: the ARP claim delivered, the echo from .50 dropped. **Fail** | C1, C2, C4, C5 delivered, victim poisoned; C3: ARP claim delivered, echo dropped. **Fail** |
| **D** | D1, D2 6/6, ping OK. Pass | D1 6/6. Pass | D1, D2 6/6. Pass | D1, D2 6/6 in both modes. **Pass** |
| **E** | E2 (forged MAC) delivered. **Fail** | not tested | DHCP mode: E1 delivered, **E2 delivered**. **Fail** (L3 rules cannot see MACs) | DHCP mode: **E1 delivered (2), E2 0**. **Pass**. In static mode E1 is dropped too, as it should be |
| **F** | F1 delivered. **Fail** | F1: 0. **Not per port** | F1: 0. **Pass** | F1: 0. **Pass** |

ipfw's counters after the static run (23): rule 10000 (forged MAC) 12
packets, 10010 (IPv6) 4, 10020 (ARP, unchecked) 23, 10030 (IPv4 from
192.0.2.10) 5, 10040 (rest of the port) 10. The ARP rule counts 0 bytes for
layer-2 ARP, a cosmetic quirk.

### 4.2 Do pf `ether` rules see frames on a bridge member at all?

Only the broadcast and multicast ones, and only as the copy delivered to the
host. Pass rules with counters on every interface (run 13), with D1, A1, C2,
C4 and F1 sent:

- `ether pass in on epair0a`: 5 packets, exactly D1's ARP request, C4's two
  broadcast ARPs and F1's two IPv6 multicasts. No unicast frame matched: not
  the echo requests, not A1, not C2.
- `ether pass out on epair0a`, `out on epair1a`, `out on bridge1`: 0.
- `in on bridge1`: 6, the bridge's re-injected multicast copies.

With blocking rules on the port (run 14), the block rules counted matches
(forged MAC 4, IPv6 2, rest 2), and **every one of those frames still
reached the victim**. Bridge forwarding had already sent them on before the
local copy reached `ether_demux()`. This is consistent with 2.1. I did not
read `bridge_input()`'s multicast branch line by line; the counters are
the evidence for the "local copy" reading.

### 4.3 Which direction keyword applies on the host end

- **`in` on the host end means "from the guest"** (epair0a's receive side).
  It works for pf L3 with `pfil_member=1` (B and F above) and for ipfw's
  IN calls. pf `ether in` never sees forwarded unicast (4.2).
- With `net.link.bridge.ipfw=1`, link-layer rules run **`out` on the
  destination member** (run 15: `ether block out on epair1a` dropped A1, B2
  and F1). pf `ether` has no "received on" match, so an `out` rule cannot
  express "frames that came in on the attacker's port". ipfw can:
  `layer2 ... recv epair0a` matches `m_rcvif`, the ingress port, on that
  OUT call (2.3). That is why ipfw passes A, B, E and F per port and pf
  does not.

### 4.4 Anchors (pf)

- Main ruleset: `ether anchor "skep/*"` plus `anchor "skep/*"`. Per-jail
  sub-anchors `skep/a5d310ee` and `skep/18c5d08e` loaded with `pfctl -a
  skep/<id> -f -` and flushed with `-F ethernet` / `-F rules` / `-F all`.
  After both sub-anchors were loaded, the main ruleset still printed exactly
  those two lines. Flushing a scratch sub-anchor (`skep/syntax`) removed
  only that anchor.
- `ether` and L3 anchors are separate (`pfctl -s Anchors` lists `skep`
  twice), and each needs its own reference line. Sub-anchor rules were
  evaluated through those references (their counters rose). **Not tested:**
  a main ruleset without the references. By pf's anchor semantics nothing
  in `skep/*` would then be evaluated. pfctl cannot add the reference lines
  to an admin's ruleset without reloading it (section 7, Q1).
- `pfctl -a skep/<id> -F all` prints `pfctl: Operation not supported by
  device` for one of its parts (no ALTQ in GENERIC). The rules are cleared
  and the anchor goes away. The empty parent `skep` stays listed until the
  main ruleset is flushed.
- pfctl quirks found on 15.1-p7:
  - `pfctl -nvf` prints `ether anchor "skep/*"` as `ether anchor "/*"`. The
    real load shows `"skep/*"` correctly. Display only.
  - **A negated IPv4 address in `l3` is stored and printed as an IPv6
    prefix**: `l3 from ! 192.0.2.10` shows as `l3 from ! c000:20a::/32`,
    both with `-n` and after loading. It still matches IPv4 correctly (run
    15: `out-neg-l3` dropped B1 and B2, 4 packets). The cause looks like the
    address family not being set for negated `l3` addresses. Worth an
    upstream report; Skep should avoid negated `l3` addresses.
  - pfctl adds `proto 0x0800` by itself to an `l3` rule with an IPv4
    address. Such a rule does not touch ARP. The "`l3` matches every ARP
    frame" behaviour from 2.2 applies only when the rule says `proto 0x0806`
    explicitly or has no IPv4 `l3` address.
  - A table in `ether ... l3 from <x>` loads (§32.1 assumed it would not).
    Not tested on traffic.

### 4.5 pf enabled with an empty ruleset

`kldload pf` and `pfctl -e` with no rules (run 10): a new SSH session
worked, the API answered `/health`, 0 states, `bridge0` membership
unchanged, and D1, A1, C2 and F1 behaved exactly as in the control.
**No effect on other traffic.** (pf's status line said "Disabled for 2
days" right after the load; its clock starts at boot. Harmless.)

### 4.6 What happens to rules when the epair is destroyed

pf L3 rules bound to `epair0a` (run 17):

- The attacker was stopped and `epair0a` destroyed. **The rules stayed**,
  still naming `epair0a`, and the kif stayed with its counters.
- Both jails were stopped and the **victim** started first. **It got
  `epair0a` and inherited the attacker's rules.** Then the attacker started
  on `epair1a` and pinged the victim. The victim's replies (source
  192.0.2.20, in on `epair0a`) were dropped by the attacker's `B-ip` rule
  (+5 packets) and the ping failed 100 %. Meanwhile **the attacker ran
  unfiltered**. ipfw `recv <name>` matches by name string as well (2.3), so
  it has the same hazard by source. Not run separately with ipfw.
- Binding to an **interface group** instead (`ifconfig epair1a group
  skepatk`, rules `on skepatk`): rules applied while the port was in the
  group (B-ip-grp +2). After a stop and start the new host end was not in
  the group, so **the rules stopped applying** (fail-open until something
  re-adds the group), and nothing else inherited them. Group names cannot end
  in a digit, so a UUID prefix needs a suffix.

### 4.7 ipfw load safety

- `kenv net.inet.ip.fw.default_to_accept=1`, then the safety job (`sleep
  600; kldunload ipfw`, nohup, root), then `kldload ipfw`. Checked before
  anything else: `sysctl net.inet.ip.fw.default_to_accept: 1`, `ipfw list`
  showed `65535 allow ip from any to any`, dmesg said "default to accept", a
  new SSH session worked, and the API answered `/health`.
- The tests needed more than 10 minutes, so after that check I replaced the
  10-minute job with a 30-minute one rather than run without a watchdog. It
  was cancelled at the end and never fired.
- As read in 2.3, the default rule is created in every vnet, the jails'
  included. With a deny default, loading ipfw would also have cut every
  running jail's network.
- `net.link.ether.ipfw=1` (needed, 2.3) makes ipfw see layer-2 frames on
  **every** interface (`ether_demux`/`ether_output_frame`), `vtnet0`
  included. All of those hit only the default allow (the Skep rules all say
  `recv epair0a`). A new SSH session was checked right after setting it.

## 5. Exact rules used

pf main ruleset (run 12):

```
ether anchor "skep/*"
anchor "skep/*"
```

pf `ether`, attacker anchor `skep/a5d310ee`, static mode (run 14, failed A–F):

```
ether block in quick on epair0a from ! 02:5e:00:0a:00:10 label "A-mac"
ether block in quick on epair0a proto 0x86dd label "F-ipv6"
ether pass in quick on epair0a proto 0x0806 label "C-arp-unchecked"
ether pass in quick on epair0a proto 0x0800 l3 from 192.0.2.10 label "B-ip-ok"
ether block in quick on epair0a label "rest"
```

pf `ether` on egress with `net.link.bridge.ipfw=1 ipfw_arp=1` (run 15):

```
ether block out quick on epair1a from 02:5e:00:00:00:01 label "out-forged-mac"
ether block out quick on epair1a proto 0x86dd label "out-ipv6"
ether block out quick on epair1a proto 0x0800 l3 from ! 192.0.2.10 label "out-neg-l3"
ether pass out on epair1a label "out-seen"
ether pass out on epair0a label "out-to-atk-seen"
```

pf L3 with `net.link.bridge.pfil_member=1 pfil_onlyip=0` (run 16). Static:

```
block in quick on epair0a inet from ! 192.0.2.10 label "B-ip"
block in quick on epair0a inet6 label "F-ipv6"
pass in quick on epair0a no state label "rest"
```

DHCP: the same without the `B-ip` line. `pfil_onlyip` was set to 0 so that
`pfil_member=1` would not start dropping non-IP frames on `bridge0`.

ipfw with `net.link.bridge.ipfw=1 net.link.bridge.ipfw_arp=1
net.link.ether.ipfw=1` (runs 23, 24). Static:

```
ipfw add 10000 set 10 deny layer2 not mac any 02:5e:00:0a:00:10 recv epair0a
ipfw add 10010 set 10 deny layer2 mac-type ipv6 recv epair0a
ipfw add 10020 set 10 allow layer2 mac-type arp recv epair0a
ipfw add 10030 set 10 allow ip from 192.0.2.10 to any layer2 mac-type ipv4 recv epair0a
ipfw add 10040 set 10 deny layer2 recv epair0a
```

DHCP (MAC-only):

```
ipfw add 10000 set 10 deny layer2 not mac any 02:5e:00:0a:00:10 recv epair0a
ipfw add 10010 set 10 deny layer2 mac-type ipv6 recv epair0a
ipfw add 10040 set 10 allow layer2 recv epair0a
```

Note: `layer2 ip from X to any` must be written `allow ip from X to any
layer2 ...`; the body comes first (run 21).

## 6. Recommendation

1. **Do not build on pf.** pf has no per-ingress-port layer-2 filter on a
   bridge on 15.1: `ether in` on the member never sees forwarded unicast
   (2.1, 4.2), `ether out` needs `net.link.bridge.ipfw=1` and still cannot
   name the ingress port (4.3), and L3 rules do only B and F. C is
   impossible.
2. **ipfw is the only tested mechanism that enforces A, B, D, E and F per
   port.** It costs a lot outside the port, though:
   - **three global sysctls**: `net.link.bridge.ipfw=1` (which also sets
     `pfil_member`, `pfil_bridge` and `pfil_onlyip` to 0 for every bridge,
     turning off any admin pf filtering on bridges unless they are set
     again afterwards), `net.link.bridge.ipfw_arp=1`
     and `net.link.ether.ipfw=1` (layer-2 ipfw on every interface);
   - **a persistent loader tunable**, because a plain `kldload ipfw` defaults
     to deny and would cut the host and every running jail;
   - **rules bound by interface name**, which are inherited when the name is
     reused (4.6);
   - no answer for C.
3. **C needs ARP payload inspection.** Neither pf nor ipfw has it, so no
   combination of the two closes C. Without C, a jail can still poison other
   guests' ARP caches (C1, C2, C4) and the gateway's. With A and B in place
   it can then read or black-hole the redirected traffic, but cannot send as
   the victim (B drops it). That is a confidentiality and denial-of-service
   gap, not full impersonation.
4. **Next step: a short netgraph probe**, with Andy's approval to load
   `netgraph`, `ng_socket`, `ng_ether` and `ng_bpf` (non-persistent, same
   restore rules). Per port: `epairNa:lower → ng_bpf → epairNa:upper`, with
   BPF programs for A, B, C, E and F. Reasons, all from source and still to
   be tested:
   - frames are seen before `BRIDGE_INPUT()` (2.4), on the ingress port only;
   - BPF can read the ARP sender fields, so C becomes possible;
   - the node hangs off the interface and dies with it, so a reused name
     inherits nothing;
   - no global sysctl, no firewall module, no clash with an admin's pf or
     ipfw.

   The same probe would also have to test VMs (tap host ends),
   performance, and what a jail's root can do about it (nothing, if the
   node lives in the host vnet). If netgraph also fails, fall back to ipfw
   for A, B, D, E and F, and document C as a known limitation in the ADR.

## 7. Open design questions for the ADR

1. **Coexistence with an admin's own firewall.** pf needs `ether anchor` and
   `anchor` lines in the admin's main ruleset; ipfw needs a reserved rule
   range and set, and an admin's `ipfw -f flush` removes Skep's rules. Both
   ipfw sysctls change behaviour for every bridge (`bridge.ipfw=1` disables
   `pfil_member` and `pfil_bridge`). Does Skep own the firewall, refuse to
   run beside one, or detect and warn? netgraph would avoid most of this.
2. **When the filter is not loaded.** Refuse to start a guest on an
   enforcing port (fail closed), start it with a loud health warning, or
   load the module itself? Loading ipfw safely needs the
   `default_to_accept` tunable to be set persistently first (Andy's
   approval: `loader.conf`).
3. **Rule lifecycle.**
   - Install before the host end joins the bridge, or at least before the
     jail runs: a guest can send as soon as its interface is up.
   - Remove at stop.
   - On agent restart, rebuild from the database and compare with the kernel
     state: rules survive an agent restart but not a reboot.
   - At boot, before any guest autostarts.
   - What happens to a rule whose interface disappears (4.6: pf keeps it).
4. **Binding.** Names are reused (4.6). Options: bind to an interface group
   added before the port comes up, rename the host end to a unique stable
   name, bind by ifindex through an ipfw interface table, or use netgraph,
   which needs no binding.
5. **Fail-closed behaviour.**
   - A failed install must fail the start.
   - Someone may unload the module or flush the rules underneath a running
     guest. Detect it with a health check, and decide whether guests are
     stopped or flagged.
   - What does the port do between start and install?
6. **DHCP ports.** MAC-only passes any IPv4 source from the recorded MAC.
   Should guests also be stopped from acting as a DHCP server (UDP source
   port 67) or sending IPv6 router advertisements where IPv6 is allowed?
7. **Traffic to the host itself.** Frames for an address on the bridge go
   through `ether_demux()`, not the forwarding path. ipfw with
   `net.link.ether.ipfw=1` sees them IN on the member; that was not tested
   (`bridge1` had no address).
8. **VMs.** A tap's host side is the tap itself; the same hooks should apply
   but were not measured.
9. **Static jails and bpf.** Static jails have no bpf (ruleset 5). C5 shows
   that the ARP sender MAC can be forged without it, so C does not depend on
   bpf.
10. **Scale.** ipfw rules are linear per port; `lookup` tables keyed by
    `src-mac` could help but cannot be combined with `recv` in one table
    key. Measure with many ports.
11. **Upstream.** The pfctl negated-`l3` IPv4 display bug (4.4).

## 8. Restore and verify (run 30)

- ipfw: rules in set 10 deleted; `net.link.ether.ipfw`, `net.link.bridge.ipfw`
  and `ipfw_arp` back to 0, `pfil_onlyip` back to 1 (`pfil_member` and
  `pfil_bridge` 0); `kldunload ipfw`; watchdog killed (its log empty, it
  never fired); `kenv -u net.inet.ip.fw.default_to_accept`. A new SSH
  session worked.
- pf (run 18, before ipfw): every anchor and the main ruleset flushed, 0
  states, `pfctl -d`, `kldunload pf`; `pfil_member` back to 0 and
  `pfil_onlyip` back to 1. (`net.link.bridge.ipfw`, `ipfw_arp` and
  `pfil_onlyip` were already restored after run 15.)
- Test jails stopped and deleted through the API (tasks succeeded, `GET`
  404, container list empty); API session logged out (204). `bridge1`
  destroyed (no members left).
- `agent.json` restored from the backup with no jail running: sha256
  **`490bc08738a2dd835952e19c4f59e869857fd15a1a6ce8e8c0b9df1d1f3cb134`**
  (matches). Agent and API restarted: text inodes 3044883/3044885 match the
  installed files, `b61e578`, both running, `/health` ok. Backup removed.
- Final state: `kldstat` as before the run (no pf, no ipfw; `if_epair`
  was already loaded); no ipfw kenv; `net.link.bridge.*` as in the
  pre-check; no jails; interfaces `vtnet0 vtnet1 lo0 bridge0`; **`bridge0`
  members `vtnet1` only**; `skeptest/skep/jail` empty;
  `/var/run/skep/jails` empty; no RCTL rules; only the host's devfs mounted;
  no probe files in `/tmp`; no stray tcpdump or watchdog; `/` at 17 %.
  `/etc/rc.conf`, `/boot/loader.conf` and `/etc/sysctl.conf` mtimes are
  Nov 15–16, before the run.
- SSH to hearst was never lost. `vtnet0`, `vtnet1` and `bridge0` were not
  touched. The global sysctls were changed only while their tests ran:
  `pfil_member` with `pfil_onlyip=0`, so `bridge0` (one member, nothing to
  forward) dropped nothing new.

## 9. Changes on hearst during the run, all undone

pf and ipfw loaded, enabled and unloaded; the kenv tunable; the
`net.link.bridge.*` and `net.link.ether.ipfw` sysctls; `bridge1`; the
`agent.json` edit and its backup; two agent and API restarts; the two test
jails (with `rawping` and `rawl2` inside, and bpf unhidden in the attacker
jail's `/dev`); interface group `skepatk` on a test epair; T4, T5 and proxy
ARP inside the attacker jail; `/tmp` harness files.

## 10. Commits

On `feat/m7c-antispoof` (no PR): `tools/rawl2` and its tests, the jails
README's tool note and spoofing row, this file and the run record. Test
tooling and docs; no Skep code, API, security behaviour or schema change.
