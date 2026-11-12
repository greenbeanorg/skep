# Security overview

This document describes the security posture of Skep 0.1.0 (Foundation). It
states plainly what is and is not protected.

## The API is unauthenticated in 0.1.0

The 0.1.0 API has **no authentication, no authorization, and no TLS**. They
must be in place before any endpoint that changes state is added. Skep's
standing requirements (TLS everywhere, mutual TLS between nodes, Argon2id,
RBAC, audit logging) are not met by this release, and the release must not
be deployed as if they were.

What limits the exposure until then:

- **Loopback by default.** The agent listens on `127.0.0.1:8765`.
- **Remote binding is refused.** A non-loopback listen address, including
  the "all interfaces" form `:8765`, is a configuration error unless
  `api.allow_insecure_remote` is `true` in the configuration file. There is
  deliberately no flag or environment variable for it. When it is set, the
  agent logs a warning at every start.
- **Read-only API.** Every endpoint accepts only GET and HEAD. Nothing in
  0.1.0 changes host state.

Anyone who can reach the port can read the host's hostname, OS release, CPU
model, memory size, installed hypervisor binaries, ZFS pool names and
sizes, and the task history, including task types, times, and actor names. On a multi-user host, "anyone" includes every local user.

## What 0.1.0 does enforce

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

**The service does not run as root.** Under rc.d the agent runs as the
unprivileged `skep` account; only the `daemon(8)` supervisor is root. See
[the service documentation](../installation/freebsd-service.md).

**Test-only code is not shipped.** The `test.sleep` task type used by the
native integration tests is compiled only with the `skep_testtasks` build
tag. CI fails if the shipped FreeBSD binary contains it.

**Separate databases.** Node state (`agent.db`) and identity data (`api.db`)
are separate databases with separate schemas, so the future unprivileged API
process never needs to open the agent's state. Only `agent.db` is opened in
0.1.0.

**Schema-level rules.** `users.password_hash` only accepts Argon2id hashes in
PHC format, so a plaintext password cannot be stored by mistake. Both
`audit_log` tables reject updates and deletes. In the agent's schema, tasks
and audit entries cannot be stored without an actor, and audit entries
cannot be stored without the caller's UID. Triggers also enforce the legal
task state transitions and make finished tasks and their request fields
immutable. No user accounts exist yet, and nothing writes to the API-side
audit log.

**Operation audit.** Every task writes to the agent's audit log when it is
requested and when it finishes, in the same transaction as the state
change. A request without an actor is refused before anything is written.
Refused requests are recorded too. So far the only actor is `system`: the
agent's own inventory refresh.

**No task control over the network.** Tasks can be listed and read, not
submitted or cancelled. Those operations need an authenticated actor and
arrive with Milestone 4.

**Task content.** Task payloads are not returned by the API. Results,
errors, and events are, so handlers must not put secrets in them. Unexpected
handler errors are logged and replaced by a generic message on the task.

**HTTP hardening.** Read, write, and idle timeouts and a header size limit
are set. Responses carry `X-Content-Type-Options: nosniff`,
`Cache-Control: no-store`, and a deny-all `Content-Security-Policy`.

## The privilege boundary

[ADR 0004](../decisions/0004-privilege-boundary.md) is accepted. It splits
the system into an unprivileged `skep-api`, which is the only network
listener, and a root `skep-agent` that accepts typed operations over a Unix
socket and checks the caller's UID from the kernel.

0.1.0 implements the parts of that decision that concern stored data (the
database split, the mandatory actor) and the peer-credential code, which
was validated on FreeBSD. It does **not** yet implement the split itself.
Until Milestone 4, the agent still serves the read-only API on loopback
without authentication, exactly as described at the top of this document.

## Open items

- **Authentication, sessions, RBAC, and TLS** for the API: Milestone 4. No
  state-changing endpoint is added before it.
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
