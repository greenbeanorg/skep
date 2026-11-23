#!/usr/bin/env python3
"""Job 4c clean-up after the flag was cleared by hand: stop the test jail,
delete it and the test template, show what is left."""
import json, subprocess, sys
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS
a = API("langrishe"); ip = HOSTS["langrishe"]
cid, tpl = sys.argv[1], sys.argv[2]
def ssh(c):
    r = subprocess.run(["timeout", "120", "ssh", "-o", "BatchMode=yes", ip, c], capture_output=True, text=True)
    return (r.stdout + r.stderr).strip()
def out(**kw): print(json.dumps(kw), flush=True)
st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
t = a.task(st, b, "stop") if st == 202 else None
out(step="stop", http=st, status=t and t["status"], error=t and t.get("error"))
_, c = a.req("GET", f"/containers/{cid}")
out(step="state", **{k: c.get(k) for k in ("state", "desired_state", "jid", "state_detail")})
st, b = a.req("DELETE", f"/containers/{cid}")
out(step="delete-container", status=a.task(st, b, "delete")["status"] if st == 202 else (st, b))
st, b = a.req("DELETE", f"/storage/templates/{tpl}")
t = a.task(st, b, "delete") if st == 202 else None
out(step="delete-template", http=st, status=t and t["status"], error=t and t.get("error"), body=None if t else b)
out(step="end", jls=ssh("jls -N name"), datasets=ssh("zfs list -H -o name -r zroot/skep").splitlines(),
    mounts=ssh("mount | grep skep || echo none"), runfiles=ssh("sudo ls /var/run/skep/jails 2>&1"),
    containers=a.req("GET", "/containers")[1].get("items"), templates=[x["name"] for x in a.req("GET", "/storage/templates")[1].get("items", [])])
