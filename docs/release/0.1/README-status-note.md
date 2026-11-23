# Draft: README status note for 0.1

*Draft for Andy; replaces the "Status" paragraph and the security notice at
the top of README.md when 0.1 is tagged. Links are relative to the repository root, as README.md needs.*

---

> **Status: 0.1.0, a pre-release. Not for production.**
>
> Skep 0.1 manages one FreeBSD 15.1 host: bhyve virtual machines, jails, ZFS
> storage and guest-port filtering, through a web interface, a REST API and
> `skepctl`. It has been run on lab virtual machines only (Intel VT-x, nested
> under KVM); bare metal and AMD hosts are not validated yet
> ([bare-metal runbook](tests/integration/bare-metal/README.md)). There is no
> clustering, no high availability and no backup in 0.1.
>
> **It runs a root agent.** `skep-agent` runs as root on the host and performs
> privileged operations (bhyve, jails, ZFS, network interfaces) on behalf of
> the unprivileged `skep-api`. The boundary between them is a Unix socket
> with a closed set of typed operations and an allow-list in `agent.json`
> ([ADR 0004](docs/decisions/0004-privilege-boundary.md)), and it has had no
> external security review. A flaw in the agent, or in what it allows, is a
> flaw with root privileges. Run 0.1 on a host you can afford to rebuild,
> keep `skep-api` on loopback or a management network you trust, and read
> [docs/security/overview.md](docs/security/overview.md) first.
>
> Known issues: [docs/release/0.1/known-issues.md](docs/release/0.1/known-issues.md).
