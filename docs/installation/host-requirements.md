# Host requirements and the manual install

This page is the complete, ordered procedure for turning a stock
FreeBSD® 15.1-RELEASE (amd64) install into a Skep host, by hand. Until the
installer exists (Milestone 10, [ADR 0012](../decisions/0012-installer.md),
accepted 2026-11-22, not yet built), this is how Skep is installed. It also serves as the inventory
that ADR is based on: every step below is something an installer would
automate, check, or leave to the operator.

It collects what is otherwise spread across
[Running Skep as FreeBSD services](freebsd-service.md),
[Virtual machines](../administration/virtual-machines.md#preparing-the-host),
[Jails](../administration/jails.md),
[Jail templates](../administration/jail-templates.md),
[Guest-port anti-spoofing](../administration/guest-port-filtering.md), and
the ADRs. Those pages stay the authority for what each setting does; this
one is the order to do it in.

## Where this comes from, and what is validated

Every step was done on the lab host `hearst` (FreeBSD 15.1-RELEASE-p7,
amd64, UFS root, a pkgbase install, 4 vCPUs and 6 GiB RAM as a Proxmox VM
with CPU type `host`) between Milestone 1 and Milestone 7c. The state of
`hearst` on 2026-11-22 was checked read-only for this page.

| Area | Status |
|---|---|
| UFS-root host, file-backed ZFS pool, pkgbase install | Validated on `hearst` |
| ZFS-root host | **Pending FreeBSD validation**; nothing in Skep depends on the root file system type, but no ZFS-root host has run it |
| A host installed from distribution sets (not pkgbase) | **Pending FreeBSD validation**: affects only the pkgbase template source (step 11) |
| Bare metal, AMD-V hosts | **Pending validation**; only nested Intel VT-x with EPT and unrestricted guest has run bhyve under Skep |
| Persistent `kld_list` and bridge `vlanfilter` across a reboot | Validated on `hearst` 2026-11-22 ([results](../../tests/integration/jails/results/validation-2026-11-22.md)) |

## What Skep never does to the host by itself

So that the order makes sense: Skep's agent changes nothing in the host's
boot, kernel, or network configuration, ever (ADR 0003, ADR 0009
decision 6b, ADR 0011 decision 6). It loads no kernel modules, creates no
bridges, edits no `rc.conf` or `loader.conf`, creates no pools, and adds
no users. Each of those is a step for the administrator below. What the
agent does create at run time is listed in
[What Skep creates by itself](#what-skep-creates-by-itself).

## 0. Before you start

- FreeBSD 15.1-RELEASE, amd64, freshly installed with `bsdinstall`. Root
  on UFS or ZFS. The host's userland and kernel at the same patch level.
- A CPU with hardware virtualization: Intel VT-x with EPT and
  unrestricted guest (check `dmesg | grep -A2 VT-x` for `EPT` and `UG`),
  or AMD-V with RVI (not yet validated). Without it bhyve is unavailable;
  jails still work.
- Root on the host (a console or SSH plus `su`/`sudo`).
- **Two network interfaces are strongly recommended**: one for
  management (the host's own address, SSH, the web interface), one as the
  uplink of the guest bridge, with no address. Skep refuses to put a guest
  on a bridge that has a management interface as a member. In the
  examples, `vtnet0` is management and `vtnet1` is the guest uplink.
- The management interface should have a static address. One configured
  by DHCP works, but the API's listen address (step 12) is the current
  lease's and stops working if the lease changes; use a DHCP reservation
  if you keep DHCP. (`skep-setup` will warn about this, not refuse.)
- If the host is itself a virtual machine (as `hearst` is), the outer
  hypervisor must expose VT-x (Proxmox CPU type `host`) and must let the
  guest uplink send frames from MAC addresses it did not assign (on
  Proxmox, the VM firewall's MAC filter off for that NIC).

Throughout, a line marked **(now)** applies the setting to the running
system without a reboot; the file edit makes it permanent.

## 1. Bring the host up to date

```sh
# A pkgbase install:
pkg upgrade
# A distribution-set install:
freebsd-update fetch install
```

Reboot if the kernel changed. `freebsd-version -ku` should show the same
release for both. Jail templates must match the kernel's major.minor
(`release_unsupported` otherwise); a different patch level only marks a
template `patch_differs`.

## 2. Install the packages Skep needs

```sh
pkg install edk2-bhyve      # UEFI firmware for VMs: /usr/local/share/edk2-bhyve
```

That is the only required package. `pkg` itself is bootstrapped by the
first `pkg` command if it is not already there; the jail template builder
needs `/usr/local/sbin/pkg` (step 11).

Optional: `sqlite3` (to inspect or `VACUUM` the databases), `tmux`.
Skep's binaries are static Go programs (CGO off) and need no runtime
libraries; the web interface is embedded in `skep-api`.

## 3. Boot-time kernel settings: `/boot/loader.conf`

```
# bhyve and its serial consoles (required for VMs)
vmm_load="YES"
nmdm_load="YES"

# RACCT: Skep's jail limits use RCTL, which needs it (every limit
# other than disk_quota). Recommended; skep-setup will add it by default.
kern.racct.enable="1"

# Optional, small hosts with ZFS: cap the ARC so guests fit (1 GiB here)
vfs.zfs.arc.max="1073741824"

# Only when this FreeBSD host is itself a VM under KVM (nested bhyve):
hw.vmm.vmx.cap.virtual_interrupt_delivery="0"
```

**(now)** `kldload vmm nmdm`. RACCT cannot be turned on without a
reboot. The nested-virtualization tunable is explained in
[Virtual machines](../administration/virtual-machines.md); it is not
needed on bare metal. `hearst` has all five lines.

## 4. Modules loaded after boot: `kld_list` in `/etc/rc.conf`

```sh
sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"
```

- `if_epair` gives jails their interfaces. Without it a jail with a NIC is
  refused with `epair_unavailable`.
- `netgraph ng_socket ng_ether ng_bpf` filter every guest port
  (anti-spoofing, ADR 0011). Without them a jail or VM with a NIC is
  refused with `port_filter_unavailable`, and the health check
  `port_filters` reports running guests unfiltered.

**(now)** `kldload if_epair netgraph ng_socket ng_ether ng_bpf`.
`if_bridge` is loaded by `ifconfig` when the bridge is created.

## 5. ZFS, or a directory pool

ZFS (recommended; required for snapshots, clones, and jails):

```sh
sysrc zfs_enable=YES
```

On a ZFS-root host the pool exists already (`zroot`). On a UFS-root host,
create a pool on a spare disk (`zpool create tank /dev/da1`), or for a
test host a file-backed pool, as `hearst` has (`skeptest`, backed by
`/var/tmp/skep/skeptest.img`). Skep never creates, imports, or destroys
pools; it works only under `<pool>/<dataset_root>` (default
`<pool>/skep`), which it creates on first use. Set encryption,
compression, or a quota on `<pool>/skep` yourself first if you want them:
Skep's datasets inherit them.

A host with no ZFS can run VMs from a directory pool instead (no
snapshots, no clones, no jails):

```sh
mkdir -p /var/db/skep-disks
chown root:wheel /var/db/skep-disks
chmod 0700 /var/db/skep-disks
```

## 6. The guest bridge, with VLAN filtering

Skep creates no bridges. Create one for guests, with the uplink's VLAN ID
set **before** `vlanfilter` is switched on (otherwise every guest on the
bridge loses its untagged traffic):

```sh
sysrc cloned_interfaces+="bridge0"
sysrc ifconfig_vtnet1="up"
sysrc ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"
```

- `ifuntagged vtnet1 1`: the uplink's PVID. A trunk uplink also gets
  `iftagged vtnet1 <vids>`.
- `defuntagged 1`: every member Skep adds later gets PVID 1.
- `vlanfilter`: last.

**(now)**, in this order: `ifconfig bridge0 create`, `ifconfig vtnet1 up`,
`ifconfig bridge0 addm vtnet1 ifuntagged vtnet1 1 defuntagged 1`,
`ifconfig bridge0 vlanfilter up`. The bridge needs no address. Do this
from the console, or over the management interface only: a mistake on
the bridge must not be able to cut your own session.

A bridge without `vlanfilter` still works; the agent reports it as the
non-critical health check `bridge_vlan_unfiltered`.

## 7. The service account

```sh
id 409; getent group 409       # both must find nothing
pw groupadd skep -g 409
pw useradd skep -u 409 -g skep -d /nonexistent -s /usr/sbin/nologin -c "Skep service account"
```

`skep-api` runs as this account; the rc.d scripts refuse to start without
it and never create it. UID and GID **409** are fixed by
[ADR 0012](../decisions/0012-installer.md) Decision 1, so that every node
of a future cluster has the same owner on files that move between nodes;
the package will create the account with these numbers. If 409 is taken
on your host, stop and report it. Hosts set up before that decision have
whatever `pw` assigned (`hearst`: 1002); that works on a single host.

## 8. The programs and the rc.d scripts

Build on a development machine (`make build-freebsd`: the web interface
plus the three binaries in `bin/freebsd-amd64/`) and copy them over, then:

```sh
install -o root -g wheel -m 0555 skep-agent skep-api /usr/local/sbin/
install -o root -g wheel -m 0555 skepctl /usr/local/bin/
install -o root -g wheel -m 0555 packaging/rc.d/skep_agent packaging/rc.d/skep_api /usr/local/etc/rc.d/
skep-agent -version; skep-api -version
```

Check the version and commit match the build. There is no signed package
yet: compare SHA-256 sums with the build machine. (The package will also
install `LICENSE` and `THIRD_PARTY_NOTICES` in
`/usr/local/share/licenses/skep/`.) The agent runs as root,
so nothing it runs may be writable by anyone else, including every
directory above it.

## 9. Configuration files

```sh
mkdir -p /usr/local/etc/skep
chown root:wheel /usr/local/etc/skep && chmod 0755 /usr/local/etc/skep
```

`/usr/local/etc/skep/agent.json`, `root:wheel` `0600` (only the root
agent reads it; the API can never change it):

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
    "directories": [],
    "dataset_root": "skep",
    "media_dir": "/var/db/skep/media"
  },
  "network": {
    "bridges": ["bridge0"],
    "management_interfaces": ["vtnet0"]
  }
}
```

- `pools`, `directories`, `bridges`: the allow-lists. Empty means no VM
  can be created, or none can have a NIC.
- `management_interfaces`: every interface that carries the host's own
  management traffic. Skep refuses a guest on a bridge that contains one.
- Every other key has a default; the full list with defaults is
  `packaging/etc/agent.json.sample`.

`/usr/local/etc/skep/api.json` is optional, `root:skep` `0640` if
present. Its main use is the listen address (step 12).

## 10. Enable and start the services

```sh
sysrc skep_agent_enable=YES skep_api_enable=YES
service skep_agent start
service skep_api start
service skep_agent status    # as root: unprivileged status is unreliable
service skep_api status
```

The scripts create `/var/run/skep` and `/var/db/skep/{agent,api}` with
the right owners at every start (table below). Logs go to syslog, tags
`skep-agent` and `skep-api`, in `/var/log/daemon.log` with the stock
`syslog.conf`.

## 11. Jail templates: the package source

Jails are built from templates, and a template's userland comes from one
of two sources (`GET /api/v1/storage/template-sources` says which this
host can use):

- **pkgbase** (default): `pkg --rootdir` with the **host's own
  repository configuration**. The `FreeBSD-base` repository is disabled
  in the stock `/etc/pkg/FreeBSD.conf`. A pkgbase-installed host
  (`hearst`) has it enabled already, in
  `/usr/local/etc/pkg/repos/FreeBSD.conf`:

  ```
  FreeBSD-base: { enabled: yes }
  ```

  On a host installed from distribution sets, enabling it only adds a
  repository; it does not convert the host to pkgbase, and `pkg upgrade`
  touches only installed packages. This case is **pending validation**.
- **dist**: `base.txz` and its `MANIFEST` from the release, in a
  root-owned directory named by `storage.dist_dir`. Pending FreeBSD
  validation through the deployed agent.

## 12. TLS, the network address, and the first administrator

At its first start `skep-api` creates a key and a self-signed certificate
in `/var/db/skep/api/tls`. It listens on `127.0.0.1:8443` until told
otherwise. Create the first administrator **before** opening it to the
network:

```sh
skep-api admin create --username NAME     # as root; prompts twice, no echo
skep-api tls fingerprint                  # write this down
```

There are no default credentials, and login answers `503 setup_required`
until an administrator exists. Then set the management address, either
in `api.json`:

```json
{ "listen_address": "10.79.20.170:8443" }
```

or as `hearst` does, with `sysrc skep_api_args="-listen 10.79.20.170:8443"`,
and `service skep_api restart`. Never use `0.0.0.0` on a host whose other
interfaces face guest or storage networks.

Compare the browser's or `skepctl`'s view of the key with
`skep-api tls fingerprint` on first connection
([web interface](../administration/web-ui.md),
[skepctl](../administration/skepctl.md)).

## 13. Install media and a first check

- Create `media_dir` (Skep only reads it, and never creates it):
  `install -d -o root -g wheel -m 0700 /var/db/skep/media`. Put ISO files
  in it, each verified against the publisher's checksum.
- `curl --cacert ... https://<address>:8443/api/v1/health` answers
  `{"status":"ok"}`; with a session, `GET /api/v1/system` shows
  `bhyve`, `zfs`, and the guest network checks. `port_filters` and
  `bridge_vlan_unfiltered` should both be clean.

## Directory layout and ownership

Created by the administrator:

| Path | Owner | Mode | What |
|---|---|---|---|
| `/usr/local/sbin/skep-agent`, `skep-api` | `root:wheel` | `0555` | Programs |
| `/usr/local/bin/skepctl` | `root:wheel` | `0555` | CLI |
| `/usr/local/etc/rc.d/skep_agent`, `skep_api` | `root:wheel` | `0555` | rc.d scripts |
| `/usr/local/etc/skep` | `root:wheel` | `0755` | Configuration directory |
| `/usr/local/etc/skep/agent.json` | `root:wheel` | `0600` | Agent configuration |
| `/usr/local/etc/skep/api.json` | `root:skep` | `0640` | API configuration (optional) |
| `/var/db/skep/media` | `root:wheel` | `0700` | Install media (`media_dir`); Skep only reads it |
| `<directory pool>` | `root:wheel` | `0700` | Only if used |

Created by the rc.d scripts at every start:

| Path | Owner | Mode | What |
|---|---|---|---|
| `/var/run/skep` | `root:skep` | `0750` | Socket and pid files |
| `/var/db/skep` | `root:wheel` | `0755` | Data root |
| `/var/db/skep/agent` | `root:wheel` | `0700` | `agent.db`, the audit archive |
| `/var/db/skep/api` | `skep:skep` | `0700` | `api.db`, `tls/`, the audit archive |

## What Skep creates by itself

At run time, the agent creates and removes only its own objects:

- datasets and zvols under `<pool>/<dataset_root>` (`vm/`, `jail/`,
  `templates/`), and VM disk files under `<directory pool>/skep/vm/`;
- `epair` and `tap` interfaces for running guests, and their netgraph
  `ng_bpf` nodes, added to allow-listed bridges only;
- devfs ruleset 6001 in the kernel (not in `/etc/devfs.rules`) when a jail
  with DHCP starts; it refuses to start if 6001 holds something else;
- RCTL rules on `jail:skep-<id>` while a jail runs;
- `/dev/nmdm<N>A/B` consoles and `bhyve` processes for running VMs.

It needs no change to the host's `/etc/devfs.rules`, `/etc/login.conf`,
`/etc/sysctl.conf`, or `pf`. A jail's own `/etc/login.conf` decides how
much memory one of its services may lock
([Jails](../administration/jails.md#limits)); that is the jail
administrator's, not the host's.

## hearst today

For comparison, the lab host as of 2026-11-22, after the validation run
that made `kld_list` and the bridge line persistent and rebooted:

| Setting | `hearst` | This procedure |
|---|---|---|
| `loader.conf` | all five lines of step 3 | same |
| `kld_list` | `if_epair netgraph ng_socket ng_ether ng_bpf` | step 4 |
| `bridge0` | `addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up` | step 6 |
| Listen address | `skep_api_args="-listen 10.79.20.170:8443"` | step 12, either form |
| Pool | `skeptest`, file-backed, plus a directory pool `dirpool` | step 5 |
| `FreeBSD-base` | enabled (pkgbase install) | step 11 |
| `skep` account | UID/GID 1002 (set up before ADR 0012 fixed 409) | step 7 |

## Remove

See [Running Skep as FreeBSD services](freebsd-service.md#remove). Stop and
delete every guest first; removing `/var/db/skep` deletes the node
identity and every audit log. Skep's datasets under `<pool>/skep` are not
removed by that; destroy them by hand once nothing uses them. Revert
steps 3, 4, and 6 only if nothing else on the host needs them.
