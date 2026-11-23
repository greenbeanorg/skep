#!/usr/bin/env python3
"""j3kill.py LABEL: stop the test jail through the API and kill the agent
(and its jail -r) after the epairs are gone but before jail -r ends."""
import json, os, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from zapi import API, S
label = sys.argv[1]; ip = "10.79.20.82"
cid = open(os.path.join(S, "j3-cid")).read().strip()
def ssh(c):
    return subprocess.run(["ssh", "-o", "BatchMode=yes", ip, c], capture_output=True, text=True, timeout=120).stdout
a = API("langrishe")
def show(tag):
    _, c = a.req("GET", f"/containers/{cid}")
    print(tag, json.dumps({k: c.get(k) for k in ("state", "desired_state", "jid", "state_detail")}),
          "host_interface=", [n.get("host_interface") for n in c.get("nics", [])], flush=True)
ssh(f"sudo jexec skep-{cid} sh -c 'echo sleep 30 > /etc/rc.shutdown.local'")
print("rc.shutdown.local:", ssh(f"sudo jexec skep-{cid} cat /etc/rc.shutdown.local").strip())
show("before stop:")
st, b = a.req("POST", f"/containers/{cid}/actions", {"action": "stop"})
print("stop submitted", st, b.get("id") or b.get("task", {}).get("id"), flush=True)
t0 = time.time(); seen = None
while time.time() - t0 < 25:
    out = ssh(f"ifconfig -l | tr ' ' '\\n' | grep -c '^epair' ; pgrep -f 'jail -f .* -r skep-{cid}' | head -1; jls -N name | grep -c {cid}")
    n_ep, jr, injls = (out.split("\n") + ["", "", ""])[:3]
    if n_ep.strip() == "0" and jr.strip() and injls.strip() == "1":
        seen = jr.strip(); break
    time.sleep(0.3)
print(f"after {time.time()-t0:.1f}s: epairs gone, jail -r pid {seen}, jail still in jls", flush=True)
if not seen:
    raise SystemExit("did not reach the window")
agent = ssh("pgrep -f '^/usr/local/sbin/skep-agent' | head -1").strip()
print("kill -9 agent", agent, "and jail -r", seen, flush=True)
print(ssh(f"sudo kill -9 {agent} {seen}; sleep 1; echo jls:; jls -N name; echo epairs:; ifconfig -l | tr ' ' '\\n' | grep epair; echo end"))
# daemon(8) restarts the agent after skep_agent_restart_delay; wait for the socket to answer.
for _ in range(60):
    time.sleep(2)
    try:
        st, _ = a.req("GET", "/system")
        if st == 200:
            break
    except Exception:
        pass
print("agent back:", ssh("pgrep -lf '^/usr/local/sbin/skep-agent'").strip(), flush=True)
time.sleep(2)
show("after restart:")
print(ssh(f"echo jls:; jls -N name; echo epairs:; ifconfig -l | tr ' ' '\\n' | grep epair; echo bridge1:; ifconfig bridge1 | grep member; echo log:; sudo grep {cid} /var/log/daemon.log | tail -n 6 | cut -c1-330"))
