#!/usr/bin/env python3
"""Job 4c (PR #57, amendment aa): the completion-failure path, live.
A test template and a test jail without NICs on langrishe. The jail's
/etc/rc.shutdown.local sleeps 30 s; a stop is submitted; while jail -r runs
(after the stop task rewrote jail.conf), that test jail's jail.conf gets
the schg flag and the agent and its jail -r are killed. At agent start
completeStop runs StopJail again, whose jail.conf write fails (EPERM), so
the container must be recorded unknown with the detail. Then the flag is
cleared (always, in finally), the jail stopped again, and everything the
run made deleted."""
import json, subprocess, sys, time
sys.path.insert(0, __import__("os").path.dirname(__file__))
from zapi import API, HOSTS

a = API("langrishe"); ip = HOSTS["langrishe"]
def ssh(cmd, t=120):
    r = subprocess.run(["timeout", str(t), "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw):
    print(json.dumps(kw), flush=True)
def show(tag, cid):
    _, c = a.req("GET", f"/containers/{cid}")
    out(step=tag, **{k: c.get(k) for k in ("state", "desired_state", "jid", "state_detail")})
    return c

tpl = cid = conf = None
flagged = False
try:
    st, b = a.req("POST", "/storage/templates", {"name": "j4c-tpl", "pool": "zroot", "source": "pkgbase"})
    t = a.task(st, b, "template create"); tpl = b["template"]["id"]
    out(step="template", id=tpl, status=t["status"], error=t.get("error"))
    st, b = a.req("POST", "/containers", {"name": "j4c", "template_id": tpl})
    t = a.task(st, b, "container create"); cid = b.get("container", b).get("id")
    out(step="container", id=cid, status=t["status"])
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "start"})
    out(step="start", status=a.task(st, b, "start")["status"])
    conf = f"/var/run/skep/jails/{cid}/jail.conf"
    ssh(f"sudo jexec skep-{cid} sh -c 'echo sleep 30 > /etc/rc.shutdown.local'")
    m0 = ssh(f"sudo stat -f %m {conf}")[1]
    show("before-stop", cid)
    st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
    out(step="stop-submitted", http=st)
    t0 = time.time(); jr = None
    while time.time() - t0 < 25:
        _, o = ssh(f"pgrep -f 'jail -f .* -r skep-{cid}' | head -1; jls -N name | grep -c {cid}; sudo stat -f %m {conf}")
        parts = (o.split("\n") + ["", "", ""])[:3]
        if parts[0].strip() and parts[1].strip() == "1" and parts[2].strip() != m0:
            jr = parts[0].strip(); break
        time.sleep(0.3)
    out(step="window", seconds=round(time.time() - t0, 1), jail_r_pid=jr)
    if not jr:
        raise SystemExit("did not reach the window")
    rc, o = ssh(f"sudo chflags schg {conf} && ls -lo {conf}")
    flagged = rc == 0
    out(step="schg", rc=rc, ls=o)
    agent = ssh("pgrep -f '^/usr/local/sbin/skep-agent' | head -1")[1]
    out(step="kill", agent=agent, jail_r=jr, output=ssh(f"sudo kill -9 {agent} {jr}; sleep 1; jls -N name")[1])
    for _ in range(60):
        time.sleep(2)
        try:
            st, _ = a.req("GET", "/system")
            if st == 200:
                break
        except Exception:
            pass
    time.sleep(3)
    out(step="agent-back", pid=ssh("pgrep -f '^/usr/local/sbin/skep-agent'")[1])
    show("after-restart", cid)
    out(step="log", lines=ssh(f"sudo grep -E 'jail_stop_interrupted|jail_stop_completed|stops_completed' /var/log/daemon.log | grep -v '\"stops_completed\":0' | tail -n 3 | cut -c1-600")[1].splitlines())
finally:
    if conf and flagged:
        rc, o = ssh(f"sudo chflags noschg {conf}; ls -lo {conf}")
        out(step="noschg", rc=rc, ls=o)
    if cid:
        ssh(f"sudo jexec skep-{cid} rm -f /etc/rc.shutdown.local")
        st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
        out(step="stop-again", http=st, status=a.task(st, b, "stop")["status"] if st == 202 else b)
        show("after-stop-again", cid)
        st, b = a.req("DELETE", f"/containers/{cid}")
        out(step="delete-container", status=a.task(st, b, "delete")["status"] if st == 202 else (st, b))
    if tpl:
        st, b = a.req("DELETE", f"/storage/templates/{tpl}")
        out(step="delete-template", status=a.task(st, b, "delete")["status"] if st == 202 else (st, b))
    out(step="end", jls=ssh("jls -N name")[1], datasets=ssh("zfs list -H -o name -r zroot/skep")[1].splitlines(),
        mounts=ssh("mount | grep -c skep || true")[1], runfiles=ssh("sudo ls /var/run/skep/jails 2>&1")[1])
