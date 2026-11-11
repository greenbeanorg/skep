# ADR 0002: Host discovery abstraction

- Status: Accepted
- Date: 2026-11-11

## Context

The agent has to describe its host: CPU, memory, operating system, and
whether bhyve, QEMU, and ZFS can be used. The production host is FreeBSD.
Development and CI run on Linux, where none of the FreeBSD interfaces exist.

Two problems follow. The agent must run on Linux so the API and service
layers can be developed there. And the FreeBSD logic, which is the part that
matters, must be testable on Linux, because that is where unit tests run.

## Decision

### One small interface

```go
type Discoverer interface {
    Discover(ctx context.Context) (models.Hardware, error)
}
```

`hardware.New` returns the implementation for the build target. The service
layer depends only on the interface.

### Platform logic is untagged; only the system bindings carry build tags

The FreeBSD logic in `freebsd.go` has no build tag. It reaches the system
only through four seams:

| Seam | Real implementation | In tests |
|---|---|---|
| `SysctlReader` | `sysctl(3)` through `golang.org/x/sys/unix` (`sysctl_freebsd.go`) | Values parsed from a captured `sysctl` fixture |
| `existsFunc` | `os.Lstat` | A set of paths |
| `pathFinder` | Search of fixed system directories | A map |
| `CommandRunner` | `exec.Command` with fixed arguments | Canned output |

The Linux logic in `linux.go` is built the same way around an `fs.FS` rooted
at `/`. Only `sysctl_freebsd.go`, `new_linux.go`, and `new_other.go` are
tagged, and each is a few lines of wiring.

The result is that the FreeBSD decision logic is compiled, vetted, and unit
tested on every platform, against fixtures captured from a real FreeBSD 15.1
host.

### Native interfaces before commands

On FreeBSD, facts come from `sysctl(3)`, not from running `sysctl(8)`, and
module state is inferred from sysctl OIDs that exist only while a module is
loaded (`hw.vmm.maxcpu`, `vfs.zfs.version.module`), not from `kldstat(8)`.
Commands are used only where no stable native interface exists: listing
pools (`zpool list`) and reading a QEMU version (`--version`).

`golang.org/x/sys` is added as a dependency for this. It is maintained by
the Go team and is the standard way to reach `sysctl(3)` without cgo.

### Capabilities report status and reason

Every capability carries `status` (`available`, `unavailable`, `unknown`)
and a `reason`. A failed probe degrades one capability or adds a warning. It
never fails the pass. `Discover` returns an error only when its context is
cancelled.

`unknown` is distinct from `unavailable` on purpose. "The check failed" and
"the check ran and the answer is no" lead to different actions for an
administrator and, later, for the scheduler.

### Linux is labelled as what it is

The Linux implementation reports `platform: linux-dev`. bhyve is always
`unavailable` there, with a reason that says so. Nothing on Linux stands in
for a FreeBSD feature.

## Alternatives considered

- **Everything behind build tags.** Simple, but the FreeBSD logic would only
  compile and be tested on FreeBSD, so most mistakes would surface late.
- **Shelling out to `sysctl`, `kldstat`, and `uname` and parsing text.**
  Easy to fixture, but slower, dependent on output formats, and it adds
  command execution to a privileged agent where a system call would do.
- **One struct of function fields per fact.** More flexible, but it scatters
  the decision logic. Four seams proved to be enough.

## Consequences

- Fixtures need to be kept honest. `internal/hardware/testdata/README.md`
  records which are real captures and which are synthetic.
- Passing unit tests on Linux show that the logic handles the fixture. They
  do not show that a real FreeBSD host produces that input. Runtime behaviour
  is validated on FreeBSD and tracked in the architecture overview.
- This interface is for host facts. Hypervisor engines get their own
  interface with per-engine capability discovery when the first VM milestone
  starts. That interface is not designed here.
