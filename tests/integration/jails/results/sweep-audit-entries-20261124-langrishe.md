# The three sweep audit entries, live — langrishe, 2026-11-24

Overnight run, job 2. langrishe: FreeBSD 15.1, Skep eadd643 (installed,
with PR #88's `skep-agent audit export --unsealed`). Before this run the
entries `container.sweep.half_started_removed`, `quarantine_removed` and
`devfs_removed` were unit-tested only.

## Method

One pkgbase test template and three NIC-less scratch jails, `j2h`, `j2q`,
`j2d`, all started through the API. Then, with `skep_agent` stopped
(`service skep_agent stop`; jails keep running) and `agent.db` copied aside
(sha256 of copy and original equal):

- `j2d`: a hand `jail -r skep-<id>`, which leaves the jail's devfs mounted
  on its root's `/dev` (the method of step 10, check 5i);
- `j2h`: `observed_state` set to `starting` (one-row UPDATE of agent.db,
  the half-started case: a running jail recorded starting);
- `j2q`: `observed_state` set to `failed` (quarantined, as SR-06 window 2).

Then `service skep_agent start`. The UPDATEs used a scratch Go helper
(langrishe has no `sqlite3`), removed afterwards.

## Result

| Entry | Count | Resource | Actor | Outcome | Container afterwards |
|---|---|---|---|---|---|
| `container.sweep.devfs_removed` (id 1071) | 1 | j2d | system | success | stopped, "removed outside Skep, or the host restarted" |
| `container.sweep.quarantine_removed` (id 1072) | 1 | j2q | system | success | stopped, "a quarantined jail ... was removed at agent start without running /etc/rc.shutdown" |
| `container.sweep.half_started_removed` (id 1073) | 1 | j2h | system | success | stopped, "a start was interrupted by an agent restart; the half-started jail was removed without running /etc/rc.shutdown" |

The agent's log has the matching `orphaned_devfs_removed`,
`jail_quarantine_removed` and `jail_start_interrupted` lines and the
summary `removed_without_exec_stop: 2`. No jail ran afterwards; the
quarantine and half-started removals left no devfs (no extra devfs entry).

The failure outcomes of the three entries (removal fails) were not induced:
they need a jail that `jail -r` cannot remove, and remain unit-tested only.

## Restoring

The copy of `agent.db` was **not** copied back: the edited rows belonged
to the test containers, which were then deleted through the API, and
restoring the copy would have erased the audit entries this job proved
(and the template and container history). The copy was deleted. The
langrishe fingerprint matched the start except the mtime of
`/var/db/skep/agent` (the agent's stop and start remove and recreate
`-wal`/`-shm`).

## Output

```
{"step": "template", "status": "succeeded"}
{"step": "created+started", "name": "j2h", "id": "a0f6b510-6f8a-4b0b-a556-6f3f47d07530", "start": "succeeded"}
{"step": "created+started", "name": "j2q", "id": "3bb0b69b-c5a5-4a87-9602-2c96957565ff", "start": "succeeded"}
{"step": "created+started", "name": "j2d", "id": "010c252d-4295-4080-9595-f9a9a57def82", "start": "succeeded"}
{"step": "audit-before", "last_id": 1066, "count": 1066}
{"step": "agent-stop", "out": "Stopping skep_agent.\nWaiting for PIDS: 44057.\nskep_agent is not running.\nno-agent"}
{"step": "copy-aside", "out": ["total 922", "-rw-------  1 root wheel 1736704 Nov 24 02:23 agent.db", "drwx------  2 root wheel       2 Nov 22 21:33 vm", "e23f8e1c77f26d50ed617571ffce1260ff5cb974c68a0ada5e1c1ee3002fdc77", "e23f8e1c77f26d50ed617571ffce1260ff5cb974c68a0ada5e1c1ee3002fdc77"]}
{"step": "hand-jail-r", "rc_out": "skep-a0f6b510-6f8a-4b0b-a556-6f3f47d07530\nskep-3bb0b69b-c5a5-4a87-9602-2c96957565ff\n1"}
{"step": "update-row", "name": "j2h", "state": "starting", "out": "rows affected 1"}
{"step": "update-row", "name": "j2q", "state": "failed", "out": "rows affected 1"}
{"step": "rows-before-start", "rows": ["010c252d-4295-4080-9595-f9a9a57def82 j2d running ", "a0f6b510-6f8a-4b0b-a556-6f3f47d07530 j2h starting ", "3bb0b69b-c5a5-4a87-9602-2c96957565ff j2q failed"]}
{"step": "agent-start", "out": "Starting skep_agent."}
{"step": "agent-back", "ok": true}
{"step": "sweep-entry", "entry": {"id": 1071, "occurred_at": "2026-11-24T07:23:13.954Z", "actor_id": "system", "actor_name": "system", "peer_uid": 0, "action": "container.sweep.devfs_removed", "resource_type": "container", "resource_id": "010c252d-4295-4080-9595-f9a9a57def82", "task_id": null, "phase": "finished", "outcome": "success", "task_status": null, "source_ip": null, "request_id": null, "details": null}}
{"step": "sweep-entry", "entry": {"id": 1072, "occurred_at": "2026-11-24T07:23:14.005Z", "actor_id": "system", "actor_name": "system", "peer_uid": 0, "action": "container.sweep.quarantine_removed", "resource_type": "container", "resource_id": "3bb0b69b-c5a5-4a87-9602-2c96957565ff", "task_id": null, "phase": "finished", "outcome": "success", "task_status": null, "source_ip": null, "request_id": null, "details": null}}
{"step": "sweep-entry", "entry": {"id": 1073, "occurred_at": "2026-11-24T07:23:14.056Z", "actor_id": "system", "actor_name": "system", "peer_uid": 0, "action": "container.sweep.half_started_removed", "resource_type": "container", "resource_id": "a0f6b510-6f8a-4b0b-a556-6f3f47d07530", "task_id": null, "phase": "finished", "outcome": "success", "task_status": null, "source_ip": null, "request_id": null, "details": null}}
{"step": "check", "action": "container.sweep.half_started_removed", "count": 1, "resource_id_ok": true, "actor": ["system"], "outcome": ["success"]}
{"step": "check", "action": "container.sweep.quarantine_removed", "count": 1, "resource_id_ok": true, "actor": ["system"], "outcome": ["success"]}
{"step": "check", "action": "container.sweep.devfs_removed", "count": 1, "resource_id_ok": true, "actor": ["system"], "outcome": ["success"]}
{"step": "container-after", "name": "j2h", "state": "stopped", "detail": "a start was interrupted by an agent restart; the half-started jail was removed without running /etc/rc.shutdown", "jid": null}
{"step": "container-after", "name": "j2q", "state": "stopped", "detail": "a quarantined jail (refused before any of its code ran) was removed at agent start without running /etc/rc.shutdown", "jid": null}
{"step": "container-after", "name": "j2d", "state": "stopped", "detail": "the jail is not running although it should be: it was removed outside Skep, or the host restarted", "jid": null}
{"step": "log", "lines": ["Nov 24 02:10:35 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:10:35.329537857-05:00\",\"level\":\"INFO\",\"msg\":\"containers: observed the jails\",\"containers\":0,\"running\":0,\"removed_without_exec_stop\":0,\"stops_completed\":0}", "Nov 24 02:23:13 langrishe skep-agent[47581]: {\"time\":\"2026-11-24T02:23:13.954101595-05:00\",\"level\":\"WARN\",\"msg\":\"hostops: unmounted a devfs a removal outside Skep left on the jail's root\",\"event\":\"orphaned_devfs_removed\",\"container_id\":\"010c252d-4295-4080-9595-f9a9a57def82\",\"mountpoint\":\"/zroot/skep/jail/010c252d-4295-4080-9595-f9a9a57def82/dev\",\"fsids\":[\"FSID:1895890695:113\"]}", "Nov 24 02:23:14 langrishe skep-agent[47581]: {\"time\":\"2026-11-24T02:23:14.005525051-05:00\",\"level\":\"WARN\",\"msg\":\"containers: removed a quarantined jail without running any of its code\",\"event\":\"jail_quarantine_removed\",\"container_id\":\"3bb0b69b-c5a5-4a87-9602-2c96957565ff\"}", "Nov 24 02:23:14 langrishe skep-agent[47581]: {\"time\":\"2026-11-24T02:23:14.056293183-05:00\",\"level\":\"WARN\",\"msg\":\"containers: removed a jail an agent restart left half-started\",\"event\":\"jail_start_interrupted\",\"container_id\":\"a0f6b510-6f8a-4b0b-a556-6f3f47d07530\"}", "Nov 24 02:23:14 langrishe skep-agent[47581]: {\"time\":\"2026-11-24T02:23:14.056760109-05:00\",\"level\":\"INFO\",\"msg\":\"containers: observed the jails\",\"containers\":3,\"running\":0,\"removed_without_exec_stop\":2,\"stops_completed\":0}"]}
{"step": "host", "jls": "", "devfs": "zroot/skep/jail\t\t/zroot/skep/jail\tzfs\trw,noatime,nfsv4acls \t0 0\nzroot/skep/jail/a0f6b510-6f8a-4b0b-a556-6f3f47d07530 /zroot/skep/jail/a0f6b510-6f8a-4b0b-a556-6f3f47d07530 zfs\trw,noatime,nfsv4acls \t0 0\nzroot/skep/jail/3bb0b69b-c5a5-4a87-9602-2c96957565ff /zroot/skep/jail/3bb0b69b-c5a5-4a87-9602-2c96957565ff zfs\trw,noatime,nfsv4acls \t0 0\nzroot/skep/jail/010c252d-4295-4080-9595-f9a9a57def82 /zroot/skep/jail/010c252d-4295-4080-9595-f9a9a57def82 zfs\trw,noatime,nfsv4acls \t0 0"}
{"step": "delete-container", "name": "j2h", "status": "succeeded"}
{"step": "delete-container", "name": "j2q", "status": "succeeded"}
{"step": "delete-container", "name": "j2d", "status": "succeeded"}
{"step": "delete-template", "status": "succeeded"}
{"step": "end", "jls": "", "datasets": ["zroot/skep", "zroot/skep/jail", "zroot/skep/templates", "zroot/skep/vm"], "runfiles": "", "devfs": "1"}
```

## Driver (j2.py)

```python
#!/usr/bin/env python3
"""Overnight 2026-11-24 job 2: the three sweep audit entries, live on
langrishe. Three NIC-less scratch jails from one test template, all
started. With the agent stopped (agent.db copied aside first): a hand
`jail -r` of j2d leaves its devfs (step 10, 5i); j2h is recorded
`starting` and j2q `failed` (one-row UPDATEs of agent.db). The agent is
started; its sweep must write container.sweep.half_started_removed (j2h),
quarantine_removed (j2q) and devfs_removed (j2d). Entries are read with
`skep-agent audit export --unsealed` (PR #88, installed). Everything made
here is deleted in finally."""
import json, subprocess, sys, time
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS
a = API("langrishe"); ip = HOSTS["langrishe"]
H = "/var/tmp/skep-j2/sqlhelper"; DB = "/var/db/skep/agent/agent.db"; COPY = "/var/tmp/skep-j2/agent.db.copy"
def ssh(cmd, t=180):
    r = subprocess.run(["timeout", str(t), "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw): print(json.dumps(kw), flush=True)
def entries_after(n):
    rc, o = ssh(f"sudo /usr/local/sbin/skep-agent audit export --unsealed 2>/dev/null")
    rows = [json.loads(l) for l in o.splitlines() if l.startswith("{")]
    return rows, [r for r in rows if r["id"] > n]
def wait_agent():
    for _ in range(60):
        time.sleep(2)
        try:
            st, _ = a.req("GET", "/containers")
            if st == 200: return True
        except Exception: pass
    return False

tpl = None; ids = {}; agent_stopped = False
try:
    st, b = a.req("POST", "/storage/templates", {"name": "j2-tpl", "pool": "zroot", "source": "pkgbase"})
    t = a.task(st, b, "template"); tpl = b["template"]["id"]; out(step="template", status=t["status"])
    for n in ("j2h", "j2q", "j2d"):
        st, b = a.req("POST", "/containers", {"name": n, "template_id": tpl})
        t = a.task(st, b, "create " + n); ids[n] = b.get("container", b).get("id")
        st, b = a.req("POST", f"/containers/{ids[n]}/actions", {"action": "start"})
        out(step="created+started", name=n, id=ids[n], start=a.task(st, b, "start")["status"])
    rows, _ = entries_after(0); last = rows[-1]["id"]; out(step="audit-before", last_id=last, count=len(rows))

    out(step="agent-stop", out=ssh("sudo service skep_agent stop; sudo service skep_agent status; pgrep -lx skep-agent || echo no-agent")[1])
    agent_stopped = True
    out(step="copy-aside", out=ssh(f"sudo ls -l /var/db/skep/agent; sudo cp -p {DB} {COPY} && sudo sha256 -q {DB} {COPY}")[1].splitlines())
    jd = ids["j2d"]
    out(step="hand-jail-r", rc_out=ssh(f"sudo jail -r skep-{jd} 2>&1 | tail -3; jls -N name; mount -p | grep -c '{jd}/dev' || true")[1])
    for n, state in (("j2h", "starting"), ("j2q", "failed")):
        out(step="update-row", name=n, state=state,
            out=ssh(f"sudo {H} exec {DB} \"UPDATE containers SET observed_state = ? WHERE id = ?\" {state} {ids[n]}")[1])
    out(step="rows-before-start", rows=ssh(f"sudo {H} rows {DB}")[1].splitlines())
    out(step="agent-start", out=ssh("sudo service skep_agent start")[1]); agent_stopped = False
    out(step="agent-back", ok=wait_agent())
    time.sleep(3)
    _, new = entries_after(last)
    sweep = [r for r in new if r["action"].startswith("container.sweep.")]
    for r in sweep:
        out(step="sweep-entry", entry=r)
    for want, n in (("container.sweep.half_started_removed", "j2h"), ("container.sweep.quarantine_removed", "j2q"), ("container.sweep.devfs_removed", "j2d")):
        got = [r for r in sweep if r["action"] == want]
        out(step="check", action=want, count=len(got), resource_id_ok=all(r.get("resource_id") == ids[n] for r in got),
            actor=[r.get("actor_name") for r in got], outcome=[r.get("outcome") for r in got])
    for n in ids:
        _, c = a.req("GET", f"/containers/{ids[n]}")
        out(step="container-after", name=n, state=c.get("state"), detail=c.get("state_detail"), jid=c.get("jid"))
    out(step="log", lines=ssh("sudo grep -E 'jail_start_interrupted|jail_quarantine_removed|orphaned_devfs_removed|observed the jails' /var/log/daemon.log | tail -n 5 | cut -c1-400")[1].splitlines())
    out(step="host", jls=ssh("jls -N name")[1], devfs=ssh("mount -p | grep skep/jail || echo none")[1])
finally:
    if agent_stopped:
        out(step="finally-agent-start", out=ssh("sudo service skep_agent start")[1]); wait_agent()
    for n, cid in ids.items():
        _, c = a.req("GET", f"/containers/{cid}")
        if isinstance(c, dict) and c.get("state") == "running":
            st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
            if st == 202: a.wait(b.get("id") or b["task"]["id"])
        st, b = a.req("DELETE", f"/containers/{cid}")
        out(step="delete-container", name=n, status=a.task(st, b, "d")["status"] if st == 202 else (st, b))
    if tpl:
        st, b = a.req("DELETE", f"/storage/templates/{tpl}")
        out(step="delete-template", status=a.task(st, b, "d")["status"] if st == 202 else (st, b))
    out(step="end", jls=ssh("jls -N name")[1], datasets=ssh("zfs list -H -o name -r zroot/skep")[1].splitlines(),
        runfiles=ssh("sudo ls /var/run/skep/jails 2>&1")[1], devfs=ssh("mount -p | grep -c skep/jail || true")[1])
```
