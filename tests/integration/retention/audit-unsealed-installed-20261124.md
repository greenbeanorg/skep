# `skep-agent audit export --unsealed`, installed build (2026-11-24)

Package built from main eadd643 (PR #88 and #90 merged), installed with
`pkg install -y -f` and `skep-setup upgrade -yes -keep 50` (exit 0) on
langrishe and then smith (FreeBSD 15.1). The scratch helper used in E (an
online `VACUUM INTO` copy and a held connection) and every scratch file
were removed afterwards.

| Check | Host | Result |
|---|---|---|
| As uid 1001 | langrishe | exit 1, "must run as root (running as uid 1001)", nothing on stdout |
| Root, agent running | langrishe | exit 0, 1012 JSON lines; stderr count line; database directory listing identical; agent pid unchanged |
| Root, agent stopped | langrishe | exit 1, "no service has this database open ... so that it leaves no files behind"; listing identical; agent restarted, health ok |
| `audit verify`, `audit export`, agent running | langrishe | both exit 0, over an empty archive (no segment sealed yet on this VM) |
| Archiving off, scratch data dir, held connection | langrishe | exit 0, 1012 of 1012 entries, no `audit-archive` directory created |
| Root, agent running | smith | exit 0, 742 JSON lines, stderr count line |

## langrishe output (A, B, D, E)

```
== A: as aba (uid 1001)
exit 1; stdout bytes 0; stderr: skep-agent: audit export --unsealed must run as root (running as uid 1001)
== B: root, agent running
exit 0
stdout lines 1012; first: {"id":1,"occurred_at":"2026-11-22T23:40:48.702Z","actor_id":"system","actor_name":"system","peer_uid":0,"action":"discovery.refresh","resource_type":"node","res
last line: {"id":1012,"occurred_at":"2026-11-24T07:10:11.364Z","actor_id":"system","actor_name":"system","peer_uid":0,"action":"discovery.refresh","resource_type":"node","resource_id":"4ae4a218-b2b0-477d-b8c5-f3
stderr: 1012 entries exported; they are not sealed yet, so the archive's hash verification does not cover them
non-JSON stdout lines: 0
listing before:
-rw-------  1 root wheel 1650688 Nov 24 02:10:11 2026 agent.db
-rw-------  1 root wheel   32768 Nov 24 02:10:11 2026 agent.db-shm
-rw-------  1 root wheel  350232 Nov 24 02:10:11 2026 agent.db-wal
drwx------  2 root wheel       2 Nov 22 21:33:02 2026 vm
listing after:
-rw-------  1 root wheel 1650688 Nov 24 02:10:11 2026 agent.db
-rw-------  1 root wheel   32768 Nov 24 02:10:11 2026 agent.db-shm
-rw-------  1 root wheel  350232 Nov 24 02:10:11 2026 agent.db-wal
drwx------  2 root wheel       2 Nov 22 21:33:02 2026 vm
LISTING IDENTICAL
agent pid before [43614 ] after [43614 ]
PID UNCHANGED
== D: audit verify / export (sealed), agent running
0 sealed segments in /var/db/skep/agent/audit-archive: 0 ok, 0 missing, 0 invalid
verify exit 0
export exit 0 stdout lines 0 stderr: 
== E: archiving off, scratch data dir, scratch connection held open (1c live)
journal_mode wal
holding /var/tmp/skep-j3/data/agent/agent.db rows 1012
exit 0
stdout lines 1012; stderr: 1012 entries exported; they are not sealed yet, so the archive's hash verification does not cover them
first id: {"id":1,"occurred_at  last id: {"id":1012,"occurred
total 5
drwx------  2 root wheel       5 Nov 24 02:10 .
drwxr-xr-x  3 root wheel       3 Nov 24 02:10 ..
-rw-r--r--  1 root wheel 1601536 Nov 24 02:10 agent.db
-rw-r--r--  1 root wheel   32768 Nov 24 02:10 agent.db-shm
-rw-r--r--  1 root wheel       0 Nov 24 02:10 agent.db-wal
holder 43887 stopped
```

## langrishe output (C, agent stopped)

```
Stopping skep_agent.
Waiting for PIDS: 43612.
stop exit 0
skep_agent is not running.
no skep-agent process
exit 1
stdout lines 0; stderr: skep-agent: database: no service has this database open: /var/db/skep/agent/agent.db-wal does not exist; --unsealed reads the database only while the agent runs, so that it leaves no files behind
listing before:
-rw-------  1 root wheel 1658880 Nov 24 02:10:35 2026 agent.db
drwx------  2 root wheel       2 Nov 22 21:33:02 2026 vm
listing after:
-rw-------  1 root wheel 1658880 Nov 24 02:10:35 2026 agent.db
drwx------  2 root wheel       2 Nov 22 21:33:02 2026 vm
LISTING IDENTICAL
Starting skep_agent.
start exit 0
skep_agent is running as pid 44059 (supervisor pid 44057).
```

## Scripts

```sh
#!/bin/sh
# Job 3 on langrishe, 2026-11-24, run as root with the installed binary.
B=/usr/local/sbin/skep-agent
D=/var/db/skep/agent
O=/var/tmp/skep-j3; mkdir -m 700 -p $O
lst() { ls -lT $D | sed 1d; }
apid() { pgrep -x skep-agent | tr '\n' ' '; }

echo "== B: root, agent running"
p1=$(apid); lst > $O/b1
$B audit export --unsealed > $O/b.out 2> $O/b.err; echo "exit $?"
lst > $O/b2; p2=$(apid)
echo "stdout lines $(wc -l < $O/b.out | tr -d ' '); first: $(head -c 160 $O/b.out)"
echo "last line: $(tail -n 1 $O/b.out | head -c 200)"
echo "stderr: $(cat $O/b.err)"
echo "non-JSON stdout lines: $(grep -vc '^{"id":' $O/b.out)"
echo "listing before:"; cat $O/b1; echo "listing after:"; cat $O/b2
cmp -s $O/b1 $O/b2 && echo "LISTING IDENTICAL" || echo "LISTING DIFFERS"
echo "agent pid before [$p1] after [$p2]"; [ "$p1" = "$p2" ] && echo "PID UNCHANGED" || echo "PID CHANGED"

echo "== D: audit verify / export (sealed), agent running"
$B audit verify; echo "verify exit $?"
$B audit export > $O/d.out 2> $O/d.err; echo "export exit $? stdout lines $(wc -l < $O/d.out | tr -d ' ') stderr: $(cat $O/d.err)"

echo "== E: archiving off, scratch data dir, scratch connection held open (1c live)"
S=$O/data; mkdir -m 700 -p $S/agent
$O/sqlhelper backup $D/agent.db $S/agent/agent.db && $O/sqlhelper setmode $S/agent/agent.db wal
cat > $O/agent-noarchive.json <<'J'
{
  "storage": {"pools": ["zroot"], "directories": [], "dataset_root": "skep", "media_dir": "/var/db/skep/media"},
  "network": {"bridges": [], "management_interfaces": ["vtnet0"]},
  "retention": {"audit_archive": false}
}
J
timeout 60 $O/sqlhelper hold $S/agent/agent.db 30 & HP=$!
sleep 2
$B audit export --unsealed -config $O/agent-noarchive.json -data-dir $S > $O/e.out 2> $O/e.err; echo "exit $?"
echo "stdout lines $(wc -l < $O/e.out | tr -d ' '); stderr: $(cat $O/e.err)"
echo "first id: $(head -n1 $O/e.out | cut -c1-20)  last id: $(tail -n1 $O/e.out | cut -c1-20)"
ls -la $S/agent
kill $HP 2>/dev/null; wait $HP 2>/dev/null; echo "holder $HP stopped"
```

```sh
#!/bin/sh
# Job 3 part C on langrishe: agent stopped.
B=/usr/local/sbin/skep-agent
D=/var/db/skep/agent
O=/var/tmp/skep-j3
lst() { ls -lT $D | sed 1d; }
service skep_agent stop; echo "stop exit $?"
service skep_agent status; pgrep -lx skep-agent || echo "no skep-agent process"
lst > $O/c1
$B audit export --unsealed > $O/c.out 2> $O/c.err; echo "exit $?"
lst > $O/c2
echo "stdout lines $(wc -l < $O/c.out | tr -d ' '); stderr: $(cat $O/c.err)"
echo "listing before:"; cat $O/c1; echo "listing after:"; cat $O/c2
cmp -s $O/c1 $O/c2 && echo "LISTING IDENTICAL" || echo "LISTING DIFFERS"
service skep_agent start; echo "start exit $?"
sleep 5; service skep_agent status
```
