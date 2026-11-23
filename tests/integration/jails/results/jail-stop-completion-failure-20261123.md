# Jail stop recovery (#57): the completion-failure path, live — langrishe, 2026-11-23

Unattended run; Andy away; hearst off. Package from `fix/jail-stop-recovery`
90270cf, installed with `pkg add -f` + `skep-setup upgrade -yes` (health ok).
Logs and drivers in [jail-stop-completion-failure-20261123/](jail-stop-completion-failure-20261123/).

## How the failure was induced (nothing on the host outside the test jail)

A test template (`j4c-tpl`, pkgbase) and a test jail (`j4c`, no NICs, so no
bridge and no agent.json change). In the jail, `/etc/rc.shutdown.local` is
`sleep 30`. A stop was submitted; as soon as `jail -r skep-<id>` ran and the
stop task had rewritten the jail's `jail.conf` (mtime changed), that one file
(`/var/run/skep/jails/<id>/jail.conf`, the test jail's own) got `chflags schg`,
then the agent and its `jail -r` were killed (`kill -9`). daemon(8) restarted
the agent; `completeStop` ran `StopJail` again, whose atomic write of
`jail.conf` cannot replace an immutable file.

## Result

| Check | Result |
|---|---|
| agent log at start | **ERROR** `containers: could not complete a stop an agent restart interrupted`, `event=jail_stop_interrupted`, error `rename …/jail.conf.tmp …/jail.conf: operation not permitted` |
| `GET /containers/{id}` 3 s after the agent answered | `state: running`, `desired_state: stopped`, `jid: 5`, `state_detail: null` |
| a second stop while the flag was still set | failed (the same write) |
| after `chflags noschg`: stop | succeeded; `stopped`, no jid |
| delete container, delete template | both succeeded |
| end | `jls` empty; only the base datasets `zroot/skep{,/jail,/templates,/vm}`; `/var/run/skep/jails` empty; no file under `/var/run/skep` or `/var/db/skep` with `schg`; no `jail.conf.tmp` left |

**Finding (for Andy, amendment aa):** `completeStop` records the container
`unknown` with the detail "a stop interrupted by an agent restart could not be
completed at agent start: …; stop it again", but the jail still exists (its
`jail -r` was killed), and `GET /containers/{id}` observes the live jail
(`Service.Get` → `observe`, service.go:225), so the first read shows `running`
with desired `stopped` and **no detail**. The operator still sees that it should
be stopped, but not why it is not; the reason is only in the agent's log. Not
changed: it is a behaviour question for the Proposed amendment (keep the
detail while desired is stopped and the jail runs, as `observe` already does
for a stopped jail?).

**Run note (my error, recovered):** the driver judged the `chflags schg` step by
the exit code of a following `ls` without sudo (permission denied), so its
`finally` did not clear the flag, and its own clean-up then failed (the stop
because of the flag, the delete because the jail still ran). The flag was
cleared by hand at once (`chflags noschg`, verified with `ls -lo` and a `find
-flags +schg` over Skep's directories), and `j4c_cleanup.py` stopped and deleted
everything (log `*-job4c-langrishe-unflag.log`, `*-job4c-langrishe-cleanup.log`).
The driver in this directory is the one that ran, bug included.

End state of langrishe: Skep 90270cf (this branch) running; no containers,
templates, VMs or test datasets.
