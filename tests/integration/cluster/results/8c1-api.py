#!/usr/bin/env python3
"""8c1-api.py: the step 8c-1 lab's client of skep-api on smith, langrishe
and star (run on merrick).

TLS: the server's certificate is fetched, the SHA-256 of its public key is
compared with what "sudo skep-api tls fingerprint" prints on the host over
ssh, and only then is it trusted, as the only CA. A changed key stops the
script.

Passwords are read from files (a ~/skep-lab-creds file, or a 0600 file the
lab made for a temporary user) and sent only in the login body. They are
never printed, logged or kept. Session tokens are kept in STATE (a 0600
file in the scratchpad, deleted at the end of the run), never printed.

Usage:
  8c1-api.py login HOST USER PWFILE          prints status, degraded, minutes left
  8c1-api.py req HOST USER METHOD PATH [JSON] prints status and the body (no secrets)
  8c1-api.py health HOST                     prints the health document's cluster_users check
  8c1-api.py users HOST USER                 prints "id username role disabled" lines
"""
import json, os, ssl, subprocess, sys, time, urllib.error, urllib.request

HOSTS = {"smith": "10.79.20.81", "langrishe": "10.79.20.82", "star": "10.79.20.83"}
STATE = os.environ.get("LAB8C1_STATE", "")


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
    try:
        with urllib.request.urlopen(r, context=ctx, timeout=60) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
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
        sys.exit("set LAB8C1_STATE to a file in the scratchpad")
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
        print(f"{host} {method} {path} as {user}: {st} {json.dumps(clean(b))[:600]}")
    elif cmd == "health":
        host = sys.argv[2]
        st, b = call(host, "GET", "/health")
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
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
