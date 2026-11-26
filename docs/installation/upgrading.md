# Upgrading a host

A host installed from the package (Milestone 10) is upgraded with
`pkg upgrade skep` and then [`skep-setup upgrade`](skep-setup.md#upgrades),
which refuses while agent tasks are pending or running, stops `skep_api`
so no new task arrives, waits up to `-wait` (60 s) for the tasks to
finish, backs up both databases (`skep-agent db backup`, `skep-api db
backup`), restarts the services in order and checks them. That path ran
on two lab VMs nested under KVM (`smith` and `langrishe`) on 2026-11-22
([record](../../tests/integration/setup/results/lab-vm-validation-20261122.md),
part 7), not on bare metal. A host installed by hand is upgraded as
below: replace the binaries, prepare the host
for whatever the new release needs, and restart the services in order.
This page has one section per upgrade that needs more than the binaries.

The general steps, as root:

```sh
service skep_api stop
service skep_agent stop
install -o root -g wheel -m 0555 skep-agent skep-api /usr/local/sbin/
install -o root -g wheel -m 0555 skepctl /usr/local/bin/
service skep_agent start       # runs the agent's schema migrations, if any
service skep_api start         # runs the API's
service skep_agent status; service skep_api status
skep-agent -version; skep-api -version
```

Before replacing the binaries, copy `/var/db/skep/agent/agent.db` and
`/var/db/skep/api/api.db` somewhere safe: `skep-agent db backup` and
`skep-api db backup` (from a build that has them) write consistent copies
to `/var/db/skep/backup/` while the services run; otherwise stop the
services, or use `sqlite3 <db> ".backup <file>"`. Migrations only go forward, and an
older binary refuses a newer schema, so that copy is the only way back.

Restart both services: from a build with the version handshake on,
`skep-api` answers `503 agent_version_mismatch` while the agent is
another build.

**Running guests are not stopped by an agent restart.** VMs run under
their own runner processes, which survive it (ADR 0009 decision 7);
jails, their interfaces, and their filters are kernel state. A task that
was pending or running when the agent stopped is marked `interrupted`; it
is never resumed or re-run, and must be resubmitted. `skep-setup upgrade`
refuses rather than do that unless given `-force`; by hand, check
`skepctl tasks list --status pending` and `--status running` (or the
Tasks page) first, and stop `skep_api` before the agent.

## To step 8c-1 or later (main, the unreleased cluster preview): the users move into the cluster

From step 8c-1 the users are `skep-cluster`'s (ADR 0013 amendment 4).
Migration `api/0006` keeps a host's own users and marks them
**pending**: logins work as before, and every user change answers
`503 users_import_pending` until the import. `skep-api` reads the
users from `skep-cluster`'s socket (`cluster_socket` in `api.json`,
default `/var/run/skep-cluster/cluster.sock`), so `skep_cluster` must be
running.

**From step 8c-2, `skep-setup upgrade` does this on a single host**
(ADR 0013 amendment 5, D10–D13): after the package upgrade, run

```sh
pkg upgrade skep
skep-setup upgrade
```

Before it stops anything it refuses (exit 3, nothing changed) on another
build of `skep-cluster`, a missing `skep-cluster` account,
`skep_cluster_enable` set to anything but `YES`, a non-default
`skep_cluster_*` directory, or a running `skep_cluster` that is joining,
rotating its key or removed (the message names the command that ends
that state). Then, in order: it stops `skep_api` and waits for tasks,
backs up both databases (**not** the cluster store: that comes with step
8f), restarts `skep_agent`, runs `skep-cluster init` on a host that has
no cluster yet (a one-node cluster, no network port), adds
`skep_cluster_enable="YES"` where the line is missing (with a journal of
command `upgrade`), restarts `skep_cluster` and waits for its leader,
starts `skep_api`, and, **on a node that is alone in its cluster**, runs
`skep-api admin import-users` (never with `--yes`) and checks that the
import state is `done`. A failure from the init on is exit 4: `skep_api`
is started again, the message says what to check, and running `skep-setup
upgrade` again is safe. Meanwhile, without `skep_cluster`, an enabled
administrator can still log in with a degraded session of at most an
hour; everyone else gets `503 no_quorum`.

**In a cluster of several nodes** `skep-setup upgrade` does not import:
it prints the steps and exits 0. Do it by hand:

1. Upgrade **every** node of a cluster first, one at a time
   (`skep-setup upgrade` on each), and wait for all voters and `health ok`
   after each. A `skep-cluster` of an earlier build stops at the first
   user entry and must not meet one.
2. On one node only: `skep-api admin import-users`. It sends this host's
   users to the cluster in one entry (at most 192 KiB; 2000 users at
   most) and records its operation ID first, so running it again after a
   crash finishes the import. The sessions stay.
3. On every other node: `skep-api admin import-users`. The cluster has
   users already: the command lists this node's users and says the
   cluster's replace them; run it again with `--yes` to accept. That
   node's sessions end, and only the cluster's accounts exist on it.

A host whose import is still pending cannot join a cluster:
`skep-cluster join` refuses it until `skep-api admin import-users` has
run there, and refuses an `api.db` older than the installed `skep-api`
(`unmigrated`: run `skep-setup upgrade` first).

**Going back to 0.1** after such an upgrade: install the 0.1 package
again, stop the services, and restore both database backups from
`/var/db/skep/backup/<old build>-<time>/` as the general steps above
describe (an older binary refuses the newer schema). The 0.1 package has
no `skep_cluster` script, so the `skep_cluster_enable` line is left over
and does nothing (`skep-setup rollback` of the upgrade's journal removes
it where unchanged); the cluster's data stays in `/var/db/skep/cluster`
until `skep-setup remove -purge-data`.

## From b61e578 (Milestone 7b) to Milestone 7c (main at 578b7d8 or later)

b61e578 is the build `hearst` runs as of 2026-11-22. Between it and
current main:

- **Guest-port anti-spoofing** (ADR 0011): every jail epair and VM tap
  gets a netgraph `ng_bpf` filter before it joins its bridge, and the
  agent checks every running guest's ports at start and every 10 seconds.
- A fix to TLS key replacement (`skep-api tls regenerate` now takes a
  directory lock); nothing to do.
- The web interface loads by route; nothing to do.

**Nothing changed in the database schema, the configuration files, or
the rc.d scripts.** No migration runs; `agent.json` and `api.json` stay
as they are.

### Before the upgrade

1. **Load the netgraph modules, and make them permanent.** In
   `/etc/rc.conf`:

   ```sh
   sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"
   kldload netgraph ng_socket ng_ether ng_bpf     # now
   kldstat -m ng_bpf -m ng_ether -m ng_socket     # all three present
   ```

   `if_epair` was already needed for jails with NICs; on a host where it
   was loaded by hand it disappears at the next reboot, so add it too.
   Without the netgraph modules, the new agent refuses every jail or VM
   start with a NIC (`port_filter_unavailable`, 409) and reports running
   guests as unfiltered.

2. **Check what the filters will enforce on guests that are running.**
   At its first start the new agent installs a filter on every running
   guest's port. From that moment a guest can send only:

   - with the MAC address Skep recorded for that NIC: a guest that set
     another MAC inside itself loses its network;
   - for a jail with a static address: from the IPv4 address Skep
     recorded, and ARP for it only. A jail whose root changed its own
     address in its `rc.conf` loses its network until it uses the
     recorded address again or the NIC is changed through the API;
   - without DHCP-server replies and IPv6 router advertisements: a guest
     that serves DHCP or acts as an IPv6 router stops doing so for the
     rest of the bridge.

   Compare each running static jail's address (`jexec skep-<id> ifconfig`)
   with what the API shows, and fix any difference before upgrading.

3. **Optional: switch on VLAN filtering on the guest bridges.** Not
   needed for the upgrade; without it the agent reports the non-critical
   health check `bridge_vlan_unfiltered` and stops nothing. The order
   matters, because a member without a PVID loses its untagged traffic
   the moment `vlanfilter` is on. For a bridge `bridge0` with uplink
   `vtnet1` on VLAN 1, **from the console or the management interface**:

   ```sh
   ifconfig bridge0 ifuntagged vtnet1 1        # the uplink first
   ifconfig bridge0 defuntagged 1              # new members get PVID 1
   # each running guest port on bridge0 (ifconfig bridge0 shows them):
   ifconfig bridge0 ifuntagged epair3a 1
   ifconfig bridge0 vlanfilter                 # last
   sysrc ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"
   ```

   A guest port left without a PVID is fixed by restarting that guest.
   Details: [Bridge VLAN filtering](../administration/guest-port-filtering.md#bridge-vlan-filtering).

### The upgrade

The general steps above. Guests can stay running.

### What the first agent start does to running guests

Before it serves its socket (so before `skep-api` can reach it), the
agent checks the ports of every running jail and VM once:

| The host | What happens to each running guest's port |
|---|---|
| Modules loaded | The port has no filter (b61e578 installed none), so the check fails: event `port_filter_missing` and an audit entry, then the filter is installed and read back: `port_filter_restored` and an audit entry. The guest keeps running and keeps its network, now filtered. |
| Modules loaded, but the install fails | The port is **removed from its bridge** (`port_filter_detached`): the guest is cut off rather than left unfiltered, until it is restarted. Not seen in validation. |
| Modules **not** loaded | Nothing is installed and nothing is detached. Health check `port_filters` reports every port unfiltered, the agent's health is `degraded`, and guests keep running unfiltered. Load the modules: the next check, within 10 seconds, installs the filters. New starts with a NIC are refused until then. |

Validated on `hearst`, a lab VM nested under KVM, on 2026-11-21
(m7c-validation item 5): b61e578
rebuilt from its commit, a jail started under it with a bare port, then
the M7c build deployed: the start-up check installed the filter
(`missing`, then `restored`, before the socket was served), the program
read back identical to the generated one, and a forged MAC from that jail
no longer reached another port.

### After the upgrade

```sh
ngctl list | grep skf_                  # one node per running guest port
skepctl system                          # or the web interface's system page
```

With a session, `GET /api/v1/health` should show `port_filters` `ok`
after the first check (it shows the repairs of the first start until
then), and `bridge_vlan_unfiltered` `ok` if you did step 3. The audit log
has one `port_filter.missing` and one `port_filter.restored` entry per
running guest port.

### Going back

Reinstall the b61e578 binaries and restart the services as above; no
database restore is needed for this pair, since neither has a schema
change. Filters installed by the newer agent stay on running guests'
ports (b61e578 neither checks nor removes them) until each guest stops.

### Status of this procedure

- The modules loaded live and the agent upgrade with running guests:
  validated on `hearst`.
- `kld_list` across a reboot (the modules come back, and the next guest
  start is filtered; Skep has no autostart): validated on `hearst`
  2026-11-22 (`tests/integration/jails/results/validation-2026-11-22.md`).
- `vlanfilter` on `bridge0`: set live in this order and persistent in
  `rc.conf` on `hearst`, validated across the same reboot.
