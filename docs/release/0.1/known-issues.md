# Skep 0.1 known issues

> **0.1 is a single-host preview validated only nested under KVM; it has not
> been run on bare metal.**

## Pending bare-metal validation

Each item is a step of the
[bare-metal runbook](../../../tests/integration/bare-metal/README.md), which
gives the commands and the pass criterion. None has run on real hardware.

- **Install on hardware** (step 2): a fresh Auto-ZFS install, the package
  and `skep-setup apply`, with the agent's start check passing on the
  installed paths with no path changed by hand.
- **Discovery** (step 3): CPU cores and threads on a host with SMT, memory,
  every disk and NIC, and the virtualization fields while `vmm` is unloaded.
- **bhyve without the nested-KVM workaround** (step 4):
  `hw.vmm.vmx.cap.virtual_interrupt_delivery` at its default while guests
  run.
- **A VM on real disks** (step 5): non-thin zvols with a full
  `refreservation`, the guest's SMT topology, `nvme` and `ahci-hd` disks
  beyond a smoke boot, and an `e1000` NIC.
- **Thick disks larger than the pool** (step 6): refused with
  `insufficient_space` and nothing created.
- **Zvol device-node miss rate on real disks** (step 7). On the lab VMs'
  virtual disks: 45/50 and 43/50 creates; on file-backed pools, 0/100 on
  hearst and 98/100 on smith, cause not known ([backlog](../../backlog.md)).
  The repair before a VM start covers any rate.
- **A degraded pool** (step 8): reported `DEGRADED` with the storage health
  check degraded, then `ONLINE` after the resilver.
- **The password prompt on the system console** (step 9) of
  `skep-api admin create`.
- **Guest ports on a physical uplink** (step 10): spoofed MAC and IPv4 from a
  static-IPv4 jail stopped, tagged frames kept off the uplink, and the
  filter's throughput cost (about 12 % in a VM).
- **A reboot with guests** (step 11): no guest started, drift recorded, no
  leftover epairs or taps.
- **An AMD-V host with nested paging** (step 12).

If you have a machine to spare, the
[runbook](../../../tests/integration/bare-metal/README.md) is the whole
procedure, and results recorded as it describes are welcome.

## Platform and validation

- **Only lab virtual machines have run Skep** (FreeBSD 15.1-RELEASE amd64,
  UFS and ZFS root, Intel VT-x nested under KVM). Bare metal, AMD-V hosts,
  real disks, SMT, `nvme`/`ahci-hd` disks beyond a boot and `e1000` NICs are
  unvalidated ([runbook](../../../tests/integration/bare-metal/README.md)).
- **The full VM checks are from an earlier build.** Every check of ADR 0009
  decision 11, including a guest operating system's install and boot to
  its login prompt, passed on `hearst` on 2026-11-14 on build 44a8784
  ([results](../../../tests/integration/vm/results/step6-checks.txt)).
  Later builds started VMs at the bhyve level only, with nothing looked at
  inside the guest: on `hearst` f73f4fd and dd7a8b6 on 2026-11-22
  ([results](../../../tests/integration/jails/results/m7c-validation.md),
  [results](../../../tests/integration/jails/results/validation-2026-11-22.md))
  and ce1d910 on 2026-11-23 (recorded in the [backlog](../../backlog.md)
  only); on `smith` and `langrishe`, VMs with empty disks on 2026-11-22
  and 23 ([installer](../../../tests/integration/setup/results/lab-vm-validation-20261122.md),
  [zvol nodes](../../../tests/integration/setup/results/zvol-device-node-20261122.md),
  [c9ecac9](../../../tests/integration/setup/results/task-error-details-20261123.md)).
  On the 0.1 build (the installed package from c8ce17b), on `smith` (UFS
  root, nested under KVM) on 2026-11-24, a VM with one zvol disk and no NIC
  was created, started, stopped, force-stopped and deleted, and booted from
  the FreeBSD 15.1 bootonly ISO to the installer's first prompt on the
  serial console ([results](../../../tests/integration/vm/results/smoke-0.1-c8ce17b-20261124-smith.md)).
  A guest operating system's install, its boot from its own disk, and its
  networking have not been checked since 2026-11-14.
- **Nested under KVM**, a guest's first start can stop the whole host unless
  `hw.vmm.vmx.cap.virtual_interrupt_delivery=0` is set before `vmm` loads.
  The agent logs a warning at start and reports a degraded `nested_interrupts`
  health check when it sees this, but does not prevent it
  ([VM guide](../../administration/virtual-machines.md)).
- No clustering, HA, backup, network management, graphical consoles or
  metric history in 0.1. **On `main` after 0.1 (Milestone 8, not
  released):** a host without VMs and jails can join another host's
  cluster (step 8b-1), and be removed, wiped (`leave --wipe`) or give up
  a join (`join --abandon`) (step 8b-2a), as a preview: users and guests
  are not shared, `admit` and `remove` are root-only on a member; step
  8b-2b adds node key rotation (`key rotate`, root, every node upgraded
  first), a cluster audit (`audit verify`) and a clock-offset check; with
  a member down a join and a key rotation wait, and a removal that leaves
  too few answering voters needs an override and waits for them. The data
  directory layout changed in 8b-2a (`state/`, no migration; a host on an
  8b-1 build is moved by hand)
  ([Joining hosts into a cluster](../../administration/cluster.md),
  [limits](../../architecture/skep-cluster.md#limits-of-8b-2a-and-8b-2b)).

## Installation and upgrade

- **Modules load at the next boot.** `skep-setup apply` configures `vmm`,
  `nmdm` and the netgraph and epair modules but never loads them; until a
  reboot (or the printed `kldload`), VMs and guest networking do not work,
  health reads `degraded`, and the first `skep-setup upgrade` exits 4
  (incomplete) on that health, naming the modules. `check`, `plan` and
  `apply` name them too.
- **Fixed account numbers.** The package refuses to install on a host
  where another account or group already holds UID or GID **409** (the
  `skep` account) or, from the build that adds `skep-cluster`
  (Milestone 8), **410** (`skep-cluster`). Neither number is registered in
  the FreeBSD ports `UIDs`/`GIDs` files yet, and 409 is now listed there
  for `emfor`. The numbers stay; registering them
  comes with a port submission ([backlog](../../backlog.md)).
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
  starts, and `-force` first waits its full task wait (`-wait`, default
  60 s). Start the agent, then upgrade.
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
- **`skep-setup check` without `-mgmt`** prints the agent's state and
  start check, then exits 2 with the usage error, so a refusal is visible
  but not in the exit code. Other missing or invalid answers still exit 2
  before the agent is reported. With `-mgmt`, its exit code 1 also covers
  other items for the operator, so read the printed "agent start check"
  line.
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
