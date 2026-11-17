# Step 9 build: progress log

ADR 0010 phase 7b, step 9 (RCTL limits and quotas, Decision 11; jail
snapshots and clones, reusing step 2; Decisions 2, 3, 13, 14, 16),
branch `feat/m7b-templates`. Started 2026-11-17 from `33e73f1`. hearst
runs `79620ef`.

**Approved on hearst for this run:** `scripts/deploy-test.sh`; creating,
starting, stopping, and deleting test containers and jails; read-only
inspection. **Not approved:** reboots, `loader.conf`, `kld_list`,
`rc.conf`, `devfs.rules`, pkg installs, ZFS changes outside
`skeptest/skep`, anything on swearengen or OPNsense. Spoofing stays
deferred.

## 1. Probe on hearst (read-only, 2026-11-17)

- `15.1-RELEASE-p6`; `kern.racct.enable: 1` (the step 6 reboot);
  `rctl` lists no rules; `rctl -u process:<pid>` reports `memoryuse`,
  `vmemoryuse`, `maxproc`, `openfiles`, `pcpu`, and the rest.
  `kern.racct.rctl.throttle_*` at their defaults.
- No jails (`jls` empty), no epairs (`vtnet0 vtnet1 lo0 bridge0`).
- `skeptest/skep/jail` exists and is empty to its owner (0700, not
  readable by the probe user, as intended); two templates with
  `@skep-base`; no `refquota` or `quota` set anywhere under `skeptest`.
- hearst now has 4 CPUs, 6 GiB RAM, and 4 GiB swap (CLAUDE.md still says
  2 and 2 GiB). Swap matters for the `memoryuse` check: an over-limit
  jail may be paged out rather than refused (Decision 11 says to record
  that if it happens).
- `skep_agent` running (`79620ef`).

## 2. Plan

### Build

1. **Limits model and validation.** `models.ContainerLimits` on create,
   update (stopped only, as today), and the container:
   `memory` (bytes, `ByteSize`; RCTL `memoryuse:deny`), `cpu_percent`
   (`pcpu:deny`; 100 is one CPU, bounded by `100 * hw.ncpu`),
   `max_processes` (`maxproc:deny`), `open_files` (`openfiles:deny`),
   `disk_quota` (bytes; ZFS `refquota` on the jail root, so snapshots do
   not count against it). 0 or absent means no limit. Stored in
   `containers.config` with the options; no migration.
2. **Refusals.** Any RCTL limit with RACCT off: `rctl_unavailable`.
   `mlock` is accepted only with a `memory` limit and RACCT on, otherwise
   `mlock_requires_memory_limit` (replaces step 7's blanket refusal). A
   stored RCTL limit and RACCT off at start (after a reboot without the
   tunable) refuses the start with `rctl_unavailable`: a jail never runs
   without the limits it was given. A disk quota below what the root
   already uses is refused by ZFS and reported as `invalid_request`.
3. **`hostops`.** RCTL calls with fixed argument lists:
   `rctl -r jail:skep-<id>` before `jail -c` and after every removal
   (stop, discard, failed start, sweep), `rctl -a jail:skep-<id>:<res>:deny=<n>`
   after `jail -c` and before the `/dev` guard and `/etc/rc`, then
   `rctl -l jail:skep-<id>` compared with the expected set (mismatch:
   the jail is discarded, `container_start_failed`). `zfs set refquota`
   on the jail root at create and update (owner-checked). RACCT state from
   `kern.racct.enable` in the capabilities. The fake mirrors all of it.
4. **Jail snapshots.** Tasks `container.snapshot.create` (running:
   `crash_consistent`; stopped: `consistent`), `container.snapshot.rollback`
   (stopped only; `destroy_newer` as for VMs; `started_once` stays 1,
   Decision 13), `container.snapshot.delete` (`snapshot_has_clones`).
   Reuses `hostops.Snapshot/Rollback/DestroySnapshot` with the container
   owner and the `snapshots` store. Routes under
   `/api/v1/containers/{id}/snapshots`, permission `container:snapshot`
   (declared now, as step 7 said), in the agent's internal API, the
   public API, the client, and `api/openapi.yaml`.
5. **Jail clones** (`container.clone`): a new container whose root is
   `zfs clone` of one snapshot of the source's root, with
   `skep:container_id` and `readonly=off` set and the source's
   `refquota` set again (clones do not inherit it); new ID, name, and
   MAC addresses; the same template, NIC slots, bridges, addressing,
   options, limits, and by default hostname (Decision 3: "a clone is the
   same guest"); `started_once` inherited, 1 if taken from a running
   jail; `cloned_from_snapshot_id` recorded. Clone from current state is
   snapshot-then-clone in one task, leaving the snapshot named after the
   clone. Deleting the origin snapshot or the source is refused with
   `snapshot_has_clones`. **The route waits on Q1.**
6. Docs: ADR 0010 step 9 implementation notes, `api/openapi.yaml`, the
   administration guide's jail section (limits, RACCT cost).

### Validate on hearst (records in `step9-*.txt`)

- **Limits:** `memoryuse` 256 MiB, then `dd if=/dev/zero of=/dev/null bs=512m count=1`
  in the jail (dd allocates and fills its buffer; no binary is copied
  into the jail): refused or not, with `rctl -u` and the process's RSS
  recorded. If it is not refused, `vmemoryuse` is evaluated the same way
  and the result goes to Andy (Decision 11) before "verified".
  `pcpu` 50 with a busy `sh` loop: the jail's `pcpu` from `rctl -u` and
  the loop's `%CPU` over time. `maxproc` and `openfiles` hit and
  refused. `rctl -l` shows exactly Skep's rules while running and none
  after stop. `mlock` without a memory limit refused; with one, the
  kernel reports `allow.mlock`.
- **Quota:** `refquota` set on the root; writing past it in the jail
  fails with `EDQUOT`; a snapshot does not count against it.
- **Snapshots:** taken running and stopped; rollback of a marker file;
  `destroy_newer` names and destroys exactly the newer snapshots.
- **`started_once`** (Decision 16): rollback of a started jail to a
  snapshot taken before its first start; an SSH key update is still
  `container_started`.
- **Clones** (if Q1 is answered in time): a clone gets a new MAC and its
  own DHCP lease; origin snapshot and source delete refused with
  `snapshot_has_clones`; the clone's host keys equal the source's (as
  Decision 3 says).
- **Permissions:** viewer refused `container:snapshot` routes, audited.
- Clean-up: no test containers, jails, epairs, RCTL rules, or datasets
  left.

### Not run on hearst

- **RACCT off** (`rctl_unavailable`): needs a reboot without
  `kern.racct.enable`, not approved. Unit tests only.

## 3. Built (2026-11-17)

- `188c8d7` **feat(jails): RCTL limits and the disk quota.** As planned
  in section 2, items 1 to 3. `models.ContainerLimits` (`memory`,
  `cpu_percent`, `max_processes`, `open_files`, `disk_quota`) on create,
  update, and the container, stored in `containers.config` (no
  migration). `hostops/jailrctl.go`: `rctl -a` after `jail -c`, read back
  with `rctl jail:skep-<id>` and compared before the parameter check, the
  `/dev` guard, and `/etc/rc`; rules removed after every `jail -r` and
  before every start and delete (list first: `rctl -r` exits 1 when
  nothing matches, and `rctl -l` only works for processes, both found by
  the read-only probe). `refquota` set at create and update
  (owner-checked; a new `refquota` column in the fixed `zfs list`
  columns). `rctl_unavailable` (409) on create, update, and start;
  `mlock` accepted with a memory limit and RACCT. `GET /system` has
  `containers: {rctl, max_cpu_percent}`. The fake mirrors it. Unit tests:
  `internal/hostops/jailrctl_test.go`, `internal/containers/limits_test.go`.
- `a3ef267` **fix(hostops): read one- and two-byte sysctls.** Found on
  hearst: `kern.racct.enable` is a one-byte bool on 15.1, so reading it
  failed ("unexpected integer size of 1 bytes"), the capabilities were
  missing, and every start would have failed. Native test
  `TestSysctlWidthsNative` passed on hearst.
- On merrick: `go build`, `go vet` (Linux and FreeBSD), `go test ./...`,
  `make test` (race detector), and the pre-push `make lint` all passed.
- **Not built** (I stopped first, section 5): jail snapshots, rollback,
  clones, their routes, OpenAPI, and docs.

## 4. Validation on hearst (`a3ef267`, 2026-11-17)

Through the deployed agent and API as `s7oper` and `s7viewer`, with
curl verifying the certificate pinned against `skep-api tls fingerprint`.
Test jail `s9lim` from `minimal-151`, no NIC.

| Check | Result | Record |
|---|---|---|
| Deploy | `188c8d7`, then `a3ef267` with `deploy-test.sh` | [deploy](step9-deploy-188c8d7.txt), [deploy](step9-deploy-a3ef267.txt) |
| Capabilities | `containers: {"rctl":true,"max_cpu_percent":400}` (missing before the fix) | [cleanup-end](step9/cleanup-end.txt) |
| `mlock` without a memory limit | 422 `mlock_requires_memory_limit` | [limits](step9/limits.txt) |
| `cpu_percent` 401 on 4 CPUs | 400 `invalid_request` | [limits](step9/limits.txt) |
| Rules while running | exactly Skep's four (`memoryuse:deny=268435456`, `pcpu:deny=50`, `maxproc:deny=40`, `openfiles:deny=256`), no others on the host; `allow.mlock` on; `/dev` guard passed (allow-list entries only) | [limits](step9/limits.txt) |
| **`memoryuse` 256 MiB** | **Not enforced.** The jail held 514 MiB resident (`memoryuse=539623424`) for 19 s; nothing refused, nothing paged out, host swap 0. See section 5. | [memoryuse](step9/memoryuse.txt) |
| `vmemoryuse` evaluation | Refuses at allocation (`dd: input buffer: Cannot allocate memory`), but counts address space: at 256 MiB even a 128 MiB buffer fails; a 64 MiB buffer is 76 MB resident and 229 MB of address space. A rule added by hand on the test jail and removed afterwards | [vmemoryuse-eval](step9/vmemoryuse-eval.txt) |
| `pcpu` 50 | Throttled: samples alternate 28/74 (mean 56); the loop ran at 61 % of the host's unthrottled rate | [pcpu](step9/pcpu.txt) |
| `maxproc` 40 | 39 processes, then `Cannot fork: Resource temporarily unavailable` | [maxproc-openfiles-quota](step9/maxproc-openfiles-quota.txt) |
| `openfiles` 256 | `tail: f1: Too many open files` (241 of 300 refused) | same |
| `refquota` 512M | Random data stopped at 434 MiB written, `referenced` = 512 MiB; freed on `rm` | same |
| Stop | no RCTL rule left on the name or on the host; `jail_rctl_removed` logged | [limits](step9/limits.txt) |
| Delete | dataset destroyed and listed in the result | [limits](step9/limits.txt) |

Not run: RACCT off (needs a reboot; unit tests only). Snapshots,
rollback, `started_once`, and clones: not built.

## 5. STOP: the memory limit needs your decision (Q4)

Decision 11: "If it can [allocate past the limit] ... that is recorded
... and `vmemoryuse` deny ... is evaluated as the memory limit, or as a
second limit beside it, before the feature is called verified." It can,
so the choice is yours. **Q4. What should `limits.memory` be?**

- **A. Keep `memoryuse` deny, documented as soft.** It is accounted
  (`rctl -u`, monitoring) but not a hard cap: a jail exceeds it while the
  host has free memory. The pageout daemon may push an over-limit jail
  out under memory pressure (not tested: it needs pressure on hearst's
  6 GiB). The UI and docs would say "resident memory target, not
  enforced at allocation". Not "verified" as a limit.
- **B. Use `vmemoryuse` deny instead.** Hard, at allocation. But it caps
  address space, not memory: about 3x resident for `dd` here, and much
  more for programs that reserve address space up front (Go, Java,
  jemalloc arenas). Operators must size it well above the memory they
  mean; the field would be `address_space`, not `memory`.
- **C. Both, as two fields:** `memory` (`memoryuse`, soft, documented)
  and `address_space` (`vmemoryuse`, hard, optional).
- **D. No memory limit in 7b** (CPU, processes, files, quota stay). The
  `mlock` opt-in is then never accepted.

Related, for `mlock`: RCTL has `memorylocked`, which caps locked memory
directly. Decision 9 ties `mlock` to a memory limit; tying it to a
`memorylocked` limit instead would make the opt-in safe whatever Q4
decides. Built now as the ADR says (memory limit required).

I recommend C plus `memorylocked` for `mlock`. Nothing beyond the
limits piece is built until you answer.

## 6. Open questions for Andy

- **Q1. Permission for `container.clone`.** Decision 14's table puts
  clone under `container:write` only. For VMs the same table says "clone
  needs `vm:write` as well" as `vm:snapshot`, and a clone from current
  state creates a snapshot that is left behind.
  Options: **A.** `container:write` only, as the table reads.
  **B.** `container:write` and `container:snapshot`, as for VMs.
  Built-in roles behave the same either way (operator has both, viewer
  neither); only custom roles differ. I will build the clone without its
  route and stop there if this is open.
- **Q2. Jail usage in `/metrics`** (Decision 15: per-jail CPU, memory,
  and process count from `rctl -u`, and disk used). Not named in step 9
  or 10 of the implementation order. Options: **A.** step 9 (it reads
  the same RCTL state), **B.** step 10 with the UI. Not built until you
  say.
- **Q4.** The memory limit, section 5.
- **Q5. A test fix outside step 9, for approval.** `fixture.create` in
  `internal/containers/service_test.go` asserts that the create reply is
  `provisioned: false`, but the task can finish before the reply reads
  the row. Under CPU load the package fails about 1 run in 8 on the base
  commit and about 4 in 8 with step 9 (more tests use the helper). The
  full `go test ./...` failed 3 tests this way once. Proposed fix: drop
  the `Provisioned` part of that assertion (the reply is a snapshot).
  Not changed.
- **Q6. CLAUDE.md is out of date** on hearst's size: it says 2 cores and
  2 GB; hearst has 4 CPUs, 6 GiB, and 4 GiB swap. Not changed.
- **Q3. RACCT-off live check.** Unit tests only unless you approve a
  reboot of hearst with `kern.racct.enable` removed from `loader.conf`
  and put back afterwards (two reboots).

## 7. hearst's state at the stop

`a3ef267` deployed (undo: redeploy `79620ef`); `skep_agent` and
`skep_api` running; `/api/v1/health` ok. No containers, jails, epairs, or
RCTL rules; bridge0 has vtnet1 only; `skeptest/skep/jail` empty. No files
left in hearst's `/tmp`. Templates and the users s7admin, s7oper,
s7viewer unchanged. [step9/cleanup-end.txt](step9/cleanup-end.txt).

## 8. Andy's answers (2026-11-17), recorded as amendment j

Recorded in ADR 0010 as amendment j before any further work.

- **Q4: option A.** Keep `limits.memory` (`memoryuse` deny), labelled
  soft in the API docs and the UI. No `address_space` (`vmemoryuse`) in
  7b; backlog. `mlock` is tied to an RCTL `memorylocked` limit instead of
  the memory limit. Before the docs: one longer `memoryuse` run (about
  3 minutes holding twice the limit, watching `rctl -u` and whether pages
  are swapped out), and the docs say exactly what the limit does.
- **Q1: option B.** A jail clone needs `container:write` and
  `container:snapshot`, as for VMs.
- **Q2:** jail usage in `/metrics` goes in step 9 if it is a read-only
  exposure of `rctl -u`; if bigger, backlog after 7b. Assessment owed.
- **Q3:** no reboots; the RACCT-off live check goes on the
  pending-FreeBSD-validation list.
- **Q5:** approved; fix the `fixture.create` helper.
- **Q6:** approved; fix CLAUDE.md (hearst: 4 CPUs, 6 GiB RAM, 4 GiB
  swap).
- **Also to answer and record:** why only 59 of 300 opens succeeded under
  `openfiles` 256, and in the docs that `refquota` includes the
  template's referenced data and that `pcpu` is approximate.
- **Rules from here:** no hand-made changes on hearst (the hand-added
  `vmemoryuse` rule of section 4 was one) and no `curl -k`/unverified
  fetches (section 4's clean-up had one); such steps are listed for
  approval instead. Snapshots and clones are not built yet.

## 9. After the answers (2026-11-17)

**Built:** `limits.locked_memory` (RCTL `memorylocked` deny), and `mlock`
now needs it and RACCT instead of the memory limit (`4c7d8e0`); the
`fixture.create` fix (Q5, `ec86060`; 12 of 12 package runs passed under
load); CLAUDE.md (Q6, `f93a3e0`); the limits in `api/openapi.yaml`
(`ContainerLimits`, `ContainerCapabilities`, `System.containers`), the
regenerated `web/src/api/schema.d.ts`, and a Limits section in
`docs/administration/jails.md`. `memory` is labelled soft in both. There
is no container UI yet, so the UI label is owed with step 10's UI.

**Validated on hearst** (`4c7d8e0`, through the deployed API, pinned
certificate). My first requests reached the old services: deploy-test.sh
installs but never restarts, and I had not restarted them. They were
restarted, the stray container deleted, and every check repeated; both
are in the records.

| Check | Result | Record |
|---|---|---|
| Deploy | `4c7d8e0`, services restarted | [deploy](step9-deploy-4c7d8e0.txt) |
| `mlock` with only `memory` | 422 `mlock_requires_memory_limit` | [limits-locked](step9/limits-locked.txt) |
| `locked_memory` over `memory` | 400 `invalid_request` | same |
| Rules with `mlock` | `memoryuse`, `openfiles`, and `memorylocked:deny=33554432` exactly; `allow.mlock` true; none left after stop | same, [cleanup](step9/cleanup-4c7d8e0.txt) |
| **`memoryuse` 256 MiB, 512 MiB held for 3 min** | Never refused. Flat at 514 MiB for about 80 s, then fell about 0.85 MiB/s to 425 MiB at 185 s. No swap used, no pageouts: the pages moved from active to inactive in RAM (vm_daemon's RACCT deactivation). Soft, as documented | [memoryuse-long](step9/memoryuse-long.txt) |
| **Why 59 of 300 opens** | RACCT `openfiles` charges each process's descriptor-table size (40, then 128, then 256 slots; never shrinks), not open descriptors; idle, cron is charged 40 with 5 open. `tail` opens files through a Capsicum helper (`system.fileargs`) whose table is charged too. At the 60th file tail needs a 128-slot table: 40 + 128 + 128 = 296 > 256. Nothing else held ~200 files | [openfiles-analysis](step9/openfiles-analysis.txt) |

`memorylocked` is applied and read back, but whether it refuses an
`mlock` past it was **not tested live**: nothing in the minimal template
calls mlock(2). Testing it means copying a small test program (a static
FreeBSD binary that mlocks N MiB) into a test jail's `/tmp`, which is a
hand-made change: **listed for approval** (undo: it goes with the
jail's deletion).

**Q2, jail usage in `/metrics`: it is the read-only kind, so step 9.**
What it takes: one typed read operation in hostops (`rctl -u
jail:skep-<id>`, parsed, only for running jails and only with RACCT on);
a `containers` list in the metrics sample beside `vms` (CPU percent and
CPU time, resident memory, processes, open files from RCTL, and disk
used and referenced from the ZFS inventory the sampler already reads);
the model, OpenAPI, and tests. No host change and no new privilege; I
estimate 200 to 300 lines with tests. Not built: you asked which first.

**Pending FreeBSD validation:** RACCT off, live (Q3: no reboots);
`memorylocked` refusing `mlock` (above, needs approval); static
addressing (amendment i). Spoofing stays deferred.

**hearst now:** `4c7d8e0` deployed and running (undo: redeploy
`a3ef267`, or `79620ef`, then restart both services); health ok; no
containers, jails, epairs, or RCTL rules; bridge0 has vtnet1 only;
`skeptest/skep/jail` empty. hearst's `/tmp` has four old
`skep-adminprompt*`/`skep-api-test*` directories from 2026-11-12 to
11-14, not from step 9; left alone.

## 10. Continued run (2026-11-17): STOP, a clone decision is needed (Q7)

**Built and pushed** (on merrick: `go vet` Linux and FreeBSD, `go test
./...`, `make web-check`, and the pre-push `make lint` passed; one
unrelated flake, below):

- `5a779a4` **fix(scripts): restart the services after a test deploy and
  verify the running build.** `deploy-test.sh HOST` now stops skep_api and
  skep_agent, starts them again, and succeeds only when each running
  process's text inode (`fstat`) is the installed file's and that file's
  `-version` names the checkout's commit. A checkout with uncommitted
  changes is refused. Run on hearst: both services restarted and verified
  at `5a779a4`; the refusal checked too
  ([deploy](step9-deploy-5a779a4.txt)).
- `2595432` **docs(jails):** `open_files` counts file-table slots (40,
  then 128, then 256, never shrinking), with a sizing rule: per process at
  least 40 or about twice the most files it keeps open, summed, plus half
  again.
- `2c12a76` **feat(monitoring): jail usage in `/metrics`** (Q2). Each jail
  with its root's `disk_used_bytes`/`disk_referenced_bytes` and, running
  with RACCT, `cpu_percent`, `cpu_time_seconds`, `resident_memory_bytes`,
  `processes`, `open_file_slots` from `rctl -u jail:skep-<id>`. Read only.
  The read probe showed `rctl -u` prints zeros (exit 0) for a jail name
  that does not exist, so `hostops.JailUsage` checks `jls -j` before and
  after the read. **One change for you to confirm:** `GET /metrics` now
  also requires `container:read` (it names jails, as it already required
  `vm:read` and `storage:read`); viewer and operator have it, so neither
  built-in role changes. Not deployed to hearst yet.

**Not built:** jail snapshots and clones (item 4), and the hearst
validation (item 5). I stopped at the clone design before writing either.

### Q7: a jail clone's NICs would not come up

A jail's NIC is an epair whose jail end is renamed on the host to
`sj<first 10 hex digits of the container ID>n<slot>` before
`vnet.interface` moves it into the jail (amendment h; the name must be
unique on the host while it is there). Skep's `/etc/rc.conf` in the
jail's tree names that interface (`ifconfig_sj…n0="SYNCDHCP"`, or the
static address). A clone has a new container ID, so its NICs get new
names, but its tree is the source's, and its rc.conf names the source's
interfaces. Decision 3 says Skep does not rewrite a clone's tree, and
Decision 13 forbids writing into a tree that has run code (a clone of a
started jail always has `started_once = 1`). Built as the ADR reads, a
clone of any jail with NICs would boot with its NICs unconfigured. VM
clones do not have this problem, because the guest names its NICs
(`vtnet0`) independently of Skep's IDs.

Read-only probe on hearst: `ifconfig -j <jail>` exists on 15.1 and runs the
host's ifconfig inside a vnet jail's network stack ("even if the
ifconfig binary is not available inside the jail"), so an interface can
be renamed inside the jail without running any of the jail's code.

| Option | What changes | Cost and risk |
|---|---|---|
| A. A clone keeps its source's in-jail names: each container records a "guest interface prefix" (its own ID's for a new jail, inherited by a clone). The epair's jail end is still named by the container's own ID on the host (unique there), and after `jail -c`, before the `/dev` guard and `/etc/rc`, Skep renames it inside the jail with `ifconfig -j skep-<id> <host name> name <guest name>` | The prefix goes in `containers.config` (no migration); one fixed-argument command per NIC in the start path, checked like the other steps; Decision 3 and 13 hold unchanged | A new step in every start (live validation needed); in-jail names no longer tell which container a jail is |
| B. Rewrite Skep's own rc.conf in the clone's tree at clone time, with the clone's names | Small: `ConfigureJail` on the clone | Writes into a tree that has run code: an exception to Decision 13, and the opposite of Decision 3's "Skep does not rewrite them". Not recommended |
| C. Fixed in-jail names for every jail (`skep0`, `skep1`, … by slot), renamed inside the jail as in A | rc.conf lines use the fixed names; the same `ifconfig -j` step as A; no prefix to store | Changes amendment h's naming for all jails, existing ones included: a jail already started has the old names in its rc.conf, and Skep may not rewrite it (none exist outside hearst's test runs) |
| D. Build clones now but refuse to clone a jail with NICs (`storage_feature_unsupported`, saying why), and decide A or C later | Clone works for jails without NICs only | A half feature; the decision is only postponed |

My recommendation is C, because it makes a jail's network configuration
independent of its identity, as a VM's is. It costs one rename per NIC
at start. If you would rather not touch existing jails' naming, A is the
next best. Either way the `ifconfig -j` step needs live validation on
hearst. Two related points need your answer whichever option you pick:

- **Static addresses.** A clone copies its source's addressing (as
  planned in section 2). With a static address, the clone and the source
  then hold the same address, so running both conflicts. The options are
  to document it (as for VM clones, whose guests keep their addresses), to
  refuse starting a jail whose static address a running jail holds, or to
  require new addressing in the clone request.
- **Snapshots don't depend on Q7.** If you agree, I can build and
  validate jail snapshots (create, rollback, delete, `container:snapshot`)
  and the remaining hearst checks (`memorylocked` with the test program,
  `/metrics`) while you decide, and leave only the clone for after.

### Other findings

- **A flake in `internal/templates`:** `TestCreateAndDeleteTemplate`
  failed once in the full run (`created.Template.Provisioned` was already
  true: the task finished before the create response was read, the same
  race as Q5's `fixture.create`). It passed in 5 of 5 reruns and 3 of 3
  without my changes. Not fixed: it is outside this run's scope. The fix
  is the same as `ec86060` (don't assert the flag in the create
  response).

**hearst now:** `5a779a4` deployed by the new script, both services
restarted and verified (undo: redeploy `4c7d8e0`). `5a779a4` differs from
`4c7d8e0` only in `deploy-test.sh` and the results files, so the running
code is the same. No containers, jails, epairs, or RCTL rules were
created in this run. The only commands run on hearst were the deploy,
`fstat`, `procstat`, `stat`, `rctl -u` on a nonexistent jail and on cron's
process, and `man ifconfig`.

## 11. Andy's answers (2026-11-17), recorded as amendment k

Recorded in ADR 0010 as amendment k before any further work.

- **Q7: option C.** Every jail uses fixed in-jail interface names by slot,
  `skep0`, `skep1`, and so on, renamed after the vnet move with
  `ifconfig -j` (the host's binary; no jail code runs). Host-end names and
  descriptions stay ID-based, and orphan detection keys on the host end.
  On a normal stop each jail end is renamed back (or its epair destroyed)
  before the jail is removed. Skep's rc.conf lines use `skepN`.
  **Before building:** probe on hearst what 15.1 does when a jail is
  removed abnormally and two returned interfaces are both named `skep0`,
  and confirm orphan detection still identifies them. If that is unsafe,
  stop and report.
- **Static addresses on clones:** the clone request must give new
  addressing for every static NIC, or it is refused with 422. A start is
  also refused when the jail's static IP is already held by a running Skep
  jail on the same bridge.
- **`/metrics` requiring `container:read`:** confirmed.
- **Flake:** approved; apply the Q5 helper fix to
  `TestCreateAndDeleteTemplate`.
- **Docs:** clones share the source's SSH host keys (regenerate them
  inside the clone). An opt-in host-key reset on clone goes in the
  backlog.

Stopped after recording, as asked. Nothing built yet; hearst unchanged
since section 10 (`5a779a4`, no jails, epairs, or RCTL rules).

## 12. Amendment k probe (2026-11-17): safe, build continued

Run on hearst with sudo, as approved (item 1 of the run). Two epairs on
no bridge with no address, host ends described `skep:<test-id>:0`, and
empty vnet jails (`path` an empty 0700 directory in `/tmp`, `persist`,
nothing executed in them). Records: [probe-k](step9/probe-k.txt),
[probe-k2](step9/probe-k2.txt).

| Question | Result on 15.1-RELEASE-p6 |
|---|---|
| `ifconfig -j <jail> sj…n0 name skep0` | Works; prints `skep0`; two jails can each have `skep0` |
| Two jails removed without renaming back | Both `jail -r` exit 0; the host then has **two interfaces named `skep0`** (`ifconfig -l` lists both); nothing fails and nothing is lost |
| Host ends after that | Unchanged: `epair0a`/`epair1a`, descriptions intact, so orphan detection (host ends by description; `skep0` never matches `epair[0-9]+a`) still finds both owners |
| Destroying each host end | Removes exactly its own peer `skep0` |
| `skep0` already on the host when another returns | Return succeeds; two `skep0` again |
| Normal stop, renamed back first | Returns as `sj…n0`, destroyed with its host end |
| `jail -r -f <conf>` with `vnet.interface` after the in-jail rename | Exit 0: jail(8) runs no `-vnet` command at removal (the vnet teardown returns the interface), so a missing name never fails a removal |
| Destroying the host end while the jail runs | Removes the jail end inside the jail (seen in the first attempt of probe-k2) |

**My mistake in probe-k2's first attempt:** its jail.conf had
`exec.stop = "/usr/bin/true"`, which the empty root does not have, so
`jail -r` failed and left `probek1` (JID 34, nothing running in it). I
removed it with `jail -r probek1` straight away and reran without
`exec.stop`. Record: [first attempt](step9/probe-k2-first-attempt.txt).

**Verdict:** safe. Duplicate names are possible only after a removal
Skep did not make, and Skep never addresses a returned jail end by name.

**Built choice (within amendment k's "renamed back (or its epair
destroyed)"):** every removal Skep makes destroys the epairs *before*
`jail -r`, rather than renaming back. Reasons: it does not depend on
the jail's interface list, which the jail's root controls (it can rename
`skep0` away, or to `vtnet0`); and no jail end ever returns to the host on
a Skep removal. Cost: the jail's `/etc/rc.shutdown` runs without its NICs
(a rename-back would also take `skep0` away before shutdown).

**Finding, already true before amendment k:** a jail's root can rename
its interface to any name (`vtnet0`, `bridge0`) and, if the jail is then
removed outside Skep, it returns to the host under that name, beside the
real one. Skep's own removals no longer return anything; removals outside
Skep still can. Not fixed; for your judgement.

hearst after the probes: no jails, no epairs, `/tmp` probe files removed.

## 13. Built after the probe (2026-11-17)

- **`skep<slot>` names** (`aa63eee`): rename inside the jail after the
  move-in check, before the `/dev` guard; every Skep removal destroys the
  epairs before `jail -r` (section 12).
- **Templates flake** (amendment k, as Q5).
- **Jail snapshots and clones** (amendments j and k): the tasks
  `container.snapshot.create/rollback/delete` and `container.clone`;
  `hostops.CloneJail`; the routes (`container:snapshot` declared; clone
  needs `container:write` and `container:snapshot`); the static-address
  rules; `container_address_in_use` at start; OpenAPI, the admin guide,
  and the ADR notes.

### Q8: a started jail with a static address cannot be cloned

Amendment k says a clone must give new addressing for every static NIC.
The new address has to go into the clone's `rc.conf`, and Skep writes into
a tree only before it has run code (Decision 13, reaffirmed in amendment
k). A clone inherits its source's `started_once` (a snapshot of a started
jail can hold a tree that ran code), so:

| Source | Clone request | Result |
|---|---|---|
| any, DHCP NICs only | anything | clone works; new MAC, own lease |
| static NIC, never started | no new addressing | `clone_addressing_required` (422) |
| static NIC, never started | new addressing | clone works; Skep writes the clone's rc.conf |
| static NIC, started | no new addressing | `clone_addressing_required` (422) |
| static NIC, started | new addressing | `container_started` (409) |

So a started static jail cannot be cloned at all. Built that way because
it is the only reading that breaks no accepted decision. Options:
**A.** Keep it (documented). **B.** Allow Skep to rewrite only its own
`rc.conf` (the file with its header) in a clone, an exception to Decision
13 (Q7's option B, which you did not choose). **C.** Apply the new address
from the host with `ifconfig -j` after `/etc/rc` (no jail code runs, but
the jail's own rc.conf still names the old address and wins on the next
`service netif restart` inside the jail). My recommendation is A for 7b,
with B in the backlog if you want static clones.

**Also chosen without asking (please confirm):** a clone's hostname
defaults to the clone's name, as on a create (section 2's plan said the
source's hostname); nameservers default to the source's, and are dropped
when no NIC keeps a static address.

## 14. Validation on hearst (`003a33f`, 2026-11-17)

Deployed with `deploy-test.sh` (both services restarted and verified at
`003a33f`, [deploy](step9-deploy-003a33f.txt)). All requests through the
deployed API as `s7oper`/`s7viewer`, curl verifying hearst's certificate
(fetched over SSH, key pin `779cc5ec…` equal to `skep-api tls
fingerprint`); no `-k`.

| Check | Result | Record |
|---|---|---|
| `skep0` inside a DHCP jail | **Passed.** rc.conf `ifconfig_skep0="SYNCDHCP"`; `ifconfig -j` shows `lo0 skep0` with lease 10.79.60.106; the host has only `epair0a` (described) | [skepn-dhcp](step9/skepn-dhcp.txt) |
| `/metrics` on a running jail | **Passed.** `containers[]` with cpu, resident memory, processes, open-file slots, disk; viewer and operator 200; values match `rctl -u` | [metrics-jail](step9/metrics-jail.txt) |
| Snapshot, rollback, delete | **Passed.** running → `crash_consistent`, stopped → `consistent`; rollback without `destroy_newer` 409 `snapshot_not_latest`; with it the newer one destroyed and named; marker restored; `started_once` stays true, SSH key update 409 `container_started`; delete leaves no ZFS snapshot | [snapshots](step9/snapshots.txt) |
| Clone of a DHCP jail | **Passed.** Clone of the running source: new MAC, own lease 10.79.60.107 on `skep0` beside the source's .106; `started_once` inherited; origin snapshot `crash_consistent`, left behind; ed25519 host keys identical (documented); origin snapshot delete and source delete 409 `snapshot_has_clones` | [clone-dhcp](step9/clone-dhcp.txt), [orphans](step9/orphans-skepn.txt) |
| Static-NIC clone | **Passed.** Without new addressing, and with the same IP under another prefix: 422 `clone_addressing_required` (slots [0]), nothing made; with new addressing (source never started) the clone's rc.conf/resolv.conf hold the new address, the source's unchanged. Neither jail was ever started: nothing static on VLAN 60 | [clone-static](step9/clone-static.txt) |
| Permissions | **Passed.** viewer: GET snapshots 200; POST snapshot, rollback, DELETE snapshot, clone 403, each audited `authz.deny` with the permission and route | [permissions](step9/permissions.txt) |
| `memorylocked` refuses mlock | **Passed.** `locked_memory` 32 MiB: 16 and 30 MiB locked (`rctl -u memorylocked=16777216` while held), 33 and 64 MiB refused ENOMEM; root's `RLIMIT_MEMLOCK` unlimited and `vm.max_user_wired` ~4.5 GiB, so the refusal is RCTL's | [mlock-live](step9/mlock-live.txt) |
| Orphans with `skepN` names | **Passed.** kill -9 of the agent, both DHCP jails removed by hand: two `skep0` on the host, host ends still described; restarted agent warned for `epair0a` and `epair1a` (owners named), `found 2`, `/health` degraded; each owner's start destroyed its own leftover, health ok; normal stops returned nothing to the host (`jail_interfaces_destroyed`) | [orphans-skepn](step9/orphans-skepn.txt) |
| Duplicate static IP at start | **Not run** (pending): needs a bridge that is not on VLAN 60, see below | — |

### Finding: a devfs left by a removal outside Skep blocks the delete

Cleanup failed on `c9dhcp`: `zfs destroy` could not unmount its root
("pool or dataset is busy"). The two jails I removed by hand in check 9
(`jail -r skep-<id>`, without Skep's jail.conf) left their devfs mounted
on `<root>/dev`; Skep's later start mounted and its stop unmounted its own
devfs, but the earlier one stayed. Skep does not look for a devfs left on
a jail root at start or delete. Not fixed (off the approved list).
Proposed fix, for your approval: at start (before `jail -c`) and at
delete, when no jail of that name runs, unmount a devfs mounted exactly at
`<root>/dev` of the jail's own dataset (`umount` of that path only,
checked against `mount -p`), and log it as `orphaned_devfs_removed`; plus
a unit test. Possibly also an `orphaned_mounts` part of the health check.

[cleanup](step9/cleanup-003a33f.txt)

### hearst now, and what needs your approval

`003a33f` deployed and running; health ok. No jails, epairs, or RCTL
rules. Left: containers `s9dhcp` (c5d71062…, with snapshot `c9dhcp`) and
`c9dhcp` (340ac09a…), both stopped, each with that leftover devfs mount.
**To finish the cleanup** (a hand-made change, so listed, not run):
`sudo umount /skeptest/skep/jail/340ac09a-3dd5-4a2e-8327-19d6c8625885/dev`
and `sudo umount /skeptest/skep/jail/c5d71062-66f8-4008-862e-b5b01e963245/dev`,
then API deletes of `c9dhcp`, the snapshot, and `s9dhcp`. Undo: none
needed (the mounts serve no jail). Alternatively, build the fix above and
let it clean them up as its own live test.

**Duplicate static IP at start** needs a bridge without an uplink (for
example `ifconfig bridge1 create` with no members, added to `bridges` in
agent.json, agent restarted; undo: `ifconfig bridge1 destroy`, revert
agent.json). That is a network and config change: not done, listed.

**Still pending FreeBSD validation:** duplicate static IP refused at
start; `container_started` for new addressing on a clone of a started
static jail (needs a started static jail); RACCT off (Q3); spoofing and
the OPNsense ICMP rule re-test (deferred before the step 10 merge).

## 15. Andy's answers (2026-11-17), recorded as amendment l

Recorded in ADR 0010 as amendment l before any further work.

- **Q8:** keep refusing clones of a started static jail in 7b. Backlog:
  an opt-in through a Skep-owned `/etc/rc.conf.d` file written with the
  `O_RESOLVE_BENEATH` in-jail writer.
- **Approved defaults:** clone hostname = clone name; nameservers dropped
  when no static NIC remains. Recorded: on a normal stop the epairs are
  destroyed before `jail -r` (shutdown scripts run without NICs).
- **Devfs fix approved,** with requirements: find the mount with
  getfsstat where `f_mntonname` is exactly `<root>/dev` and the type is
  devfs; unmount by fsid (`MNT_BYFSID`), never by a resolved path; run at
  start, delete, and in the start-up sweep; a test that a jail root whose
  `dev` is a symlink leaves the host untouched.
- **hearst cleanup:** no hand changes; the devfs fix cleans up `s9dhcp`
  and `c9dhcp` as its live test.
- **Duplicate static IP test approved,** live only: `ifconfig bridge1
  create` (no uplink, no rc.conf), `bridge1` in agent.json `bridges`,
  192.0.2.0/24. Undo after the test: remove from agent.json, restart,
  `ifconfig bridge1 destroy`.
- **Security-review list (step 10):** duplicate host interface names
  after an abnormal removal; a jail root renaming its interface to a
  host-like name (`vtnet0`).

Stopped after recording, as asked. Nothing built; hearst unchanged since
section 14 (`003a33f`; `s9dhcp`, `c9dhcp` stopped with their leftover
devfs mounts).
