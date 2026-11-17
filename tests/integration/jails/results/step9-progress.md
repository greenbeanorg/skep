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

## 3. Open questions for Andy

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
- **Q3. RACCT-off live check.** Unit tests only unless you approve a
  reboot of hearst with `kern.racct.enable` removed from `loader.conf`
  and put back afterwards (two reboots).
