#!/usr/bin/env python3
"""8e1fix-api.py (from 8e1-api.py; 8e1fix prints each audit entry's request_id
and adds taskget HOST USER ID NODE: GET /tasks/ID?node=NODE, status and the
task's ID, node and status): 8e1-api.py (from 8d2-api.py): the step 8e-1 lab's client of skep-api on smith, langrishe,
star and blazanov (run on merrick). From 8c2-api.py; adds an optional
Idempotency-Key (env IDEM_KEY) and prints the Skep-Quorum header.
8e1 prints the Skep-Node and X-Request-ID headers of every answer and adds:
jailid HOST USER NAME, cluster HOST USER (each member's API pin),
fwdhealth HOST USER (the node_forwarding check).

TLS: the server's certificate is fetched, the SHA-256 of its public key is
compared with what "sudo skep-api tls fingerprint" prints on the host over
ssh, and only then is it trusted, as the only CA. A changed key stops the
script.

Passwords are read from files (a ~/skep-lab-creds file, or a 0600 file the
lab made for a temporary user) and sent only in the login body. They are
never printed, logged or kept. Session tokens are kept in STATE (a 0600
file in the scratchpad, deleted at the end of the run), never printed.

8d2 adds: mkjail HOST USER NAME (from template lab-tpl, waits for the task),
rmjail HOST USER NAME (waits), jails HOST USER, task HOST USER ID,
remove HOST USER NAME CLUSTERID (the public remove).

Usage:
  8c1-api.py login HOST USER PWFILE          prints status, degraded, minutes left
  8c1-api.py req HOST USER METHOD PATH [JSON] prints status and the body (no secrets)
  8c1-api.py health HOST [USER]              prints the health document's cluster_users check
  8c1-api.py users HOST USER                 prints "id username role disabled" lines
  8c1-api.py create HOST USER NAME PWFILE ROLE  creates a user, the password read from PWFILE
  8c1-api.py setpw HOST USER ID PWFILE       sets a user's password from PWFILE
  8c2-api.py audit HOST USER ACTION          prints the newest access audit entries with ACTION
  8c2-api.py admit HOST USER REQFILE OUTFILE  admits the join request in REQFILE; the admission goes to OUTFILE (0600)
"""
import json, os, ssl, subprocess, sys, time, urllib.error, urllib.request

HOSTS = {"smith": "10.79.20.81", "langrishe": "10.79.20.82", "star": "10.79.20.83", "blazanov": "10.79.20.84"}
STATE = os.environ.get("LAB8E1FIX_STATE", os.environ.get("LAB8E1_STATE", ""))
QUORUM = ""
NODE = ""
RID = ""


def pinned(host):
    ip = HOSTS[host]
    out = subprocess.run(["ssh", "-n", "-o", "BatchMode=yes", "aba@" + ip, "sudo skep-api tls fingerprint"],
                         capture_output=True, text=True, check=True).stdout
    pin = [l.split()[-1] for l in out.splitlines() if l.startswith("public key:")][0].split(":", 1)[1]
    cert = ssl.get_server_certificate((ip, 8443))
    spki = subprocess.run(["sh", "-c", "openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum"],
                          input=cert.encode(), capture_output=True, check=True).stdout.split()[0].decode()
    if spki != pin:
        sys.exit(f"{host}: the server's key {spki} is not the host's pin {pin}")
    pem = os.path.join(os.path.dirname(STATE), host + "-cert.pem")
    with open(pem, "w") as f:
        f.write(cert)
    ctx = ssl.create_default_context(cafile=pem)
    ctx.check_hostname = False  # the only trusted CA, pinned by its key above
    return ctx, f"https://{ip}:8443/api/v1"


def tokens():
    try:
        with open(STATE) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}


def save(t):
    fd = os.open(STATE, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(t, f)


def call(host, method, path, body=None, token=None):
    ctx, base = pinned(host)
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(base + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    if os.environ.get("IDEM_KEY"):
        r.add_header("Idempotency-Key", os.environ["IDEM_KEY"])
    global QUORUM, NODE, RID
    try:
        with urllib.request.urlopen(r, context=ctx, timeout=150) as resp:
            raw = resp.read()
            QUORUM, NODE, RID = resp.headers.get("Skep-Quorum", ""), resp.headers.get("Skep-Node", ""), resp.headers.get("X-Request-ID", "")
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        QUORUM, NODE, RID = e.headers.get("Skep-Quorum", ""), e.headers.get("Skep-Node", ""), e.headers.get("X-Request-ID", "")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw.decode(errors="replace")


def clean(b):
    """The body without a token."""
    if isinstance(b, dict):
        return {k: clean(v) for k, v in b.items() if k != "token"}
    return b


def main():
    if not STATE:
        sys.exit("set LAB8D2_STATE to a file in the scratchpad")
    cmd = sys.argv[1]
    if cmd == "login":
        host, user, pwfile = sys.argv[2:5]
        with open(os.path.expanduser(pwfile)) as f:
            pw = f.read().rstrip("\r\n")
        st, b = call(host, "POST", "/auth/login", {"username": user, "password": pw, "session": "bearer"})
        pw = None
        if st == 200:
            t = tokens()
            t[host + "/" + user] = b["token"]
            save(t)
            exp = b["session"]["expires_at"]
            left = (time.mktime(time.strptime(exp[:19], "%Y-%m-%dT%H:%M:%S")) - time.mktime(time.gmtime())) / 60
            print(f"{host} login {user}: 200 degraded={b['session'].get('degraded')} minutes_left={left:.0f}")
        else:
            print(f"{host} login {user}: {st} {json.dumps(clean(b))}")
    elif cmd == "req":
        host, user, method, path = sys.argv[2:6]
        body = json.loads(sys.argv[6]) if len(sys.argv) > 6 else None
        st, b = call(host, method, path, body, tokens().get(host + "/" + user))
        q = f" [Skep-Quorum: {QUORUM}]" if QUORUM else ""
        q += f" [Skep-Node: {NODE or '-'}] [X-Request-ID: {RID}]"
        print(f"{host} {method} {path} as {user}: {st}{q} {json.dumps(clean(b))[:900]}")
    elif cmd == "jailid":
        host, user, name = sys.argv[2:5]
        st, b = call(host, "GET", "/containers?limit=200", None, tokens().get(host + "/" + user))
        ids = [c["id"] for c in b.get("items", []) if c["name"] == name] if st == 200 else []
        print(ids[0] if ids else "")
    elif cmd == "cluster":
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/cluster", None, tokens().get(host + "/" + user))
        if st != 200:
            print(f"{host} cluster: {st} {json.dumps(clean(b))}")
            return
        for m in b["members"]:
            print(f"{host} member {m['name']} {m['id']} {m['state']} api_pin={m.get('api_pin', '-')}")
    elif cmd == "fwdhealth":
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/health", None, tokens().get(host + "/" + user))
        c = b.get("checks", {}).get("node_forwarding") if isinstance(b, dict) else b
        print(f"{host} node_forwarding: {json.dumps(c)}")
    elif cmd == "health":
        host = sys.argv[2]
        tok = tokens().get(host + "/" + sys.argv[3]) if len(sys.argv) > 3 else None
        st, b = call(host, "GET", "/health", None, tok)
        c = b.get("checks", {}).get("cluster_users") if isinstance(b, dict) else b
        print(f"{host} health: {st} status={b.get('status') if isinstance(b, dict) else '?'} cluster_users={json.dumps(c)}")
    elif cmd == "users":
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/users", None, tokens().get(host + "/" + user))
        if st != 200:
            print(f"{host} users: {st} {json.dumps(clean(b))}")
            return
        for u in b["items"]:
            print(f"{u['id']} {u['username']} {u['role']} disabled={u['disabled']}")
    elif cmd in ("create", "setpw"):
        host, user, target, pwfile = sys.argv[2:6]
        with open(pwfile) as f:
            pw = f.read().strip()
        if cmd == "create":
            body, method, path = {"username": target, "password": pw, "role": sys.argv[6]}, "POST", "/users"
        else:
            body, method, path = {"password": pw}, "PUT", "/users/" + target
        pw = None
        st, b = call(host, method, path, body, tokens().get(host + "/" + user))
        body = None
        print(f"{host} {method} {path} as {user}: {st} {json.dumps(clean(b))[:600]}")
    elif cmd == "admit":
        # 8c2fix: the admission goes to a 0600 file, never to the output.
        host, user, reqfile, outfile = sys.argv[2:6]
        with open(reqfile) as f:
            req = f.read().strip()
        st, b = call(host, "POST", "/cluster/admissions", {"request": req}, tokens().get(host + "/" + user))
        if st == 200:
            fd = os.open(outfile, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w") as f:
                f.write(b["admission"])
            b = {k: v for k, v in b.items() if k != "admission"}
        print(f"{host} POST /cluster/admissions as {user}: {st} {json.dumps(clean(b))[:600]}")
    elif cmd == "guests":
        # 8d1: the inventory, one line per item (name kind owner state flagged task)
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/cluster/guests", None, tokens().get(host + "/" + user))
        if st != 200:
            print(f"{host} guests: {st} {json.dumps(clean(b))}")
            return
        print(f"{host} guests: index {b['guests_index']}, {len(b['items'])} items")
        for i in b["items"]:
            print(f"  {i['kind']} {i['name']} {i.get('id','-')} owner={i['owner']['node_name']} {i['state']} flagged={i['flagged']} task={i.get('task_id','-')}")
    elif cmd == "taskcount":
        # 8d1: the number of tasks the agent lists (newest 200), by type
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/tasks?limit=200", None, tokens().get(host + "/" + user))
        types = {}
        for t in b.get("items", []):
            types[t["type"]] = types.get(t["type"], 0) + 1
        print(f"{host} tasks: {sum(types.values())} {json.dumps(types, sort_keys=True)}")
    elif cmd == "audit":
        host, user, action = sys.argv[2:5]
        st, b = call(host, "GET", "/audit?limit=100", None, tokens().get(host + "/" + user))
        if st != 200:
            print(f"{host} audit: {st} {json.dumps(clean(b))}")
            return
        for e in b["items"]:
            if e["action"] == action:
                print(f"{host} audit {e['occurred_at']} {e['actor_name']} {e['action']} {e['outcome']} rid={e.get('request_id','')} {e.get('resource_id','')} {json.dumps(e.get('details'))}")
    elif cmd in ("mkjail", "rmjail"):
        host, user, name = sys.argv[2:5]
        tok = tokens().get(host + "/" + user)
        if cmd == "mkjail":
            st, b = call(host, "GET", "/storage/templates?limit=200", None, tok)
            tpl = [t["id"] for t in b.get("items", []) if t["name"] == "lab-tpl"] if st == 200 else []
            if not tpl:
                print(f"{host} mkjail {name}: no template lab-tpl ({st})")
                return
            st, b = call(host, "POST", "/containers", {"name": name, "template_id": tpl[0]}, tok)
        else:
            st, b = call(host, "GET", "/containers?limit=200", None, tok)
            ids = [c["id"] for c in b.get("items", []) if c["name"] == name] if st == 200 else []
            if not ids:
                print(f"{host} rmjail {name}: no such jail ({st})")
                return
            st, b = call(host, "DELETE", "/containers/" + ids[0], None, tok)
        if st != 202:
            print(f"{host} {cmd} {name}: {st} {json.dumps(clean(b))[:600]}")
            return
        task = b.get("task", b)
        tid = task["id"]
        for _ in range(240):
            st2, t = call(host, "GET", "/tasks/" + tid, None, tok)
            if st2 == 200 and t["status"] in ("succeeded", "failed", "cancelled", "interrupted"):
                break
            time.sleep(1)
        print(f"{host} {cmd} {name}: 202 task {tid} {t.get('status')} {json.dumps(t.get('error'))[:300]}")
    elif cmd == "taskget":
        host, user, tid, node = sys.argv[2:6]
        st, b = call(host, "GET", "/tasks/" + tid + "?node=" + node, None, tokens().get(host + "/" + user))
        if st == 200:
            print(f"{host} GET /tasks/{tid}?node={node}: 200 id={b.get('id')} node_id={b.get('node_id')} status={b.get('status')}")
        else:
            print(f"{host} GET /tasks/{tid}?node={node}: {st} {json.dumps(clean(b))[:400]}")
    elif cmd == "firsttask":
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/tasks?limit=1", None, tokens().get(host + "/" + user))
        items = b.get("items", []) if isinstance(b, dict) else []
        print(items[0]["id"] if st == 200 and items else f"none ({st})")
    elif cmd == "jails":
        host, user = sys.argv[2:4]
        st, b = call(host, "GET", "/containers?limit=200", None, tokens().get(host + "/" + user))
        names = sorted(c["name"] for c in b.get("items", [])) if st == 200 else b
        print(f"{host} jails: {st} {names}")
    elif cmd == "task":
        host, user, tid = sys.argv[2:5]
        st, t = call(host, "GET", "/tasks/" + tid, None, tokens().get(host + "/" + user))
        print(f"{host} task {tid}: {st} {t.get('status') if isinstance(t, dict) else t}")
    elif cmd == "remove":
        host, user, name, cid = sys.argv[2:6]
        st, b = call(host, "POST", "/cluster/members/" + name + "/remove", {"cluster_id": cid}, tokens().get(host + "/" + user))
        print(f"{host} POST /cluster/members/{name}/remove as {user}: {st} {json.dumps(clean(b))[:900]}")
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
