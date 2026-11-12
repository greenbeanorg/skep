# skepctl integration test

`skepctl-test` runs the real `skep-agent`, `skep-api`, and `skepctl`
binaries as an ordinary user, in a scratch directory, and checks `skepctl`
end to end (Milestone 4b; ADR 0006 decisions 3, 5, and 6), together with
the local commands `skep-api admin disable` and `admin delete`. The agent
and the API run in development mode with their state in the scratch
directory, the API on `127.0.0.1:18443`. `SKEPCTL_CONFIG_DIR` points
`skepctl` into the scratch directory too. Nothing outside it is changed,
and it is removed at the end, also when the test aborts.

`skepctl` runs on a pseudo-terminal for the trust question and the
password, and in a new session without a controlling terminal for
everything else, as it would in a script.

| Part | What it checks |
|---|---|
| First contact without a terminal | Login fails; the error names `--trust-key` with the pin `skep-api tls fingerprint` prints; nothing is stored; no HTTP request reaches `skep-api` (only a handshake); a `--trust-key` that does not match is refused |
| First contact on a terminal | The pin is shown and the question asked on the terminal; the password prompt follows, with echo off; the password never appears on the terminal; login succeeds |
| State directory | `0700`, both files `0600` and owned by the user; the pin and a bearer token are stored |
| Read-only commands | `system` (text and `--json`), `hardware` for the node from the system document, `tasks list` with the startup `discovery.refresh` task, `tasks show`, a bad status refused by the server, `audit list` with the bearer login |
| Loose permissions | A `0644` token file and a `0755` directory are refused with the `chmod` that fixes them |
| Changed key | After `skep-api tls regenerate` and `SIGHUP`: every command and login is refused with both pins shown, no HTTP request reaches the server (neither token nor password), the token is kept; `login --trust-key NEW` replaces the pin and ends the previous session on the server |
| Logout | Ends the session; the token is removed; later commands say "not logged in" |
| Session ended on the server | `skep-api admin disable` of the logged-in user: the next command says the session has ended and the token is removed |
| Last administrator | `admin disable` and `admin delete --yes` of the only enabled administrator are refused; `delete` without `--yes` does nothing; deleting another user works; all of it, refusals included, is in the access audit log as `local:<uid>` |
| Operator-supplied certificate | `skep-api` restarted with an operator certificate from a private CA: the pinned self-signed key no longer matches and the error points to `--ca-file`; `--system-roots` refuses the private CA; `login --ca-file` switches from the pin to chain verification; the operator renews the certificate **with a new key** and `SIGHUP`: commands keep working without a new login; a certificate from another CA is refused with no HTTP request reaching the server |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api bin/freebsd-amd64/skepctl \
    bin/freebsd-amd64-test/skepctl-test hearst:skep-m4b/

# On hearst, as an ordinary user
cd ~/skep-m4b && ./skepctl-test -dir .
```

## Result

FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12), all binaries at `7828260`,
as the ordinary user `aba` (uid 1001): **PASS**, 72 checks. (The first run,
at `b4c8001` before the operator-certificate phase existed, passed its 62
checks.) The checks, with long messages cut:

```
== setup: agent and API in development mode, one administrator
ok     skep-api admin create andy: Administrator "andy" created (user ID 71b1c9b3-12d8-4f5e-aaaf-1d84142cacfe).
ok     skep-api answers on 127.0.0.1:18443
== first contact without a terminal: refused, the key is shown, nothing stored
ok     login fails (exit 1)
ok     the error names --trust-key with the pin skep-api prints
ok     the certificate's key is shown for comparison
ok     no pin was stored
ok     no HTTP request reached skep-api (only a handshake)
ok     a --trust-key that does not match is refused: The server https://127.0.0.1:18443 presented this certificate:
== first contact on a terminal: the key is confirmed, the password typed without echo
ok     skepctl asks on the terminal
ok     the terminal shows the pin skep-api prints
ok     then asks for the password
ok     echo is off while the password is typed
ok     logged in
ok     exit status 0
ok     the terminal never showed the password
== the state directory: 0700, files 0600, owned by the user
ok     state directory mode 0700 (want 0700)
ok     pins.json mode 0600 (want 0600)
ok     pins.json owned by uid 1001
ok     sessions.json mode 0600 (want 0600)
ok     sessions.json owned by uid 1001
ok     pins.json holds the confirmed pin
ok     sessions.json holds a bearer token for andy, and the current server
== read-only commands
ok     system (text): platform freebsd, the TLS public key shown
ok     system --json: node c58dfe4d-0178-4b13-8f85-3bd437b58625, platform freebsd
ok     hardware of this node (from the system document)
ok     tasks list shows the startup discovery.refresh task
ok     tasks list --json --limit 1
ok     tasks show 0e4ddbf8-b0b4-4c30-a497-f4854f5e8e79, with its events
ok     a bad status is refused by the server: skepctl: unknown task status bogus (HTTP 400, invalid_request), request cb411084e8c598d5cb30032c
ok     audit list: andy's login is recorded
ok     the login was a bearer session (bearer)
== a token file readable by others is refused
ok     refused with mode 0644: skepctl: /tmp/skep-skepctlN/home/.config/skepctl/sessions.json is accessible by other users (mode 0644) and is
ok     refused with a 0755 directory: skepctl: /tmp/skep-skepctlN/home/.config/skepctl is accessible by other users (mode 0755); run: chmod 7
== a changed key: refused before anything is sent
ok     skep-api tls regenerate
ok     the key changed: sha256:1c5ca1a95e919dbb648e54a3a471537c2a61f8bf98f6c57f41dc62bfa45f02ad
ok     system refuses and shows both pins
ok     no HTTP request reached skep-api (the token was not sent)
ok     the token is kept
ok     login without --trust-key is refused too
ok     the password was not sent
ok     login --trust-key with the new pin replaces it: Replacing the pinned key of https://127.0.0.1:18443:
ok     system works with the new key
ok     the previous session was ended on the server by the new login
== logout
ok     logout: Logged out of https://127.0.0.1:18443.
ok     the token is gone from sessions.json
ok     system after logout: skepctl: not logged in to https://127.0.0.1:18443: run skepctl login --server https://127.0.0.1:18443
== a session ended on the server: the token is forgotten
ok     skep-api admin create carol
ok     carol logs in
ok     skep-api admin disable carol: "carol" was disabled and all of that user's sessions were ended.
ok     carol's next command: skepctl: the session with https://127.0.0.1:18443 has ended (expired, logged out, or revoked): run skepctl login
ok     the ended session's token was removed
== the last administrator: neither disabled nor deleted
ok     admin disable andy refused: skep-api: "andy" is the only enabled administrator and cannot be disabled; create another administrator first (skep
ok     admin delete andy refused: skep-api: "andy" is the only enabled administrator and cannot be deleted; create another administrator first (skep-a
ok     admin delete carol without --yes does nothing: skep-api: deleting "carol" (role admin) cannot be undone; run the command again with --yes to co
ok     admin delete carol --yes: "carol" was deleted and all of that user's sessions were ended.
ok     andy logs in again
ok     audit: the disable of carol, by local:1001
ok     audit: the refused disable of andy
ok     audit: the refused delete of andy
ok     audit: the delete of carol
ok     audit: carol's session revoked by the disable
== an operator-supplied certificate: chain verification, renewals with a new key
ok     skep-api answers with the operator certificate
ok     the pinned self-signed key no longer matches; the error points to --ca-file
ok     --system-roots refuses the private CA: skepctl: the certificate of https://127.0.0.1:18443 does not verify against the system roots; nothing wa
ok     login --ca-file switches from the pin to chain verification: Switching https://127.0.0.1:18443 from the pinned key sha256:1c5ca1a95e919dbb648e5
ok     pins.json holds mode ca and the CA file
ok     system works
ok     the operator renews the certificate with a new key
ok     system works after the renewal, without a new login; the server shows the new key
ok     a certificate from another CA is refused: skepctl: the certificate of https://127.0.0.1:18443 is not trusted; nothing was sent to it: Get "http
ok     no HTTP request reached skep-api
ok     logout
PASS
```

The message for a changed key, as shown (the pins are from the first run):

```
skepctl: the public key of https://127.0.0.1:18443 has changed; nothing was sent to it.
  pinned:    sha256:da6faf4c06f2c3f646c20df0780627d13e675ca83f0a527db4265044c1f4e821
  presented: sha256:e96a9580aa967031a806d5a67e96691a8323a466b871c3cedc1276999140bcf3
This happens after "skep-api tls regenerate" on the host, and it is also what
an attack in the middle looks like. Run "skep-api tls fingerprint" on the host.
Only if it prints the presented key, accept it with:
  skepctl login --server https://127.0.0.1:18443 --username NAME --trust-key sha256:e96a…
If the server now has an operator-supplied certificate, use --ca-file FILE or
--system-roots instead of --trust-key
```

The unit tests of `pkg/client`, `internal/ctlstate`, `cmd/skepctl`, and
`cmd/skep-api` were also run natively on hearst (`go test -c` for FreeBSD,
run there): all pass.

The unit tests of `pkg/client`, `internal/ctlstate`, and `cmd/skepctl`
were run natively on hearst again at `7828260`: all pass.

## rc.d service and another host (manual run)

Approved separately and run by hand on 2026-11-12, binaries at `bb03881`.
The rc.d variables pointed at the binaries in `~/skep-m4b`; `skep-api`
used a scratch state directory (`skep_api_data_dir=/var/tmp/skep-m4b`) so
that the throwaway administrator `skepctl-check` did not stay in the real
`api.db`, where the last-administrator rule would have kept it; the agent
used its real state. `skep_api_args="-listen 10.79.20.170:8443"` opened
the API to the management network for the run.

- Under rc.d: the agent ran as root, `skep-api` as `skep`, listening on
  `10.79.20.170:8443` only.
- From merrick (Linux `skepctl`, `10.79.20.80`): first contact without a
  terminal refused and showed the pin, which was the one
  `skep-api tls fingerprint` printed on the host; a wrong `--trust-key`
  refused; `--trust-key` with the right pin logged in; `system` (the real
  node, `production host`), `hardware`, `tasks list`, `tasks show`, and
  `audit list` all worked; the login is audited from `10.79.20.80`.
- On hearst (FreeBSD `skepctl`), against the same service at the same
  time: login, `system`, `audit list`, logout.
- The access audit log then showed two separate logouts: the one on hearst
  ended only its own session, and merrick's session stayed valid until
  merrick logged out.
- Back to loopback (`sysrc -x skep_api_args`, restart): `skep-api`
  listened on `127.0.0.1:8443` only, and merrick got "connection refused".
- Afterwards both services were stopped, every rc.conf entry was removed
  (`/etc/rc.conf` byte-identical to before, by SHA-256), and the scratch
  state directory was deleted. The only lasting change is the agent's own
  history in `agent.db` (one startup `discovery.refresh` task).

Not covered:

- `--system-roots` with a certificate from a public CA. The lab has
  none; the check that a private CA is refused with `--system-roots`, and
  the unit tests, cover the code path.
- The trust question and password prompt on the system console (as for
  the `skep-api admin` prompt).
