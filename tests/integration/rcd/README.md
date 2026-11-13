# rc.d service integration test

`rcd-test` checks the `skep_agent` and `skep_api` rc.d services on a real
FreeBSD host. It runs as root, drives `service(8)`, talks to the agent over
its socket, and reads the agent's database read-only. It needs a **test
build** of the agent (build tag `skep_testtasks`), whose `test.sleep` task
gives it something in flight to interrupt. Neither the test build nor
`rcd-test` is ever installed with Skep.

It starts, stops, and kills the two services and removes `/var/run/skep`.
It changes nothing else. The account, the rc.d scripts, and the rc.conf
variables are set up by the operator beforehand.

| Phase | What it checks |
|---|---|
| `lifecycle` | Start with the run directory missing; ownership and modes, including the agent's directory and database owned by root and closed to `skep`; the agent runs as root under `daemon(8)` and has no TCP or UDP listener; the socket is `root:skep 0660`; a peer with UID `skep` is answered, a peer outside the group is refused by file permissions, and a peer inside the group but outside the allow-list is dropped by the agent's peer-credential check; `kill -9` of the agent with a task in flight (supervisor restarts it, the task is `interrupted`/`agent_restart`, not resumed, audit trail complete); `stop` with a task in flight (`agent_shutdown`); `restart`; no task left unfinished |
| `crashloop` | With `skep_agent_args="-no-such-flag"`, so that the agent exits at every start: `start` fails with a warning, `status` says the agent is not running although the supervisor is, the supervisor keeps retrying, `stop` cleans up |
| `norestart` | With `skep_agent_restart_delay=0`: a killed agent stays down, the supervisor exits, the pid files go; the next start marks the task `interrupted`/`agent_restart` |
| `api` | `skep-api` refuses to run as root; `skep_api` starts before the agent and answers `503 agent_unavailable`; it runs as `skep` under a root `daemon(8)`, listens on its loopback port and nothing else; without a session only the health status is readable; an administrator is created with `skep-api admin create` run as root (which gives up root: what it writes belongs to `skep`) and logs in; once the agent is up the public API serves the node through the socket; a running task is cancelled through the API and the agent's audit log names the user as the actor and `skep` as the verified peer; the agent restarts alone and the same API process recovers; `kill -9` of `skep-api` is supervised; `stop` leaves the agent running |
| `pre-reboot` | Leaves the agent service running with a task in flight and saves the boot time, node ID, and task ID |
| `post-reboot` | Starts nothing. The service came up at boot; node ID unchanged; history kept; the in-flight task was recorded `interrupted`/`agent_shutdown` by the agent during shutdown |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64-test/skep-agent-testbuild bin/freebsd-amd64-test/rcd-test \
    bin/freebsd-amd64/skep-api packaging/rc.d/skep_agent packaging/rc.d/skep_api hearst:skep-m4/

# On hearst (see docs/installation/freebsd-service.md for the account)
sudo install -o root -g wheel -m 0555 ~/skep-m4/skep_agent ~/skep-m4/skep_api /usr/local/etc/rc.d/
sudo sysrc skep_agent_enable=YES \
    skep_agent_bin=/home/aba/skep-m4/skep-agent-testbuild \
    skep_agent_env="SKEP_TEST_SLEEP=5m" \
    skep_api_enable=YES \
    skep_api_bin=/home/aba/skep-m4/skep-api

sudo ~/skep-m4/rcd-test lifecycle
sudo ~/skep-m4/rcd-test -api-bin /home/aba/skep-m4/skep-api api

sudo sysrc skep_agent_args="-no-such-flag"
sudo ~/skep-m4/rcd-test crashloop
sudo sysrc -x skep_agent_args

sudo sysrc skep_agent_restart_delay=0
sudo ~/skep-m4/rcd-test -restart-delay 0s norestart
sudo sysrc -x skep_agent_restart_delay

sudo ~/skep-m4/rcd-test -state ~/skep-m4/reboot-state.json pre-reboot
sudo shutdown -r now
sudo ~/skep-m4/rcd-test -state ~/skep-m4/reboot-state.json post-reboot
```

## Milestone 5 `api` phase on FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-13)

The `api` phase with the health statuses of #15 (the agent down answers
`503` with `status: unavailable`, with and without a session, and
`/system` answers `503 agent_unavailable`): **PASS**, 71 checks, with the
test build and `skep-api` at `61089f1`, which carries the web interface.

## Milestone 4 result on FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12)

Account `skep` uid 1002, gid 1002. Agent test build and `skep-api` at
`f837d23`; `rcd-test` with the stale-socket fix described below. A Proxmox
snapshot (`pre-m4`) was taken before. The existing Milestone 3 installation
was upgraded in place: `/var/db/skep/agent` was `skep:skep` before the
first start of the new script.

| Phase | Result |
|---|---|
| `lifecycle` | **PASS**, 100 checks |
| `crashloop` | **PASS**, 12 checks |
| `norestart` | **PASS**, 50 checks (second run, see below) |
| `api` | **PASS**, 71 checks |
| `pre-reboot`, one `shutdown -r now`, `post-reboot` | **PASS**; `post-reboot` 24 checks |

The upgrade from Milestone 3: the first start changed the owner of
`/var/db/skep/agent` and `agent.db` from `skep:skep` to `root:wheel`, and
the node ID stayed `97fc6968-7e82-49c9-bda8-9026775ca964`, the one recorded
for Milestone 3 below. The task history was kept.

`lifecycle`, excerpt:

```
ok     /var/db/skep/agent is 0:0 mode 0700 (want 0:0 0700)
ok     /var/db/skep/agent/agent.db is 0:0 mode 0600 (want 0:0 0600)
ok     uid 1002 (skep) cannot open the agent's database (refused-open: open /var/db/skep/agent/agent.db: permission denied)
ok     agent pid 4620 runs as root (user=root ruser=root)
ok     the agent listens on no TCP or UDP socket ([])
ok     /var/run/skep/agent.sock is a socket, 0:1002 mode 0660 (want 0:1002 0660)
ok     uid 1002 (skep) gid 1002: the agent answers (ok)
ok     uid 65534 gid 65534: file permissions refuse the connection (refused-connect: dial unix /var/run/skep/agent.sock: connect: permission denied)
ok     uid 65534 gid 1002: connects, and the agent's peer check drops it (refused-peer: Get "http://agent/internal/v1/system": EOF)
ok     system over the socket: platform=freebsd production_host=true dev_mode=false node_id=97fc6968-7e82-49c9-bda8-9026775ca964
ok     the supervisor started a new agent (pid 4728, was 4620)
ok     task a5957a7a-... is interrupted with error code "agent_restart"
ok     the socket file was removed (lstat /var/run/skep/agent.sock: no such file or directory)
ok     no task is left pending or running after the final stop (42 tasks in history)
```

`api`, excerpt:

```
ok     started directly as root: exit 1, "skep-api: refusing to run as root: skep-api is the network-facing process and must run as the unprivileged skep account (use the skep_api rc.d service)"
ok     /var/db/skep/api/tls/key.pem is 1002:1002 mode 0600 (want 1002:1002 0600)
ok     skep-api pid 6266 runs as skep, not root (user=skep ruser=skep)
ok     skep-api listens on 127.0.0.1:8443 and nothing else ([tcp4 127.0.0.1:8443])
ok     the API is not reachable on 10.79.20.170:8443
ok     health without a session and without the agent: status 503, body map[status:degraded] (the status only)
ok     'skep-api admin' run as root: Administrator "rcd-test" created (user ID 76281f45-...).
ok     /var/db/skep/api/api.db is 1002:1002 mode 0600 (want 1002:1002 0600)
ok     login over the API as rcd-test: status 200, role "admin"
ok     system without the agent: status 503, code "agent_unavailable"
ok     the agent's audit log: actor "rcd-test", peer uid 1002 (want 1002, the skep account), request ID "rcd-test-cancel"
ok     the same skep-api (pid 6266) answers again: status 200
ok     the supervisor started a new skep-api (pid 6506, was 6266)
ok     the agent kept running
```

`post-reboot`, excerpt:

```
ok     kern.boottime changed: { sec = 1794524341, usec = 654897 } Thu Nov 12 17:59:01 2026
ok     agent pid 1343 runs as root (user=root ruser=root)
ok     the node ID is unchanged: 97fc6968-7e82-49c9-bda8-9026775ca964
ok     task history survived and grew: 56 tasks before, 58 now
ok     task cbece339-... is interrupted with error code "agent_shutdown"
```

After the reboot `skep_api` had also started at boot, checked by hand:
`skep_api is running as pid 1374`, `sockstat` showed `skep skep-api 1374
tcp4 127.0.0.1:8443`, `GET /api/v1/health` answered `{"status":"ok"}`, and
the key and certificate in `/var/db/skep/api/tls` were still the ones
generated before the reboot (`key.pem` `skep:skep 0600`).

**One failure in the first `norestart` run, in the test, not the agent.**
After `kill -9` the agent cannot remove its socket, so the check "the socket
file was removed" failed. Nothing answered on the stale socket, and the
next start replaced it and served again; both were checked and passed.
`rcd-test` now expects a stale socket after `SIGKILL` and only reports it.
The rerun passed all 50 checks.

Afterwards: the services were stopped, every `skep_*` variable was removed
from `rc.conf`, and the test administrator `rcd-test` was deleted from
`api.db`. There is no local command to delete a user, and the API refuses
to delete the last administrator, so it was deleted with `sqlite3` as
`skep`, in one transaction with an audit entry (`local:1001`,
`user.delete`). The rc.d scripts, `/var/db/skep/agent` (now root-owned),
and `/var/db/skep/api` remain.

Not covered:

- A tmpfs `/var/run`, a ZFS-root host, and an agent that ignores SIGTERM,
  as for Milestone 3.
- The password prompt on the system console (see
  `tests/integration/adminprompt`).

## Milestone 3 result on FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12)

This run predates the split: the agent still ran as `skep` and served the
API on loopback TCP. It is kept as the record of what the service
lifecycle, crash supervision, and reboot behaviour were validated with.

Account `skep` uid 1002, gid 1002. Every phase was run with the rc.d
script as committed here; the agent test build was at `7bfb32c` (the agent
code itself is unchanged since `6ad561a`).

All five phases: **PASS**, every check. The host was rebooted twice in
total (once with an earlier version of the script, once with this one) and
was back within 30 seconds both times.

`lifecycle` (78 checks), excerpt:

```
       $ service skep_agent status -> exit 0: skep_agent is running as pid 3731 (supervisor pid 3729).
ok     supervisor pid 3201 is daemon(8) running as root (root: daemon: /home/aba/skep-m3/skep-agent-testbuild[3202] (daemon))
ok     agent pid 3202 runs as skep, not root (user=skep ruser=skep)
ok     the API is not reachable on 10.79.20.170:8765
== kill -9 the agent while a task is in flight
ok     a test.sleep task is in flight with progress events: id=ac243fea-... status=running
       sent SIGKILL to agent pid 3202
ok     the supervisor started a new agent (pid 3293, was 3202)
ok     task ac243fea-... is interrupted with error code "agent_restart"
ok     its last event is "task is interrupted"
ok     its audit trail is requested/success then finished/failure(interrupted), actor system, peer uid 1002
ok     the interrupted task was not resumed
== service stop while a task is in flight
ok     service stop succeeds (took 25ms)
ok     task 1064cc5b-... is interrupted with error code "agent_shutdown"
```

(The pids in the excerpt are from the first of two runs; the status line is
from the second, with the final script.)

`crashloop`:

```
       $ service skep_agent start -> exit 1: Starting skep_agent. | ... WARNING: skep-agent is not running after start; see the daemon log, tag skep-agent | ... WARNING: the supervisor keeps retrying every 5s; use 'service skep_agent stop' to stop it
ok     service start reports that the agent is not running (exit 1)
ok     the supervisor is running (pid 3220)
       $ service skep_agent status -> exit 1: skep_agent is not running: the supervisor (pid 3220) is up, but skep-agent has exited and is being restarted.
ok     the supervisor is still running after two restart delays
ok     no process of skep is left running between attempts ("")
ok     service stop succeeds
ok     both pid files were removed
```

`norestart`:

```
       sent SIGKILL to agent pid 2761
ok     the supervisor exited with the agent
ok     both pid files were removed
       $ service skep_agent status -> exit 1: skep_agent is not running.
ok     the killed agent's task is still recorded as running until the next start (running)
== the next start sweeps the task
ok     task 66dd7f07-... is interrupted with error code "agent_restart"
```

`pre-reboot`, `shutdown -r now`, `post-reboot`:

```
ok     kern.boottime changed: { sec = 1794465822, usec = 647064 } Thu Nov 12 01:43:42 2026
       $ service skep_agent status -> exit 0: skep_agent is running as pid 1322 (supervisor pid 1320).
ok     /var/run/skep is 0:1002 mode 0750 (want 0:1002 0750)
ok     agent pid 1322 runs as skep, not root (user=skep ruser=skep)
ok     the node ID is unchanged: 97fc6968-7e82-49c9-bda8-9026775ca964
ok     task history survived and grew: 31 tasks before, 33 now
ok     task 783f6de9-... is interrupted with error code "agent_shutdown"
ok     this start ran discovery.refresh: ... status=succeeded actor=system peer_uid=1002
```

Also checked by hand:

- The shipped (untagged) agent, started and stopped through the service:
  `status` running, `/api/v1/health` ok, running as `skep`.
- Log lines arrive in `/var/log/daemon.log` only, with the default
  `daemon.info`.

Not covered:

- On this host `/var/run` is on the UFS root and `cleanvar` removes files,
  not directories, so `/var/run/skep` itself survived the reboots. The
  "directory is missing" case is covered by `lifecycle`, which removes it,
  not by a real tmpfs `/var/run`.
- A ZFS-root host.
- An agent that ignores SIGTERM past rc.subr's wait.
- An agent that starts, runs for a while, and then begins to crash: `start`
  has long returned, so only `status` and the log show it.
