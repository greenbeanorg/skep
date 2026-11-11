# ADR 0003: Host discovery is read-only

- Status: Accepted
- Date: 2026-11-11

## Context

Discovery runs at every agent start and again whenever the API is queried
and the cache has expired. It runs on hosts that may be carrying production
workloads, possibly with root privileges once the agent manages hypervisors
and storage. Anything discovery changes, it changes repeatedly, unattended,
and on every node.

Some of the obvious ways to answer "can this host run bhyve?" change the
host. Loading `vmm.ko` to see whether it loads is one. Others are less
obvious: on FreeBSD, running `zpool` as root loads `zfs.ko` if it is not
loaded yet.

## Decision

**Discovery observes the host and never changes it.** Specifically:

1. It never loads or unloads a kernel module. It reports whether a module is
   loaded and whether the module file is present, and stops there.
2. It never runs a command that can change state as a side effect. The pool
   list is requested only when the ZFS module is already loaded. A unit test
   (`TestFreeBSDNeverRunsZpoolWhenZFSNotLoaded`) guards this.
3. It never writes outside the agent's own data directory, and creates no
   devices, datasets, interfaces, or configuration.
4. The commands it may run are an allow-list with fixed arguments:
   `zpool list -H -p -o name,size,allocated,free,fragmentation,capacity,health`
   and `<qemu binary> --version`. Adding a command means changing this ADR.
5. Enabling a capability is an explicit administrative action. Until Skep
   has authenticated, audited operations for that, an administrator does it
   by hand (for example `kldload vmm`).

## Consequences

- A capability that needs a module to be loaded is reported `unavailable`
  with a reason that says the module is loadable. The agent does not "fix"
  it. This is deliberate: a host where bhyve is `unavailable` because vmm is
  not loaded is correctly described, not broken.
- Some facts cannot be established without changing state and are therefore
  not reported. CPU virtualization support (VT-x, EPT) is only confirmed by
  the vmm module once it is loaded. Discovery says so in the reason text
  instead of guessing.
- Discovery is safe to run on any host at any time, including one that is
  not meant to become a Skep node, and safe to run unprivileged. It was
  validated on FreeBSD 15.1 as an unprivileged user and as root, with
  `kldstat` and `zpool list` showing the same state before and after.
- When operations that load modules or create resources are added, they go
  through the task engine with authentication and audit logging. They do not
  go into the discovery package.

## Alternatives considered

- **Load vmm on demand during discovery.** Gives a definite answer about
  hardware support, but changes kernel state on every node without anyone
  asking, and makes a read-only API endpoint privileged.
- **Run `zpool list` unconditionally.** Simpler, but loads ZFS on hosts that
  do not use it, such as UFS-root installs that have not opted in.
