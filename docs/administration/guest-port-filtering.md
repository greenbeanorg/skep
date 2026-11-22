# Guest-port anti-spoofing

Every network interface Skep gives a guest is filtered on the host: the
host end of a jail's epair and every VM's tap. A guest can send only with
the MAC address Skep recorded for that NIC, and, on a static jail NIC,
only from its configured IPv4 address. This closes SR-02 (ADR 0010
amendment y). The design is [ADR 0011](../decisions/0011-guest-port-anti-spoofing.md).

Status: built in Milestone 7c. The behaviour on FreeBSD is validated on
`hearst` as recorded in
`tests/integration/jails/results/m7c-validation.md`. Anything not listed
there is pending FreeBSD validation.

## What is enforced

Each port gets a netgraph `ng_bpf` filter between its `ng_ether` node's
`lower` and `upper` hooks. Frames the guest sends are checked before they
reach the bridge, on the port they came in on. The filter is **default
deny**: only what is listed below passes.

| | Static jail NIC | DHCP jail NIC | VM NIC |
|---|---|---|---|
| Ethernet source = the recorded MAC | enforced | enforced | enforced |
| IPv4 source = the configured address | enforced | not enforced | not enforced (Skep records no VM address) |
| ARP sender MAC = the recorded MAC | enforced | enforced | enforced |
| ARP sender IP = the configured address (or 0.0.0.0, the RFC 5227 probe) | enforced | not enforced | not enforced |
| DHCP from 0.0.0.0 | dropped (a static NIC needs none) | passes | passes |
| UDP from source port 67 (acting as a DHCP server) | dropped | dropped | dropped |
| IPv6 (0x86dd) | dropped | dropped | passes, MAC-only, router advertisements dropped |
| STP/RSTP, LLDP, LACP, PAUSE, 802.1X, any other ethertype, Q-in-Q | dropped | dropped | dropped |

The API has no IPv6 setting for jail NICs yet, so every jail NIC has IPv6
off and its port drops IPv6. The filter for a jail NIC with IPv6 (allowed
MAC-only, with the router-advertisement guard, like a VM) is built and
unit-tested, and is used once the API can set it.

A single 802.1Q tag is outside the filter's reach (the kernel moves it
out of the frame before netgraph sees it). The bridge's own VLAN
filtering covers it; see [Bridge VLAN filtering](#bridge-vlan-filtering).

### Order at start

A port is filtered **before** it joins its bridge, so there is no
unfiltered moment:

1. the epair or tap is created;
2. the filter is installed (`ngctl -f -`, one batch) and read back;
3. only then is the port added to the bridge (`addm`).

If step 2 fails, the guest does not start: `port_filter_failed`. For a
jail the start task fails with that code. For a VM the run ends `failed`
with the reason (`port_filter_failed: …`), as a tap failure does.

## Preparing the host

### The netgraph modules

Skep loads no kernel modules. Add the netgraph modules to `kld_list` in
`/etc/rc.conf`, with the modules jails and VMs already need:

```
kld_list="if_epair netgraph ng_socket ng_ether ng_bpf"
```

(`vmm` and `nmdm` stay in `/boot/loader.conf` as described in the
[VM guide](virtual-machines.md#preparing-the-host).) To load them now
without a reboot: `kldload netgraph ng_socket ng_ether ng_bpf`.

Without them, every start of a jail or VM with a NIC is refused with
`port_filter_unavailable`, before a task is created. Guests without NICs
are not affected.

What loading them changes:

- `ng_ether` attaches a netgraph node to every Ethernet interface and
  bridge. With nothing connected to it, it changes nothing.
- `ng_socket` cannot be unloaded, and `ng_ether` cannot be unloaded while
  Ethernet interfaces exist. Only a reboot removes them.
- **Every VNET jail's root can then use netgraph sockets** (FreeBSD grants
  `PRIV_NETGRAPH_CONTROL` to the root of any VNET jail; no jail parameter
  controls it). Inside a jail that lets root send raw frames out of the
  jail's own interface, for example with `nghook skep0: lower`. On a Skep
  port the filter drops them like any other forged frame. **A VNET jail
  that Skep does not manage gets the same ability, and has no filter**:
  on a shared bridge it can forge MACs and poison ARP caches. Don't run
  unmanaged VNET jails on bridges that carry Skep guests, or filter them
  yourself. A jail root cannot see or change host netgraph nodes, and
  cannot load new node types.

### Bridge VLAN filtering

Bridges are the operator's; Skep creates none. On FreeBSD 15 an
`if_bridge` can filter VLANs. Each member gets a PVID (its untagged
VLAN), and the bridge drops tagged frames a member is not allowed to
send. That stops a guest from sending frames into a VLAN of its choosing,
which the port filter cannot see.

**Set the uplink's PVID first.** With `vlanfilter` on, a member without a
PVID loses all its untagged traffic. Switching it on before the uplink has
a PVID cuts every guest on the bridge off the network.

```
# /etc/rc.conf, a guest bridge with an untagged uplink vtnet1 on VLAN 1
cloned_interfaces="bridge0"
ifconfig_vtnet1="up"
ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"
```

- `ifuntagged vtnet1 1`: the uplink's PVID. A trunk uplink also gets
  `iftagged vtnet1 <vids>`.
- `defuntagged 1`: every new member, including the ports Skep adds, gets
  PVID 1.
- `vlanfilter`: turn filtering on, last.

The same at runtime, in this order: `ifconfig bridge0 ifuntagged vtnet1 1
defuntagged 1`, then `ifconfig bridge0 vlanfilter`. Existing guest ports
need `ifuntagged <port> 1` once, or a guest restart.

Skep does not refuse a guest on a bridge without VLAN filtering. It
reports it as the health check `bridge_vlan_unfiltered`, which degrades
the agent's health but stops nothing. The installer (Milestone 10) will
set `vlanfilter` by default. Refusing starts on unfiltered bridges is to
be revisited for 1.0.

## Health checks, events, and audit

The agent checks every running guest's ports at start (before it serves
its socket) and every **10 seconds** after that:

- the port's netgraph node is an ether node with exactly `lower` and
  `upper` connected, both to the same `ng_bpf` node, on its `jail` and
  `bridge` hooks;
- the program on `jail` is the one generated for that NIC, instruction by
  instruction, and `bridge` is the pass-all.

Only ports whose install has completed are checked, which means ports
that are members of their bridge. Each check takes the port's lock without
waiting and skips a port whose start is still running.

When a check fails:

1. the event `port_filter_missing` is logged at ERROR, with an audit entry
   `port_filter.missing` (actor `system`);
2. the filter is **reinstalled** on the port: hooks to an old `ng_bpf`
   node are removed, then the filter is installed and read back, all by
   netgraph node ID. Success logs `port_filter_restored`, with an audit
   entry `port_filter.restored`;
3. if the reinstall fails, the port is **removed from its bridge**
   (`deletem`): the guest is cut off rather than unfiltered. This logs
   `port_filter_detached`, with an audit entry `port_filter.detached`
   whose details carry `port_filter_failed`;
4. if the same port fails **three checks in a row**, because the filter
   keeps disappearing even though each reinstall works, it is detached
   the same way, and no further reinstall is tried **until the guest is
   restarted**.

A hook connected to the port by someone else (not an `ng_bpf` node) is
never removed. The reinstall then fails, and the port is detached.

Health checks (in `GET /api/v1/health` with a session, and in the
`health` field of `GET /api/v1/system`):

| Check | Fails while |
|---|---|
| `port_filters` | a running guest's port has no complete filter, was just repaired, or is detached; or the netgraph modules are not loaded |
| `bridge_vlan_unfiltered` | a bridge with a running guest port has no `VLANFILTER`, or a guest port on it has no PVID |

Both checks are non-critical: a failure makes the agent's health
`degraded` (HTTP 200), not `unavailable`. The agent can still serve
requests. Alert on the `status` field, as described in
[Monitoring health](../installation/freebsd-service.md#monitoring-health).

A detached port stays detached until its guest is restarted. Restarting
the guest creates a new port with a new filter. To find out why the
filter kept disappearing, check the agent log and the host for anything
that changes netgraph (`ngctl list`, `ngctl show <port>:`). Only host root
can.

## On the host

```sh
ngctl list                    # skf_<port> nodes, one per filtered port
ngctl show epair3a:           # lower and upper connected to skf_epair3a
ngctl msg skf_epair3a: getstats '"jail"'   # frames seen and passed
ifconfig bridge0              # bridge flags=1<VLANFILTER>, untagged <pvid> per member
```

The `skf_<port>` name is only for reading `ngctl list`. Skep identifies
nodes by ID.

## Troubleshooting

| Symptom | Check |
|---|---|
| Start refused with `port_filter_unavailable` | `kldstat -m ng_bpf` (and `netgraph`, `ng_socket`, `ng_ether`); add them to `kld_list` |
| Start fails with `port_filter_failed` | the agent log names the step (`inspect`, `install`, `verify`); something else connected to the port's netgraph hooks? |
| A static jail lost its network after its root changed `rc.conf` | the filter enforces the address Skep recorded; change the NIC through the API, or use the recorded address |
| A guest has no network and `port_filters` says *detached* | the filter could not be kept; read the audit log (`port_filter.*`), fix the cause, restart the guest |
| A guest's DHCP server or IPv6 router does not work | both are dropped from guests by design |
| `bridge_vlan_unfiltered` | see [Bridge VLAN filtering](#bridge-vlan-filtering) |

## Known limitations

- **MAC-only ports can still claim another IPv4 address.** A DHCP jail or
  a VM can answer ARP for, and send from, an address it does not hold,
  as long as it uses its own recorded MAC. That traffic can be traced to
  the guest (its MAC is in every neighbour table), but it is not
  prevented. An optional pinned address for VM NICs is in the backlog.
- **IPv6 is MAC-only where it is allowed.** The IPv6 source address and
  neighbour discovery are not pinned (a later ADR).
- **The router-advertisement guard is best-effort.** It drops ICMPv6
  type 134 directly after the IPv6 header. An RA behind an extension
  header (hop-by-hop, destination options, fragment) passes.
- **The DHCP-server guard covers IPv4 only.** It looks at the first
  fragment of a datagram. Later fragments pass, and without their first
  fragment they never reassemble.
- **Detection is polling.** Someone with host root who removes a filter
  opens the port for up to 10 seconds before the check notices. netgraph
  sends no notification.
- **VLAN tags** are stopped only by the bridge's `vlanfilter`, which is
  the operator's configuration.
- **Throughput:** about 12 % less single-stream TCP through an epair,
  measured in a small VM (ADR 0011, probe part 2 section 11). To be
  measured again on real hardware in Milestone 11.
- **Unmanaged VNET jails** get netgraph sockets when the modules are
  loaded, and have no filter (see above).
