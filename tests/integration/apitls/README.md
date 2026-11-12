# TLS integration test

`apitls-test` runs the real `skep-api` binary as an ordinary user, in a
scratch directory, and checks its TLS handling end to end (ADR 0006
decision 6). It needs no privileges, no agent, and no installed service,
and it removes its scratch directory when it is done. It listens on
`127.0.0.1:18443` while it runs.

| Part | What it checks |
|---|---|
| First start | Key and certificate are generated; state and TLS directories `0700`, key `0600`; ECDSA P-256; 397 days; names include the hostname, `localhost`, and every address of the host; TLS 1.3 negotiated; a TLS 1.2 client is refused; plain HTTP gets no API response; no HSTS; `tls fingerprint` prints the pin the server presents |
| Renewal | A certificate for the same key that expires in 10 days is put on disk and the server gets `SIGHUP`: it issues a new 397-day certificate with the same key and names, without restarting; a connection opened before still works; the renewal is in the access audit log |
| `tls regenerate` | New key; the running server picks it up; the audit entry has the actor `local:<uid>` and both key fingerprints; a restart keeps the pair |
| Damaged files | With a broken key file `skep-api` refuses to start, names `tls regenerate`, and does not overwrite anything |
| Operator certificate | Served as configured; nothing generated; HSTS sent; replaced files are loaded on `SIGHUP`; a certificate that does not match the key is refused and logged while the old one stays; the operator's files are never written; `tls regenerate` refuses; changed files are also picked up without a signal within the one-minute check |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64/skep-api bin/freebsd-amd64-test/apitls-test hearst:skep-m4/

# On hearst, as an ordinary user
~/skep-m4/apitls-test -bin ~/skep-m4/skep-api
```

## Result

FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12), `skep-api` at `94042c7`, as
the ordinary user `aba` (uid 1001): **PASS**, 40 checks. Excerpt:

```
ok     key file mode 0600 (want 0600)
ok     negotiated TLS version 0x0304 (want 0x0304, TLS 1.3)
ok     a client limited to TLS 1.2 is refused (remote error: tls: protocol version not supported)
ok     the key is ECDSA P-256 (*ecdsa.PublicKey)
ok     validity is 397 days (397.00 days left)
ok     DNS names [hearst.greenbean.org hearst localhost] include the hostname "hearst.greenbean.org" and localhost
ok     IP addresses [127.0.0.1 ::1 10.79.20.170] include every address of this host (0 missing)
ok     no Strict-Transport-Security with the self-signed certificate ("")
== renewal on a live server: same key, new certificate, no restart
ok     after SIGHUP the server presents a new certificate (serial 3439...)
ok     the public key is unchanged: sha256:01e834dd...
ok     the new certificate is valid for 397 days (397.00)
ok     skep-api was not restarted (pid 3315)
ok     the connection opened before the renewal still works
ok     the renewal is in the access audit log
== 'tls regenerate' creates a new key, audited, and the server picks it up
ok     the key change is in the access audit log with actor local:1001 and both fingerprints
== broken generated files are refused, not replaced
ok     skep-api refuses to start and names 'tls regenerate' (exit status 1)
== operator-supplied certificate
ok     Strict-Transport-Security is sent with an operator certificate ("max-age=63072000")
ok     after SIGHUP the server presents the replaced certificate
ok     a certificate that does not match the key is not loaded; the old one stays
ok     changed files are picked up without a signal
```

Not covered:

- The generated key and certificate under the real `skep` account in
  `/var/db/skep/api/tls`. That is checked by `rcd-test api`.
- A real 30-day wait: the renewal is triggered by putting a near-expiry
  certificate for the same key on disk.
