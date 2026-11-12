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

FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-12), all binaries at `b4c8001`,
as the ordinary user `aba` (uid 1001): **PASS**, 62 checks. The output, without the
two checks whose messages span several lines:

```
== setup: agent and API in development mode, one administrator
ok     skep-api admin create andy: Administrator "andy" created (user ID 8ff9779a-50e3-4358-bcfb-1188d1af1cee).
ok     skep-api answers on 127.0.0.1:18443
== first contact without a terminal: refused, the key is shown, nothing stored
ok     login fails (exit 1)
ok     the error names --trust-key with the pin skep-api prints
ok     the certificate's key is shown for comparison
ok     no pin was stored
ok     no HTTP request reached skep-api (only a handshake)
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
ok     system --json: node 97c0efbb-b3f3-4436-890d-8cb31ca84a5a, platform freebsd
ok     hardware of this node (from the system document)
ok     tasks list shows the startup discovery.refresh task
ok     tasks list --json --limit 1
ok     tasks show 386dc1c1-0342-4079-aef8-5646f4eb52a6, with its events
ok     a bad status is refused by the server: skepctl: unknown task status bogus (HTTP 400, invalid_request), request 3301568bb17626948115a03b
ok     audit list: andy's login is recorded
ok     the login was a bearer session (bearer)
== a token file readable by others is refused
ok     refused with mode 0644: skepctl: /tmp/skep-skepctlN/home/.config/skepctl/sessions.json is accessible by other users (mode 0644) and is
ok     refused with a 0755 directory: skepctl: /tmp/skep-skepctlN/home/.config/skepctl is accessible by other users (mode 0755); run: chmod 7
== a changed key: refused before anything is sent
ok     skep-api tls regenerate
ok     the key changed: sha256:e96a9580aa967031a806d5a67e96691a8323a466b871c3cedc1276999140bcf3
ok     system refuses and shows both pins
ok     no HTTP request reached skep-api (the token was not sent)
ok     the token is kept
ok     login without --trust-key is refused too
ok     the password was not sent
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
ok     logout
PASS
```

The message for a changed key, as shown:

```
skepctl: the public key of https://127.0.0.1:18443 has changed; nothing was sent to it.
  pinned:    sha256:da6faf4c06f2c3f646c20df0780627d13e675ca83f0a527db4265044c1f4e821
  presented: sha256:e96a9580aa967031a806d5a67e96691a8323a466b871c3cedc1276999140bcf3
This happens after "skep-api tls regenerate" on the host, and it is also what
an attack in the middle looks like. Run "skep-api tls fingerprint" on the host.
Only if it prints the presented key, accept it with:
  skepctl login --server https://127.0.0.1:18443 --username NAME --trust-key sha256:e96a…
```

The unit tests of `pkg/client`, `internal/ctlstate`, `cmd/skepctl`, and
`cmd/skep-api` were also run natively on hearst (`go test -c` for FreeBSD,
run there): all pass.

Not covered:

- `skepctl` and `skep-api` on different hosts. The API in this test listens
  on loopback. Nothing in `skepctl` depends on the address, but a run
  across the network is still to do.
- `skepctl` against the `skep_api` rc.d service. The services were not
  running on hearst and were not started for this test.
- The trust question and password prompt on the system console (as for
  the `skep-api admin` prompt).
