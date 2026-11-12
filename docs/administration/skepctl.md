# skepctl

`skepctl` is the command-line client for Skep. It uses only the public REST
API under `/api/v1`, exactly as the web UI will, through the Go client in
`pkg/client`. It never reads Skep's databases or files on the host, and it
runs anywhere it can reach `skep-api` over TLS: on the host itself, or on
an administrator's workstation.

It follows [ADR 0006](../decisions/0006-api-split-authentication-tls.md):
a bearer session whose token is kept in a `0600` file (decision 3), and
trust on first use that pins the SHA-256 of the server's public key
(decision 6). There is no flag that skips the key check.

Validated on FreeBSD 15.1-RELEASE as an ordinary user, with the API on
loopback; see [tests/integration/skepctl](../../tests/integration/skepctl/README.md)
for what was run and what is still pending.

## Commands

```sh
skepctl login --server HOST[:PORT] --username NAME [--password-stdin] [--trust-key PIN]
skepctl logout [--server HOST[:PORT]]

skepctl system [--json]
skepctl hardware [--node ID] [--json]
skepctl tasks list [--status S] [--limit N] [--cursor C] [--json]
skepctl tasks show ID [--json]
skepctl audit list [--limit N] [--cursor C] [--json]
skepctl version
```

- The port defaults to `8443`. Only `https` is accepted.
- Every command takes `--server`. Without it, the server of the last
  login is used.
- Text output is for people. `--json` prints the API's response unchanged,
  for scripts. Paged lists print the command for the next page on standard
  error, so standard output stays clean.
- What a user may read is decided by the server: a `viewer` gets `403` from
  `audit list`, for example.

## First login: trusting the server's key

On first contact `skepctl` connects, shows the certificate the server
presented, and asks before it sends anything else:

```
$ skepctl login --server 10.79.20.170 --username andy
The server https://10.79.20.170:8443 presented this certificate:
  public key:   sha256:3f0c…
  subject:      CN=hearst
  names:        hearst, localhost, 10.79.20.170
  valid until:  2027-12-14T10:02:11Z
skepctl has not connected to this server before. Compare the public key with the
output of "skep-api tls fingerprint" on the host before you trust it.
Trust this key? [y/N] y
Password for andy at https://10.79.20.170:8443:
Logged in to https://10.79.20.170:8443 as andy (role admin).
```

Compare the key with what the host prints:

```sh
skep-api tls fingerprint        # on the host, as root or skep
```

Without a terminal, or to skip the question after comparing the key by
other means, pass it: `--trust-key sha256:3f0c…`. A key given this way must
match what the server presents, or nothing is trusted.

The pin is the SHA-256 of the certificate's public key, not of the
certificate. `skep-api` renews its self-signed certificate with the same
key, so renewals do not disturb `skepctl`.

## When the key changes

A new key comes only from `skep-api tls regenerate` on the host (or a new
operator certificate with a new key). `skepctl` then refuses to connect,
sends nothing (not the token, not a password), and says what it expected
and what it got:

```
skepctl: the public key of https://10.79.20.170:8443 has changed; nothing was sent to it.
  pinned:    sha256:3f0c…
  presented: sha256:91aa…
```

That is also what an attack in the middle looks like. Check
`skep-api tls fingerprint` on the host; only if it prints the presented
key, accept it by naming it:

```sh
skepctl login --server 10.79.20.170 --username andy --trust-key sha256:91aa…
```

There is no interactive "accept anyway" for a changed key.

## Sessions

- `login` asks the server for a bearer session (`"session": "bearer"`).
  The server sets no cookie for it and accepts its token only in an
  `Authorization: Bearer` header.
- The token is stored in the state directory; see below. A new login to the
  same server ends the previous session on the server before replacing it.
- `logout` ends the session on the server and then deletes the token. If
  the server cannot be reached, the token is kept so the logout can be
  retried; a session the server already ended is simply forgotten.
- When the server answers `401` (idle or absolute timeout, a password
  change, the user disabled, an administrator's action), `skepctl` deletes
  the token and asks you to log in again.
- The password is read from the terminal without echo. `--password-stdin`
  reads one line from standard input instead. It is never accepted as an
  argument or from the environment.

## State directory

`$SKEPCTL_CONFIG_DIR`, or `skepctl` in `$XDG_CONFIG_HOME` (normally
`~/.config/skepctl`):

| File | Holds | Mode |
|---|---|---|
| (directory) | | `0700` |
| `pins.json` | The trusted public key of each server, and when it was trusted | `0600` |
| `sessions.json` | The bearer token, username, and expiry for each server; the server of the last login | `0600` |

Files are replaced atomically (temporary file, `fsync`, rename). `skepctl`
refuses to use the directory or a file if it belongs to another user, is a
symbolic link, or is readable or writable by others, and says which
`chmod` fixes it. A readable `sessions.json` would leak a token, and a
writable `pins.json` would let someone else choose the key `skepctl`
trusts.

The token is stored as it is, because it must be sent as it is. Anyone who
can read the file as you can use the session until it expires or you log
out. Long-lived tokens for automation are a separate, future credential
type (ADR 0006 decision 3).

## Local administrator commands on the host

These are part of `skep-api`, not `skepctl`. They open `api.db` directly,
have no network path, and are for the host's administrator:

```sh
skep-api admin disable --username NAME       # disable a user; ends that user's sessions
skep-api admin delete  --username NAME --yes # delete a user; ends that user's sessions
```

Both refuse the only enabled administrator, exactly as the API does
(ADR 0006 decision 5), and both are written to the access audit log with
the actor `local:<uid>`, including a refusal. `delete` does nothing without
`--yes`. Through the API the same is `PUT /users/{id}` with
`{"disabled": true}` and `DELETE /users/{id}`.
