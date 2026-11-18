# Step 10 (UI part): progress log

Unattended run, 2026-11-17, on `feat/m7b-templates` from `668a16c`.
Scope: the Containers section of the web UI (ADR 0010 step 10, UI only).
merrick only: no hearst, no API calls, no logins, no backend changes.

## 1. Survey

- The generated types (`web/src/api/schema.d.ts`) are current
  (`npm run check-types` passes) and hold every container endpoint:
  list, create, get, update, delete, actions, console-log, snapshots
  (list, create, get, delete, rollback), clone, `/metrics` with
  `ContainerMetrics`, `/system` with `containers.rctl` and
  `containers.max_cpu_percent`, templates under `/storage/templates`.
- Permissions: `container:read`, `container:write`, `container:delete`,
  `container:power`, `container:snapshot` (`internal/auth/permissions.go`);
  a clone needs `container:write` and `container:snapshot` (amendment j).
  `/metrics` needs `system:read`, `vm:read`, `storage:read`, and
  `container:read`. Listing templates needs `storage:read`.
- Allowed bridges: only in `System.bhyve.bridges` (the same
  `network.bridges` list; `cmd/skep-agent/main.go`). The UI reads it from
  there for jails too.

## 2. Plan

New `web/src/pages/containers/`:
- `container.ts`: pure helpers (state, limits parsing and checks, error
  hints, clone addressing rules), with unit tests.
- `ContainersPage.tsx`: list, RACCT notice, create button.
- `CreateContainerDialog.tsx`: template, name, hostname, NICs
  (DHCP/static), nameservers, SSH key, options, limits.
- `LimitsFields.tsx`: the limit inputs with their explanations.
- `ContainerDetailPage.tsx`: state, power actions, delete, task progress
  and failure detail, configuration, NICs, limits, usage from
  `/metrics`, console log.
- `ContainerSnapshotsCard.tsx`: snapshots, rollback, delete, clone.
Plus `types.ts` aliases and permissions, `client.ts` methods, the route,
and the navigation entry.

Not in scope (not asked): editing a jail (PUT), templates management.

## 3. Built (2026-11-17)

Screens, all in `web/src/pages/containers/`:

- **Containers list** (`/containers`, `container:read`): name and hostname,
  state, provisioning and clone badges, template and release, addressing
  per NIC, created. "Create container" only with `container:write`. The
  RACCT-off notice when `GET /system` reports `containers.rctl: false`.
- **Create dialog**: built templates only (with their packages, or "from
  base.txz", and a patch-level note); name and hostname; up to 8 NICs
  (bridge from the allow-list, DHCP or static IPv4 with an optional
  gateway; one gateway per jail; `skep<slot>` named); nameservers only when
  no NIC uses DHCP (up to 3); root's SSH key (one line, no options); System
  V IPC and Lock memory options; the six limits. Without `storage:read`
  (needed to list templates) it says so.
- **Limits**, shown at create and on the detail page: memory labelled
  **soft** (resident-memory target, not a cap; pages move to the inactive
  queue); CPU **approximate** (RACCT averages; bound
  `max_cpu_percent`); **file-table slots** with the sizing rule
  (descriptor-table slots, steps 40/128/256, never shrink, casper counts;
  size well above what the programs keep open; at least 64); locked memory
  **tied to `mlock`** (the option needs it and RACCT; not more than
  memory); disk quota (refquota includes the ~78 MiB shared with a minimal
  template). Without RACCT the RCTL fields are disabled and the disk quota
  still works.
- **Detail page** (`/containers/:id`): state, `state_detail`; Start / Stop /
  Restart (`container:power`, by state); Delete (`container:delete`,
  disabled while the jail runs, with the confirmation that every snapshot
  goes too); the task of the last write followed until it ends, with its
  latest progress event, and on failure the error code, message, and a hint
  (`container_start_failed` points to the console log); the console log
  (reloaded when a task ends); configuration (started-once explained);
  network table (in-jail name, bridge, IPv4, MAC, host end); limits;
  a warning when the jail has RCTL limits and RACCT is off; the clone
  identity note on a linked clone.
- **Usage card**: from `GET /metrics` (only asked for when the role has
  `system:read`, `vm:read`, `storage:read`, and `container:read`;
  otherwise it says which are needed): CPU % against the limit, CPU time,
  resident memory against the limit marked soft (with a line when over),
  processes, file-table slots, disk referenced against the quota, disk
  used with snapshots, the sample time. While stopped, or without RACCT,
  it says why only the disk values are there.
- **Snapshots card**: take (`container:snapshot`; crash-consistent note
  while running), roll back (stopped only; an older snapshot needs the
  "destroy newer" checkbox, sent as `destroy_newer: true`; `started_once`
  stays explained), delete (disabled while clones depend on it).
- **Clone dialog** (needs `container:write` **and** `container:snapshot`,
  amendment j): name, hostname (defaults to the name), from a snapshot or
  a new one; the clone identity note (SSH host keys and `/etc/hostid` are
  the source's; regenerate the host keys). A source with static NICs asks
  for new addressing on each (the request is disabled until given); DHCP
  NICs are kept. **A started jail with a static address cannot be
  cloned**: the Clone buttons are disabled and a notice explains that
  Skep writes addressing only into a tree that has never run code
  (amendment l, Q8).

Permission-aware throughout: actions the role lacks are hidden; actions
the state forbids are disabled with the reason. The API decides
everything again; the UI never enforces.

Plumbing: container types and permissions in `web/src/api/types.ts`,
client calls in `web/src/api/client.ts`, the two routes in `App.tsx`, the
navigation entry (was a placeholder) in `navigation.ts`.

## 4. Tests (merrick, 2026-11-17)

- `make web-check`: type-check, generated types current, **92 tests
  passed** (38 new in `web/src/pages/containers/`: helpers 15, create
  dialog 8, detail page 12, list 3).
- `go test ./...`: passed. `make lint`: passed. `vite build`: built (with
  the chunk-size warning; I did not check whether it predates this run).
- Every test runs against a stubbed `fetch`; nothing reached an agent.

## 5. Commits

`3127beb` (this log), `dc5e9d6` (API calls and helpers), `51b0a9e`
(screens), `5449a7b` (tests), `5f09129` (docs: web-ui.md, jails.md), the
README note, and this report. All on `feat/m7b-templates`, pushed; no PR,
nothing merged.

## 6. Open questions for Andy (not decided)

- **Q1, API gap: which templates have sshd.** The API decides with
  `TemplateHasSSHD` (a regular file `usr/sbin/sshd`) but does not expose it,
  so the UI cannot show the SSH key field "where the template has sshd".
  Options: (A) add a `has_sshd` boolean to `Template` (a backend change,
  not in this run); (B) infer it in the UI from `packages` containing
  `FreeBSD-ssh` or from `source: dist`, a guess that can be wrong;
  (C) show the field for every template and rely on
  `container_ssh_unavailable`, which the UI explains. **Built for now:
  (C)**, because it needs no guess and no backend change; the help text
  says the key needs a template with sshd. Your choice decides whether it
  stays.
- **Q2, bridges for jails come from `System.bhyve.bridges`.** It is the
  same `network.bridges` list, but it sits in the bhyve block; a host
  whose bhyve block is absent would show no bridges for jails. Options:
  (A) keep it; (B) add the bridges to `System.containers` or a network
  block (backend change).
- **Q3, editing a jail** (`PUT /containers/{id}`: hostname, options,
  limits, bridge or MAC, the key before the first start) is not in the
  UI; it was not in this run's list. In step 10, or later?
- **Q4, the README's jails row** is stale for steps 8 and 9 (it still says
  no NICs, limits, snapshots, or clones). I changed only its UI fragment.
- **Q5, clone nameservers:** an empty field keeps the source's (the API
  has no way to clear them on a clone); the UI says "The source's, unless
  changed here". Fine as is?
- Template management (build and delete templates) has no UI; the create
  dialog points to `POST /storage/templates`.

## 7. Needs hearst validation (nothing was run there)

Rotate the `s7*` passwords first (step 9 section 16). Then, with the
deployed API and a browser (or Playwright):
- create a DHCP jail and a static one from `minimal-151`; follow the task
  to the end; start, stop, restart, delete;
- a failed start (`container_start_failed`) shows the code, hint, and
  rc's output in the console log;
- take, roll back (with and without newer snapshots), delete snapshots;
  clone a DHCP jail; a never-started static jail's clone with new
  addressing; the refusal for a started static jail;
- the usage card with RACCT on, against `rctl -u`;
- the actions each of `s7admin`, `s7oper`, `s7viewer` sees, and that the
  API refuses what the UI hides;
- that the error hints match the real messages;
- RACCT off (needs a reboot; stays unit-tested only, like Q3 of step 9).

## 8. Andy's answers (2026-11-17)

1. **`has_sshd`** (Q1): approved backend change. Template responses get
   `has_sshd`, computed by the same check the agent uses for
   `container_ssh_unavailable` (one source of truth). OpenAPI and the web
   types are regenerated; the UI shows the SSH key field only when it is
   true. The stopgap (field for every template) is removed. Recorded in
   ADR 0010 as amendment m.
2. **Bridges** (Q2): stay in `GET /system` (`System.bhyve.bridges`).
3. **Container edit** (Q3): yes, in the UI, limited to what
   `PUT /containers/{id}` already supports, with the same permission
   handling (`container:write`; hidden without it).
4. **README jails row** (Q4): approved, update it for steps 8 and 9.
5. **Clone nameservers** (Q5): fine for 7b. The clone dialog pre-fills the
   source's nameservers with an "inherited from source" note. Backlog: API
   clear semantics (empty list vs omitted).

Also asked: what API field the create dialog's "System V IPC" option maps
to, where the ADR approves it, and exactly which jail parameters it sets
(`new` or `inherit`); if anything could set `inherit`, stop and report.
Backlog: the vite chunk-size warning (check whether it predates this run).

### The System V IPC option (answer)

- **API field:** `ContainerCreate.options.sysvipc` (also
  `ContainerUpdate.options.sysvipc`), a boolean; the UI's "System V IPC"
  checkbox sends `options: {sysvipc: true}`.
- **Approved by:** ADR 0010 Decision 9 (accepted): "Opt-ins: `sysvipc`
  (`sysvshm`, `sysvsem`, `sysvmsg` = `new`) and `mlock`"; Decision 9's
  option C names the reason (PostgreSQL needs SysV IPC).
- **What it sets** (`internal/hostops/jails.go`, `JailSpec.sysv()` and the
  jail.conf generator): `sysvshm`, `sysvsem`, `sysvmsg` = **`new`** with
  the option, **`disable`** without it. Those are the only two values the
  function returns. `allow.sysvipc = 0` is always written (the legacy
  parameter that means `inherit` for all three). After every start the
  kernel's values are compared with the spec (`sysvshm/sem/msg` must equal
  `new` or `disable` as asked), and a mismatch removes the jail; the unit
  test `jails_test.go` "sysv" case refuses `sysvshm=inherit`.
- **Can anything set `inherit`?** Not for a container: no code path writes
  `inherit` for the sysv parameters, and the post-start check would refuse
  it. **One adjacent finding, not a container path:** the template build
  jail (step 6, `templates_jail.go`) sets `allow.sysvipc=0` but does not set
  `sysvshm/sem/msg` explicitly, and `checkBuildJail` does not check them;
  they are left to the kernel's default for a new jail, which with
  `allow.sysvipc=0` I expect to be `disable`, **not verified** (it needs
  FreeBSD source or a read-only `jls -n` during a build on hearst). The
  build jail runs only Skep's fixed commands. I did not stop for it,
  because nothing sets `inherit`; you may want the build jail to set
  `disable` explicitly and check it (backend, not in this run).
- **Backlog:** the vite chunk-size warning (checked in section 9).

## 9. Built after the answers (2026-11-17)

1. **`has_sshd`** (`5ad2e3b`, the only backend change): `models.Template`
   has `HasSSHD`; the template service fills it in `decorate` (list and
   get) from `Jails.TemplateHasSSHD`, the same function `checkSSH` uses to
   refuse a key with `container_ssh_unavailable`; false for a template that
   is not built or whose root cannot be read (logged). The agent already
   passes the full host ops (`Jails` included) to the template service.
   `api/openapi.yaml` (required, documented) and `web/src/api/schema.d.ts`
   regenerated. The create dialog shows the key field only when
   `has_sshd` is true and never sends a key otherwise; the stopgap text is
   gone. Go test `TestTemplateHasSSHD` (false, true, list, unbuilt).
2. Bridges: unchanged (`System.bhyve.bridges`).
3. **Edit** (`4925ce2`): an Edit button on the detail page (hidden without
   `container:write`, disabled unless the jail is `stopped`) opens a dialog
   over `PUT /containers/{id}`: hostname, options, and the full limits
   (PUT replaces them; an emptied field removes that limit) at any time;
   before the first start also NICs (add, remove, addressing),
   nameservers, and the SSH key (only with `has_sshd`; empty removes it);
   after the first start only each NIC's bridge, sent with the recorded
   addressing unchanged (the agent's `sameAddressing` check would refuse
   anything else with `container_started`), and the template is not
   fetched. MAC addresses are kept (not editable in the UI, which PUT
   allows; left out deliberately).
4. **README** (`6a9bfae`): the jails row covers steps 8 and 9 and what is
   pending on hearst; the Web interface and "Not yet" rows were stale for
   the same reason and were corrected too.
5. **Clone nameservers** (`a386e3a`): pre-filled with the source's, noted
   "Inherited from source"; an empty field also keeps them (backlog item
   recorded in amendment m).

**Test flake found and fixed** (in `4925ce2`): the create and edit tests
sometimes acted before `GET /system` had returned the bridges, or before
the dialog was open (`user.clear` on an unfocusable element). They now
wait for both. Before the fix, 1 run in 4 failed; after it, 6 of 6 and the
final run passed. The unexplained `make web-check` exit 2 earlier in this
run was most likely the same race (its output was not kept).

**Vite chunk-size warning (backlog):** it predates this run. At
`668a16c` the bundle was 806.7 kB (224.2 kB gzip); now 856.8 kB
(238.1 kB gzip), so the Containers section adds about 50 kB. Code
splitting by route (the xterm console is the obvious candidate) would
fix it; not done.

## 10. Stop report (2026-11-17)

**Checks on merrick (final run):** `make web-check` 103 tests passed;
`go test ./...` 40 packages ok, none failed; `make lint` passed. Nothing
ran on hearst; no API calls, no logins.

**Commits after the answers:** `7f08001` (answers, amendment m), the
sysvipc record, `5ad2e3b` (has_sshd), `4925ce2` (edit), `a386e3a` (clone
nameservers), `6a9bfae` (README), and this report. Pushed; no PR, nothing
merged. ADR 0010 is already Accepted; amendment m records your answer.

**Open for you:**
- The template build jail leaves `sysvshm/sem/msg` to the kernel default
  (section 8, "The System V IPC option"). Make it explicit `disable` and
  check it? (backend, step 6 code.)
- Route-level code splitting for the chunk warning (backlog).

**Needs hearst validation** (in addition to section 7): `has_sshd` on
`minimal-151` (true) and on a template without `FreeBSD-ssh` (false),
against a create with a key; the edit dialog on a never-started jail
(addressing, nameservers, key written) and on a started one (bridge only;
limits from the next start; a quota below the referenced size refused).

## 11. Andy's answers (2026-11-17), added to amendment m

1. **Build jail sysv:** approved backend change. The step 6 build jail sets
   `sysvshm`, `sysvsem`, `sysvmsg` = `disable` explicitly, and they are
   checked against the kernel after `jail -c`, as for ordinary jails (a
   mismatch refuses the build and removes the build jail). Unit test.
   Added to the hearst list: verify the build jail's `sysv*` = `disable`.
2. **Edit dialog:** a save that would remove a limit (an emptied field)
   first asks for confirmation, listing the limits being removed.

## 12. Built after the answers of section 11 (2026-11-17)

1. **Build jail sysv** (`756a1a2`): `buildJailArgs` passes
   `sysvshm=disable sysvsem=disable sysvmsg=disable` to `jail -c`;
   `checkBuildJail` requires all three to be `disable` in the kernel's
   report (a missing value, `inherit`, or `new` is refused), so
   `StartBuildJail` fails and the builder's deferred clean-up removes the
   build jail (covered by the existing `TestBuildJailCheckFailureRemovesJail`).
   Unit tests: the arguments, and the check refusing `inherit`/`new`/absent.
   The jls output recorded on hearst (`testdata/jls-build-jail-15.1.json`,
   15.1-RELEASE-p6) already shows `disable` for all three, so the kernel
   default was `disable` there; the explicit setting makes it not depend on
   that. `TestStartBuildJailChecksParameters` still skips as non-root.
   Docs: jail-templates.md step 7.
2. **Limit removal confirmation** (this commit series): a save that would
   remove a limit lists the limits and asks; "Keep editing" sends nothing.

Checks on merrick: `make web-check` 105 tests passed (3 runs);
`go test ./...` no failures; `make lint` passed; the hostops tests compile
for FreeBSD. Nothing on hearst.

**Hearst validation list, added:** verify the build jail's `sysvshm`,
`sysvsem`, `sysvmsg` = `disable` (`jls -n` during a template build, or the
check passing on a real build), and the edit dialog's removal confirmation
against the real API.

## 13. Security review fix run (2026-11-18, merrick only)

Andy's triage of [security-review-m7b.md](security-review-m7b.md) is
amendment n of ADR 0010 and the review's Triage section, committed and
pushed before any code (`f7fe2fb`). Nothing ran on hearst; no API calls
against it; no deploy; no PR.

### 13.1 Built, per fix

1. **SR-03, MAC addresses** (`4b05dcd`).
   - `hostops.JailMACProblem`: unicast, locally administered (`0x02`),
     not all zeroes. `normalizeMAC` (create, update, clone) refuses a
     rule violation with the new `container_mac_invalid` (422); a
     malformed address stays `invalid_request` (400). hostops `checkNICs`
     applies the same rule.
   - At start: `checkMACs` in `checkAction` (submission and task) refuses
     a recorded MAC that breaks the rules (one stored before them) and one
     a Skep VM holds (`container_mac_in_use`, 409). In `StartJail`, after
     the jail's own leftover epairs are destroyed and before any epair is
     created, `checkMACsFree` reads `ifconfig <bridge>` (its own `ether`),
     each member's `ether`/`hwaddr`, and `ifconfig <bridge> addr` (the
     learnt table), and refuses a MAC found there, saying where.
   - UI: hints for both codes. OpenAPI and jails.md updated.
   - Tests: the rule table; `checkNICs`; `checkMACsFree` (bridge, member,
     learnt on a member, learnt on an epair, free, a vanished member, a
     member named `-j` refused before it reaches ifconfig); create
     (multicast, universal, zero, malformed), update and clone refusals;
     start refusals (on the bridge, held by a VM, a legacy MAC), then a
     good start.
2. **SR-04, locked memory** (`9534d48`, test only). **Stopped; see 13.6.**
   What the fix specified is already in the code: `mlock` without
   `locked_memory` is refused with `mlock_requires_memory_limit` (422,
   `limits.go:36`, tests `limits_test.go:74-76`), the UI requires it
   (`mlockProblem`), and `memorylocked` is its own RCTL rule whenever
   `locked_memory` is set, independent of `memory`
   (`JailRctlRules`), read back with the others. `limits.go:58` only
   checks `locked_memory <= memory` when `memory` is set, which is right.
   I added one test (the rule added and read back with no memory limit,
   and refused when the read-back lacks it). No docs or UI text changed.
3. **SR-06, quarantine** (`63fc44b`).
   - hostops: `discardAfterFailedStart` returns its error; when a refusal
     (the `/dev` guard, parameters, interfaces, RCTL, or a failed
     `jail -c`) is followed by a failed removal, `StartJail` returns the
     new `container_quarantined` (409) with both causes.
   - containers: `start` records such a container `failed` (the state
     already existed in the schema's CHECK and the model; nothing set it
     until now). `observe` leaves `failed` alone, so a GET or a list never
     shows it running. `checkAction` refuses start, stop, and restart with
     `container_quarantined`. Delete accepts it, removes the jail with
     `DiscardJail` (no `exec.stop`) and fails with
     `container_quarantined` if that fails again. `ObserveAll` retries the
     removal for every `failed` container; removed, it is recorded
     `stopped` with a detail saying so and can start again.
   - Other paths checked (13.5): a half-started jail the sweep cannot
     remove is now quarantined instead of observed running; a stop or
     restart task that finds the container still recorded `starting` is
     refused.
   - UI: no power action for `failed`, a "quarantined" notice, Edit
     disabled, Delete enabled; hint for the code. OpenAPI (state
     description, delete, start) and jails.md updated.
   - Tests: hostops (guard refusal plus failed `jail -r`: quarantined,
     no `jexec`, no `exec.stop` in the conf); containers (never running
     across GET and list, start/stop/restart refused, delete failing then
     working without exec.stop, sweep failing then working, the
     half-started sweep failure, the `starting` stop and restart refusal;
     each asserts the fake recorded no removal with exec.stop); UI
     (`powerActions`, the notice and buttons).
4. **SR-09, FIFO** (`3673f7c`). `readSkepFile` opens with `O_NONBLOCK`
   plus `O_RESOLVE_BENEATH|O_NOFOLLOW|O_CLOEXEC`, fstats, and refuses a
   non-regular file before reading; `replaceSkepFile` and
   `removeSkepFile` go through it. `replaceFile` creates its temporary
   with `O_CREAT|O_EXCL`, so it cannot open a FIFO. FreeBSD tests:
   `TestReadSkepFileRefusesNonRegular` (FIFO, directory, symlink, and a
   character device node when `mknod` works; non-root apart from the
   device, each with a 5-second hang guard) and
   `TestWriteJailFilesRefusesFIFO` (root; rc.conf and resolv.conf). They
   compile for FreeBSD; **pending FreeBSD validation**.
5. **SR-11, rollback** (`44bd986`). The route requires `container:write`
   and also `container:snapshot`, like clone. RBAC test with a role that
   has only `container:snapshot` (403, `permission: container:write`,
   audited) and one with only `container:write` (403, `container:snapshot`);
   the operator row added to the role table. OpenAPI
   (`x-skep-permission`/`x-skep-also-requires`), jails.md, ADR 0010's
   permission table. UI hides Roll back without both; two UI tests.
6. **SR-05, docs** (`3b92d13`). jails.md, Limits: "What the limits do not
   cover" (swap, threads, shared memory, ptys, disk I/O, kernel memory)
   and "Limits are optional". Backlog entry below.

### 13.2 Tests (merrick)

- `make web-check`: 3 runs, each 15 files, 111 tests passed.
- `go test ./... -count=1`: 40 packages ok, none failed.
- `make lint` (go vet, FreeBSD go vet, FreeBSD vet with the test-tasks
  tag): passed. The pre-push hook ran it again on every push.
- `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`: compiles.
  The FreeBSD-only tests (SR-09) have not run.

### 13.3 SR-01: comparison and recommendation (probe only, nothing built)

What the code does now: `runToFile` (`exec_freebsd.go:89-120`) opens
`<JailRunDir>/<id>/console.log` `O_APPEND|O_CREATE` and hands the
`*os.File` to `jexec ... /bin/sh /etc/rc` as fd 1 and 2. Go passes an
`*os.File` straight to the child (no copy goroutine), so `cmd.Wait`
returns when `jexec` exits, and every process rc leaves behind keeps a
descriptor on the host file. The file is never truncated. **A second
writer the review did not name:** the jail.conf sets `exec.consolelog`
to the same file (`jails.go:345`, `:382`), so jail(8) itself appends
`/etc/rc.shutdown`'s output there on every `jail -r` with `exec.stop`;
processes rc.shutdown starts live until `jail -r` kills the jail, up to
the stop timeout (at most 300 s). Any fix should cover both.

**(a) A pipe the agent drains into console.log with a byte cap.**
- Daemons that keep the descriptor: while the agent drains (also past the
  cap, discarding), writes succeed and nobody blocks. Once the agent
  closes the read end at the end of the start, a write gets `EPIPE` and
  `SIGPIPE`, whose default action kills the writer; a process that
  ignores SIGPIPE just gets `EPIPE`. A properly daemonized rc.d service
  (stdio on `/dev/null`, `daemon(3)`/`daemon(8)`) is unaffected; a
  `foo &` in rc.local that prints later dies at its first write. If the
  agent stopped reading without closing, writers would block when the
  pipe buffer fills, so it must always drain or close. Keeping the pipe
  open for the jail's life avoids SIGPIPE but makes it depend on the
  agent: an agent restart closes it anyway, so a daemon would die at a
  random agent restart, which is worse than dying predictably.
- Failed-start log in the UI: the console log is the head of rc's output
  up to the cap. To keep rc's last lines (where the error usually is)
  when the output is longer, keep the last 64 KiB in memory and write it
  after a `skep: N bytes discarded` marker at close; the UI's 64 KiB tail
  then shows the real end.
- The 2-minute timeout: `exec.Cmd` must get the pipe's write end as an
  `*os.File` (not an `io.Writer`, or `Wait` waits for EOF, which daemons
  never send); the agent closes its copy of the write end after starting
  jexec, drains in its own goroutine, and stops when rc exits or the
  timeout fires, without waiting for EOF. On timeout the existing
  `jail -r` then kills every writer.
- The sweep: nothing on disk keeps growing, so nothing to sweep; the
  file is truncated (`O_TRUNC`) at each start, which also bounds the
  files existing jails already have.
- Testing: the drain, cap, marker, tail, close-without-EOF and the
  SIGPIPE behaviour are POSIX pipe semantics, unit-testable on merrick
  with `/bin/sh` and a background writer; jexec and real rc.d services
  need hearst.

**(b) Capture only during the start window, then `/dev/null`.** The agent
cannot change another process's descriptors: a file or pipe a daemon
already holds stays what it is. Without running code in the jail (which
the jail controls), "reopen as `/dev/null`" is not available from the
host. In practice (b) is (a) with the read end closed at the end of the
start: the same SIGPIPE/EPIPE for daemons that keep writing, the same
failed-start log, the same timeout handling and tests. It adds nothing
over (a) except a name.

**(c) Keep a file, cap and rotate.** Rotation (rename) does not stop
writers: they keep the old inode, which keeps using space until they
close it. Periodic `ftruncate` bounds the file to roughly one period of
writing at disk speed, not to a size, and jail root can `ftruncate` and
`fchmod` it too. `RLIMIT_FSIZE` on jexec would apply to every file the
jail's processes write (breaking the jail) and jail root can raise it. A
real cap needs a size-limited file system for the run directory (a
`tmpfs` with `size=`, or a ZFS dataset with a quota per jail), which is a
host change, differs between ZFS and UFS roots, and if shared lets one
jail fill it for all (and `jail.conf` lives in the same directory).
Daemons get `ENOSPC` (no kill), and the failed-start log keeps everything
up to the cap. No timeout interaction; the sweep would have to rotate old
files. Hard to test without a FreeBSD file system.

**Recommendation: (a).** Pipe drained by the agent; console.log
truncated at each start; the first **256 KiB** written, the rest
discarded and counted, then a `skep: N bytes of output discarded`
marker and the last 64 KiB (`ConsoleLogMax`) appended at close; the read
end closed when the start ends (rc exited, or the timeout's removal is
done). A minimal jail's rc writes a few KiB, so 256 KiB is two orders of
magnitude of room, and the file can never exceed about 320 KiB per jail.
For the second writer, drop `exec.consolelog` from the jail.conf so
jail(8) gives `exec.stop` its own stdout and stderr, which `jail -r`
already runs through the capped runner (4 MiB in memory, `WaitDelay`
1 s; jail -r kills every jailed process before it returns); log or keep
that output if wanted. Document that a process rc starts that writes to
stdout or stderr after the start gets SIGPIPE. The template builder's
use of `Output` (`templates_pkg.go:58-62`) is lower risk (fixed
commands, no scripts) and could move to the same mechanism. Proposed
hearst checks before and after: the review's `dd` proof (baseline, then
the file stays at the cap and `procstat -f` shows a pipe), and sshd,
cron, and syslogd still running in a minimal jail after the read end
closes.

### 13.4 nlink

Refusing `nlink > 1` in `readSkepFile` is **not worth adding**. The writer
never writes through an existing file: `replaceFile` writes a new file
(`O_CREAT|O_EXCL`) and renames it over the name, so a hard link is
replaced, not followed; `removeSkepFile` unlinks only the name, so the
other link survives untouched. The read is 25 bytes compared with
Skep's header and never returned to anyone. A hard link cannot cross
file systems, and each jail root is its own ZFS dataset, so the other
name can only be inside the jail's own tree. Only a template can plant
one (writes happen before the first start), and a template's own hard
links are ordinary. A refusal would add a failure mode without closing
anything.

### 13.5 Paths checked for rc.shutdown in a jail that never completed a start

- Refusal before rc, removal failed: was SR-06; now quarantined.
- `jail -c` failed and its removal failed: quarantined (same path).
- Agent died mid-start, the sweep's removal failed: was observed running
  (stop would run rc.shutdown); now quarantined.
- A container left recorded `starting` with no task (start ended but its
  outcome was not recorded): a stop or restart task now refuses it; the
  next agent start sweeps it without exec.stop.
- `/etc/rc` failed or timed out: removed with exec.stop, by design
  (decision 8, amendment i): the guard passed and rc ran. If that removal
  fails, the jail is later observed running and can be stopped with
  rc.shutdown; unchanged. Noted, not a quarantine case.
- Restart, delete (non-quarantined), clone, rollback, snapshot, edit:
  none runs exec.stop on a jail whose start did not complete. Edit,
  snapshot, rollback, and clone of a quarantined container are refused
  with `container_not_stopped` while its jail exists; if the jail has
  vanished they proceed (they run nothing in a jail).

### 13.6 Stopped on (not built)

1. **SR-04's upper bound.** The fix as specified exists; the review's
   actual gap is that `locked_memory` itself has no maximum (1 TiB is
   accepted, and the bound is whatever the user asks). Options:
   (i) require `memory` with `mlock` (the review's direction; weak,
   because `memoryuse` is soft and does not bound wiring);
   (ii) cap `locked_memory` at a host figure, for example a new
   `containers.max_locked_memory_bytes` defaulting to 25 % of
   `hw.physmem`, also checked at every start; (iii) (ii) plus a check
   that running jails' `locked_memory` sum stays under
   `vm.max_user_wired`. I recommend (ii); (iii) refuses starts on load
   rather than on configuration, which is harder to explain.
2. **UI MAC validation.** The web UI has no MAC field: create, edit, and
   clone never send one (the server generates `02:` addresses, and an
   edit keeps the slot's). There is nothing to validate; adding a MAC
   input is a new feature. Only the hints were added.

### 13.7 Changes not in the request (for approval)

1. "Present on the target bridge" includes the bridge's learnt address
   table (`ifconfig <bridge> addr`) and members' `hwaddr` as well as
   their `ether`. A device the bridge has not seen is not caught.
2. The start-time DB check covers Skep VMs too. Jail-to-jail duplicates
   cannot be stored (`container_networks.mac_address` is UNIQUE), so for
   "another Skep container" the start check adds nothing beyond the
   index.
3. A MAC recorded before amendment n is refused at start
   (`container_mac_invalid`); an update with a new MAC is needed.
4. hostops `checkNICs` enforces the locally-administered rule as well
   (defence in depth).
5. The half-started sweep failure is quarantined, and a stop or restart
   task of a container still recorded `starting` is refused (13.5).
6. `runDelete` checks `checkDeletable` before the running check (the
   order changed so a quarantined jail is removed first).
7. The `ObserveAll` summary log field `half_started_removed` is now
   `removed_without_exec_stop` (it counts quarantine removals too).

### 13.8 Backlog (from the triage)

- SR-05: default limits for new jails, or limits required per role.
- SR-07: the duplicate-IP check can race between two starts, and covers
  only the same bridge and static addresses.
- SR-08: an interface renamed and re-described by jail root returns to
  the host under that name after a removal outside Skep.
- SR-10: the dist source checks the directory, not `base.txz` and
  `MANIFEST`; hash and extract by one descriptor.
- SR-I1: no per-container ownership; a clone duplicates the source's
  secrets (SSH host keys, authorized key).
- SR-I2: console.log content is the jail's; strip terminal escapes in any
  future `skepctl` console-log command.
- SR-I4: the `/dev` guard is a point-in-time, top-level check.
- SR-I5: the kernel attack surface of a default jail (`/dev/zfs`,
  `/dev/fuse`, `/dev/pf`, raw sockets, BPF for DHCP).
- SR-I6: the build jail can read the host routing table.
- SR-02 is deferred to the OPNsense spoofing tests, not backlog.

### 13.9 Hearst validation list, added (each needs Andy's approval)

- **SR-03:** a create with a universal MAC (`58:9c:fc:...`) and with
  `01:...` is 422 `container_mac_invalid`; start a DHCP jail and read
  `ifconfig <bridge> addr` to confirm the learnt-table format the parser
  expects; then, on the test bridge, an epair made by hand with a
  locally administered MAC as a member (a network change, approval
  first), and a start of a jail given that MAC is refused with
  `container_mac_in_use` naming the member. Also confirm the bridge's own
  address format in `ifconfig bridgeX`.
- **SR-04:** a jail with `locked_memory` and no `memory`: `rctl
  jail:skep-<id>` shows exactly the `memorylocked` rule.
- **SR-06:** a quarantine cannot be provoked on hearst without a host
  devfs change, so: start a test jail whose `/etc/rc.shutdown.local`
  writes a marker file, set its `observed_state` to `failed` with
  `sqlite3` (a DB edit, approval first), and check that the UI shows the
  quarantine notice and no Start/Stop, that the API refuses start and
  stop with 409 `container_quarantined`, and that an agent restart
  removes the jail with no marker written and records it `stopped`;
  repeat with a delete.
- **SR-09:** run the cross-compiled hostops test binary as root:
  `-test.run 'ReadSkepFileRefusesNonRegular|WriteJailFilesRefusesFIFO'
  -test.v`, and record whether the device-node case ran or was skipped.
- **SR-11:** with the `s7*` users (rotated first): the operator can roll
  back; a test role with only `container:read` and `container:snapshot`
  gets 403 `permission: container:write` and sees no Roll back.
- **SR-I3:** inside a test jail, `zfs list`, `zpool list`, and
  `zfs set skep:container_id=x <its dataset>` fail or show nothing.
- **SR-01 baseline** (before any fix): the review's bounded `dd` proof.

## 14. Andy's answers to section 13 (2026-11-18), recorded as amendment o

1. **Approved:** all seven changes of 13.7; no MAC field in the UI; no
   `nlink` refusal.
2. **SR-01: build option (a).** The agent truncates console.log at each
   start and drains a pipe into it: the first 256 KiB, then a
   `N bytes discarded` marker, then the last 64 KiB; the pipe is closed
   when the start ends (success, failure, or timeout). Document the
   SIGPIPE behaviour for daemons that keep stdout. Drop `exec.consolelog`
   from the jail.conf. `jail -r` output goes through the output-capped
   runner; output past the cap is discarded with a marker and must never
   turn a removal into an error (change the runner's strict mode for this
   call, or give it a capped mode).
3. **SR-04: `containers.max_locked_memory_bytes`**, a total across
   running jails, default 25 % of physical memory (read at agent start).
   The API refuses a single `locked_memory` above the total (422). At
   start the agent refuses when this jail's `locked_memory` plus that of
   the jails already running exceeds the total (a new `container_*` code
   with a hint). Docs: bhyve VMs with wired memory draw from the same
   system pool.
4. **Hearst tests of 13.9 approved** for the validation run, on
   conditions: after Andy's Proxmox snapshot; `skep-api` stopped during
   the `sqlite3` edit; the hand-made epair and the test data removed
   afterwards; each change recorded in this log.

No code was written in this session; SR-01 and SR-04 are the next build.

## 15. Amendment o built (2026-11-18, merrick only)

SR-01 and SR-04 as decided in amendment o (section 14). Nothing ran on
hearst; no API calls against it; no deploy; no PR. The code fitted the
amendment; nothing was stopped on.

### 15.1 Built

1. **SR-01, the console log** (`543f769`, `8e4a41b`; docs `d63085e`).
   - `hostops.runToConsoleLog` (new `exec_unix.go`, `//go:build unix`, so
     the tests run on merrick and in the FreeBSD test binary): opens
     `console.log` `O_TRUNC|O_CREATE|O_NOFOLLOW|O_CLOEXEC` 0600, gives
     jexec the write end of an `os.Pipe` as `*os.File` (so `Wait` returns
     when jexec exits, never at end-of-file), closes the agent's copy of
     the write end right after `Start`, and drains the read end in a
     goroutine. Layout (`consoleLog`): the first 256 KiB
     (`ConsoleLogHead`) written as they arrive; beyond that, bytes are
     counted and the last 64 KiB (`ConsoleLogTail`) kept in memory; at
     the end, if anything was dropped, `\nskep: N bytes discarded\n`,
     then the tail. Most the file can hold: 320 KiB plus the marker.
   - When jexec exits (success, failure, or the start timeout's kill),
     the read end gets a 100 ms deadline to collect what rc left in the
     pipe buffer, and is then closed whatever still holds the write end.
     A process rc left running that writes later gets `SIGPIPE`, or
     `EPIPE` if it ignores it.
   - A failed write to `console.log` stops the writing, never the
     draining, and is returned after rc ends, so the start fails (as a
     failed open already did).
   - `exec.consolelog` is gone from the jail.conf. `removeJail` runs
     `jail -r` with the new `EnvCommand.MarkOverflow`: stdout past 4 MiB is
     cut with the same marker line, never an error; stderr was already
     capped at 4 MiB without an error. The output is not kept (a debug
     log line gives its size).
   - `runCommand` takes an overflow mode: `overflowCut` (`Run`, as
     before), `overflowFail` (the template builder's strict mode, as
     before), `overflowMark` (new).
   - `EnvCommand.Output` and `runToFile` are gone (the jail start was
     their only user); `ConsoleLog` replaces `Output`.
   - Docs: jails.md has a "The console log" section (layout, truncation,
     SIGPIPE/EPIPE for late writers, how to redirect a background
     command in `rc.local`, rc.shutdown's output not in the log); the
     security overview and the console-log endpoint in the OpenAPI
     document say the same. The UI's empty-log text no longer mentions
     `rc.shutdown`.
2. **SR-04, the locked-memory total** (`384c6b6`; docs `a634f61`).
   - `containers.max_locked_memory_bytes` in the agent config (sample
     updated; a negative value is refused at load). The agent reads
     `hw.physmem` once at start (`VMM.Capabilities`) and logs the total;
     `containers.MaxLockedMemory` returns the setting when it is above 0,
     else physical memory / 4. If physical memory could not be read and
     nothing is set, the total is 0 (no jail may lock memory) and a
     warning is logged.
   - `checkLimits` (create, update, and before every start) refuses a
     `locked_memory` above the total with
     `container_locked_memory_above_max` (422).
   - The start check: `checkAction` (submission) and `start` (in the
     task, also for a restart) refuse with
     `container_locked_memory_exhausted` (409) when this jail's
     `locked_memory` plus that of every jail the host reports running
     (each counted with its stored value; limits change only while a jail
     is stopped) and of the starts under way passes the total. The task
     reserves its share in an in-memory map, under a mutex, from before
     `MarkStarting` until `StartJail` returns. A jail without
     `locked_memory` is never refused by this check.
   - UI hints for both codes; OpenAPI and jails.md (Limits and
     Configuration), the security overview. The docs say that
     `vm.max_user_wired` is one pool shared with bhyve VMs that wire
     their memory, and that Skep's VMs do not wire theirs in this release
     (no `-S` or `memory.wired` in the bhyve config).

### 15.2 Tests (merrick)

- New, hostops (`exec_unix_test.go`, real `/bin/sh` processes): the
  head/marker/tail layout (9 cases); a write failure that keeps
  draining; a cap hit during a start (6000 lines, about 380 KiB: the file
  is exactly head + marker with the right count + tail, mode 0600); a
  truncated earlier log; a symbolic link at `console.log` refused and its
  target untouched; the pipe closed after the start while a `sleep 30`
  holds it (returns in about 0.1 s); a write after the start, killed by
  `SIGPIPE` by default and `EPIPE` with `trap '' PIPE`, the log not
  grown; a timeout (pipe closed, the output before it kept); a failure
  (`CommandError` pointing at the console log); the three overflow modes
  of `runCommand`; and `removeJail` with shutdown output past the cap
  succeeding (a runner that really runs a shell past the cap in the mode
  `removeJail` asks for). The jail.conf test now also refuses
  `exec.consolelog`.
- New, containers: the default calculation (7 cases, and the fixture's
  16 GiB giving 4 GiB); the 422 at create (with and without `mlock`) and
  update, a value at the total accepted and started, and a total of 0
  when physical memory is unknown; the total at start (refused with both
  figures in the details, exactly full accepted, a jail without locked
  memory, restart, freed by a stop); a running jail not counted against
  itself; the reservation blocking a second start until released.
- New, config: a negative `max_locked_memory_bytes` refused.
- Mutation checks run by hand: `jail -r` without `MarkOverflow` fails the
  shutdown test; dropping the self-exclusion fails the start test (it did
  not until I added the direct check of a running jail).
- Results: `go test ./... -count=1` passed (40 packages with tests);
  `make lint` passed (gofmt, go vet for Linux and FreeBSD, staticcheck;
  the first push was refused by the hook for an unused `overflowCut` on
  Linux, fixed in `8e4a41b` by a test of that mode);
  `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/` compiled;
  `make web-check` passed three times in a row (15 files, 112 tests each).

### 15.3 Changes not in the request (for approval)

1. The portable process code (`runCommand`, `capped`, the new console
   log) moved from `exec_freebsd.go` to a new `exec_unix.go`, so its
   tests run on merrick; `execRunner` stays FreeBSD-only.
2. `EnvCommand.Output` renamed `ConsoleLog`, and `runToFile` removed.
3. A second new code, `container_locked_memory_above_max` (422): the
   amendment names a new `container_*` code for the start refusal only,
   and the single-value 422 needed a code of its own (`invalid_request`
   is 400).
4. The single-value check also runs before every start (it is in
   `checkLimits`), so a jail created before the setting was lowered is
   refused at start with the 422 code.
5. The in-memory reservation of starts under way, so two concurrent
   starts cannot both take the last room.
6. Every `locked_memory` counts toward the total, with or without the
   `mlock` option (it is what RCTL allows the jail). A jail without
   `locked_memory` always passes the start check, even when the running
   jails are over the total after the setting was lowered.
7. Unreadable physical memory with no setting gives a total of 0 (fail
   closed), with a warning; an info line logs the total at agent start.
8. A failed write to `console.log` (after a successful open) fails the
   start, after rc ran, so the jail is removed with `exec.stop`.
9. `console.log` is truncated when rc is about to run, not at the start
   of `StartJail`: a start refused before rc (or `container_not_stopped`
   for a running jail) leaves the previous log as it was.
10. The UI's empty console-log text, and a debug log line with the size
    of `jail -r`'s output.

### 15.4 Open points (not built; for a decision)

1. **`/etc/rc.shutdown`'s output is no longer kept anywhere.** It used to
   go to `console.log` through `exec.consolelog`; now it is capped in
   memory and dropped. Options: (a) keep as built; (b) append it to
   `console.log` after rc's output, through the same head/tail layout
   with its own smaller budget (for example 64 KiB), written by the
   agent after `jail -r` returns. I recommend (b) if the shutdown output
   is wanted for debugging; (a) otherwise.
2. **A failed `jail -r` reports the first line of its stderr**, which now
   includes the jail's `rc.shutdown` stderr, so the task error of a
   failed stop can start with text the jail wrote. jail(8)'s own error
   comes after. Options: use the last line of stderr for `jail -r`, or a
   fixed message plus the exit status. I recommend the last line.
3. **The total is not in `GET /system`**, so the UI cannot check a
   `locked_memory` value before sending it; the 422 and its hint are what
   the user sees. Adding `containers.max_locked_memory_bytes` to the
   capabilities would let the form check it.

### 15.5 Proposed hearst tests (to add to the validation list)

**SR-01:**

- Before deploying: on the current build, the review's baseline proof
  (section 13.9, already approved) so the before/after can be compared.
- The cross-compiled hostops test binary, as an unprivileged user (no
  root needed): `-test.run
  'ConsoleLog|MarkOverflow|ShutdownOutput' -test.v`, which checks the
  pipe, SIGPIPE, and EPIPE behaviour on FreeBSD's own pipes and `/bin/sh`.
- After the deploy, the rendered `jail.conf` of a test jail has no
  `exec.consolelog`.
- A test jail with `( dd if=/dev/zero bs=1m count=50 2>/dev/null; sleep
  600 ) &` in `/etc/rc.local`: after the start task succeeds, `sudo ls -l
  /var/run/skep/jails/<id>/console.log` stays at most about 320 KiB;
  `sudo procstat -f` of the jail's processes shows no descriptor on
  `console.log` (fd 1 is a pipe, or the writer has died of SIGPIPE);
  `df /` unchanged. Stop the jail and remove the test lines.
- A test jail whose `rc.local` prints about 1 MiB in the foreground: the
  console log is head + `skep: N bytes discarded` + tail, and `GET
  .../console-log` ends with rc's last line.
- A minimal jail with sshd: after the start, and again after a minute,
  `sshd`, `cron`, and `syslogd` still run (`sudo jexec skep-<id> ps ax`),
  and SSH to the jail works.
- A test jail whose `/etc/rc.shutdown.local` prints about 8 MiB: the stop
  task succeeds and the jail is gone.
- A second start of the same jail truncates its console log.

**SR-04:**

- The agent's start log line shows `max_locked_memory_bytes` equal to
  `sysctl -n hw.physmem` / 4 (about 1.5 GiB on hearst).
- A create with `limits.locked_memory` above that is 422
  `container_locked_memory_above_max`.
- Two jails with `mlock` and `locked_memory` 1 GiB and 768 MiB (no
  `memory` limit): the first starts and `rctl jail:skep-<id>` shows
  exactly the `memorylocked` rule; the second start is 409
  `container_locked_memory_exhausted` and its jail is never created; after
  a stop of the first, the second starts. Stop and delete both
  afterwards.
- Optional, bounded: `tests/integration/jails/mlocktest` in the first
  jail locks up to its limit and fails past it, watching `sysctl
  vm.stats.vm.v_wire_count` return to its baseline after the stop.

None of these changes hearst's configuration: the SR-04 checks use the
default total, so `agent.json` is not edited.
