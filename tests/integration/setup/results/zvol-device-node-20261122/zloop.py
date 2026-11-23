#!/usr/bin/env python3
"""zloop.py HOST POOL: Job 1 step 4 through the API. 50 vm.create, 20 disk
adds, a GPT VM, 20 vm.clone, 20 rollbacks, GPT checks, then start + force-stop
of every VM. Counts repairs per path from task events, and at start from the
agent's log. Writes zloop-HOST.json. Test VMs are left for zcleanup."""
import datetime, json, os, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from zapi import API, HOSTS, S

name, pool = sys.argv[1], sys.argv[2]
ip = HOSTS[name]
a = API(name)
out = {"host": name, "pool": pool, "paths": {}, "creates": [], "errors": []}
REP, MISS = "repaired the device node of a disk", "the device node of a disk is missing"


def ssh(cmd, check=True):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True, timeout=300)
    if check and r.returncode != 0:
        raise SystemExit(f"ssh {cmd!r}: rc {r.returncode} {r.stderr[-400:]}")
    return r.stdout


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def count(path, t):
    p = out["paths"].setdefault(path, {"tasks": 0, "failed": 0, "repaired": 0, "left_to_start": 0})
    p["tasks"] += 1
    if t["status"] != "succeeded":
        p["failed"] += 1
        out["errors"].append({"path": path, "task": t["id"], "status": t["status"], "error": t.get("error")})
    for e in t.get("events") or []:
        if e["message"] == REP:
            p["repaired"] += 1
        elif e["message"].startswith(MISS):
            p["left_to_start"] += 1
    return t


def disk(size="64M"):
    return {"pool": pool, "size": size, "thin": True}


def create(n):
    st, b = a.req("POST", "/vms", {"name": n, "cpu_count": 1, "memory": "256M", "disks": [disk()]})
    t = count("create", a.task(st, b, "create " + n))
    out["creates"].append({"name": n, "seconds": (ts(t["finished_at"]) - ts(t["created_at"])).total_seconds(),
                           "repaired": sum(e["message"] == REP for e in t["events"])})
    return b["vm"]["id"]


def gpt_check(label):
    p1 = ssh(f"ls /dev/zvol/{pool}/skep/vm/*/ 2>/dev/null | grep -c 'p[0-9]' || true").strip()
    geom = ssh("sysctl -n kern.geom.conftxt | grep -c zvol || true").strip()
    out[f"gpt_check_{label}"] = {"p_nodes": int(p1 or 0), "conftxt_zvol_lines": int(geom or 0)}


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


ids = {}
log("50 creates")
for i in range(1, 51):
    ids[f"zc-{i:02d}"] = create(f"zc-{i:02d}")
log("20 disk adds")
for i in range(1, 21):
    vid = ids[f"zc-{i:02d}"]
    st, b = a.req("PUT", f"/vms/{vid}", {"add_disks": [disk()]})
    count("disk_add", a.task(st, b, "disk add"))

log("GPT VM")
gid = ids["zgpt"] = create("zgpt")
gds = f"{pool}/skep/vm/{gid}/disk0"
ssh(f"sudo dd if=$HOME/gpt64.img of=/dev/zvol/{gds} bs=1m 2>&1 >/dev/null; sudo sha256 -q /dev/zvol/{gds}")
img = subprocess.run(["sha256sum", os.path.join(S, "gpt64.img")], capture_output=True, text=True).stdout.split()[0]
dev = ssh(f"sudo sha256 -q /dev/zvol/{gds}").strip()
out["gpt"] = {"dataset": gds, "image_sha256": img, "disk_sha256": dev, "match": img == dev}
st, b = a.req("POST", f"/vms/{gid}/snapshots", {"name": "base"})
t = a.task(st, b, "snapshot")
st, snaps = a.req("GET", f"/vms/{gid}/snapshots")
items = snaps.get("items", snaps) if isinstance(snaps, dict) else snaps
sid = [s for s in items if s["name"] == "base"][0]["id"]

log("20 clones")
for i in range(1, 21):
    n = f"zk-{i:02d}"
    st, b = a.req("POST", f"/vms/{gid}/clone", {"name": n, "snapshot_id": sid})
    t = count("clone", a.task(st, b, "clone " + n))
    ids[n] = (t.get("result") or {}).get("vm_id") or b.get("vm", {}).get("id")
gpt_check("after_clones")

log("20 rollbacks")
rb_sha_ok = 0
for i in range(1, 21):
    ssh(f"sudo dd if=/dev/random of=/dev/zvol/{gds} bs=1m count=1 seek=8 2>/dev/null")
    st, b = a.req("POST", f"/vms/{gid}/snapshots/{sid}/rollback", {})
    count("rollback", a.task(st, b, "rollback"))
    if ssh(f"sudo sha256 -q /dev/zvol/{gds}").strip() == img:
        rb_sha_ok += 1
out["rollback_sha_matches_image"] = rb_sha_ok
gpt_check("after_rollbacks")

log("nodes")
lines = ssh(f"for d in $(zfs list -H -o name -t volume -r {pool}/skep/vm); do v=$(zfs get -H -o value volmode $d); "
            f"if [ -c /dev/zvol/$d ]; then echo \"$d $v node\"; else echo \"$d $v MISSING\"; fi; done").split("\n")
lines = [l for l in lines if l]
out["nodes_before_start"] = {"volumes": len(lines), "missing": [l for l in lines if l.endswith("MISSING")],
                             "volmode_not_dev": [l for l in lines if " dev " not in l]}

log("start + force-stop every VM")
mark = ssh("sudo wc -l < /var/log/daemon.log").strip()
started = 0
for n, vid in ids.items():
    st, b = a.req("POST", f"/vms/{vid}/actions", {"action": "start"})
    t = count("start", a.task(st, b, "start " + n))
    if t["status"] == "succeeded":
        time.sleep(1)
        _, vm = a.req("GET", f"/vms/{vid}")
        if vm.get("observed", {}).get("state") == "running":
            started += 1
    st, b = a.req("POST", f"/vms/{vid}/actions", {"action": "force_stop"})
    a.task(st, b, "force-stop " + n)
out["started_and_running"] = started
out["vm_count"] = len(ids)
logl = ssh(f"sudo tail -n +{int(mark) + 1} /var/log/daemon.log")
out["paths"]["start"]["repaired_agent_log"] = logl.count("repaired a disk's device node before the start")
out["start_log_errors"] = [l[-300:] for l in logl.split("\n") if '"level":"ERROR"' in l][:10]
out["ids"] = ids
c = sorted(x["seconds"] for x in out["creates"])
out["create_seconds"] = {"min": c[0], "median": c[len(c) // 2], "max": c[-1],
                         "with_repair_median": sorted(x["seconds"] for x in out["creates"] if x["repaired"])[len([x for x in out["creates"] if x["repaired"]]) // 2] if any(x["repaired"] for x in out["creates"]) else None,
                         "without_repair": [x["seconds"] for x in out["creates"] if not x["repaired"]]}
json.dump(out, open(os.path.join(S, f"zloop-{name}.json"), "w"), indent=1)
print(json.dumps({k: v for k, v in out.items() if k not in ("creates", "ids")}, indent=1))
