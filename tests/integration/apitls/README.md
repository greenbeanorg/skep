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

**Pending FreeBSD validation.** This section is replaced by the recorded
run.
