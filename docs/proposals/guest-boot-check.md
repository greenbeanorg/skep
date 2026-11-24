# Proposal: confirming that a guest OS booted in the lab tests

Status: proposal, no code.

## Today

Guest start is verified by the bhyve process existing. That shows the VM was
created and is running. It does not show that a loader, kernel or userland
ran. `snaptest` on hearst runs firmware only and would pass this check.

## Options

| Option | What it proves | What it needs | Cost and limits |
|---|---|---|---|
| **A. Serial console marker** | The guest's kernel and rc reached a login prompt, or a chosen marker | A guest with a serial console (FreeBSD images have one); a user with `vm:console`; a non-interactive mode for `tests/integration/vm/consolectl` that waits for a pattern | Small tool change. The agent keeps the last 64 KiB of output, so a late attach still sees the boot. No host or network change |
| **B. DHCP request seen on the bridge** | The guest's network stack came up and ran a DHCP client | `tcpdump` as root on the host, filtered on the guest's MAC; a NIC on a bridge | No guest change, no DHCP server access needed. Proves less: an early-boot PXE or firmware client could also send one, so match on the OS's client if possible |
| **C. DHCP lease on the server** | As B, plus an address was assigned | Read access to OPNsense | Outside what lab runs may touch; not recommended |
| **D. SSH or ICMP to the guest** | The guest is up and reachable | An address, a route from the test host, a key in the image | The strongest proof of a usable guest, but needs a prepared image and a known address |
| **E. Guest agent** | Whatever it reports | A new component in every guest image, and a host channel | Skep has none. Far too large for this purpose |

## Recommendation

**A, the console marker.** It is the cheapest option that really shows the OS
booted, it uses the public API, and it changes neither the host nor the
network.

- The test waits, with a time limit, for `login:` on the console.
- A prepared image can do better by printing a per-run marker from
  `/etc/rc.local`. That separates this boot from text left in the buffer by
  an earlier one. Without it, the test must attach from the VM's start, or
  check that the buffer began after the start.
- Keep the bhyve process check as the first step.
- Add B later only if a test needs to prove networking.

## What it needs

1. A `-wait-for PATTERN -timeout D` mode in `consolectl`, which today is
   interactive only. The existing pinned-key and token-file handling stay.
2. A bootable guest image on each lab VM. `snaptest` cannot pass, because it
   has no OS.
3. A temporary test user with `vm:console`, deleted at cleanup.

## Risks

- Console text comes from the guest and is untrusted. The tool must only
  match it and must never act on it or echo it unescaped.
- A pattern as short as `login:` can match output from a program in the guest.
  This matters for hostile-guest tests, where the per-run marker is not proof
  either, since the guest could print it. For those, treat the result as "the
  guest produced output", not as a security property.
- Nested guests boot slowly on these hosts, so the time limit must be generous
  and a timeout must report what the console last showed.

## Questions for Andy

1. Accept A?
2. Which image should be the standard boot-check guest on smith and langrishe?
