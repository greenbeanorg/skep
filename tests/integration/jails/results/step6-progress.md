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
