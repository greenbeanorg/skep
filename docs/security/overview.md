# Security overview

This document describes the security posture of Skep 0.1.0 as of
Milestone 4. It states plainly what is and is not protected.

## Status

Milestone 4 is in progress
([ADR 0006](../decisions/0006-api-split-authentication-tls.md)). The split
into two processes, TLS, accounts, sessions, and permission checks are
done. One endpoint acts on the node: cancelling a task. Nothing in the
API creates or changes host resources yet.

`skep-api` listens on `127.0.0.1:8443` by default. Opening it to a network
is an explicit change of `listen_address`.

## Sessions and login

- **Everything needs a session**, except `GET /api/v1/health`, which
  without one returns the status only (no version, no uptime, no detail),
  and the login itself.
- **Opaque tokens, stored hashed.** A session token is 32 random bytes.
  `api.db` holds only its SHA-256, so a copy of the database contains no
  usable session.
- **One kind of session per login, chosen by the client.** A cookie login
  sets `__Host-skep_session` (`Secure`, `HttpOnly`, `SameSite=Strict`,
  `Path=/`) and does not return the token in the body, so JavaScript never
  sees it. A bearer login, only on request, returns the token in the body
  and sets no cookie. A token is accepted only through the channel it was
  issued for.
- **Lifetimes:** 30 minutes idle and 12 hours absolute by default. A new
  session is issued at every login. Logout deletes the session; a password
  change, a role change, disabling, and deletion delete all of the user's
  sessions. The user and role are read on every request.
- **Cross-site requests.** `SameSite=Strict` is the first control. The
  second: a request authenticated by cookie that is not a GET or HEAD must
  carry an `Origin` header naming this host over HTTPS, or it is refused
  and audited. Bearer requests are exempt, because a browser does not
  attach them by itself. Request bodies must be `application/json`.
- **Failed logins are indistinguishable.** A wrong password, an unknown
  user, and a disabled user get the same answer, and the unknown and
  disabled cases verify a dummy hash made with the current parameters, so
  they take the same time.
- **Rate limits, never a lockout.** Per source address: 10 attempts a
  minute. Per username and address: after 5 failures each further attempt
  must wait, doubling from 1 s to 60 s. Per username across all addresses:
  50 failures in 15 minutes, then refusal until the window drains. Nothing
  is written to the user, existing sessions keep working, and
  `skep-api admin` on the host is unaffected. An attacker with many
  addresses can keep new logins for one username throttled while the
  attack lasts; that cost is accepted. The limits are in memory and reset
  at a restart.
- **The source address is the TCP peer.** `X-Forwarded-For` is ignored
  unless `auth.trusted_proxies` lists the proxy.
- **Audit.** Every successful login, failed login, logout, and session
  revocation is written to the access audit log with the source address.
  Throttled logins are aggregated into one entry per source address per
  minute, with the number of attempts, so a flood of refused attempts
  cannot grow the log one row per request (ADR 0007, amendment b). A
  submitted username is recorded only if it has the shape of a username.
  Tokens and their hashes are never logged.
- **Before setup.** Until an administrator exists, login answers
  `503 setup_required` and names the command to run on the host.
- **The agent is told who.** Each request to the agent carries the
  authenticated user as its actor.

## What else is enforced

**No command execution from input.** No API input reaches a command line.
The only external commands are `zpool list -H -p -o ...` and
`qemu-* --version`, with fixed argument lists, run through `exec.Command`
without a shell, with a fixed minimal environment, no stdin, a timeout, and a
cap on captured output.

**Binaries are not resolved through `$PATH`.** Discovery looks for
executables in a fixed list of system directories, so the environment of a
privileged agent cannot redirect it to another binary.

**Discovery is read-only.** It does not load kernel modules, create devices,
or change configuration, and it avoids commands that could load a module as
a side effect. See [ADR 0003](../decisions/0003-read-only-discovery.md).

**Input validation.** Node IDs in URLs must match `[A-Za-z0-9._-]{1,64}`.
Client-supplied request IDs are accepted only in the same character set, so
arbitrary input is not written to the logs. Unknown keys in the
configuration file are rejected.

**No internal detail in responses.** Unexpected errors return a generic
`internal_error` with a request ID. The cause is written to the log only.
The health endpoint reports that the database is unreachable, not why.

**Local state permissions.** The directory holding the agent database is
created `0700` and the database file `0600`.

**The process that listens on the network is not root.** `skep-api` runs
as the unprivileged `skep` account and refuses to start as root. The agent
runs as root and has no network listener. See
[the service documentation](../installation/freebsd-service.md).

**Test-only code is not shipped.** The `test.sleep` task type used by the
native integration tests is compiled only with the `skep_testtasks` build
tag. CI fails if the shipped FreeBSD binary contains it.

**Separate databases.** Node state (`agent.db`) and identity data (`api.db`)
are separate databases with separate schemas, each opened by one process
only: the unprivileged API never opens the agent's state, and the agent
never opens the API's.

**Schema-level rules.** `users.password_hash` only accepts Argon2id hashes in
PHC format, so a plaintext password cannot be stored by mistake. Both
`audit_log` tables reject updates and deletes. In the agent's schema, tasks
and audit entries cannot be stored without an actor, and audit entries
cannot be stored without the caller's UID. Triggers also enforce the legal
task state transitions and make finished tasks and their request fields
immutable. In the API's schema, a session row holds only the SHA-256 of
its token.

**Operation audit.** Every task writes to the agent's audit log when it is
requested and when it finishes, in the same transaction as the state
change. A request without an actor is refused before anything is written.
Refused requests are recorded too. The actor is `system` for work the
agent starts itself, and the authenticated user for a request that came
through the API.

**Task control.** Tasks can be listed, read, and, with `task:cancel`,
cancelled. They cannot be submitted over the API; submission arrives with
the first operation that needs it.

**Task content.** Task payloads are not returned by the API. Results,
errors, and events are, so handlers must not put secrets in them. Unexpected
handler errors are logged and replaced by a generic message on the task.

**HTTP hardening.** Read, write, and idle timeouts, a header size limit,
and a request body limit are set. Responses carry `X-Content-Type-Options: nosniff`,
`Cache-Control: no-store`, and a deny-all `Content-Security-Policy`.

## Authorization

- **One role per user, global scope.** A role is a set of
  `resource:action` permission strings, declared in one file
  (`internal/auth/permissions.go`). This milestone defines
  `system:read`, `node:read`, `task:read`, `task:cancel`, `user:read`,
  `user:manage`, `role:read`, and `audit:read`.
- **Built-in roles**, created by migration: `admin` (stored as `*`: every
  permission, including later ones), `operator` (all reads plus
  operational actions, never `user:manage`), `viewer` (reads, without
  `user:read` and `audit:read`). Roles cannot be edited in this release.
- **Deny by default, in one place.** Every route is declared in one table
  with the permission it requires, and one function enforces it. A route
  with a missing or unknown requirement is refused for everyone, the
  administrator included, and a unit test fails if the table contains
  one. Another test calls every route as every built-in role.
- **Only two routes are public:** the health status and the login. The
  routes under `/auth` need a session and nothing more, because they
  concern only the caller's own session and password.
- **Every denial is audited** with the actor, the permission, and the
  route.
- **Changes apply at the next request.** The user and role are read from
  the database for every request, and a role change also ends the user's
  sessions.
- **The agent makes no authorization decision** (ADR 0004). It records
  the actor the API names.
- **The audit log is readable through the API** by roles with
  `audit:read`. It is append-only and holds no passwords, tokens, or
  hashes.

## Accounts and passwords

- **No default credentials, and no network path to the first account.**
  The first administrator is created with `skep-api admin create` on the
  host. The command opens `api.db` directly, so it runs as the service
  account: started as root it gives up root first, and as any other user
  it fails on file permissions. There is no first-run page and no setup
  token.
- **Passwords are never accepted as an argument or from the environment.**
  They are read from the terminal with echo off, or from standard input
  with `--password-stdin`.
- **Argon2id**, through `golang.org/x/crypto/argon2`: 64 MiB, 3 passes,
  1 lane, a 16-byte random salt, a 32-byte hash, stored as a PHC string
  that carries its parameters. The parameters can be raised in the
  configuration and never set below 19 MiB and 2 passes.
- **At most two hashes run at once.** A flood of logins costs at most
  128 MiB and two cores; further attempts wait briefly and are refused.
- **Password rules** follow NIST SP 800-63B: 12 to 256 characters, no
  composition rules, no forced rotation.
- **No pepper.** Skep has no secret storage yet; this is revisited with
  secret encryption.
- **Roles.** Three built-in roles (`admin`, `operator`, `viewer`) are
  created by migration and cannot be changed or deleted, even with direct
  SQL. The only enabled administrator cannot be disabled, deleted, or
  demoted.
- **Changes end sessions.** A password change, a role change, disabling,
  and deletion delete the user's sessions in the same transaction and are
  written to the access audit log, one entry per ended session.
- **The agent contains none of this.** `skep-agent` does not link the
  password or user code and never opens `api.db`.

## TLS

- **TLS 1.3 only, and nothing else.** There is no plaintext listener, not
  even for a redirect, and no setting that adds one.
- **Generated certificate.** Without configuration, `skep-api` generates
  an ECDSA P-256 key and a self-signed certificate at first start, as the
  service account, in `/var/db/skep/api/tls` (`0700`; key file `0600`). The
  agent never sees the key.
- **The key is a plaintext file.** An unattended service must read its key
  at boot, so there is no other form. "No plaintext secrets" is met for
  stored credentials, which are hashes; a TLS server key cannot be hashed.
- **Renewal keeps the key.** The certificate is renewed 30 days before
  expiry with the same key and swapped in without a restart. Each renewal
  is written to the access audit log with the old and new fingerprints.
- **A new key only on request.** `skep-api tls regenerate`, run on the
  host, is the only thing that creates a new key. It is audited with the
  actor `local:<uid>` and both key fingerprints. If the generated files are
  damaged, `skep-api` refuses to start instead of replacing the key.
- **Pinning.** The value to pin is the SHA-256 of the public key
  (`skep-api tls fingerprint`, also in the system document). It survives
  renewals.
- **Operator certificates** are read, reloaded on change and on `SIGHUP`,
  checked to match their key, and never written. A bad pair is logged and
  the previous one stays in use.
- **HSTS** is sent only with an operator certificate. With the self-signed
  one it would stop users from clicking through the browser warning of a
  fresh install.
- **Local commands drop root.** `skep-api tls ...` started as root changes
  to the owner of `/var/db/skep/api` before it opens anything.

### skepctl

- **Trust on first use, pinned key.** `skepctl` pins the SHA-256 of the
  server's public key (SubjectPublicKeyInfo). On first contact it shows the
  key and asks, or takes it from `--trust-key`; nothing but a TLS handshake
  happens before that. Every later connection is refused during the
  handshake unless the key matches, so neither a token nor a password
  reaches a server with another key. A changed key is accepted only by
  naming the new pin with `--trust-key`. There is no option that turns the
  check off.
- **No chain or host name check in pin mode.** With a pinned key, neither
  adds anything: the pin names the exact key. The Go TLS configuration
  therefore sets `InsecureSkipVerify` together with a `VerifyConnection`
  function that does the pin check. TLS 1.3 only, no proxy, redirects are
  not followed.
- **Chain mode for operator certificates.** With `--ca-file` or
  `--system-roots`, the chain and host name are verified the standard way
  (no `InsecureSkipVerify`), so an operator's renewal with a new key does
  not lock clients out; a pin is optional and checked in addition. The
  user chooses the mode at login, and it is checked with a handshake
  before it is stored; the server cannot select it. A client cannot be
  built with neither a pin nor chain verification (`pkg/client`).
- **Re-login and logout end only skepctl's own session**, by its token.
  The user's other sessions, such as a browser's, are not affected.
- **Token file.** The bearer token is stored as it is in
  `~/.config/skepctl/sessions.json` (`0600`, directory `0700`, atomic
  replacement). A file or directory that is a symbolic link, belongs to
  another user, or is readable or writable by others is refused. The
  pins file gets the same treatment, because whoever can write it chooses
  the trusted key. A `401` deletes the token; logout deletes it after the
  server has ended the session.
- **Terminal input.** The password is read without echo, or from standard
  input with `--password-stdin`; never from an argument or the
  environment. The trust question is asked on `/dev/tty`, so piped input
  cannot answer it. Values from the server are stripped of control
  characters before they are printed as text.

## The privilege boundary

[ADR 0004](../decisions/0004-privilege-boundary.md) splits the system into
an unprivileged `skep-api`, which is the only network listener, and a root
`skep-agent` that accepts typed operations over a Unix socket. This is
implemented:

- The agent listens on `/var/run/skep/agent.sock` (`root:skep`, `0660`, in
  a `0750` directory) and nowhere else.
- For every connection the agent reads the peer's credentials from the
  kernel and drops it unless its UID is on the allow-list (`root` and
  `skep`). File permissions are the first check, this is the second.
- Every request must carry the actor it is made for
  (`Skep-Actor-Id`, `Skep-Actor-Name`); a request without one is refused
  with 400 before any handler runs, reads included. The agent records the
  actor and the verified peer UID. It cannot verify the actor and makes no
  authorization decision from it; that is the API's job.
- The internal API carries the read operations and one operation that
  changes something: cancelling a task. There is no operation that takes
  a command, a path, or a shell string.
- For a cancellation the agent writes its own audit entry with the actor
  the API named, the peer UID it read from the kernel, the client's
  address, and the request ID. That entry is in `agent.db`, which
  `skep-api` cannot open: a compromised API can add false entries to its
  own log but cannot remove the agent's record of what it asked for. The
  request ID is the same in both logs.
- Each process owns one database the other cannot open:
  `/var/db/skep/agent` is `root`, `0700`; `/var/db/skep/api` is `skep`,
  `0700`.

The actor of a request is the authenticated user. The health check that
`skep-api` makes without a user carries the actor `skep-api`. The socket
and the two rc.d services are pending FreeBSD validation.

## Open items

- **Scoped permissions** (per VM, pool, or tenant) and **custom roles**:
  not designed yet; the schema allows both to be added without changing
  the meaning of existing data.
- **Long-lived API tokens** for automation are deferred to their own ADR;
  until then automation logs in for a bearer session.
- **Host confinement configuration** for the agent (which pools,
  interfaces, and devices Skep may touch): designed with the first
  privileged operation.
- **Root helper.** Whether to split a minimal root helper out of the agent
  is revisited before 1.0.
- **Signed per-request tokens** replace the API-asserted identity before
  any agent accepts requests from another node.
- **Retention.** Both audit logs are strictly append-only, and tasks and
  task events are never removed. Retention and archiving need a design
  that does not weaken the audit guarantee.
- **Secrets storage.** Nothing secret is stored yet. Secret encryption is
  required before anything is.

## Reporting a vulnerability

Skep is pre-release software with no deployed users. Until a security
contact is published, report issues privately to the repository owner rather
than in a public issue.
