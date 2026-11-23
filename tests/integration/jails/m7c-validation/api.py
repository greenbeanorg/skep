#!/usr/bin/env python3
"""Small Skep API client for the M7c validation on hearst. Trusts exactly the
pinned certificate (fetched by scripts/e2e-trust.mjs). Passwords come from
~/.skep-test-creds and are never printed."""
import json, os, ssl, sys, time, urllib.request, urllib.error

BASE = "https://10.79.20.170:8443/api/v1"
CA = os.path.join(os.path.dirname(__file__), "..", "hearst-api.pem")
CTX = ssl.create_default_context(cafile=CA)
TOK = os.path.join(os.path.dirname(__file__), ".tokens")

def password(user):
    with open(os.path.expanduser("~/.skep-test-creds")) as f:
        for line in f:
            p = line.split()
            if len(p) == 3 and p[0] == user:
                return p[2]
    raise SystemExit("no such test user")

def req(method, path, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(r, context=CTX, timeout=120) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw.decode(errors="replace")

def token(user):
    os.makedirs(TOK, mode=0o700, exist_ok=True)
    p = os.path.join(TOK, user)
    if os.path.exists(p):
        t = open(p).read().strip()
        st, _ = req("GET", "/system", token=t)
        if st == 200:
            return t
    st, b = req("POST", "/auth/login", {"username": user, "password": password(user), "session": "bearer"})
    if st != 200:
        raise SystemExit(f"login {user}: HTTP {st}")
    t = b.get("token") or b.get("session_token")
    fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.write(fd, t.encode()); os.close(fd)
    print(f"login {user}: HTTP 200 (token not shown)", file=sys.stderr)
    return t

def wait(tok, task_id, timeout=300):
    end = time.time() + timeout
    while time.time() < end:
        st, t = req("GET", "/tasks/" + task_id, token=tok)
        if st == 200 and t["status"] in ("succeeded", "failed", "cancelled", "interrupted"):
            return t
        time.sleep(1)
    raise SystemExit("task timed out: " + task_id)

def task_id(b):
    if isinstance(b, dict):
        if "task" in b and isinstance(b["task"], dict):
            return b["task"]["id"]
        return b.get("id") or b.get("task_id")
    return None

if __name__ == "__main__":
    # api.py USER METHOD PATH [JSON] [--wait]
    user, method, path = sys.argv[1:4]
    body = json.loads(sys.argv[4]) if len(sys.argv) > 4 and sys.argv[4] != "--wait" else None
    tok = token(user)
    st, b = req(method, path, body, tok)
    out = {"http": st, "body": b}
    if "--wait" in sys.argv and st == 202:
        t = wait(tok, task_id(b))
        out["task"] = {k: t.get(k) for k in ("id", "type", "status", "resource_id")}
        # The API's Task carries a failure as error: {code, message}; there
        # are no error_code or error_message fields (they printed null).
        err = t.get("error") or {}
        out["task"]["error_code"] = err.get("code")
        out["task"]["error_message"] = err.get("message")
    print(json.dumps(out, indent=1, default=str))
