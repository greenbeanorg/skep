# Two JSON records on one daemon.log line (backlog item, 2026-11-22)

Backlog item from step 10 run 3 (`tests/integration/jails/results/step10-progress.md`
section 26.4 item 7; ADR 0010 amendment w backlog): two of the agent's JSON
log records once arrived on one `/var/log/daemon.log` line.

## The observation

`tests/integration/jails/results/step10-hearst-run3.txt` line 358 (hearst,
2026-11-21 11:36:19):

```
Nov 21 11:36:19 hearst skep-agent[95278]: {"time":"…11:36:19.285383473-05:00",…"event":"jail_rctl_removed",…} {"time":"…11:36:19.28950392-05:00",…"event":"container_stop_forced",…}
```

The two records were written 17 µs apart by one logger in one process. The
same pair four seconds earlier (lines 301–302, 28 µs apart) arrived on two
lines.

## Cause (confirmed by reading the FreeBSD 15.1-RELEASE-p7 source on hearst)

Neither Skep's logger nor the pipe joins anything. `log/slog`'s JSON handler
writes each record, newline included, with one `Write` under its mutex, and
every logger in skep-agent and skep-api comes from one handler on stderr
(`cmd/skep-agent/main.go:120`, `cmd/skep-api/main.go:140`). The joining
happens after the pipe:

1. **daemon(8)** (`skep_agent` and `skep_api` run under `daemon -f -S …`,
   `packaging/rc.d/`) reads the program's output in `listen_child()`
   (`/usr/src/usr.sbin/daemon/daemon.c`, lines ~690–740): one `read()` of
   up to 4095 bytes, then `memrchr(buf, '\n', pos)` finds the **last**
   newline and `do_output()` sends everything up to it as **one**
   `syslog(priority, "%.*s", len, buf)` call. When both records are in the
   pipe before daemon(8) is scheduled, that one message holds both, with a
   newline between them.
2. **syslog(3)** (`lib/libc/gen/syslog.c:273`) strips only the trailing
   newline.
3. **syslogd(8)** (`usr.sbin/syslogd/syslogd.c:924`,
   `parsemsg_remove_unsafe_characters`) replaces every other `\n` with a
   space. Result: two records on one line, separated by a space.

So it depends on scheduling, which is why it was seen once. Two related
effects of the same code: a record longer than 4095 bytes fills
daemon(8)'s buffer without a newline and is sent as two syslog messages
(two lines, neither valid JSON alone); and nothing is truncated, because
syslog(3) and syslogd accept 8192 bytes.

Not reproduced on hearst in this run (read-only access). Reproducing it is
cheap if wanted: a program under `daemon -S` that writes two short lines
with one `write(2)` each, back to back, shows them on one `daemon.log`
line most of the time.

## Options (none applied: each changes logging behaviour or the rc.d scripts)

| Option | What it does | Cost |
|---|---|---|
| A. Document it (this change) | `freebsd-service.md` says a syslog line can hold several records and how to parse them | Nothing changes for operators who grep; parsers must handle it |
| B. The agent and API log to syslog themselves | A `slog.Handler` that sends one record per `syslog(3)`-style datagram to `/var/run/log` (`log/syslog`, standard library), selected by a config key; daemon(8) without `-S` keeps only stray output | New config key and rc.d change (ask first); records keep their boundaries; the rc.d syslog variables move to the program's config |
| C. daemon(8) writes a file | `daemon -o /var/log/skep/agent.log` instead of `-S`; the file gets the bytes unchanged, one record per line | rc.d change, newsyslog(8) entry, file permissions; loses syslog forwarding |
| D. Fix daemon(8) upstream | Loop over lines with `memchr` and send one `syslog()` per line, as daemon(8) did before the 14.0 rework | A FreeBSD bug report and patch; Skep still runs on releases without it |

**Recommendation:** A now (done here), and D as a FreeBSD problem report, since
one syslog message per line is what `-S` users expect. B is the right
Skep-side fix if records must never be joined (for example for log
shipping); it is a config and rc.d change, so it waits for Andy.
