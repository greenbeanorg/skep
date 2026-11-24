# Draft: Skep 0.1 known issues

*Draft for Andy (2026-11-23). Each item says what happens and what to do;
the source is linked.*

## Platform and validation

- **Only lab virtual machines have run Skep** (FreeBSD 15.1-RELEASE amd64,
  UFS and ZFS root, Intel VT-x nested under KVM). Bare metal, AMD-V hosts,
  real disks, SMT, `nvme`/`ahci-hd` disks beyond a boot and `e1000` NICs are
  unvalidated ([runbook](../../../tests/integration/bare-metal/README.md)).
- **Nested under KVM**, a guest's first start can stop the whole host unless
  `hw.vmm.vmx.cap.virtual_interrupt_delivery=0` is set before `vmm` loads.
  The agent logs a warning at start and reports a degraded `nested_interrupts`
  health check when it sees this, but does not prevent it
  ([VM guide](../../administration/virtual-machines.md)).
- No clustering, HA, backup, network management, graphical consoles or
  metric history.

## Installation and upgrade

- **Modules load at the next boot.** `skep-setup apply` configures `vmm`,
  `nmdm` and the netgraph and epair modules but never loads them; until a
  reboot (or the printed `kldload`), VMs and guest networking do not work,
  health reads `degraded`, and the first `skep-setup upgrade` exits 4
  (incomplete) on that health, naming the modules. `check`, `plan` and
  `apply` name them too.
- **Firmware.** The package depends on `edk2-bhyve`: install a local file
  with `pkg install ./skep-0.1.0.pkg`; `pkg add` refuses until the firmware
  is installed.
- **pkg does not stop `pkg delete skep` while guests run.** The package
  script warns, but pkg deletes the files anyway; use `skep-setup remove`,
  which refuses ([packaging](../../installation/packaging.md)).
- After a late refusal during `upgrade`, the new API can answer
  `503 agent_version_mismatch` next to the old agent until both are restarted
  (accepted for 0.1; an agent drain mode is in the backlog).
- **`skep-setup upgrade` with the agent not running.** Upgrade counts
  unfinished tasks from the agent's database. If some are recorded, it
  refuses and says to wait for tasks that cannot finish until the agent
  starts, and `-force` first waits its full task wait (default 60 s). Start
  the agent, then upgrade.
- `skep-setup remove -revert-host` also lists lines an earlier install added,
  because journals survive `pkg delete` (backlog).

## Jails

- **A stop interrupted by an agent restart is finished at the next agent
  start**, before the agent serves its socket, one jail after another. Each
  can delay the agent's start by up to about 190 s. If finishing it fails,
  the container is recorded `unknown` and then reads `running` with desired
  `stopped` while the jail still runs; the reason stays in its `state_detail`.
  Stop it again ([ADR 0010 amendment aa](../../decisions/0010-storage-and-jails.md)).
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

- Guest-port filters cost about 12 % single-stream throughput through an
  epair in a small VM; unmeasured on hardware. Detection of a removed filter
  is by polling (up to 10 s). VLAN isolation relies on the operator's
  `vlanfilter` bridge ([guide](../../administration/guest-port-filtering.md)).
- **IPv4 and ARP addresses are pinned only on static-IPv4 jail NICs.** A DHCP
  jail or a VM keeps its recorded MAC but can send from, or answer ARP for,
  an IPv4 address it does not hold.
- **IPv6 source addresses are not pinned.** VM ports pass IPv6 with the MAC
  check and a best-effort router-advertisement guard; neighbour discovery is
  not pinned either. Jail NICs have no IPv6 setting yet and drop IPv6.
- **Starts are not refused on a bridge without VLAN filtering.** The health
  check `bridge_vlan_unfiltered` degrades the agent's health and stops
  nothing.

## Security

- The agent runs as root; no external review yet.
- **The agent refuses to start when a checked path is unsafe:** not owned by
  root, writable by its group or others, or carrying a non-trivial ACL. It
  checks its binary, its configuration file, its data and socket directories,
  and their parents. The message prints the fix (ADR 0004 amendment of
  2026-11-23).
- **What a refusal looks like under the supervisor** (observed on a ZFS-root
  lab VM). `service skep_agent start` exits 1 and warns; `status` says the
  agent has exited and is being restarted. The supervisor retries every 5 s
  and never gives up, so the refusal repeats until the path is fixed or
  `service skep_agent stop` is run. syslogd collapses the repeats into "last
  message repeated N times", so count those too. `skep_api` keeps running and
  its health answers 503 `unavailable`. With a session, the `agent` check says
  the socket is not answering and points at `daemon.log`, which holds the
  cause. After the fix the agent came back by itself within 5 s.
- **`skep-setup check` reports a start-check refusal only when it is given
  the host's answers** (for example `-mgmt`). Without them it exits 2 before
  it reports the agent. Its exit code 1 also covers other items for the
  operator, so read the printed "agent start check" line.
- **Recent agent audit entries are read on the host, and only while the
  agent runs.** Entries not yet sealed, such as the start-up sweep's, are
  read with `skep-agent audit export --unsealed` as root while the agent
  runs. It refuses while the agent is stopped, and the API does not serve
  the agent's audit log (backlog).
- **`user:manage` is admin-equivalent:** a holder can assign any role,
  `admin` included. Grant it only to administrators.
- **Task error messages can carry command output** (for example the last
  line of a command's standard error), visible to anyone who can read tasks.
  A review is in the backlog.
