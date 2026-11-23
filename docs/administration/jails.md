# Jails

Skep runs FreeBSD jails on the host's own kernel, each on a ZFS clone of a
[jail template](jail-templates.md). This page covers what Skep does when
you create, start, stop, and delete one, and what a jail can and cannot do.
The design is [ADR 0010](../decisions/0010-storage-and-jails.md), decisions
8, 9, 12, and 13, with amendments e, f, and g.

Status: built in Milestone 7, phase 7b, steps 7 to 9 (jails, VNET
networking, limits, snapshots, and clones), validated on FreeBSD 15.1
(`hearst`, see
[tests/integration/jails](../../tests/integration/jails/README.md)); what
is pending FreeBSD validation is listed in the step 9 progress log. The
web interface's Containers section is built in step 10 (see
[The web interface](web-ui.md)) and was validated against a real agent
on `hearst` on 2026-11-21 (section 26 of the
[step 10 progress log](../../tests/integration/jails/results/step10-progress.md));
`skepctl container` is still to come.

## The API

| Request | Permission | What it does |
|---|---|---|
| `GET /api/v1/containers`, `GET /api/v1/containers/{id}` | `container:read` | List or read jails; the state is what `jls` reports at the time |
| `GET /api/v1/containers/{id}/console-log` | `container:read` | The last 64 KiB of what `/etc/rc` wrote at the last start (see [The console log](#the-console-log)) |
| `POST /api/v1/containers` | `container:write` | Create a jail from a template (`container.create`) |
| `PUT /api/v1/containers/{id}` | `container:write` | Change a stopped jail (`container.update`) |
| `POST /api/v1/containers/{id}/actions` | `container:power` | `start`, `stop` (optional `timeout`), `restart` |
| `DELETE /api/v1/containers/{id}` | `container:delete` | Destroy a stopped jail and its dataset (`container.delete`) |
| `GET .../containers/{id}/snapshots`, `GET .../snapshots/{sid}` | `container:read` | List or read a jail's snapshots |
| `POST .../containers/{id}/snapshots` | `container:snapshot` | Take a snapshot (`container.snapshot.create`) |
| `POST .../snapshots/{sid}/rollback` | `container:write` and `container:snapshot` | Roll a stopped jail back (`container.snapshot.rollback`); it destroys the current data, so it needs the same as a clone (ADR 0010 amendment n) |
| `DELETE .../snapshots/{sid}` | `container:snapshot` | Delete a snapshot (`container.snapshot.delete`) |
| `POST .../containers/{id}/clone` | `container:write` and `container:snapshot` | Make a linked clone (`container.clone`) |

The built-in `operator` role has all five permissions, `viewer` only
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
  needs a template with sshd: otherwise `container_ssh_unavailable` (422). A template's `has_sshd` (in `GET /storage/templates`) answers that with the same check (amendment m); the web UI offers the key field only when it is true.
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
outside the jail. An existing `rc.conf` or `resolv.conf` is opened with
`O_NONBLOCK` as well and must be a regular file before anything is read
or replaced: a FIFO, device node, directory, or link that a template put
there fails the task instead of blocking it (ADR 0010 amendment n, SR-09).

**Nameservers.** `nameservers` (up to three IPv4 or IPv6 addresses) go
into the jail's `/etc/resolv.conf` before its first start, for static
addressing only: with a DHCP NIC, `dhclient` writes `resolv.conf`, and
nameservers are refused (`invalid_request`). For a jail without a NIC
they are refused with `nameservers_require_nic` (422), at create,
update, and clone, as the web interface offers the field only when the
jail has a NIC. An update that leaves `nameservers` out keeps the stored
list, so one that removes the last NIC of a jail with nameservers must
also send `"nameservers": []`.

**MAC addresses.** A NIC's MAC address, given in a create, an update,
or a clone, must be unicast, locally administered (the `0x02` bit of the
first byte set: the second hex digit is 2, 6, A, or E), and not all
zeroes; anything else is refused with `container_mac_invalid` (422).
Left out, Skep generates `02:` and five random bytes. A MAC address
that another Skep jail or a Skep VM already holds is refused at create,
update, and clone with `container_mac_in_use` (409), whose details name
the holder (`held_by_kind` `container` or `vm`, `held_by_id`,
`held_by_name`), as at start (amendment u). The VM side refuses a MAC
address a jail or another VM holds with `vm_mac_in_use` (409). At every start,
before any epair is created, the agent also refuses
(`container_mac_in_use`) a MAC address that a Skep VM holds, or that is
already present on the NIC's bridge: the bridge's own address, a member
interface's (`ether` or `hwaddr`), or one in the bridge's address table
(`ifconfig <bridge> addr`). So an API user cannot give a jail the MAC of
the LAN gateway, the host, or another machine the bridge has seen (ADR
0010 amendment n, SR-03). A device the bridge has not yet seen is not
caught; the locally-administered rule is what keeps burnt-in addresses
out. A MAC recorded before these rules is refused at start with
`container_mac_invalid`; give the NIC a new one with an update.

**Start.** Skep marks the jail as started for good, writes
`/var/run/skep/jails/<id>/jail.conf` (root only), and runs
`jail -f <conf> -c skep-<id>`, which creates the jail, mounts its devfs,
and moves its interfaces in, with nothing running in it (the jail.conf
has no `exec.start`). Before any process runs in the jail, Skep checks
it:

1. its parameters, read back from the kernel, must be the baseline
   below;
2. its interfaces must have left the host; each is then renamed inside
   the jail to `skep<slot>` (`skep0`, `skep1`, ...) with the host's
   `ifconfig -j skep-<id> <name> name skep<slot>`, so every jail's
   `rc.conf` names `skep0` whatever its ID, and a clone's copied tree
   configures its own NICs (ADR 0010 amendment k). While an epair is on
   the host its jail end has an ID-based name, `sj<10 hex digits>n<slot>`,
   and its host end keeps the description `skep:<id>:<slot>`;
3. **the `/dev` guard:** the jail's `/dev`, listed from the host, must be
   a devfs and hold nothing outside the allow-list `fd`, `null`, `zero`,
   `crypto`, `random`, `urandom`, `pts`, `ptmx`, `stdin`, `stdout`,
   `stderr`, `zfs`, `fuse`, `pf`, plus `bpf` and `bpf<N>` for a jail with
   a DHCP interface.

A jail that fails a check is removed without running `/etc/rc.shutdown`
(none of its code has run) and the start fails with
`container_start_failed`, naming any extra device. **If that removal
fails too**, the jail is *quarantined* (ADR 0010 amendment n, SR-06): the
start fails with `container_quarantined`, the container's `state` is
`failed` with both causes in `state_detail`, and it stays `failed` whether
or not the jail still exists. It is never shown as running, and start,
stop, and restart are refused with `container_quarantined`, because a
stop would run the refused jail's own `/etc/rc.shutdown`. A delete
removes it without `exec.stop` and then deletes the container (if the
removal fails again, the delete fails and the jail stays quarantined);
the agent's next start also retries the removal without `exec.stop` and,
if it works, records the container `stopped`. Only then does Skep
run the jail's `/etc/rc` itself, as `jexec -l -U root skep-<id> /bin/sh
/etc/rc`, bounded by `containers.start_timeout_seconds` (default 120). If
`/etc/rc` fails or outlasts the timeout, the jail is stopped as below
(`/etc/rc.shutdown` runs) and removed, and the start fails with
`container_start_failed`. If `jail -c` itself fails, the error carries
the first line of its standard error with control and format characters
removed and cut to 200 bytes (amendment r). rc's output goes to
`/var/run/skep/jails/<id>/console.log` (below), which `GET
.../console-log` serves; it is gone after a host reboot.

### The console log

rc gets a pipe as its standard output and error, not a file (ADR 0010
amendment o, SR-01). The agent truncates `console.log` at each start
(when rc is about to run) and drains the pipe into it: the first 256 KiB
are written as they arrive; anything more is counted and dropped, and
when the start ends a line `skep: N bytes discarded` and the last 64 KiB
of rc's output are written after it. A console log is therefore never
larger than about 320 KiB, and the 64 KiB `GET .../console-log` returns
ends with rc's real last lines. Its content is the jail's (a jail can
write anything there, including lines that look like Skep's).

Every start of a jail with the stock `/etc/rc` shows these four lines
after `Starting Network: lo0.`; they are harmless and come from the base
system, not from Skep:

```
route: message indicates error: File exists
add host 127.0.0.1: gateway lo0 fib 0: route already in table
route: message indicates error: File exists
add host ::1: gateway lo0 fib 0: route already in table
```

`/etc/rc.d/routing` always adds the loopback routes, but the kernel has
already created them when `lo0` got its addresses a moment earlier, so
`route add` reports `EEXIST`. Loopback works either way. Skep does not
filter them out of the console log (it never edits rc's output). Details:
[tests/integration/jails/results/route-file-exists-at-jail-start.md](../../tests/integration/jails/results/route-file-exists-at-jail-start.md).

The agent closes its end of the pipe as soon as the start ends, whether
rc succeeded, failed, or timed out, and never waits for every writer to
close it. **A process rc leaves running that later writes to its
standard output or error gets `SIGPIPE`, which kills it by default, or
`EPIPE` if it ignores the signal.** Properly daemonized services (stdio
on `/dev/null`, `daemon(8)`, the base system's rc.d scripts) are not
affected; a command started in the background from `/etc/rc.local`, such
as `foo &`, that prints later dies at its first write. Redirect such a
command's output inside the jail (`foo >/var/log/foo.log 2>&1 &`). No
process in a jail ever holds a descriptor on a host file.

`/etc/rc.shutdown`'s output (at a stop, a restart, or the removal after a
failed rc) is appended to the console log after a line `--- shutdown ---`
(amendment p). The jail.conf has no `exec.consolelog`, so it goes to
`jail -r`'s own standard output, of which the agent keeps the first
32 KiB and the last 32 KiB in memory, with a `skep: N bytes discarded`
line between them when there was more, and writes it after `jail -r`
returns. What `/etc/rc.shutdown` writes to standard error follows after a
line `--- shutdown stderr ---` (amendment q): its last 32 KiB, preceded by
a `skep: N bytes discarded` line when there was more. Both marker lines
are written even when their part is empty. The two streams are kept
apart, so how their lines interleaved is lost. A console log therefore
holds at most about 416 KiB. The shutdown output is written as the jail
produced it: control and format characters are not removed from the
file. **`GET .../console-log` cleans what it returns** (amendment r):
Unicode format characters (category Cf, among them the bidirectional
overrides and isolates U+202A–U+202E and U+2066–U+2069, and zero-width
characters) and C0 and C1 control characters (and DEL) are removed,
except newline and tab, so carriage returns go too and ANSI escape
sequences lose their ESC byte. A byte that is not UTF-8 is shown as
U+FFFD. The web UI and any other client therefore see the cleaned text;
read the file itself (`/var/run/skep/jails/<id>/console.log`, root only)
for the raw bytes. `truncated` still describes the file's tail, before
the cleanup. Its size never makes a stop
fail, and neither does a failure to write it (the agent logs
`jail_shutdown_log_failed`). A removal that runs none of the jail's code
(a refused or quarantined jail, or a start interrupted by the agent's
restart) appends nothing. Since the next start truncates the log, after a
restart it shows only the new start's rc output.

**Stop.** Skep first destroys the jail's epairs (destroying a host end
removes its jail end inside the jail), so no jail end comes back to the
host; the jail's `/etc/rc.shutdown` therefore runs without its NICs. Then
`jail -r` runs `/etc/rc.shutdown`, sends SIGTERM to what is
left, and after the stop timeout SIGKILL; the kernel then removes the
jail and its devfs mount. A jail cannot refuse to stop, so there is no
force stop. The timeout is `containers.stop_timeout_seconds` (default 10)
or the request's `timeout` (1 to 300). `/etc/rc.shutdown` itself is
bounded by `exec.timeout`, which is `containers.start_timeout_seconds`
(default 120), and the agent kills `jail -r` after the start timeout plus
the stop timeout plus 30 seconds.

**Skep can always remove its jails** (amendment u). The jail's root
controls its `/etc/rc.shutdown`, and one that exits non-zero, or hangs,
makes `jail -r` fail without removing the jail. When `jail -r` fails,
times out, or exits 0 but leaves the jail, the agent:

1. appends `/etc/rc.shutdown`'s output to the console log as for a
   successful stop (the `--- shutdown ---` and `--- shutdown stderr ---`
   sections, with what it wrote until it failed or was killed), then the
   line `--- jail -r failed: <line> ---`, where `<line>` is the last line
   of `jail -r`'s standard error (`no jail output on stderr` when there
   was none; `timed out after <duration>, ...` for a timeout);
2. removes the jail with `jail -R`, which runs no `exec.*` command and none
   of the jail's code: the kernel kills every process left in the jail;
3. does the cleanup `jail -r` would have done: the devfs left on
   `<root>/dev` is unmounted by its fsid, the epairs are destroyed, and
   the RCTL rules removed. The console log and the runtime directory stay.

The stop then succeeds. The agent logs `jail_remove_failed` (with
`jail_output`, the line) and then `container_stop_forced` at WARN; the
task result does not carry a warning (tasks have none in this release),
so the agent log and the console log are where a forced stop shows. The
line has control characters and Unicode format characters (category Cf,
such as the bidirectional overrides) removed and is cut to 200 bytes:
`jail -r`'s standard error begins with what the jail's `/etc/rc.shutdown`
wrote, so even its last line may be the jail's text. If `jail -R` fails
too, or leaves the jail, the stop (or restart) fails with
`container_remove_failed`, whose message gives both commands' last stderr
lines, cleaned the same way; the agent logs `container_remove_failed` at
ERROR. The same fallback applies to every removal Skep makes: the removal
after a failed `/etc/rc`, and the removals without `exec.stop` of a
refused, quarantined, or half-started jail (which append nothing to the
console log). A jail is quarantined only when `jail -R` fails as well.

**Delete.** The jail must be stopped, or quarantined (above); when
the removal of a quarantined jail fails even with `jail -R`, the delete
fails with `container_remove_failed` and the jail stays quarantined. Skep unmounts a devfs a removal
outside Skep left on its `<root>/dev` (below), then destroys its dataset
and snapshots and its runtime directory. A template with jails cloned from it
cannot be deleted.

**The agent restarting.** A jail is persistent: it outlives the agent,
and the agent reads every jail's state before it serves its socket. After
a host reboot every jail is stopped; Skep does not start it again (no
autostart), and `state_detail` says the jail should be running. A jail
whose start the agent's death interrupted (between `jail -c` and the end
of `/etc/rc`) is removed at the next agent start, without
`/etc/rc.shutdown`, since Skep cannot tell whether its `/dev` was checked;
`state_detail` says so. If that removal fails, the jail is quarantined
as above instead of being shown as running. A stop or restart task that
finds a jail still recorded `starting` (a start that ended without
recording its outcome) is refused with `container_quarantined` for the
same reason; the next agent start removes it.

A jail whose **stop** the agent's death interrupted (its epairs already
destroyed, `/etc/rc.shutdown` perhaps not finished) has its stop completed
at the next agent start, with `/etc/rc.shutdown` run again as a stop runs
it; it ends `stopped` with no epairs, and `state_detail` says the stop was
completed at agent start. This can delay the agent's start by up to the
jail's start timeout plus its stop timeout (about 160 s with the defaults)
per such jail. If the completion fails, the agent records the jail
`unknown` with the reason. What a read then shows depends on the host:
while the jail still runs (the usual case: `jail -r` and then `jail -R`
failed), `GET /api/v1/containers/{id}` and the list show `state: running`,
`desired_state: stopped`, its `jid`, and that reason in `state_detail`;
stop it again. If the jail is gone, they show `stopped` with no detail.
(ADR 0010 amendment aa, Proposed.)

**A jail running although it should be stopped.** Whenever a jail runs
while its desired state is `stopped` (a stop task failed, a stop
completion at agent start failed, or the jail was started outside Skep),
reads show the observed state, `running`, and keep the detail recorded
with the failure: a failed stop task's "the last stop failed: …", or the
completion's reason above. With no such detail, `state_detail` is "the
jail is running although it should be stopped; stop it again". A
successful stop clears the detail. A jail in the middle of its own start
or stop is not judged this way.

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
| `locked_memory` | RCTL `memorylocked` | The memory the jail may lock with mlock(2); `options.mlock` needs it. At most `memory` when both are set, and at most `containers.max_locked_memory_bytes`, which is also the total of all running jails (below). |
| `cpu_percent` | RCTL `pcpu` | 100 is one CPU, up to 100 per host CPU. Throttles, approximately: RACCT samples and averages, so a busy jail swings around the limit (50 gave samples from 28 to 74; the loop ran at 61 % of its unthrottled speed). |
| `max_processes` | RCTL `maxproc` | A fork past it fails ("Cannot fork"). |
| `open_files` | RCTL `openfiles` (≥ 64) | Counts descriptor-table slots, not open files: see below. |
| `disk_quota` | ZFS `refquota` on the jail root (≥ 64 MiB) | Works without RACCT; snapshots do not count. It includes the blocks the root shares with its template (about 78 MiB for a minimal template), so a 512 MiB quota left room for about 434 MiB of new data. Changed at once by an update. A quota below what the root would reference is refused before any task is submitted, with `container_disk_quota_below_usage` (422; details `disk_quota` and `referenced_bytes`): at create, the template's size, which a new root references from the start; at update, what the root already references. |

**`open_files` counts file-table slots, not open files.** RACCT charges
each process of the jail the size of its descriptor table, and the
jail's figure is the sum. A table grows in steps as the process uses
higher descriptor numbers (40 slots, then 128, then 256, then larger
steps), and it never shrinks while the process lives, even after the
files are closed. A process that has never needed more than its first
few descriptors is not charged at all. Helper processes count too:
FreeBSD's `tail` opens files through a Capsicum helper, which has its
own table. On the test host an idle minimal jail was charged 40 (cron
with 5 files open), and with `open_files` 256 one `tail -f` opened only
59 files: at the 60th its table and its helper's needed 128 slots each,
and 40 + 128 + 128 is past 256.

A rough sizing rule: for every process the jail runs at once, count at
least 40, or about twice the most files it ever has open, whichever is
larger (count helpers as processes); add the processes up and leave half
as much again for headroom. A jail running cron, sshd, and one daemon
that keeps about 100 files open needs about (40 + 40 + 200) x 1.5, so
420, or 512 to be safe. When a limit is hit, opens fail with "Too many
open files" (`EMFILE`), and `rctl -u jail:skep-<id>` on the host shows
the jail's current figure.

**What the limits do not cover** (ADR 0010 amendment n, SR-05). Even a
jail with every limit set can use these host-wide resources without a
Skep limit, so a jail's root (or a buggy service in it) can still
degrade the host and its other jails and VMs:

- **swap:** `memory` is soft (above), so a jail past it is paged out,
  not stopped; it can fill the host's swap. Skep sets no `swapuse` or
  `vmemoryuse` rule;
- **threads:** no `nthr` rule, so `max_processes` times
  `kern.threads.max_threads_per_proc` can reach the host-wide thread
  limit;
- **shared memory:** POSIX `shm_open` objects, and System V shared memory
  with the `sysvipc` option, have no `shmsize` rule;
- **pseudo-terminals:** no `pseudoterminals` rule;
- **disk I/O:** no `readbps`, `writebps`, `readiops`, or `writeiops`
  rule, so a jail can starve others of I/O on the shared pool
  (`disk_quota` limits space, not bandwidth);
- **kernel memory** that RACCT does not count: mbufs and socket buffers,
  sockets, and pf states in the jail's own pf (`/dev/pf` is visible).

**Limits are optional.** Every limit may be left out, and a user with
`container:write` can create a jail with no limits at all, or remove a
jail's limits with an update (the web UI asks first; the API does not).
Skep has no default limits and no limits required per role in this
release; that is in the backlog. Grant `container:write` only to users
you trust with the host's shared resources.

**Locked memory has a host total** (ADR 0010 amendment o, SR-04).
Locked pages are wired: they cannot be paged out, so the host, the
agent, other jails, and VMs lose them. The agent's
`containers.max_locked_memory_bytes` bounds the sum of `locked_memory`
over all running jails; by default (0 or unset) it is a quarter of
physical memory (`hw.physmem`), read once when the agent starts, so a
change of memory or of the setting needs an agent restart. A single
`locked_memory` above the total is refused at create and update
(`container_locked_memory_above_max`, 422). A start is refused when the
jail's `locked_memory` plus that of the jails already running or
starting is more than the total (`container_locked_memory_exhausted`,
409); the message gives both figures. A jail without `locked_memory`
always fits. Every `locked_memory` counts, with or without the `mlock`
option, since the figure is what RCTL allows the jail. If the agent could
not read physical memory and the setting is unset, no jail may have
`locked_memory`. The total is Skep's own; the kernel's bound on wired
user memory, `vm.max_user_wired`, is one system pool shared with
everything else on the host, **bhyve VMs included**: a VM whose guest
memory is wired (`bhyve -S`, `memory.wired`) draws from the same pool.
Skep's VMs do not wire their memory in this release, but leave room for
VMs and other wired memory when you raise the setting.

**One process locks at most its login class's limit** (amendments t and
u). Before the RCTL `memorylocked` rule, mlock(2) checks the process's
own RLIMIT_MEMLOCK, which comes from the jail's login classes
(`/etc/login.conf` in the jail; the defaults on 15.1):

- `/etc/rc`, which Skep starts with `jexec -l -U root`, `rc.local`, and
  their children, and `jexec -U root`, get the `root` class: unlimited.
- rc.d services started through rc.subr get the `daemon` class, 128 MiB:
  rc.subr applies it (`limits -C daemon`) to every service it starts. The
  jail's administrator raises it per service in the jail's `rc.conf`, with
  `<name>_limits="-l <size>"` (for example `postgresql_limits="-l 2g"`) or
  `<name>_login_class=<class>`.
- Other logins, and `jexec` without `-U`, get `default`, 64 KiB.

So a service that locks more than 128 MiB needs that setting in its jail
even when `limits.locked_memory` is higher. `limits.locked_memory` stays
the bound for the whole jail. Skep does not change these classes: after
the first start the jail's tree is its administrator's.

**Usage.** `GET /api/v1/metrics` lists every jail with the space of its
root (`disk_used_bytes`, snapshots included, and `disk_referenced_bytes`,
what `disk_quota` applies to) and, while it runs on a host with RACCT,
what `rctl -u jail:skep-<id>` reports: `cpu_percent` (approximate, as
for the limit), `cpu_time_seconds`, `resident_memory_bytes`,
`processes`, and `open_file_slots` (the figure `open_files` applies to,
not a count of open files). The agent only reads: no rule is added or
changed to get them. The figures are a sample the agent takes every 30
seconds, not a reading made for the request, so they can be up to 30
seconds old; `observed_at` in the response says when the sample was
taken. A jail started since the last sample is missing from it, and
one stopped since then still shows `running` with its last figures.

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
  guard), no `exec.consolelog` (amendment o), and never a host-side
  command.

What that means inside a jail: mounting, loading kernel modules, writing
sysctls, changing the hostname, lowering the securelevel, and clearing
`schg` flags all fail. **Raw sockets work** (`ping` does): FreeBSD gives
every VNET jail raw sockets on its own network stack, whatever
`allow.raw_sockets` says, and a VNET jail's root can also change its
interface's MAC and add addresses. Skep does not rely on
`allow.raw_sockets`, and root in a jail is not trusted on the network.
Today nothing on the host checks what a jail sends: see
[Known limitations](#known-limitations).

`securelevel=2` means base system files with the `schg` flag cannot be
replaced from inside the jail, so `pkg upgrade` of `FreeBSD-*` packages
in a jail fails. Packages from ports install normally.

**A jail removed outside Skep** (`jail -r` by hand, or a crash between
removing a jail and destroying its epairs) returns its jail ends to the
host named `skep<slot>`, and two such jails return two interfaces both
named `skep0`: FreeBSD 15.1 allows that on a return. Skep never acts on
those names. It finds a jail's epairs by their host ends' description
`skep:<id>:<slot>`; the `orphaned_interfaces` health check reports them,
and the jail's next start, stop, or delete destroys them (destroying a
host end removes its own jail end, whatever it is called). Don't run
`ifconfig skep0 ...` on the host while two exist: it acts on one of them.

A `jail -r` by hand without Skep's `jail.conf` can also leave the jail's
devfs mounted on `<root>/dev`, which keeps the dataset busy so the delete
would fail. Before a start, at a delete, and at every agent start for each
jail that does not run, Skep looks for such a mount in the mount table
(`getfsstat(2)`): only one whose mount point is exactly `<root>/dev` and
whose type is `devfs`. It unmounts it by its file system ID
(`unmount(2)` with `MNT_BYFSID`), never by a path, so a `dev` that is a
symbolic link cannot point the unmount at a host mount (ADR 0010
amendment l). Each one is logged with the event `orphaned_devfs_removed`,
the mount point, and the fsid; a failed check at agent start is logged as
`orphaned_devfs_check_failed`, and the jail's next start or delete tries
again.

## Known limitations

- **Anti-spoofing on jail ports** (ADR 0011, Milestone 7c): every jail
  NIC's epair host end carries a netgraph filter, installed before it
  joins the bridge. A static NIC can send only with its recorded MAC and
  configured IPv4 address (IP and ARP). A DHCP NIC gets MAC pinning only:
  its root can still claim another IPv4 address with its own MAC
  (traceable, not prevented). IPv6 is dropped on jail NICs, and so are
  DHCP-server replies and control frames such as STP and LLDP. Starts
  need the netgraph modules (`port_filter_unavailable` otherwise). See
  [Guest-port anti-spoofing](guest-port-filtering.md) for the details, the
  health checks, and the limits, including what loading `ng_socket` lets
  any VNET jail's root do.
- A jail whose root changes its own address in `rc.conf` loses
  connectivity on the new address: the recorded address is enforced.

## Snapshots and clones

A jail's root is one ZFS dataset, so a snapshot is one `zfs snapshot` of
it, named `@skep-<snapshot-id>` and carrying `skep:snapshot_id`, as for
VMs ([storage](storage.md)). Taken while the jail runs it is
`crash_consistent` (what a power cut would leave); taken while it is
stopped, `consistent`. Memory is not saved. A jail has at most
`storage.max_snapshots_per_resource` snapshots (default 64).

**Rollback** needs a stopped jail (`container_not_stopped`). Rolling back
to a snapshot that is not the latest destroys the newer ones and needs
`"destroy_newer": true`; the result and the audit entry name them. A
rollback never resets `started_once`: even after rolling back to a
snapshot taken before the first start, Skep does not write into the tree
again.

**A clone** is a new jail whose root is `zfs clone` of one snapshot of the
source's root. Without `snapshot_id` the task takes a snapshot first,
named after the clone, and leaves it behind. The clone gets a new ID, the
given name and hostname (default: the name), and new MAC addresses, and
keeps the source's template, options, limits (the disk quota, which ZFS
does not copy to a clone, is set again), SSH key, NIC slots, bridges, and
addressing. Its in-jail interfaces are `skep0`, `skep1`, ... like every
jail's, so the `rc.conf` it copied configures them. While the clone
exists, its origin snapshot and the source cannot be deleted
(`snapshot_has_clones`).

```json
POST /api/v1/containers/<id>/clone
{"name": "web2", "snapshot_id": "<snapshot ID>"}
```

- **A clone has its source's `started_once`.** A snapshot of a jail that
  has started may hold a tree that has run code, so Skep never writes
  into such a clone.
- **Static addresses.** Every NIC of the source with a static address
  needs new addressing in `nics` (one entry per NIC, in slot order; empty
  fields keep the source's), or the request is refused with
  `clone_addressing_required` (422) and the slots. The new address must go
  into the clone's `rc.conf`, so it is accepted only when the source has
  never started; for a started source the request is `container_started`.
  **A jail with a static address can therefore be cloned only before its
  first start.** A DHCP jail can always be cloned, and its clone asks for
  its own lease with its new MAC address.
- **A start is refused when a running or starting Skep jail on the same
  bridge holds one of the jail's static addresses**
  (`container_address_in_use`). The addresses compared are the ones Skep
  wrote; an address changed inside a jail, a DHCP lease, and another
  bridge on the same L2 segment are not seen. Two starts submitted at the
  same moment are checked against each other: the check is repeated in
  the database transaction that records the start, so the second one
  fails with the same code (SR-07).
- **A clone is the same guest.** Its `/etc/ssh` host keys and
  `/etc/hostid` are the source's. Regenerate the host keys inside the
  clone before it shares a network with the source:

  ```sh
  rm /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
  service sshd restart   # sshd's rc script generates new keys
  ```

  An option to reset the host keys on clone is in the backlog.

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
  "start_timeout_seconds": 120,
  "max_locked_memory_bytes": 0
}
```

`max_locked_memory_bytes` is the total `limits.locked_memory` of all
running jails (see [Limits](#limits)); 0 means a quarter of physical
memory, and a negative value is refused at load.
