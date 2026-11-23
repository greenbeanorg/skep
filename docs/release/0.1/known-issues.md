# Draft: Skep 0.1 known issues

*Draft for Andy (2026-11-23). Each item says what happens and what to do;
the source is linked. Items marked (PR #n) depend on a pull request still
open when this was written.*

## Platform and validation

- **Only lab virtual machines have run Skep** (FreeBSD 15.1-RELEASE amd64,
  UFS and ZFS root, Intel VT-x nested under KVM). Bare metal, AMD-V hosts,
  real disks, SMT, `nvme`/`ahci-hd` disks beyond a boot and `e1000` NICs are
  unvalidated ([runbook](../../../tests/integration/bare-metal/README.md)).
- **Nested under KVM**, a guest's first start can stop the whole host unless
  `hw.vmm.vmx.cap.virtual_interrupt_delivery=0` is set before `vmm` loads.
  Skep does not detect it ([VM guide](../../administration/virtual-machines.md)).
- No clustering, HA, backup, network management, graphical consoles or
  metric history.

## Installation and upgrade

- **Modules load at the next boot.** `skep-setup apply` configures `vmm`,
  `nmdm` and the netgraph and epair modules but never loads them; until a
  reboot (or the printed `kldload`), VMs and guest networking do not work,
  health reads `degraded`, and the first `skep-setup upgrade` exits 3.
  `check`, `plan` and `apply` name the modules (PR #61).
- **Firmware.** The package depends on `edk2-bhyve` (PR #61): install a
  local file with `pkg install ./skep-0.1.0.pkg`; `pkg add` refuses until the
  firmware is installed.
- **pkg does not stop `pkg delete skep` while guests run.** The package
  script warns, but pkg deletes the files anyway; use `skep-setup remove`,
  which refuses ([packaging](../../installation/packaging.md)).
- After a late refusal during `upgrade`, the new API can answer
  `503 agent_version_mismatch` next to the old agent until both are restarted
  (accepted for 0.1; an agent drain mode is in the backlog).
- `skep-setup remove -revert-host` also lists lines an earlier install added,
  because journals survive `pkg delete` (backlog).
- `skep-setup check`/`apply` report a group- or other-writable
  `/var/db/skep` with exit status 2 (`upgrade`: 3) (found 2026-11-23, PR #58).

## Jails

- A jail start can wait up to about 160 s when a stop was interrupted
  (PR #57). If an agent restart interrupts a stop and finishing it fails, the
  container shows `running` with desired `stopped` and the reason is only in
  the agent's log (found 2026-11-23, PR #57).
- `route: File exists` lines at jail start in `daemon.log` are harmless
  base-system output ([note](../../../tests/integration/jails/results/route-file-exists-at-jail-start.md)).
- Two log records can share one `daemon.log` line (daemon(8)); Skep logging to
  syslog itself is in the backlog
  ([note](../../../tests/integration/rcd/daemon-log-joined-records.md)).

## Storage

- A new zvol often has no `/dev/zvol` node for a while (FreeBSD/OpenZFS);
  Skep repairs it before a VM starts and refuses a zvol at `volmode=none`
  with the exact command to fix it (ADR 0010 amendment z). A report to
  OpenZFS is drafted ([upstream](../../upstream/openzfs-zvol-volmode-dev-node.md)).
- Memory "available" does not count the ZFS ARC.

## Networking

- Guest-port filters cost about 12 % single-stream throughput in a VM;
  unmeasured on hardware. Detection of a removed filter is by polling (up to
  10 s). VLAN isolation relies on the operator's `vlanfilter` bridge
  ([guide](../../administration/guest-port-filtering.md)).

## Security

- The agent runs as root; no external review yet. The agent does not yet
  refuse to start when its binary or configuration is writable by other users
  (design proposed, PR #63).
