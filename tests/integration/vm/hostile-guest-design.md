# Design note: hostile frames from inside a real VM

Status: **design only**, 2026-11-22. No code, no hearst change. For Andy
to decide the open points at the end before anything is built or run.

## Why

ADR 0011 Decision 12 item 7 checked the VM port filter in two halves
([m7c-validation.md](../jails/results/m7c-validation.md), item 7):

1. **A real VM** (`m7c-vm`, empty disk, UEFI network boot): its tap got
   the VM program before joining `bridge1`, the program read back
   identical to the generated one, and the 11 frames the firmware sent
   from its own MAC passed. No hostile frames: the firmware sends none,
   and there was no login on a guest.
2. **The hostile cases, emulated**: the same program, printed by
   `tools/pfscript`, was put on a hand-made epair whose far end was in a
   test jail, and `tools/rawl2` sent the 13 cases from that jail. 13/13
   passed.

What the emulation does not show:

- frames **written by bhyve** to a tap (virtio-net or e1000 back end), as
  opposed to frames written by a jail's bpf to an epair: header layout
  after the back end, offloads (a guest with TSO or checksum offload on
  hands bhyve large or partially checksummed segments), VLAN tags the
  guest sets with `vlan(4)` instead of in a hand-built frame;
- the filter **as installed by the runner** on a VM tap and kept across a
  guest reboot inside one run (the bhyve reboot loop keeps the tap);
- a guest that **changes its MAC** (`ifconfig vtnet0 ether ...`) or puts
  its NIC in promiscuous mode, which only a guest kernel can do;
- frames sent **before the guest OS**, from the firmware or a network
  boot loader, beyond what the empty-disk VM happened to send.

## What the test needs

1. A guest that boots to an OS with **root**, so that it can open
   `/dev/bpf` and write arbitrary frames, change its MAC, and create
   `vlan(4)` interfaces.
2. A way to **get the frame tools into the guest** (`rawl2`, `rawping`;
   FreeBSD amd64 builds with the `skep_testtasks` tag, never shipped) and
   to **run commands** in it from a script.
3. The **same observation points** as `m7c-validation/cases.sh`:
   `tcpdump` on `bridge1` and on the victim's port (a test jail on
   `bridge1`), and the victim's ARP table.

## The guest image

| Option | How it gets into a Skep VM | Access | Notes |
|---|---|---|---|
| **A. FreeBSD 15.1 official VM image** (`FreeBSD-15.1-RELEASE-amd64-BASIC-CLOUDINIT-ufs.raw.xz`, or the plain `-ufs.raw.xz`) | Create a Skep VM with an empty thin disk of the image's size or larger on `skeptest`; with the VM stopped, write the image into its zvol on the host (`xz -dc ... \| dd of=/dev/zvol/skeptest/skep/vm/<id>/disk0 bs=1m conv=sync`) | SSH with a per-run key, given by a config-drive ISO (`cidata`) that `nuageinit` reads at first boot, attached as install media. Fallback: the serial console | Same OS as the tools, so `rawl2` runs unchanged. Writing the zvol is a host operation outside Skep's API on a test object: **needs approval** |
| **B. Install from the 15.1 disc1 ISO** through the serial console | The normal Skep flow (as in the Milestone 6 install check), driven by a person in `consolectl` or the browser | Serial console; SSH after installation | No host-side writes beyond the ISO in `media_dir`. Slow and manual; a scripted install over the console WebSocket would be new tooling |
| **C. A small custom live ISO** (mfsBSD-style) with the tools and an SSH key baked in | Attached as media; no disk at all | SSH, or the console | Needs an ISO build (`makefs -t cd9660` on a FreeBSD host); the image must be rebuilt per key; most new tooling |
| D. A Linux guest with scapy | As A | SSH | Rejected: a second OS, and `rawl2`/`rawping` are FreeBSD-only (`main_freebsd.go`) |

**Recommendation: A**, with B as the fallback if writing a zvol on the host
is not approved. A gives a reproducible guest in minutes and keeps the
tools as they are. Points to verify before relying on it: that 15.1's
BASIC-CLOUDINIT image exists under that name, and that `nuageinit` on 15.1
reads a `cidata` ISO attached as a CD (otherwise use the plain image and
set the root key through the console once).

### Reaching the guest

`bridge1` has no DHCP server. Two ways, both without touching VLAN 60:

- **IPv6 link-local**: the VM program passes IPv6 (only router
  advertisements are dropped), so the host can reach the guest at
  `fe80::<eui64>%bridge1` from the host's own link-local address on
  `bridge1`. No addressing to configure.
- **Static IPv4** in the cloud-init network configuration, in
  192.0.2.0/24 (the test bridge's documentation range); the host already
  takes 192.0.2.1 on `bridge1` in the jail runs.

The SSH key is generated per run on merrick, held in the run directory,
put into the `cidata` ISO, and deleted at cleanup. It is never committed or
printed. No password is set for root in the guest.

## Cases

The probe's cases ([m7c-probe.md](../jails/results/m7c-probe.md) and
[m7c-probe-netgraph.md](../jails/results/m7c-probe-netgraph.md), same
names), sent from inside the guest on its NIC, with the VM program's
expected result, plus VM-only cases (M1 to M5). "Dropped" means 0
matching frames on `bridge1` and on the victim's port, as in `cases.sh`.
The VM program checks the MAC only (Skep records no IPv4 address for a
VM), passes IPv6 except router advertisements, and drops DHCP-server
replies and control frames.

| Case | Sent from the guest | Expected |
|---|---|---|
| D1 / D2 | Normal ping both ways | Pass |
| A1 | `rawl2`: ICMP with a forged Ethernet source | Dropped |
| A2 | `ifconfig vtnet0 ether <forged>`, ping, restore | Dropped while forged (a guest kernel changing its MAC: the case the emulation could only approximate) |
| B1 / B2 | `rawping` from an unused on-subnet / off-subnet IPv4 address, own MAC | Pass (residual: no IPv4 check for a VM) |
| C1 | ARP reply with another sender IP, own MAC | Pass (residual, as B) |
| C2 | ARP reply with a forged sender MAC in the payload | Dropped |
| C5 | `arp -s <ip> <forged> pub` in the guest | Dropped (the forged reply) |
| E1 / E2 | DHCP DISCOVER from the own / a forged MAC | Pass / Dropped |
| F1 | ICMPv6 echo to ff02::1, own MAC | Pass |
| G1 | DHCP offer (UDP 67 to 68) | Dropped |
| G2 | IPv6 router advertisement | Dropped |
| L1 / L2 | LLDP, STP BPDU | Dropped |
| V1 | One 802.1Q tag, real addresses | Dropped by `vlanfilter` (passes without it: the documented gap) |
| V2 / V3 | One tag with a forged MAC; two tags | Dropped |
| **M1** | `ifconfig vtnet0 promisc`, then D1 | Pass (promiscuous mode changes nothing on send) |
| **M2** | `vlan(4)` on `vtnet0` with tag 10, ping from it (the guest's own stack, not `rawl2`) | Dropped by `vlanfilter` |
| **M3** | Large TCP send with TSO and checksum offload on, to the victim | Pass, and no frame from another MAC |
| **M4** | Guest `reboot` inside the run, then A1 | Still dropped: same tap, same filter (node ID unchanged) |
| **M5** | The VM with the `e1000` back end instead of `virtio-net`: A1, D1 | As above |

The filter's own counters (`ngctl msg skf_<tap>: getstats '"jail"'`)
are read before and after each case, as in the jail runs.

## What it needs on hearst (all to be approved)

- A test VM on `bridge1` (1 vCPU, 512 MiB to 1 GiB, a thin disk of about
  6 GiB on `skeptest`), and a test jail as the victim, as in the M7c run.
- The image (about 700 MB compressed) downloaded on merrick and copied to
  hearst by `scp`; Pi-hole may block the download host, in which case the
  run stops and says so.
- **Option A only:** writing the image into the stopped test VM's zvol on
  the host. Not in the standing pre-approvals (those cover `dd` inside
  test jails, not host zvols).
- The `cidata` ISO in `media_dir` (`/var/db/skep/media`), built with
  `makefs` on hearst or on merrick, removed at cleanup.
- `bridge1` with `vlanfilter` and PVIDs for V1 to V3 and M2
  (pre-approved: test bridges).

Nothing touches `bridge0`, VLAN 60, or the management network. Memory
fits hearst (6 GiB, with the 1 GiB host reserve).

## Pass criteria

All cases as expected, with the transcript in
`tests/integration/jails/results/` like the M7c run, after which the
emulated half of item 7 can be marked superseded. A failure of any
"Dropped" case is a security finding: the run stops there and reports.

## Open points for Andy

1. Option A (official image written into a test VM's zvol) or B
   (interactive install through the console)?
2. May the run write a test VM's zvol on the host, and put a generated
   ISO in `media_dir`?
3. May the run download the 15.1 VM image (and from which mirror, given
   Pi-hole)?
4. Should M3 (offloads) and M5 (e1000) be in the first run, or later?
5. Should this become a standing test before each release (it needs a
   guest image kept somewhere), or a one-off for 0.1?
