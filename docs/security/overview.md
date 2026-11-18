# Security overview

This document describes the security posture of Skep 0.1.0 as of
Milestone 5. It states plainly what is and is not protected.

## Status

Milestone 4 ([ADR 0006](../decisions/0006-api-split-authentication-tls.md))
is done: the split into two processes, TLS, accounts, sessions, and
permission checks. Milestone 5 ([ADR 0008](../decisions/0008-web-ui.md))
adds the web interface, a client of the API with no privileged path of
its own. One endpoint acts on the node: cancelling a task. Nothing in the
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
  second: any request that carries an `Origin` header, on any route,
  authenticated or not (the login and the logout included), must name
  this host over HTTPS, or it is refused. The third: a request
  authenticated by cookie that is not a GET or HEAD must carry such an
  `Origin`. Requests without `Origin`, such as `skepctl`'s, are not
  affected; a browser cannot attach a bearer token by itself. A refusal
  of a request with a valid session is audited under its user; one
  without a session is only logged, so that no web page can fill the
  audit log. Request bodies must be `application/json`.
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
- **Background requests do not extend a session.** A request that carries
  `X-Skep-Background` (any non-empty value) is authenticated like any
  other, the idle check included, but does not refresh `last_seen_at`.
  The web interface sends it on its own refreshes, so an unattended page
  idles out. A client can only shorten its own session with it.

## The web interface

The interface ([ADR 0008](../decisions/0008-web-ui.md)) is static files
served by `skep-api` on the same origin as the API. It adds no endpoint
and no privilege: everything it does is an API request under the user's
session and permissions, audited like any other client's.

- **Same origin, cookie session only.** The login asks for a cookie
  session; the token is `HttpOnly` and never reaches JavaScript. The
  browser adds `Origin` to the interface's writes by itself, which the
  origin check requires. Requests use the browser's `same-origin`
  credentials, never `include`. There is no CORS and no second origin.
- **Nothing about the session in browser storage.** The identity is held
  in memory and asked of the server at every page load
  (`GET /auth/session`). `localStorage` holds the theme choice only.
- **Content-Security-Policy** on the interface's responses:
  `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'
  data:; font-src 'self'; connect-src 'self'; base-uri 'none';
  form-action 'self'; frame-ancestors 'none'`, with
  `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, and
  `Cross-Origin-Opener-Policy: same-origin`. No inline script or style
  exists in the interface; the theme script is a file. The API's own
  responses keep their stricter policy. The browser tests fail on any CSP
  violation.
- **Caching.** Only the hashed files under `/assets` are cacheable;
  `index.html` and every API response are `no-store`.
- **Hidden is not denied.** Navigation entries the role lacks are hidden
  as a convenience; the server decides, and a page reached by its address
  without the permission does not call the API.
- **The first visit.** With the self-signed certificate the browser warns,
  and the interface cannot help: it is not loaded yet. The administration
  guide says how to compare the certificate with
  `skep-api tls fingerprint` on the host before accepting it. The
  Settings page shows the pin for reference only, because a man in the
  middle would serve its own page.
- **Development serving** (Vite) is HTTPS with the development
  certificate and proxies `/api` over verified HTTPS; no development
  switch in `skep-api` relaxes the cookie, the origin check, or the CSP.

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
  `audit:read`. It holds no passwords, tokens, or hashes. The API serves
  the live table; entries older than the retention time are in the
  archive (see [Audit logs and retention](#audit-logs-and-retention)).

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
  host, is the only thing that creates a new key. It writes two access
  audit entries, `tls.key.regenerate` with `details.phase` `requested` and
  then `finished`, joined by `details.operation_id`, each with the actor
  `local:<uid>` and both key fingerprints. The intent is written after the
  new key exists in memory and before anything on disk changes; if it
  cannot be written, the key does not change. A key change that a crash
  interrupts, and the next start completes, therefore always has its
  intent, naming the installed key, on record, even without an outcome.
  If the generated files are damaged, `skep-api` refuses to start instead
  of replacing the key.
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
- The internal API carries the read operations, task cancellation, and
  the VM operations (create, update, delete, the power actions, the
  console). Every one is a typed request that becomes a task, or, for the
  console, a byte stream to one VM. There is no operation that takes a
  command, a path, or a shell string.
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

## Host operations and confinement (Milestone 6)

The agent changes the host for the first time in Milestone 6
([ADR 0009](../decisions/0009-first-vm-bhyve.md)). How that is bounded:

- **One package holds every privileged call** (`internal/hostops`): ZFS,
  taps and bridges, `bhyvectl`, starting bhyve and the per-VM runner, and
  the files under Skep's state directories. Every command is a fixed
  absolute path (`/sbin/zfs`, `/sbin/ifconfig`, `/usr/sbin/bhyve`, ...) with
  an explicit argument list, a minimal fixed environment, and no shell.
- **Nothing from a request is a path or a name on the host.** Dataset
  names, zvol devices, tap descriptions, console devices, and state files
  are derived from Skep's UUIDs and the configuration. ISO files are named
  by a pattern that cannot contain a separator and are resolved against
  the directory listing first. A rendered bhyve configuration rejects any
  value with a line break.
- **The confinement is root-owned configuration** in `agent.json`:
  `storage.pools`, `storage.directories`, `network.bridges`,
  `network.management_interfaces`,
  the firmware and media directories. The API cannot change it. Empty
  allow-lists, the default, mean no VM storage and no VM network. A
  bridge that has a management interface as a member is refused at
  create, at start, and again by the runner before a tap joins it. Skep
  never creates, destroys, or re-addresses a bridge.
- **Skep only destroys what it can prove is its own**: a dataset carries
  `skep:vm_id`, and `zfs destroy -r` (never `-R`) runs on the VM's own
  dataset only; a tap is destroyed only if its description names the VM.
- **Directory pools are reached only through `os.Root`** (ADR 0010
  decision 6). The pool's directory is checked on every use (not a
  symbolic link, owned by root, mode `0700`, on local UFS or ZFS), then
  opened as a root that no path can leave, even through a symbolic link.
  Disk files are opened with `O_NOFOLLOW`, created with `O_EXCL` and mode
  `0600`, and a VM's directory is removed only when it holds nothing but
  `disk<N>.img` regular files. A file disk carries no ownership marker of
  its own, as a dataset carries `skep:vm_id`: its path, derived from the
  VM's ID inside a root-only directory, is the ownership.
- **Runners are identified before they are signalled.** A VM's runner
  is a root process in its own session. Before any signal the agent reads
  the process's argument list from the kernel (`kern.proc.args`) and
  compares it with the exact runner command for that VM, so a recycled
  PID is never signalled.
- **bhyve's configuration is passed in a root-only file** (`-k`), never
  on the command line.
- **Kernel modules are the operator's.** Skep loads neither `vmm` nor
  `nmdm`; a start is refused with the reason when one is missing.
- **Every write is a task with two audit entries** in the agent's log, and
  refusals decided by the host (a pool or bridge not allowed, not enough
  memory or space) are audited as denied without a task.

### The serial console

- A console is a login on the guest and has its own permission,
  `vm:console`; `viewer` does not have it.
- The browser reaches it as a WebSocket on the same origin. The handshake
  goes through the same checks as any request: the session (cookie or
  bearer, never both), the permission, and the `Origin` check of ADR 0006.
- `skep-api` relays bytes; it never opens a device. The agent is the only
  reader of the console device and writes `vm.console.attach` and
  `vm.console.detach` (duration and byte counts) with the actor and the
  verified peer; `skep-api` writes the same two actions to the access log
  with the same request ID.
- A console does not outlive its session: the session is re-checked every
  minute and the WebSocket closed (code 4001) when it has ended. Typing
  refreshes the session at most once a minute; guest output never does.
- At most four clients per console. A client that cannot keep up is
  disconnected rather than slowing the guest.
- The web interface's CSP is unchanged (`style-src 'self'`): the terminal
  draws with xterm.js's WebGL renderer only, because its default renderer
  injects `<style>` elements the CSP would block. Without WebGL 2, or when
  the context is lost, the console is shown as unavailable; it never falls
  back to the other renderer.

## Jail templates (Milestone 7b, step 6)

Building a template is the first time the root agent installs software
and makes outbound connections, so it has its own rules
([ADR 0010](../decisions/0010-storage-and-jails.md), step 6, "Decision on
package scripts"; [guide](../administration/jail-templates.md)).

- **What `pkg --rootdir` would do unaided.** pkg 2.9.2 runs package shell
  scripts with the host's `/bin/sh`, unchrooted and as root, with only
  `PKG_ROOTDIR` pointing at the target. It runs Lua scripts unchrooted
  too, and saves unsandboxed triggers for later. A script's writes stay in
  the template only because its text uses `${PKG_ROOTDIR}`
  ([trace](../../tests/integration/jails/results/step6-pkg-trace.txt)).
  Skep does not rely on that.
- **No package code runs on the host.** pkg runs with
  `RUN_SCRIPTS=false` and `PKG_TRIGGERS_ENABLE=false`, a fixed argument
  list, and an environment of exactly `PATH`, `ASSUME_ALWAYS_YES`, and
  `TMPDIR` (in the work area). Package names come only from
  `storage.template_packages` and must match `FreeBSD-*`. The repositories
  and signing keys are the host's own. A template root is always empty
  when pkg starts, because pkg looks for trust anchors in the root first.
- **What the scripts would have done, Skep does, inside a jail.** Every
  script and trigger file in the new template must match a compiled,
  per-release allowlist (package, kind, phase, SHA-256). The allowlist is
  in the code, not the configuration, because it is a security boundary.
  The commands those scripts stand for are fixed in Skep's code and run
  in a throwaway build jail on the template. That jail has no network, no
  devfs, every `allow.*` off including `allow.suser`, securelevel 2, and
  no `exec.*`. Its parameters are read back from the kernel before
  anything runs in it. Commands enter with `jexec -l`, which gives root's
  login-class environment.
- **The host is checked, not trusted.** Every file under `/etc`,
  `/var/db/pkg`, and `/usr/share/certs` is recorded before pkg starts and
  after the jail is removed, except the exact paths on a short compiled
  list (only `/var/db/pkg/vuln.xml`, which periodic's `pkg audit -F`
  rewrites; no wildcards; ADR 0010 records each reason). A build that
  wrote an excluded path would not be caught. Any difference fails the build
  (`template_host_changed`), destroys the template, and is reported; Skep
  does not try to repair the host.
- **Clean-up does not depend on the task's context.** A failed, cancelled,
  or timed-out build removes its jail and destroys its dataset on a
  context of its own. A jail left by a crash is removed at the next agent
  start, before the socket is served.
- **Paths inside a template are read through `os.Root`**, and the
  trigger directories and `bin/freebsd-version` without following symbolic
  links, so nothing the packages ship can redirect a read or a removal to
  the host.
- **Only admin can build** (`storage:write`).

Ordinary jails (step 7) keep `allow.suser` on. With it off, root in a jail
has no privilege and `/etc/rc` fails (ADR 0010, amendment e). Only the
build jail turns it off.


## Jails (Milestone 7b, step 7)

A jail shares the host's kernel, so its isolation rests on parameters
Skep fixes and nobody can change through the API
([ADR 0010](../decisions/0010-storage-and-jails.md), decisions 8, 9, and
13, amendments e and f; [guide](../administration/jails.md)).

- **A generated jail.conf, no host-side command.** jail(8) reads a
  root-only `jail.conf` that Skep renders from typed values: the path is
  derived from the pool and the ID, the hostname is a validated DNS name,
  and the only command is `exec.stop="/bin/sh /etc/rc.shutdown jail"`,
  which runs inside the jail. There is never an `exec.start`,
  `exec.prestart`, `exec.poststart`, or any other command on the host,
  no `mount.fstab`, and no `exec.consolelog`.
- **No jailed process holds a host file (amendment o, SR-01).** `/etc/rc`
  writes to a pipe the agent drains into the jail's `console.log`
  (truncated at each start, at most the first 256 KiB plus a marker and
  the last 64 KiB) and closes when the start ends; a process rc leaves
  running that writes later gets `SIGPIPE` or `EPIPE`, so a jail cannot
  fill the host's `/var/run` (on a UFS-root host, its root file system)
  through its console. `/etc/rc.shutdown` writes to `jail -r`'s output,
  capped in the agent's memory and not kept; past the cap it is cut, and
  never makes a removal fail.
- **The `/dev` guard (amendment i).** `jail -c` creates the jail with
  nothing running in it. The agent lists the jail's `/dev` from the host
  (opened without following links, required to be a devfs mounted at
  `<root>/dev`) and removes the jail, without `/etc/rc.shutdown`, if it
  holds anything outside a short allow-list (`bpf*` only for a DHCP jail).
  Only then does the agent run `/etc/rc` in the jail with `jexec -l -U
  root`. This is a check of the result, not of the ruleset text: amendment
  h's ruleset 6001 (`include 5` plus `bpf*`) exposed every host device,
  because devfs does not resolve the includes of an included ruleset;
  ruleset 6001 now writes out ruleset 5's own rules plus `bpf*`.
- **The baseline.** VNET, `enforce_statfs=2`, `children.max=0`,
  `securelevel=2`, devfs ruleset 5, `allow.suser` and
  `allow.reserved_ports` on and every other `allow.*` off. After `jail -c`
  the agent reads the parameters back from the kernel and removes a jail
  that does not match.
- **Raw sockets cannot be taken away from a VNET jail.** FreeBSD 15.1
  grants them to every jail with its own network stack before it looks
  at `allow.raw_sockets`. A jail can therefore craft and spoof packets
  (source IP and MAC) on its own interface, as a VM can on its NIC. The
  isolation test of the networking step includes spoofing from a jail.
- **Host users never reach a jail's tree.** The directory that holds the
  jail roots, like the templates directory, is `root:wheel 0700` and
  checked before every start: a jail controls setuid binaries in its tree,
  and a host user who reached one could run it with the jail root's IDs.
  Checked on hearst: an unprivileged user gets Permission denied on the
  jail root and on a setuid file inside a running jail.
- **Skep writes into a jail only before its first start**, and then only
  `etc/rc.conf` (its own, recognised by its header) and root's
  `authorized_keys`, each opened relative to the jail root's directory
  descriptor with `O_RESOLVE_BENEATH | O_NOFOLLOW` and replaced by a
  rename, so a planted symbolic link can neither be followed nor lead
  outside the root (natively tested). `started_once` is set before the
  jail first runs and never goes back; afterwards such a write is
  `container_started`.
- **SSH keys, not passwords.** The only credential Skep handles for a jail
  is root's public key, accepted only as one plain OpenSSH key line
  without options, and only for a template that has sshd. sshd allows
  root with a key only. Templates hold no SSH host keys (the build fails
  if one is present); each jail generates its own at its first start.
- **No console, no jexec.** Nothing in the API enters a jail.
## Audit logs and retention

There are two audit logs: the agent's operation audit in `agent.db` and
skep-api's access audit in `api.db`. Each process keeps custody of its
own. What they promise ([ADR 0007](../decisions/0007-retention.md)):

1. An entry is never modified.
2. An entry is never removed from the live table unless it is inside a
   sealed archive segment. The database enforces this with a trigger,
   whoever asks; there is no window in which the rule is switched off.
3. The index of sealed segments is never modified or shortened.
4. The segments form a SHA-256 hash chain, so a missing, reordered, or
   altered segment is detectable.

"Append-only" therefore describes the record as a whole, the live table
plus the sealed segments, and no longer the table alone.

- **Sealing** happens after each calendar month (UTC): its entries are
  written to `audit-archive/` next to the database (`0700`, files `0400`,
  owned by the process that owns the log) and the segment is recorded.
  Which segment an entry belongs to is decided by its ID, never by its
  timestamp, so a skewed clock cannot misplace one.
- **Removal** of live entries is separate and later. The agent's log:
  never, unless the operator sets `retention.audit_live_days`. The access
  log: 90 days after the end of the month, by default. Before live
  entries go, their segment file is read again and must match its
  recorded hash; if it is missing or altered nothing is removed, the run
  fails, and health is degraded.
- **Each seal is also written to syslog** at `daemon.notice` with the
  month, the ID range, the count, and the hash. On a host that forwards
  syslog, that is an off-host anchor for the chain.
- **Skep never deletes a segment file.** How long they are kept, and
  where, is the operator's decision.
- **Tasks and task events** are operational history, not the record, and
  are deleted by age (90 days by default). What happened stays in the
  audit log, including what each retention run removed.
- `skep-agent audit verify` and `skep-api audit verify` check the index
  and every file and report the first break.

The limit: none of this holds against root on the host, who can rewrite
the database, the files, and the index together. It detects accidents,
bugs, and partial tampering, and with an off-host copy of the latest hash
it detects complete tampering too. A compromised `skep-api` can damage
its own log and archive, detectably; the agent's are out of its reach.

## Open items

- **Scoped permissions** (per VM, pool, or tenant) and **custom roles**:
  not designed yet; the schema allows both to be added without changing
  the meaning of existing data.
- **Long-lived API tokens** for automation are deferred to their own ADR;
  until then automation logs in for a bearer session.
- **VNC** consoles are deferred until the agent can authenticate to
  bhyve's framebuffer on the client's behalf (ADR 0009 decision 8a).
- **Install media integrity** is the operator's: Skep lists what is in
  the media directory and does not verify checksums.
- **Root helper.** Whether to split a minimal root helper out of the agent
  is revisited before 1.0.
- **Signed per-request tokens** replace the API-asserted identity before
  any agent accepts requests from another node.
- **Per-entry hash chaining** of the audit logs, which would make
  tampering inside the live table detectable too, and **signed segments**,
  which wait for node keys (ADR 0007, decision 5).
- **Secrets storage.** Nothing secret is stored yet. Secret encryption is
  required before anything is.

## Reporting a vulnerability

Skep is pre-release software with no deployed users. Until a security
contact is published, report issues privately to the repository owner rather
than in a public issue.
