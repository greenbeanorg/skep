# Building the FreeBSD package

How a Skep release becomes `skep-<version>.pkg` ([ADR 0012](../decisions/0012-installer.md)
Decision 1). The tree and the manifest are staged on the development
machine; `pkg create` runs by hand on a FreeBSD 15 amd64 lab VM, because
it needs FreeBSD's `pkg` (answer 2). There is no Go `.pkg` writer.

**Status:** the staging is built and tested on Linux. `pkg create`, `pkg
add`, `pkg upgrade` and `pkg delete` with this manifest and these scripts
are **pending FreeBSD validation** on the lab VMs ADR 0012 asks for.

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
| `dist/pkg/+MANIFEST` | Name `skep`, origin `sysutils/skep`, ABI `FreeBSD:15:amd64` (set here, not detected: Go stamps a FreeBSD 12.3 note into its binaries), prefix `/usr/local`, license `BSD2CLAUSE`, and the two package scripts |
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
- **pre-deinstall** refuses while Skep guests run: jails named `skep-*`
  in `jls` and VMs named `skep-*` in `/dev/vmm`. It never refuses when
  `PKG_UPGRADE` is set, because `pkg upgrade` runs the old package's
  deinstall scripts and an upgrade leaves guests running. `pkg delete -f`
  overrides it.

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

1. `pkg add out/skep-0.1.0.pkg`: the account appears as `skep:409:409`
   (`id skep`), nothing in `rc.conf` changed (`sha256 /etc/rc.conf` before
   and after), no service started.
2. Run it again with the account already present: no change.
3. `skep-setup check` and `skep-setup plan`.
4. With a Skep guest running: `pkg delete skep` is refused and lists the
   guest; `pkg delete -f skep` is not needed for this check.
5. With a guest running, `pkg upgrade` (or `pkg add -f` of a newer build)
   succeeds, the guest keeps running, and **record what pkg sets in
   `PKG_UPGRADE`** (`env` in a test pre-deinstall script): the refusal's
   skip depends on it (answer 14).
6. With no guest running: `pkg delete skep` removes the files and keeps
   `/var/db/skep`, `/usr/local/etc/skep/*.json` and the account.

Signing and the repository (Decision 8) are separate, manual release
steps with Andy's keys; they are not built yet.
