# Running Skep as FreeBSD services

Skep runs as two rc.d services (ADR 0004, ADR 0006):

| Service | Script | Runs as | Listens on |
|---|---|---|---|
| `skep_agent` | `packaging/rc.d/skep_agent` | `root` | The Unix socket `/var/run/skep/agent.sock` only. No network listener. |
| `skep_api` | `packaging/rc.d/skep_api` | `skep` (unprivileged) | TCP, `127.0.0.1:8443` by default |

The package and [`skep-setup`](skep-setup.md) (Milestone 10,
[ADR 0012](../decisions/0012-installer.md)) do these steps for you; they
ran on two lab VMs nested under KVM on 2026-11-22
([record](../../tests/integration/setup/results/lab-vm-validation-20261122.md)).
The steps below are the manual ones;
[Host requirements](host-requirements.md) puts every step of a new host
in order. They were run on FreeBSD 15.1-RELEASE-p6 in a lab VM nested
under KVM (`hearst`), not on bare metal, including an in-place
upgrade of a Milestone 3 installation; the native test and its recorded
result are in
[tests/integration/rcd](../../tests/integration/rcd/README.md).

## What the services do

Both scripts run their program under `daemon(8)`, which supervises it. A
crashed process is started again after `*_restart_delay` seconds (default
5). `daemon(8)` itself is root in both cases; for `skep_api` it drops to
the `skep` account before it starts `skep-api`, and `skep-api` refuses to
start if it finds itself running as root.

A restarted agent resumes nothing: it marks unfinished tasks `interrupted`
(ADR 0005). A restarted API server holds no state that matters to tasks.

The start order does not matter for correctness. `skep_api` declares
`REQUIRE: skep_agent` so that the agent is normally up first, but either
service can start first or restart alone. Without the agent, the API
answers node requests with `503 agent_unavailable`.

**A third service, `skep_cluster`** (Milestone 8, step 8a), runs
`skep-cluster` as the `skep-cluster` account (UID/GID 410, created by the
package). It is not enabled by `skep-setup` yet, and neither of the other
two services needs it. To try it on a host:

```sh
skep-cluster init                    # as root, once; reads the node ID from the running agent
sysrc skep_cluster_enable=YES
service skep_cluster start
skep-cluster status
```

It opens no network port unless `skep-cluster listen -address <addr>` or
`skep-cluster join-request -address <addr>` is run ([joining hosts into a
cluster](../administration/cluster.md)). What it does, its files and its socket are in
[skep-cluster](../architecture/skep-cluster.md). After a package upgrade,
restart it by hand: `skep-setup upgrade` does not know it yet.

Both scripts create what they need at every start, so a cleared `/var/run`
is not a problem:

| Path | Owner | Mode | Purpose |
|---|---|---|---|
| `/var/run/skep` | `root:skep` | `0750` | The agent socket and the pid files |
| `/var/run/skep/agent.sock` | `root:skep` | `0660` | Created by the agent; the only way to reach it |
| `/var/db/skep` | `root:wheel` | `0755` | Shared data directory |
| `/var/db/skep/agent` | `root:wheel` | `0700` | The agent's database; closed to `skep` |
| `/var/db/skep/api` | `skep:skep` | `0700` | The API server's state |

Each program creates its audit archive itself, when it first seals a
segment:

| Path | Owner | Mode | Purpose |
|---|---|---|---|
| `/var/db/skep/agent/audit-archive` | `root:wheel` | `0700`, files `0400` | Sealed segments of the agent's operation audit log |
| `/var/db/skep/api/audit-archive` | `skep:skep` | `0700`, files `0400` | Sealed segments of the access audit log |

The agent checks the peer of every socket connection against an allow-list
of UIDs (`root` and `skep` by default), using credentials reported by the
kernel. File permissions are the first check; this is the second.

- `service <name> start` waits up to ten seconds for the program itself. If
  it exits at once (a bad argument, an unreadable configuration file),
  `start` prints a warning and returns 1. The supervisor is still running
  and keeps retrying; `service <name> stop` stops it.
- `service <name> status` reports the program, not the supervisor: it
  returns 1 while a failing program is being restarted.
- Both are stopped by `rc.shutdown` (`KEYWORD: shutdown`), so the agent
  records running tasks as `interrupted` before the host goes down.
- Log lines (JSON) go to syslog as `daemon.info`, tags `skep-agent` and
  `skep-api`. With the stock `syslog.conf` they land in
  `/var/log/daemon.log`. **A syslog line can hold more than one JSON
  record**, separated by a space: daemon(8) on FreeBSD 15.1 sends every
  complete line it read from the program in one go as one syslog message,
  and syslogd turns the newline between them into a space. It happens
  when two records are written within microseconds of each other. A
  record longer than about 4 KB is split over two syslog lines. A parser
  of `daemon.log` should decode the JSON values one after another (for
  example `jq -c .` on the text after `skep-agent[pid]: `) rather than
  assume one record per line. Details:
  [tests/integration/rcd/daemon-log-joined-records.md](../../tests/integration/rcd/daemon-log-joined-records.md).

### Upgrading

[Upgrading a host](upgrading.md) has the general steps and what each
release needs, including from b61e578 (Milestone 7b) to Milestone 7c.

### Upgrading from the Milestone 3 service

Until Milestone 4 the agent ran as `skep` and owned `/var/db/skep/agent`.
The new `skep_agent` script changes the owner of that directory and
everything in it to `root:wheel` the first time it starts
(`chown -R -P`, logged). Nothing else has to be migrated. The old rc.conf
variable `skep_agent_runas` is no longer read and can be removed.

## Install

```sh
# The service account, UID/GID 409 (ADR 0012). No login shell, no home directory.
pw groupadd skep -g 409
pw useradd skep -u 409 -g skep -d /nonexistent -s /usr/sbin/nologin -c "Skep service account"

install -o root -g wheel -m 0555 skep-agent skep-api /usr/local/sbin/
install -o root -g wheel -m 0555 packaging/rc.d/skep_agent packaging/rc.d/skep_api /usr/local/etc/rc.d/

sysrc skep_agent_enable=YES skep_api_enable=YES
service skep_agent start
service skep_api start
service skep_agent status
service skep_api status
```

The scripts never create the account; they refuse to start without it.

Configuration files are optional. When present they are read from
`/usr/local/etc/skep/agent.json` and `/usr/local/etc/skep/api.json`; the
samples are in `packaging/etc/`. `api.json` must be readable by the `skep`
account (`root:skep`, `0640`).

## The first administrator

There are no default credentials. After `skep_api` has started once (which
creates `/var/db/skep/api`), create an administrator on the host:

```sh
skep-api admin create --username andy
```

The command asks for the password twice, without echo. Run it as root: it
changes to the `skep` account before it opens anything. For unattended
installs, `--password-stdin` reads the password from standard input. A
lost password is replaced the same way, with
`skep-api admin reset-password --username NAME`, which also ends that
user's sessions. `skep-api` does not need to be running, or restarted, for
either command.

`skep-api admin disable --username NAME` and
`skep-api admin delete --username NAME --yes` disable or delete a user on
the host, ending its sessions. Neither touches the only enabled
administrator.

Since step 8c-1 (unreleased) the users are `skep-cluster`'s: these
commands write through its socket (`cluster_socket` in `api.json`,
default `/var/run/skep-cluster/cluster.sock`) and need `skep_cluster`
running with quorum; without it they answer `no_quorum` and change
nothing. A host upgraded from 0.1 runs `skep-api admin import-users` once
(see [cluster administration](../administration/cluster.md#users-in-a-cluster-step-8c-1)).

`skep-api admin exists --username NAME` says whether a user exists. It
opens `api.db` read-only and only if it exists: it never creates the
database or its directory and never migrates the schema. Exit status 0:
the user exists (with its role, and whether it is disabled); 1: it does
not; 2: the question could not be answered (no `api.db`, an unreadable
database, a bad name). `skep-setup` uses it to plan `-admin`.

To use the API from the command line, see
[skepctl](../administration/skepctl.md). To use it from a browser, see
[the web interface](../administration/web-ui.md) below and in its own
guide.

## Opening the API to the network

`skep-api` listens on `127.0.0.1:8443` until told otherwise. To reach it
from the management network, set the management address in
`/usr/local/etc/skep/api.json` and restart the service:

```json
{ "listen_address": "10.79.20.170:8443" }
```

Create the administrator first. Every endpoint except the health status
and the login needs a session.

## The web interface

`skep-api` serves the web interface on the same address, from files
embedded in the binary: open `https://` plus the listen address (for the
example above, `https://10.79.20.170:8443/`) and log in. The binaries from
`make build-freebsd` carry it; a `skep-api` built with `make build-freebsd-go`
or `go build` serves a page that says the interface was not built in, and
the API is unaffected.

On the first visit the browser warns about the self-signed certificate.
Compare it with the host before accepting it:

```sh
skep-api tls fingerprint                                                   # the public key pin
openssl x509 -in /var/db/skep/api/tls/cert.pem -noout -fingerprint -sha256  # the certificate fingerprint
```

and check that the browser's certificate details show the same public key
or the same fingerprint. [docs/administration/web-ui.md](../administration/web-ui.md)
has the browser-by-browser steps and what the interface does.

## TLS certificate

`skep-api` serves TLS 1.3 only. At its first start it generates a key and a
self-signed certificate in `/var/db/skep/api/tls` and renews the
certificate by itself, keeping the key. To see the public key pin, or to
replace the key:

```sh
skep-api tls fingerprint
skep-api tls regenerate     # new key; every client that pinned the old one must confirm it
```

Run them as root (they change to the service account first) or as `skep`.
`skep-api tls regenerate` does not need a restart: the running server
picks the new pair up within a minute, or at once with
`pkill -HUP -F /var/run/skep/api.pid`.

A new pair is first written, complete and synced, as `key.pem.next` and
`cert.pem.next`, and then moved into place: the key, then the
certificate. The whole swap runs under an exclusive `flock(2)` lock on
the TLS directory, and every read of the pair takes a shared lock on it,
so the server never loads a key and certificate that do not belong
together, even while `skep-api tls regenerate` runs in another process.
A renewal takes the same lock and first checks that `key.pem` still holds
the key it is renewing. If the host stops between the two moves, the next
start (or the next reload) finishes the move. Leftover `.next` files next to
a working pair (the host stopped before anything was moved) are ignored
and replaced by the next regenerate. Skep never moves a pending key
whose certificate does not match it.

Each regenerate is two entries in the access audit log, action
`tls.key.regenerate`: the intent (`"phase":"requested"`), written before
anything on disk changes, and the outcome (`"phase":"finished"`, success
or failure), joined by `operation_id`. An intent without an outcome means
the command was interrupted. Compare its `new_public_key_sha256` with
`skep-api tls fingerprint` to see whether the new key was installed.

To use your own certificate, set `tls.cert_file` and `tls.key_file` in
`/usr/local/etc/skep/api.json`. The files must be readable by `skep`. After
replacing them, the server reloads them within a minute or on `SIGHUP`.

## rc.conf variables

`skep_agent`:

| Variable | Default | Meaning |
|---|---|---|
| `skep_agent_enable` | `NO` | Start the agent at boot |
| `skep_agent_bin` | `/usr/local/sbin/skep-agent` | The agent binary |
| `skep_agent_rungroup` | `skep` | Group of the run directory and the socket |
| `skep_agent_data_dir` | `/var/db/skep` | Passed as `-data-dir`; overrides `data_dir` in the configuration file |
| `skep_agent_run_dir` | `/var/run/skep` | Socket and pid files |
| `skep_agent_config` | empty | Passed as `-config`. When empty, the agent reads `/usr/local/etc/skep/agent.json` if it exists |
| `skep_agent_args` | empty | Extra `skep-agent` arguments |
| `skep_agent_restart_delay` | `5` | Seconds before a crashed agent is restarted; `0` disables restarts |
| `skep_agent_syslog_facility` | `daemon` | |
| `skep_agent_syslog_priority` | `info` | |
| `skep_agent_syslog_tag` | `skep-agent` | |
| `skep_agent_env` | empty | Environment for the agent (standard rc.subr variable) |

`skep_api` has the same variables with the prefix `skep_api_` (binary
`/usr/local/sbin/skep-api`, tag `skep-api`), plus:

| Variable | Default | Meaning |
|---|---|---|
| `skep_api_runas` | `skep` | Account `skep-api` runs as. Must not be root |
| `skep_api_rungroup` | `skep` | Group of the run directory and of `/var/db/skep/api` |

`*_flags`, `*_user`, and `*_group` are ignored on purpose: rc.subr would
apply them to `daemon(8)` instead of the program.

Pid files: `agent-supervisor.pid` and `api-supervisor.pid` are `daemon(8)`,
which `service` signals; `agent.pid` and `api.pid` are the programs.

If the agent's socket path or run directory is changed, change
`agent_socket` in `api.json` (or `skep_api_args="-agent-socket ..."`) to
match.

## Retention and the audit archive

Both services clean up after themselves ([ADR 0007](../decisions/0007-retention.md)).
Each does its work once after start and then once a day.

| What | Default | Setting |
|---|---|---|
| Finished tasks and their events | deleted 90 days after the task finished | `retention.tasks_days` in `agent.json` (`0` keeps them; otherwise at least 7) |
| The agent's operation audit log | sealed monthly; entries are never removed from the database | `retention.audit_live_days` in `agent.json` (`0`, or at least 35) |
| The access audit log | sealed monthly; entries are removed 90 days after the end of their month | `retention.audit_live_days` in `api.json` (`0` never removes; otherwise at least 35) |
| Sealing itself | on | `retention.audit_archive` in either file; `false` seals nothing and never removes an audit entry |

- **A segment** is one month of one audit log, as a file
  `audit-<node id>-<YYYY-MM>-<first id>-<last id>.jsonl.gz`. Segments are
  chained by their SHA-256 hashes. Skep never deletes or rewrites one.
  You may copy them elsewhere at any time, and move old ones away if you
  must; a segment that is gone is reported by every run and by `audit
  verify`.
- **Back up the two `audit-archive` directories** with the databases.
  After live entries are removed, the segment is the only copy.
- **Each seal is logged to syslog** at `daemon.notice`, tags `skep-agent`
  and `skep-api`. With the stock `syslog.conf` these lines are in
  `/var/log/messages`. If you forward syslog to another host, they are an
  independent record of the hashes.
- **Health.** `GET /api/v1/health` (with a session) and the `health`
  field of `GET /api/v1/system` have the checks `audit_archive` and
  `access_audit_archive`. One is `degraded` when an archive failed
  verification, when a segment that is needed is missing, and when last
  month is still unsealed on the 8th. The cause is in the service's log.
  A degraded archive does not stop the service, so the endpoint still
  answers `200`; see [Monitoring health](#monitoring-health).
- **The agent's run is a task**, `maintenance.retention`, visible with
  `skepctl tasks list`. Its result says what was deleted and sealed. A
  failed run loses nothing; the next one starts over.

Check and read the archive on the host, whether or not the services run:

```sh
skep-agent audit verify                  # as root
skep-agent audit export --month 2026-10  # archived entries, one JSON object per line
skep-agent audit export --unsealed       # as root, agent running: entries not yet sealed, read-only
skep-api audit verify                    # as root (it switches to skep) or as skep
skep-api audit export
```

`audit verify` and `audit export` (without `--unsealed`) refuse a missing database, and one that
the service has not yet migrated to the segment index; they create and
migrate nothing, and write no entries. They open the database read-write,
however, so SQLite may still touch its files: with the service stopped it
creates `-wal` and `-shm` and removes them again when the command exits,
after writing into the database any changes an unclean stop left in
`-wal`. A database in rollback-journal mode, which neither service leaves
behind, is switched to WAL. With the service running, the files are not
changed.

`audit export --unsealed` reads only while the agent runs, and refuses
while it is stopped: SQLite creates the database's `-wal` and `-shm` files
even for a read-only open, and would leave them behind on a database no
service has open. Its entries are not sealed yet, so no segment hash covers
them; it says so on stderr, with the number of entries, and stdout carries
only the entries.

`audit verify` prints one line per segment and the hash at the head of the
chain, and exits non-zero at the first break. Use `--allow-missing` if you
moved segments away on purpose.

If `audit verify` reports an altered or missing segment whose entries are
still in the database, nothing has been lost: the entries stay until the
file is restored from a copy. Do not edit `audit_segments`; the database
refuses it.

The database files do not shrink when rows are removed; SQLite reuses the
space, so they stop growing. To shrink one, stop its service and run
`VACUUM` on it with `sqlite3(1)`.

## Database backups

```sh
skep-agent db backup
skep-api db backup
```

Each writes a consistent copy of its database (SQLite `VACUUM INTO` on a
read-only connection; the service keeps running) to
`/var/db/skep/backup/<version>+<commit>-<UTC timestamp>/` (`0700`, the
copy `0600`), with its SHA-256 and a listing of the audit archive (not a
copy). `-dir DIR` writes into a given directory there, `-keep N` keeps
the newest N backups made this way (default 3). `skep-setup upgrade`
runs both before every restart ([skep-setup](skep-setup.md#upgrades)).

## Monitoring health

`/health` and `/healthz` at the root of the port, with or without a
trailing slash, are not health checks:
they answer `404` with a JSON error naming `/api/v1/health`.

`GET /api/v1/health` needs no session. Without one it answers only the
status:

| Status | HTTP | Meaning |
|---|---|---|
| `ok` | 200 | Every check passed. |
| `degraded` | 200 | A non-critical check failed, for example an audit archive. The service serves requests, but needs an operator. |
| `unavailable` | 503 | A critical check failed: `skep-api` cannot reach the agent, or the agent cannot reach its database. The service cannot serve requests. |

With a session, when the agent's socket does not answer, the `agent`
check says the agent is down, still starting, or refusing to start, and
points at `/var/log/daemon.log`, where a refused start is logged. Without a session the answer stays the status alone.

Use the HTTP status for a load balancer or a supervisor, which should act
only when the service cannot serve. Use the `status` field for alerting,
which should also notice a degraded component:

```sh
curl -s --cacert $CA https://hearst:8443/api/v1/health | grep -q '"status":"ok"' || echo "attention"
```

Which check failed is shown only with a session: in the full health
document, where each check says whether it is `critical`, and in the
`health` field of `GET /api/v1/system`.

The agent's guest-network checks are non-critical: `port_filters` (a
running guest's port lost its anti-spoofing filter, was detached, or the
netgraph modules are missing) and `bridge_vlan_unfiltered` (a guest bridge
without VLAN filtering). See
[Guest-port anti-spoofing](../administration/guest-port-filtering.md).

## Kernel modules and bridges for guests

Skep loads no kernel modules. A host that runs guests with NICs needs
these in `kld_list` in `/etc/rc.conf`:

```sh
sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"
kldload if_epair netgraph ng_socket ng_ether ng_bpf    # now, without a reboot
```

`+=` appends to an existing `kld_list`; writing `kld_list="…"` by hand
would replace it and drop modules the host already loads at boot.

`if_epair` gives jails their interfaces. The netgraph modules filter every
guest port (ADR 0011). `vmm` and `nmdm` go in `/boot/loader.conf`
([VM guide](../administration/virtual-machines.md#preparing-the-host)).
Guest bridges should filter VLANs (`vlanfilter`, with the uplink's
`ifuntagged` PVID set **first**, and `defuntagged`), as described in
[Bridge VLAN filtering](../administration/guest-port-filtering.md#bridge-vlan-filtering).

## Talking to the agent directly

The agent has no TCP port. For debugging, as root or `skep`:

```sh
curl --unix-socket /var/run/skep/agent.sock \
    -H 'Skep-Actor-Id: local' -H 'Skep-Actor-Name: local' \
    http://agent/internal/v1/system
```

The actor headers are mandatory on every request. The internal API is not
a public interface and may change without notice.

## Remove

```sh
service skep_api stop
service skep_agent stop
sysrc -x skep_api_enable skep_agent_enable
rm /usr/local/etc/rc.d/skep_api /usr/local/etc/rc.d/skep_agent
rm /usr/local/sbin/skep-api /usr/local/sbin/skep-agent
rm -r /var/run/skep
rm -r /var/db/skep          # deletes the node identity, task history, audit logs, and API state
pw userdel skep             # also removes the skep group
```

## Limits

- The native rc.d harness ran on a UFS-root lab VM nested under KVM
  only. On a ZFS-root lab VM (`langrishe`) the services were installed by
  the package, enabled and started by `skep-setup apply`, and running
  after a reboot
  ([record](../../tests/integration/setup/results/lab-vm-validation-20261122.md)),
  without the harness. The audit archive and retention were also validated
  with the data directory on a ZFS dataset. Nothing here has run on bare
  metal.
- Logging to a file with rotation is not offered; use syslog and
  `newsyslog(8)`.
- The account's UID and GID are fixed at 409 by ADR 0012 (the lowest pair
  free in the ports registry on 2026-11-22; registered when a port is
  submitted). Hosts set up earlier keep what `pw` assigned.
