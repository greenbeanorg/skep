#!/usr/bin/env python3
"""zapi.py: Skep API client for the zvol device-node API validation (Job 1).
Token from the skepctl session dir; the server cert is fetched, its SPKI
sha256 checked against skepctl's pin, then trusted as the only CA. No
password is read here."""
import base64, hashlib, json, os, ssl, subprocess, sys, time, urllib.request, urllib.error

S = os.path.dirname(os.path.abspath(__file__))
HOSTS = {"smith": "10.79.20.81", "langrishe": "10.79.20.82"}


class API:
    def __init__(self, name):
        self.name, ip = name, HOSTS[name]
        d = os.path.join(S, "skepctl-" + name)
        srv = f"https://{ip}:8443"
        sess = json.load(open(os.path.join(d, "sessions.json")))["sessions"]
        self.token = sess[srv]["token"] if srv in sess else list(sess.values())[0]["token"]
        pin = json.load(open(os.path.join(d, "pins.json")))["servers"][srv]["public_key_sha256"].split(":", 1)[1]
        pem = os.path.join(S, f"{name}-api.pem")
        cert = ssl.get_server_certificate((ip, 8443))
        spki = subprocess.run(["sh", "-c", "openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum"],
                              input=cert.encode(), capture_output=True, check=True).stdout.split()[0].decode()
        if spki != pin:
            raise SystemExit(f"{name}: server key {spki} does not match the pin {pin}")
        open(pem, "w").write(cert)
        self.ctx = ssl.create_default_context(cafile=pem)
        self.ctx.check_hostname = False  # cert is pinned by SPKI above and is the only trusted CA
        self.base = srv + "/api/v1"

    def req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method)
        r.add_header("Content-Type", "application/json")
        r.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(r, context=self.ctx, timeout=120) as resp:
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else None)
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, raw.decode(errors="replace")

    def wait(self, tid, timeout=600):
        end = time.time() + timeout
        while time.time() < end:
            st, t = self.req("GET", "/tasks/" + tid)
            if st == 200 and t["status"] in ("succeeded", "failed", "cancelled", "interrupted"):
                return t
            time.sleep(0.5)
        raise SystemExit(f"task {tid} did not finish")

    def task(self, st, b, what):
        if st != 202:
            raise SystemExit(f"{what}: HTTP {st} {json.dumps(b)[:400]}")
        tid = b.get("id") or b.get("task", {}).get("id") or b.get("task_id")
        return self.wait(tid)
