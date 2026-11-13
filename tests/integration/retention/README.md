# Retention integration test

`retention-test` checks retention ([ADR 0007](../../../docs/decisions/0007-retention.md))
on a real FreeBSD host, with both programs running under their rc.d
scripts. It runs as root. Neither it nor the shipped binaries it starts
contain anything test-only.

It is built not to disturb the host:

- It works in a data directory and a run directory **of its own**, which
  it creates and which must not exist beforehand. An installed Skep in
  `/var/db/skep` is not touched; stop its services first, because the
  rc.d scripts are the same.
- It does **not change `rc.conf`**: the settings an operator would put
  there are passed to the rc.d scripts in their environment.
- It does **not change the clock**. The history that retention acts on is
  written into the two databases, dated in past months, before the
  services start. The services then run with the real time.
- It stops the services and removes its two directories at the end
  (`-keep` leaves the data directory for inspection).

The file system is the one the data directory is on; `-fs` says which one
that must be, and the test refuses to start on another.

| Step | What it checks |
|---|---|
| Seed | `agent.db` (root) with audit entries five months and one month old and three finished tasks (200, 120, and 10 days old); `api.db` (owned by `skep`) with access audit entries of the same two months |
| Start | `skep_agent` and `skep_api` start through the rc.d scripts |
| Agent task | One `maintenance.retention` task ran after start and succeeded; the two old tasks and their six events are gone, the recent one is left, no event without a task; two segments sealed with the expected ID ranges; no live audit entry removed (the agent's default); the `finished` audit entry records the cutoff, the counts, and both seals |
| Agent archive | `agent/` and `agent/audit-archive` are `root:wheel 0700`, the files `0400`, no temporary file left; each seal is in `/var/log/messages` as a `daemon.notice` line with its hash; the `skep` account cannot list the directory |
| Agent commands | `skep-agent audit verify` and `audit export` while the agent runs; the health check `audit_archive` is `ok` |
| API | `skep-api` recorded an `audit.archive` run; two segments; the entries older than 90 days are gone from the live table and the younger month is still there; `api/audit-archive` is `skep:skep 0700`, files `0400`; its seal reached syslog from the unprivileged account; its segments carry the agent's node ID; `skep-api audit verify` and `audit export`, started as root, leave nothing behind that is not owned by `skep`; the removed entries are read back from the archive |
| Restart | After `restart`, a second task verifies both segments, seals nothing, removes nothing; the files are byte for byte unchanged |
| Altered segment | One byte of a segment is changed and the agent restarted: the task fails with `audit_archive_invalid`, its audit entry says so, the live entries are untouched, health is `degraded`, `audit verify` names the segment and exits 1 |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api \
    bin/freebsd-amd64-test/retention-test hearst:skep-m4c/

# On hearst. The account and the rc.d scripts are installed as described
# in docs/installation/freebsd-service.md; the services are stopped.
sudo ~/skep-m4c/retention-test -fs ufs -data-dir /var/tmp/skep/m4c-ufs \
    -agent-bin /home/aba/skep-m4c/skep-agent -api-bin /home/aba/skep-m4c/skep-api
```

For ZFS, point `-data-dir` at a new directory inside a dataset and pass
`-fs zfs`. On hearst the only pool that may be used is the file-backed
test pool `skeptest`:

```sh
sudo zfs create skeptest/skep-m4c
sudo ~/skep-m4c/retention-test -fs zfs -data-dir /skeptest/skep-m4c/data \
    -agent-bin /home/aba/skep-m4c/skep-agent -api-bin /home/aba/skep-m4c/skep-api
sudo zfs destroy skeptest/skep-m4c
```

## Result on FreeBSD 15.1-RELEASE-p6 (hearst)

Binaries at `f8c1788`, rc.d scripts as installed for Milestone 4
(identical to `packaging/rc.d`). Account `skep` uid 1002, gid 1002. The
installed services were stopped and `rc.conf` has no `skep_*` entries.

| File system | Result |
|---|---|
| UFS (`/var/tmp/skep/m4c-ufs` on `/dev/da0s1a`) | **PASS**, 57 checks (2026-11-12 23:25 local, 2026-11-13 04:25 UTC) |
| ZFS (a dataset in `skeptest`) | **Pending FreeBSD validation**: not run. The `zfs` kernel module was not loaded and `skeptest` was not imported on hearst, and loading a kernel module there needs Andy's go-ahead |

The UFS run, shortened (node ID and hashes abbreviated, long lines cut):

```
== seed /var/tmp/skep/m4c-ufs with old history; nothing here changes the clock
       Filesystem   Type 512-blocks     Used     Avail Capacity  Mounted on
       /dev/da0s1a  ufs   251847480 31705920 199993768    14%    /
ok     the data directory is on /dev/da0s1a ufs (want ufs)
== start skep_agent and skep_api through their rc.d scripts
       $ skep_agent start -> exit 0: Starting skep_agent.
       $ skep_api start -> exit 0: Starting skep_api.
ok     the agent answers on /var/run/skep-retention-test/agent.sock
== the agent's retention task: tasks pruned, audit segments sealed, nothing removed
ok     one maintenance.retention task ran after start (1 found)
ok     it succeeded: status=succeeded error_code=""
ok     tasks: 2 removed with 6 events at 90 days (want 2, 6, 90)
ok     of the seeded tasks only the recent one is left (1 left, err=<nil>)
ok     no events without a task (0)
ok     two segments sealed, for 2026-06 and 2026-10: [{Month:2026-06 FirstID:1 LastID:4 Entries:4 Hash:51258774a497… File:audit-<node>-2026-06-1-4.jsonl.gz} {Month:2026-10 FirstID:5 LastID:6 Entries:2 Hash:d33923e2c800… File:au...
ok     their ID ranges are 1-4 and 5-6 (1-4, 5-6)
ok     no live audit entry was removed: the agent's default (0)
ok     all 6 sealed entries are still in the live table (6)
ok     the finished audit entry records the cutoff, the counts, and the seals: {"audit_entries_removed":0,"audit_segments_sealed":[{"entries":4,"first_id":1,"last_id":4,"month":"2026-06","sha256":"51258774a497…"},{"entries":2,"...
== the agent's archive directory: root only, written with sync and rename on ufs
ok     /var/tmp/skep/m4c-ufs/agent is 0:0 mode 0700 (want 0:0 0700)
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive is 0:0 mode 0700 (want 0:0 0700)
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive/audit-<node>-2026-06-1-4.jsonl.gz is 0:0 mode 0400 (want 0:0 0400)
ok     audit-<node>-2026-06-1-4.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive/audit-<node>-2026-10-5-6.jsonl.gz is 0:0 mode 0400 (want 0:0 0400)
ok     audit-<node>-2026-10-5-6.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive holds 2 files, want 2: [audit-<node>-2026-06-1-4.jsonl.gz audit-<node>-2026-10-5-6.jsonl.gz]
ok     segment file audit-<node>-2026-06-1-4.jsonl.gz exists
ok     the seal of 2026-06 is in syslog at notice: /var/log/messages: Nov 12 23:25:23 hearst skep-agent[4205]: audit segment sealed: log=agent node=<node> month=2026-06 first_id=1 last_id=4 entries=4 sha256=51258774a497… previo...
ok     segment file audit-<node>-2026-10-5-6.jsonl.gz exists
ok     the seal of 2026-10 is in syslog at notice: /var/log/messages: Nov 12 23:25:23 hearst skep-agent[4205]: audit segment sealed: log=agent node=<node> month=2026-10 first_id=5 last_id=6 entries=2 sha256=d33923e2c800… previo...
ok     uid 1002 (skep) cannot read the agent's archive: refused: open /var/tmp/skep/m4c-ufs/agent/audit-archive: permission denied
== skep-agent audit verify and export, while the agent runs
ok     audit verify: exit 0
ok     audit export prints the 6 archived entries (exit 0, 6 lines)
ok     health check audit_archive: {Status:ok Error:} (err=<nil>)
== skep-api: its own archive, as skep; entries older than 90 days removed after sealing
ok     skep-api recorded an archive run in its access audit log
ok     the run succeeded and removed the 5 entries of 2026-06
ok     2 segments; 2026-06 gone from the live table (0 left); 2026-10 still live (2 of 2)
ok     /var/tmp/skep/m4c-ufs/api is 1002:1002 mode 0700 (want 1002:1002 0700)
ok     /var/tmp/skep/m4c-ufs/api/audit-archive is 1002:1002 mode 0700 (want 1002:1002 0700)
ok     /var/tmp/skep/m4c-ufs/api/audit-archive/audit-<node>-2026-06-1-5.jsonl.gz is 1002:1002 mode 0400 (want 1002:1002 0400)
ok     audit-<node>-2026-06-1-5.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/api/audit-archive/audit-<node>-2026-10-6-7.jsonl.gz is 1002:1002 mode 0400 (want 1002:1002 0400)
ok     audit-<node>-2026-10-6-7.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/api/audit-archive holds 2 files, want 2: [audit-<node>-2026-06-1-5.jsonl.gz audit-<node>-2026-10-6-7.jsonl.gz]
ok     skep-api's seal is in syslog, sent as an unprivileged user: /var/log/messages: Nov 12 23:25:23 hearst skep-api[4240]: audit segment sealed: log=api node=<node> month=2026-10 first_id=6 last_id=7 entries=2 sha256=c72613a8...
ok     skep-api's segments carry the node ID it got from the agent (<node>)
ok     skep-api audit verify, started as root, gives up root and verifies: exit 0
ok     the removed entries of 2026-06 are read back from the archive (5 lines, exit 0)
ok     nothing under /var/tmp/skep/m4c-ufs/api belongs to anyone but skep after the root-started commands (0)
== restart the agent: a second run verifies the archive and seals nothing new
       $ skep_agent restart -> exit 0: Stopping skep_agent. | Waiting for PIDS: 4203. | Starting skep_agent.
ok     a second retention task ran after the restart (2 found)
ok     it verified the 2 segments, sealed none, and removed nothing: status=succeeded
ok     the segment files are byte for byte what they were before the restart
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive is 0:0 mode 0700 (want 0:0 0700)
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive/audit-<node>-2026-06-1-4.jsonl.gz is 0:0 mode 0400 (want 0:0 0400)
ok     audit-<node>-2026-06-1-4.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive/audit-<node>-2026-10-5-6.jsonl.gz is 0:0 mode 0400 (want 0:0 0400)
ok     audit-<node>-2026-10-5-6.jsonl.gz is a segment and not a leftover temporary file
ok     /var/tmp/skep/m4c-ufs/agent/audit-archive holds 2 files, want 2: [audit-<node>-2026-06-1-4.jsonl.gz audit-<node>-2026-10-5-6.jsonl.gz]
== alter a segment and restart: the task fails, nothing is removed, health is degraded
       $ skep_agent restart -> exit 0: Stopping skep_agent. | Waiting for PIDS: 4287. | Starting skep_agent.
ok     a third retention task ran (3 found)
ok     it failed with audit_archive_invalid: status=failed error_code="audit_archive_invalid"
ok     its finished audit entry says so: {"audit_entries_removed":0,"audit_segments_verified":1,"error_code":"audit_archive_invalid","events_removed":0,"tasks_cutoff":"2026-08-15T04:25:23Z","tasks_removed":0,"tasks_retention_da...
ok     the live entries are untouched (6 of 6)
ok     health is degraded: {Status:degraded Error:the audit archive failed verification; no audit entries are removed until it is repaired}
ok     audit verify names the altered segment and exits 1
== stop the services
       $ skep_api stop -> exit 0: Stopping skep_api. | Waiting for PIDS: 4238.
       $ skep_agent stop -> exit 0: Stopping skep_agent. | Waiting for PIDS: 4331.
PASS, 57 checks
```

Afterwards the test's directories were gone, the services were stopped as
before, and nothing else on the host had changed.
