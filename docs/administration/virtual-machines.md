# Virtual machines

Skep runs virtual machines with bhyve, the hypervisor of the FreeBSD®
operating system: UEFI guests with disks on ZFS, bridged virtio
networking, and a serial console in the web interface. The design is
[ADR 0009](../decisions/0009-first-vm-bhyve.md).

> **Validation status.** Verified on FreeBSD 15.1-RELEASE (amd64,
> UFS-root, itself a KVM guest): FreeBSD was installed into a VM through
> the browser console, and every check in
> [tests/integration/vm](../../tests/integration/vm/README.md) passed
> (guest isolation once the lab firewall was fixed). A ZFS-root
> host, real disks, `e1000`, `nvme`, and `ahci-hd` are still pending.

## What this release does and does not do

| Does | Does not (yet) |
|---|---|
| Create a VM with up to 8 disks (zvols) and install media, and up to 4 NICs | Add, remove, or resize disks or NICs after creation |
| Start, stop (ACPI), force-stop, restart, delete | Restart a VM by itself: no autostart, no reconciliation |
| Attach and detach ISO files while the VM is stopped | Upload or download ISO files |
| Serial console in the browser, for up to 4 clients at once | VNC or any graphical console |
| Bridge NICs to bridges the operator created | Create bridges, VLAN tags, NAT, or firewall rules |
| ZFS pools named in the configuration | UFS-root hosts without a ZFS pool; QEMU |

## Preparing the host

Skep never changes the host's kernel, boot, or network configuration by
itself. An administrator does the following once.

1. **Load the hypervisor at boot.** In `/boot/loader.conf`:

   ```
   vmm_load="YES"
   nmdm_load="YES"
   ```

   `vmm` is bhyve's kernel module, `nmdm` provides the serial console
   devices. To load them now without a reboot: `kldload vmm nmdm`.
   On a host with little memory and ZFS, also cap the ARC, for example
   `vfs.zfs.arc.max="1073741824"`, so that guests and the ARC both fit.

2. **Install the UEFI firmware.** `pkg install edk2-bhyve`. It installs
   `/usr/local/share/edk2-bhyve/BHYVE_UEFI.fd` and the variable store
   template `BHYVE_UEFI_VARS.fd`. Each VM gets its own copy of the
   variable store, so it remembers its boot entries.

3. **Have a ZFS pool.** `zfs_enable="YES"` in `/etc/rc.conf`. Skep keeps
   everything it creates under `<pool>/skep/vm/<vm-id>`, and touches no
   other dataset.

4. **Create a bridge for guests, separate from management.** For example,
   with a second interface `vtnet1` for guest traffic:

   ```
   # /etc/rc.conf
   cloned_interfaces="bridge0"
   ifconfig_vtnet1="up"
   ifconfig_bridge0="addm vtnet1 up"
   ```

   The bridge needs no address. Skep refuses to put a guest on a bridge
   that has a management interface as a member. The switch port or the
   upstream hypervisor must accept frames from MAC addresses it did not
   assign (on Proxmox, the VM firewall's MAC filter must be off for that
   device), and guests need a DHCP server on that segment if they are to
   get addresses.

5. **Name what Skep may use** in `/usr/local/etc/skep/agent.json`
   (root-owned; the API cannot change it):

   ```json
   {
     "hypervisor": {
       "bhyve": {
         "firmware_dir": "/usr/local/share/edk2-bhyve",
         "host_memory_reserve_bytes": 1073741824
       }
     },
     "storage": {
       "pools": ["tank"],
       "dataset_root": "skep",
       "media_dir": "/var/db/skep/media"
     },
     "network": {
       "bridges": ["bridge0"],
       "management_interfaces": ["vtnet0"]
     }
   }
   ```

   With empty `pools` (the default) no VM can be created; with empty
   `bridges` no VM can have a NIC. `host_memory_reserve_bytes` is memory
   left to the host: a VM does not start when the running VMs and it would
   need more than physical memory minus the reserve. Restart the agent
   after a change: `service skep_agent restart` (running VMs are not
   affected).

6. **Put install media in place.** Copy ISO files into `media_dir`
   (`root:wheel`, `0700`). Only regular files named like
   `FreeBSD-15.1-RELEASE-amd64-disc1.iso` (letters, digits, `.`, `_`,
   `-`, ending in `.iso`) are listed. Verify the checksum before use. A
   file that is still being copied is listed too, so copy to another name
   and rename it when it is complete.

The system page of the web interface, and `skepctl system --json`, show
whether bhyve is available and, if not, exactly what is missing.

### A FreeBSD host that is itself a KVM guest (nested bhyve)

bhyve inside a KVM virtual machine (for example a FreeBSD VM on Proxmox
with CPU type `host`, as the lab host `hearst` is) can hit a KVM bug on
the guest's first `VMLAUNCH`: KVM on the outer host fails to emulate the
instruction and stops the whole FreeBSD VM. Nothing is logged inside
FreeBSD and no crash dump is written; the outer host's QEMU log shows
`KVM internal error. Suberror: 1` with the instruction bytes
`0f 01 c2` (`VMLAUNCH`). On `hearst` this happened at the first start of
a VM through Skep, on 2026-11-14.

The workaround is to turn off APIC virtual interrupt delivery in bhyve on
the nested FreeBSD host. The tunable, read when `vmm.ko` loads
(`sys/amd64/vmm/intel/vmx.c`), is
`hw.vmm.vmx.cap.virtual_interrupt_delivery` (the older name
`hw.vmm.vmx.use_apic_vid` is still accepted). With it at 0, posted
interrupts are off too.

```
# now, with no VM running
kenv hw.vmm.vmx.cap.virtual_interrupt_delivery=0
kldunload vmm && kldload vmm
sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery   # must print 0

# at every boot: /boot/loader.conf
hw.vmm.vmx.cap.virtual_interrupt_delivery="0"
```

It costs some interrupt-delivery performance in guests, which matters
little in a nested lab and is not needed on bare metal. **Status on
`hearst`: applied and verified** (both caps 0, also after a reboot; a
plain bhyve boot and then a full FreeBSD installation through Skep ran
without trouble); see
[tests/integration/vm](../../tests/integration/vm/README.md). Skep does
not detect this condition yet (a follow-up).

## Creating and installing a VM

In the web interface: **Virtual machines → Create VM**. Choose the CPUs,
memory, one disk on an allowed pool (thin or fully reserved), optional
install media, and a bridge. The form covers the common case; a definition
file with `skepctl vm create -f` can have more disks and NICs:

```json
{
  "name": "fbsd1",
  "cpu_count": 2,
  "memory": "1G",
  "disks": [
    { "size": "8G", "pool": "tank", "thin": true },
    { "media": "FreeBSD-15.1-RELEASE-amd64-disc1.iso" }
  ],
  "nics": [{ "bridge": "bridge0" }]
}
```

Creating a VM creates its datasets and zvols (`volmode=dev`,
`volblocksize=16K`, a full `refreservation` unless `thin`). A full disk
larger than the pool's available space is refused; a thin disk may be,
with a warning in the task result.

Then **Start**, and **Console**. With install media attached and an empty
disk the firmware boots the ISO. Install as usual through the console.
When the installer reboots, bhyve exits and Skep's runner starts it again
by itself. Stop the VM, **Detach** the ISO, and start it again: it boots
from its disk.

## Power actions

| Action | What happens |
|---|---|
| Start | `desired_state` becomes `running`; the runner creates the taps and starts bhyve; the task succeeds when the guest runs |
| Stop | `desired_state` becomes `stopped`; the guest gets an ACPI power-button press (with UEFI firmware this powers off even a guest still at its loader menu) and has `timeout` seconds (default 90, at most 300). If it does not shut down, the task **fails** with `stop_timeout` and the VM keeps running. A stop never turns into a force stop by itself |
| Force stop | The guest is powered off at once, as pulling the plug would. Also cleans up a VM whose runner died |
| Restart | A stop followed by a start, in one task |
| Delete | Only when stopped. Destroys the VM's datasets (recursively, only its own), its firmware variables, and its records. ISO files are not touched |

A guest that powers itself off, crashes, or is stopped from inside stays
stopped: Skep restarts nothing. The VM then shows **drift** (desired
`running`, observed `stopped`). After a host reboot every VM is stopped
and the ones that were running show drift.

A guest that reboots more than 10 times in a minute is stopped and marked
`failed`.

## The console

The serial console is COM1 of the guest. FreeBSD's loader, installer,
and installed system use it when the firmware has no graphical console.
The UEFI firmware itself prints nothing there, so the first output after
a start is the loader's. The
agent keeps the last 64 KiB of output from the moment the VM starts, so a
client that attaches later sees what came before. Up to four clients can
attach at once; their input all goes to the guest.

A console is a login on the guest, so it needs its own permission,
`vm:console`. It ends when the session ends (checked every minute).
Typing in it counts as using the session; output from the guest does not,
so a console left open on an unattended screen logs out like any other
page. Every attachment is in both audit logs.

The console in the browser needs WebGL 2. Without it, or when the
browser takes the graphics context back, the console says "console
unavailable" and offers Reconnect; the VM is not affected. (The terminal
library's other renderer would break the interface's Content Security
Policy, so it is never used.)

`skepctl` does not attach to consoles yet.

## Permissions

| Permission | Allows | operator | viewer |
|---|---|---|---|
| `vm:read` | List and read VMs | yes | yes |
| `vm:write` | Create VMs, change stopped ones, attach and detach media | yes | no |
| `vm:delete` | Delete a VM and its disks | yes | no |
| `vm:power` | Start, stop, force-stop, restart | yes | no |
| `vm:console` | Attach to the serial console | yes | no |
| `storage:read` | List the allowed pools and the media | yes | yes |

## On the host

Everything about a VM can be traced from a shell, by its ID:

| What | Where |
|---|---|
| bhyve instance | `/dev/vmm/skep-<id>`; `bhyvectl --vm=skep-<id> --get-stats` |
| Process | `pgrep -lf vm-runner` (one runner per running VM) and its child `bhyve: skep-<id>` |
| Disks | `zfs list -r <pool>/skep/vm/<id>`; user properties `skep:vm_id`, `skep:disk_id` |
| Taps | `ifconfig` shows `description: skep:<id>:<slot>` |
| Console devices | `/dev/nmdm-skep-<id>.1A` (bhyve) and `.1B` (the agent) |
| Run state | `/var/run/skep/vm/<id>/state.json`, `runner.log` (gone after a reboot) |
| Configuration, firmware variables, bhyve output | `/var/db/skep/agent/vm/<id>/bhyve.conf`, `uefi-vars.fd`, `bhyve.log` |

A VM keeps running when the agent restarts or is killed: the runner is a
separate process, and the restarted agent finds it again before it answers
any request. Do not kill a runner by hand; if one dies, the VM is shown as
`failed` with the reason, and **Force stop** cleans up.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Start refused with `bhyve_unavailable` | The reason names what is missing: `vmm`, `nmdm`, or the firmware |
| Start refused with `bridge_not_allowed` | The bridge is not in `network.bridges`, does not exist, or has a management interface as a member |
| `insufficient_memory` | Running VMs plus this one exceed physical memory minus `host_memory_reserve_bytes` |
| Start fails with `vm_start_failed` | The task message carries the runner's detail; `bhyve.log` has bhyve's own output |
| The guest has no network | The tap is on the bridge (`ifconfig bridge0`)? The uplink accepts foreign MAC addresses? A DHCP server on that segment? |
| Guests reach networks they should not | Skep's bridge has no address and the host does not forward, so guest traffic leaves only through the guest network's own gateway; what a guest can reach is that network's firewall policy, not Skep's |
| The console shows nothing | Some guests print to a graphical console only; Skep has no graphical console yet |
