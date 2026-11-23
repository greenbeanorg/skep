#!/usr/bin/env python3
"""PR #57 fix round, Part 7 (langrishe, unattended). One test template and
one test jail with a DHCP NIC on a test bridge bridge1 (on vtnet1), its
/etc/rc.shutdown.local set to sleep 30 so a stop can be caught.
  B: j3kill success case: stop, kill -9 the agent and its jail -r after the
     epairs are gone; the next agent start completes the stop.
  C: agent stopped (service stop, SIGTERM) during that completion; the
     container must stay recorded stopping (read from a copy of agent.db
     taken with the agent stopped), and the next start completes it.
  D: induced failure: the stop's jail.conf gets chflags schg before the
     kill, so the completion fails; GET must show running with the detail.
     A stop task with the flag still set fails; GET keeps its detail.
Everything made is removed in finally; agent.json restored, sha256 checked."""
import json, os, sqlite3, subprocess, sys, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from zapi import API, HOSTS

ip = HOSTS["langrishe"]
SCR = os.environ["SCRATCH"]
CONF = "/usr/local/etc/skep/agent.json"
BAK = "/usr/local/etc/skep/agent.json.p57bak"
WANT_SHA = "3740c603b03b399655048fc48ac85140fd4b48f4fe29afd450911b196b751fed"

def ssh(cmd, t=180):
    r = subprocess.run(["timeout", str(t), "ssh", "-o", "BatchMode=yes", ip, cmd], capture_output=True, text=True)
    return r.returncode, (r.stdout + r.stderr).strip()
def out(**kw):
    print(json.dumps(kw), flush=True)

a = None
def api():
    global a
    if a is None:
        a = API("langrishe")
    return a
def show(tag, cid):
    st, c = api().req("GET", f"/containers/{cid}")
    if st != 200:
        out(step=tag, http=st, body=c); return None
    out(step=tag, http=st, **{k: c.get(k) for k in ("state", "desired_state", "jid", "state_detail")},
        host_interface=[n.get("host_interface") for n in c.get("nics", [])])
    return c
def wait_agent(cid, limit=240):
    t0 = time.time()
    while time.time() - t0 < limit:
        time.sleep(2)
        try:
            st, _ = api().req("GET", f"/containers/{cid}")
            if st == 200:
                return round(time.time() - t0, 1)
        except Exception:
            pass
    raise SystemExit("the agent did not come back")
def logcount(cid, event):
    return int(ssh(f"sudo grep {cid} /var/log/daemon.log | grep -c '\"event\":\"{event}\"' || true")[1] or 0)
def loglines(cid, n=8):
    return ssh(f"sudo grep -E 'jail_stop_|stops_completed' /var/log/daemon.log | grep {cid} | tail -n {n} | cut -c1-520")[1].splitlines()
def host_state(tag):
    out(step=tag, jls=ssh("jls -N name")[1].splitlines(), epairs=ssh("ifconfig -l | tr ' ' '\\n' | grep epair || true")[1].split(),
        bridge1=ssh("ifconfig bridge1 | grep member || true")[1].splitlines())
def task(st, b, what):
    t = api().task(st, b, what)
    return t["status"], t.get("error")
def start(cid):
    st, b = api().req("POST", f"/containers/{cid}/actions", {"action": "start"})
    out(step="start", result=task(st, b, "start"))
def catch_stop(cid, need_mtime=None):
    """Submit a stop and wait until the epairs are gone (or jail.conf was
    rewritten) while jail -r still runs and the jail is in jls."""
    st, b = api().req("POST", f"/containers/{cid}/actions", {"action": "stop"})
    out(step="stop-submitted", http=st, task=b.get("id") if isinstance(b, dict) else b)
    t0 = time.time()
    conf = f"/var/run/skep/jails/{cid}/jail.conf"
    while time.time() - t0 < 25:
        _, o = ssh(f"ifconfig -l | tr ' ' '\\n' | grep -c '^epair' ; pgrep -f 'jail -f .* -r skep-{cid}' | head -1; jls -N name | grep -c {cid}; sudo stat -f %m {conf}")
        n_ep, jr, injls, m = (o.split("\n") + ["", "", "", ""])[:4]
        if n_ep.strip() == "0" and jr.strip() and injls.strip() == "1" and (need_mtime is None or m.strip() != need_mtime):
            out(step="window", seconds=round(time.time() - t0, 1), jail_r_pid=jr.strip())
            return jr.strip()
        time.sleep(0.3)
    raise SystemExit("did not reach the window")
def kill_agent(jr):
    agent = ssh("pgrep -f '^/usr/local/sbin/skep-agent' | head -1")[1]
    out(step="kill -9", agent=agent, jail_r=jr, after=ssh(f"sudo kill -9 {agent} {jr}; sleep 1; jls -N name")[1].splitlines())

tpl = cid = None
flagged = conf_edited = bridge_made = agent_stopped = False
phase = sys.argv[1] if len(sys.argv) > 1 else "all"
try:
    # --- setup ---
    rc, o = ssh(f"sudo sha256 -q {CONF}")
    out(step="agent.json sha256 before", sha=o, matches=o == WANT_SHA)
    if o != WANT_SHA:
        raise SystemExit("agent.json is not what it was at the start of the run")
    rc, o = ssh("sudo ifconfig bridge1 create && sudo ifconfig bridge1 addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up && ifconfig bridge1")
    bridge_made = rc == 0 or "bridge1" in ssh("ifconfig -l")[1]
    out(step="bridge1", rc=rc, out=o.splitlines())
    rc, o = ssh(f"sudo cp -p {CONF} {BAK} && sudo sed -i '' 's/\"bridges\": \\[\\]/\"bridges\": [\"bridge1\"]/' {CONF} && sudo grep -n bridges {CONF}")
    conf_edited = True
    out(step="agent.json edited", rc=rc, out=o)
    out(step="agent restart", out=ssh("sudo timeout -f 120 service skep_agent restart")[1].splitlines())
    time.sleep(3)
    st, b = api().req("POST", "/storage/templates", {"name": "p57-tpl", "pool": "zroot", "source": "pkgbase"})
    r = task(st, b, "template create"); tpl = b["template"]["id"]
    out(step="template", id=tpl, result=r)
    st, b = api().req("POST", "/containers", {"name": "p57", "template_id": tpl,
                                              "nics": [{"bridge": "bridge1", "ipv4": {"mode": "dhcp"}}]})
    if st != 202:
        raise SystemExit(f"container create: {st} {b}")
    cid = b.get("container", b).get("id")
    out(step="container", id=cid, result=task(st, b, "container create"))
    start(cid)
    ssh(f"sudo jexec skep-{cid} sh -c 'echo sleep 30 > /etc/rc.shutdown.local'")
    out(step="rc.shutdown.local", content=ssh(f"sudo jexec skep-{cid} cat /etc/rc.shutdown.local")[1],
        lease=ssh(f"sudo jexec skep-{cid} ifconfig skep0 inet | grep inet || true")[1])

    # --- B: the j3kill success case ---
    show("B before stop", cid)
    jr = catch_stop(cid)
    kill_agent(jr)
    out(step="B agent back after (s)", seconds=wait_agent(cid))
    show("B after restart", cid)
    host_state("B host")
    out(step="B log", lines=loglines(cid, 4))

    # --- C: agent stopped during a completion ---
    start(cid)
    ssh(f"sudo jexec skep-{cid} sh -c 'echo sleep 30 > /etc/rc.shutdown.local'")
    show("C before stop", cid)
    n0 = logcount(cid, "jail_stop_completing")
    jr = catch_stop(cid)
    kill_agent(jr)
    t0 = time.time()
    while logcount(cid, "jail_stop_completing") == n0:
        if time.time() - t0 > 60:
            raise SystemExit("the restarted agent did not log jail_stop_completing")
        time.sleep(0.5)
    out(step="C completion started", seconds_after_kill=round(time.time() - t0, 1),
        jail_r=ssh(f"pgrep -lf 'jail -f .* -r skep-{cid}' || true")[1])
    time.sleep(3)
    agent_stopped = True
    rc, o = ssh("time sudo timeout -f 120 service skep_agent stop 2>&1")
    out(step="C service skep_agent stop", rc=rc, out=o.splitlines())
    host_state("C host with the agent stopped")
    out(step="C log", lines=loglines(cid, 3))
    # A copy of agent.db, taken with the agent stopped, read here.
    rc, d = ssh("sudo sh -c 'd=$(mktemp -d /tmp/p57db.XXXXXX); cp -p /var/db/skep/agent/agent.db* $d/ && chown -R aba $d && echo $d'")
    local = os.path.join(SCR, "p57db"); os.makedirs(local, exist_ok=True)
    subprocess.run(["scp", "-q", "-o", "BatchMode=yes", f"{ip}:{d}/*", local + "/"], check=True, timeout=60)
    ssh(f"rm -rf {d}")
    db = sqlite3.connect(f"file:{local}/agent.db?mode=ro", uri=True)
    row = db.execute("SELECT desired_state, observed_state, observed_detail FROM containers WHERE id = ?", (cid,)).fetchone()
    db.close()
    for f in os.listdir(local):
        os.remove(os.path.join(local, f))
    out(step="C recorded with the agent stopped (agent.db copy)", desired_state=row[0], observed_state=row[1], observed_detail=row[2])
    out(step="C service skep_agent start", out=ssh("sudo timeout -f 120 service skep_agent start")[1].splitlines())
    agent_stopped = False
    out(step="C agent back after (s)", seconds=wait_agent(cid))
    show("C after the next start", cid)
    host_state("C host")
    out(step="C log", lines=loglines(cid, 3))

    # --- D: induced failure (schg on the jail's jail.conf) ---
    start(cid)
    ssh(f"sudo jexec skep-{cid} sh -c 'echo sleep 30 > /etc/rc.shutdown.local'")
    conf = f"/var/run/skep/jails/{cid}/jail.conf"
    m0 = ssh(f"sudo stat -f %m {conf}")[1]
    show("D before stop", cid)
    jr = catch_stop(cid, need_mtime=m0)
    rc, o = ssh(f"sudo chflags schg {conf}")
    flagged = True
    out(step="D schg", rc=rc, ls=ssh(f"sudo ls -lo {conf}")[1])
    if rc != 0:
        raise SystemExit("chflags failed")
    kill_agent(jr)
    out(step="D agent back after (s)", seconds=wait_agent(cid))
    show("D GET after the failed completion (1st read)", cid)
    show("D GET after the failed completion (2nd read)", cid)
    st, l = api().req("GET", "/containers")
    out(step="D list", items=[{k: i.get(k) for k in ("name", "state", "desired_state", "jid", "state_detail")} for i in l.get("items", [])])
    out(step="D log", lines=loglines(cid, 3))
    st, b = api().req("POST", f"/containers/{cid}/actions", {"action": "stop"})
    out(step="D stop task with the flag still set", result=task(st, b, "stop"))
    show("D GET after the failed stop task", cid)
finally:
    out(step="cleanup")
    if agent_stopped:
        out(step="agent start", out=ssh("sudo timeout -f 120 service skep_agent start")[1])
    if cid:
        conf = f"/var/run/skep/jails/{cid}/jail.conf"
        if flagged:
            out(step="noschg", rc=ssh(f"sudo chflags noschg {conf}")[0], ls=ssh(f"sudo ls -lo {conf}")[1])
        ssh(f"sudo jexec skep-{cid} rm -f /etc/rc.shutdown.local")
        st, c = api().req("GET", f"/containers/{cid}")
        if st == 200 and c.get("state") != "stopped":
            st, b = api().req("POST", f"/containers/{cid}/actions", {"action": "stop"})
            out(step="final stop", result=task(st, b, "stop") if st == 202 else (st, b))
        show("after the final stop (Part 5b: detail cleared?)", cid)
        st, b = api().req("DELETE", f"/containers/{cid}")
        out(step="delete container", result=task(st, b, "delete") if st == 202 else (st, b))
    if tpl:
        st, b = api().req("DELETE", f"/storage/templates/{tpl}")
        out(step="delete template", result=task(st, b, "delete") if st == 202 else (st, b))
    if conf_edited:
        rc, o = ssh(f"sudo cp -p {BAK} {CONF} && sudo sha256 -q {CONF}")
        out(step="agent.json restored", sha=o, matches=o == WANT_SHA)
        if o == WANT_SHA:
            ssh(f"sudo rm -f {BAK}")
        out(step="agent restart", out=ssh("sudo timeout -f 120 service skep_agent restart")[1].splitlines())
    if bridge_made:
        out(step="bridge1 destroy", rc=ssh("sudo ifconfig bridge1 destroy")[0])
    _, v = ssh("ifconfig vtnet1 | head -1")
    if "<UP," in v:
        ssh("sudo ifconfig vtnet1 down")
    out(step="end", vtnet1=ssh("ifconfig vtnet1 | head -1")[1], ifaces=ssh("ifconfig -l")[1], jls=ssh("jls -N name")[1],
        datasets=ssh("zfs list -H -o name -r zroot/skep")[1].splitlines(),
        runfiles=ssh("sudo ls /var/run/skep/jails 2>&1")[1], schg=ssh("sudo find /var/run/skep /var/db/skep -flags +schg 2>&1")[1],
        conf_sha=ssh(f"sudo sha256 -q {CONF}")[1], bak=ssh(f"sudo ls {BAK} 2>&1")[1],
        agent=ssh("/usr/local/sbin/skep-agent -version; sudo service skep_agent status; sudo service skep_api status")[1].splitlines())
