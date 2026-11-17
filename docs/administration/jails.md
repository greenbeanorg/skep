# Jails

Skep runs FreeBSD jails on the host's own kernel, each on a ZFS clone of a
[jail template](jail-templates.md). This page covers what Skep does when
you create, start, stop, and delete one, and what a jail can and cannot do.
The design is [ADR 0010](../decisions/0010-storage-and-jails.md), decisions
8, 9, 12, and 13, with amendments e, f, and g.

Status: built in Milestone 7, phase 7b, step 7, and validated on FreeBSD
15.1 (`hearst`, see
[tests/integration/jails](../../tests/integration/jails/README.md)). Jails
have **no network interface yet** besides their own `lo0`: VNET networking
(an epair on an allow-listed bridge) is step 8. Resource limits, snapshots,
and clones are step 9; the web interface and `skepctl container` are step 10.

## The API

| Request | Permission | What it does |
|---|---|---|
| `GET /api/v1/containers`, `GET /api/v1/containers/{id}` | `container:read` | List or read jails; the state is what `jls` reports at the time |
| `GET /api/v1/containers/{id}/console-log` | `container:read` | The last 64 KiB of what `/etc/rc` and `/etc/rc.shutdown` wrote |
| `POST /api/v1/containers` | `container:write` | Create a jail from a template (`container.create`) |
| `PUT /api/v1/containers/{id}` | `container:write` | Change a stopped jail (`container.update`) |
| `POST /api/v1/containers/{id}/actions` | `container:power` | `start`, `stop` (optional `timeout`), `restart` |
| `DELETE /api/v1/containers/{id}` | `container:delete` | Destroy a stopped jail and its dataset (`container.delete`) |

The built-in `operator` role has all four permissions, `viewer` only
`container:read`. Building templates is `storage:write`, admin only. Every
write is a task and is audited in both logs; a delete lists every dataset
it destroyed.

```json
POST /api/v1/containers
{"name": "web1", "hostname": "web1.lab", "template_id": "<template ID>",
 "options": {"sysvipc": false}, "ssh_authorized_key": "ssh-ed25519 AAAA... me@laptop"}
```

- `name`: 1 to 63 letters, digits, `.`, `_`, `-`. `hostname` defaults to
  the name; a name with `_` needs an explicit DNS hostname.
- The template must be built and have the host's major.minor release
  (`template_not_provisioned`, `release_unsupported`).
- `options.sysvipc` gives the jail its own System V IPC (PostgreSQL needs
  it). `options.mlock` needs a locked-memory limit, `limits.locked_memory`,
  and RACCT (`mlock_requires_memory_limit`).
- `limits`: see [Limits](#limits).
- `ssh_authorized_key` is one OpenSSH public key line without options. It
  needs a template with sshd: otherwise `container_ssh_unavailable` (422).
  Templates built with the default `storage.template_packages` include
  `FreeBSD-ssh`; one built from `FreeBSD-set-minimal` alone does not.

## What happens

**Create.** Skep clones the template's `@skep-base` snapshot to
`<pool>/<root>/jail/<id>` (instant; the clone shares unchanged blocks with
the template), sets `readonly=off` on it (the template stays read-only),
and, with an SSH key, writes into the new tree, which has never run:

- `/etc/rc.conf`, starting with Skep's header line:
  `sshd_enable="YES"` and `sshd_flags="-o PermitRootLogin=prohibit-password"`
  (FreeBSD's sshd refuses root by default; this allows root with a key
  only, without editing `sshd_config`);
- `/root/.ssh/authorized_keys` (0600, in a 0700 `.ssh`, root:wheel).

Every one of these writes is made relative to the jail root's directory
descriptor with `O_RESOLVE_BENEATH | O_NOFOLLOW`, and files are replaced
by a new file and a rename, so a symbolic link cannot lead a write
outside the jail.

**Start.** Skep marks the jail as started for good, writes
`/var/run/skep/jails/<id>/jail.conf` (root only), and runs
`jail -f <conf> -c skep-<id>`, which creates the jail, mounts its devfs,
and moves its interfaces in, with nothing running in it (the jail.conf
has no `exec.start`). Before any process runs in the jail, Skep checks
it:

1. its parameters, read back from the kernel, must be the baseline
   below;
2. its interfaces must have left the host;
3. **the `/dev` guard:** the jail's `/dev`, listed from the host, must be
   a devfs and hold nothing outside the allow-list `fd`, `null`, `zero`,
   `crypto`, `random`, `urandom`, `pts`, `ptmx`, `stdin`, `stdout`,
   `stderr`, `zfs`, `fuse`, `pf`, plus `bpf` and `bpf<N>` for a jail with
   a DHCP interface.

A jail that fails a check is removed without running `/etc/rc.shutdown`
(none of its code has run) and the start fails with
`container_start_failed`, naming any extra device. Only then does Skep
run the jail's `/etc/rc` itself, as `jexec -l -U root skep-<id> /bin/sh
/etc/rc`, bounded by `containers.start_timeout_seconds` (default 120). If
`/etc/rc` fails or outlasts the timeout, the jail is stopped as below
(`/etc/rc.shutdown` runs) and removed, and the start fails with
`container_start_failed`. rc's output goes to
`/var/run/skep/jails/<id>/console.log`, which `GET .../console-log`
serves; it is gone after a host reboot.

**Stop.** `jail -r` runs `/etc/rc.shutdown`, sends SIGTERM to what is
left, and after the stop timeout SIGKILL; the kernel then removes the
jail and its devfs mount. A jail cannot refuse to stop, so there is no
force stop. The timeout is `containers.stop_timeout_seconds` (default 10)
or the request's `timeout` (1 to 300).

**Delete.** The jail must be stopped. Skep destroys its dataset and
snapshots and its runtime directory. A template with jails cloned from it
cannot be deleted.

**The agent restarting.** A jail is persistent: it outlives the agent,
and the agent reads every jail's state before it serves its socket. After
a host reboot every jail is stopped; Skep does not start it again (no
autostart), and `state_detail` says the jail should be running. A jail
whose start the agent's death interrupted (between `jail -c` and the end
of `/etc/rc`) is removed at the next agent start, without
`/etc/rc.shutdown`, since Skep cannot tell whether its `/dev` was checked;
`state_detail` says so.

## Limits

`limits` on create, or on an update of a stopped jail (which replaces
every limit), takes these fields; absent or zero means none (ADR 0010
decision 11, amendment j). All but `disk_quota` are RCTL deny rules on
`jail:skep-<id>`. They need RACCT: `kern.racct.enable=1` in
`/boot/loader.conf` and a reboot, the operator's choice (RACCT costs a
little on every process on the host). Without it a jail that asks for one
is refused with `rctl_unavailable` (409), at create, update, and every
start; Skep never starts a jail without the limits it was given. `GET
/api/v1/system` reports `containers.rctl` and `containers.max_cpu_percent`.

The rules are added after `jail -c`, while nothing runs in the jail, read
back before the `/dev` guard and `/etc/rc`, and removed when the jail
stops (RCTL would otherwise keep them on the jail's name).

What each limit does on FreeBSD 15.1, measured on the test host:

| Field | Mechanism | Behaviour |
|---|---|---|
| `memory` | RCTL `memoryuse` (≥ 16 MiB) | **Soft.** An allocation past it is not refused: a jail held twice its limit at once. While it is over, the host moves its pages to the inactive queue a little at a time (after about 80 s, under 1 MiB/s on the test host), so they are the first memory reclaimed when the host runs short; nothing is swapped out while memory is free. A target and an accounting figure, not a cap. An address-space limit (`vmemoryuse`) is not offered in this release. |
| `locked_memory` | RCTL `memorylocked` | The memory the jail may lock with mlock(2); `options.mlock` needs it. At most `memory` when both are set. |
| `cpu_percent` | RCTL `pcpu` | 100 is one CPU, up to 100 per host CPU. Throttles, approximately: RACCT samples and averages, so a busy jail swings around the limit (50 gave samples from 28 to 74; the loop ran at 61 % of its unthrottled speed). |
| `max_processes` | RCTL `maxproc` | A fork past it fails ("Cannot fork"). |
| `open_files` | RCTL `openfiles` (≥ 64) | Counts descriptor-table slots, not open files. Each process is charged its table size, which grows in steps (40, 128, 256, ... slots) and never shrinks; helper processes count too (FreeBSD's `tail` opens files through a Capsicum helper). With 256, one `tail -f` opened 59 files. Size it well above what the jail keeps open. |
| `disk_quota` | ZFS `refquota` on the jail root (≥ 64 MiB) | Works without RACCT; snapshots do not count. It includes the blocks the root shares with its template (about 78 MiB for a minimal template), so a 512 MiB quota left room for about 434 MiB of new data. Changed at once by an update; a quota below what the jail already references is refused. |

## The fixed baseline

Every jail gets the same parameters; none can be changed through the API:

- its own network stack (`vnet`), with an epair per NIC (step 8);
- `enforce_statfs=2`, `children.max=0`, `securelevel=2`;
- devfs with ruleset 5 (`devfsrules_jail_vnet`): `null`, `zero`,
  `random`, `urandom`, `fd`, `pts`, `std*`, and `zfs`. A jail with a DHCP
  interface gets Skep's ruleset 6001 instead: ruleset 5's own rules
  written out (`include 1` to `include 4`, `path pf unhide`) plus
  `path bpf* unhide`, which `dhclient` needs. The agent adds 6001 to the
  kernel when it is absent and refuses a start
  (`devfs_ruleset_conflict`) if 6001 holds anything else. Either way the
  `/dev` guard checks the result before the jail runs anything;
- `allow.suser` and `allow.reserved_ports` on (root in the jail is root
  there and can bind ports below 1024); every other `allow.*` off,
  including `allow.set_hostname`, `allow.mount`, `allow.chflags`, and
  `allow.unprivileged_proc_debug`;
- System V IPC off unless the `sysvipc` option is set;
- `exec.clean`, `exec.stop="/bin/sh /etc/rc.shutdown jail"`,
  `exec.timeout` from `containers.start_timeout_seconds` (default 120),
  and nothing else: no `exec.start` (Skep runs `/etc/rc` after the `/dev`
  guard) and never a host-side command.

What that means inside a jail: mounting, loading kernel modules, writing
sysctls, changing the hostname, lowering the securelevel, and clearing
`schg` flags all fail. **Raw sockets work** (`ping` does): FreeBSD gives
every VNET jail raw sockets on its own network stack, whatever
`allow.raw_sockets` says, so a jail can craft packets on its own
interface as a VM can on its NIC.

`securelevel=2` means base system files with the `schg` flag cannot be
replaced from inside the jail, so `pkg upgrade` of `FreeBSD-*` packages
in a jail fails. Packages from ports install normally.

## Writing into a jail only before its first start

Skep writes into a jail's tree only while it has never started
(`started_once` false). After the first start, an update that would
write into it (changing or removing the SSH key) is refused with
`container_started`, even after a rollback. The hostname and the options
are jail parameters, not files, and can change whenever the jail is
stopped.

## Host directories

| Path | Owner, mode | What |
|---|---|---|
| `<pool>/<root>/jail` (mounted) | root:wheel 0700 | The parent of every jail root. Checked before every start (`jail_parent_insecure` if it changed): a jail's tree holds setuid binaries a host user must never reach |
| `<pool>/<root>/jail/<id>` | the template's | A jail root |
| `/var/run/skep/jails/<id>/` | root 0700 | `jail.conf`, `console.log` |

## Configuration

```json
"containers": {
  "stop_timeout_seconds": 10,
  "start_timeout_seconds": 120
}
```
