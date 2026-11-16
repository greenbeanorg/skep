# Step 6 build: progress log

ADR 0010 phase 7b, step 6 (template store, dist source, pkgbase builder,
tasks, routes), branch `feat/m7b-templates`. Started 2026-11-16 from
`65307d1`, as an unattended run.

## 1. Feasibility probe on hearst: STOPPED (spec not buildable as written)

**What was done.** Before writing builder code, I hand-ran the accepted
specification's steps on hearst in a new dataset, `skeptest/step6-probe`:
the install with scripts and triggers off, the build jail with every
`allow.*` off and no devfs, and the fixed commands through `jexec`.
Record: [step6-probe.txt](step6-probe.txt); scripts in
[../step6-probe/](../step6-probe/).

**Found.**

1. `jexec -l -U root` fails before `pwd_mkdb` has run: `getpwnam: root: No
   such file or directory`. The jail has no `pwd.db` yet, and FreeBSD's
   `files` passwd backend reads nothing else. The follow-up ruling's fixed
   order starts with certctl and pwd_mkdb, both run as `-U root`.
2. After `pwd.db` exists, `-U root` still fails, with `initgroups: root:
   Operation not permitted`, because item 2 turns every `allow.*` off,
   including `allow.suser`.
3. `jexec -l` without `-U` (uid 0, root's login-class environment) runs all
   six commands with every `allow.*` off, no devfs, and `securelevel=2`. So
   does `allow.suser=1` with `-l -U root` once pwd_mkdb has run.
4. `services.db` cannot match the trace. The package script feeds
   services_mkdb a directory and gets an **empty** database, both in the
   trace and in hearst's own base system. The ADR's `/etc/services` builds
   the populated one. pwd.db, spwd.db, login.conf.db and the certs are
   byte-identical to the trace.
5. Things that hold as specified: scripts and triggers off (nothing runs,
   nothing deferred); `TMPDIR` used and cleaned up; all four script hashes
   and the `mandoc.ucl` hash match the ADR; no command needs devfs or a
   securelevel below 2; `jail -r` plus `jls -j` confirm removal.

**Decisions made.** None about the spec. I picked the probe's location
myself (a new skeptest dataset, scripts piped over ssh stdin so nothing was
copied to the host). This is easy to undo.

**Surprising.** Items 1, 2 and 4 above. Also, `pkg info --raw --raw-format
json-compact -a` prints one JSON array, not one object per line.

**Stopped** under the run's rule: "the ADR spec turns out to be unbuildable
as written. Don't work around the spec." Options are at the end of
step6-probe.txt. I recommend A: `jexec -l` without `-U`, every `allow.*`
off, and keep `/etc/services`.

**Commit:** `e3fc0c7`.

**hearst state:** no jails. `skeptest/step6-probe` (162M) is left in place.
`skeptest/trace-7b` and `/var/tmp/skep-trace7b` are untouched. Nothing else
changed.

## 2. Andy's answers recorded (2026-11-16)

**Done.** ADR 0010: the "Open" section became "Resolved" (option A:
`jexec -l` without `-U`, every `allow.*` off in the build jail;
`/etc/services` kept, with the reason; the read-only lookup check);
amendment e to Decision 9 (ordinary jails keep `allow.suser` on);
spec items 2 and 3 and the follow-up ruling point to it. The lookup check:
`getent services ssh` and `domain` both answer on hearst, because
`services: compat` reads the `/etc/services` text and only the `db` source
opens the db (libc `getservent.c`, releng/15.1). `skeptest/step6-probe`
destroyed.

**Commit:** see the next entry.

## 3. hostops template layer, allowlist, build sequence

**Done.** `internal/hostops` gains a `Templates` interface
(`templates.go`, `templates_pkg.go`, `templates_jail.go`, `hostfiles.go`):
the templates dataset and its 0700 check, the template dataset and work
area, `pkg install` with the fixed environment and options, `pkg info
--raw`, the trigger-directory listing through `os.Root` without following
links, `USERLAND_VERSION` read from `bin/freebsd-version`, `base.txz`
checked against `MANIFEST` and extracted, the build jail (create, check
the kernel's view of its parameters, `jexec -l`, remove and confirm), the
host-file record. The real runner gains `RunEnv` (its own environment,
timeout, and output cap; output past the cap is an error there, not cut).
`internal/templates` holds the compiled allowlist (15.1, pinned by a test
against the ADR's exact strings) and the build sequence with its clean-up.
Unit tests: 20 in hostops, 20 in templates, all passing on merrick.

**Decisions made (small, easy to undo, in line with the ADR):**

- The work area is a directory `<templates mountpoint>/.work-<id>`, not a
  dataset (item 8 allows either). `<root>/templates` is a mounted dataset
  (unlike `<root>/vm`, which is `canmount=off`), so the work areas are in
  it and the 0700 parent is a real directory of its own.
- The build jail's `allow.*` list is fixed in code (19 parameters, the
  15.1 GENERIC set), and after `jail -c` every `allow.*` that `jls
  --libxo=json -n` reports must be `false`. The probe showed two default to
  on (`reserved_ports`, `unprivileged_proc_debug`), which is why each is
  listed. The JSON form is read rather than `jls -n`, whose `allow.noX`
  spelling is ambiguous. Fixture: `internal/hostops/testdata/jls-build-jail-15.1.json`,
  recorded on hearst.
- A build command runs only if an allowlisted script or trigger in the
  template asks for it (the ADR maps each script and trigger to its
  commands), always in the fixed order. The OpenSSL `makewhatis` runs only
  if `usr/share/openssl/man` exists (the ADR's "if it exists").
- The template is set `readonly=on` after `@skep-base` ("read-only from
  then on", decision 7b).
- New error codes besides the ADR's three: `template_build_failed` (500,
  a pkg, tar, or build-jail command that ran and failed, with the first
  stderr line, as item 3 asks), `template_name_taken` (409),
  `template_not_provisioned` (409), `template_archive_mismatch` (422, the
  dist check, which the ADR leaves unnamed). `release_unsupported` (422)
  and `jail_parent_insecure` (409) get their constants. A source that is
  not available is `storage_feature_unsupported`.
- A symlink or other non-regular entry in a trigger directory fails with
  `template_script_unknown` (it is the allowlist check that fails).
- A failed build that also changed the host files reports
  `template_host_changed`, with the build's own error in the details. A
  clean-up failure (a jail that will not go) is appended to the task
  error's message and details.
- No general event stream exists in the agent yet. A host change goes in
  the task error, the audit details and an `event=template_host_changed`
  error log. The start-up sweep (next piece) logs a warning.
- A `.work-` directory name cannot collide with a template: template IDs
  are UUIDs.

## 4. Template store, service, tasks, agent wiring, internal routes

**Done.** `database.TemplateStore` (container_templates, cursor pages by
name); `templates.Service` with `container.template.create` (cancellable,
30 min) and `container.template.delete` (5 min), the `template-sources`
read, idempotency like the VM tasks, and `SweepBuildJails`, which the
agent runs after the engine starts and before it serves its socket.
Configuration: `storage.template_packages` (default
`["FreeBSD-set-minimal"]`, each `^FreeBSD-[a-z0-9][a-z0-9-]*$`, no
repeats) and `storage.dist_dir` (absolute, clean). The confinement carries
`DistDir`. The fake host simulates a whole build, with the real 15.1
script texts, so the dev agent and the UI can use templates. The internal
API gets `GET /internal/v1/storage/template-sources`, `GET|POST
/internal/v1/storage/templates`, and `GET|DELETE
/internal/v1/storage/templates/{id}`. `go test ./...` passes on merrick.

**Decisions made:**

- Routes live under `/storage/templates`, with `storage:read` to read and
  `storage:write` to change (decision 14 makes writing `storage:write`).
  `GET /storage/template-sources` is what "capabilities" means in the
  ADR: host release, whether pkgbase and dist can be used, and why not.
- Templates are refused in a **directory pool**
  (`storage_feature_unsupported`) in this step. Decision 6 describes jail
  roots copied from a template directory, but a template build into a
  directory pool needs its own layout and has no snapshot, so it is left
  for later and flagged in the report. This is a scope gap, not a security
  choice.
- A dist template's row holds a placeholder checksum of zeros until the
  build has checked the archive (the schema requires a checksum on every
  dist row). The API never shows the placeholder.
- The packages recorded on a template, and passed to its task, are the
  configured ones at request time.
- On a host whose release has no allowlist, a pkgbase request is refused
  with `release_unsupported` before any task exists.
- Deleting a template first removes a build jail of that template if one
  is somehow left, then destroys the dataset and work area.

## 5. Public routes, permission, agent client, OpenAPI

**Done.** skep-api serves `GET /api/v1/storage/template-sources`,
`GET|POST /api/v1/storage/templates`, `GET|DELETE
/api/v1/storage/templates/{id}`. `storage:write` is now declared and
enforced on the two writes (admin only: `api/0005` already left it out of
operator and viewer). The permission matrix test covers the new routes,
and explicit rows assert that an operator's and a viewer's template create
gets 403 and is audited. The agent client gains the five calls. OpenAPI
documents the paths, schemas, and codes; `web/src/api/schema.d.ts` is
regenerated (`npm run check-types` passes).

**Decisions made:** template writes are audited in the access log with
resource type `container_template`. A refusal by the task's companion
(`template_name_taken`) is audited once, by the engine.

**Still owed (memory note, Andy 2026-11-15):** the real-request check that
an operator's `container.template.create` is refused 403 on hearst. That
needs the deployed skep-api. It is not part of this run's validation list
and is left for the step that deploys (see the end of this log).

## 6. Validation on hearst

**Done.** Record: [step6-build.txt](step6-build.txt), logs in
[step6-build/](step6-build/). The FreeBSD test binary of
`internal/templates` drove the real builder against
`skeptest/step6/templates`, as root, from the scratch dataset
`skeptest/step6-scratch`. Nothing else on hearst was changed: no deploy, no
config edit.

- Build: `15.1-RELEASE-p6`, 66 packages, 6 commands in the build jail,
  `@skep-base`, read-only. 36 s.
- Host files identical, both by the builder (764 entries) and from outside
  (1521 lines, the trace's method). `pkg` ran with exactly `PATH`,
  `ASSUME_ALWAYS_YES`, `TMPDIR`. No build jail left, no deferred trigger,
  no catalogues, no cache, no work area.
- Comparison with `skeptest/trace-7b`: +2 (`mandoc.db` ×2, makewhatis ran),
  −18 (the repository catalogues and the trace's deferred trigger),
  2 changed: `services.db` (intended: populated instead of empty) and
  `local.sqlite` (only `packages.time` differs). Nothing flagged.
- Refusal: an appended line in `mandoc.ucl` → `template_script_unknown`
  with the path, hash and text; nothing left behind.
- Native hostops template tests as root: 20 pass, 1 skip.

**Surprising:**

- The first build check failed on its own threshold (1000 host files);
  the trace's "1,500" was a line count. Fixed in `e4f39e8` and re-run:
  PASS.
- `pkg shell <file>` opens the *host's* package database, not the file. I
  ran it once by mistake and it failed to parse before doing anything. The
  host files were re-hashed straight away and were identical. Comparisons
  of databases are done on copies on merrick from now on.
- The whole `hostops.test`, with `TMPDIR` on ZFS, fails the 7a
  `TestDirPoolNative`. That is the documented compression effect, not this
  step.

**Commits:** `9ecd9af` (integration test), `e4f39e8` (threshold), this
entry's commit (records).

## 7. Documentation

**Done.** New guide `docs/administration/jail-templates.md`; a "Jail
templates" section in `docs/security/overview.md` and in
`docs/architecture/overview.md`; the README's status table; a pointer from
the storage guide; ADR 0010's step 6 "Built" notes and status line; the
README of `tests/integration/jails`.
