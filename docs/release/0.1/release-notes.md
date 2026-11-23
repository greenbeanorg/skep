# Draft: Skep 0.1.0 release notes

*Draft for Andy (2026-11-23). Pre-release; not for production.*

Skep is an open-source virtualization and infrastructure management platform
built on the FreeBSD® operating system. 0.1.0 is the first release: one
FreeBSD 15.1 host, managed through a web interface, a REST API under
`/api/v1` (OpenAPI in `api/openapi.yaml`) and the `skepctl` command line.

## Highlights

- **Two processes, one boundary.** `skep-api` runs unprivileged, is the only
  network listener (TLS 1.3, self-signed with a pinnable key or your own
  certificate), and reaches the host only through `skep-agent`, which runs as
  root behind a Unix socket with peer-credential checks and a closed set of
  typed operations (ADR 0004, 0006).
- **Accounts and permissions.** Argon2id passwords, cookie and bearer
  sessions, login rate limits that never lock accounts, roles with
  deny-by-default permissions, two audit logs (the agent's operations and the
  API's access), sealed monthly into hash-chained segments (ADR 0007).
- **Tasks.** Every change is an asynchronous task with history, events,
  idempotency keys and cancellation; a failed task's error carries reviewed
  structured details (PR #62).
- **bhyve virtual machines.** UEFI guests on zvols or directory pools, up to
  8 disks (`virtio-blk`, `nvme`, `ahci-hd`) and 4 NICs (`virtio-net`,
  `e1000`), install media, a per-VM runner that survives agent restarts, the
  serial console in the browser, snapshots (also running), rollback, linked
  clones, disk add/grow/remove (ADR 0009, 0010).
- **Jails.** VNET jails on ZFS clones of templates built from pkgbase (or an
  operator's `base.txz`) with every package script checked against an
  allowlist; a fixed security baseline checked against the kernel after each
  start; DHCP or static IPv4; RCTL limits and quotas; snapshots and clones
  (ADR 0010).
- **Guest-port anti-spoofing.** A netgraph `ng_bpf` filter on every guest
  port before it joins its bridge (MAC, IPv4 and ARP sender, no rogue DHCP or
  router advertisements), checked every 10 s (ADR 0011).
- **Storage.** ZFS pool and dataset inventory, capacity and locked-dataset
  health checks, directory pools for hosts without ZFS, zvol device nodes
  repaired before use.
- **Installer.** A FreeBSD package (account `skep`, UID/GID 409) and
  `skep-setup` (`check`, `plan`, `apply`, `rollback`, `upgrade`, `remove`)
  that only adds to `rc.conf`/`loader.conf`, journals every change and backs
  up the databases before an upgrade (ADR 0012).

## Requirements

FreeBSD 15.1-RELEASE amd64, UFS or ZFS root; Intel VT-x with EPT and
unrestricted guest for VMs (AMD-V unvalidated); `edk2-bhyve`; a ZFS pool for
jails. See [host requirements](../../installation/host-requirements.md).

## Known issues

See [known-issues.md](known-issues.md). In short: validated on lab virtual
machines only; modules configured by setup load at the next boot; the agent
runs as root and has not had an external review.

## Thanks and trademarks

FreeBSD® is a registered trademark of The FreeBSD Foundation. Skep is not
affiliated with or endorsed by The FreeBSD Foundation.
