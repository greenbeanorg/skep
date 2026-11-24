# Skep 0.1.0 release notes

*Pre-release; not for production. 0.1 is a single-host preview validated
only nested under KVM; it has not been run on bare metal.*

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
  structured details.
- **bhyve virtual machines.** UEFI guests on zvols or directory pools, up to
  8 disks and install media together (`virtio-blk`, `nvme`, `ahci-hd`, and
  `ahci-cd` for media) and 4 NICs (`virtio-net`, `e1000`), a per-VM runner that survives agent restarts, the
  serial console in the browser, snapshots (also running), rollback, linked
  clones, disk add/grow/remove (ADR 0009, 0010).
- **Jails.** VNET jails on ZFS clones of templates built from pkgbase (or an
  operator's `base.txz`) with every package script checked against an
  allowlist; a fixed security baseline checked against the kernel after each
  start; DHCP or static IPv4; RCTL limits and quotas; snapshots and clones
  (ADR 0010).
- **Guest-port anti-spoofing.** A netgraph `ng_bpf` filter on every guest
  port, jails and VMs, before it joins its bridge, checked every 10 s. Every
  port pins the Ethernet source and ARP sender MAC and drops DHCP-server
  traffic. **The IPv4 source and ARP sender IP are pinned only on jail NICs
  with a static IPv4 address**, not on DHCP jail NICs or VMs. Jail ports drop
  IPv6; VM ports pass it MAC-only, with a best-effort router-advertisement
  guard (ADR 0011).
- **Storage.** ZFS pool and dataset inventory, capacity and locked-dataset
  health checks, directory pools for hosts without ZFS, zvol device nodes
  repaired before use.
- **Installer.** A FreeBSD package (account `skep`, UID/GID 409) and
  `skep-setup` (`check`, `plan`, `apply`, `rollback`, `upgrade`, `remove`)
  that only adds to `rc.conf`/`loader.conf`, journals every change and backs
  up the databases before an upgrade (ADR 0012). Its exit codes: 0 success;
  1 `check` found work for `apply` or something for the operator; 2 usage
  error; 3 refused, nothing changed; 4 incomplete, changes were made and the
  run failed or could not verify the result.
- **Agent start check.** As root on FreeBSD, the agent refuses to start if
  its binary, configuration, data or socket directory, or any parent, is not
  root-owned, is group- or other-writable, or has a non-trivial ACL. The
  message prints the fix; there is no override (ADR 0004 amendment of
  2026-11-23).

## Requirements

FreeBSD 15.1-RELEASE amd64, UFS or ZFS root; Intel VT-x with EPT and
unrestricted guest for VMs (AMD-V unvalidated); `edk2-bhyve`; a ZFS pool for
jails. See [host requirements](../../installation/host-requirements.md).

## Known issues

See [known-issues.md](known-issues.md). In short: validated on lab virtual
machines only, nested under KVM, and never run on bare metal (the items
pending bare-metal validation are listed there); modules configured by setup
load at the next boot; the agent runs as root and has not had an external
review.

## License

Skep is released under the BSD 2-Clause License ([LICENSE](../../../LICENSE)).

## Thanks and trademarks

FreeBSD® is a registered trademark of The FreeBSD Foundation. Skep is not
affiliated with or endorsed by The FreeBSD Foundation.
