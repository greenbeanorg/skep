# Bare-metal validation runbook

Everything still marked "pending" in the docs that **only real hardware can
settle**, in the order to run it on one fresh machine. Each step has the
commands and the pass criterion. Record every result in
`tests/integration/bare-metal/results/<date>-<host>.md` with the logs, as the
other integration suites do; a step not run stays pending.

Sources of the list (2026-11-23): [host requirements](../../../docs/installation/host-requirements.md)
("Bare metal, AMD-V hosts"), the [VM guide](../../../docs/administration/virtual-machines.md)
and [tests/integration/vm](../vm/README.md) ("not verifiable on the lab host": non-thin
disk on real disks, SMT topology, `e1000`, `nvme`/`ahci-hd` beyond a smoke boot), the
[storage guide](../../../docs/administration/storage.md) ("real disks"),
[architecture overview](../../../docs/architecture/overview.md) (degraded pool, SMT core
count, system console prompt, VT-x probing, nested-KVM workaround),
[ADR 0011](../../../docs/decisions/0011-guest-port-anti-spoofing.md) decision 9 and the
[filtering guide](../../../docs/administration/guest-port-filtering.md) (throughput on real
hardware), and the zvol device-node miss rate
([results](../setup/results/zvol-device-node-20261122.md); only virtual disks so far).

**Already settled on the lab VMs, not repeated here:** ZFS-root (langrishe), the
package and `skep-setup` (smith, langrishe), the zvol repair and refusal, persistent
`kld_list` and `vlanfilter` across a reboot (hearst). Items that do not need hardware
(a tmpfs `/var/run`, QEMU detection, a host installed from distribution sets) belong to
the lab VMs.

## What you need

- Machine A: Intel CPU with VT-x, EPT and unrestricted guest, **with SMT on**, at
  least two physical disks for the data pool (a mirror) besides the boot disk, at
  least two NICs (management, guest uplink). 16 GiB RAM or more.
- Machine B (step 12 only): an AMD CPU with AMD-V and RVI (nested paging).
- A switch port for the guest uplink that carries VLAN 60 untagged (DHCP for guests)
  and one tagged test VLAN, plus a second machine on that network for step 10.
- FreeBSD 15.1-RELEASE amd64 install media; the Skep package for the release being
  checked, built as in [packaging](../../../docs/installation/packaging.md).

Throughout: `A=-mgmt <mgmt-if> -pool tank`, `SK="skepctl"` logged in from the
workstation with the key pin compared to `skep-api tls fingerprint` on the host
(never skip the comparison).

## 1. Record the hardware

```sh
sysctl hw.model hw.ncpu kern.smp.cores kern.smp.threads_per_core hw.physmem
dmesg | grep -E -A3 'VT-x|AMD Features2|SVM'
camcontrol devlist; nvmecontrol devlist 2>/dev/null
ifconfig -a | grep -E '^[a-z]|options='
```

Pass: recorded in the results file (model, cores, threads per core, RAM, disks, NICs
and their `options=` including `VLAN_HWTAGGING`/`VLAN_HWFILTER`).

## 2. Install FreeBSD on ZFS, then the package

`bsdinstall` with root on ZFS (Auto ZFS) on the boot disk. Then:

```sh
freebsd-version -ku; uname -a
zpool create tank mirror /dev/<disk1> /dev/<disk2>
pkg install ./skep-0.1.0.pkg            # pulls edk2-bhyve (packaging.md)
skep-setup check $A; skep-setup plan $A --create-bridge bridge0 --uplink <guest-if>
skep-setup apply $A --create-bridge bridge0 --uplink <guest-if> -start -admin <name>
shutdown -r now
```

Pass: `pkg install` lists `edk2-bhyve` "required by skep"; after the reboot `skep-setup
check $A` has no `kernel modules` warning, no firmware warning, `vlanfilter` on
`bridge0`, both services running, health `ok` (`skepctl system`).

## 3. Discovery on real hardware

```sh
$SK hardware --json > hw.json; $SK system --json > system.json
```

Pass: CPU model, **cores and threads match step 1 on an SMT machine** (pending: "core
count on a host with SMT"); memory within 1 % of `hw.physmem`; every disk and NIC of
step 1 listed; `bhyve` capability `available` with `vmm` loaded. Then `kldunload vmm`
(no guests), read the node again: the virtualization fields say what the architecture
overview promises while vmm is unloaded ("VT-x, EPT not probed"); `kldload vmm`.

## 4. bhyve without the nested-KVM workaround

On bare metal `hw.vmm.vmx.cap.virtual_interrupt_delivery` keeps its default.

```sh
sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery hw.vmm.vmx.cap.posted_interrupts
grep -c virtual_interrupt /boot/loader.conf    # must be 0: no workaround here
```

Pass: the sysctl reads 1 (or the CPU lacks the feature: record it) and no workaround is
configured. Steps 5 to 7 then run with it enabled; a host hang there is a failure.

## 5. A VM on real disks: non-thin, buses, NICs, topology

`vm.json`:

```json
{"name": "bm1", "cpu_count": 4, "memory": "2G",
 "config": {"cpu": {"sockets": 1, "cores": 2, "threads": 2}},
 "disks": [{"pool": "tank", "size": "8G", "thin": false, "bus": "nvme"},
           {"pool": "tank", "size": "2G", "thin": false, "bus": "ahci-hd"},
           {"pool": "tank", "size": "2G", "thin": true,  "bus": "virtio-blk"}],
 "nics": [{"bridge": "bridge0", "model": "e1000"}, {"bridge": "bridge0", "model": "virtio-net"}]}
```

```sh
$SK vm create -f vm.json --wait
# attach the install image (copied to /var/db/skep/media) in the web interface,
# or PUT /api/v1/vms/{id} {"media": ["FreeBSD-15.1-RELEASE-amd64-disc1.iso"]}
$SK vm start bm1 --wait
zfs get -H -o name,property,value refreservation,volmode -r tank/skep/vm
```

Install FreeBSD into the guest from the web console onto `nda0`, reboot the guest,
then in the guest:

```sh
sysctl hw.ncpu kern.smp.cores kern.smp.threads_per_core
camcontrol devlist; nvmecontrol devlist; ifconfig em0; ifconfig vtnet0
dd if=/dev/zero of=/root/fill bs=1m count=1024 && sync    # on the nvme disk
```

Pass: the non-thin zvols have `refreservation` equal to their size; the guest sees 4 CPUs
as 2 cores × 2 threads (pending: "SMT topology in the guest"); `nda0` (nvme) and `ada0`
(ahci-hd) work beyond boot: install, reboot, 1 GiB written and read back (pending:
"`nvme` and `ahci-hd` beyond a smoke boot"); `em0` (`e1000`) and `vtnet0` both get a
DHCP address on VLAN 60 and pass traffic (pending: "an `e1000` NIC"). Stop, force-stop
and restart all work.

## 6. Insufficient space with thick disks

Create a VM whose non-thin disks exceed `zfs get available tank` by 1 GiB.

Pass: refused with `insufficient_space`, `details` naming `pool`, `available_bytes`,
`required_bytes`; nothing created (`zfs list -r tank/skep/vm` unchanged).

## 7. zvol device-node miss rate on real disks

The probe from [the zvol results](../setup/results/zvol-device-node-20261122.md) Part 2a
(`probe.sh`, 50 creates one per second under `tank/zvolprobe`), then 20 `vm create` +
`vm start` cycles through the API.

Pass: the miss rate is recorded (virtual disks: 45/50, 43/50); every start succeeds,
with "zvol device node repaired" in the log for each miss; `tank/zvolprobe` destroyed
afterwards.

## 8. A degraded pool

```sh
zpool offline tank /dev/<disk2>
# the pool as Skep reports it: Storage page, or GET /api/v1/storage/pools
zpool online tank /dev/<disk2>; zpool status tank   # wait for the resilver
```

Pass: while offline the pool reports `DEGRADED` (pending: "a degraded or faulted pool")
and its fragmentation parses, including `-` if shown; health shows the storage check
degraded; after `online` and the resilver, `ONLINE` again and health `ok`.

## 9. The password prompt on the system console

On the machine's own console (keyboard and screen, or IPMI serial), as root:

```sh
skep-api admin create <name2>
```

Pass: the password prompt does not echo, a mismatch is refused, the user exists after
(pending: "password prompt on the system console"). Delete the user afterwards.

## 10. Guest ports on a physical uplink: isolation, VLANs, throughput

Two jails and the VM of step 5 on `bridge0` (DHCP, VLAN 60), and the second machine on
the same switch.

```sh
# in jail 1: change its MAC, then its IPv4 address; send a VLAN-tagged frame
# (the spoofing checks of tests/integration/jails, step 10)
# throughput, guest to the second machine, 30 s, 3 runs each:
iperf3 -c <second-machine> -t 30                       # with the filter (normal)
```

For the comparison without the filter, a jail outside Skep on an epair on the same
bridge (ADR 0011 probe part 2 section 11 method).

Pass: spoofed MAC, spoofed IPv4 and tagged frames do not reach the second machine
(`tcpdump` there); the guests' own traffic does. Throughput with the filter is recorded
next to the unfiltered number (VM measurement: about 12 % less); ADR 0011 decision 9 is
revisited if the loss is much larger.

## 11. Reboot with guests

Set one VM and one jail to start at boot (desired running), `shutdown -r now`.

Pass: the host comes back; both services run; the guests come back as the docs say
(or stay stopped as the docs say), with no leftover epairs or taps
(`ifconfig -l`); `skep-setup check $A` clean.

## 12. AMD-V host (machine B)

Repeat steps 1 to 5 on machine B (a smaller pool and VM are fine).

Pass: `skep-setup check` reports virtualization ok ("AMD-V with nested paging", from
`SVM: … NP` in `dmesg.boot`); the bhyve capability is available; step 5's VM installs
and runs (pending: "Bare metal, AMD-V hosts").

## 13. Clean-up

Delete the test VMs, jails, templates and test users through the API; `zpool destroy
tank` only if the machine is going back; `skep-setup remove` per
[skep-setup](../../../docs/installation/skep-setup.md) if Skep is removed.

Pass: `zfs list -r tank/skep` shows only Skep's base datasets (or nothing after
remove); no `skep-` jails or VMs (`jls`, `ls /dev/vmm`).
