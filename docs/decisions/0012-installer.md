# ADR 0012: Installing, upgrading, and removing Skep (Milestone 10)

- Status: **Accepted** (2026-11-22). Andy's answers to
  [the decisions](#decisions-for-andy) are recorded in
  [Answers](#answers-2026-11-22) and folded into the decisions below.
  [Amendment (2026-11-22)](#amendment-2026-11-22-build-decisions-confirmed)
  records three build decisions Andy confirmed;
  [the fix-round amendment](#amendment-2026-11-22-fix-round-the-pre-deinstall-guard-and-the-review-fixes)
  records the pre-deinstall guard for 0.1 and the review fixes Andy
  approved. [The release-asset amendment (2026-11-24)](#amendment-2026-11-24-no-release-asset-for-01),
  accepted by Andy, defers Decision 8's signed downloads: 0.1 has no
  release asset.
- Date: 2026-11-22
- Depends on: [ADR 0003](0003-read-only-discovery.md) (enabling a
  capability is an administrative act), [ADR 0004](0004-privilege-boundary.md)
  and [ADR 0006](0006-api-split-authentication-tls.md) (the service
  account, the first administrator, the listen address),
  [ADR 0009](0009-first-vm-bhyve.md) (Decision 6b: Skep loads no
  modules), [ADR 0010](0010-storage-and-jails.md) (directory pools for the
  UFS profile, `if_epair` in `kld_list`), [ADR 0011](0011-guest-port-anti-spoofing.md)
  (the netgraph modules in `kld_list`, `vlanfilter` by default).
- Evidence: [host-requirements.md](../installation/host-requirements.md),
  the complete manual procedure and the inventory this ADR automates; the
  throwaway probe on branch `probe/m10-installer` (not for merge; results
  in `probe/m10-installer/results/` on that branch).

## Context

Today a Skep host is set up by hand in 13 ordered steps
([host-requirements.md](../installation/host-requirements.md)): boot
settings, `kld_list`, ZFS, a guest bridge with VLAN filtering, the service
account, binaries and rc.d scripts, configuration files, services, the
template package source, TLS and the first administrator, media, and a
check. Each step is documented; none is automated, and the only
"installer" is `scripts/deploy-test.sh`, a lab tool.

Earlier ADRs left specific jobs to Milestone 10:

| From | Promise |
|---|---|
| ADR 0006 Decision 7 | The installer asks for the management address and writes it |
| ADR 0010 Decision 6, amendment h | A UFS-root profile, using directory pools; `if_epair` written into `kld_list` |
| ADR 0011 Decisions 1 and 6 | `netgraph ng_socket ng_ether ng_bpf` written into `kld_list`; `vlanfilter` on by default |
| freebsd-service.md | A fixed UID for the `skep` account is a packaging decision |

The release this sizes for is **0.1, a single-host preview**: one
FreeBSD 15.1 amd64 host, an administrator who installed FreeBSD and is
comfortable with `rc.conf`, no cluster, no HA. What matters most at
this size is that installing Skep never damages a host the
administrator already uses, that an upgrade does not touch running
guests, and that every step can be undone.

### Constraints carried over

- **Skep loads no kernel modules and never reboots** (ADR 0009 6b). A
  setup tool may write `loader.conf` and `kld_list` for the next boot,
  but loading modules now or rebooting stays the administrator's act.
- **Never a change that can cut management connectivity** without a
  rollback path (CLAUDE.md, networking). A bridge change on the wrong
  interface takes the host off the network.
- **No default credentials**, no network path to become administrator
  (ADR 0006 Decision 8).
- **No shell from untrusted input.** The tool runs as root and reads the
  host's own files; it must parse them, never source them.

## The options

### (a) A FreeBSD package plus `skep-setup`, on a stock FreeBSD install

The administrator installs FreeBSD 15.1 with the stock `bsdinstall`
(their choice of UFS or ZFS root, partitioning, network), then
`pkg install skep` (or `pkg add skep-0.1.0.pkg`), then runs
`skep-setup`.

- **The package** installs files and nothing else: the three programs,
  the rc.d scripts, the sample configurations, the license. Its only
  script creates the `skep` group and user when absent (as ports do). It
  does not edit `rc.conf`, does not enable or start services, and does
  not print instructions that hide a change. That keeps `pkg install -y`
  and `pkg upgrade` safe in automation.
- **`skep-setup`** (one Go binary in the package) does the host
  configuration, in three verbs:
  - `skep-setup check`: read-only. What is installed, what is missing,
    what conflicts, what it would refuse. Exit status for scripts.
  - `skep-setup plan [flags]`: read-only. The exact changes `apply` would
    make, line by line, and what it will leave to the administrator.
  - `skep-setup apply [flags]`: makes the planned changes, after showing
    the plan and asking (or `--yes`). Backs up every file before changing
    it, writes a journal, and never does anything not in the plan.

**What it automates:** loader.conf lines (`vmm_load`, `nmdm_load`, and
optionally `kern.racct.enable`), `kld_list` (append only), `zfs_enable`,
a **new** guest bridge on a spare NIC (with `vlanfilter` on), the
configuration files (`agent.json` with the allow-lists and management
interfaces, the listen address), the media directory, enabling and
starting the services (by `service`, not by rewriting rc.d), and
running `skep-api admin create` interactively. Read-only checks of
everything else: VT-x/EPT, `edk2-bhyve`, the pool's state, the
`FreeBSD-base` repository, the patch level.

**What stays manual:** installing FreeBSD and partitioning disks;
creating, importing, or destroying pools; any change to an existing
bridge or interface (including `vlanfilter` on an existing bridge,
which setup prints instead); loading modules now; RACCT's reboot; the
nested-virtualization tunable; the outer hypervisor's MAC filter; firewall
rules.

The probe shows both halves are feasible: `pkgproto` laid out the
package and wrote a `.pkg` from the `make build-freebsd-go` output on
Linux, self-checked; `setupplan` read a stock UFS, a stock ZFS, and
hearst's `rc.conf`/`loader.conf` and produced the plan, including every
refusal below, with no command run and no file opened for writing.

### (b) A custom install image built on `bsdinstall`

A Skep ISO/USB image: FreeBSD 15.1 plus Skep, with a scripted
`bsdinstall` (`installerconfig`) and a few extra dialogs (management
interface and address, guest uplink, pool layout, administrator).

**What it automates:** everything in (a), plus disk partitioning, the root
file system and pool layout (the ZFS and UFS profiles), the network
configuration, and the first boot. A host is ready at the first login.

**What stays manual:** BIOS/UEFI settings (VT-x), choosing disks,
anything after installation that (a)'s setup would also leave manual.
Upgrades still need a package (or a new image, which cannot upgrade a
host in place).

**Costs:** a FreeBSD build host and `release(7)` to make images; a new
image for every FreeBSD patch release that matters (security advisories
and errata), or an installer that runs `freebsd-update`/`pkg upgrade`
first; signed images; disk partitioning code, the step most likely to
destroy data; it does nothing for hosts that are already installed, which
is every host a 0.1 tester has.

### (c) First (a), then (b)

(a) for 0.1 and early releases; (b) later as a convenience that runs
`bsdinstall`, installs the same package, and runs the same
`skep-setup apply` with answers from its dialogs. Nothing is built twice:
(b) wraps (a).

## Comparison

| | (a) package + `skep-setup` | (b) custom image | (c) a, then b |
|---|---|---|---|
| Automates | Host configuration and services on an installed FreeBSD | Also partitioning, pools, network, first boot | (a) now, (b) later |
| Stays manual | FreeBSD install, pools, existing network | BIOS, disk choice | As (a), then as (b) |
| Testing without bare metal | Official 15.1 **VM images** (UFS and ZFS qcow2) on Proxmox, reverted by snapshot; unit tests on fixture roots (as the probe); optionally a FreeBSD VM in CI for `pkg add` and `skep-setup check` (no bhyve) | Boots the ISO in a Proxmox VM; partitioning needs several virtual disk layouts; real hardware (NICs, NVMe, UEFI quirks) only on bare metal | As (a), then as (b) |
| Artifacts and signing | One `.pkg` per release; a signed `pkg` repository (pkg verifies natively) and/or `SHA256SUMS` signed with `ssh-keygen -Y sign` (verifiable with base tools) | An image per release and per FreeBSD patch, signed and checksummed; also contains FreeBSD's own artifacts | As (a), then (b) |
| ZFS-root vs UFS-root | Inherited from the admin's install; setup detects it and offers ZFS pools or a directory pool | The installer's profiles choose and build it | — |
| Idempotency, re-run | Each action is "ensure": checked, done only if missing; a re-run with nothing to do changes nothing | A one-shot install; re-running means reinstalling | — |
| Upgrade | `pkg upgrade skep`, then `skep-setup upgrade` (back up databases, ordered restart) | Needs (a)'s package path anyway | (a) |
| Rollback | Setup's journal and backups; old package + restored databases; a boot environment on ZFS root | Reinstall | (a) |
| Uninstall | `skep-setup remove --plan`, then `pkg delete skep`; data kept unless asked | No uninstall; reinstall FreeBSD | (a) |
| Work for 0.1 | A package build, one Go tool, tests, docs | All of (a) plus image build infrastructure, partitioning, dialogs | (a) |
| Risk to an existing host | Low: refuses what it cannot change safely | N/A (a new host only) | (a) |

## Decision: (c), committing to (a) only for 0.1

Accepted as recommended (answer 1).

Build (a) now. Record (b) as the intended later step, without building
or designing it beyond confirming that (a) leaves room for it (a
non-interactive `skep-setup apply` with answers from a file is enough).
Reasons:

1. Every 0.1 tester already has, or will install, a stock FreeBSD; (b)
   helps nobody who already has a host, and (a) helps everyone.
2. (a)'s risk can be bounded by refusals; (b)'s biggest piece,
   partitioning, is the part most able to destroy data and the hardest
   to test without bare metal.
3. Upgrades and removal need a package in every option. (a) builds the
   part that is needed regardless.
4. It respects the boundaries the earlier ADRs drew: Skep never loads
   modules, never reboots, never creates pools, never changes an
   interface it did not create.

The rest of this ADR specifies (a).

## Decision 1: the package

- **Name** `skep`, origin `sysutils/skep`, ABI `FreeBSD:15:amd64`, prefix
  `/usr/local`, license `BSD2CLAUSE` (the license PR).
- **Contents:** `sbin/skep-agent`, `sbin/skep-api`, `sbin/skep-setup`,
  `bin/skepctl` (`0555 root:wheel`); `etc/rc.d/skep_agent`, `skep_api`
  (`0555`); `etc/skep/agent.json.sample`, `api.json.sample` (`0644`; no
  secrets); `share/licenses/skep/` with `LICENSE` and
  `THIRD_PARTY_NOTICES` (the file `make notices` generates and CI checks);
  `share/doc/skep/` (optional).
- **Never** contains `agent.json` or `api.json` themselves, so an upgrade
  cannot overwrite the administrator's configuration. `skep-setup`
  writes them from its answers on first setup.
- **Dependencies:** none required. `edk2-bhyve` is checked by
  `skep-setup check` and offered (`pkg install edk2-bhyve` printed), not
  a hard dependency: a jails-only host does not need it.
- **Scripts:** pre-install creates the `skep` group and user, with the
  fixed UID and GID below, if absent. No post-install changes.
  Pre-deinstall refuses (exit non-zero) while `skep_agent` runs and Skep
  guests exist, so `pkg delete` cannot orphan running guests; `-f`
  overrides as usual for pkg. **It must not refuse during an upgrade:**
  `pkg upgrade` runs the old package's deinstall scripts before installing
  the new one, so the refusal is skipped when `PKG_UPGRADE` is set in the
  script's environment (answer 14). pkg's exact behaviour (that the
  variable is set, and its value) is to be verified on a lab VM during the
  build, and there are tests for both cases: an upgrade with guests
  running succeeds without touching them, and a `pkg delete` with guests
  is refused.
- **The `skep` account: UID 409, GID 409** (answer 5). Fixed now because
  clustering (Milestone 8) wants the same UID on every node: files that
  move between nodes (replicated datasets, backups) keep their owner. 409
  was the lowest number free in both the ports `UIDs` and `GIDs` registries
  (`freebsd-ports` main, read 2026-11-22; both files list it as
  `# free: 409`), and is outside base's range. It is **not reserved**
  until a port is submitted, so the number is re-checked against the
  registry before each release and registered with the port. Setup
  refuses when UID 409 or GID 409 belongs to another name, and warns
  (does not change anything) when a `skep` account exists with another
  number, as on hosts set up by hand before this ADR (`hearst`: 1002).
- **Building:** the release is staged from `make build-freebsd` and packed
  with `pkg create -M +MANIFEST -r stage`, run by hand on a lab FreeBSD VM
  for each release (answer 2). The probe's Go `.pkg` writer is dropped.
- **ABI from the manifest.** Go stamps FreeBSD binaries with a FreeBSD
  12.3 ELF note (seen in the probe); the package ABI must be set in the
  manifest, not detected from the binaries.

## Decision 2: what `skep-setup` may change, and what it refuses

**It only adds.** For files it does not own (`/boot/loader.conf`,
`/etc/rc.conf`), it adds a missing line or appends to a list. It never
rewrites, reorders, or removes an existing line, and an existing line with
another value is a **conflict**, reported and left alone. Changes go
through `sysrc` (and `sysrc -f /boot/loader.conf`), which edits only the
named variable, never through text substitution.

**It refuses to edit a file it cannot parse.** `rc.conf` is shell. Any
line that is not a plain `name=value` assignment (a command, a
substitution, an `if`, a sourced file), and any `rc.conf.local` or
`rc.conf.d` entry that sets a variable it would add, makes setup edit
nothing in that file and print the steps instead (probe: tested).

**It refuses outright** (exit status 3, nothing done):

| Situation | Why |
|---|---|
| The named management interface has no `ifconfig_` line, or one with neither an address nor `DHCP`/`SYNCDHCP` | It will not guess which interface carries management |
| A bridge to be allow-listed has the management interface as a member | Skep would refuse guests on it anyway; guests next to management is the one network layout Skep forbids |
| The uplink for a new bridge has an address, other settings, is the management interface, or is in another bridge | It is in use |
| Only one of the `skep` user and group exists, or the user has a login shell or a home | Something else made it |
| The release is not FreeBSD 15.x amd64 | Not a supported platform |
| `agent.json` or `api.json` exists and was not written by this tool | The administrator's file; setup prints its proposed version beside it |
| Asked to apply while a previous apply's journal is incomplete | Finish or roll back first |

**It warns, and continues,** when the management interface is configured
by DHCP (`ifconfig_IF="DHCP"` or `"SYNCDHCP"`): the listen address it
writes is the current lease's, and changes if the lease does. The warning
says so and suggests a static address or a DHCP reservation (answer 6).

**It never touches**, on any host:

- any pool (create, import, export, destroy, properties) or dataset outside
  `<pool>/<dataset_root>`, which the agent itself creates on use;
- an existing bridge's members or flags, including `vlanfilter`: setup
  prints the safe order (uplink PVID, `defuntagged`, then `vlanfilter`)
  for the administrator to run from the console;
- any `ifconfig_*` line other than the new bridge and its new uplink, and
  never `defaultrouter`, `ifconfig_*_alias*`, routes, or IPv6 settings;
- `pf`, `ipfw`, `/etc/devfs.rules`, `/etc/login.conf`, `/etc/sysctl.conf`,
  or the boot loader beyond the lines in Decision 3;
- running guests, jails not named `skep-*`, and any bhyve VM it did not
  start; it does not stop or restart anything during `apply` except
  Skep's own services, and only when told to.

**On a host that already runs guests** (Skep's or anyone's), `apply` adds
boot-time lines only and never runs `ifconfig` or `kldload`. It reports
non-Skep jails and bhyve processes so the administrator knows they share
the host (and, if they use Skep's bridge, that ADR 0011 says to filter
them separately).

## Decision 3: the changes, exactly

| Item | Change | When it takes effect |
|---|---|---|
| `/boot/loader.conf` | `vmm_load="YES"`, `nmdm_load="YES"` if missing | Next boot |
| `/boot/loader.conf` | `kern.racct.enable="1"` unless `--no-racct` (Skep's jail limits use RCTL, which needs RACCT); the plan says it takes a reboot | Next boot |
| `/etc/rc.conf` | `kld_list+=` the missing ones of `if_epair netgraph ng_socket ng_ether ng_bpf` | Next boot; setup prints the `kldload` line |
| `/etc/rc.conf` | `zfs_enable="YES"` when a ZFS pool is named and it is missing | Next boot |
| `/etc/rc.conf` | A **new** bridge, only with `--create-bridge NAME --uplink IF`: `cloned_interfaces+=`, `ifconfig_IF="up"`, `ifconfig_NAME="addm IF ifuntagged IF 1 defuntagged 1 vlanfilter up"` | Next boot; or setup prints the `ifconfig` lines in order |
| `/etc/rc.conf` | `skep_agent_enable`, `skep_api_enable` | `service ... start` with `--start` |
| `/usr/local/etc/skep/agent.json` | Written once from the answers (pools, directory pools, bridges, management interfaces); `0600 root:wheel` | Agent start |
| `/usr/local/etc/skep/api.json` | Written once with `listen_address`; `0640 root:skep` | API start |
| `/var/db/skep/media` | Created `0700 root:wheel` | — |
| First administrator | `skep-api admin create` run interactively (or `--password-stdin`); never a password in a flag, file, or the journal | — |

Answer 6 confirmed this table: a new bridge only when explicitly asked,
on a spare unaddressed NIC; an existing bridge is never changed.
Answer 7: `vlanfilter` is on for bridges setup creates, and printed (not
applied) for existing ones.

The listen address defaults to the management interface's address, port
8443, shown in the plan (with the DHCP warning above when it applies); `0.0.0.0` is refused unless `--listen-all` is
given (ADR 0006 Decision 7).

## Decision 4: idempotency, the journal, and rollback

Answer 12: for 0.1 setup records what it did in its own journal only, not
in the agent's audit log.

- Every action is "ensure X": it checks, and acts only if X is not
  already true. A second `apply` with the same answers plans nothing and
  changes nothing (to be tested on the fixtures and on a VM).
- Before a file is changed, setup copies it to
  `/var/db/skep/setup/<UTC timestamp>/` (`0700 root:wheel`) with its
  SHA-256, and records each action in `journal.json` there: before and
  after hashes, the exact variable and value added.
- `skep-setup rollback [timestamp]` restores the files of one apply
  **only if each is still byte-identical to what setup left** (its
  "after" hash); a file the administrator changed since is reported, not
  overwritten. It does not unload modules, delete the bridge interface
  from the running system, or remove the account.
- On ZFS root, `skep-setup apply` and `upgrade` offer
  `bectl create skep-pre-<version>-<timestamp>` first (`--be`). The
  default ZFS layout keeps `/var/db` inside the boot environment, so
  booting it rolls back Skep's databases too; that is said in the plan,
  because it also discards audit entries written since.

## Decision 5: upgrades

**Binaries.** `pkg upgrade skep` replaces files only. A replaced binary
does not change a running process, and the package does not restart
services (Decision 1).

**`skep-setup upgrade`** does the rest, in order:

1. Checks the new binaries' versions agree (`skep-agent -version`,
   `skep-api -version`, `skep-setup` itself) and that the release is
   one this version supports.
2. Backs up both databases with new `skep-agent db backup` and
   `skep-api db backup` commands (SQLite's online backup), to
   `/var/db/skep/backup/<old version>-<timestamp>/`, keeping the last 3
   backups. The audit archives are listed (file names, sizes, SHA-256),
   not copied (answer 10).
3. Stops `skep_api`, stops `skep_agent`, starts `skep_agent` (which runs
   the agent's schema migrations), starts `skep_api` (the API's
   migrations), and verifies each running process is the installed file
   and the new version, as `scripts/deploy-test.sh` does today.
4. Reports `GET /api/v1/health` from the local socket.

**Schema migrations** are forward-only and run at start. An older binary
refuses a newer schema ("refusing to run against a newer schema",
`internal/database/migrate.go`). So a downgrade is: install the old
package, restore the step 2 backup, start. Without the backup there is
no downgrade.

**Configuration.** New keys get defaults, so an old `agent.json` keeps
working. A release that renames or removes a key must say so in its
upgrade notes, and the agent's validation must name the key; setup does
not rewrite configuration on upgrade.

**Running guests across the agent restart:** unaffected, by design and
validated on `hearst`.

- VMs run under detached per-VM runners that survive an agent restart
  (ADR 0009 Decision 7).
- Jails, their epairs, RCTL rules, and port filters are kernel state and
  survive (ADR 0011, validated: same `ng_bpf` node IDs before and after).
- At start, the agent re-checks every running guest's port filter before
  it serves its socket and installs missing ones; this is also how an
  upgrade from a Skep without filters (b61e578) was handled
  (m7c-validation item 5).
- Tasks running when the agent stops are marked `interrupted` (ADR 0005);
  setup refuses to upgrade while tasks are `running` unless `--force` (pending ones too since the [amendment](#amendment-2026-11-22-build-decisions-confirmed)).

**Compatibility between programs.** `skep-api` and `skep-agent` come from
one package and are upgraded together; there is no version handshake
between them today. Added (answer 9, approved): the agent reports its
version on its internal API and `skep-api` refuses to serve with 503
`agent_version_mismatch` when the versions differ.

**FreeBSD itself.** A FreeBSD patch upgrade (`pkg upgrade` of base, or
`freebsd-update`) is the administrator's. A minor upgrade (15.1 to 15.2)
makes jail templates `release_unsupported` until rebuilt; setup's check
says so before and after.

## Decision 6: uninstall

`skep-setup remove --plan` lists what would be removed and what is kept;
`skep-setup remove` then:

- refuses while any Skep guest exists (running or stopped), unless
  `--keep-guests`, which stops nothing and only warns that the guests'
  datasets stay and that nothing will manage them;
- stops and disables both services;
- reverts its own lines in `rc.conf`/`loader.conf` only where they are
  unchanged since it added them (journal), and only with `--revert-host`
  (other software may rely on `vmm` or `if_epair`);
- keeps `/var/db/skep` and `/usr/local/etc/skep` unless `--purge-data`,
  which first prints that it deletes the node identity and every audit
  log, and asks;
- never destroys a dataset: it lists those under `<pool>/<dataset_root>`
  for the administrator.

Then `pkg delete skep` removes the files. The `skep` account is removed
only with `--purge-data` (files may still be owned by it otherwise).

Answer 11: these defaults stand; data, host lines, and the account are
kept unless asked.

## Decision 7: version and compatibility checks

Answer 8 confirmed: refuse anything but 15.x amd64; warn on 15.x other
than 15.1.

- The package's ABI restricts it to FreeBSD 15 amd64 (pkg enforces it).
- `skep-setup check` reports, and `apply` refuses on: not 15.x amd64,
  kernel and userland at different major.minor. It **warns** (does not
  refuse) on a minor other than the qualified one (15.1 for 0.1), and on
  hardware virtualization missing (jails still work).
- The agent's existing discovery remains the authority at run time
  (bhyve available, ZFS, modules); setup reuses its checks rather than
  duplicating them where it can (one code path for "what is missing").

## Decision 8: signing and integrity

Both, per answer 3: a signed `pkg` repository and a signed `.pkg`
download. The files go on GitHub Releases; the repository on GitHub Pages
once the repository is public.

- **Repository:** a `pkg` repository (`pkg repo` with an RSA 4096
  signing key) hosted as static files. `pkg` verifies the catalogue signature and each
  package's checksum natively; the administrator adds a
  `/usr/local/etc/pkg/repos/skep.conf` with the public key's path.
- **Direct download:** `skep-<version>.pkg` and `SHA256SUMS`, with
  `SHA256SUMS.sig` made by `ssh-keygen -Y sign` with an Ed25519 key, verifiable on stock
  FreeBSD with `ssh-keygen -Y verify` (base system, no package needed).
- **Keys** (answer 4): generated and held by Andy on his Mac, with one
  offline backup; the public keys and their fingerprints published in
  the README, the docs, and the release notes. CI never holds a private
  key; signing is a manual release step for 0.1.
- **Reproducibility:** the Go build is already `-trimpath` with fixed
  ldflags; the probe writes archives with a fixed mtime. Reproducible
  packages are a goal, not a 0.1 requirement.

## Decision 9: how it is tested

- **Unit tests** on fixture roots, as the probe does: stock UFS and ZFS
  `rc.conf`/`loader.conf` as `bsdinstall` writes them, hearst's, and
  hostile cases (shell code in `rc.conf`, a bridge holding management,
  conflicting values). Every refusal has a test.
- **No FreeBSD VM job in CI for 0.1** (answer 13): the package steps
  (`pkg add`, `skep-setup check`/`plan`, both services started, `pkg
  upgrade` with guests running, `pkg delete` with and without guests) are
  run by hand on a lab VM for each release. Revisit at 1.0.
- **Lab VMs:** two fresh Proxmox VMs from the official FreeBSD 15.1 VM
  images (UFS and ZFS root), two NICs each, VT-x exposed, with a snapshot
  to revert to after each run. On each: install, `apply` twice
  (idempotent), a VM and a jail on the new bridge, `upgrade` from the
  previous build with both guests running, `rollback`, `remove`. Not
  `hearst`, which is the integration host and is not stock.
- **Bare metal:** not needed for (a) at 0.1; hardware qualification is
  Milestone 11.

## Decisions for Andy

As they were asked; the answers are in [Answers](#answers-2026-11-22).

1. **Approach:** (a) package plus `skep-setup` for 0.1, with (b) recorded
   as later and not designed now (recommended), or another choice.
2. **Package build:** `pkg create` on a FreeBSD builder (recommended:
   a CI FreeBSD VM, or a manual release step on a lab VM), or the Go
   writer from the probe after it is validated against real `pkg`.
3. **Distribution:** a signed `pkg` repository, a signed `.pkg` download,
   or both (recommended: both; the repository makes `pkg upgrade` work).
   Where it is hosted (GitHub Releases for files; GitHub Pages or another
   static host for the repository).
4. **Signing keys:** algorithm (RSA for `pkg repo`, Ed25519 for
   `ssh-keygen -Y`), who holds them, and whether signing is a manual step
   (recommended for 0.1).
5. **The `skep` UID/GID:** keep `pw`'s choice (today), pick a fixed
   number now, or request one in the ports `UIDs`/`GIDs` registry when
   submitting a port. Recommended: `pw`'s choice for 0.1, registry before
   a port.
6. **Scope of `apply`:** confirm the table in Decision 3, in particular
   that setup **may create a new bridge** on a spare, unaddressed NIC
   when explicitly asked, and **never changes an existing bridge**.
7. **`vlanfilter`:** on by default for bridges setup creates; printed,
   not applied, for existing bridges (recommended), or applied to an
   existing bridge with a confirmation (risks cutting guests off if the
   uplink's PVID is wrong).
8. **Platform policy:** refuse on anything but 15.x amd64; warn (not
   refuse) on 15.x other than 15.1.
9. **Agent/API version check:** add a version handshake with a new error
   code (`agent_version_mismatch`, 503), or rely on one package
   installing both. This is an API contract change.
10. **Pre-upgrade database backup:** a new `skep-agent db backup` /
    `skep-api db backup` command used by `skep-setup upgrade`, kept how
    long, and whether the backup includes the audit archive or only lists
    it.
11. **Uninstall defaults:** keep data, keep host lines, keep the account
    unless asked (recommended).
12. **Where setup records what it did:** its own journal only
    (recommended for 0.1, since it runs before the agent's database
    exists), or also an entry in the agent's audit log when the agent is
    running.
13. **CI:** whether to add a FreeBSD VM job (a third-party Action) for
    package tests.
14. **Pre-deinstall refusal:** may the package's pre-deinstall script
    refuse while Skep guests exist (recommended), given `pkg delete -f`
    still overrides it?

## Answers (2026-11-22)

Andy's answers, numbered as the decisions above:

1. Approach (c): (a) package plus `skep-setup` for 0.1; (b) recorded as
   later, not designed.
2. `pkg create` run by hand on a lab VM for each release; the probe's Go
   `.pkg` writer is dropped.
3. Both a signed `pkg` repository and a signed `.pkg` download; files on
   GitHub Releases, the repository on GitHub Pages once the repository is
   public.
4. RSA 4096 for the `pkg` repository, Ed25519 for `ssh-keygen -Y`;
   generated and held by Andy on his Mac with one offline backup; manual
   signing for 0.1.
5. A fixed `skep` UID/GID now, a free number from the ports registry,
   recorded with the reason (clustering wants matching UIDs): **409/409**
   (Decision 1).
6. Decision 3's table confirmed; a new bridge only on explicit request on
   a spare unaddressed NIC; an existing bridge is never changed. Change: a
   management interface on DHCP is accepted with a warning (the listen
   address may change), not refused (Decision 2's table updated).
7. `vlanfilter` on for bridges setup creates; printed, not applied, for
   existing bridges.
8. Refuse anything but 15.x amd64; warn on 15.x other than 15.1.
9. Add the version handshake: 503 `agent_version_mismatch` (approved).
10. Add `skep-agent db backup` / `skep-api db backup`; keep the last 3;
    the audit archive is listed, not copied.
11. Uninstall keeps data, host lines, and the account unless asked.
12. Setup's own journal only for 0.1.
13. No FreeBSD VM CI job for 0.1; a manual lab-VM check per release;
    revisit at 1.0.
14. Pre-deinstall refusal: yes, but it must not refuse during an upgrade
    (pkg runs the old package's deinstall scripts on upgrade): skipped when
    `PKG_UPGRADE` is set. pkg's exact behaviour is verified on a lab VM
    during the build, with tests for upgrade-with-guests and
    delete-with-guests.

Also decided:

- **RACCT on by default:** setup adds `kern.racct.enable="1"` unless
  `--no-racct`, and the plan says it needs a reboot, because Skep's jail
  limits use RCTL (Decision 3).
- **Third-party notices:** the package installs `THIRD_PARTY_NOTICES`
  beside the license in `share/licenses/skep/` (Decision 1).

## Amendment (2026-11-22): build decisions confirmed

The build (PR #52) took three decisions on its own, numbered 5, 8 and 14
in that run's report. Andy confirmed all three on 2026-11-22. They refine
Decisions 1, 3 and 6; nothing above them changes.

- **Build decision 5: the pre-deinstall refusal does not depend on the
  agent.** The package's `pre-deinstall` script refuses (exit 1) while a
  Skep guest runs, looking at the host itself (jails named `skep-*` in
  `jls`, VMs named `skep-*` in `/dev/vmm`), **even when `skep_agent` is
  stopped**. A stopped agent is the usual state before `pkg delete`
  (`skep-setup remove` stops it), and it is when a running guest would be
  left with nothing to manage it. As before, it never refuses when
  `PKG_UPGRADE` is set, and `pkg delete -f` overrides it (Decision 1,
  answer 14).
- **Build decision 8: `--admin` needs `--start` or an existing
  `/var/db/skep/api`.** `skep-api admin create` opens `api.db`. Run as
  root before `skep_api` has ever started, it would create the API's data
  directory owned by root and lock the unprivileged API out of it. So the
  plan refuses `--admin NAME` unless `--start` is also given (the
  administrator is then created after both services start) or
  `/var/db/skep/api` already exists (Decision 3).
- **Build decision 14: what `--revert-host` reverts.** `skep-setup remove
  --revert-host` reverts only what an apply's journal records as added and
  what is unchanged since (Decision 6):
  - a variable setup set with `=` is removed (`sysrc -x`) only while it
    still has the value setup gave it; a changed one is reported as a
    conflict and left alone;
  - for a list setup appended to with `+=` (`kld_list`,
    `cloned_interfaces`), only the words that apply added are removed
    (`sysrc NAME-="..."`). Apply journals only the words that were
    missing, so a word the operator had before apply (as `NAME` or
    `NAME.ko` in `kld_list`), and any word added after it that setup did
    not add, stays. Rolling back the apply, or the remove, restores the
    file as it was and keeps those words too;
  - applies that were rolled back are ignored;
  - when `rc.conf` or `loader.conf` (or an override file) holds lines that
    are not plain assignments, setup edits nothing in it and prints the
    `sysrc` steps instead;
  - modules stay loaded and interfaces exist until the next boot.

  Covered by `TestRemoveRevertHost`, `TestRemoveRevertLeavesChangedLines`
  and `TestRevertHostKeepsTheOperatorsKldListWords`
  (`internal/setup/remove_test.go`). Real `sysrc -=` and `-x` semantics
  are pending FreeBSD validation on the lab VMs.
- **Upgrade and unfinished tasks (approved 2026-11-22):** `skep-setup
  upgrade` refuses while tasks are pending or running, not only running;
  it stops `skep_api`, re-reads the tasks, and waits up to `-wait` (60 s)
  before backing up and restarting the agent. If tasks remain without
  `-force` it refuses, and every refusal or failure after the API stop
  starts `skep_api` again and checks it. `-force` says pending and running
  tasks are interrupted and must be resubmitted (Decision 5).

## Amendment (2026-11-22, fix round): the pre-deinstall guard and the review fixes

Andy decided and approved these on 2026-11-22 (fix round for PR #52,
after the Fable review and the lab-VM run recorded in PR #54). They
refine Decisions 1, 2, 5 and 6 and build decisions 5 and 8; where they
differ, this amendment wins.

- **The guard for 0.1 is `skep-setup remove`.** pkg 2.9.2 deletes the
  files even when the pre-deinstall script fails (it stops only with
  `DEVELOPER_MODE` on), so build decision 5's refusal is not enforced.
  The script now prints a **warning** that says pkg deletes the files
  anyway and names `skep-setup remove`, which does refuse while Skep
  guests exist. It keeps exit 1, for a pkg that honours it.
  **`pkg lock` was rejected**: it would also block `pkg upgrade`.
- **Package scripts** set `PATH=/sbin:/bin:/usr/sbin:/usr/bin` first.
- **Root commands** (Decision 2): every base-system program is run by
  absolute path with a fixed minimal environment (`PATH`, `HOME=/`;
  `TERM` only for the interactive `admin create`); non-interactive
  commands run in their own process group; `apply`, `rollback`,
  `upgrade` and `remove` set umask `022`. With `-root` other than `/` and
  the real runner, service, `pw`, administrator and upgrade actions are
  refused.
- **`-admin`** (build decision 8, whose refusal stays): `skep-api admin
  create` runs as the `skep` user (UID and primary GID from the root's
  `/etc/passwd`), only once `skep_api`'s pid file and `api.db` exist (a
  bounded wait). A failed admin create finishes the journal with the
  error and the command exits non-zero with the command to retry.
- **Directory pools** must be a real directory (`lstat`), owned by UID 0,
  not group or other writable.
- **`agent.json` / `api.json` not written by setup** are parsed:
  allow-lists (or the listen address) equal to the answers are `ok`,
  different is a conflict with the proposed content, unparsable is
  refused. Setup never rewrites them.
- **Upgrade** (Decision 5): a service that is not running is not
  stopped (status first); once `skep_agent` is being stopped, Ctrl-C
  cancels nothing (bounded waits); `-keep >= 1` and the backup directory
  are checked before `skep_api` stops; after the starts, the agent's
  socket is polled (up to 30 s) until it reports the expected build.
  The agent's database path comes from its configuration; an agent that
  answers on its socket while that database is missing is a refusal,
  and a missing table is an error, not zero.
- **Remove** (Decision 6): `-purge-data` is refused while any Skep guest
  runs, also with `-keep-guests`. Skep keeps no ZFS key material (ADR
  0010: the operator holds the keys); remove checks the `keylocation` of
  the guest datasets' encryption roots and refuses `-purge-data` if one
  is a file under `/var/db/skep`. `-revert-host` keeps `zfs_enable`
  while any pool is imported and says so.
- **Journal**: a journal directory without a readable `journal.json` is
  shown as an unfinished run with no recorded changes that `rollback ID`
  clears; rollback prints the run it undoes before asking.

Covered by tests in `internal/setup`, `cmd/skep-setup`,
`internal/dbbackup` and `scripts/pkgstage`; all of it is pending FreeBSD
validation on the lab VMs.

## Amendment (2026-11-24): no release asset for 0.1

Accepted by Andy on 2026-11-24. It changes when Decision 8 applies, not
what it says; where they differ for 0.1, this amendment wins.

- **0.1 has no release asset.** No `skep-<version>.pkg`, `SHA256SUMS` or
  `SHA256SUMS.sig` is published on GitHub Releases for 0.1, and there is
  no `pkg` repository.
- **Users build the package themselves**, as the README describes:
  `make stage` on a development machine, then `pkg create` on a FreeBSD
  15 amd64 host, and `pkg install ./skep-0.1.0.pkg`
  ([packaging.md](../installation/packaging.md)).
- **Signed packages on GitHub Releases, and the signed repository, are
  deferred until signing is built.** Decision 8 stays as written and is
  the plan for the first release that publishes a package; its "signing
  is a manual release step for 0.1" applies to that release instead.
  The keys in [What Andy must provide](#what-andy-must-provide), item 4,
  are needed then.
- The work is a row in the [backlog](../backlog.md).

## Amendment (2026-11-25, Proposed): skep_cluster (ADR 0013 step 8c-2)

Written with step 8c-2 of ADR 0013 (its amendment 5 lists the decisions
D1–D29). **Status: Proposed**; Andy accepts it by merging that PR.

- **Upgrade may add one rc.conf line, with a journal.** Decision 5's
  upgrade changed no host file; from 8c-2 it adds
  `skep_cluster_enable="YES"` when the line is unset, through the
  Applier's own rules (re-read before the change, read back after), with
  a journal of command `upgrade` that rollback and `remove -revert-host`
  treat like an apply's. Set to anything else, upgrade refuses before
  anything is stopped.
- **New actions.** `cluster-init` (`skep-cluster init`, apply and upgrade;
  never undone by rollback); the services `skep_cluster` (start, stop);
  `pw-delete` and `remove-path` now also cover the `skep-cluster` account
  and `cluster.json`.
- **Upgrade's order** gains, after the agent, the init (where none), the
  line (where unset), the restart of `skep_cluster` with its own checks,
  and, after `skep_api`, the import of a host that is alone in its
  cluster (ADR 0013 amendment 5, D11 and D13).
- **remove and the cluster.** remove refuses while the node is not alone
  in its cluster, as the running `skep_cluster` reports it, and names the
  commands that make it so; it stops and disables `skep_cluster` with the
  other two services, and `-purge-data` deletes the cluster's node key,
  store and audit with the rest of `/var/db/skep`, `cluster.json`, and the
  `skep-cluster` account.

## What Andy must provide

1. **Two fresh FreeBSD 15.1-RELEASE amd64 VMs on Proxmox**, from the
   official VM images or a stock `bsdinstall`: one UFS root, one ZFS
   root. Each with 2+ vCPUs, 4+ GiB RAM, CPU type `host` (VT-x/EPT), two
   NICs (management on the lab network, as `hearst`'s `vtnet0`; a guest uplink on
   an isolated bridge or VLAN 60 with the Proxmox MAC filter off), a spare
   virtual disk for a pool on the UFS one, and a Proxmox snapshot of the
   untouched state. Passwordless sudo for the lab user, as on `hearst`.
2. **Approval to run the package (`pkg add`, `pkg upgrade`, `pkg delete`)
   and `skep-setup` on those VMs**, which `hearst`'s rules do not cover.
3. **Answers to the decisions above.**
4. **The signing keys** (RSA 4096 for the repository, Ed25519 for
   `ssh-keygen -Y`), generated on Andy's Mac with one offline backup, when
   signing is built.
5. **Later, not for 0.1:** a bare-metal box for hardware qualification
   (Milestone 11) and, if (b) is pursued, a FreeBSD build host for
   `release(7)`.

## Not decided here

- The custom image (b): its dialogs, partitioning, and profiles.
- A ports submission (`sysutils/skep`) and its review.
- Multi-node installation and joining a cluster (Milestone 8).
- Upgrading across FreeBSD minor releases, which needs rebuilding jail
  templates (ADR 0010 Decision 7b).
