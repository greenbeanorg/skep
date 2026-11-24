# Security policy

## Status

Skep 0.1 is a single-host preview, validated only on lab virtual machines
nested under KVM. It is not for production use. `skep-agent` runs as root,
and the boundary between it and `skep-api` has had no external security
review. The [README](README.md) status note and the
[security overview](docs/security/overview.md) describe what is and is not
protected.

## Supported versions

Only the latest release receives security fixes. Older releases are not
patched.

## Reporting a vulnerability

Report a vulnerability privately through GitHub's private vulnerability
reporting: open the repository's **Security** tab and choose
**Report a vulnerability**. Do not open a public issue, pull request or
discussion for a vulnerability.

Skep has one maintainer. Reports are handled on a best-effort basis, and
there is no promised time for a response, a fix or a disclosure.

## What to include

A useful report contains:

- The Skep version, as printed by `skep-agent -version` (and
  `skep-api -version` if it differs), for example
  `skep-agent 0.1.0 (commit ..., built ..., go... freebsd/amd64)`.
- The FreeBSD version, as printed by `freebsd-version -ku`, and whether the
  host is bare metal or a virtual machine.
- The steps to reproduce the problem, starting from a fresh install where
  possible, and the configuration involved (`agent.json`, `api.json`) with
  passwords, keys and tokens removed.
- What happens, what you expected to happen, and what an attacker gains:
  for example, which account or network position the attack needs and what
  it reaches.
