# ADR 0012: Installing, upgrading, and removing Skep (Milestone 10)

- Status: **Proposed** (2026-11-22). Not to be merged until Andy has
  answered [the decisions](#decisions-for-andy), the answers are recorded
  here, and the status is Accepted.
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

## Recommendation: (c), committing to (a) only for 0.1

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
  secrets); `share/licenses/skep/`; `share/doc/skep/` (optional).
- **Never** contains `agent.json` or `api.json` themselves, so an upgrade
  cannot overwrite the administrator's configuration. `skep-setup`
  writes them from its answers on first setup.
- **Dependencies:** none required. `edk2-bhyve` is checked by
  `skep-setup check` and offered (`pkg install edk2-bhyve` printed), not
  a hard dependency: a jails-only host does not need it.
- **Scripts:** pre-install creates group and user if absent. No
  post-install changes. Pre-deinstall refuses (exit non-zero) while
  `skep_agent` runs and Skep guests exist, so `pkg delete` cannot
  orphan running guests; `-f` overrides as usual for pkg.
- **Building:** the release job runs `make build-freebsd`, stages the
  tree, and either (i) runs `pkg create -M +MANIFEST -r stage` on a
  FreeBSD builder, or (ii) writes the `.pkg` from Go as the probe does.
  The probe shows (ii) works up to the archive; whether FreeBSD's `pkg`
  accepts it is **not yet tested**. Recommendation: (i) for releases (the
  supported tool), with (ii) kept only if (i) is impractical in CI.
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
| The named management interface has no `ifconfig_` line with an address | It will not guess which interface carries management |
| A bridge to be allow-listed has the management interface as a member | Skep would refuse guests on it anyway; guests next to management is the one network layout Skep forbids |
| The uplink for a new bridge has an address, other settings, is the management interface, or is in another bridge | It is in use |
| Only one of the `skep` user and group exists, or the user has a login shell or a home | Something else made it |
| The release is not FreeBSD 15.x amd64 | Not a supported platform |
| `agent.json` or `api.json` exists and was not written by this tool | The administrator's file; setup prints its proposed version beside it |
| Asked to apply while a previous apply's journal is incomplete | Finish or roll back first |

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
| `/boot/loader.conf` | `kern.racct.enable="1"` only with `--racct` | Next boot |
| `/etc/rc.conf` | `kld_list+=` the missing ones of `if_epair netgraph ng_socket ng_ether ng_bpf` | Next boot; setup prints the `kldload` line |
| `/etc/rc.conf` | `zfs_enable="YES"` when a ZFS pool is named and it is missing | Next boot |
| `/etc/rc.conf` | A **new** bridge, only with `--create-bridge NAME --uplink IF`: `cloned_interfaces+=`, `ifconfig_IF="up"`, `ifconfig_NAME="addm IF ifuntagged IF 1 defuntagged 1 vlanfilter up"` | Next boot; or setup prints the `ifconfig` lines in order |
| `/etc/rc.conf` | `skep_agent_enable`, `skep_api_enable` | `service ... start` with `--start` |
| `/usr/local/etc/skep/agent.json` | Written once from the answers (pools, directory pools, bridges, management interfaces); `0600 root:wheel` | Agent start |
| `/usr/local/etc/skep/api.json` | Written once with `listen_address`; `0640 root:skep` | API start |
| `/var/db/skep/media` | Created `0700 root:wheel` | — |
| First administrator | `skep-api admin create` run interactively (or `--password-stdin`); never a password in a flag, file, or the journal | — |

The listen address defaults to the management interface's address, port
8443, shown in the plan; `0.0.0.0` is refused unless `--listen-all` is
given (ADR 0006 Decision 7).

## Decision 4: idempotency, the journal, and rollback

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
2. Backs up both databases with SQLite's online backup, to
   `/var/db/skep/backup/<old version>-<timestamp>/`, and the audit
   archives' file lists. (New code: today nothing takes this backup.)
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
  setup refuses to upgrade while tasks are `running` unless `--force`.

**Compatibility between programs.** `skep-api` and `skep-agent` come from
one package and are upgraded together; there is no version handshake
between them today. Proposed: the agent reports its version on its
internal API and `skep-api` refuses to serve (503, a new error code)
when the versions differ. That is an API/error-code change and needs its
own approval (decision 9).

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

## Decision 7: version and compatibility checks

- The package's ABI restricts it to FreeBSD 15 amd64 (pkg enforces it).
- `skep-setup check` reports, and `apply` refuses on: not 15.x amd64,
  kernel and userland at different major.minor. It **warns** (does not
  refuse) on a minor other than the qualified one (15.1 for 0.1), and on
  hardware virtualization missing (jails still work).
- The agent's existing discovery remains the authority at run time
  (bhyve available, ZFS, modules); setup reuses its checks rather than
  duplicating them where it can (one code path for "what is missing").

## Decision 8: signing and integrity

- **Repository:** a `pkg` repository (`pkg repo` with a signing key)
  hosted as static files. `pkg` verifies the catalogue signature and each
  package's checksum natively; the administrator adds a
  `/usr/local/etc/pkg/repos/skep.conf` with the public key's path.
- **Direct download:** `skep-<version>.pkg` and `SHA256SUMS`, with
  `SHA256SUMS.sig` made by `ssh-keygen -Y sign`, verifiable on stock
  FreeBSD with `ssh-keygen -Y verify` (base system, no package needed).
- **Keys:** generated and held offline by Andy; the public key and its
  fingerprint published in the README, the docs, and the release notes.
  CI never holds the private key; signing is a manual release step for
  0.1.
- **Reproducibility:** the Go build is already `-trimpath` with fixed
  ldflags; the probe writes archives with a fixed mtime. Reproducible
  packages are a goal, not a 0.1 requirement.

## Decision 9: how it is tested

- **Unit tests** on fixture roots, as the probe does: stock UFS and ZFS
  `rc.conf`/`loader.conf` as `bsdinstall` writes them, hearst's, and
  hostile cases (shell code in `rc.conf`, a bridge holding management,
  conflicting values). Every refusal has a test.
- **Package tests in CI (optional, needs approval):** a FreeBSD VM on a
  Linux runner (QEMU, no nested bhyve) to run `pkg add`, `skep-setup
  check`/`plan`, start both services, and `pkg delete`. This adds a
  third-party Action; the alternative is running the same steps by hand
  on a lab VM per release.
- **Lab VMs:** two fresh Proxmox VMs from the official FreeBSD 15.1 VM
  images (UFS and ZFS root), two NICs each, VT-x exposed, with a snapshot
  to revert to after each run. On each: install, `apply` twice
  (idempotent), a VM and a jail on the new bridge, `upgrade` from the
  previous build with both guests running, `rollback`, `remove`. Not
  `hearst`, which is the integration host and is not stock.
- **Bare metal:** not needed for (a) at 0.1; hardware qualification is
  Milestone 11.

## Decisions for Andy

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

## What Andy must provide

1. **Two fresh FreeBSD 15.1-RELEASE amd64 VMs on Proxmox**, from the
   official VM images or a stock `bsdinstall`: one UFS root, one ZFS
   root. Each with 2+ vCPUs, 4+ GiB RAM, CPU type `host` (VT-x/EPT), two
   NICs (management on the lab network, as `hearst`'s `vtnet0`; a guest uplink on
   an isolated bridge or VLAN 60 with the Proxmox MAC filter off), a spare
   virtual disk for a pool on the UFS one, and a Proxmox snapshot of the
   untouched state. Passwordless sudo for the lab user, as on `hearst`.
2. **Approval to run the probe's package (`pkg add`) and `skep-setup`
   on those VMs**, which `hearst`'s rules do not cover.
3. **Answers to the decisions above.**
4. **A signing key pair** (or approval for Claude to generate one on a
   machine Andy names, with the private key never leaving it), when
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
