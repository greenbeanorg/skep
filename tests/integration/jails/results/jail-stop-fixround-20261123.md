# Jail stop recovery (#57) fix round: live validation — langrishe, 2026-11-23

Unattended run; Andy away; hearst off. Package from `fix/jail-stop-recovery`
eae2ab3, installed with `pkg add -f` + `skep-setup upgrade -yes` (health ok).
Driver and logs: [jail-stop-fixround-20261123/](jail-stop-fixround-20261123/)
(`fixround.py`; the first attempt stopped at template create with a 401, an
expired skepctl session, and rolled back its setup; see `*-attempt1-401.log`).

Setup: test bridge `bridge1` on vtnet1 (`ifuntagged vtnet1 1 defuntagged 1
vlanfilter`), `agent.json` `network.bridges` = `["bridge1"]` (backed up, then
restored, sha256 `3740c603…1fed` checked), a pkgbase test template `p57-tpl`,
a test jail `p57` with one DHCP NIC on `bridge1`, its `/etc/rc.shutdown.local`
= `sleep 30` so each stop could be caught.

## B — the j3kill success case

Stop submitted; 0.1 s later the epair was gone, `jail -r` running and the jail
still in `jls`: `kill -9` of the agent and that `jail -r`. daemon(8) restarted
the agent.

| Check | Result |
|---|---|
| Log at start | `jail_stop_completing` (WARN), `max_wait_seconds: 190`; 30 s later `jail_stop_completed` (WARN) |
| GET after the restart (36 s after the kill) | `stopped` / desired `stopped` / `jid` null / `host_interface` null / "a stop interrupted by an agent restart was completed at agent start" |
| Host | `jls` empty, no epairs |

## C — agent stopped during a completion

As B, then, 4.4 s after the kill, the restarted agent logged
`jail_stop_completing` and ran `jail -r` (pid 59194); 3 s later `service
skep_agent stop` (took 1.1 s).

| Check | Result |
|---|---|
| Log | `jail_stop_completion_cancelled` (WARN, `error: context canceled`); no `jail_stop_interrupted` |
| Host with the agent stopped | the jail still in `jls` (its `jail -r` was killed), no epairs |
| `agent.db` (a copy taken with the agent stopped, read on merrick, then deleted) | desired `stopped`, observed **`stopping`**, detail NULL |
| `service skep_agent start` | `jail_stop_completing`, then `jail_stop_completed` 30 s later |
| GET after that start | `stopped` / `stopped` / null / "…was completed at agent start"; `jls` empty, no epairs |

## D — induced failure (schg)

As B, but once the stop task had rewritten the jail's `jail.conf` (mtime
changed, `jail -r` running), `chflags schg` on that file, then the kill.

| Check | Result |
|---|---|
| Log at start | `jail_stop_completing`, then **ERROR** `jail_stop_interrupted`: `rename …/jail.conf.tmp …/jail.conf: operation not permitted` |
| GET, 1st and 2nd read (6 s after the kill) | `state: running`, `desired_state: stopped`, `jid: 9`, `state_detail`: "a stop interrupted by an agent restart could not be completed at agent start: rename …: operation not permitted; stop it again" |
| List | the same state and detail |
| A stop task with the flag still set | `failed` (`task_failed`) |
| GET after it | `running` / `stopped` / 9 / "the last stop failed: rename …: operation not permitted" |
| After `chflags noschg`: stop | `succeeded`; GET `stopped`, `state_detail` **null** (a successful stop clears it) |

Before this fix round the same failure read `running` with `state_detail: null`
([jail-stop-completion-failure-20261123.md](jail-stop-completion-failure-20261123.md)).

## End state

Container and template deleted; `agent.json` restored (sha256 matches, backup
removed); `bridge1` destroyed; vtnet1 down as before; `jls` empty; only
`zroot/skep{,/jail,/templates,/vm}`; `/var/run/skep/jails` empty; no file with
`schg` under `/var/run/skep` or `/var/db/skep`; agent restarted; health ok;
langrishe on eae2ab3.

Note: the jail's DHCP lease was not shown by the driver (`ifconfig skep0
inet` printed nothing right after the start); the NIC and its epair were
there (`host_interface: epair0a`). The lease does not bear on these cases.
