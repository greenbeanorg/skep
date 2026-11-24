# RACCT off, with a guest — langrishe, 2026-11-24

Overnight run, job 1. langrishe: FreeBSD 15.1, Skep eadd643 (installed),
`kern.racct.enable` 0 (`GET /system`: `containers.rctl: false`). One pkgbase
test template per driver and NIC-less scratch jails, all deleted at the
end (end state: no jails, only `zroot/skep{,/jail,/templates,/vm}`,
`/var/run/skep/jails` empty). Drivers: `j1.py`, `j1b.py` below (they use
the pinned client `zapi.py` of `tests/integration/setup/results/zvol-device-node-20261122/`).

| Check | Expected (docs / OpenAPI) | Result |
|---|---|---|
| create with `memory`, `cpu_percent`, `max_processes`, `open_files`, `locked_memory` (each alone) | `rctl_unavailable` 409 | **409 `rctl_unavailable`** for each, details `{"racct_enabled": false}`; nothing created |
| update of a stopped jail adding `max_processes` or `memory` | `rctl_unavailable` 409 | **409 `rctl_unavailable`** |
| start of a jail that has an RCTL limit | `rctl_unavailable` 409 | **Not induced:** such a jail cannot be made through the API without RACCT (create and update refuse); it needs a reboot without RACCT or an agent.db edit, neither approved for this job. Unit-tested (`TestRacctDisabled`, host fake) |
| `options.mlock` without `locked_memory` (create, update) | `mlock_requires_memory_limit` | **422 `mlock_requires_memory_limit`** |
| `options.mlock` with `locked_memory` | docs: needs RACCT (`mlock_requires_memory_limit`) | **409 `rctl_unavailable`**: the limit is refused first (as `TestRacctDisabled` intends); the docs' wording suggests `mlock_requires_memory_limit` |
| `disk_quota` alone: create, update, start | works without RACCT | create 202 → succeeded, `refquota` 128M; update to 160 MiB succeeded at once (`refquota` 160M); start succeeded; no RCTL rule (`rctl`: "present, but disabled") |
| fill past the quota inside the jail | write refused | `dd if=/dev/random` stopped after 29.5 MiB; `referenced` = `refquota` = 128M. (The first run wrote `/dev/zero`, which compression stores as nothing: 200 MiB written, `referenced` unchanged; a test error, not a Skep one) |
| jail metrics | no RACCT figures, no error | `GET /metrics` 200; the jail's entry has only `id`, `name`, `state`, `disk_used_bytes`, `disk_referenced_bytes`. The first read, right after the start, did not list the jail yet (sample from before the start, as documented: up to a minute old); the next sample did. No RACCT warning in the agent log, only the "task refused" lines of the refusals |

Differences from the docs (backlog rows, no code change):
- `jails.md` and the OpenAPI `mlock` description say mlock needs a locked-memory limit and RACCT (`mlock_requires_memory_limit`); on a host without RACCT, mlock with a locked-memory limit answers `rctl_unavailable` (409). The `!info.Enabled` half of the mlock check in `internal/containers/limits.go` is therefore never reached.
- `jails.md` gives "about 78 MiB for a minimal template" as what a new root references; the default template (`FreeBSD-set-minimal`, `FreeBSD-ssh`) built on 2026-11-24 references 98.5 MiB.

## Output, j1.py

```
{"step": "system.containers", "http": 200, "containers": {"rctl": false, "max_cpu_percent": 200, "max_locked_memory_bytes": 1042917376}}
{"step": "template", "id": "f7335890-0196-4a0e-a4eb-e296d92540fc", "status": "succeeded", "error": null}
{"step": "create-with-limit", "limits": {"memory": 268435456}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-with-limit", "limits": {"cpu_percent": 50}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-with-limit", "limits": {"max_processes": 100}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-with-limit", "limits": {"open_files": 128}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-with-limit", "limits": {"locked_memory": 67108864}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-mlock", "body": {"options": {"mlock": true}}, "http": 422, "error": {"code": "mlock_requires_memory_limit", "message": "mlock is accepted only together with a locked-memory limit (limits.locked_memory), because locked memory cannot be paged out"}}
{"step": "create-mlock", "body": {"options": {"mlock": true}, "limits": {"locked_memory": 67108864, "memory": 268435456}}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "create-disk-quota", "http": 202, "error": null}
{"step": "create-disk-quota-task", "id": "6bdd86a0-54bf-492c-bb11-0d8449634bc0", "status": "succeeded", "error": null}
{"step": "refquota", "zfs": ["128M", "98.5M"]}
{"step": "update", "body": {"limits": {"disk_quota": 134217728, "max_processes": 100}}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "update", "body": {"limits": {"disk_quota": 134217728, "memory": 268435456}}, "http": 409, "error": {"code": "rctl_unavailable", "message": "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it", "details": {"racct_enabled": false}}}
{"step": "update", "body": {"options": {"mlock": true}, "limits": {"disk_quota": 134217728}}, "http": 422, "error": {"code": "mlock_requires_memory_limit", "message": "mlock is accepted only together with a locked-memory limit (limits.locked_memory), because locked memory cannot be paged out"}}
{"step": "update-disk-quota", "http": 202, "status": "succeeded", "refquota": "160M"}
{"step": "container", "limits": {"disk_quota": 167772160}, "options": {"sysvipc": false, "mlock": false}, "state": "stopped"}
{"step": "start", "status": "succeeded", "error": null}
{"step": "rctl", "rules": "rctl: RACCT/RCTL present, but disabled; enable using kern.racct.enable=1 tunable\nrctl: RACCT/RCTL present, but disabled; enable using kern.racct.enable=1 tunable"}
{"step": "fill", "rc": 0, "output": "200+0 records out\n209715200 bytes transferred in 1.023719 secs (204856123 bytes/sec)", "zfs": ["160M", "98.5M", "208K"]}
{"step": "metrics", "http": 200, "observed_at": "2026-11-24T07:18:35.338900538Z", "container": [], "keys": null}
{"step": "metrics-after-35s", "http": 200, "observed_at": "2026-11-24T07:19:05.349139657Z", "container": [{"id": "6bdd86a0-54bf-492c-bb11-0d8449634bc0", "name": "j1q", "state": "running", "disk_used_bytes": 212992, "disk_referenced_bytes": 103301120}]}
{"step": "agent-log-warnings", "lines": ["Nov 24 02:18:49 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:18:49.572097-05:00\",\"level\":\"WARN\",\"msg\":\"task refused\",\"type\":\"container.create\",\"actor_id\":\"9f3f544d-959d-44df-8e5f-ee4fdd30b6ca\",\"error_code\":\"rctl_unavailable\"}", "Nov 24 02:18:49 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:18:49.575258388-05:00\",\"level\":\"WARN\",\"msg\":\"task refused\",\"type\":\"container.create\",\"actor_id\":\"9f3f544d-959d-44df-8e5f-ee4fdd30b6ca\",\"error_code\":\"rctl_unavailable\"}", "Nov 24 02:18:49 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:18:49.58089274-05:00\",\"level\":\"WARN\",\"msg\":\"task refused\",\"type\":\"container.create\",\"actor_id\":\"9f3f544d-959d-44df-8e5f-ee4fdd30b6ca\",\"error_code\":\"rctl_unavailable\"}", "Nov 24 02:18:50 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:18:50.303604385-05:00\",\"level\":\"WARN\",\"msg\":\"task refused\",\"type\":\"container.update\",\"actor_id\":\"9f3f544d-959d-44df-8e5f-ee4fdd30b6ca\",\"error_code\":\"rctl_unavailable\"}", "Nov 24 02:18:50 langrishe skep-agent[44057]: {\"time\":\"2026-11-24T02:18:50.30797829-05:00\",\"level\":\"WARN\",\"msg\":\"task refused\",\"type\":\"container.update\",\"actor_id\":\"9f3f544d-959d-44df-8e5f-ee4fdd30b6ca\",\"error_code\":\"rctl_unavailable\"}"]}
{"step": "stop", "status": "succeeded"}
{"step": "delete-container", "id": "6bdd86a0-54bf-492c-bb11-0d8449634bc0", "status": "succeeded"}
{"step": "delete-template", "status": "succeeded"}
{"step": "end", "jls": "", "datasets": ["zroot/skep", "zroot/skep/jail", "zroot/skep/templates", "zroot/skep/vm"], "runfiles": ""}
```

## Output, j1b.py (fill with incompressible data)

```
{"step": "compression", "zfs": "on"}
{"step": "template", "status": "succeeded"}
{"step": "create", "status": "succeeded"}
{"step": "start", "status": "succeeded"}
{"step": "before", "zfs": ["128M", "98.5M"]}
{"step": "fill", "rc": 0, "output": "30+0 records in\n29+1 records out\n30932992 bytes transferred in 1.903716 secs (16248742 bytes/sec)", "zfs": ["128M", "128M"]}
{"step": "stop", "status": "succeeded"}
{"step": "delete-container", "status": "succeeded"}
{"step": "delete-template", "status": "succeeded"}
{"step": "end", "jls": "", "datasets": ["zroot/skep", "zroot/skep/jail", "zroot/skep/templates", "zroot/skep/vm"], "runfiles": ""}
```

## j1.py

```python
#!/usr/bin/env python3
"""Overnight 2026-11-24 job 1: RACCT off, with a guest (langrishe).
One test template and NIC-less scratch jails. Limits other than
disk_quota at create and update, options.mlock, disk_quota alone (with a
fill past it inside the jail), and jail metrics. Everything made here is
deleted in finally."""
import json, subprocess, sys, time
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS

a = API("langrishe"); ip = HOSTS["langrishe"]
MiB = 1 << 20
def ssh(cmd, t=180):
    r = subprocess.run(["timeout", str(t), "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw):
    print(json.dumps(kw), flush=True)
def err(b):
    if isinstance(b, dict):
        e = b.get("error", b)
        return {k: e.get(k) for k in ("code", "message", "details") if k in e}
    return b

tpl = None; made = []
try:
    st, s = a.req("GET", "/system")
    out(step="system.containers", http=st, containers=s.get("containers"))
    st, b = a.req("POST", "/storage/templates", {"name": "j1-tpl", "pool": "zroot", "source": "pkgbase"})
    t = a.task(st, b, "template create"); tpl = b["template"]["id"]
    out(step="template", id=tpl, status=t["status"], error=t.get("error"))
    if t["status"] != "succeeded":
        raise SystemExit("template build failed")

    # Create with each RCTL limit alone.
    for lim in ({"memory": 256 * MiB}, {"cpu_percent": 50}, {"max_processes": 100},
                {"open_files": 128}, {"locked_memory": 64 * MiB}):
        st, b = a.req("POST", "/containers", {"name": "j1-lim", "template_id": tpl, "limits": lim})
        out(step="create-with-limit", limits=lim, http=st, error=err(b))
        if st == 202:
            made.append(b.get("container", b).get("id")); a.wait(b.get("id") or b["task"]["id"])
    # mlock without and with a locked-memory limit.
    for body in ({"options": {"mlock": True}},
                 {"options": {"mlock": True}, "limits": {"locked_memory": 64 * MiB, "memory": 256 * MiB}}):
        st, b = a.req("POST", "/containers", dict({"name": "j1-mlock", "template_id": tpl}, **body))
        out(step="create-mlock", body=body, http=st, error=err(b))
        if st == 202:
            made.append(b.get("container", b).get("id"))

    # disk_quota alone.
    st, b = a.req("POST", "/containers", {"name": "j1q", "template_id": tpl, "limits": {"disk_quota": 128 * MiB}})
    out(step="create-disk-quota", http=st, error=err(b) if st != 202 else None)
    t = a.task(st, b, "create j1q"); cid = b.get("container", b).get("id"); made.append(cid)
    out(step="create-disk-quota-task", id=cid, status=t["status"], error=t.get("error"))
    out(step="refquota", zfs=ssh(f"zfs get -H -o value refquota,referenced zroot/skep/jail/{cid}")[1].split())

    # Update (stopped) with RCTL limits, and with mlock.
    for body in ({"limits": {"disk_quota": 128 * MiB, "max_processes": 100}},
                 {"limits": {"disk_quota": 128 * MiB, "memory": 256 * MiB}},
                 {"options": {"mlock": True}, "limits": {"disk_quota": 128 * MiB}}):
        st, b = a.req("PUT", f"/containers/{cid}", body)
        out(step="update", body=body, http=st, error=err(b) if st != 202 else None)
        if st == 202:
            t = a.wait(b.get("id") or b["task"]["id"]); out(step="update-task", status=t["status"], error=t.get("error"))
    # Update with disk_quota alone (changed at once).
    st, b = a.req("PUT", f"/containers/{cid}", {"limits": {"disk_quota": 160 * MiB}})
    t = a.task(st, b, "update quota"); out(step="update-disk-quota", http=st, status=t["status"],
        refquota=ssh(f"zfs get -H -o value refquota zroot/skep/jail/{cid}")[1])
    _, c = a.req("GET", f"/containers/{cid}")
    out(step="container", limits=c.get("limits"), options=c.get("options"), state=c.get("state"))

    # Start: no RCTL limit, so it starts.
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "start"})
    t = a.task(st, b, "start"); out(step="start", status=t["status"], error=t.get("error"))
    out(step="rctl", rules=ssh(f"sudo rctl 2>&1 | head -5; sudo rctl -u jail:skep-{cid} 2>&1 | head -3")[1])
    # Fill past the quota inside the jail.
    rc, o = ssh(f"sudo jexec skep-{cid} dd if=/dev/zero of=/root/fill bs=1m count=200 2>&1 | tail -2")
    out(step="fill", rc=rc, output=o, zfs=ssh(f"zfs get -H -o value refquota,referenced,used zroot/skep/jail/{cid}")[1].split())
    ssh(f"sudo jexec skep-{cid} rm -f /root/fill")
    # Metrics: wait for a sample that includes the running jail.
    time.sleep(2)
    st, m = a.req("GET", "/metrics")
    jm = [x for x in (m.get("containers") or []) if x.get("id") == cid]
    out(step="metrics", http=st, observed_at=m.get("observed_at"), container=jm, keys=sorted(jm[0].keys()) if jm else None)
    time.sleep(35)
    st, m = a.req("GET", "/metrics")
    jm = [x for x in (m.get("containers") or []) if x.get("id") == cid]
    out(step="metrics-after-35s", http=st, observed_at=m.get("observed_at"), container=jm)
    out(step="agent-log-warnings", lines=ssh("sudo grep -i 'racct\\|rctl' /var/log/daemon.log | tail -n 5 | cut -c1-300")[1].splitlines())
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
    out(step="stop", status=a.task(st, b, "stop")["status"])
finally:
    for cid in made:
        _, c = a.req("GET", f"/containers/{cid}")
        if isinstance(c, dict) and c.get("state") == "running":
            st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
            if st == 202: a.wait(b.get("id") or b["task"]["id"])
        st, b = a.req("DELETE", f"/containers/{cid}")
        out(step="delete-container", id=cid, status=a.task(st, b, "delete")["status"] if st == 202 else (st, b))
    if tpl:
        st, b = a.req("DELETE", f"/storage/templates/{tpl}")
        out(step="delete-template", status=a.task(st, b, "delete")["status"] if st == 202 else (st, b))
    out(step="end", jls=ssh("jls -N name")[1], datasets=ssh("zfs list -H -o name -r zroot/skep")[1].splitlines(),
        runfiles=ssh("sudo ls /var/run/skep/jails 2>&1")[1])
```

## j1b.py

```python
#!/usr/bin/env python3
"""Job 1 rerun of the disk_quota fill with incompressible data (the first
run wrote zeros, which compression stores as nothing)."""
import json, subprocess, sys
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS
a = API("langrishe"); ip = HOSTS["langrishe"]; MiB = 1 << 20
def ssh(cmd, t=300):
    r = subprocess.run(["timeout", str(t), "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw): print(json.dumps(kw), flush=True)
tpl = cid = None
try:
    out(step="compression", zfs=ssh("zfs get -H -o value compression zroot/skep/jail")[1])
    st, b = a.req("POST", "/storage/templates", {"name": "j1b-tpl", "pool": "zroot", "source": "pkgbase"})
    t = a.task(st, b, "template"); tpl = b["template"]["id"]; out(step="template", status=t["status"])
    st, b = a.req("POST", "/containers", {"name": "j1b", "template_id": tpl, "limits": {"disk_quota": 128 * MiB}})
    t = a.task(st, b, "create"); cid = b.get("container", b).get("id"); out(step="create", status=t["status"])
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "start"})
    out(step="start", status=a.task(st, b, "start")["status"])
    out(step="before", zfs=ssh(f"zfs get -H -o value refquota,referenced zroot/skep/jail/{cid}")[1].split())
    rc, o = ssh(f"sudo jexec skep-{cid} dd if=/dev/random of=/root/fill bs=1m count=100 2>&1 | tail -3")
    out(step="fill", rc=rc, output=o, zfs=ssh(f"zfs get -H -o value refquota,referenced zroot/skep/jail/{cid}")[1].split())
    ssh(f"sudo jexec skep-{cid} rm -f /root/fill")
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
    out(step="stop", status=a.task(st, b, "stop")["status"])
finally:
    if cid:
        _, c = a.req("GET", f"/containers/{cid}")
        if isinstance(c, dict) and c.get("state") == "running":
            st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"}); a.wait(b.get("id") or b["task"]["id"])
        st, b = a.req("DELETE", f"/containers/{cid}"); out(step="delete-container", status=a.task(st, b, "d")["status"] if st == 202 else (st, b))
    if tpl:
        st, b = a.req("DELETE", f"/storage/templates/{tpl}"); out(step="delete-template", status=a.task(st, b, "d")["status"] if st == 202 else (st, b))
    out(step="end", jls=ssh("jls -N name")[1], datasets=ssh("zfs list -H -o name -r zroot/skep")[1].splitlines(), runfiles=ssh("sudo ls /var/run/skep/jails 2>&1")[1])
```
