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
files). `check` and `plan` were run read-only on `hearst`
([output](../../tests/integration/setup/results/hearst-check-plan-20261122.txt)).
`apply`, `rollback`, `upgrade` and `remove` are **pending FreeBSD
validation** on the two lab VMs ADR 0012 asks for; they have never run
against a real host's files.

```
skep-setup check     [answers]   read-only: what is configured, missing, in conflict, refused
skep-setup plan      [answers]   read-only: the exact changes apply would make
skep-setup apply     [answers]   make them (asks first, or -yes)
skep-setup rollback  [ID]        undo one apply or remove where files are unchanged since
skep-setup upgrade               after "pkg upgrade skep"
skep-setup remove    [-plan]     before "pkg delete skep"
```

Run it as root: it reads `agent.json` (`0600`). Exit status: `0` nothing
to do, `1` (`check`) something for `apply` or for you, `2` usage, `3`
refused.

## The answers

| Flag | Meaning |
|---|---|
| `-mgmt IF[,IF]` | **Required.** The management interface(s). Never changed. The first one's current address is the default listen address. Guests are refused on any bridge that contains one |
| `-bridge NAME` | An existing guest bridge to allow-list. Setup changes nothing on it |
| `-create-bridge NAME -uplink IF` | Create a new guest bridge on a spare interface with no address and no other settings, `vlanfilter` on |
| `-pool NAME` | A ZFS pool to allow-list; it must be imported and `ONLINE` |
| `-dir-pool NAME=/PATH` | A directory pool for VM disks; the directory must exist, root-owned, not writable by others |
| `-listen ADDR:PORT` | The API listen address (default: the management address, port 8443). `0.0.0.0` or `::` only with `-listen-all` |
| `-no-racct` | Do not add `kern.racct.enable="1"` (jail limits other than `disk_quota` are then refused) |
| `-start` | Start both services at the end of `apply` |
| `-admin NAME` | Create the first administrator (asks for the password, twice, without echo). Needs `-start`, or `/var/db/skep/api` to exist already (the API has run once); otherwise the plan refuses, because `skep-api admin create` run as root first would create the API's data directory owned by root ([ADR 0012 amendment](../decisions/0012-installer.md#amendment-2026-11-22-build-decisions-confirmed), build decision 8) |

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
with its mode and owner. Every result is read back and checked. A variable already set to another value
is a **conflict**: reported, never overwritten. `agent.json` and
`api.json` are created, never rewritten; if one exists and setup did not
write it, `apply` refuses and prints its proposed version next to it.

## What it refuses

`apply` does nothing at all while any of these stands:

- FreeBSD other than 15.x, an architecture other than amd64, or a kernel
  and userland at different releases (15.x other than 15.1 is a warning);
- no `rc.conf`;
- a management interface that does not exist, has no `ifconfig_` line,
  or has one with neither an address nor DHCP (**DHCP is accepted with a
  warning**: the listen address is the current lease's);
- a bridge to allow-list that has a management interface as a member, or
  does not exist;
- a new bridge whose uplink is a management interface, does not exist,
  has an address (now or in `rc.conf`), has other settings or aliases, or
  is in another bridge; or a new bridge name that exists already;
- only one of the `skep` user and group, a `skep` user with a login shell
  or a home, no `skep` account at all (the package creates it), or UID or
  GID 409 held by another name;
- an `agent.json` or `api.json` it did not write;
- a pool that is not imported or not `ONLINE`; a directory pool writable
  by others;
- `0.0.0.0` without `-listen-all`; no address to listen on;
- an earlier run that did not finish (roll it back first).

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
(`service NAME status`) is `ok`; with `-admin`, a username an earlier run
created is `ok` (also after that run was rolled back, since rollback
never deletes users).

`skep-setup rollback [ID]` (default: the newest run) puts a file back
only if it is still byte-identical to what the run left; a file changed
since is reported and left alone. Files and directories setup created are
removed. It does not stop or start services, delete the administrator,
unload modules or destroy interfaces. If an `apply` fails part-way, it
stops, and the next `plan` refuses until that run is rolled back.

## Upgrades

```sh
pkg upgrade skep
skep-setup upgrade
```

`upgrade` refuses, changing nothing, unless the installed `skep-agent`,
`skep-api` and `skep-setup` are one build, and while any agent task is
**pending or running**. A task interrupted by the agent restart is never
resumed or re-run: it ends `interrupted` and must be resubmitted, so only
`-force` upgrades over unfinished tasks. Then it:

1. stops `skep_api`, so no new task can be submitted, and reads the
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
   the new build;
4. reports the agent's health.

Every refusal or failure after `skep_api` was stopped (the second look
at the tasks, the wait, a backup, the agent's stop, start or check)
starts `skep_api` again and checks it. The `skep_api` that comes back is
already the **new** build (pkg replaced the file), while `skep-agent` is
still the old one if the failure came before its restart. Until a later
`skep-setup upgrade` completes, the API then answers `503
agent_version_mismatch` to everything that needs the agent, and its
health is `unavailable`. The tasks keep running on the agent; run
`skep-setup upgrade` again once they have finished.

Running guests are not touched: VM runners and jails survive the agent
restart. If only one of the two services was restarted, the API answers
`503 agent_version_mismatch` until both run the same build.

### Database backups by hand

```sh
skep-agent db backup          # -> /var/db/skep/backup/<version>-<commit>-<timestamp>/agent.db
skep-api db backup            # -> .../api.db
```

Each writes a consistent copy (SQLite `VACUUM INTO`, read-only on the
live database, so the service can keep running), its `.sha256`, and a
listing of the audit archive (names, sizes, SHA-256; the sealed segments
are not copied). `-dir` names the directory, `-keep N` how many backups
to keep. A downgrade is: install the old package, restore both copies
with the services stopped, start them.

## Removing Skep

```sh
skep-setup remove -plan       # read-only
skep-setup remove             # then: pkg delete skep
```

`remove` refuses while any Skep guest exists, running or stopped (or
use `-keep-guests`: nothing is stopped and nothing will manage them).
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
  - rolled-back applies are ignored; in a file that is not plain
    `name=value` lines, nothing is edited and the `sysrc` steps are printed
    for you.

  Modules stay loaded and interfaces exist until the next boot.
- `-purge-data` (always asks, even with `-yes`) deletes `/var/db/skep`
  except setup's journals (node identity, databases, audit logs and
  archives, backups, the TLS key, install media), `agent.json` and
  `api.json` (copies stay in the journal), and the `skep` account.

It never destroys a dataset; it lists the ones under each pool's
`skep` dataset for you.

`pkg delete skep` itself refuses while a Skep guest is running: the
package's pre-deinstall script looks for jails named `skep-*` (`jls`) and
VMs named `skep-*` (`/dev/vmm`) on the host, so it refuses **even when
`skep_agent` is stopped**, as it is after `skep-setup remove`. It never
refuses during `pkg upgrade`; `pkg delete -f` overrides it ([ADR 0012
amendment](../decisions/0012-installer.md#amendment-2026-11-22-build-decisions-confirmed),
build decision 5).
