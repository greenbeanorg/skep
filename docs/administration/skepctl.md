# skepctl

`skepctl` is the command-line client for Skep. It uses only the public REST
API under `/api/v1`, exactly as the web UI will, through the Go client in
`pkg/client`. It never reads Skep's databases or files on the host, and it
runs anywhere it can reach `skep-api` over TLS: on the host itself, or on
an administrator's workstation.

It follows [ADR 0006](../decisions/0006-api-split-authentication-tls.md):
a bearer session whose token is kept in a `0600` file (decision 3), and
trust on first use that pins the SHA-256 of the server's public key
(decision 6). When the server has an operator-supplied certificate,
`skepctl` instead verifies the certificate chain and host name, like any
TLS client (see [below](#operator-supplied-certificates)). There is no
flag that skips both.

Validated on FreeBSD 15.1-RELEASE in a lab VM nested under KVM
(`hearst`), not on bare metal: as an ordinary user with the API on
loopback, and against the `skep_api` rc.d service from another host; see
[tests/integration/skepctl](../../tests/integration/skepctl/README.md).

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

skepctl vm list [--json]
skepctl vm show VM [--json]
skepctl vm create -f FILE [--wait] [--json]
skepctl vm start VM [--wait] [--json]
skepctl vm stop VM [--timeout SECONDS] [--wait] [--json]
skepctl vm force-stop VM [--wait] [--json]
skepctl vm restart VM [--timeout SECONDS] [--wait] [--json]
skepctl vm delete VM [--wait] [--json]
skepctl vm clone VM NAME [--snapshot SNAPSHOT] [--description TEXT] [--wait] [--json]

skepctl vm snapshot list VM [--json]
skepctl vm snapshot create VM NAME [--description TEXT] [--wait] [--json]
skepctl vm snapshot rollback VM SNAPSHOT [--destroy-newer] [--wait] [--json]
skepctl vm snapshot delete VM SNAPSHOT [--wait] [--json]

skepctl cluster show [--json]
skepctl cluster admit REQUEST [--json]
skepctl cluster withdraw NAME [--json]
skepctl cluster remove NAME [--cluster-id ID] [--json]
```

- The port defaults to `8443`. Only `https` is accepted.
- Every command takes `--server`. Without it, the server of the last
  login is used.
- Text output is for people. `--json` prints the API's response unchanged,
  for scripts. Paged lists print the command for the next page on standard
  error, so standard output stays clean. In text output, control
  characters in values from the server become spaces and Unicode format
  characters (such as the bidirectional overrides) are removed; `--json`
  is not changed.
- What a user may read is decided by the server: a `viewer` gets `403` from
  `audit list`, for example.
- `VM` is a VM's ID or its name. A write prints the task it started;
  `--wait` follows the task until it ends and exits non-zero when it
  failed. `vm create -f -` reads the definition from standard input; an
  unknown field in it is an error. The definition is the body of
  `POST /api/v1/vms` (see [Virtual machines](virtual-machines.md)).
  `skepctl` does not attach to consoles yet.
- `SNAPSHOT` is a snapshot's ID or its name. A snapshot of a running VM
  is crash-consistent. A rollback needs the VM stopped and goes to an
  older snapshot only with `--destroy-newer`, which destroys every newer
  one (ADR 0010 decision 2). `vm clone` without `--snapshot` takes a
  snapshot first, named after the clone, and leaves it behind. It prints
  the reminder that a clone is the same guest, with the source's
  hostname, SSH host keys, and machine ID. With `--json`, the reminder
  goes to standard error. Snapshots and clones need a ZFS pool.
- `cluster` (step 8c-2): `show` needs `system:read`; `admit`, `withdraw`
  and `remove` need `cluster:manage` (the `admin` role). `admit` prints
  the host's name, node ID, address and pin, asks you to compare the pin
  with the one the host shows on its own console, then prints the
  admission and the `skep-cluster join` command. `remove` shows the member
  and asks for the cluster ID on the terminal (or takes `--cluster-id`),
  and waits up to two minutes for the answer. See [Joining hosts into a
  cluster](cluster.md#through-the-api-and-skepctl-step-8c-2).

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

## Operator-supplied certificates

Pinning is for `skep-api`'s self-signed certificate, which is renewed
with the same key. An operator-supplied certificate (`tls.cert_file` and
`tls.key_file`) is usually renewed with a **new** key, and a pin would
lock every client out at each renewal. For such a server, tell `skepctl`
what to verify the certificate against:

```sh
skepctl login --server mgmt.example.org --username andy --ca-file /etc/ssl/operator-ca.pem
skepctl login --server mgmt.example.org --username andy --system-roots
```

- The chain is verified against the CA certificates in the file, or
  against the system's roots, and the server's name (or IP address) must
  be in the certificate. TLS 1.3 only, as always.
- Nothing is asked: the CA decides. The setting is checked with a TLS
  handshake before it is stored, so a CA that does not verify the server
  changes nothing and no password is asked for.
- Renewals, including with a new key, need no action as long as the new
  certificate is issued by the same CA.
- A pin is optional in this mode: `--trust-key PIN` together with
  `--ca-file` or `--system-roots` checks both. With a pin, a renewal with
  a new key needs a new login, as in pin mode.
- The CA file is read again on every run, so replacing it takes effect at
  once. If the CA changes, log in again with the new `--ca-file`.
- To go back to pinning, log in with `--trust-key PIN` alone.

Choosing the mode is up to the user, never the server: `skepctl` does not
switch modes by itself, and a server cannot ask for it.

## Sessions

- `login` asks the server for a bearer session (`"session": "bearer"`).
  The server sets no cookie for it and accepts its token only in an
  `Authorization: Bearer` header.
- The token is stored in the state directory; see below. A new login to the
  same server ends the previous session on the server before replacing it:
  only the session whose token `skepctl` stored. The user's other sessions
  (a browser, `skepctl` on another machine) are not touched, and neither
  are they by `logout`.
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
| `pins.json` | How each server is trusted: its pinned public key, or chain verification with a CA file (or the system roots) and an optional pin; and when | `0600` |
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
