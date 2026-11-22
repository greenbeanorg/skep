# M7c validation on hearst: guest-port anti-spoofing (ADR 0011 Decision 12)

- Date: 2026-11-22 (UTC), unattended run, Proxmox snapshot `p7-plus-b61e578`.
- Host: `hearst`, FreeBSD 15.1-RELEASE-p7, UFS root, test pool `skeptest`.
- Build under test: `f73f4fd` (branch `feat/m7c-antispoof`), deployed with
  `scripts/deploy-test.sh` (inode, `-version` and service checks passed).
- Transcript: [m7c-validation-hearst.txt](m7c-validation-hearst.txt)
  (concatenated from `~/skep-test-logs/20261122T003343Z-hearst-m7c-val-*.log`).
- Scripts: [../m7c-validation/](../m7c-validation/) (`cases.sh`, the probe
  cases with automatic verdicts; `failopen.sh`; `api.py`, the pinned API
  client; `rc.local.m7c`). Frame tools: `tools/rawl2` (new modes `dhcpoffer`,
  `ra`, `lldp`, `stp`), `tools/rawping`, `tools/pfscript` (prints the agent's
  program or install script for a hand-made port).
- Approved for this run by Andy: live `kldload` of netgraph, ng_socket,
  ng_ether, ng_bpf; live-only `vlanfilter` on `bridge0` with `vtnet1`'s PVID
  set first; a DHCP test jail on `bridge0`; agent restarts with test jails
  running. Not done (needs Andy): item 10, reboot with `kld_list`.

## Order (answer 11)

1. Item 6 with the modules **not** loaded (as found after the 2026-11-21 reboot).
2. `kldload netgraph ng_socket ng_ether ng_bpf` (live only).
3. Items 1, 2 (static, then DHCP), 7, 3, 4, 5, 8, 9, then the upgrade part of 5.

## Results

| Item | What | Result |
|---|---|---|
| 6 | Module missing: jail start and VM start with a NIC | **Pass.** Both HTTP 409 `port_filter_unavailable`, `missing_modules` all four, audited `denied`; no epair, tap or bridge member created; no module loaded by Skep (`kldstat` before and after) |
| — | Install by the agent | Every port the agent made had `lower`/`upper` to one `ng_bpf` node; the program read back with `getprogram` was **identical, instruction by instruction,** to the generated one (static jail 36 insns, DHCP jail 31, VM tap 36) |
| 1, 2 | T1–T5 and the P2 cases through the agent, **static** jail on `bridge1` | **Pass, 23/23** (D1 D2 A1 A2 B1 B2 C1–C5 T5 E1–E3 F1, guards G1 DHCP offer and G2 RA, control frames L1 LLDP and L2 STP, V1–V3). Every forged case: 0 frames on `bridge1` and on the victim's port; the victim's ARP table stayed clean. V1 (one tag, real addresses) passed before `vlanfilter`, the documented gap closed in item 9 |
| 1, 2 | The same with a **DHCP** jail on `bridge1` (MAC-only program) | **Pass, 22/22**: forged MACs, forged ARP sender MACs (C2, E3, C5), IPv6, DHCP offer, RA, LLDP, STP, Q-in-Q dropped; IP spoofing with the jail's own MAC (B1, B2, C1, C3, C4, T5) delivered **by design**, which is residual (b): the victim learnt .50 and .60 at the DHCP jail's own MAC |
| 7 | VM on `bridge1` | **Pass.** `m7c-vm` (empty disk, so UEFI network boot): its tap carried the VM program (MAC-only + IPv6 + RA guard), identical to the generated one, and was a bridge member only after the install. All 11 frames the firmware sent from the recorded MAC passed (DHCPDISCOVER, DHCPv6 solicit, router solicitation, neighbour solicitation, MLD with a hop-by-hop header): `recvFrames=11 recvMatchFrames=11` |
| 7 | The VM program against hostile frames | **Pass, 13/13**, emulated: no login on a guest was available, so the identical program (by `pfscript`, same package) was put on a hand-made epair whose other end was moved into a test jail. Forged MAC, forged ARP sender MAC, DHCP offer, RA, LLDP, STP, Q-in-Q dropped; IPv6 echo, ARP for another IP with its own MAC, DHCP discover passed |
| 3 | Ordering: forged sender in the jail's `/etc/rc.local` | **Pass.** The jail changed its MAC and sent spoofed-source packets from rc.local; 0 forged frames on `bridge1` and on the victim's port during the start; filter counters `recvFrames=6 recvMatchFrames=1` |
| 4 | Fail-open detection on a running jail | **Pass.** `rmhook lower`, `shutdown skf_<if>`, pass-all `setprogram`, `rmhook upper`: each found and repaired within one interval (8–10 s), audited `port_filter.missing` + `port_filter.restored`; health `port_filters` showed the repair until the next clean round; A1 dropped and D1 passing afterwards |
| 4 | Reinstall failure | **Pass.** `lower` removed and held by a socket node (`nghook`): the reinstall's `mkpeer` failed (`File exists`), the port was **detached** within 1 s (`port_filter.detached`, reason carries `port_filter_failed`), the jail lost its network (ping 100 % lost), and after the foreign node was gone nothing was reinstalled (0 audit entries in 12 s): the port stays detached until the guest restarts |
| 4 | Three failed checks in a row | **Pass.** `lower` removed after each repair on a DHCP jail's port: missing/restored (1), missing/restored (2), missing/**detached** (3, "the filter failed 3 checks in a row") |
| 5 | Agent restart with 5 filtered jails and the VM running | **Pass.** Filters untouched (same `ng_bpf` node IDs before and after), no `port_filter.*` audit entries |
| 5 | A filter removed while the agent was down | **Pass.** Restored by the start-up check at 20:02:03.104, **before** "internal API listening" at 20:02:03.117 |
| 5 | Upgrade from b61e578 with a running jail | **Pass.** b61e578 (rebuilt from that commit) deployed; a jail started under it had a bare port (0 hooks); after deploying `f73f4fd` the start-up check installed the filter (`missing` + `restored`, before the socket was served), program identical, a forged MAC from that jail no longer reached the victim |
| 8 | DHCP jail on `bridge0`, real lease | **Pass.** Lease 10.79.60.116 from 10.79.60.1 through the filter, renewal (`service dhclient restart`) through it, all DHCP frames matched; a forged-MAC DISCOVER: 0 frames on `vtnet1` |
| 9 | VLAN filtering | **Pass.** `bridge_vlan_unfiltered` warned for both bridges before. `bridge1`: `defuntagged 1`, PVID on every member, `vlanfilter`: D1/D2 delivered, one tag and two tags not delivered to the victim; control with `-vlanfilter`: the same tagged frame delivered (2), with `vlanfilter` (0). A new member got PVID 1 from `defuntagged`. `bridge0`, live only: `ifuntagged vtnet1 1` **first**, then the DHCP jail's port, then `vlanfilter`: the host's own traffic and the jail's DHCP renewal and ping worked; the warning cleared, health `ok` |
| 10 | Reboot with `kld_list`, autostart guest filtered | **Not run** (needs Andy) |

### Test artifacts, not product failures (kept in the transcript)

- The first static run (`…-04-static-cases.log`) reported 20 FAILs, each with
  exactly 1 frame on the bridge and the victim: tcpdump writes an empty line
  when `timeout` stops it, and the script counted lines. Every capture file
  held only that newline. Fixed (`grep -c .`), rerun: 23/23
  (`…-05-static-cases-rerun.log`).
- The VM-program G2 case first failed on its capture filter (`ip6`), which
  matched `m7c-vm`'s own DHCPv6 solicits from another MAC; narrowed to RAs
  from the sender, rerun: pass, and the jail-side capture shows the two RAs
  sent.
- The first item 9 run had D1/D2 "fail" on the bridge capture (with
  `vlanfilter` on, the bridge's tcpdump no longer matches those frames, as P2
  noted; they were delivered to the victim) and V1–V3 send nothing (the
  attacker jail had been restarted, which remounted its `/dev` without bpf).
  Rerun counting at the victim port, with bpf unhidden again, plus a control.

## State left

Restored and verified: `agent.json` sha256 `490bc087…` (as before), `rc.conf`
and `loader.conf` unchanged, `bridge0` identical to before (`diff`),
`bridge1` destroyed, all test jails and the test VM deleted, devfs ruleset
6001 removed again (absent before the run), `/tmp/m7cv` removed, services
running **b61e578** (rebuilt from that commit; not byte-identical to the
earlier b61e578 binary, build time 2026-11-22T01:07:06Z), health `ok`.
**The netgraph modules netgraph, ng_socket, ng_ether, ng_bpf stay loaded**
(live, not persistent), as asked; `if_epair` was already loaded.

## For Andy: item 10

Add to `/etc/rc.conf` on hearst (replacing any existing `kld_list`):

```
kld_list="if_epair netgraph ng_socket ng_ether ng_bpf"
```

then deploy the branch head (`scripts/deploy-test.sh hearst` from
`feat/m7c-antispoof`) and reboot. Check that `kldstat` lists the four
netgraph modules, then start a test jail and a test VM with NICs through the
API and check their filters (`ngctl show <port>:`, health `port_filters ok`).
Note: Decision 12 item 10 speaks of an *autostart* guest, but this release
has no autostart: nothing starts a guest after a reboot (`desired_state` is
not reconciled, ADR 0009 decision 2c). So item 10 can only check that the
modules come back from `kld_list` and that the next start is filtered.
