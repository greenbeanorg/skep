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
| Capabilities: `available` with the limits once `vmm`, `nmdm`, and the firmware are in place | **passed** | [step2-datasets.txt](results/step2-datasets.txt) |
| Capabilities: `unavailable` with the reason and `bhyve_unavailable` when `vmm` is unloaded | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Confinement: a pool and a bridge outside the allow-lists are refused and audited, nothing created | **passed** | [step2-datasets.txt](results/step2-datasets.txt) |
| `vm.create` on ZFS | **passed** | [step2-datasets.txt](results/step2-datasets.txt) |
| Install from media | **passed** (the firmware writes nothing to COM1; loader, kernel, installer, and login seen in the browser) | [step6-checks.txt](results/step6-checks.txt); first attempt: [step3-first-start-FAILED.txt](results/step3-first-start-FAILED.txt), workaround: [step3-manual-bhyve-probe.txt](results/step3-manual-bhyve-probe.txt) |
| Guest reboot | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Media detach | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Networking: DHCP, internet, no reach to `10.79.20.0/24`, tap described and removed | **failed (isolation)**: DHCP, internet, and the tap passed; the guest reaches `10.79.20.0/24` because the lab firewall routes VLAN 60 to VLAN 20 (nothing on hearst forwards or bridges it) | [step6-checks.txt](results/step6-checks.txt) |
| `vm.stop` (clean; ACPI ignored → `stop_timeout`, keeps running) | **passed** (an ACPI-ignoring guest made with `hw.acpi.power_button_state=NONE`; the UEFI loader menu does not ignore it) | [step6-checks.txt](results/step6-checks.txt) |
| `vm.force_stop` (running guest; runner-less instance) | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| `vm.restart` | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Agent restart and `kill -9` | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Host reboot | see below | |
| `vm.delete` | **passed** | [step6-checks.txt](results/step6-checks.txt), [step2-datasets.txt](results/step2-datasets.txt) |
| Permissions | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Console (audit, fifth refused, closed after logout, idle) | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Concurrency | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Interrupted create | **passed** | [step6-checks.txt](results/step6-checks.txt) |
| Unit tests natively | **passed** | [step6-checks.txt](results/step6-checks.txt) |

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
