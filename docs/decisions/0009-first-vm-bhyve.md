# ADR 0009: The first working VM on bhyve (Milestone 6)

- Status: **Accepted**; implemented (steps 1 to 5 of the suggested order),
  FreeBSD validation in progress, see [Implementation notes](#implementation-notes). Every recommendation was
  accepted, with `hearst`'s guest NIC on the lab's VLAN 60 instead of the
  management VLAN; the review answers and two amendments are under
  [Decisions](#decisions) and are worked into the text. The changes
  `hearst` needs are under [Changes hearst needs](#changes-hearst-needs),
  all approved.
- Date: 2026-11-13 (proposed), 2026-11-14 (accepted)
- Depends on: [ADR 0003](0003-read-only-discovery.md) (discovery never
  changes the host), [ADR 0004](0004-privilege-boundary.md) (typed
  operations only, host confinement, `internal/hostops`),
  [ADR 0005](0005-agent-task-engine.md) (every host operation is a task,
  nothing is resumed), [ADR 0006](0006-api-split-authentication-tls.md)
  (permissions, sessions, the origin check), [ADR 0008](0008-web-ui.md)
  (the UI is a client of the public API and nothing more).

## Summary

Milestone 6 makes Skep create, boot, observe, control, and delete one kind
of virtual machine: a bhyve guest with UEFI firmware, zvol-backed disks on
ZFS, bridged virtio networking, and a serial console reachable from the
web interface. It is the first milestone in which the agent changes the
host, so it also creates the privileged operations package that ADR 0004
reserved for this moment, and the host confinement configuration that
says what Skep may touch.

| # | Decision | Recommendation |
|---|---|---|
| 1 | Scope and the hypervisor layer | bhyve only; `engine: qemu` is refused with a clear code; one `internal/hostops` package holds every privileged call, with a fake on Linux |
| 2 | VM configuration schema and where state lives | Typed columns for what the database must constrain, a typed JSON `config` document for the rest; desired and observed never share a column; `desired_state` is recorded but not reconciled in this milestone |
| 3 | Disk storage on ZFS | One dataset per VM under `<pool>/skep/vm/<vm-id>`, one zvol per disk named `disk<slot>`; `volmode=dev`, `volblocksize=16K`, full reservation by default with an opt-in `thin`; pools allow-listed in the agent's configuration; `skeptest` is the lab pool |
| 4 | Install media | ISO files the operator places in a root-owned media directory, listed read-only and attached by name; no upload and no download in this milestone |
| 5 | Guest networking | Bridged only: the operator creates the bridge, Skep creates one tap per NIC and adds it; on `hearst`, a second virtual NIC on the lab's untrusted VLAN 60, with the Proxmox firewall off, carries guest traffic so the management NIC is never touched |
| 6 | Boot | UEFI from the `edk2-bhyve` package with a per-VM copy of the variable store; `firmware: bios` refused; the operator loads `vmm` at boot, Skep loads nothing |
| 7 | Running bhyve | A detached per-VM runner process, started by the agent, that owns the bhyve reboot loop and survives an agent restart; the agent observes it through state files |
| 8 | Console | Serial over `nmdm`, brokered by the agent and relayed by `skep-api` over a WebSocket to xterm.js; keystrokes count as session activity, guest output does not; VNC deferred until the agent can authenticate to bhyve's framebuffer on the client's behalf |
| 9 | Lifecycle operations as tasks | `vm.create`, `vm.update`, `vm.start`, `vm.stop`, `vm.force_stop`, `vm.restart`, `vm.delete`, each with a time limit and a cancellability declaration; `stop` never escalates to `force_stop` by itself |
| 10 | Permissions and audit | `vm:read`, `vm:write`, `vm:delete`, `vm:power`, `vm:console`, `storage:read`; `operator` gets all of them, `viewer` the two reads; every task and every console attachment audited in both logs |
| 11 | What "verified on FreeBSD 15.1" means | A named acceptance check per feature, run on `hearst`, recorded with its output under `tests/integration/vm`; a feature without a recorded check is pending |
| 12 | Clients in this milestone | The web interface gets the VMs section with the console; `skepctl` gets list, show, and the actions; the QEMU adapter, NAT, hot-plug, snapshots, and autostart are not in this milestone |

## Context

### What exists

- The two-process shape, the socket, the task engine, permissions, and
  audit are built and validated on FreeBSD. Submitting a task over the
  socket is the one piece of ADR 0005 that still has no caller.
- `migrations/sqlite/agent/0001` already defines `virtual_machines`,
  `vm_disks`, `vm_networks`, `storage_pools`, and `storage_volumes`, with
  the desired/observed convention. They have never held a row. This ADR
  treats them as a draft to be amended by migration `agent/0004`, not as
  a decision already made.
- Discovery reports bhyve `available` only when `vmm` is loaded and
  `/usr/sbin/bhyve` exists, and never loads it (ADR 0003).
- `internal/hostops` and the host confinement configuration were deferred
  by ADR 0004 "to the first privileged operation". That is this milestone.
- The web interface has a placeholder for VMs that names Milestone 6.

### What hearst looks like today (read-only survey, 2026-11-13)

| Fact | Observed |
|---|---|
| Release | FreeBSD 15.1-RELEASE-p6, GENERIC, amd64, UFS-root |
| CPU and memory | 4 vCPUs, 6 GiB (the resize is already done); `VT-x: ... EPT, UG` in the boot messages |
| bhyve | `/usr/sbin/bhyve`, `bhyvectl`, `bhyveload` from the base package `FreeBSD-bhyve-15.1p5`; `bhyve -k config_file` is supported; `bhyve_config(5)` is installed; `SIGTERM` triggers an ACPI power-off; the `slirp` network backend is documented and needs the `libslirp` package |
| UEFI firmware | Not installed. `/usr/share/bhyve` holds only keyboard layouts and gdb files. The package `edk2-bhyve-g202508_3` is in the repository (also the meta-package `bhyve-firmware`) |
| Kernel modules present, none loaded | `vmm.ko`, `nmdm.ko`, `if_bridge.ko`, `if_epair.ko`, `zfs.ko`, `pf.ko`; `tuntap` is compiled into the kernel, so tap needs no module |
| Network | One interface, `vtnet0`, `10.79.20.170/24`, MAC `bc:24:11:a0:36:7e`; no bridge, no tap, no pf; `net.inet.ip.forwarding=0`; `net.link.tap.up_on_open=0` |
| ZFS | Module not loaded; no `zpool.cache`; the file-backed pool's vdev is `/var/tmp/skep/skeptest.img` |
| Skep | Milestone 5 test build under rc.d from `/home/aba/skep-m5`, API on `10.79.20.170:8443`, the test build's `test.sleep` task enabled |
| Resolver | The lab's Pi-hole pair (`10.79.10.250`, `10.79.10.249`) |

Two facts about the Proxmox host matter here. `hearst` is itself a KVM
guest on `swearengen`, with CPU type `host`, which is why `vmm` has EPT
and unrestricted guest mode to work with. And the Proxmox firewall, when
enabled on a VM's network device, drops frames whose source MAC address is
not that device's configured address (its MAC filter, on by default), so a
bhyve guest bridged straight onto `vtnet0` would send frames that never
leave `swearengen`. Decision 5 is shaped by that.

## Decision 1: scope and the hypervisor layer

### 1a. Which engine

| Option | Notes |
|---|---|
| A. bhyve only; `engine: qemu` is accepted by the schema and refused by validation with `engine_unsupported` | One engine to verify. The interface and the capability model are built against a real adapter, not a guess. |
| B. bhyve and QEMU (TCG) together | Doubles the validation surface before one VM has booted. QEMU is not installed on the lab host. |
| C. QEMU first, because it also runs on the Linux development machine | Would make a development convenience the first production path. Rejected. |

**Recommendation: A.** The `Hypervisor` interface (`Validate`, `Create`,
`Start`, `Stop`, `Restart`, `Delete`, `Inspect`, `List`, `Capabilities`)
is defined in `internal/hypervisor` with `internal/hypervisor/bhyve` as
its only adapter. `Capabilities` is real from the start: it reports
whether `vmm` is loaded, whether the firmware file exists, whether `nmdm`
is available, and the host's CPU and memory limits, and `vm.start`
refuses with `bhyve_unavailable` and the same reason text when a
capability is missing. The QEMU adapter is a later milestone and the
interface must not acquire a method only one engine could implement.

### 1b. Where the privileged calls live

ADR 0004 rule 6: every call that needs root goes through one package
behind an interface. Options for its shape:

| Option | Notes |
|---|---|
| A. One package `internal/hostops` with small interfaces per area (`ZFS`, `Net`, `VMM`, `Files`, `Procs`), a FreeBSD implementation, and a fake for Linux | Matches ADR 0004. The fake lets the API, the UI, and the task handlers be developed and unit-tested on `merrick`, and keeps every `exec.Command` call in one reviewable place. |
| B. Each subsystem package shells out itself | Spreads the privileged surface across the tree. Rejected by ADR 0004. |
| C. A cgo binding to `libvmmapi` | Breaks the pure-Go cross-compile that CI depends on, for no capability that `bhyve`, `bhyvectl`, and the device nodes do not already give. Rejected. |

**Recommendation: A.** Rules for the package:

- Every command is `exec.Command` with a fixed binary path and an explicit
  argument list, as discovery does today: `/sbin/zfs`, `/sbin/ifconfig`,
  `/usr/sbin/bhyve`, `/usr/sbin/bhyvectl`. No `$PATH`, no shell.
- Every argument that comes from a request is first turned into a typed
  value (a size in bytes, a slot number, a resource ID) and then rendered.
  Dataset names, device paths, tap names, and file paths are derived from
  IDs and configuration, never accepted (ADR 0004 rule 2).
- The Linux implementation is a **fake**, selected by file suffix
  (`_linux.go`), that records calls and simulates a VM in memory. It is
  what `make run-dev` and the Playwright suite use on `merrick`. It never
  ships in a FreeBSD binary, and a FreeBSD binary run as an ordinary user
  (dev mode on `hearst`) uses the real implementation and fails on
  permissions, which is the correct outcome.
- The package reads its confinement from the agent's root-owned
  configuration (Decision 1c) and refuses anything outside it before any
  command runs, whoever asks.

### 1c. Host confinement configuration

New sections in `agent.json` (root-owned, not editable through the API):

```json
{
  "hypervisor": {
    "bhyve": {
      "firmware_dir": "/usr/local/share/edk2-bhyve",
      "host_memory_reserve_bytes": 1073741824,
      "max_vcpus_per_vm": 0
    }
  },
  "storage": {
    "pools": ["skeptest"],
    "dataset_root": "skep",
    "media_dir": "/var/db/skep/media"
  },
  "network": {
    "bridges": ["bridge0"],
    "management_interfaces": ["vtnet0"]
  }
}
```

- `storage.pools` is the allow-list. A request naming any other pool is
  refused with `pool_not_allowed`. Empty means no VM storage is possible,
  which is the default: a fresh install can create no VM until the
  operator names a pool.
- `network.bridges` is the allow-list of bridges a NIC may join. An
  interface in `management_interfaces` can never be a member of anything
  Skep touches, and Skep never creates, destroys, or re-addresses a bridge
  in this milestone (Decision 5).
- `host_memory_reserve_bytes` is memory Skep leaves to the host when it
  decides whether a VM may start (Decision 9). `max_vcpus_per_vm` of 0
  means the lower of the host's logical CPUs and `hw.vmm.maxcpu`.

## Decision 2: the VM configuration schema and where state lives

### 2a. Identifiers

| Option | Notes |
|---|---|
| A. The VM ID stays a UUID; the bhyve instance name is `skep-<uuid>` | Follows the schema convention ("IDs are opaque UUIDs, never reused"). A `vmm` instance name may be up to 229 bytes on FreeBSD 13 and later (`VM_MAX_NAMELEN` in `dev/vmm/vmm_param.h`, checked on `hearst`), so the whole UUID fits and nothing is truncated. |
| B. Small integer VM IDs, as Proxmox uses | Readable, but a second ID space to keep unique across a future cluster, and a convention borrowed from the product the standing instructions say not to copy. |
| C. The user's VM name is the bhyve instance name | Names change; the device node under `/dev/vmm` would not follow, and names would need bhyve's character set. |

**Recommendation: A.** The instance name is also the `nmdm` device name
(`/dev/nmdm-skep-<uuid>.1A` for bhyve and `.1B` for the agent; `nmdm`
creates nodes on demand and accepts names beyond the `nmdmN` form, as
vm-bhyve has long relied on, which the first validation step confirms)
and the name of the runtime directory. The VM's UUID is passed to bhyve
as the SMBIOS UUID (`-U`), so the guest sees the same identity the API
does.

### 2b. Columns or a document

| Option | Notes |
|---|---|
| A. Everything in typed columns | Every option is a migration; the table grows with each device type. |
| B. Everything in one JSON document | Nothing can be constrained or indexed by the database; a bad document is found only by code. |
| C. Columns for what the database must constrain or index, a typed JSON document for the rest | Name, engine, CPU, memory, firmware, states, and the disk and NIC rows stay columns with their `CHECK`s; console and boot options live in `config`, validated by one Go type (`models.VMConfig`) and documented in `api/schemas/vm-config.json`. |

**Recommendation: C**, which is what `agent/0001` sketched. Migration
`agent/0004` amends the draft tables:

- `virtual_machines`: add `provisioned INTEGER NOT NULL DEFAULT 0` (1 once
  `vm.create` has finished; an unprovisioned VM can only be deleted), and
  `observed_pid INTEGER` (the bhyve process, while running). The existing
  `observed_state` set (`unknown`, `stopped`, `starting`, `running`,
  `stopping`, `failed`) is kept. `firmware` keeps `bios` in its `CHECK`
  for QEMU later; bhyve validation refuses it (Decision 6).
- `vm_disks`: add `media_name TEXT` for install media, and a `CHECK` that
  a row has exactly one backing: `bus = 'ahci-cd'` with `media_name` and
  no volume, or another bus with `storage_volume_id` and no media.
- `storage_volumes`: add `desired_thin INTEGER NOT NULL DEFAULT 0`,
  `observed_volsize_bytes`, and `observed_referenced_bytes`.
- `vm_networks`: add `observed_tap TEXT` (the tap name while running) and
  the rule that `vlan_tag` must be `NULL` in this milestone (a VLAN on a
  bridge port is the networking milestone's).
- `storage_pools`: becomes a cache of the allow-listed pools as observed
  by discovery, one row per allowed pool, refreshed by
  `discovery.refresh`; `type = 'directory'` stays reserved.

### 2c. Desired state, observed state, and drift

| Option | Notes |
|---|---|
| A. `desired_state` is set by the action (`start` sets `running`, `stop` sets `stopped`) and nothing reconciles it in this milestone | Honest about what is built: no controller exists. A guest that powers itself off leaves `desired = running`, `observed = stopped`, and the API shows both. Nothing restarts it. |
| B. Reconcile locally: the agent restarts a VM whose observed state falls below its desired state | Turns a guest power-off into a restart loop, and does the thing ADR 0004 and the HA rules say not to do without ownership and fencing. |
| C. Reconcile only at host boot ("autostart") | Useful and expected, but it needs to tell a host boot from an agent restart, and the agent restarting VMs on its own start would restart VMs the guest shut down. Deferred to the HA and controller milestones as an explicit `autostart` flag. |

**Recommendation: A.** The agent never starts a VM that nobody asked it
to start in this milestone. The difference between desired and observed
is data the UI shows, not a trigger. After a host reboot every VM is
observed `stopped`, and the ones whose desired state is `running` are
listed as drifted.

What is observed, and how, is in Decision 7. The rule from `agent/0001`
holds: `desired_*` and configuration columns are written only by tasks
that act for a user; `observed_*` columns are written only by observation.

## Decision 3: disk storage on ZFS

### 3a. Layout and naming

| Option | Notes |
|---|---|
| A. One dataset per VM, `<pool>/<root>/vm/<vm-id>`, with one zvol per disk, `disk<slot>` | A VM's storage is one subtree: a recursive snapshot, a recursive destroy, a per-VM quota later, and a per-VM `zfs list` all fall out of the layout. |
| B. Flat zvols, `<pool>/<root>/<vm-id>-disk<slot>` | Simpler names, no grouping; every per-VM operation becomes a filter. |
| C. Named after the VM's name | Renames and uniqueness problems, as in Decision 2a. |

**Recommendation: A.** `<root>` is `storage.dataset_root` (default
`skep`). Skep creates `<pool>/skep` and `<pool>/skep/vm` on first use,
inside an allow-listed pool, which is within the confinement, and never
touches a dataset outside `<pool>/skep`. Every dataset Skep creates
carries the user properties `skep:vm_id` and, on a zvol, `skep:disk_id`,
so a stray dataset can be matched to its owner without guessing from the
name.

### 3b. Properties

| Property | Recommendation | Why |
|---|---|---|
| `volmode` | `dev` | The zvol appears as `/dev/zvol/<pool>/skep/vm/<id>/disk0` and nothing else: no GEOM tasting of the guest's partition table on the host, no `p1` nodes to confuse anyone. |
| `volblocksize` | `16K`, set explicitly | The OpenZFS default moved from 8K to 16K in 2.2; setting it keeps behaviour independent of the host's version. A per-disk override is validated to a power of two between 4K and 128K. |
| Reservation | Full (`refreservation`, the `zfs create -V` default) unless the disk is created with `thin: true`, which uses `-s` | A thin disk on a pool that fills gives the guest I/O errors with no warning. The safe default is the one that cannot overcommit; thin is the operator's choice, and what the small lab pool will use. |
| `compression` | Inherited | Set by the operator on the pool or on `<pool>/skep`; Skep does not override it. |
| Encryption, dedup, quotas | Not set | Encryption of VM disks is the storage milestone's design, not a flag to add in passing. |

### 3c. Sizing

- Size is given in bytes, is at least 16 MiB, and is a multiple of 1 MiB;
  the API also accepts the suffixes `M`, `G`, `T` on strings and
  normalises them before validation.
- A full-reservation disk larger than the pool's `available` space is
  refused with `insufficient_space`, naming the pool and both numbers.
- A thin disk may exceed the available space; the task result carries a
  warning when it does, and the disk is still created. A thin disk larger
  than the pool itself is refused.
- Resizing, adding, or removing a disk after `vm.create` is **not** in
  this milestone; a VM's disks are fixed at creation and go with the VM at
  deletion. Growing a zvol is a one-line ZFS operation, but the guest-side
  story and the hot-plug question belong to the storage milestone.

### 3d. The lab pool, and UFS-root hosts

`skeptest` is a file-backed pool on `hearst`'s UFS root. It is the only
pool the agent's configuration lists there, so it is the only pool Skep
can touch on that host, which is exactly the confinement rule working as
intended. Performance measurements on it mean nothing and are not made.

A UFS-root host without any pool cannot run a VM in this milestone. The
standing instructions ask for equivalent functionality on UFS-root
installs; the schema's `file` volumes and `directory` pools exist for
that, and they are the storage milestone's work (Decision 12). The
installation guide will say so plainly until then.

## Decision 4: install media

| Option | Notes |
|---|---|
| A. The operator copies ISO files into a root-owned media directory on the host; Skep lists them read-only and attaches them by file name | No upload path through the unprivileged API, no download by the root agent, no new secret or quota. Whoever installs Skep has shell access to the host. |
| B. Upload through the API | Gigabytes through `skep-api` and then across the socket to the agent, with quota, resumption, and integrity to design. |
| C. The agent downloads from a URL, with a checksum | The root process makes outbound requests to an address a user typed. Possible later, with an allow-list; not for the first VM. |

**Recommendation: A.** Rules:

- `storage.media_dir` (default `/var/db/skep/media`, `root:wheel 0700`).
  Only regular files whose names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.iso$`
  are listed; anything else in the directory is ignored and logged once.
- `GET /api/v1/storage/media` returns name, size, and modification time
  (`storage:read`). Nothing about the path.
- A VM attaches media by name in a `vm_disks` row with `bus: ahci-cd`,
  `read_only: 1`, and `media_name`. The agent resolves the name against
  the directory listing at `vm.start`; a name that is no longer there
  fails the start with `media_not_found`. The name is never joined into a
  path without that check, and never contains a separator.
- Media can be attached at create and attached or detached by
  `vm.update` while the VM is stopped (Decision 9).

## Decision 5: guest networking

### 5a. The model Skep offers in this milestone

| Option | Notes |
|---|---|
| A. Bridged: the operator creates a bridge and names it in `network.bridges`; Skep creates one tap per NIC, adds it to the bridge, and destroys it when the VM stops | The standard model for a virtualization host. Skep creates and destroys only its own taps; bridges, uplinks, and addresses stay the operator's until the networking milestone. |
| B. NAT inside the host: a bridge without an uplink, an address on it, `pf nat`, forwarding, and an address service for guests | Needs pf enabled on the management host, forwarding on, and a DHCP server that is not in base. Every piece is a change to the host's own networking with a lock-out risk. It is a real feature for some sites, and it is the networking milestone's to design with its rollback path. |
| C. Both now | Twice the validation; B alone is most of the networking milestone. |

**Recommendation: A.** Details:

- Tap names are `tap` plus a number chosen by the kernel
  (`ifconfig tap create`), recorded as `observed_tap`, and given a
  description `skep:<vm-id>:<slot>` so `ifconfig` output is traceable.
  `tuntap` is in the GENERIC kernel, so this loads no module.
- The agent brings the tap `up` itself and does not depend on
  `net.link.tap.up_on_open`, which is `0` by default.
- The bridge named by the request must be in `network.bridges`, must
  exist, and must not contain a management interface, or the start is
  refused (`bridge_not_allowed`). Skep never creates the bridge.
- MAC addresses are generated as `02:` followed by five random bytes
  (locally administered, unicast) unless the request supplies one, which
  must be unicast and is checked for uniqueness on the node by the
  existing `UNIQUE` constraint. The generated prefix is not the FreeBSD
  Foundation's OUI that bhyve itself uses.
- The guest device is `virtio-net`; `e1000` stays in the schema and is
  accepted, because bhyve has it, but only `virtio-net` is in the
  validation plan.
- When the runner exits for good, it destroys the taps it created. A tap
  found at agent start with a `skep:` description and no running VM is
  destroyed by the next `vm.start` or `vm.force_stop` of that VM, and is
  otherwise reported, never silently.

### 5b. What the host must allow, in general

For bridged guests to reach anything: a bridge with a physical or virtual
uplink; the uplink's switch port or upstream hypervisor must accept frames
from MAC addresses it did not assign; no host firewall rule dropping on
the bridge or the taps (`net.link.bridge.pfil_member` and `pfil_bridge`
are irrelevant while pf is off); and, if guests should get addresses, a
DHCP server on that segment. None of this is Skep's to configure in this
milestone, and all of it goes in the administration guide.

### 5c. hearst

| Option | Notes |
|---|---|
| A. Disable the Proxmox firewall (or only its MAC filter) on `hearst`'s existing `net0`, and make `vtnet0` a member of `bridge0` | Puts the management interface into a bridge on a remote host. Moving the address to the bridge is a connectivity change with no safe rollback from here; leaving it on the member works but is the arrangement FreeBSD discourages. The standing rule says never apply a change that does not protect management connectivity. Rejected for the lab. |
| B. Add a second virtio NIC (`net1`) to `hearst` in Proxmox, with the Proxmox firewall **disabled** on that device; on `hearst`, `bridge0` with `vtnet1` as its only member and no address; guests get lab addresses from the lab's DHCP | The management path is untouched. It is also the shape the administration guide will recommend: a guest bridge separate from management. One Proxmox change and two `rc.conf` lines. (Proposed on the same VLAN as `net0`; accepted on VLAN 60, below.) |
| C. NAT inside `hearst` (option B of 5a) | More host changes, pf on a host that today has none, and a feature this milestone does not build. |
| D. An isolated `bridge0` with no uplink | No Proxmox change. Proves that a VM boots and that the tap path works, but the guest has no network, so DHCP, virtio-net under load, and installing anything over the network cannot be verified. |
| E. bhyve's own `slirp` network backend, a user-space NAT stack inside the bhyve process that serves DHCP and DNS to the guest and needs no bridge, no tap, no pf, and no Proxmox change; only the `libslirp` package (bhyve(8) on 15.1 documents it and names `net/libslirp`; `libslirp-4.9.5` is in the repository) | The guest gets outbound connectivity through `hearst`'s own address, so the Proxmox MAC filter never sees a foreign MAC. Slow, per-VM, and it exercises none of Skep's tap and bridge code, so it cannot stand in for validating the production model. It is a good fallback for the lab and a candidate backend for isolated hosts in the networking milestone. |

**Recommendation: B**, with E as the fallback if the Proxmox change is
declined, and D only if a package install is declined too.

**Accepted: B, on VLAN 60.** `net1` is not on `net0`'s VLAN 20 (the
management network, `10.79.20.0/24`). It is on VLAN 60, the lab's
untrusted LAB VLAN, with the Proxmox firewall off on the device. Guests
get their addresses by DHCP from the lab's Kea pool on VLAN 60, and the
lab firewall's LAB rules block that VLAN from the private networks, so a
guest reaches the internet but not `hearst`'s management address or
anything else on `10.79.20.0/24`. That isolation is the lab network's
doing, not Skep's; Skep's part is that the guest bridge has no address
and no management member. Andy added `net1` and took the Proxmox
snapshot `pre-m6` before acceptance. On `hearst`:

```
# /etc/rc.conf
cloned_interfaces="bridge0"
ifconfig_vtnet1="up"
ifconfig_bridge0="addm vtnet1 up"
```

`rc` loads `if_bridge` for the cloned interface. `vtnet0` and its address
are not mentioned anywhere and are listed in
`network.management_interfaces`. The VLAN is Proxmox's business: `vtnet1`
receives untagged frames, so nothing on `hearst` names VLAN 60, and Skep's
rule that `vlan_tag` is `NULL` in this milestone is unaffected.

## Decision 6: boot

### 6a. Firmware

| Option | Notes |
|---|---|
| A. UEFI from `edk2-bhyve` (`BHYVE_UEFI.fd`), with a per-VM copy of `BHYVE_UEFI_VARS.fd` as the variable store | The current firmware for bhyve. Persistent variables mean the firmware remembers its boot entries across restarts, so a guest installed from ISO boots from its disk afterwards. |
| B. `bhyveload` | FreeBSD guests only, no UEFI, no graphical path later. Fine as a tool, wrong as the platform's boot model. |
| C. UEFI with the CSM (legacy BIOS) image | No longer part of the firmware package; `bios` cannot be honoured on bhyve. |

**Recommendation: A.** `firmware: bios` is refused with `firmware_unsupported`
naming the engine. The firmware directory is configuration
(`hypervisor.bhyve.firmware_dir`), so a different package path or a
future base-system firmware is a configuration change. The variable store
copy lives in the VM's persistent directory (Decision 7) and is deleted
with the VM. If the firmware file is missing, `Capabilities` says so and
`vm.start` refuses with `bhyve_unavailable`.

Boot order is the firmware's: with install media attached and an empty
disk, the firmware boots the media; after an installation the guest's
boot entry is in the variable store. Skep exposes no boot-order setting
in this milestone. Whether the FreeBSD installer and the installed system
both use the serial console under this firmware is one of the checks in
Decision 11, not something assumed.

### 6b. Enabling bhyve on the host

| Option | Notes |
|---|---|
| A. The operator sets `vmm_load="YES"` in `loader.conf` (and `nmdm_load="YES"` if the module is not loaded on first open); Skep loads nothing, and `vm.start` refuses with the reason when `vmm` is absent | Consistent with ADR 0003 and its "enabling a capability is an administrative action". One documented line. |
| B. The agent loads `vmm` at start when configured to | A root process changing kernel state on every boot because of a configuration flag. Possible later as an audited task; not needed to boot the first VM. |
| C. `vm.start` loads `vmm` on demand | A VM start that changes kernel state as a side effect. Rejected. |

**Recommendation: A.**

## Decision 7: running bhyve

### 7a. Who runs the bhyve process

The constraint from ADR 0004: VMs keep running when the agent restarts.
bhyve also exits on every guest reboot (exit status 0) and expects to be
started again, so something must own that loop.

| Option | Notes |
|---|---|
| A. bhyve is a direct child of the agent, and the agent runs the reboot loop | Simplest. A running guest survives an agent restart (it is not killed with its parent), but its next reboot is not picked up by anyone, and the restarted agent has no handle on the orphan. |
| B. A detached per-VM runner process, started by the agent in its own session, that owns the reboot loop, the taps, the device teardown, and a small set of state files; the agent observes and signals it | The runner is what survives. It is about one screen of logic. The agent is stateless about running VMs between restarts and re-adopts runners by reading their state. |
| C. An rc.d service per VM | Writes to `rc.conf` for every VM, and ties lifecycle to `service(8)` semantics. |

**Recommendation: B**, as a subcommand of the agent binary
(`skep-agent vm-runner --vm <id>`, in `internal/hypervisor/bhyve/runner`),
so there is one artifact to ship and one code path for rendering the
bhyve configuration. The runner:

1. Creates the taps (Decision 5), writes the `bhyve_config(5)` file,
   copies the UEFI variable store if the VM has none yet, and runs
   `bhyve -k <config> <instance>`.
2. On bhyve exit (statuses from bhyve(8) on 15.1): `0` (rebooted) starts
   bhyve again with the same configuration; `1` (powered off) and `2`
   (halted) end the run with state `stopped`; `3` (triple fault), `4`
   (error), `5` (suspended), or death by a signal end it with state
   `failed` and the status as detail. When a stop was requested (below),
   any exit ends the run as `stopped`.
3. On ending the run: `bhyvectl --vm=<instance> --destroy`, destroys its
   taps, writes the final state, and exits.
4. Honours signals from the agent: `SIGTERM` means graceful stop (the
   runner records the request, then sends `SIGTERM` to bhyve, which bhyve
   delivers to the guest as an ACPI power-button event); `SIGUSR1` means
   force stop (`bhyvectl --vm=<instance> --force-poweroff`, then `SIGKILL`
   to bhyve if it is still there, then step 3).

Configuration is passed with `-k`, a root-only file, never on the command
line, so nothing about a VM appears in `ps` output beyond the instance
name. The rendered file is kept for diagnosis.

Directories:

| Path | Lifetime | Holds |
|---|---|---|
| `/var/run/skep/vm/<id>/` (`root 0700`) | Until reboot | `runner.pid`, `bhyve.pid`, `state.json` (state, detail, since, taps, pids), `stop-requested` |
| `/var/db/skep/agent/vm/<id>/` (`root 0700`) | With the VM | `uefi-vars.fd`, `bhyve.conf` (last rendered), `bhyve.log` (bhyve's stdout and stderr, truncated at each start) |

### 7b. How the agent observes

- A watcher in the agent reads every `state.json` under `/var/run/skep/vm`
  every 2 seconds (configurable) and after each lifecycle task, checks
  that the recorded runner PID is alive and is a `skep-agent` process,
  cross-checks `/dev/vmm/<instance>`, and writes `observed_state`,
  `observed_detail`, `observed_pid`, `observed_tap`, and `observed_at`.
  The watcher writes observed columns only.
- At agent start the watcher runs before the socket is served, so a
  restarted agent knows what is running before it answers anything.
  Tasks are interrupted as ADR 0005 says; VMs are not touched.
- A `/dev/vmm/<instance>` node with no live runner is reported as `failed`
  with the detail "instance exists without a runner"; `vm.force_stop`
  destroys it. A runner with no `vmm` instance is reported the same way
  after one grace interval.
- After a host reboot `/var/run` is empty, so every VM is observed
  `stopped` and the UI shows the drift from Decision 2c.

## Decision 8: the console

### 8a. Serial, VNC, or both

| Option | Notes |
|---|---|
| A. Serial console in this milestone; VNC deferred | Serial is what the firmware, the FreeBSD loader, the installer, and any server guest use. The `nmdm` device nodes are root-only, so the path is private to the agent. |
| B. Serial and VNC, where VNC is bhyve's `fbuf` device bound to `127.0.0.1` with a random per-start password, relayed by the agent | `fbuf` listens on TCP only, so any local process on the host can connect to it, and the RFB password scheme is the weak DES challenge that bhyve's own manual says not to trust. The password would also have to reach the browser for noVNC to use it, or the agent must perform the RFB authentication itself and hand the browser an already authenticated stream. The second is the right design and is more than a milestone that has not booted a VM should carry. |
| C. VNC only | Leaves server guests without the console they actually use. |

**Recommendation: A, with B's agent-side authentication relay as the
design VNC will use when it comes.** The schema's `config.console.vnc`
exists, defaults to disabled, and is refused with `not_implemented` if
enabled, so the shape is fixed now and nothing pretends to work.

### 8b. How the browser reaches it

```
browser (xterm.js) --WebSocket, cookie session, Origin checked--> skep-api
    --WebSocket over the agent socket, actor headers--> skep-agent
    --/dev/nmdm-<instance>.1B--> bhyve (com1 on .1A) --> guest
```

| Option | Notes |
|---|---|
| A. The agent brokers: it holds the `nmdm` B side, keeps a scrollback buffer, fans out to attached clients, and audits every attachment; `skep-api` relays bytes after authenticating and authorising | One reader of the device, audit with the actor on the agent's side, and the API process never needs device access. |
| B. The runner exposes a per-VM Unix socket that `skep-api` connects to directly | Skips the agent, so attachment is not in the operation audit log and a second peer-credential check is needed in every runner. |
| C. Server-sent events for output and `POST` for input | Two half-channels for a byte stream. |

**Recommendation: A.**

- `GET /api/v1/vms/{id}/console/serial` upgrades to a WebSocket. It needs
  `vm:console`. A browser sends the cookie and `Origin` on the handshake,
  so the session check and the origin check of ADR 0006 apply unchanged;
  a bearer client sends `Authorization`. The attachment is written to the
  access audit log (`vm.console.attach`).
- `skep-api` opens `/internal/v1/vms/{id}/console/serial` on the agent
  socket with the usual actor headers; the agent writes
  `vm.console.attach` to the operation audit log with the actor and the
  verified peer UID, and `vm.console.detach` with the duration and byte
  counts when it ends.
- The agent sends the last 64 KiB of output on attach, then live output.
  Input from any attached client goes to the guest. At most 4 attachments
  per VM; a fifth is refused with `console_busy`.
- **A console does not outlive its session.** `skep-api` re-checks the
  session once a minute and closes the WebSocket when it has ended,
  expired, or been revoked.
- **Typing is activity; output is not** (amendment b). Input sent to the
  guest (a keystroke frame from the client) counts as session activity:
  `skep-api` refreshes the session's `last_seen_at` when it relays input,
  at most once a minute, the same throttle ADR 0006 applies to every
  request. Output from the guest never counts, and neither does the
  WebSocket's own keep-alive, so an open console on an unattended screen
  idles out like any other page while a user typing in one stays logged
  in. The once-a-minute session re-check above then closes the console
  of a session that has idled out.
- Frames are binary; the UI uses xterm.js (`@xterm/xterm`, with the fit
  addon), a new frontend dependency. No terminal emulation happens on
  the server.
- The Go standard library has no WebSocket server. Options:
  `golang.org/x/net/websocket` (same module family as the existing
  dependencies, but described by its own maintainers as lacking features
  and not recommended for new code), `github.com/coder/websocket` (pure
  Go, no dependencies, maintained, a context-aware API), or
  `github.com/gorilla/websocket` (widely used, larger). **Recommendation:
  `github.com/coder/websocket`**, reason: a WebSocket server for the
  console, used on both hops. It is the one new Go dependency of the
  milestone.

## Decision 9: lifecycle operations as tasks

Every write is a task, including the ones that change only the database:
that keeps "one unfinished task per resource" serialising an update
against a start, and it keeps the audit trail in one place.

| Task type | Cancellable while running | Time limit | What it does |
|---|---|---|---|
| `vm.create` | Yes (cleans up what it made) | 10 min | Validates against capabilities and confinement; inserts the VM, disk, NIC, and volume rows (`provisioned = 0`) in the task's own transaction so the name is reserved at once; creates the dataset and zvols; copies the variable store; sets `provisioned = 1` |
| `vm.update` | No | 1 min | Changes CPU, memory, description, console settings, and attached media. Only while observed `stopped`. Disks and NICs are fixed in this milestone |
| `vm.start` | No | 2 min | Re-validates (bridge, media, memory budget, instance name free), launches the runner, waits for state `running`; sets `desired_state = running` first |
| `vm.stop` | No | 5 min | Sends the graceful stop; waits up to the request's `timeout` (default 90 s, maximum 300 s) for `stopped`; on timeout **fails** with `stop_timeout` and leaves the VM running. It never escalates. Sets `desired_state = stopped` first |
| `vm.force_stop` | No | 1 min | Signals force stop; waits for the runner to end; destroys a runner-less instance |
| `vm.restart` | No | 7 min | `vm.stop` then `vm.start` in one task; a stop timeout fails the task and the VM stays running |
| `vm.delete` | No | 10 min | Requires observed `stopped` and no runner; destroys the VM dataset recursively (`zfs destroy -r`, never `-R`), the persistent directory, and the rows; records the dataset names and sizes in the audit entry |

Rules shared by all of them:

- **Memory budget.** `vm.start` refuses with `insufficient_memory` when
  the memory of VMs observed `running` or `starting`, plus this VM's,
  would exceed physical memory minus `host_memory_reserve_bytes`.
- **CPU.** `cpu_count` between 1 and the lower of the host's logical CPUs
  and `hw.vmm.maxcpu`; the request may name sockets, cores, and threads
  in `config`, and their product must equal `cpu_count`.
- **Memory.** At least 128 MiB, a multiple of 1 MiB.
- **Name.** `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`, unique on the node.
- **Interruption.** Following ADR 0005, an interrupted `vm.create` leaves
  a row with `provisioned = 0` and whatever datasets it got to;
  `vm.delete` of such a VM removes what exists and does not fail on what
  does not. An interrupted `vm.start` leaves whatever the runner did; the
  watcher reports it; the user decides.
- **Refusals** are the task engine's: `resource_busy` when another task
  holds the VM, `idempotency_key_conflict`, `queue_full`.

The API:

| Method and path | Permission | Returns |
|---|---|---|
| `GET /api/v1/vms` | `vm:read` | A page of VMs, cursor paginated like tasks; filter by `state` |
| `POST /api/v1/vms` | `vm:write` | `202` with the VM (unprovisioned) and the `vm.create` task; `Idempotency-Key` honoured |
| `GET /api/v1/vms/{id}` | `vm:read` | The VM with desired and observed state, disks, NICs, media, console settings |
| `PUT /api/v1/vms/{id}` | `vm:write` | `202` with the `vm.update` task; fields left out are unchanged, as `PUT /users/{id}` does |
| `DELETE /api/v1/vms/{id}` | `vm:delete` | `202` with the `vm.delete` task |
| `POST /api/v1/vms/{id}/actions` | `vm:power` | Body `{"action": "start" \| "stop" \| "force_stop" \| "restart", "timeout": seconds}`; `202` with the task |
| `GET /api/v1/vms/{id}/console/serial` | `vm:console` | WebSocket |
| `GET /api/v1/storage/pools` | `storage:read` | The allow-listed pools with their observed health and space |
| `GET /api/v1/storage/media` | `storage:read` | The media directory listing |

`GET /api/v1/system` gains the bhyve capability detail (firmware found,
`nmdm` available, limits) so the UI can explain a refused start before it
is attempted.

## Decision 10: permissions and audit

| Option | Notes |
|---|---|
| A. One permission, `vm:manage` | Cannot give an operator power control without also giving deletion. |
| B. `vm:read`, `vm:write` (create, update), `vm:delete`, `vm:power`, `vm:console`, plus `storage:read` | Deletion destroys data and is its own permission. The console is its own permission because it is a login on the guest, not a read of Skep. `storage:read` exists so the create form can list pools and media without a VM permission implying storage visibility. |
| C. B with per-VM scope | ADR 0006 decision 5 deferred scoping; nothing changed. |

**Recommendation: B.** Migration `api/0004` drops and recreates the
built-in role triggers as `api/0002` anticipated and updates the rows:
`operator` gains all six, `viewer` gains `vm:read` and `storage:read`,
`admin` is `*` already. The route table test that calls every route as
every role is extended, and the test that fails on a route without a
permission covers the WebSocket route too.

Audit, in both logs, following ADR 0005:

- Each task writes `requested` and `finished` entries with the task type
  as the action and the VM as the resource. `vm.delete`'s `finished`
  entry lists the datasets destroyed and their sizes; `vm.create`'s lists
  the datasets created. Nothing in any entry is a path a caller supplied.
- Refused submissions (`pool_not_allowed`, `bridge_not_allowed`,
  `engine_unsupported`, `firmware_unsupported`, `insufficient_memory`,
  `insufficient_space`, `media_not_found`, `bhyve_unavailable`) are
  `requested` entries with outcome `denied` or `failure` as ADR 0005's
  table says, and no task is created.
- Console attach and detach are their own entries (Decision 8b), in both
  the access log and the operation log, with the request ID linking them.
- `skep-api` audits every denial as it does today.

## Decision 11: what "verified on FreeBSD 15.1" means

A feature in this milestone is **verified** when its named check below has
been run on `hearst` (FreeBSD 15.1-RELEASE, amd64) with the FreeBSD build
of the binaries, and the check's output is recorded under
`tests/integration/vm/` next to the harness that ran it, the way
`tests/integration/rcd` and `tests/integration/retention` record theirs.
A feature with no recorded check is **pending FreeBSD validation**
however green CI is, and the README's "what works" table says which is
which. A check that was run and failed is recorded as failed, with the
output.

| Feature | Check |
|---|---|
| Capabilities | With `vmm` unloaded: `/system` says `unavailable` with the reason, and `vm.start` is refused with `bhyve_unavailable`. With `vmm` loaded and the firmware installed: `available`, with the limits |
| Confinement | A create naming a pool not in `storage.pools`, and a start naming a bridge not in `network.bridges`, are refused and audited; nothing is created |
| `vm.create` on ZFS | The dataset tree and zvols exist with the recorded properties (`volmode=dev`, `volblocksize=16K`, the reservation or `-s`, the user properties); the row is `provisioned`; a thin disk larger than `available` carries the warning; a full one is refused |
| Install from media | A VM with 2 vCPUs, 1 GiB, one 8 GiB thin disk, one NIC on `bridge0`, and the FreeBSD 15.1 disc1 ISO attached starts; the firmware, loader, and installer appear on the serial console in the browser; FreeBSD is installed to the zvol through that console |
| Guest reboot | The installer's final reboot exits bhyve with status 0 and the runner starts it again without the agent's involvement; the guest comes up from its disk |
| Media detach | `vm.update` while stopped detaches the ISO; the next start boots from disk |
| Networking | Through `bridge0`, the guest gets a DHCP address from the lab's Kea pool on VLAN 60 (Decision 5c), reaches the internet, and **cannot** reach `10.79.20.0/24` (`hearst`'s management address and the rest of the management network); the tap has the `skep:` description; the tap is gone after stop |
| `vm.stop` | An ACPI stop shuts the FreeBSD guest down cleanly within the timeout; `observed_state` is `stopped`; the instance and the taps are gone. A stop of a guest that ignores ACPI (the installer's menu) times out, fails with `stop_timeout`, and leaves the VM running |
| `vm.force_stop` | Ends a running guest; destroys a runner-less `/dev/vmm` instance left by killing the runner by hand |
| `vm.restart` | Stop and start in one task; the task ID is in both audit phases |
| Agent restart | `service skep_agent restart` and `kill -9` of the agent while a guest runs: the guest keeps running, the console reconnects, the restarted agent reports `running` with the right PID before it serves its socket |
| Host reboot | After `reboot`, the VM is observed `stopped`, `desired_state` is still `running`, and the API shows the drift; nothing starts by itself |
| `vm.delete` | Refused while running; after a stop, the datasets, the persistent directory, and the rows are gone; the audit entry lists what was destroyed; the media file is untouched |
| Permissions | `viewer` can list and read VMs and storage and is refused `POST /vms`, every action, `DELETE`, and the console, each refusal audited; `operator` can do all of them |
| Console | Attach and detach are in both audit logs with the same request ID; a fifth attachment is refused; the console closes within a minute of logging out elsewhere; with a shortened idle timeout, a console left unattended while the guest prints output idles out and closes, and one in which a user keeps typing does not |
| Concurrency | A second `start` while the first is pending gets `resource_busy` naming the task |
| Interrupted create | A `vm.create` interrupted by killing the agent leaves an unprovisioned VM; `vm.delete` cleans it up |
| Unit tests natively | The `hostops`, `hypervisor/bhyve`, runner, and console packages' tests pass on `hearst` |

Not verifiable on the lab host and recorded as pending: a ZFS-root host; a
non-thin disk on a pool with real disks; SMT topology in the guest; an
`e1000` NIC; `nvme` and `ahci-hd` disk buses beyond a smoke boot; a tmpfs
`/var/run` (still pending from ADR 0004).

## Decision 12: clients and what is not in this milestone

| Option | Notes |
|---|---|
| A. The web interface gets the VMs section (list, detail, create form, actions, media attach and detach, the serial console); `skepctl` gets `vm list`, `vm show`, `vm create -f file.json`, `vm start|stop|force-stop|restart|delete` | The UI is the console's natural client; `skepctl` gets the operations a script needs. Both use only the public API. |
| B. `skepctl` also attaches to the console | A WebSocket client in `skepctl` with raw terminal handling. Useful; a follow-up. |
| C. API only, no clients | The milestone's name is "first working VM", and a VM nobody can see installed is not that. |

**Recommendation: A**, with B as the first follow-up after validation.

Explicitly **not** in this milestone, so that nobody looks for them: the
QEMU adapter; NAT or any bridge management; VLAN tags on NICs; adding,
removing, or resizing disks after creation; hot-plug of anything;
snapshots and clones; cloning a VM; templates; VNC; autostart at host
boot; migration; PCI passthrough; CPU pinning; a `directory` pool for
file-backed disks on UFS-root hosts.

## Consequences

- The agent changes the host for the first time. Everything it does goes
  through `internal/hostops`, is bounded by the root-owned confinement
  configuration, and is a task with two audit entries.
- Two new dependencies: `github.com/coder/websocket` (Go) and
  `@xterm/xterm` with its fit addon (frontend). Both are named in the
  decisions with their reasons.
- A new long-lived root process per running VM (the runner), observable
  through state files, and signal-controlled by the agent.
- The bhyve instance name, the `nmdm` names, the dataset names, the tap
  descriptions, and the SMBIOS UUID all derive from the VM's UUID, so an
  operator at a shell can tie any of them back to the API without a
  lookup.
- The API gains the VM and storage routes above, six permissions, and a
  WebSocket endpoint. `api/openapi.yaml` describes the WebSocket route
  with its permission even though OpenAPI cannot describe the stream.
- A fresh install still cannot create a VM until the operator names a
  pool and a bridge in `agent.json`. That is the point.
- UFS-root hosts without a pool wait for the storage milestone.

## Suggested implementation order

Each step is one reviewable change that leaves the tree working, with the
fake `hostops` carrying the Linux build and tests throughout.

1. `internal/hostops` interfaces, the FreeBSD implementation of the ZFS
   and inspection calls, the Linux fake, the confinement configuration,
   and migrations `agent/0004` and `api/0004` (Decisions 1, 2, 10).
2. `vm.create`, `vm.update`, `vm.delete`, task submission over the agent
   socket, the VM routes without actions, `storage/pools`,
   `storage/media` (Decisions 3, 4, 9). First validation on `hearst`:
   datasets.
3. The runner, the watcher, `vm.start`, `vm.stop`, `vm.force_stop`,
   `vm.restart`, the actions route (Decisions 5, 6, 7, 9). Second
   validation: a guest boots from the ISO with its console read from
   `/dev/nmdm-...B` by hand.
4. The console broker, the WebSocket relay, the audit entries
   (Decision 8).
5. The web interface's VMs section and `skepctl` (Decision 12).
6. The full check table of Decision 11 on `hearst`, the recorded outputs,
   the documentation updates (README, architecture overview, security
   overview, administration guide), and this ADR's implementation notes.

## Changes hearst needs

All of these were approved at acceptance (review answer 10). Items 1 to
3 were done by Andy before acceptance; items 4 to 11 are applied during
the build, in the order of the implementation steps that need them. They
are grouped by where the change happens.

**Already done**

- 4 vCPUs and 6 GiB. Observed today: `hw.ncpu` 4, `hw.realmem` 6 GiB.

**On the Proxmox host `swearengen`, for Decision 5c option B** (done)

1. A second network device on `hearst`, `net1` (virtio), on VLAN 60, the
   lab's untrusted LAB VLAN, with the Proxmox firewall **disabled on that
   device** (which also disables its MAC filter). No change to `net0`.
2. DHCP for guests: the lab's Kea pool on VLAN 60. The LAB firewall
   rules block the VLAN from private networks.
3. The Proxmox snapshot `pre-m6` of `hearst` (Skep will not rely on
   it).

**On hearst, `/boot/loader.conf`** (persistent; survives reboots)

4. `vmm_load="YES"` (Decision 6b).
5. `nmdm_load="YES"`, unless the first open of a `/dev/nmdm*` node turns
   out to load it on 15.1; the first validation step records which.
6. Optional, recommended on a 6 GiB host with a file-backed pool:
   `vfs.zfs.arc.max="1073741824"`, so the ARC leaves room for a 1 GiB
   guest and the host.

**On hearst, `/etc/rc.conf`** (persistent)

7. `zfs_enable="YES"`, so `zfs.ko` loads at boot and `skeptest` is
   re-imported from the cache file after the first manual import
   (`zpool import -d /var/tmp/skep skeptest` once, as root). Whether a
   file vdev under `/var/tmp` re-imports at boot on a UFS root is one of
   the things to confirm; the alternative is a manual import at the start
   of each test session and no `rc.conf` line.
8. `cloned_interfaces="bridge0"`, `ifconfig_vtnet1="up"`,
   `ifconfig_bridge0="addm vtnet1 up"` (Decision 5c option B; for option D
   only the first line; nothing at all for option E, which instead needs
   `pkg install libslirp` under "packages and files").

**On hearst, packages and files**

9. `pkg install edk2-bhyve` (Decision 6a). The firmware directory it
   installs is then put in `hypervisor.bhyve.firmware_dir`.
10. The FreeBSD 15.1-RELEASE amd64 disc1 ISO (about 1.3 GiB; 95 GiB free
    on `/`) in the media directory of the test installation, fetched on
    `hearst` or copied from `merrick`. If the download fails on name
    resolution, that is reported, not worked around.
11. A Milestone 6 test installation under `/home/aba/skep-m6`, like the
    Milestone 5 one, with an `agent.json` carrying the confinement block
    of Decision 1c for `skeptest`, `bridge0`, and `vtnet0`. This is
    within the existing test arrangement and listed for completeness.

**What stays untouched:** `vtnet0` and its address, pf (stays off),
forwarding (stays off), the root filesystem beyond the lines above, and
every disk other than `/var/tmp/skep/skeptest.img`. Loading and unloading
`vmm` by hand between tests stays within the standing permission.

## Decisions

Accepted on 2026-11-14 with these answers to the ten review questions.

1. **Bridged only, with the second NIC on hearst** (Decisions 5a and 5c,
   option B), with one change: `net1` is on VLAN 60, the lab's untrusted
   LAB VLAN with the firewall off on the device, not on VLAN 20 with
   `net0`. Guests get DHCP from the lab's Kea pool on VLAN 60, and the
   LAB firewall rules block them from private networks. `net1` is added
   and the snapshot `pre-m6` taken. Options D and E are not needed.
2. **Serial console only, VNC deferred**, with the agent-side RFB
   authentication relay as its future design (Decision 8a).
3. **`github.com/coder/websocket`** as the one new Go dependency
   (Decision 8b).
4. **Full reservation by default, `thin` opt-in** (Decision 3b).
5. **`vm.stop` never escalates** to a force stop (Decision 9).
6. **No reconciliation and no autostart** in this milestone (Decision 2c).
7. **Disks and NICs fixed at creation**, media attachable while stopped
   (Decisions 3c, 9).
8. **The six permissions** and their assignment to `operator` and
   `viewer` (Decision 10).
9. **The runner as a subcommand of `skep-agent`** (Decision 7a).
10. **All the hearst changes are approved**, applied during the build
    (see [Changes hearst needs](#changes-hearst-needs)).

Two amendments were made at acceptance and are worked into the text.

- **a. The networking check proves isolation as well as reach.** Decision
  5c and the Networking check of Decision 11 now say: the guest gets a
  DHCP address on VLAN 60 through `bridge0`, reaches the internet, and
  cannot reach `10.79.20.0/24`. The proposal asked only for an address
  and the gateway.
- **b. Console input is session activity; output is not.** Keystrokes
  sent to the guest refresh the session, at most once a minute; output
  from the guest does not. An unattended console still idles out; a user
  typing in one does not (Decision 8b, and the Console check of Decision
  11). The proposal counted no console traffic as activity, which would
  have logged out a user in the middle of a long session at the guest's
  console.

## Implementation notes

Recorded during the build (2026-11-14). The decisions above stand; these
are the places where the implementation had to choose something the text
left open, or departs from it.

- **A third frontend dependency, `@xterm/addon-webgl`.** Decision 8b names
  `@xterm/xterm` and its fit addon. xterm.js's default DOM renderer
  injects `<style>` elements, which the web interface's CSP
  (`style-src 'self'`, ADR 0008) blocks; the browser test caught six
  violations. Loosening the CSP was not an option. The WebGL renderer
  (same project, same licence) draws on a canvas and, loaded before the
  terminal opens, xterm never creates the DOM renderer. The DOM renderer
  is never used: without WebGL 2 (checked before the terminal is built),
  if the WebGL canvas is not in place after opening, or when the context
  is lost, the console shows "console unavailable" with Reconnect, and the
  browser test checks the context-loss path for CSP violations.
  `screenReaderMode` keeps the rows as text for assistive technology.
- **Durable runtime writes.** After KVM stopped `hearst`, the writes of the
  seconds before (0-byte binaries among them) were lost; every file the agent and
  the runner write under the run and state directories (`spec.json`,
  `state.json`, `runner.pid`, `bhyve.conf`, the per-VM `uefi-vars.fd`) is
  written to a temporary file, fsynced, renamed, and the directory
  fsynced; a new VM directory's entry is fsynced too. Lab deploys go
  through `scripts/deploy-test.sh`, which verifies size and SHA-256 and
  ends with `sync`.
- **Runner identity.** The agent recognises a runner by its PID file and
  by the process's own argument list from `kern.proc.args`, compared with
  the exact command for that VM, before it sends any signal.
- **The runner reads a spec, not the configuration.** The agent writes
  `spec.json` (the VM, its derived paths, and the confinement) into the
  root-only runtime directory; the runner never reads `agent.json` or the
  database, so the agent's flags and data directory need not be repeated
  on the runner's command line.
- **A reboot loop ends the run.** More than 10 guest reboots in a minute
  end it as `failed`. The ADR's runner restarted bhyve after every status
  0 without a bound.
- **A stop request that times out stays pending.** After `stop_timeout` the
  VM keeps running, as decided; the runner still remembers the request, so
  a guest that obeys the power button late is then stopped, not rebooted.
  `desired_state` is already `stopped` at that point.
- **Force stop removes a dead runner's record.** After it has destroyed a
  runner-less instance and its taps, the leftover run state is removed, so
  the VM shows `stopped` instead of the failure that no longer exists.
- **The console is opened when the watcher first sees the VM running**,
  not at the first attach, so the scrollback holds the boot from the
  start. A client that falls more than 512 chunks behind is disconnected.
- **HTTP status codes** of the new error codes: a request naming
  something the host does not offer is 422 (`pool_not_allowed`,
  `bridge_not_allowed`, `engine_unsupported`, `firmware_unsupported`,
  `media_not_found`); one the state refuses is 409 (`vm_not_stopped`,
  `vm_not_running`, `insufficient_*`, `bhyve_unavailable`, `stop_timeout`,
  `console_busy`, and the new `vm_start_failed`); `not_implemented` is 501.
- **Nested bhyve under KVM.** The first start on `hearst` stopped the whole
  VM: KVM on `swearengen` failed to emulate bhyve's first `VMLAUNCH`
  (QEMU: `KVM internal error. Suberror: 1`, `0f 01 c2`), a known
  nested-virtualisation bug, not a Skep defect. The workaround is
  `hw.vmm.vmx.cap.virtual_interrupt_delivery=0` on the nested host
  (administration guide). **Follow-up:** a capability or health check
  that recognises a FreeBSD host running under KVM with APIC virtual
  interrupt delivery enabled and says so before a start, rather than
  letting the first `VMLAUNCH` take the host down. Not in this milestone.
- **Validation so far.** ZFS provisioning, confinement refusals, and
  deletion passed on `hearst`. The first `vm.start` made `hearst`
  unreachable; see [tests/integration/vm](../../tests/integration/vm/README.md)
  for the record and the state of every check.

