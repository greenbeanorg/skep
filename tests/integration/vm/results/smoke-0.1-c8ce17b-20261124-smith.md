# VM smoke test on the installed 0.1 build, smith, 2026-11-24

| | |
|---|---|
| Date | 2026-11-24, 16:08–16:18 UTC |
| Host | `smith` (10.79.20.81), FreeBSD 15.1-RELEASE-p7 amd64, UFS root, pool `tank` on `da1` |
| Nesting | **Nested under KVM**: smith is a VM on the Proxmox host `swearengen` (`kern.vm_guest: kvm`, `hw.hv_vendor: KVMKVMKVM`, Intel Core i5-10600K, 2 vCPUs, 4 GiB). Not bare metal |
| Build | the installed package `skep-0.1.0` built from `c8ce17b`; `skep-agent`, `skep-api`, `skepctl`, `skep-setup` all print `0.1.0 (commit c8ce17b, built 2026-11-24T10:57:56Z, go1.27.4 freebsd/amd64)`. main's commits after `c8ce17b` change Markdown only, so this is the 0.1 code |
| vmm | `hw.vmm.vmx.cap.virtual_interrupt_delivery: 0` and `posted_interrupts: 0` at the start and at the end (the nested-KVM workaround in [virtual-machines.md](../../../../docs/administration/virtual-machines.md)) |
| Firmware | `edk2-bhyve-g202508_3` |
| Client | `skepctl` and `consolectl` built on merrick from main (`682076b`, no Go change since `c8ce17b`); the server's key pinned (`sha256:751f8015…a3e568`, matching `skep-api tls fingerprint` on smith); `curl --cacert --pinnedpubkey` for the media calls |
| Logs (merrick) | `~/skep-test-logs/20261124T160858Z-job1b-create.log`, `20261124T161023Z-job1c-iso.log`, `20261124T161729Z-job1d-cleanup.log` |

This is the first VM started on any build after `ce1d910`. It checks
start, stop, force-stop, boot from install media, and delete. It does not
cover a full install, networking (no bridge is allowed on smith, so the VM
had no NIC), snapshots, clones, or anything on bare metal.

## Results

| Step | Result |
|---|---|
| a. State check | **pass** |
| b. Create, start, runner, `/dev/vmm`, console, health | **pass** |
| b. Stop, start again, force-stop | **pass** |
| c. ISO download and SHA256 check | **pass** |
| c. Guest boot from the ISO to the installer, seen with `consolectl -wait-for` | **pass** (26 s after start) |
| d. Clean up | **pass** |

## a. State check (before 16:08 UTC)

```
ssh 10.79.20.81 'freebsd-version -ku; sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery hw.vmm.vmx.cap.posted_interrupts;
  skep-agent -version; skep-api -version; skepctl -version; skep-setup -version;
  sudo service skep_agent status; sudo service skep_api status; ls /dev/vmm; pgrep -lf "bhyve|runner";
  sysctl -n vm.stats.vm.v_free_count hw.pagesize; zpool list tank; zfs list -r tank; ifconfig -g tap;
  sudo cat /usr/local/etc/skep/agent.json; kldstat | grep -E "vmm|nmdm"; pkg info -x edk2'
curl --cacert smith-cert.pem https://10.79.20.81:8443/api/v1/health
skepctl login --server 10.79.20.81:8443 --username admin --password-stdin < (password file)
skepctl system --json ; skepctl vm list
```

- 15.1-RELEASE-p7; both caps 0; all four binaries at `c8ce17b`.
- `skep_agent is running as pid 54252`, `skep_api is running as pid 54339`.
- `/dev/vmm: No such file or directory`; no bhyve or runner process; no tap; `vm list` empty.
- Free memory about 3.1 GiB (822948 pages of 4 KiB); `tank` 15.5 G, 1.96 M allocated, 15.0 G available.
- `agent.json`: pools `["tank"]`, media_dir `/var/db/skep/media`, bridges `[]`. So the VM has no NIC.
- `vmm.ko` and `nmdm.ko` loaded; system document: bhyve `available` (vmm loaded, firmware installed, nmdm available), `max_vcpus_per_vm` 2, health `ok` with every check `ok`.

## b. Bhyve-level start, stop, and force-stop

Definition (`smoke.json`):

```json
{ "name": "smoke01", "cpu_count": 1, "memory": "1G",
  "disks": [ { "size": "2G", "pool": "tank" } ] }
```

| UTC | Command | Task | Status |
|---|---|---|---|
| 16:08:58 | `skepctl vm create -f smoke.json --wait --json` | `vm.create` 10d7a6ee | succeeded (zvol `tank/skep/vm/02eeeed3-…/disk0`, 2 GiB, `virtio-blk`, full reservation; no NIC) |
| 16:09:09 | `skepctl vm start smoke01 --wait --json` | `vm.start` bee8bb60 | succeeded |
| 16:10:08 | `skepctl vm stop smoke01 --wait --json` | `vm.stop` 90134182 | succeeded (0.1 s: the firmware, at its boot-failure prompt, powered off on the ACPI button) |
| 16:10:09 | `skepctl vm start smoke01 --wait --json` | `vm.start` 5a82e314 | succeeded |
| 16:10:15 | `skepctl vm force-stop smoke01 --wait --json` | `vm.force_stop` dd00dbc6 | succeeded |

After the first start (`ssh 10.79.20.81 'ls -l /dev/vmm/; ps -axo pid,ppid,user,command | grep -E "bhyve|skep-agent"; ls -l /dev/nmdm*; sudo cat /var/run/skep/vm/<id>/state.json'`):

```
crw-------  1 root wheel 0x7f Nov 24 11:09 skep-02eeeed3-8dba-41c1-b4a9-c292aba40992
54252 54250 root /usr/local/sbin/skep-agent -data-dir /var/db/skep
70593 54252 root /usr/local/sbin/skep-agent vm-runner -vm 02eeeed3-8dba-41c1-b4a9-c292aba40992 -run-dir /var/run/skep/vm
70595 70593 root bhyve: skep-02eeeed3-8dba-41c1-b4a9-c292aba40992 (bhyve)
crw--w----  1 root tty 0x83 Nov 24 11:09 /dev/nmdm-skep-02eeeed3-8dba-41c1-b4a9-c292aba40992.1A
crw--w----  1 root tty 0x84 Nov 24 11:09 /dev/nmdm-skep-02eeeed3-8dba-41c1-b4a9-c292aba40992.1B
{"state":"running","since":"2026-11-24T16:09:09.601478894Z","runner_pid":70593,"bhyve_pid":70595,"boots":1}
```

smith answered ssh 1 s after the start (`uptime`: up 18:01). Health `{"status":"ok"}`; `vm list`: running/running.

Serial console, 28 s after the start (`consolectl … -vm <id> -wait-for 'NO-SUCH-TEXT-1b' -timeout 20s`, a pattern chosen not to appear so that the timeout prints the buffered text; exit 3 as documented):

```
BdsDxe: failed to load Boot0001 "UEFI Misc Device" from PciRoot(0x0)/Pci(0x2,0x0): Not Found
BdsDxe: No bootable option or device was found.
BdsDxe: Press any key to enter the Boot Manager Menu.
```

That is firmware output on COM1 (an empty disk, no media). Note: this
edk2 build does write its boot-failure messages to the serial console;
[virtual-machines.md](../../../../docs/administration/virtual-machines.md)
and [step6-checks.txt](step6-checks.txt) (hearst, 2026-11-14, a guest that
booted) say the firmware prints nothing there.

After the stop and after the force-stop: no `/dev/vmm`, no bhyve or
runner process, no tap, health ok, `vm list` stopped/stopped. After the
second start: `/dev/vmm/skep-<id>`, runner 70686, bhyve 70688, health ok,
smith answering.

## c. Guest boot from install media

ISO, on merrick, in an empty directory:

```
B=https://download.freebsd.org/releases/amd64/amd64/ISO-IMAGES/15.1
curl -fsS -o CHECKSUM.SHA256 $B/CHECKSUM.SHA256-FreeBSD-15.1-RELEASE-amd64
curl -fsS -o FreeBSD-15.1-RELEASE-amd64-bootonly.iso $B/FreeBSD-15.1-RELEASE-amd64-bootonly.iso
sha256sum FreeBSD-15.1-RELEASE-amd64-bootonly.iso
```

Published: `SHA256 (FreeBSD-15.1-RELEASE-amd64-bootonly.iso) = 3e74120a59512cefc35840443bdd05087c8a010a27cf8a6fbb4f7450824f092e`.
Computed: the same; 546666496 bytes. The CHECKSUM file was fetched over
HTTPS from the same server; its PGP-signed copy in the release
announcement was not checked.

Placed as the guide says ([virtual-machines.md](../../../../docs/administration/virtual-machines.md), step 6: in `media_dir`, root:wheel, copied under another name and renamed):

```
scp FreeBSD-15.1-RELEASE-amd64-bootonly.iso 10.79.20.81:/tmp/skep-smoke-<iso>.part
ssh 10.79.20.81 'sha256 -q /tmp/skep-smoke-<iso>.part
  sudo install -o root -g wheel -m 0600 /tmp/skep-smoke-<iso>.part /var/db/skep/media/<iso>.part
  sudo mv /var/db/skep/media/<iso>.part /var/db/skep/media/<iso>; rm /tmp/skep-smoke-<iso>.part
  sudo sha256 /var/db/skep/media/<iso>'
```

Both hashes on smith matched. `GET /api/v1/storage/media` listed the file.

| UTC | Command | Task | Status |
|---|---|---|---|
| 16:16:28 | `curl … -X PUT -d '{"media":["FreeBSD-15.1-RELEASE-amd64-bootonly.iso"]}' https://10.79.20.81:8443/api/v1/vms/<id>` | HTTP 202, `vm.update` 053d6d7b | succeeded; disks: slot 0 `virtio-blk` zvol, slot 1 `ahci-cd` the ISO |
| 16:16:36 | `skepctl vm start smoke01 --wait --json` | `vm.start` 715bef98 | succeeded; runner 71086, bhyve 71088, `/dev/vmm/skep-<id>` |
| 16:16:36 | `consolectl -server https://10.79.20.81:8443 -pin sha256:751f… -token-file (0600 file) -vm <id> -wait-for 'Console type [vt100]' -timeout 15m` | | printed `consolectl: the pattern appeared`, exit 0, at 16:17:02 |

`Console type [vt100]` is printed only by the FreeBSD installer's
terminal-type prompt. This VM's earlier boots printed only the firmware
text above, so the match came from this boot. On a match `consolectl`
prints nothing of the console, so the text was read with a second attach
(`-wait-for 'NO-SUCH-TEXT-1c' -timeout 10s`, exit 3, the last 512 bytes):

```
Clearing /tmp (X related).
Updating motd:ln: /etc/motd: Read-only file system
.
Updating /var/run/os-release done.
Creating and/or trimming log files.
Starting syslogd.
Starting local daemons:
Starting primary installer on ttyu0

Welcome to FreeBSD!

Please choose the appropriate terminal type for your system.
Common console types are:
   ansi     Standard ANSI terminal
   vt100    VT100 or compatible terminal
   xterm    xterm terminal emulator (or compatible)

Console type [vt100]:
```

smith answered ssh after the boot (load 0.72); health ok.

**`consolectl -wait-for`, first live use:** it behaved as documented. A
match exited 0 and printed one line; a timeout exited 3 and printed the
escaped end of the console text; nothing was sent to the guest; the
server's replay of buffered text on attach was what made the second,
read-only attach show the prompt. Not exercised: exit 1 (console closed
during the wait).

## d. Clean up

| UTC | Command | Task | Status |
|---|---|---|---|
| 16:17:29 | `skepctl vm stop smoke01 --timeout 60 --wait --json` | `vm.stop` c1200b25 | succeeded in 6 s (clean ACPI shutdown of the installer system) |
| 16:17:44 | `skepctl vm delete smoke01 --wait --json` | `vm.delete` bfaaa96a | succeeded; destroyed `tank/skep/vm/02eeeed3-…` and its `disk0` |

Then:

```
ssh 10.79.20.81 'sudo rm -v /var/db/skep/media/FreeBSD-15.1-RELEASE-amd64-bootonly.iso; ls /dev/vmm; ls /dev/nmdm*;
  pgrep -lf "bhyve|vm-runner"; ifconfig -g tap; ifconfig -l; zfs list -r -t all tank; sudo ls -la /var/run/skep/vm/;
  sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery hw.vmm.vmx.cap.posted_interrupts; kenv hw.vmm.vmx.cap.virtual_interrupt_delivery;
  sudo service skep_agent status; sudo service skep_api status'
```

- No `/dev/vmm`, no `/dev/nmdm*`, no bhyve or runner, no tap (interfaces `vtnet0 vtnet1 lo0`), `/var/run/skep/vm/` empty.
- `zfs list -r -t all tank`: `tank`, `tank/skep`, `tank/skep/vm` only, as at the start.
- `media_dir` empty; `GET /api/v1/storage/media`: `{"items":[]}`; no `/tmp/skep-smoke-*` left.
- Both caps 0; `kenv` 0. `skep_agent` pid 54252 and `skep_api` pid 54339, unchanged since the start. Health `{"status":"ok"}`; `vm list` empty.
- On merrick: the ISO, the checksum file, and the token files deleted; `skepctl logout`.

Every task of the run (`skepctl tasks list`): `vm.create`, `vm.start` ×3,
`vm.stop` ×2, `vm.force_stop`, `vm.update`, `vm.delete`, all
`succeeded`, actor `admin`.
