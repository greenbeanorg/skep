# Building the FreeBSD package

How a Skep release becomes `skep-<version>.pkg` ([ADR 0012](../decisions/0012-installer.md)
Decision 1). The tree and the manifest are staged on the development
machine; `pkg create` runs by hand on a FreeBSD 15 amd64 lab VM, because
it needs FreeBSD's `pkg` (answer 2). There is no Go `.pkg` writer.

**Status:** the staging is built and tested on Linux, and `pkg create`,
`pkg add`, `pkg upgrade` and `pkg delete` with this manifest and these
scripts ran on the lab VMs (FreeBSD 15.1-RELEASE-p7, pkg 2.9.2, smith and
langrishe, 2026-11-22;
[record](../../tests/integration/setup/results/lab-vm-validation-20261122.md)).
Everything in sections 3 and 4 behaved as written **except the
pre-deinstall refusal, which pkg does not enforce** (section 2); the
script now says so and only warns (ADR 0012, fix-round amendment). The
reworded warning ran on langrishe on 2026-11-22
([record](../../tests/integration/setup/results/lab-vm-recheck-20261122.md),
L2). All of these are lab VMs nested under KVM, not bare metal.

**Dependency on the firmware.** The package depends on `edk2-bhyve`, the
bhyve UEFI firmware the agent opens to start a VM. How pkg resolves it for
a local package file (pkg 2.9.2 on langrishe, 2026-11-23, with the
firmware removed first):

- `pkg install ./skep-0.1.0.pkg` (also `-f` over an installed skep) installs
  `edk2-bhyve` from the configured FreeBSD-ports repository first, then
  skep ("edk2-bhyve: g202508_3 [FreeBSD-ports] (required by skep)").
  **Use this for a local file.**
- `pkg add ./skep-0.1.0.pkg` does not fetch it: "pkg: Missing dependency
  'edk2-bhyve'", exit 1, and the installed skep is left as it was. It
  works once the firmware is installed (`pkg install edk2-bhyve`).
- From a repository (`pkg install skep`, Decision 8, not built yet) pkg
  resolves the dependency like any other.

A host installed by hand (binaries copied, as on hearst) has no package and
so no dependency; `skep-setup check` and `plan` still warn when the
firmware files are missing.

## 1. Stage (development machine)

```sh
make stage            # builds the web interface and the FreeBSD binaries first
# or, without Node.js:
make stage-go
```

This writes `dist/pkg/` (replaced on every run; the tool refuses a
directory it did not make):

| Path | What |
|---|---|
| `dist/pkg/stage/usr/local/...` | The package's files |
| `dist/pkg/+MANIFEST` | Name `skep`, origin `sysutils/skep`, ABI `FreeBSD:15:amd64` (set here, not detected: Go stamps a FreeBSD 12.3 note into its binaries), prefix `/usr/local`, license `BSD2CLAUSE`, one dependency (`edk2-bhyve`, origin `sysutils/edk2`; see below), and the two package scripts |
| `dist/pkg/plist` | Every file with its owner and mode, `@(root,wheel,0555) sbin/skep-agent` |
| `dist/pkg/SHA256SUMS.stage` | SHA-256 of every staged file, to compare on the VM |

The package contains:

| File | Mode |
|---|---|
| `sbin/skep-agent`, `sbin/skep-api`, `sbin/skep-setup`, `bin/skepctl` | `0555 root:wheel` |
| `etc/rc.d/skep_agent`, `etc/rc.d/skep_api` | `0555 root:wheel` |
| `etc/skep/agent.json.sample`, `etc/skep/api.json.sample` | `0644 root:wheel` |
| `share/licenses/skep/LICENSE`, `share/licenses/skep/THIRD_PARTY_NOTICES` | `0444 root:wheel` |

Never `agent.json` or `api.json` themselves: an upgrade must not
overwrite the administrator's configuration, and `skep-setup apply`
writes them on first setup.

The staging tool refuses binaries that are not FreeBSD amd64 ELF files,
a version that is not a package version, and a missing file.

## 2. The package scripts

Both are plain `sh`, embedded in `+MANIFEST` from `packaging/pkg/`.

- **pre-install** creates the `skep` group and user with UID and GID
  **409** when neither exists (`pw groupadd skep -g 409`, `pw useradd skep
  -u 409 -g skep -d /nonexistent -s /usr/sbin/nologin`). It refuses (the
  install stops) when only one of them exists, or when 409 belongs to
  another name. It warns, and changes nothing, when both exist with other
  numbers (a host set up by hand earlier, such as `hearst` with 1002).
- **pre-deinstall** **warns** while Skep guests run: jails named `skep-*`
  in `jls` and VMs named `skep-*` in `/dev/vmm`. It asks the host, not the
  agent, so it warns even when `skep_agent` is stopped. The warning says
  that pkg deletes the files anyway and names `skep-setup remove`, the
  guard for 0.1, which refuses while Skep guests exist; the script still
  exits 1 for a pkg that honours it. It is silent when `PKG_UPGRADE` is
  set, because `pkg upgrade` runs the old package's deinstall scripts and
  an upgrade leaves guests running. `pkg lock` was rejected as a guard: it
  would also block `pkg upgrade` (ADR 0012, fix-round amendment).
- Both scripts set `PATH=/sbin:/bin:/usr/sbin:/usr/bin` first: pkg runs
  them as root with the caller's environment. (The script tests replace
  that one line to find their fake `pw` and `jls`.)

  **Measured on pkg 2.9.2 (2026-11-22):** `pkg upgrade` runs the old
  package's pre-deinstall with `PKG_UPGRADE=true`; `pkg delete` leaves it
  unset; `pkg add` of a newer `.pkg` is a "forced install" and runs no
  pre-deinstall at all. **A failing pre-deinstall does not stop `pkg
  delete`** unless `DEVELOPER_MODE` is on: pkg prints "DEINSTALL script
  failed", deletes the files and exits 0. The script's message is
  therefore worded as a warning (decided 2026-11-22). A failing
  pre-install does stop `pkg add` (exit 1, nothing installed).

Neither script edits `rc.conf`, enables, starts or stops a service.

## 3. Create the package (FreeBSD 15 amd64 lab VM)

Copy `dist/pkg` to the VM, then:

```sh
cd pkg
sha256 -r stage/usr/local/sbin/* stage/usr/local/bin/*   # compare with SHA256SUMS.stage
mkdir -p out
pkg create -M +MANIFEST -p plist -r stage -o out
pkg info -F out/skep-0.1.0.pkg          # name, version, ABI FreeBSD:15:amd64
pkg info -l -F out/skep-0.1.0.pkg       # the ten files
pkg info -R -F out/skep-0.1.0.pkg | grep -A3 scripts
```

## 4. Check it on the lab VM (per release, ADR 0012 answer 13)

On a VM restored from its clean snapshot:

1. `pkg install ./out/skep-0.1.0.pkg` (pulls in `edk2-bhyve`; `pkg add` needs it installed first): the account appears as `skep:409:409`
   (`id skep`), nothing in `rc.conf` changed (`sha256 /etc/rc.conf` before
   and after), no service started.
2. Run it again with the account already present: no change.
3. `skep-setup check` and `skep-setup plan`.
4. With a Skep guest running: `skep-setup remove` refuses; `pkg delete
   skep` prints the warning listing the guest and that pkg deletes the
   files anyway (pkg 2.9.2 then deletes them).
5. With a guest running, `pkg upgrade` (or `pkg add -f` of a newer build)
   succeeds, the guest keeps running, and **record what pkg sets in
   `PKG_UPGRADE`** (`env` in a test pre-deinstall script): the refusal's
   skip depends on it (answer 14).
6. With no guest running: `pkg delete skep` removes the files and keeps
   `/var/db/skep`, `/usr/local/etc/skep/*.json` and the account.

Signing and the repository (Decision 8) are separate, manual release
steps with Andy's keys; they are not built yet.
