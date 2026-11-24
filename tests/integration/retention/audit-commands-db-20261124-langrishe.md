# What `audit verify` and `audit export` do to the database (langrishe, 2026-11-24)

Run as root on langrishe (FreeBSD 15.1, installed build 1d95b4f, whose
audit commands are unchanged on main bee1404), on scratch copies under
`/var/tmp`, never on the live databases. The copies were taken online with
`VACUUM INTO` through a read-only connection and set to WAL mode. "Service
running" is imitated by a second read-write connection held open; "unclean
stop" by a process that committed into the WAL with auto-checkpoint off and
exited without closing. The scratch helper and directories were removed
afterwards.

## Findings (both `skep-agent` and `skep-api`, identical)

| Case | Result |
|---|---|
| S1 stopped, clean WAL database | exit 0; `agent.db` sha256 and mtime unchanged; no `-wal`/`-shm` left |
| S2 running (another connection open) | exit 0; `.db`, `-wal`, `-shm` sha256 and mtimes all unchanged |
| S3 missing database | exit 1 "there is no database at ..."; nothing created |
| S4 schema version 2 (before the segment index) | exit 1 "no audit segment index yet"; still version 2 afterwards (not migrated) |
| S5 rollback-journal mode, stopped | exit 0; **header switched from rollback to WAL, file rewritten** |
| S6 frames left in `-wal`, stopped | exit 0; **frames checkpointed into the `.db` file, `-wal`/`-shm` removed** |

In S4 the `-wal`/`-shm` in the listing were left by the helper's read-only
version read, not by the audit command (SQLite's known `mode=ro` behaviour).

From the code (`internal/auditcmd.Open`, `internal/database.Open`): a
`stat` refuses a missing file before anything is opened; the schema version
is only read; `database.Open` sets `journal_mode(WAL)` (S5) and, as the last
connection, SQLite checkpoints and removes the WAL on close (S1, S6).
`auditarchive.Verify` and `Export` only run SELECTs and read segment files.

## Script

```sh
#!/bin/sh
# Job 2 live check, 2026-11-24, run as root on langrishe. Scratch copies only.
H=/var/tmp/skep-job2/sqlhelper
A=/var/tmp/skep-job2/a        # agent data dir (root)
P=/var/tmp/skep-job2-api      # api data dir (skep)
lst() { echo "  -- listing $1"; ls -lT "$1" | sed 1d | sed 's/^/  /'; for f in "$1"/*.db*; do [ -f "$f" ] && echo "  sha256 $(basename $f) $(sha256 -q $f)"; done; }
hdr() { $H header "$1" | sed 's/^/  /'; }

agent() { echo "  \$ skep-agent audit $*"; /usr/local/sbin/skep-agent audit "$@" -data-dir $A >/tmp/skep-job2.out 2>&1; echo "  exit $? ; $(wc -l </tmp/skep-job2.out) lines; $(head -c 300 /tmp/skep-job2.out | tr '\n' '|')"; }
api() { echo "  \$ skep-api audit $* (as root, drops to skep)"; /usr/local/sbin/skep-api audit "$@" -data-dir $P >/tmp/skep-job2.out 2>&1; echo "  exit $? ; $(wc -l </tmp/skep-job2.out) lines; $(head -c 300 /tmp/skep-job2.out | tr '\n' '|')"; }

fresh_agent() { rm -f $A/agent/agent.db*; $H backup /var/db/skep/agent/agent.db $A/agent/agent.db && $H setmode $A/agent/agent.db wal >/dev/null; }
fresh_api() { rm -f $P/api/api.db*; su -m skep -c "$H backup /var/db/skep/api/api.db $P/api/api.db && $H setmode $P/api/api.db wal" >/dev/null; }

echo "== setup"; ls -la /var/db/skep/agent /var/db/skep/api | sed 's/^/  /'

for who in agent api; do
  if [ $who = agent ]; then D=$A/agent; DB=$D/agent.db; RUN=agent; FRESH=fresh_agent; AS=""; else D=$P/api; DB=$D/api.db; RUN=api; FRESH=fresh_api; AS="su -m skep -c"; fi
  echo; echo "######## $who"
  echo "== S1 service stopped (no other connection), WAL database, no -wal/-shm"
  $FRESH; lst $D; hdr $DB; $RUN verify; $RUN export; lst $D; hdr $DB

  echo "== S2 service running (another read-write connection held open)"
  $FRESH
  if [ -z "$AS" ]; then timeout 60 $H hold $DB 40 & HP=$!; else timeout 60 su -m skep -c "$H hold $DB 40" & HP=$!; fi
  sleep 2; lst $D; $RUN verify; $RUN export; lst $D; hdr $DB
  kill $HP 2>/dev/null; wait $HP 2>/dev/null; echo "  holder $HP stopped"

  echo "== S3 missing database"
  mv $DB $DB.aside; rm -f $DB-wal $DB-shm; lst $D; $RUN verify; lst $D; mv $DB.aside $DB

  echo "== S4 schema older than the segment index (version lowered to 2 on the copy)"
  $FRESH; if [ -z "$AS" ]; then $H version $DB 2; else su -m skep -c "$H version $DB 2"; fi
  $RUN verify; $H version $DB | sed 's/^/  after: /'; lst $D

  echo "== S5 database in rollback-journal mode, service stopped"
  $FRESH; if [ -z "$AS" ]; then $H setmode $DB delete; else su -m skep -c "$H setmode $DB delete"; fi
  hdr $DB; lst $D; $RUN verify; hdr $DB; lst $D

  echo "== S6 service stopped uncleanly: committed frames left in -wal"
  $FRESH; if [ -z "$AS" ]; then $H dirty $DB; else su -m skep -c "$H dirty $DB"; fi
  lst $D; $RUN verify; lst $D
done
rm -f /tmp/skep-job2.out
```

## Output

```
== setup
  /var/db/skep/agent:
  total 1086
  drwx------  3 root wheel       6 Nov 23 21:26 .
  drwxr-xr-x  7 root wheel       7 Nov 22 19:09 ..
  -rw-------  1 root wheel 1646592 Nov 23 21:26 agent.db
  -rw-------  1 root wheel   32768 Nov 23 21:26 agent.db-shm
  -rw-------  1 root wheel  366712 Nov 23 21:26 agent.db-wal
  drwx------  2 root wheel       2 Nov 22 21:33 vm
  
  /var/db/skep/api:
  total 182
  drwx------  3 skep skep       6 Nov 23 21:25 .
  drwxr-xr-x  7 root wheel      7 Nov 22 19:09 ..
  -rw-------  1 skep skep  229376 Nov 23 21:25 api.db
  -rw-------  1 skep skep   32768 Nov 23 21:25 api.db-shm
  -rw-------  1 skep skep   32992 Nov 23 21:25 api.db-wal
  drwx------  2 skep skep       4 Nov 22 18:40 tls

######## agent
== S1 service stopped (no other connection), WAL database, no -wal/-shm
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db
  sha256 agent.db 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  header write=wal read=wal
  $ skep-agent audit verify
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2/a/agent/audit-archive: 0 ok, 0 missing, 0 invalid|
  $ skep-agent audit export
  exit 0 ;        0 lines; 
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db
  sha256 agent.db 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  header write=wal read=wal
== S2 service running (another read-write connection held open)
holding /var/tmp/skep-job2/a/agent/agent.db rows 1008
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db
  -rw-r--r--  1 root wheel   32768 Nov 24 02:00:32 2026 agent.db-shm
  -rw-r--r--  1 root wheel       0 Nov 24 02:00:32 2026 agent.db-wal
  sha256 agent.db 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  sha256 agent.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 agent.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  $ skep-agent audit verify
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2/a/agent/audit-archive: 0 ok, 0 missing, 0 invalid|
  $ skep-agent audit export
  exit 0 ;        0 lines; 
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db
  -rw-r--r--  1 root wheel   32768 Nov 24 02:00:32 2026 agent.db-shm
  -rw-r--r--  1 root wheel       0 Nov 24 02:00:32 2026 agent.db-wal
  sha256 agent.db 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  sha256 agent.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 agent.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  header write=wal read=wal
  holder 42293 stopped
== S3 missing database
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db.aside
  sha256 agent.db.aside 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  $ skep-agent audit verify
  exit 1 ;        1 lines; skep-agent: there is no database at /var/tmp/skep-job2/a/agent/agent.db|
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:32 2026 agent.db.aside
  sha256 agent.db.aside 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
== S4 schema older than the segment index (version lowered to 2 on the copy)
schema version 2
  $ skep-agent audit verify
  exit 1 ;        1 lines; skep-agent: this database has no audit segment index yet: the service has not been started since retention was introduced, so nothing is archived|
  after: schema version 2
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:34 2026 agent.db
  -rw-r--r--  1 root wheel   32768 Nov 24 02:00:34 2026 agent.db-shm
  -rw-r--r--  1 root wheel       0 Nov 24 02:00:34 2026 agent.db-wal
  sha256 agent.db eaefcb2b8cdfe92c5f63cc127f89d482c0410521c3850c935b48efec79464da1
  sha256 agent.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 agent.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
== S5 database in rollback-journal mode, service stopped
journal_mode delete
  header write=rollback read=rollback
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:34 2026 agent.db
  sha256 agent.db 597d49b0152282fc33af1afe08b2d3f09a0861fdc8c767f5d81c15d6fccc16d3
  $ skep-agent audit verify
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2/a/agent/audit-archive: 0 ok, 0 missing, 0 invalid|
  header write=wal read=wal
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:34 2026 agent.db
  sha256 agent.db 979f67a01e7f86e3fed9b0d13ae7c61c6c887ea95b314db0b18a41fbbf3dc897
== S6 service stopped uncleanly: committed frames left in -wal
committed into the WAL; exiting without close
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1593344 Nov 24 02:00:34 2026 agent.db
  -rw-r--r--  1 root wheel   32768 Nov 24 02:00:34 2026 agent.db-shm
  -rw-r--r--  1 root wheel   16512 Nov 24 02:00:34 2026 agent.db-wal
  sha256 agent.db 1eb8fb982646b4d2e79e7af04cbb88502cc03ff5d6d74690c132114eea237a8a
  sha256 agent.db-shm 9576bc33c85a60be26c8ba193b7fd4530638159005da662387b779b6e2e21675
  sha256 agent.db-wal 8c0cb8cf554e40e758c9189489f21388ce888487c2ee380d102c652e57aab6da
  $ skep-agent audit verify
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2/a/agent/audit-archive: 0 ok, 0 missing, 0 invalid|
  -- listing /var/tmp/skep-job2/a/agent
  -rw-r--r--  1 root wheel 1597440 Nov 24 02:00:35 2026 agent.db
  sha256 agent.db 620dbe1c9f2f97452365561624c31e4f6df80d6b2715b28084b94626bf143807

######## api
== S1 service stopped (no other connection), WAL database, no -wal/-shm
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db
  sha256 api.db 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  header write=wal read=wal
  $ skep-api audit verify (as root, drops to skep)
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2-api/api/audit-archive: 0 ok, 0 missing, 0 invalid|
  $ skep-api audit export (as root, drops to skep)
  exit 0 ;        0 lines; 
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db
  sha256 api.db 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  header write=wal read=wal
== S2 service running (another read-write connection held open)
holding /var/tmp/skep-job2-api/api/api.db rows 466
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db
  -rw-r--r--  1 skep skep  32768 Nov 24 02:00:35 2026 api.db-shm
  -rw-r--r--  1 skep skep      0 Nov 24 02:00:35 2026 api.db-wal
  sha256 api.db 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  sha256 api.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 api.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  $ skep-api audit verify (as root, drops to skep)
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2-api/api/audit-archive: 0 ok, 0 missing, 0 invalid|
  $ skep-api audit export (as root, drops to skep)
  exit 0 ;        0 lines; 
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db
  -rw-r--r--  1 skep skep  32768 Nov 24 02:00:35 2026 api.db-shm
  -rw-r--r--  1 skep skep      0 Nov 24 02:00:35 2026 api.db-wal
  sha256 api.db 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  sha256 api.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 api.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
  header write=wal read=wal
  holder 42523 stopped
== S3 missing database
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db.aside
  sha256 api.db.aside 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  $ skep-api audit verify (as root, drops to skep)
  exit 1 ;        1 lines; skep-api: there is no database at /var/tmp/skep-job2-api/api/api.db|
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:35 2026 api.db.aside
  sha256 api.db.aside 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
== S4 schema older than the segment index (version lowered to 2 on the copy)
schema version 2
  $ skep-api audit verify (as root, drops to skep)
  exit 1 ;        1 lines; skep-api: this database has no audit segment index yet: the service has not been started since retention was introduced, so nothing is archived|
  after: schema version 2
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:37 2026 api.db
  -rw-r--r--  1 skep skep  32768 Nov 24 02:00:37 2026 api.db-shm
  -rw-r--r--  1 skep skep      0 Nov 24 02:00:37 2026 api.db-wal
  sha256 api.db 439ba1e85e77628a4677671b69fc0f655704faa819f352a345144a19baeb574c
  sha256 api.db-shm fd4c9fda9cd3f9ae7c962b0ddf37232294d55580e1aa165aa06129b8549389eb
  sha256 api.db-wal e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
== S5 database in rollback-journal mode, service stopped
journal_mode delete
  header write=rollback read=rollback
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:37 2026 api.db
  sha256 api.db ac72cbc017ee8a95c2e0305b6093111a744e868141c59fad42619a1f72828748
  $ skep-api audit verify (as root, drops to skep)
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2-api/api/audit-archive: 0 ok, 0 missing, 0 invalid|
  header write=wal read=wal
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:37 2026 api.db
  sha256 api.db 4503c26720781c59b91c7c48dcd1978ddb12b70549d45acb8dcc492bdfb147cf
== S6 service stopped uncleanly: committed frames left in -wal
committed into the WAL; exiting without close
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 229376 Nov 24 02:00:37 2026 api.db
  -rw-r--r--  1 skep skep  32768 Nov 24 02:00:37 2026 api.db-shm
  -rw-r--r--  1 skep skep  16512 Nov 24 02:00:37 2026 api.db-wal
  sha256 api.db 09c5114f7a8fef8ba604f63ac2a7ff2b985b0eea48f48455292ab23cb8467a48
  sha256 api.db-shm aba932348d11e57784381a279d86bf6f7235321cdf350e06db0280de89009594
  sha256 api.db-wal 97be4450b6c61474b61384bd8ed13bf9135f015fc593f76246a48f4becc27ac6
  $ skep-api audit verify (as root, drops to skep)
  exit 0 ;        1 lines; 0 sealed segments in /var/tmp/skep-job2-api/api/audit-archive: 0 ok, 0 missing, 0 invalid|
  -- listing /var/tmp/skep-job2-api/api
  -rw-r--r--  1 skep skep 233472 Nov 24 02:00:37 2026 api.db
  sha256 api.db 8fca6f7db827af13d90e41626f75732eed1496b9294710cdce0a5c4faf242fd6
```
