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
model, memory size, installed hypervisor binaries, and ZFS pool names and
sizes. On a multi-user host, "anyone" includes every local user.

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

**Local state permissions.** The data directory is created `0700` and the
database file `0600`.

**Schema-level rules.** `users.password_hash` only accepts Argon2id hashes in
PHC format, so a plaintext password cannot be stored by mistake. The
`audit_log` table rejects updates and deletes. No user accounts exist in
0.1.0 and nothing writes to the audit log yet.

**HTTP hardening.** Read, write, and idle timeouts and a header size limit
are set. Responses carry `X-Content-Type-Options: nosniff`,
`Cache-Control: no-store`, and a deny-all `Content-Security-Policy`.

## Open items to settle before state-changing endpoints

- **Privilege separation.** The agent currently needs no privileges, and was
  validated on FreeBSD as an unprivileged user. Managing bhyve, ZFS, and
  networking will need privileges. The boundary between an unprivileged API
  process and a privileged host agent has to be designed and reviewed before
  the first state-changing operation is built.
- **Authentication and TLS** for the API, and mutual TLS between nodes.
- **Audit log retention.** The log is strictly append-only today. Retention
  and archiving need a design that does not weaken that guarantee.
- **Secrets storage.** Nothing secret is stored yet. Secret encryption is
  required before anything is.

## Reporting a vulnerability

Skep is pre-release software with no deployed users. Until a security
contact is published, report issues privately to the repository owner rather
than in a public issue.
