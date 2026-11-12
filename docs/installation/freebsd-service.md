# Running Skep as FreeBSD services

Skep runs as two rc.d services (ADR 0004, ADR 0006):

| Service | Script | Runs as | Listens on |
|---|---|---|---|
| `skep_agent` | `packaging/rc.d/skep_agent` | `root` | The Unix socket `/var/run/skep/agent.sock` only. No network listener. |
| `skep_api` | `packaging/rc.d/skep_api` | `skep` (unprivileged) | TCP, `127.0.0.1:8443` by default |

There is no package or installer yet (Milestone 10), so the steps below are
manual. They were run on FreeBSD 15.1-RELEASE-p6, including an in-place
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

Both scripts create what they need at every start, so a cleared `/var/run`
is not a problem:

| Path | Owner | Mode | Purpose |
|---|---|---|---|
| `/var/run/skep` | `root:skep` | `0750` | The agent socket and the pid files |
| `/var/run/skep/agent.sock` | `root:skep` | `0660` | Created by the agent; the only way to reach it |
| `/var/db/skep` | `root:wheel` | `0755` | Shared data directory |
| `/var/db/skep/agent` | `root:wheel` | `0700` | The agent's database; closed to `skep` |
| `/var/db/skep/api` | `skep:skep` | `0700` | The API server's state |

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
  `/var/log/daemon.log`.

### Upgrading from the Milestone 3 service

Until Milestone 4 the agent ran as `skep` and owned `/var/db/skep/agent`.
The new `skep_agent` script changes the owner of that directory and
everything in it to `root:wheel` the first time it starts
(`chown -R -P`, logged). Nothing else has to be migrated. The old rc.conf
variable `skep_agent_runas` is no longer read and can be removed.

## Install

```sh
# The service account. No login shell, no home directory.
pw groupadd skep
pw useradd skep -g skep -d /nonexistent -s /usr/sbin/nologin -c "Skep service account"

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

## Opening the API to the network

`skep-api` listens on `127.0.0.1:8443` until told otherwise. To reach it
from the management network, set the management address in
`/usr/local/etc/skep/api.json` and restart the service:

```json
{ "listen_address": "10.79.20.170:8443" }
```

Create the administrator first. Every endpoint except the health status
and the login needs a session.

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

- Validated on a UFS-root host only. A ZFS-root host is pending FreeBSD
  validation.
- Logging to a file with rotation is not offered; use syslog and
  `newsyslog(8)`.
- The account's UID and GID are whatever `pw` assigns. A fixed, registered
  UID is a packaging decision for Milestone 10.
