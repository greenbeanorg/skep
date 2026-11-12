# Running skep-agent as a FreeBSD service

`packaging/rc.d/skep_agent` is the rc.d script for the host agent. There is
no package or installer yet (Milestone 10), so the steps below are manual.
They were run on FreeBSD 15.1-RELEASE-p6; see
[tests/integration/rcd](../../tests/integration/rcd/README.md) for the
recorded result.

## What the service does

- Runs `skep-agent` under `daemon(8)`, which supervises it. A crashed agent
  is started again after `skep_agent_restart_delay` seconds (default 5).
  The restarted agent resumes nothing: it marks unfinished tasks
  `interrupted` (ADR 0005).
- Runs the agent as the unprivileged account `skep`, not as root. `daemon(8)`
  itself stays root and drops privileges before it starts the agent.
- Creates what the agent needs at every start, so a cleared `/var/run` is
  not a problem:

  | Path | Owner | Mode | Purpose |
  |---|---|---|---|
  | `/var/run/skep` | `root:skep` | `0750` | Pid files now; the agent socket in Milestone 4 |
  | `/var/db/skep` | `root:wheel` | `0755` | Shared data directory |
  | `/var/db/skep/agent` | `skep:skep` | `0700` | The agent's private database directory |

- Is stopped by `rc.shutdown` (`KEYWORD: shutdown`), so the agent records
  running tasks as `interrupted` before the host goes down.
- Sends the agent's JSON log lines to syslog as `daemon.info`, tag
  `skep-agent`. With the stock `syslog.conf` they land in
  `/var/log/daemon.log`.

### Why the agent does not run as root yet

ADR 0004 makes `skep-agent` a root process with no network listener.
Today the agent still serves the unauthenticated read-only API on loopback
and performs nothing that needs privileges, so running it as root would put
a network listener in a root process for no benefit. The service therefore
runs it as `skep` until Milestone 4 moves the listener into `skep-api`. At
that point the agent's database directory changes owner to root; that
migration is part of Milestone 4.

## Install

```sh
# The service account. No login shell, no home directory.
pw groupadd skep
pw useradd skep -g skep -d /nonexistent -s /usr/sbin/nologin -c "Skep service account"

install -o root -g wheel -m 0555 skep-agent /usr/local/sbin/skep-agent
install -o root -g wheel -m 0555 packaging/rc.d/skep_agent /usr/local/etc/rc.d/skep_agent

sysrc skep_agent_enable=YES
service skep_agent start
service skep_agent status
```

The script never creates the account; it refuses to start without it.

## rc.conf variables

| Variable | Default | Meaning |
|---|---|---|
| `skep_agent_enable` | `NO` | Start the agent at boot |
| `skep_agent_bin` | `/usr/local/sbin/skep-agent` | The agent binary |
| `skep_agent_runas` | `skep` | Account the agent runs as |
| `skep_agent_rungroup` | `skep` | Group of the run and database directories |
| `skep_agent_data_dir` | `/var/db/skep` | Passed as `-data-dir`; overrides `data_dir` in the configuration file |
| `skep_agent_run_dir` | `/var/run/skep` | Pid files |
| `skep_agent_config` | empty | Passed as `-config`. When empty, the agent reads `/usr/local/etc/skep/agent.json` if it exists |
| `skep_agent_args` | empty | Extra `skep-agent` arguments |
| `skep_agent_restart_delay` | `5` | Seconds before a crashed agent is restarted; `0` disables restarts |
| `skep_agent_syslog_facility` | `daemon` | |
| `skep_agent_syslog_priority` | `info` | |
| `skep_agent_syslog_tag` | `skep-agent` | |
| `skep_agent_env` | empty | Environment for the agent (standard rc.subr variable) |

`skep_agent_flags`, `skep_agent_user`, and `skep_agent_group` are ignored on
purpose: rc.subr would apply them to `daemon(8)` instead of the agent.

Pid files: `agent-supervisor.pid` is `daemon(8)`, which `service` signals;
`agent.pid` is the agent itself.

## Remove

```sh
service skep_agent stop
sysrc -x skep_agent_enable
rm /usr/local/etc/rc.d/skep_agent /usr/local/sbin/skep-agent
rm -r /var/run/skep
rm -r /var/db/skep          # deletes the node identity, task history, and audit log
pw userdel skep             # also removes the skep group
```

## Limits

- Validated on a UFS-root host only. A ZFS-root host is pending FreeBSD
  validation.
- Logging to a file with rotation is not offered; use syslog and
  `newsyslog(8)`.
- The account's UID and GID are whatever `pw` assigns. A fixed, registered
  UID is a packaging decision for Milestone 10.
