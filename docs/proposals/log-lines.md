# Two cosmetic log items: causes, no code change

Status: findings. Neither item has a fix that is under 30 lines and leaves
behaviour unchanged, so nothing was changed.

## Two records on one `daemon.log` line

**Cause.** `daemon(8) -S` reads up to 4095 bytes from the program's pipe,
finds the last newline with `memrchr`, and sends everything before it as one
`syslog()` message. If two records are in the pipe when it reads, that
message holds both. `syslogd` then replaces the inner newline with a space.
Skep writes each record with a single `Write`, so its logger is not the cause.

**Why no fix here.** A fix means Skep sending records to syslog itself: a new
log handler, a config key and rc.d changes. That is over the limit and changes
logging behaviour. It is already in the backlog for after 0.1 (Andy,
2026-11-22, option B of
[the analysis](../../tests/integration/rcd/daemon-log-joined-records.md)).

## `route: ... File exists` at jail start

**Cause.** The jail's stock `/etc/rc.d/routing` always adds the loopback
routes, but the kernel created them when `lo0` got its addresses, so
`route add` reports `EEXIST`. The existing route is correct.

**Why no fix here.** No `rc.conf` setting skips those routes. The only
Skep-side changes are filtering the jail's console log, which Skep never
edits, or changing the jail's `/etc/rc.d/routing`. Both change behaviour, and
Andy decided on documentation only (2026-11-22,
[the analysis](../../tests/integration/jails/results/route-file-exists-at-jail-start.md)).
