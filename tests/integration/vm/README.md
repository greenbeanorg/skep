# VM integration checks (Milestone 6)

The named checks of [ADR 0009 decision 11](../../docs/decisions/0009-first-vm-bhyve.md#decision-11-what-verified-on-freebsd-151-means),
run on `hearst` (FreeBSD 15.1-RELEASE-p6, amd64, itself a KVM guest on
Proxmox with CPU type `host`) with the FreeBSD build of the binaries,
against the test installation in `/home/aba/skep-m6`. The output of every
run is in [results/](results/). A check without a recorded passing run is
**pending FreeBSD validation**, whatever CI says.

Most checks drive the public API with `curl` (the server's key pinned,
never `-k` alone) and read the host with read-only commands (`zfs list`,
`ifconfig`, `ls /dev/vmm`, the run state files). The guest installation is
done through the serial console in a browser.

## Status

| Check | Status | Record |
|---|---|---|
| Capabilities: `available` with the limits once `vmm`, `nmdm`, and the firmware are in place | **passed** | [step2-datasets.txt](results/step2-datasets.txt) (agent log: `bhyve is available ... max_vcpus_per_vm 4, guest_memory_bytes 5328257024`) |
| Capabilities: `unavailable` with the reason and `bhyve_unavailable` when `vmm` is unloaded | pending | |
| Confinement: a pool and a bridge outside the allow-lists are refused and audited, nothing created | **passed** (pool, bridge, engine) | [step2-datasets.txt](results/step2-datasets.txt) |
| `vm.create` on ZFS: datasets, `volmode=dev`, `volblocksize=16K`, reservation or none, user properties, warning for a thin disk over `available`, refusal of a full one | **passed** | [step2-datasets.txt](results/step2-datasets.txt) |
| `vm.delete`: the VM's datasets and directory and rows gone, the audit entry lists them, the media untouched | **passed** (refused-while-running part pending) | [step2-datasets.txt](results/step2-datasets.txt) |
| Install from media: firmware, loader, and installer on the browser console; FreeBSD installed to the zvol | **failed**: `hearst` stopped at the first `vm.start`; cause: KVM on `swearengen` failed to emulate bhyve's first `VMLAUNCH` (nested-virtualisation bug, not Skep). Retry after the APIC virtual interrupt delivery workaround and a manual bhyve test, both awaiting approval | [step3-first-start-FAILED.txt](results/step3-first-start-FAILED.txt) |
| Host reboot (partial, unplanned): after the forced restart the VM is observed `stopped` and nothing started by itself; the boot configuration (`vmm`, `nmdm`, `zfs`, `skeptest` import, `bridge0`) came up | **passed** for those parts (the drift part is pending: the reset also lost the start task, so `desired_state` is `stopped`) | [step3-first-start-FAILED.txt](results/step3-first-start-FAILED.txt) |
| Guest reboot: the runner restarts bhyve after status 0, the guest comes up from disk | pending | |
| Media detach: the next start boots from disk | pending | |
| Networking: DHCP on VLAN 60, internet reachable, `10.79.20.0/24` not; tap described, gone after stop | pending | |
| `vm.stop`: clean ACPI shutdown; a guest that ignores ACPI times out with `stop_timeout` and keeps running | pending | |
| `vm.force_stop`: ends a guest; destroys a runner-less instance | pending | |
| `vm.restart`: one task, both audit phases | pending | |
| Agent restart and `kill -9`: the guest keeps running, the console reconnects, the right PID before the socket is served | pending | |
| Host reboot: VM observed `stopped`, desired `running`, drift shown, nothing starts | pending | |
| Permissions: viewer refused create, actions, delete, console, each audited; operator allowed | pending on FreeBSD (passed in the Linux end-to-end and browser tests) | |
| Console: attach/detach in both logs with one request ID; a fifth refused; closed within a minute of logout; idles out unattended, not while typing | pending on FreeBSD (all but the idle parts passed in the Linux end-to-end test) | |
| Concurrency: a second start while the first is pending gets `resource_busy` | pending | |
| Interrupted create: an unprovisioned VM left, `vm.delete` cleans it up | pending | |
| Unit tests natively: `hostops`, `hypervisor/bhyve`, runner, console packages on `hearst` | pending | |

Not verifiable on this host and pending regardless: a ZFS-root host; a
non-thin disk on a pool with real disks; SMT topology in the guest; an
`e1000` NIC; `nvme` and `ahci-hd` beyond a smoke boot; a tmpfs `/var/run`.

## The test installation

```
/home/aba/skep-m6/skep-agent, skep-api, skepctl   FreeBSD builds from merrick
/home/aba/skep-m6/agent.json                      root:wheel 0600, the confinement below
/home/aba/skep-m6/media/                          root:wheel 0700, the disc1 ISO (sha256 checked)
```

```json
{
  "data_dir": "/var/db/skep",
  "hypervisor": { "bhyve": { "firmware_dir": "/usr/local/share/edk2-bhyve", "host_memory_reserve_bytes": 1073741824 } },
  "storage": { "pools": ["skeptest"], "dataset_root": "skep", "media_dir": "/home/aba/skep-m6/media" },
  "network": { "bridges": ["bridge0"], "management_interfaces": ["vtnet0"] }
}
```

`rc.conf` points `skep_agent_bin`, `skep_agent_config`, and `skep_api_bin`
at it. The host changes it relies on are the ADR's "Changes hearst needs",
items 4 to 11, recorded with the commands in
[step2-datasets.txt](results/step2-datasets.txt).
