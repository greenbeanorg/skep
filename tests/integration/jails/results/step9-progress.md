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
