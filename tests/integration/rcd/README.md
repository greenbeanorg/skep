# rc.d service integration test

`rcd-test` checks the `skep_agent` rc.d service on a real FreeBSD host. It
runs as root, drives `service(8)`, and reads the agent's database read-only.
It needs a **test build** of the agent (build tag `skep_testtasks`), whose
`test.sleep` task gives it something in flight to interrupt. Neither the
test build nor `rcd-test` is ever installed with Skep.

It starts, stops, and kills the service and removes `/var/run/skep`. It
changes nothing else. The account, the rc.d script, and the rc.conf
variables are set up by the operator beforehand.

| Phase | What it checks |
|---|---|
| `lifecycle` | Start with the run directory missing; ownership and modes; the agent runs as `skep` under a root `daemon(8)`; loopback only; `kill -9` of the agent with a task in flight (supervisor restarts it, the task is `interrupted`/`agent_restart`, not resumed, audit trail complete); `stop` with a task in flight (`agent_shutdown`); `restart`; no task left unfinished |
| `crashloop` | With `skep_agent_args="-no-such-flag"`, so that the agent exits at every start: `start` fails with a warning, `status` says the agent is not running although the supervisor is, the supervisor keeps retrying, `stop` cleans up |
| `norestart` | With `skep_agent_restart_delay=0`: a killed agent stays down, the supervisor exits, the pid files go; the next start marks the task `interrupted`/`agent_restart` |
| `pre-reboot` | Leaves the service running with a task in flight and saves the boot time, node ID, and task ID |
| `post-reboot` | Starts nothing. The service came up at boot; node ID unchanged; history kept; the in-flight task was recorded `interrupted`/`agent_shutdown` by the agent during shutdown |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64-test/skep-agent-testbuild bin/freebsd-amd64-test/rcd-test \
    packaging/rc.d/skep_agent hearst:skep-m3/

# On hearst (see docs/installation/freebsd-service.md for the account)
sudo install -o root -g wheel -m 0555 ~/skep-m3/skep_agent /usr/local/etc/rc.d/skep_agent
sudo sysrc skep_agent_enable=YES \
    skep_agent_bin=/home/aba/skep-m3/skep-agent-testbuild \
    skep_agent_env="SKEP_TEST_SLEEP=5m"

sudo ~/skep-m3/rcd-test lifecycle

sudo sysrc skep_agent_args="-no-such-flag"
sudo ~/skep-m3/rcd-test crashloop
sudo sysrc -x skep_agent_args

sudo sysrc skep_agent_restart_delay=0
sudo ~/skep-m3/rcd-test -restart-delay 0s norestart
sudo sysrc -x skep_agent_restart_delay

sudo ~/skep-m3/rcd-test -state ~/skep-m3/reboot-state.json pre-reboot
sudo shutdown -r now
sudo ~/skep-m3/rcd-test -state ~/skep-m3/reboot-state.json post-reboot
```

## Result on FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12)

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
