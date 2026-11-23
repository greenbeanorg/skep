# skep-setup: configuring, upgrading and removing a host

`skep-setup` is installed with the package (`/usr/local/sbin/skep-setup`)
and does the host configuration the package itself never does
([ADR 0012](../decisions/0012-installer.md)). It is the automated form of
the manual procedure in [Host requirements](host-requirements.md), and
follows the same rules: it only adds, it never loads a module or reboots,
it never creates a pool, and it never changes an interface or bridge it
did not create.

**Status (2026-11-22):** built and tested on Linux against fixture roots
(copies of a stock UFS install, a stock ZFS install, and `hearst`'s
files), and validated on the two lab VMs ADR 0012 asks for, FreeBSD
15.1-RELEASE-p7: `smith` (UFS root, distribution sets) and `langrishe`
(ZFS root, pkgbase). `check`, `plan`, `apply` (with `-start` and
`-admin`), a second `apply`, `rollback` of an apply and of a remove,
`upgrade` (both refusals and the success path) and `remove` (default and
`-revert-host -purge-data`) ran against the real hosts; rc.conf and
loader.conf came back byte-identical. Three fixes came out of it (sysrc
refuses dotted names; a second apply with `-start -admin` was not a
no-op; revert left `NAME=""`). Record:
[lab-VM validation](../../tests/integration/setup/results/lab-vm-validation-20261122.md).

```
skep-setup check     [answers]   read-only: what is configured, missing, in conflict, refused
skep-setup plan      [answers]   read-only: the exact changes apply would make
skep-setup apply     [answers]   make them (asks first, or -yes)
skep-setup rollback  [ID]        undo one apply or remove where files are unchanged since
skep-setup upgrade               after "pkg upgrade skep"
skep-setup remove    [-plan]     before "pkg delete skep"
```

Run it as root: it reads `agent.json` (`0600`).

| Exit status | Meaning |
|---|---|
| `0` | done, or nothing to do |
| `1` | `check`: something for `apply` or for you; any command: not confirmed |
| `2` | usage |
| `3` | refused: nothing was (or would be) done. `upgrade` also exits 3 for its failures before the agent's stop (for example a backup), after starting `skep_api` again; its message says what was left |
| `4` | **incomplete**: changes were made and the run failed or its result could not be verified. The message starts `incomplete:`. `upgrade`: the agent was stopped and did not come back as the new build (its start or its check failed), `skep_api` did not come up after the agent's restart, the agent did not answer on its socket within the wait, or its health was not `ok` (see [Upgrades](#upgrades)). `apply`: an action failed after earlier ones changed something, or the failed action may itself have changed something (a failed service stop always counts), or the administrator's creation failed after other changes or was killed or timed out. `rollback`: it failed after restoring or removing some files. `remove`: as `apply`. See [Failures part way](#failures-part-way) |

## The answers

| Flag | Meaning |
|---|---|
| `-mgmt IF[,IF]` | **Required.** The management interface(s). Never changed. The first one's current address is the default listen address. Guests are refused on any bridge that contains one |
| `-bridge NAME` | An existing guest bridge to allow-list. Setup changes nothing on it |
| `-create-bridge NAME -uplink IF` | Create a new guest bridge on a spare interface with no address and no other settings, `vlanfilter` on |
| `-pool NAME` | A ZFS pool to allow-list; it must be imported and `ONLINE` |
| `-dir-pool NAME=/PATH` | A directory pool for VM disks; the path must be a real directory (not a symbolic link), owned by root (UID 0), and not writable by its group or others |
| `-listen ADDR:PORT` | The API listen address (default: the management address, port 8443). `0.0.0.0` or `::` only with `-listen-all` |
| `-no-racct` | Do not add `kern.racct.enable="1"` (jail limits other than `disk_quota` are then refused) |
| `-start` | Start both services at the end of `apply` |
| `-admin NAME` | Create the first administrator (asks for the password, twice, without echo). Needs `-start`, or `/var/db/skep/api` to exist already (the API has run once); otherwise the plan refuses, because `skep-api admin create` run as root first would create the API's data directory owned by root ([ADR 0012 amendment](../decisions/0012-installer.md#amendment-2026-11-22-build-decisions-confirmed), build decision 8). Setup runs `skep-api admin create` **as the `skep` user** (UID and primary GID from `/etc/passwd`, never root), and only once `skep_api` has written `/var/run/skep/api.pid` and created `api.db` (it waits up to 30 s). If it fails, every other change stays made, the journal is finished with the error, and setup prints the same `apply` command to retry: it then plans only the administrator. Whether the administrator exists is asked of `skep-api` itself: with no `api.db` the create is planned; with one, `check`, `plan` and `apply` run `skep-api admin exists --username NAME` as the `skep` user with the fixed environment (read-only; it never creates or migrates `api.db`). Exit 0 is `ok`, 1 plans the create, anything else refuses with skep-api's error. Asking needs root; as another user, or with `-root` other than `/`, the plan refuses `-admin` while `api.db` exists. A NAME that exists already is left as it is: `ok` when it is an enabled `admin`, a **warning** when it is disabled or has another role (setup never changes an existing user). |

## What apply changes

| Item | Change | Effect |
|---|---|---|
| `/boot/loader.conf` | `vmm_load="YES"`, `nmdm_load="YES"` if missing | Next boot |
| `/boot/loader.conf` | `kern.racct.enable="1"` unless `-no-racct` | **Only after a reboot** |
| `/etc/rc.conf` | `kld_list+=` the missing ones of `if_epair netgraph ng_socket ng_ether ng_bpf` | Next boot |
| `/etc/rc.conf` | `zfs_enable="YES"` when a pool is named | Next boot |
| `/etc/rc.conf` | With `-create-bridge`: `cloned_interfaces+=`, `ifconfig_UPLINK="up"`, `ifconfig_BRIDGE="addm UPLINK ifuntagged UPLINK 1 defuntagged 1 vlanfilter up"` | Next boot; the plan prints the `ifconfig` lines to run now |
| `/etc/rc.conf` | `skep_agent_enable="YES"`, `skep_api_enable="YES"` | |
| `agent.json` | Written once (pools, directory pools, bridges, management interfaces), `0600 root:wheel` | Agent start |
| `api.json` | Written once with the listen address, `0640 root:skep` | API start |
| `/var/db/skep/media` | Created `0700 root:wheel` | |

Every `rc.conf` and `loader.conf` change goes through `sysrc`, except
`kern.racct.enable`: FreeBSD 15.1's `sysrc` refuses a name with a dot
("name contains characters not allowed in shell", for setting, reading
and `-x` alike), so setup appends that one line itself (and `remove
-revert-host` deletes exactly that line), replacing the file atomically
with its mode and owner and syncing the directory; a `loader.conf` that
is not a regular file (a symlink, for one) is refused, never followed.
Every result is read back and checked. A variable already set to another value
is a **conflict**: reported, never overwritten. Each variable is read
again just before its `sysrc NAME=` (or, in `remove`, `sysrc -x`); if
someone set or changed it after the plan, that one change is **skipped**
(printed, and recorded in the journal) and the rest of the run goes on.

`agent.json` and `api.json` are created, never rewritten. One that exists
and that setup did not write is the administrator's: setup parses it and
compares the allow-lists (`storage.pools`, `storage.directories`,
`network.bridges`, `network.management_interfaces`) or the listen address
(`listen_address`; absent means skep-api's default `127.0.0.1:8443`) with
the answers. The same is `ok` and the file is left as it is; different is
a **conflict**, printed with setup's proposed content; a file that is not
JSON is **refused**.

Every program setup runs as root is named by its absolute path
(`/usr/sbin/service`, `/usr/sbin/sysrc`, `/usr/sbin/pw`, `/usr/bin/fstat`,
`/sbin/zfs`, `/sbin/zpool`, `/sbin/ifconfig`, `/usr/sbin/jls`,
`/sbin/sysctl`, `/sbin/kldstat`, `/bin/freebsd-version`) and gets a fixed environment
(`PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin`,
`HOME=/`; `TERM` is kept only for the interactive `admin create`).
`apply`, `rollback`, `upgrade` and `remove` set umask `022` first. With
`-root` other than `/` (tests), anything that acts on this host's
services or accounts (`-start`, `-admin`, `remove`'s service stops and
`pw`, and `upgrade` as a whole) is refused.

**Kernel modules.** The `loader.conf` and `kld_list` lines take effect at
the next boot; setup never loads a module itself. `check`, `plan` and the
last lines of `apply` therefore compare the guest modules the boot
configuration loads (`vmm`, `nmdm`, and `if_epair netgraph ng_socket
ng_ether ng_bpf` from `kld_list`, including the ones this plan adds) with
what the kernel has now (`kldstat -q -m NAME`; a module compiled into the
kernel counts as loaded). Each one configured but not loaded is listed in
one **warning**, with what does not work without it. A reboot is always the
complete fix. `kldload` loads modules only: loader tunables added since boot
(`kern.racct.enable` when setup added it) still need the reboot. The warning
also prints a `kldload` command for the modules that are safe to load now, for
example

```
  warn     kernel modules                     configured but not loaded: vmm nmdm
                                                ...
                                                $ shutdown -r now
                                                $ kldload -n vmm nmdm
```

`vmm` reads its `hw.vmm.*` tunables from the kernel environment when it
loads, and `kldload` does not read `loader.conf`. Setup therefore compares
each `hw.vmm.*` line of `loader.conf`, `loader.conf.local` and `loader.conf.d`
with `/bin/kenv -q NAME`. If one differs, is absent, or cannot be read, `vmm`
is left out of the printed command and the warning says that loader tunables
for vmm set since the last boot are not applied by kldload, so reboot.

The warning does not change any exit code. Until the modules are loaded
the agent's health can read `degraded`, and `upgrade` then exits 4
(incomplete) with the same list in its message. `apply` does not load
them, since apply never changes the running kernel: setup reports them
and tells you to reboot (or load the ones it prints yourself; Andy's
decision, 2026-11-23).

**Firmware.** `check` and `plan` print a **warning** when the bhyve UEFI
firmware is missing (`BHYVE_UEFI.fd` or `BHYVE_UEFI_VARS.fd` in
`/usr/local/share/edk2-bhyve`, the files the agent opens): it names the
package, `edk2-bhyve`, and says VMs cannot be created without it. The skep
package depends on it, so a package install has it
([packaging](packaging.md)); the warning is for a host installed by hand,
or one where the firmware was removed afterwards. Setup never installs it:
`pkg install edk2-bhyve`.

## What it refuses

`apply` does nothing at all while any of these stands:

- FreeBSD other than 15.x, an architecture other than amd64, or a kernel
  and userland at different releases (15.x other than 15.1 is a warning);
- no `rc.conf`;
- a management interface that does not exist, has no `ifconfig_` line,
  or has one with neither an address nor DHCP (**DHCP is accepted with a
  warning**: the listen address is the current lease's);
- a bridge to allow-list that has a management interface as a member
  (now, by `ifconfig`, or in `rc.conf`), does not exist, or exists but is
  not a bridge;
- a new bridge whose uplink is a management interface, does not exist,
  has an address (now or in `rc.conf`), has other settings or aliases, or
  is in another bridge (now, by `ifconfig`, or in `rc.conf`); or a new
  bridge name that exists already;
- only one of the `skep` user and group, a `skep` user with a login shell
  or a home, no `skep` account at all (the package creates it), or UID or
  GID 409 held by another name;
- an `agent.json` or `api.json` it did not write and cannot parse;
- a pool that is not imported or not `ONLINE`; a directory pool that is a
  symbolic link, not a directory, not owned by root, or writable by its
  group or others;
- `0.0.0.0` without `-listen-all`; no address to listen on;
- an earlier run that did not finish (roll it back first);
- a `/var/db/skep` or `/var/db/skep/setup` that is a symbolic link or not a
  directory (either may be missing), or any of `/var/db/skep/setup`,
  `/var/db/skep`, `/var/db`, `/var` not owned by root or writable by its
  group or others: every command that reads or writes there (`check`,
  `plan`, `apply`, `rollback`, `upgrade`, `remove`) stops with exit 3 before
  using it.

Bridges are judged as they are **now** as well as by `rc.conf`: setup
reads `ifconfig -g bridge` and each bridge's `member:` lines and `bridge
flags` (`VLANFILTER`). An allow-listed bridge that exists only at run time
(no `ifconfig_` line in `rc.conf`) is a warning, because it is gone after a
reboot; so is `vlanfilter` that is on now but not in `rc.conf`. When the
members now and in `rc.conf` differ, the plan shows both. If `ifconfig -g
bridge` itself fails, `check` and `plan` stop with the error rather than
assume there is no bridge (on FreeBSD 15.1 it exits 0 with no output when
there is none), as they do when one bridge cannot be read.

For an allow-listed bridge without `vlanfilter`, setup first looks for
Skep guest ports among its live members (`epair*`, `tap*`). If there are
any, it prints no live steps and no `sysrc` step: stop the Skep guests on
this bridge first, then run `plan` again. Setup never sets a PVID on a
guest port; the agent sets each one when the guest starts. Otherwise the
steps are:

- **`vlanfilter` in `rc.conf` but off now**: an item of its own saying so.
  It takes effect at the next boot, or run the printed live steps now;
  there is no `sysrc` step, because the line already has it.
- **a bridge that exists now**: the live steps (each member's PVID with
  `ifuntagged MEMBER 1`, then `defuntagged 1`, then `vlanfilter`, then
  `ifconfig MEMBER up`, because adding a member does not bring it up), run
  from the console, then the `sysrc` lines that keep it across reboots.
- **a bridge that exists only at run time**: also
  `sysrc cloned_interfaces+="BRIDGE"`, unless `cloned_interfaces` already
  names it; without it the bridge is not created at boot.
- **a bridge in `rc.conf` not created yet**: only the `sysrc` line.

The `sysrc ifconfig_BRIDGE=` line names only real uplinks: for a bridge in
`rc.conf`, its `rc.conf` members (the existing line, with `ifuntagged`,
`defuntagged` and `vlanfilter` added where missing); for a run-time
bridge, its live members. An uplink with no `ifconfig_` line also gets
`sysrc ifconfig_UPLINK="up"`; an existing line is never replaced, and if it
does not bring the uplink up the item says to add `up` by hand. Every
interface name put into a printed command must be letters,
digits, `.` and `_`, and every word of a reprinted `rc.conf` line letters,
digits and `. _ : / -`; otherwise setup prints no steps for that bridge and
says which name stopped it.

A file that is not plain `name=value` lines (shell code, a substitution,
a sourced file), or a variable set in `rc.conf.local` or `rc.conf.d`,
makes setup edit nothing there and print the steps instead.

Never touched: any pool, any existing bridge (for one without
`vlanfilter` it prints the safe order: each member's PVID, then
`defuntagged`, then `vlanfilter`), routes, `pf`, `devfs.rules`,
`login.conf`, `sysctl.conf`, running guests.

## The journal and rollback

Each `apply` (and `remove`) creates `/var/db/skep/setup/<UTC timestamp>/`
(`0700`), with a copy of every file before its first change and
`journal.json`: each action, and the SHA-256 of the file before and
after. A second `apply` with the same answers finds nothing to do and
writes no journal: with `-start`, a service that is running already
(`service NAME status`) is `ok`; with `-admin`, a username that exists in
`api.db` is `ok`. The journals do not decide this: they survive
`pkg delete` and `remove -purge-data`, so after a reinstall a journal's
`admin-create` says nothing about the new database (finding 1 of PR #52).

`skep-setup rollback [ID]` (default: the newest run) first prints the
run it would undo (its ID, command, start time, and how many changes it
made), then asks. It puts a file back only if it is still byte-identical
to what the run left (a new file renamed into place, with the current
owner and mode, synced); a file changed since is reported and left alone.
Files and directories setup created are removed. It does not stop or
start services, delete the administrator, unload modules or destroy
interfaces. If an `apply` fails part-way, it stops, and the next `plan`
refuses until that run is rolled back (a failed `-admin` is the
exception, above).

A journal directory without a readable `journal.json` (a run that
stopped before its first save, a damaged file, or one whose format or ID
this skep-setup does not understand, such as a newer version's or a
directory copied under another name) does not make every
command fail: it shows as an unfinished run with no recorded changes,
which the plans refuse on until `skep-setup rollback ID` clears it.
Rollback then undoes nothing (setup cannot tell what that run changed),
keeps a damaged file as `journal.json.unreadable`, and names the run's
`files/` directory.

## Failures part way

When `apply`, `rollback` or `remove` fails, the exit status says whether it
had changed anything. A failure before any change stays exit 3. After a
change it is exit 4, and the message lists what was changed, what may have
changed (the failed action, when setup cannot show it left things as they
were), what was not changed, and what to do next:

| Failure | Running the same command again |
|---|---|
| `apply`: an action failed | Not yet. The plan refuses until `skep-setup rollback ID` undoes the run. Rollback puts the files back; it does not stop services or delete users |
| `apply`: only the administrator's creation failed | Safe. The journal is complete and the command plans only the administrator |
| `rollback`: failed part way | Safe. Files already put back are left as they are. It can fail the same way until the cause is fixed |
| `remove`: an action failed | Not yet. The plan refuses until `skep-setup rollback ID` clears the run. Rollback puts back the `rc.conf` and `loader.conf` lines; it does not start services, restore deleted data, or create the account again |

An `apply` or `remove` that failed with exit 3 also leaves an unfinished
journal, so it too needs `skep-setup rollback ID` before the next run; that
rollback has nothing to undo. An `apply` whose only action is the
administrator's creation changes nothing when the command exits with an
error: exit 3, and it can be run again. If that command was killed or timed
out, setup cannot tell whether the administrator was created: exit 4, listed
under "May have changed", and running the same command again is still safe.

A failed service stop is always "May have changed", even if the service
still runs, because the stop signal may have been delivered.

## Upgrades

```sh
pkg upgrade skep
skep-setup upgrade
```

`upgrade` refuses, changing nothing, unless the installed `skep-agent`,
`skep-api` and `skep-setup` are one build, and while any agent task is
**pending or running**. A task interrupted by the agent restart is never
resumed or re-run: it ends `interrupted` and must be resubmitted, so only
`-force` upgrades over unfinished tasks. It reads the tasks from the
database the agent's configuration names (`agent.json`, or
`skep_agent_config`, with `skep_agent_data_dir`); if that database is
missing while the agent answers on its socket, it refuses, since it would
be reading the wrong configuration. `-keep` must be at least 1, and
`/var/db/skep/backup` must be creatable, before anything is stopped. A
service whose data directory is not the default (`skep_agent_data_dir` or
`skep_api_data_dir` set in `rc.conf` to anything but `/var/db/skep`) is
refused before anything is stopped: upgrade backs up and checks only the
default layout, so upgrade that host by hand as
[Upgrading a host](upgrading.md) describes (the general steps). Then
it:

1. stops `skep_api` (a service that is not running is left as it is,
   here and for `skep_agent`), so no new task can be submitted, and reads the
   agent's tasks again. While any are pending or running it waits, up to
   `-wait` (default `60s`; `-wait 0` does not wait). If some remain and
   there is no `-force`, it **refuses** (exit 3): it starts `skep_api`
   again, checks its pid file and that it runs the installed file, and
   stops there;
2. backs up both databases with `skep-agent db backup` and `skep-api db
   backup` into `/var/db/skep/backup/<running build>-<timestamp>/`
   (keeping the newest 3);
3. stops `skep_agent`, starts it (its schema migrations run), starts
   `skep_api`, and checks each running process is the installed file at
   the new build. **From the agent's stop on, Ctrl-C does not interrupt
   the upgrade**: stopping half way would leave the host without an
   agent. The commands run in their own process group, so the terminal's
   signal does not reach them either; every wait is bounded;
4. polls the agent's socket, up to 200 s, until it answers and reports the
   new build in `Skep-Agent-Version`, then reports the agent's health. The
   agent serves its socket only after its start-up work, which includes
   completing any jail stop the restart interrupted (up to about 190 s per
   jail with the default timeouts; ADR 0010 amendment aa). After 30 s
   without an answer upgrade prints one line saying so (look for
   `jail_stop_completing` in `/var/log/daemon.log`).

**Exit 4, incomplete.** Exit 4 means changes were made and the result
could not be verified. Upgrade exits 4 when the agent was stopped and did
not come back as the new build (its start or its check failed; upgrade
starts `skep_api` again first), when `skep_api` did not come up after the
agent's restart, when the agent did not answer on its socket within 200 s,
or when its health (or reading it) was not `ok`. The agent failing to
stop is exit 4 too, because the stop may have taken effect. The message
starts with
`incomplete:`, names the backup directory, and says what to check: the
API's or the agent's log (`/var/log/daemon.log`; `jail_stop_completing`
means the agent is still completing interrupted jail stops and will
answer when they end), `service skep_agent status`, `skep-setup check`,
and, for a degraded health, the kernel modules not loaded yet. For an API
that did not come up, start it with `service skep_api start`. Except when
the agent did not come back (below), running
`skep-setup upgrade` again is safe, since both programs already run the new
build; it backs up and restarts both services again, but does not fix the
cause, and while the agent is still completing interrupted jail stops a
second restart only delays them (each stop is completed at the agent's
next start).

Every refusal or failure after `skep_api` was stopped (the second look
at the tasks, the wait, a backup, the agent's stop, start or check)
starts `skep_api` again and checks it. The `skep_api` that comes back is
already the **new** build (pkg replaced the file), while `skep-agent` is
still the old one if the failure came before its restart. Until a later
`skep-setup upgrade` completes, the API then answers `503
agent_version_mismatch` to everything that needs the agent, and its
health is `unavailable`. The tasks keep running on the agent; run
`skep-setup upgrade` again once they have finished.

If the **agent** did not come back as the new build, or its stop failed
(exit 4 in both cases), upgrade starts the API and says so; running `skep-setup upgrade` again repeats the same
start, so it helps only once the cause (in `/var/log/daemon.log`) is
fixed. If only the final `skep_api` start fails, the agent is upgraded:
start the API by hand (`service skep_api start`); upgrade need not run
again.
Andy accepted this for 0.1 (2026-11-22, seen on both lab VMs); an agent
drain mode that avoids it is in the [backlog](../backlog.md).

Running guests are not touched: VM runners and jails survive the agent
restart. If only one of the two services was restarted, the API answers
`503 agent_version_mismatch` until both run the same build.

### Database backups by hand

```sh
skep-agent db backup          # -> /var/db/skep/backup/<version>+<commit>-<timestamp>/agent.db
skep-api db backup            # -> .../api.db
```

Each writes a consistent copy (SQLite `VACUUM INTO`, read-only on the
live database, so the service can keep running), its `.sha256`, and a
listing of the audit archive (names, sizes, SHA-256; the sealed segments
are not copied; each is opened without following a symbolic link and
must be a regular file). The `.skep-backup` marker is written last, so
only a finished backup is counted and pruned; one that failed half way
is left for you. Manual and upgrade backups share one label format,
`<version>+<commit>`. `-dir` names the directory, `-keep N` how many backups
to keep. A downgrade is: install the old package, restore both copies
with the services stopped, start them.

## Removing Skep

```sh
skep-setup remove -plan       # read-only
skep-setup remove             # then: pkg delete skep
```

`remove` refuses while any Skep guest exists, running or stopped (or
use `-keep-guests`: nothing is stopped and nothing will manage them). It
counts them in the database the agent's configuration names; a missing
table is an error, and a missing database while the agent answers on its
socket is refused.
It stops `skep_api` and `skep_agent` and sets both `_enable` variables
to `NO`. By default it keeps everything else (answer 11):

- `-revert-host` also removes the lines setup's applies added, where
  they are unchanged since ([ADR 0012
  amendment](../decisions/0012-installer.md#amendment-2026-11-22-build-decisions-confirmed),
  build decision 14):
  - a variable setup set is removed (`sysrc -x`) only while it still has
    setup's value; one changed since is reported and left alone;
  - from `kld_list` and `cloned_interfaces` only the words setup added are
    removed (`sysrc NAME-="..."`). Apply records only the words that were
    missing, so a module you had listed before apply (`if_epair` or
    `if_epair.ko`) stays, as does anything you added since that setup did
    not add. Rolling back the apply or the remove keeps them too;
  - a list that apply created (absent before it, per the apply's backup)
    and that holds nothing but setup's words is deleted (`sysrc -x`):
    FreeBSD's `sysrc NAME-="..."` leaves `NAME=""` behind, so this is what
    returns `rc.conf` to its bytes before apply;
  - `zfs_enable` is **kept** while any pool is imported (without it the
    pools are not imported at boot); the plan says so;
  - words are removed one per `sysrc NAME-="WORD"` call;
  - rolled-back applies are ignored; in a file that is not plain
    `name=value` lines, nothing is edited and the `sysrc` steps are printed
    for you.

  Modules stay loaded and interfaces exist until the next boot.
- `-purge-data` (always asks, even with `-yes`) deletes `/var/db/skep`
  except setup's journals (node identity, databases, audit logs and
  archives, backups, the TLS key, install media), `agent.json` and
  `api.json` (copies stay in the journal), and the `skep` account. It is
  **refused while any Skep guest is running**, with `-keep-guests` too.
  Skep keeps no ZFS encryption key ([ADR 0010](../decisions/0010-storage-and-jails.md):
  the operator holds them); remove looks up the encryption roots of the
  datasets under each pool's `skep` dataset, and if one's `keylocation`
  is a file under `/var/db/skep`, `-purge-data` is refused (without it,
  a warning). When that check cannot run (`zfs list` or `zfs get` failing
  for a pool that is imported), `-purge-data` is refused too, not warned
  or skipped; without it, the plan warns that it is unknown. A pool that
  is not imported is reported as not checked; a pool whose `skep` dataset
  does not exist has nothing to check. The pools are read as the agent
  reads them (`agent.json`, or `skep_agent_config`); without that file the
  agent has no pools. One that cannot be read or parsed stops `remove`
  before any plan, with or without `-purge-data` (the guest count reads it
  first).

It never destroys a dataset; it lists the ones under each pool's
`skep` dataset for you.

**The guard against removing Skep under running guests is `skep-setup
remove`**, which refuses while Skep guests exist; run it before `pkg
delete`. `pkg delete` itself does not refuse: pkg 2.9.2 (measured on both
lab VMs, 2026-11-22) stops on a failing pre-deinstall script only with
`DEVELOPER_MODE` on, and by default prints "DEINSTALL script failed" and
deletes the files anyway (exit 0). The package's pre-deinstall script
therefore prints a **warning** while a Skep guest is running (jails named
`skep-*` in `jls`, VMs named `skep-*` in `/dev/vmm`, also when
`skep_agent` is stopped), saying that pkg deletes the files anyway and
naming `skep-setup remove`; it keeps exit 1 for a pkg that does honour
it. It is silent during `pkg upgrade`. `pkg lock` was considered and
rejected: it would also block `pkg upgrade`. ([ADR 0012 amendment, the
pre-deinstall guard](../decisions/0012-installer.md#amendment-2026-11-22-fix-round-the-pre-deinstall-guard-and-the-review-fixes).)
A failing pre-install, by contrast, does stop `pkg add`.
