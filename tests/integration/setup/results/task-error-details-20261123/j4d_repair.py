#!/usr/bin/env python3
"""Job 4d: the real vm.start repairing a missing /dev/zvol node. A test VM
(256 MiB, one 64 MiB thin disk, no NIC); its disk zvol is destroyed and
created again by hand with the same local properties (zvol creation leaves
no node most of the time on these hosts), until the node is missing with
volmode=dev; then vm.start. Always force-stops and deletes the VM."""
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
st, b = a.req("POST", "/vms", {"name": "j4d-repair", "cpu_count": 1, "memory": "256M",
                               "disks": [{"pool": pool, "size": "64M", "thin": True}]})
t = a.task(st, b, "create")
vm = b["vm"]["id"]
out(step="create", vm=vm, status=t["status"])
started = False
try:
    ds = f"{pool}/skep/vm/{vm}/disk0"
    dev = "/dev/zvol/" + ds
    rc, props = ssh(f"zfs get -H -p -o property,value -s local all {ds}")
    opts = []
    for line in props.splitlines():
        k, v = line.split("\t", 1)
        if k in ("volsize", "refreservation", "reservation"):
            continue
        opts += ["-o", f"{k}={v}"]
    vs = ssh(f"zfs get -H -p -o value volsize {ds}")[1]
    out(step="props", local=props.splitlines(), volsize=vs)
    missing = False
    for i in range(1, 11):
        ssh(f"sudo zfs destroy {ds}")
        rc, o = ssh("sudo zfs create -s -V " + vs + " " + " ".join(f"'{x}'" if x != "-o" else x for x in opts) + " " + ds)
        if rc != 0:
            out(step="recreate-failed", output=o)
            break
        time.sleep(1)
        if ssh(f"test -c {dev}")[0] != 0:
            missing = True
            break
    out(step="induce-missing", tries=i, missing=missing, volmode=ssh(f"zfs get -H -o value volmode {ds}")[1],
        props_after=ssh(f"zfs get -H -p -o property,value -s local all {ds}")[1].splitlines())
    if missing:
        st, b = a.req("POST", f"/vms/{vm}/actions", {"action": "start"})
        t = a.task(st, b, "start")
        started = True
        st2, d = a.req("GET", "/tasks/" + t["id"])
        out(step="start-repair", status=t["status"], error=t.get("error"),
            events=[e["message"] for e in d.get("events", [])])
        time.sleep(2)
        out(step="after-start", node=ssh(f"ls -l {dev}")[1], bhyve=ssh("pgrep -lf bhyve")[1],
            log=ssh("sudo grep 'zvol device node repaired' /var/log/daemon.log | tail -1")[1][-500:])
finally:
    if started:
        st, b = a.req("POST", f"/vms/{vm}/actions", {"action": "force_stop"})
        if st == 202:
            out(step="force_stop", status=a.task(st, b, "force_stop")["status"])
    st, b = a.req("DELETE", f"/vms/{vm}")
    t = a.task(st, b, "delete") if st == 202 else {"status": f"HTTP {st} {b}"}
    out(step="delete", status=t["status"], left=ssh(f"zfs list -H -o name -r {pool}/skep/vm | grep {vm} || echo none")[1],
        vmm=ssh("ls /dev/vmm 2>&1")[1])
