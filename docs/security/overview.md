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
- The agent's start-up sweeps write one audit entry per action, with the
  actor `system`: `container.sweep.stop_completed`,
  `container.sweep.half_started_removed`,
  `container.sweep.quarantine_removed`, `container.sweep.devfs_removed` and
  `template.sweep.build_jail_removed`. A failure's entry carries a fixed
  reason; the cause is in the agent's log.
- Each process owns one database the other cannot open:
  `/var/db/skep/agent` is `root`, `0700`; `/var/db/skep/api` is `skep`,
  `0700`.

The agent puts its build (release version and commit) in
`Skep-Agent-Version` on every response, and `skep-api` refuses every
response from another build, or one without the header, before using
it: node endpoints answer `503 agent_version_mismatch` and the public
health check reports the agent unavailable without naming versions
(ADR 0012 answer 9). Both programs come from one package; a mismatch
means only one was restarted.

The actor of a request is the authenticated user. The health check that
`skep-api` makes without a user carries the actor `skep-api`. The socket
and the two rc.d services were validated on `hearst`, a lab VM nested
under KVM, on 2026-11-12 (`tests/integration/peercred`,
`tests/integration/rcd`).

### skep-cluster and the cluster (Milestone 8, steps 8a, 8b-1, 8b-2a and 8b-2b)

[ADR 0013](../decisions/0013-clustering.md) adds a third process,
`skep-cluster`, with its own account (`skep-cluster`, UID/GID 410). What is
built is in [skep-cluster](../architecture/skep-cluster.md); the join
procedure in [Joining hosts into a cluster](../administration/cluster.md).

**The local process.**

- **The node key is out of `skep-api`'s reach.** It is in
  `/var/db/skep/cluster` (`0700`, the service account), mode `0600`. The
  API runs as `skep`, a different UID.
- **The service never talks to the agent,** and the agent's allow-list
  (`root`, `skep`) does not admit its account. Only the root-run commands
  `init` (the node ID) and `join-request` / `join` (whether the host has
  guests) read from the agent, as peer UID 0. `remove`, `leave --wipe`,
  `join --abandon` and `listen -none` do not talk to the agent.
- **No root-owned file in the data directory.** Root-run commands do
  their root work (reading the agent, writing `cluster.json`, stopping and
  starting the service) and start the same executable **as the service
  account** for anything inside the directory; each ends by checking that
  no file there belongs to root. Decisions with a root effect are not
  taken on the running service's word: "is this node alone" (and for
  `listen -none` "nothing pending") is read from the store with the
  service stopped. `leave --wipe` and `join --abandon` stop the service
  and do every file step as the service account; a marker (`wipe.json`)
  or a phase (`abandoning` in `join.json`) written before the first step
  makes the service refuse to start until the command has been run
  again to the end.
- **Its socket admits root and `skep` only,** by kernel peer credentials
  on top of file permissions, and each operation names which of the two
  may call it. `admit`, `admit --withdraw` and `remove` are root-only in
  this step.
- **A single host opens no cluster port.** Port 8444 opens only on a
  listen address given by `listen`, `join-request` or `init -address`,
  and closes again with `listen -none` (only while the node is alone with
  nothing pending) or `leave --wipe`.

**Trust between nodes: pinned keys.** There is no cluster CA. Each node
generates its own key; the replicated membership stores the SHA-256 of
each member's public key (its pin), and a peer is accepted only by that
pin.

- **The fingerprint exchange.** A host asks to join with a request
  carrying its pin (`join-request`); an administrator admits it on a member
  (`admit`), which creates a pending entry valid for 30 minutes and prints
  an admission carrying every member's pin; the host joins with the
  admission (`join`). Both sides are pinned before the first connection.
  Neither line is secret. **The administrator must compare the pin in the
  request with `skep-cluster status` on the new host's own console**
  before admitting it: an attacker who gets their own request admitted
  joins with a vote. `join` prints the cluster ID and every member and asks
  for confirmation (or `-cluster-id`), so an admission from the wrong
  cluster is not used by mistake.
- **What the listener accepts.** TLS 1.3 only, a client certificate whose
  key is pinned in a member entry or a pending entry; everything else is
  refused in the handshake. A pending key may call the join operation and
  nothing else: no Raft RPC, no read. Withdrawal and expiry close that
  key's open connections; a listener whose clock has passed the expiry
  refuses the key earlier, and none is accepted after the expiry entry.
- **The pin is the identity, on every RPC.** The node ID in each RPC's
  header must belong to the key of the connection, that kind of key must
  be allowed the operation, and a Raft RPC must also come from a server in
  the receiver's latest Raft configuration and carry that node's address.
  The TCP source address is not compared (a multi-homed host may connect
  from another address). The dialler accepts only the pin of the node it
  dials (either pin of a rotating node). Raft's inbound handler is Skep's own adaptation of the library's
  (the library's does not let the receiver check the sender); it is
  compared with upstream at every library update.
- **A joining node trusts only its admission** until the cluster's
  answer is accepted, then only the answer's members (the answer replaces
  the admission's list), until it has applied a membership listing
  itself; after a refusal that followed an unknown outcome it accepts no
  inbound key at all. **What this leaves open:** until the joining node
  has applied its own membership, it trusts the admission's or the
  answer's pins as members without a Raft configuration check, so a
  member removed after the admission was printed (or after the answer)
  is still accepted by the joining node until the joining node applies
  the removal. The admission is valid for 30 minutes and a removal
  withdraws every pending admission, which bounds it.
- **No member meets a leader it does not know**: a new node becomes a voter
  only when every member has applied the entry carrying its pin. The
  applied membership is kept on disk (`membership.json`), so this holds
  across restarts.
- Every replicated entry is checked again when it is applied, on every
  node, the same way; an entry a node cannot read stops its state machine
  instead of being skipped.

**What one compromised node can do to the others (ADR 0013 B2).** The
cluster is **one trust domain for Skep-managed resources**:

- **Root on one node** has that node's cluster key and a full copy of the
  replicated store. It can propose any entry and, as a voter, become
  leader. Once users are replicated (step 8c) and requests forwarded (8e),
  that means creating an administrator, changing roles, editing the guest
  inventory, and through them controlling Skep-managed guests, volumes and
  jails on **every** node, including reading and changing the guests'
  disks through ordinary Skep operations, and attacking every user's
  password hash offline (Argon2id slows this down; it does not stop it for
  weak passwords). In this step the replicated store holds only the
  membership: such a node can admit hosts of its own and vote.
- It **cannot** get a root shell or run commands on another node, touch
  anything outside another node's local allow-lists in `agent.json`, cut
  another node's management interface, or erase another node's logs.
- **A compromised `skep-api`** (not root) will be able to act as any user
  on every node and make every replicated write the socket offers to it,
  including `admit` from step 8c; it cannot vote, send Raft RPCs or change
  a pin.
- After a node is compromised, every password should be changed, and the
  node removed (`skep-cluster remove NAME` on a member). Members refuse
  its Raft RPCs and close its connections as soon as the configuration
  without it reaches them (a heartbeat may still be accepted for at most
  the 250 ms age of the heartbeat path's configuration copy), and refuse
  its key in the handshake once its member entry is removed.

**Removal and the stale-member risk (ADR 0013 B2, amendment 1 point 6,
amendment 2).** A member that was down during a removal still pins the
removed node until it catches up: a hostile removed node could reach it
first with a forged log at a higher term, and that member could then win
an election among the real members. Skep 0.2 does not prevent this. It
**names such members**: health on every member (`removal_applied`) and
the output of `remove` list the members that have not yet applied the
last removal. **The operator rule:** after removing a node because it is
compromised, wipe every member that was down at that time (`skep-cluster
leave --wipe`) before it returns, and join it again. The removed node
itself shuts its cluster connections when it learns of the removal (a
member's notice, or its own log). A notice is not believed on the
sender's word: the node probes, with its own key, every other node it
accepts, the sender included, and acts on the notice only if at least
one answers and none that answers accepts its key (three rounds, 2 s
apart, then the notice is dropped). A compromised member can therefore
make another node stop its cluster service only when every other node
that node accepts refuses its key too or does not answer (for example
while they are down or cut off from it); with any other member up that
still accepts the key, the notice is dropped (within the trust domain
stated above). `leave
--wipe` gives the node a new key, so its old pin is never accepted again.
A removal that would leave fewer answering voters than a majority is
refused unless the administrator overrides it with the cluster ID typed
back.

**Key rotation (step 8b-2b, amendment 3).** `skep-cluster key rotate`
replaces a node's key without stopping anything: a second pin is added to
its member entry, both pins are accepted as that node, the node switches
once every member has applied the new pin, and the leader drops the old
pin once it has seen the node use the new key; connections made with the
old pin are then closed and the old key is refused in the handshake. The
old private key leaves the disk at the switch.

- **What a compromised member can do with `key-add` and `key-abort`.** The
  leader takes the node from the caller's key, so a member can start or
  abort a rotation **only of its own key**: it cannot add a pin to another
  node's entry, end another node's rotation, or make the cluster accept a
  key for another node. Root on a member could already use that member's
  key; giving its own entry a second key it controls adds nothing beyond
  that. What it can do is keep a rotation of its own open (an abort, or
  never switching), which blocks admissions and other rotations until it
  is removed; removal is allowed during a rotation and takes both pins.
- Rotating does not make a compromised key safe by itself: if a node's
  key was stolen, remove the node (or wipe it) — a thief with the old key
  is refused only once the rotation has ended.

**The cannot-say byte and the per-node limit (step 8b-2b).** A node whose
state machine is stopped or behind its applied-membership file, or a
joining node whose join was refused, cannot tell whether it would accept
a key; it now answers such a key with one byte, 0x02, after the TLS
handshake, instead of a refusal, so that its silence is not read as "this
key is refused" by the removed notice's check, `join --abandon` or
`key_refused` (they count it as no answer). **The cost:** any key that
completes a TLS handshake, known or not, can learn that the node is not
up to date; nothing else is told. Accepted connections are also limited
per node (16 per member node, both pins counting as one; 4 per pending
key) besides the 256 in total, so one member's key cannot take every
connection slot; a connection over the limit gets 0x02.

**The cluster audit (step 8b-2b, ADR 0013 B10 as amended).** Each node's
`skep-cluster` records every replicated entry it applies or refuses, and
its own local steps, in `/var/db/skep/cluster/audit` (hash-chained JSON
lines, written before the entry counts as applied); `skep-cluster audit
verify` checks it. **Its limits:** it is written by the unprivileged
service that also parses peer traffic, so a compromised `skep-cluster`
(or root) on a node can rewrite that node's audit from the start, and a
cut-off tail cannot be detected; the **other members' audits are the
cross-check**, since each records the same entries. There is no external
anchor in 0.2 (backlog).

**Clock health.** Each node measures the other members' clock offsets
and warns (5 s) or reports critical (15 s); nothing is refused because
of a clock until forwarding (8e).

**Not built yet:** replicated users (8c), forwarding (8e), skep-api's
node-to-node key.

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
  of which the agent keeps the first and last 32 KiB in memory and
  appends them to `console.log` after a `--- shutdown ---` line
  (amendment p), followed by a `--- shutdown stderr ---` line and the
  last 32 KiB of its stderr (amendment q); neither their size nor a
  failed write makes a removal fail. The console log keeps the jail's
  bytes as written on disk, but `GET /containers/{id}/console-log`
  removes Unicode format characters and C0 and C1 control characters
  except newline and tab from what it returns (amendment r), so neither
  the web UI's `<pre>` nor a terminal can be made to reorder the text or
  move the cursor. The first stderr line of a failed `jail -c`, of a
  failed command in a template build jail, and of the build's failed
  `pkg install`, `pkg info --raw`, and `tar -x` (amendment s), which
  reach task errors, are cleaned the same way as `jail -r`'s line below. When `jail -r` fails, the error and the log carry
  only the last line of its stderr, with control characters and Unicode
  format characters (category Cf, including the bidirectional overrides
  U+202A–U+202E and U+2066–U+2069) removed, at most 200 bytes,
  and labelled as jail output, since the jail writes the start of that
  stream.
- **A jail cannot make itself unremovable (amendment u).** The jail's
  root controls `/etc/rc.shutdown`; one that exits non-zero or hangs
  makes `jail -r` fail and leaves the jail. The agent then removes it with
  `jail -R`, which runs none of the jail's code and no `exec.*` command,
  unmounts its devfs by fsid, and destroys its epairs and RCTL rules; the
  stop succeeds and is logged as `container_stop_forced`. The
  `--- jail -r failed: ... ---` line the agent appends to the console log
  carries the cleaned stderr line, so the jail cannot use it to inject
  lines or control characters. Only when `jail -R` also fails does the
  stop or delete fail, with `container_remove_failed`.
- **Locked memory has a host total (amendment o, SR-04).** `mlock` needs
  `limits.locked_memory`, and the sum of `locked_memory` over the running
  jails is bounded by `containers.max_locked_memory_bytes` (a quarter of
  physical memory by default): a single value above it is refused at
  create and update (422), and a start that would pass it is refused
  (`container_locked_memory_exhausted`). Wired memory from jails shares
  the kernel's `vm.max_user_wired` pool with bhyve VMs that wire theirs.
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
  at `allow.raw_sockets`, so Skep does not rely on that parameter, and
  root in a jail is not trusted on the network.
- **Anti-spoofing on guest ports (SR-02, ADR 0011, Milestone 7c).** Every
  guest port (a jail's epair host end, a VM's tap) carries a netgraph
  `ng_bpf` filter on its `ng_ether` node, installed and read back before
  the port joins its bridge; a start without it fails
  (`port_filter_failed`), and a start without the netgraph modules is
  refused (`port_filter_unavailable`). The filter is default deny: the
  recorded source MAC always; for a static jail NIC also the IPv4 source
  and the ARP sender IP; ARP sender MAC always; UDP source port 67 (a
  rogue DHCP server) dropped on every port; IPv6 dropped on jail NICs and
  allowed MAC-only on VM NICs with ICMPv6 router advertisements dropped;
  STP, LLDP and every other ethertype dropped. Single 802.1Q tags are left
  to the bridge's `vlanfilter` (reported, not enforced, by the warning
  check `bridge_vlan_unfiltered`). The agent re-checks every port every
  10 seconds; a missing or wrong filter is audited and reinstalled by
  netgraph node ID, and a port whose filter cannot be kept (a failed
  reinstall, or three failed checks in a row) is removed from its bridge
  until the guest restarts. A per-port lock shared by jail start, the VM
  runner, and the check keeps a port from being repaired while it is
  being created. **Residual risks, documented:** MAC-only ports (DHCP
  jails, all VMs) can still use another IPv4 address with their own MAC
  (traceable, not prevented); IPv6 source and neighbour discovery are not
  pinned; the RA guard is best-effort against extension headers; host
  root can open a port for up to one check interval; with `ng_socket`
  loaded every VNET jail's root, including jails Skep does not manage,
  can open netgraph sockets and inject frames on its own interface, which
  on a Skep port the filter drops and on an unmanaged jail's port nothing
  does. Details: [Guest-port anti-spoofing](../administration/guest-port-filtering.md).
  An ARP cache can be poisoned without bpf (`arp -s ... pub`, probe part
  1), which is why enforcement is on the host, not by hiding devices.
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

## Task error details

A failed task's `error` in the API carries `details`: the structured context
the agent recorded with the failure, read from the task's "finished" entry in
the agent's audit log (no new stored data). They travel inside the task
object, next to the error's code and message, so exactly the callers who
already receive that task's error see them: callers with `task:read`
(`GET /api/v1/tasks`, `GET /api/v1/tasks/{id}`), and the task's own
submitter when a request with the same `Idempotency-Key` is replayed (a
replay matches only the same actor and request). Nothing else returns them.

Only keys reviewed for each error code leave the agent
(`internal/tasks/errordetails.go`, `ErrorDetailKeys`); a key added later is
kept out until someone reviews it and lists it. Every key every code can
carry today (found with a scan of every `Code` + `Details` literal and every
helper that builds details, 2026-11-23):

| Error code | Shown | Never shown (flagged) |
|---|---|---|
| `bridge_not_allowed` | bridge, member | |
| `clone_addressing_required` | slots | |
| `console_busy` | max_attachments | |
| `container_address_in_use` | slot, address, bridge, held_by_container_id, held_by_container_name | |
| `container_disk_quota_below_usage` | container_id, template_id, disk_quota, referenced_bytes | |
| `container_locked_memory_above_max` | locked_memory, max_locked_memory_bytes | |
| `container_locked_memory_exhausted` | container_id, running_bytes, required_bytes, max_locked_memory_bytes | |
| `container_mac_in_use` | container_id, slot, mac_address, bridge, held_by_kind, held_by_id, held_by_name | **seen_on** (built from ifconfig output, up to a port name in the address table) |
| `container_mac_invalid` | field, slot, mac_address | |
| `container_not_provisioned`, `container_not_running`, `container_not_stopped`, `container_remove_failed`, `container_started` | container_id | |
| `container_quarantined` | container_id | **state_detail** (the stored detail of the jail's state, which can hold a failed command's text) |
| `container_ssh_unavailable` | template_id, template_name | |
| `container_start_failed` | container_id, extra_devices | |
| `devfs_ruleset_conflict` | ruleset | |
| `disk_device_missing` | dataset, reason, volmode (only `default`, `geom`, `dev` or `none`; any other value is dropped), command, rounds | **error** (text of a failed `zfs` command) |
| `disk_has_snapshots` | snapshots, foreign_snapshots | |
| `insufficient_memory` | running_bytes, required_bytes, guest_memory_bytes | |
| `insufficient_space` | pool, available_bytes, pool_size_bytes, required_bytes | |
| `jail_parent_insecure` | path | |
| `media_not_found` | media | |
| `nameservers_require_nic` | nameservers_given | |
| `pool_not_allowed` | pool | |
| `port_filter_failed` | container_id, slot, interface, step | |
| `port_filter_unavailable` | missing_modules | |
| `rctl_unavailable` | container_id, racct_enabled | |
| `release_unsupported` | template_release, host_release | |
| `resource_busy` | task_id | |
| `snapshot_has_clones` | clones | |
| `snapshot_incomplete` | snapshot_id, datasets_without_snapshot | |
| `snapshot_limit` | limit | |
| `snapshot_not_latest` | snapshot_id, newer_snapshot_ids, foreign_snapshots | |
| `stop_timeout` | timeout_seconds | |
| `storage_feature_unsupported` | pool, pool_type, pools, source | |
| `storage_locked` | dataset, keystatus | |
| `storage_unavailable` | pool, slot | |
| `template_archive_mismatch` | manifest_sha256, archive_sha256 | |
| `template_build_failed` | step, exit_code | **stderr** (a build step's output), **files** (paths of SSH host key files found in the template) |
| `template_host_changed` | (none) | **paths** (changed host files under `/etc`, `/var/db/pkg`, `/usr/share/certs`: key files among them), **build_error** (free text) |
| `template_not_provisioned` | template_id | |
| `template_script_unknown` | package, version, kind, phase, sha256, path | **text** (a package script's text) |
| `template_trigger_left` | files | |
| `vm_mac_in_use` | slot, mac_address, held_by_kind, held_by_id, held_by_name | |
| `vm_not_running`, `vm_not_stopped`, `vm_start_failed` | state | |
| any template build code | | **cleanup_error** (text of failed clean-up steps) |

No key holds a password, a passphrase or an encryption key. `storage_locked`
shows `keystatus` (`available`/`unavailable`), never a key location. The
flagged keys stay in the agent's operation audit log, which the API does not serve (it is read on the host, as root).
The `held_by_*` names are already in those errors' messages.

This table covers `details` only. An error's `message` is separate and was
already public before `details` existed: the `container_mac_in_use` message
includes the text `seen_on` holds, the `disk_device_missing` message for
`volmode_not_dev` includes the dataset's volmode as read (control characters
replaced, 64 characters at most), and a `container_quarantined` message can
include a failed removal's text. `state_detail` also stays on the container
resource itself (`container:read`), as it did before.

## Installer (Milestone 10)

`skep-setup` runs as root and edits host files, so it is held to the
rules of [ADR 0012](../decisions/0012-installer.md) Decision 2:

- It never sources a host file. `rc.conf` and `loader.conf` are parsed
  as plain `name=value` lines; anything else makes it edit nothing in that
  file. Changes go through `sysrc` with an argument list (never a shell),
  and each result is read back.
- It only adds; an existing value is never overwritten, and a variable
  changed between the plan and the change is skipped. New files are
  created with their mode and owner before they appear (temporary file,
  `chown`, then a hard link that fails if anything is there). An
  `agent.json` or `api.json` it did not write is parsed and compared,
  never rewritten.
- Every program it runs as root is named by absolute path, with a fixed
  minimal environment (`PATH`, `HOME`; `TERM` only for the interactive
  `admin create`) and umask `022`. The first administrator is created by
  `skep-api admin create` running as the `skep` user, not root, once the
  API has created its database.
- It never loads modules, reboots, creates or destroys pools or
  datasets, or changes an interface or bridge it did not create.
  `0.0.0.0` as the listen address needs `-listen-all`.
- Every change is journaled with the file's prior copy
  (`/var/db/skep/setup`, `0700`, copies `0600`; no passwords: the first
  administrator's password goes only to `skep-api admin create` on the
  terminal).
- `remove -purge-data` deletes only `/var/db/skep` (except the
  journals), `agent.json`, `api.json` and the `skep` account, and always
  asks. Its path checks refuse anything else. It is refused while a Skep
  guest runs, and when a guest dataset's ZFS key file is under
  `/var/db/skep` (Skep itself keeps no ZFS key).
- The package's scripts set a fixed `PATH`, only create the account
  (fixed UID/GID 409), and **warn** while Skep guests run when the
  package is deleted. They do not prevent the deletion: pkg 2.9.2 deletes
  the files although the pre-deinstall script fails. The guard against
  removing Skep under running guests is `skep-setup remove`, which
  refuses ([ADR 0012](../decisions/0012-installer.md), fix-round
  amendment). They never edit `rc.conf` or start anything.

Database backups (`db backup`) are `0600` in a `0700` directory and
contain everything the databases do (password hashes, session hashes,
audit logs): treat them as the databases themselves.

All of this ran on two lab VMs nested under KVM, `smith` and
`langrishe`, on 2026-11-22
([record](../../tests/integration/setup/results/lab-vm-validation-20261122.md)),
not on bare metal; pkg did not enforce the pre-deinstall refusal, which
is why the scripts now only warn.

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
- **Signed per-request envelopes** before any request is forwarded
  between nodes (Milestone 8, step 8e). A request that originates on
  another node reaches an agent only through that agent's own
  `skep-api`, which verifies the originating API's signed, short-lived
  envelope, the user and their role; the agent records the envelope and
  the originating node in its audit and does not verify it itself
  (ADR 0004, amendment of 2026-11-24; ADR 0013 B8).
- **Per-entry hash chaining** of the audit logs, which would make
  tampering inside the live table detectable too, and **signed segments**,
  which wait for node keys (ADR 0007, decision 5).
- **Secrets storage.** Nothing secret is stored yet. Secret encryption is
  required before anything is.

## Reporting a vulnerability

Skep is pre-release software with no deployed users. Until a security
contact is published, report issues privately to the repository owner rather
than in a public issue.

## The agent's start check

At start, as root on FreeBSD, `skep-agent` refuses to run unless its binary,
the configuration file it reads, its data directory, the socket's directory,
and every parent of each are owned by root, not writable by their group or
others, and carry no ACL beyond the entries that mirror the mode. If
`/bin/getfacl` fails or its output is not understood, the agent refuses.
Symbolic links are followed one component at a time, checking the directory
that holds each link; more than 16 links in a path, or a loop, is a refusal.
The database's directory is checked as well, and a ZFS path whose mode needs
`deny` entries (for example `0705`) is refused, with a mode such as `0755` or
`0700` as the fix.
There is no override. See
[ADR 0004](../decisions/0004-privilege-boundary.md), amendment of 2026-11-23.

