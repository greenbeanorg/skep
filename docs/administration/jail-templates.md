# Jail templates

A jail template is a FreeBSD userland that Skep builds once and then
clones for each jail. Jails themselves arrive in later steps of Milestone
7b. This guide covers building, listing, and deleting templates. The
design is [ADR 0010](../decisions/0010-storage-and-jails.md), decision 7,
and the step 6 "Decision on package scripts" with its "Resolved" notes.

> **Validation status.** The pkgbase builder ran on FreeBSD 15.1-RELEASE-p6
> (amd64, UFS root, file-backed test pool) through its FreeBSD test binary
> with the real host operations
> ([record](../../tests/integration/jails/results/step6-build.txt)). A
> build through a deployed agent's task and API, the `dist` source, and a
> ZFS-root host are **pending FreeBSD validation**.

## What a template is

- A ZFS dataset `<pool>/<dataset_root>/templates/<template-id>`, with its
  snapshot `@skep-base`, set `readonly=on` once built. A jail will be a
  `zfs clone` of that snapshot.
- Built by the task `container.template.create` (cancellable, 30 minutes)
  and removed by `container.template.delete`.
- Of the host's release: the template's userland must have the host
  kernel's major.minor (`freebsd-version -k`). The patch level may
  differ; the template shows `patch_differs` when it does.
- In ZFS pools only, for now. A directory pool is refused with
  `storage_feature_unsupported`.

## Permissions

| Action | Permission | Who has it |
|---|---|---|
| List templates, read one, read the sources | `storage:read` | admin, operator, viewer |
| Build or delete a template | `storage:write` | admin only |

Building is admin-only because it makes the root agent fetch and install
packages.

## Configuration (`agent.json`)

| Key | Default | Meaning |
|---|---|---|
| `storage.template_packages` | `["FreeBSD-set-minimal", "FreeBSD-ssh"]` | The base packages a pkgbase template installs. Each must match `^FreeBSD-[a-z0-9][a-z0-9-]*$`. An empty list means no pkgbase source. Never taken from a request |
| `storage.dist_dir` | empty | A root-owned directory holding `base.txz` and its `MANIFEST`, for dist templates. Empty means no dist source |

The pool must be in `storage.pools`. The request names only a template
name, a pool, and a source. Packages, URLs and paths always come from the
configuration.

## Sources

`GET /api/v1/storage/template-sources` says which sources this host can
use, and why not:

- **pkgbase**: the host's own pkgbase repository configuration and
  signing keys (`/usr/local/sbin/pkg` must be installed), a non-empty
  `storage.template_packages`, and a compiled allowlist for the host's
  major.minor. Skep 0.1 has one for 15.1.
- **dist**: `base.txz` and `MANIFEST` in `storage.dist_dir`. The archive's
  SHA-256 must match its `MANIFEST` line (`template_archive_mismatch`).

## How a pkgbase build works

1. Checks that `<pool>/<root>/templates` is a directory owned by
   root:wheel with mode 0700 (`jail_parent_insecure`). Skep creates it
   that way on first use.
2. Records every regular file and symbolic link under the host's `/etc`,
   `/var/db/pkg`, and `/usr/share/certs`: type, mode, owner, size, mtime,
   and SHA-256 (764 entries on the lab host). One exact path is left out:
   `/var/db/pkg/vuln.xml`, which the daily `pkg audit -F` of periodic(8)
   rewrites on its own schedule. The list is compiled into Skep; ADR 0010
   gives the reason for each entry.
3. Creates the template dataset and a work area
   `<templates>/.work-<id>/` (`tmp/`, `cache/`).
4. Runs `pkg --rootdir <root> install -y <packages>` with **package scripts
   and triggers off** (`-o RUN_SCRIPTS=false -o PKG_TRIGGERS_ENABLE=false`),
   the cache and `TMPDIR` in the work area, the package database in the
   template, and an environment of exactly
   `PATH=/sbin:/bin:/usr/sbin:/usr/bin`, `ASSUME_ALWAYS_YES=yes`, and
   `TMPDIR`.
5. Reads every installed package's scripts from the template's database
   and every file in its trigger directories. Each must be on Skep's
   compiled allowlist for the release (package, kind, phase, SHA-256;
   trigger file name and SHA-256). Anything else is
   `template_script_unknown`, and the error carries the script's text.
6. Checks the template's release, read from `bin/freebsd-version` without
   running it (`release_unsupported`).
7. Starts a build jail `skep-build-<id>` on the template: no network, no
   devfs, every `allow.*` off (including `allow.suser`), securelevel 2, no System V IPC
   (`sysvshm`, `sysvsem`, `sysvmsg` = `disable`, set explicitly), no
   child jails. It checks those parameters as the kernel reports them. Then
   it runs the known commands of the allowed scripts and triggers inside
   it, one by one, with `jexec -l`:
   `certctl rehash`, `pwd_mkdb`, `services_mkdb` (reading `/etc/services`),
   `cap_mkdb`, `makewhatis`. Then it removes the jail and confirms it is
   gone.
8. Fails if pkg left a deferred trigger (`template_trigger_left`). Removes
   the repository catalogues the install copied in, and checks that the
   package cache is empty.
9. Records the host files again. Any difference is
   `template_host_changed`, listing the paths. The change is reported,
   not repaired.
10. Removes the work area, takes `@skep-base`, and records the release and
    size.

A failure at any step, a cancel, or the timeout removes the build jail and
destroys the template's dataset and work area. If the agent is killed in
the middle, it removes any leftover `skep-build-*` jail at its next start,
before it serves its socket, and logs a warning (`event=build_jail_removed`)
for each. Every start logs one line with the counts
(`event=build_jail_sweep`, `found`, `removed`, `failed`), so a start that
found nothing shows `found=0`. The template row is then left
unprovisioned; delete it.

A dist build checks the archive, extracts it with `tar -x -p` into the
template, checks the release, compares the host files, and snapshots. It
runs no build jail, because the distribution set ships its databases
already built.

### What is different from a host's own pkgbase install

- `/var/db/services.db` is populated. The `FreeBSD-runtime` 15.1
  post-install script passes a directory to `services_mkdb` and gets an
  empty database, on hosts too. Skep passes `/etc/services`. Lookups
  through the default `nsswitch.conf` (`services: compat`) read the text
  file either way.
- The manual-page index (`mandoc.db`) is built in the jail. pkg would have
  deferred it to the jail's first `pkg triggers`.
- The template has no copy of the host's repository catalogues.

## Errors

| Code | Meaning | What to do |
|---|---|---|
| `template_script_unknown` | A package script or trigger file is not on the allowlist (the error shows it) | A package changed. Do not work around it; a Skep release with the new hash is needed |
| `release_unsupported` | The userland's major.minor is not the host's, or Skep has no allowlist for the host's release | Build on a host of a supported release |
| `template_host_changed` | Host files changed during the build (listed) | Investigate the listed files; something besides the build may have changed them |
| `template_trigger_left` | pkg saved a deferred trigger although triggers were off | Report it: the pkg version behaves differently from the one Skep was built against |
| `template_build_failed` | pkg, tar, or a build-jail command failed; the message has its first line of stderr (for a command in the build jail, and for the build jail's `jail -c`, with control and Unicode format characters removed and cut to 200 bytes, since the template's userland can write it; amendment r) | Read the message. A name-resolution error from pkg is usually DNS blocking `pkg.FreeBSD.org` |
| `template_archive_mismatch` | `base.txz` does not match `MANIFEST` | Replace both from the same release |
| `jail_parent_insecure` | The templates directory is not root:wheel 0700 | `chown root:wheel` and `chmod 0700` it |
| `template_name_taken` | Another template has the name | Choose another |
| `storage_feature_unsupported` | The source is not available, or the pool is a directory pool | See `GET /storage/template-sources` |

## Removing a template

`DELETE /api/v1/storage/templates/{id}` destroys the dataset, its snapshot,
and any work area. Once jails exist, a template with jails cloned from it
cannot be deleted (`snapshot_has_clones`).
