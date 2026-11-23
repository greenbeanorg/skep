#!/usr/bin/env python3
"""Job 3 + 4d on one host: a test VM (256 MiB, one 64 MiB thin disk, no NIC);
the real vm.start repairing a missing /dev/zvol node; the volmode=none
refusal, with the failed task's error.details read back from the API.
Prints JSON lines; zfs commands run over ssh. Always deletes the VM."""
import json, subprocess, sys, time
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS

name, pool = sys.argv[1], sys.argv[2]
a = API(name)
ip = HOSTS[name]
def ssh(cmd):
    r = subprocess.run(["timeout", "120", "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw):
    print(json.dumps(kw), flush=True)

vid = ssh("sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery")[1]
out(step="vid", value=vid)
if vid != "0":
    raise SystemExit("hard rule: virtual_interrupt_delivery is not 0; no VM start")
st, b = a.req("POST", "/vms", {"name": "j3-zvol", "cpu_count": 1, "memory": "256M",
                               "disks": [{"pool": pool, "size": "64M", "thin": True}]})
t = a.task(st, b, "create")
vm = b["vm"]["id"]
out(step="create", vm=vm, status=t["status"])
try:
    ds = f"{pool}/skep/vm/{vm}/disk0"
    dev = "/dev/zvol/" + ds
    # a) a missing node with volmode=dev: rename away and back until the
    # node does not come back within a second (the miss the fix repairs).
    missing = False
    for i in range(1, 31):
        ssh(f"sudo zfs rename {ds} {ds}x && sudo zfs rename {ds}x {ds}")
        time.sleep(1)
        rc, _ = ssh(f"test -c {dev}")
        if rc != 0:
            missing = True
            break
    out(step="induce-missing", tries=i, missing=missing, volmode=ssh(f"zfs get -H -o value volmode {ds}")[1])
    if missing:
        st, b = a.req("POST", f"/vms/{vm}/actions", {"action": "start"})
        t = a.task(st, b, "start")
        st2, d = a.req("GET", "/tasks/" + t["id"])
        out(step="start-repair", status=t["status"], error=t.get("error"),
            events=[e["message"] for e in d.get("events", [])])
        out(step="after-start", node=ssh(f"ls -l {dev}")[1], bhyve=ssh("pgrep -lf bhyve")[1],
            log=ssh("sudo grep 'zvol device node repaired' /var/log/daemon.log | tail -1")[1][-400:])
        st, b = a.req("POST", f"/vms/{vm}/actions", {"action": "force_stop"})
        t = a.task(st, b, "force_stop")
        out(step="force_stop", status=t["status"])
    # b) volmode=none: the start refuses; the task's error carries details.
    ssh(f"sudo zfs set volmode=none {ds}")
    time.sleep(1)
    out(step="volmode-none", volmode=ssh(f"zfs get -H -o value volmode {ds}")[1], node=ssh(f"ls -l {dev}")[1])
    st, b = a.req("POST", f"/vms/{vm}/actions", {"action": "start"})
    t = a.task(st, b, "start")
    st2, g = a.req("GET", "/tasks/" + t["id"])
    st3, l = a.req("GET", "/tasks?limit=20")
    listed = [x for x in l["items"] if x["id"] == t["id"]]
    out(step="start-refused", status=t["status"], task_id=t["id"], get_error=g.get("error"),
        list_error=listed[0].get("error") if listed else None)
    out(step="after-refusal", volmode=ssh(f"zfs get -H -o value volmode {ds}")[1], vmm=ssh("ls /dev/vmm")[1])
    cmd = (g.get("error") or {}).get("details", {}).get("command", "")
    if cmd.startswith("zfs set volmode=dev "):
        rc, o = ssh("sudo " + cmd)
        time.sleep(1)
        out(step="printed-command", rc=rc, node=ssh(f"ls -l {dev}")[1])
finally:
    st, b = a.req("DELETE", f"/vms/{vm}")
    t = a.task(st, b, "delete") if st == 202 else {"status": f"HTTP {st} {b}"}
    out(step="delete", status=t["status"], left=ssh(f"zfs list -H -o name -r {pool}/skep/vm | grep {vm} || echo none")[1])
