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
- Run 3 (26.4 item 2, amendment w): cloning a started static jail without
  addressing gets 422 `clone_addressing_required`; check
  `container_started` first.
- Run 3 (26.4 item 3, amendment w): the API accepts nameservers on a jail
  with no NIC; the UI offers them only with all-static NICs.
- Run 3 (26.4 item 4, amendment w): API design, PUT replace versus merge
  semantics (an omitted `nameservers` is kept).
- Run 3 (26.4 item 6, amendment w): the forced-removal failure line is
  always jail(8)'s own on 15.1; its cleaning is covered only by unit tests.
- Run 3 (26.4 item 7, amendment w): two JSON log records seen once on one
  `daemon.log` line; cause not established.
- Run 3 (26.4 item 10, amendment w): cosmetic `route: ... File exists`
  loopback lines at every jail start.
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

## 16. Andy's answers to section 15 (2026-11-18), recorded as amendment p

1. **Approved:** all ten changes of 15.3.
2. **15.4 point 1, rc.shutdown's output:** append the output `jail -r`
   returns to `console.log` after a `--- shutdown ---` marker, with its
   own budget: 32 KiB head, a `N bytes discarded` marker, 32 KiB tail.
   The agent writes it from memory; a failure to write it never fails
   the removal.
3. **15.4 point 2, a failed `jail -r`:** report the last line of stderr,
   with control characters stripped, capped at 200 bytes, and labelled as
   jail output in the error and the log.
4. **15.4 point 3:** no change in 7b. Backlog: expose the locked-memory
   total in `GET /system` so the UI can pre-check `locked_memory`.

No code was written in this session; points 2 and 3 of this list are the
next build.

## 17. Amendment p built (2026-11-18, merrick only)

Points 2 and 3 of section 16, as decided in amendment p. Nothing ran on
hearst; no API calls against it; no deploy; no PR. The code fitted the
amendment; nothing was stopped on.

### 17.1 Built

1. **rc.shutdown's output in the console log** (`57a7ae9`).
   - `removeJail` runs `jail -r` with `MaxOutput` 32 KiB
     (`hostops.ShutdownLogPart`) and `MarkOverflow`. The marking mode of
     `runCommand` now keeps the first **and the last** `MaxOutput` bytes
     of standard output in memory, with `skep: N bytes discarded` between
     them: the console log's own `consoleLog` writer over a
     `bytes.Buffer`, so the layout is the same and at most 64 KiB plus the
     marker is held. (Before, it kept the first 4 MiB and no tail.)
   - After `jail -r` succeeds, and only when the jail.conf had
     `exec.stop` (a stop, a restart's stop, the removal after a failed
     rc; not a discard of a refused or quarantined jail), the agent
     appends `--- shutdown ---\n` and that output to `console.log`. The
     marker starts on a line of its own (a newline is added first when
     the log does not end with one) and is written even when the
     shutdown printed nothing. The file is opened
     `O_RDWR|O_APPEND|O_CREATE|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC`, 0600, and
     anything but a regular file is refused. A failure is logged as
     `jail_shutdown_log_failed` (container, size, error) and never fails
     the removal.
   - Only standard output is appended: on success the runner returns no
     stderr (see 17.4).
   - `runCommand` takes its options as a `runOpts` struct.
2. **A failed `jail -r` reports the last line of its stderr**
   (`944a3ec`).
   - New `EnvCommand.StderrLastLine` (set only by `removeJail`): the
     runner keeps the **last** 64 KiB of stderr instead of the first
     4 MiB, and a failure's `CommandError.Stderr` is its last non-empty
     line, cleaned by `cleanLine`: C0 controls, DEL, C1 controls, and
     bytes that are not UTF-8 removed, spaces trimmed, cut to 200 bytes on
     a character boundary.
   - `removeJail` logs `jail_remove_failed` (warn) with `exit_code` and
     `jail_output`, and returns the `CommandError` (type and exit code
     kept) with the line as `jail output, last line of stderr: "..."`
     (Go-quoted), or `no jail output on stderr`. The task error of a
     failed stop therefore reads, for example, `jail: exit status 1: jail
     output, last line of stderr: "jail: skep-x: ..."`. A timeout is
     reported as before.
3. **Docs** (`9bf4a0d`): jails.md (the console log section, the stop
   section), the security overview, the OpenAPI console-log and actions
   descriptions (web types regenerated), the `StopJail` interface comment,
   the UI's empty console-log text, and an implementation note in ADR
   0010.

### 17.2 Tests (merrick)

- New or changed, hostops (`exec_unix_test.go`, real `/bin/sh` processes
  through `runCommand`):
  - `TestRunCommandMarkOverflow`: over the budget, exactly head + marker
    with the right count + tail; under the head; between head and head +
    tail (all of it, no marker); `overflowCut` and `overflowFail`
    unchanged; a failure still a failure.
  - `TestStopJailShutdownOutputPastTheBudget`: a shutdown of about
    124 KiB through `removeJail`; `jail -r` run with `MarkOverflow` and a
    32 KiB cap; `console.log` is exactly rc's output + `--- shutdown ---`
    + the first 32 KiB + `skep: N bytes discarded` + the last 32 KiB.
  - `TestStopJailShutdownOutputUnderTheBudget`: the marker layout (rc's
    output ending with and without a newline, an empty log, a silent
    shutdown).
  - `TestDiscardAppendsNoShutdownOutput`: no append without `exec.stop`.
  - `TestShutdownLogWriteFailureStillRemoves`: `console.log` a symbolic
    link (target untouched), a directory, a FIFO (no blocking, 10 s
    guard), and a missing runtime directory: the removal succeeds, `jail
    -r` ran once, `jail_shutdown_log_failed` is logged.
  - `TestRunCommandStderrLastLine`: the last line; trailing blank lines;
    control characters (`ESC`, tab, CR, BEL); a 500-byte line cut to 200;
    the last line after about 124 KiB of stderr (past the 64 KiB kept);
    empty stderr; the default mode still the first line.
  - `TestRemoveJailFailureReportsLastStderrLine`: through `removeJail`,
    with control characters, an over-long line, and empty stderr: the
    exact error text with its label, the `jail_remove_failed` log line
    with `jail_output`, no ESC or CR in the log, `StderrLastLine` set, and
    nothing appended to the console log.
- New, hostops (`hostops_test.go`): `TestCleanLine` (14 cases: escape
  sequences, CR, tab/NUL/DEL/BEL, C1 as UTF-8 and as raw bytes, an
  over-long ASCII line, an over-long multibyte line, a cut that would
  split a character, exactly 200 bytes, empty, only controls, printable
  Unicode kept) and `TestLastLine`.
- Mutation checks run by hand (file restored from a scratch copy each
  time): appending on a discard fails `TestDiscardAppendsNoShutdownOutput`;
  dropping the newline before the marker fails the "no newline" case;
  dropping `StderrLastLine` from `jail -r` fails all three cases of
  `TestRemoveJailFailureReportsLastStderrLine`.
- Results: `go test ./... -count=1` passed (40 packages with tests);
  `make lint` passed (gofmt, go vet for Linux and FreeBSD, staticcheck for
  both); `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`
  compiled; `make web-check` passed three times in a row (15 files, 112
  tests each).

### 17.3 Changes not in the request (for approval)

1. `runCommand` takes a `runOpts` struct instead of positional
   arguments.
2. The marking mode keeps head **and** tail (32 KiB each for `jail -r`)
   instead of amendment o's first 4 MiB; it is what makes the "last
   32 KiB" the real end of the output. `MarkOverflow` has no other user.
3. Nothing is appended for a removal without `exec.stop` (discard,
   quarantine sweep, interrupted start), since no shutdown ran.
4. The `--- shutdown ---` line is written even when the shutdown printed
   nothing, and starts on a line of its own.
5. The append creates a missing `console.log` (0600) and refuses a
   non-regular file, opened with `O_NONBLOCK` so a FIFO cannot block it
   (amendment o's open used `O_NOFOLLOW` only).
6. In the last-line mode the runner keeps the last 64 KiB of stderr, not
   the first 4 MiB, so a large `rc.shutdown` stderr cannot push jail(8)'s
   own error out of the buffer.
7. `cleanLine` also removes bytes that are not UTF-8 (a terminal may read
   a raw `0x9b` as a C1 CSI) and C1 controls, and trims spaces.
8. The label wording (`jail output, last line of stderr: "..."`, `no jail
   output on stderr`), the Go quoting of the line, and the
   `jail_remove_failed` log event name.
9. The UI's empty console-log text now says rc.shutdown's output is added
   at a stop; the `StopJail` interface comment says the same.

### 17.4 Open points (not built; for a decision)

1. **rc.shutdown's stderr is not in the console log.** The runner
   returns only standard output on success; with `exec.consolelog`
   (before amendment o) both streams went to the file. Options: (a) keep
   as built (stdout only; documented); (b) also append the head/tail of
   stderr after the stdout part (the interleaving of the two streams is
   lost either way). I recommend (a) unless shutdown errors are wanted
   for debugging, then (b).
2. **A failed `jail -r` appends nothing**, since the runner returns no
   output with a `CommandError`. Options: keep; or return the marked
   stdout with the error in the marking mode and append it anyway. I
   recommend keeping it until the hearst probe below shows whether a
   failing `rc.shutdown` fails `jail -r` at all.
3. **Unicode format characters are kept** (category Cf, such as U+202E,
   the right-to-left override), since they are not control characters;
   one can make the reported line display misleadingly in a log viewer
   or the UI. Option: strip Cf as well. I recommend stripping them.

### 17.5 Hearst tests for SR-01 and SR-04 (final list, for approval)

Replaces 15.5. The SR-01 baseline of 13.9 (approved under amendment o's
conditions) still comes first; the rest needs Andy's approval. None of
these changes hearst's configuration; the SR-04 checks use the default
total, so `agent.json` is not edited.

**SR-01:**

1. Before deploying: on the current build, the review's baseline proof
   (13.9) so the before/after can be compared.
2. The cross-compiled hostops test binary, as an unprivileged user:
   `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|DiscardAppends|StderrLastLine|LastStderrLine|CleanLine|LastLine' -test.v`,
   which checks the pipe, SIGPIPE, EPIPE, the head/tail layouts, the
   append (link, directory, FIFO), and the stderr line on FreeBSD's own
   pipes and `/bin/sh`.
3. After the deploy, the rendered `jail.conf` of a test jail has no
   `exec.consolelog`.
4. A test jail with `( dd if=/dev/zero bs=1m count=50 2>/dev/null; sleep
   600 ) &` in `/etc/rc.local`: after the start task succeeds, `sudo ls
   -l /var/run/skep/jails/<id>/console.log` stays at most about 320 KiB;
   `sudo procstat -f` of the jail's processes shows no descriptor on
   `console.log`; `df /` unchanged. Stop the jail: the log grows only by
   the shutdown part (at most about 64 KiB plus markers). Remove the test
   lines.
5. A test jail whose `rc.local` prints about 1 MiB in the foreground: the
   console log is head + `skep: N bytes discarded` + tail, and `GET
   .../console-log` ends with rc's last line.
6. A minimal jail with sshd: after the start, and again after a minute,
   `sshd`, `cron`, and `syslogd` still run (`sudo jexec skep-<id> ps
   ax`), and SSH to the jail works. After a stop, the console log ends
   with `--- shutdown ---` and the base system's shutdown lines
   (`Stopping sshd.` and so on).
7. A test jail whose `/etc/rc.shutdown.local` prints about 8 MiB to
   stdout: the stop task succeeds, the jail is gone, and the console log
   is rc's output + `--- shutdown ---` + 32 KiB + `skep: N bytes
   discarded` (N about 8 MiB less 64 KiB) + 32 KiB ending with the last
   line printed.
8. A test jail whose `/etc/rc.shutdown.local` writes one line to stdout
   and one to stderr: after a stop the stdout line follows the marker and
   the stderr line is absent (17.4 point 1, as built).
9. A second start of the same jail truncates its console log (the
   shutdown part is gone too).
10. **Probe, a failing shutdown:** a test jail whose
    `/etc/rc.shutdown.local` ends with `printf '\033[31mfake ok\n' >&2;
    exit 1`. Record whether `jail -r` exits non-zero and whether the jail
    is gone. If the stop fails: the task error ends with `jail output,
    last line of stderr: "..."` with no ESC byte, and the agent log has
    `jail_remove_failed` with `jail_output`. If it succeeds: the stop's
    console-log part has the marker, and the stderr line is absent. Stop
    (or delete) the jail and remove the test lines afterwards.

**SR-04 (unchanged from 15.5):**

1. The agent's start log line shows `max_locked_memory_bytes` equal to
   `sysctl -n hw.physmem` / 4 (about 1.5 GiB on hearst).
2. A create with `limits.locked_memory` above that is 422
   `container_locked_memory_above_max`.
3. Two jails with `mlock` and `locked_memory` 1 GiB and 768 MiB (no
   `memory` limit): the first starts and `rctl jail:skep-<id>` shows
   exactly the `memorylocked` rule; the second start is 409
   `container_locked_memory_exhausted` and its jail is never created;
   after a stop of the first, the second starts. Stop and delete both
   afterwards.
4. Optional, bounded: `tests/integration/jails/mlocktest` in the first
   jail locks up to its limit and fails past it, watching `sysctl
   vm.stats.vm.v_wire_count` return to its baseline after the stop.

### 17.6 Commits

`57a7ae9` feat(hostops): append rc.shutdown output to the jail's console
log; `944a3ec` feat(hostops): report the last line of a failed jail -r's
stderr; `9bf4a0d` docs(jails): document rc.shutdown output in the console
log and the jail -r error line; and this section.

## 18. Andy's answers to section 17 (2026-11-18), recorded as amendment q

1. **Approved:** all nine changes of 17.3, and the final hearst list of
   17.5.
2. **17.4 point 1, rc.shutdown's stderr:** append it after the stdout
   section, under its own `--- shutdown stderr ---` marker line, with a
   32 KiB tail (and the `N bytes discarded` marker when cut). Same
   failure rules: a failed write never fails the removal.
3. **17.4 point 2, a failed `jail -r`:** keep as built (nothing
   appended) until hearst test 10.
4. **17.4 point 3, Unicode format characters:** strip category Cf,
   including the bidi overrides U+202A–U+202E and U+2066–U+2069,
   wherever Skep reports or logs jail output, alongside the existing
   control-character cleanup.

No code was written in this session; points 2 and 4 of this list are the
next build.

## 19. Amendment q built (2026-11-21, merrick only)

Points 2 and 4 of section 18, as decided in amendment q. Nothing ran on
hearst; no API calls against it; no deploy; no PR. The code fitted the
amendment; nothing was stopped on.

### 19.1 Built

1. **rc.shutdown's stderr in the console log** (`a1f4f2b`).
   - New `EnvCommand.StderrTail` (an `io.Writer`, set only by
     `removeJail`): after a **successful** run the runner writes to it
     the last `MaxOutput` bytes of stderr (32 KiB, `ShutdownLogPart`, for
     `jail -r`), preceded by `skep: N bytes discarded` when more was
     written. The same `consoleLog` layout with a head of 0; with no head
     the marker is the first line, without the leading empty line. It is
     kept next to the 64 KiB stderr tail of the last-line mode (an
     `io.MultiWriter`), so the failure line is unchanged. A failure writes
     nothing to it, and a failed write to it is not an error.
   - `removeJail` passes a buffer and appends, after the stdout section,
     `--- shutdown stderr ---\n` and that tail, in the same single write
     to `console.log` and under the same rules (`O_APPEND|O_CREATE|
     O_NOFOLLOW|O_NONBLOCK`, 0600, regular files only; a failure logged as
     `jail_shutdown_log_failed`, now with `stderr_bytes`, and never a
     failed removal). The stderr marker starts on a line of its own (a
     newline is added when the stdout section does not end with one) and
     is written even when stderr was empty. Nothing is appended for a
     discard or a failed `jail -r` (section 18 point 3).
   - `RunEnv`'s options are built by a new `envRunOpts`, shared by the
     FreeBSD runner and the tests' `shutdownRunner`.
2. **Unicode format characters stripped** (`7335875`). `cleanLine` also
   removes category Cf (`unicode.Is(unicode.Cf, r)`), among them
   U+202A–U+202E, U+2066–U+2069, U+200B–U+200F, U+2060, U+FEFF, U+00AD.
   It is the only place Skep cleans jail output for an error or a log
   (the stderr line of a failed `jail -r`, in the task error and in
   `jail_remove_failed`); see 19.4 for the places that have no cleanup at
   all.
3. **Docs** (this commit): jails.md (the console log and stop sections;
   the log's maximum is now about 416 KiB), the security overview, the
   OpenAPI stop and console-log descriptions (web types regenerated), and
   an implementation note in ADR 0010.

### 19.2 Tests (merrick)

- New, hostops (`exec_unix_test.go`, real `/bin/sh` through
  `runCommand`):
  - `TestRunCommandStderrTail`: with and without the last-line mode:
    short stderr whole; past the limit, the marker as the first line with
    the right count and exactly the last 1000 bytes; under the limit, no
    marker; exactly the limit, no marker; one byte over, `skep: 1 bytes
    discarded`; a failure writes nothing and still reports its first or
    last line; a failing writer does not fail the command; stdout
    unchanged.
  - `TestStopJailShutdownStderrPastTheBudget`: about 124 KiB on each
    stream through `removeJail`: `console.log` is exactly rc's output +
    `--- shutdown ---` + 32 KiB + marker + 32 KiB + `--- shutdown
    stderr ---` + `skep: N bytes discarded` + the last 32 KiB; `jail -r`
    run with `StderrTail` and a 32 KiB cap.
- Changed: `TestStopJailShutdownOutputUnderTheBudget` (the stderr marker
  in every case, plus four new cases: stdout and stderr, stdout without a
  final newline, only stderr, and stderr with ESC and U+202E kept raw in
  the console log); `TestStopJailShutdownOutputPastTheBudget` (ends with
  the stderr marker); `TestShutdownLogWriteFailureStillRemoves` (the
  shutdown writes to stderr too); `TestRunCommandStderrLastLine` (a bidi
  case); `TestRemoveJailFailureReportsLastStderrLine` (a U+202E/U+202C
  case, no ESC, CR, or bidi character in the error or the log,
  `StderrTail` set, still nothing appended).
- New or changed, hostops (`hostops_test.go`): `TestCleanLine` (seven Cf
  cases: U+202E, U+202A–U+202E, U+2066–U+2069, other Cf, Cf around
  spaces, only Cf, a cut measured after removal; and a check that no
  control or Cf character survives any case);
  `TestCleanLineRemovesAllFormatCharacters` (every rune of `unicode.Cf`).
- Mutation checks run by hand (files restored from a scratch copy each
  time): dropping the Cf test fails `TestCleanLine`,
  `TestCleanLineRemovesAllFormatCharacters`, `TestRunCommandStderrLastLine`
  and `TestRemoveJailFailureReportsLastStderrLine`; dropping `StderrTail`
  from `jail -r` fails three tests; dropping the newline before the stderr
  marker fails the "stdout without newline" case; keeping the marker's
  leading newline with no head fails two tests; skipping the final write
  of the stderr tail fails three tests.
- Results: `go test ./... -count=1` passed (40 packages with tests);
  `make lint` passed (gofmt, go vet for Linux and FreeBSD, staticcheck for
  both); `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`
  compiled; `make web-check` passed three times in a row (15 files, 112
  tests each).

### 19.3 Changes not in the request (for approval)

1. `envRunOpts` builds `RunEnv`'s options in `exec_unix.go`, used by the
   FreeBSD runner and the tests' runner (the tests duplicated it before).
2. The `--- shutdown stderr ---` line is written even when stderr was
   empty (as the stdout marker is, 17.3 point 4).
3. A newline is added before the stderr marker when the stdout section
   does not end with one.
4. With no head, `consoleLog` writes the discarded marker without its
   leading empty line (only the stderr tail and the last-line buffer,
   which goes to `io.Discard`, have no head).
5. A failed write to `StderrTail` is ignored by the runner: the command
   succeeded.
6. `stderr_bytes` added to the `jail -r done` debug line and to
   `jail_shutdown_log_failed`.
7. Both sections go out in one write.
8. The docs now say the console log is not cleaned (control and format
   characters stay), and its maximum is about 416 KiB (was 384).

### 19.4 Open points (not built; for a decision)

1. **The console log keeps format characters** (and control characters,
   as before). Amendment q's "alongside the existing control-character
   cleanup" was read as `cleanLine`, the only cleanup of jail output; the
   console log has none, and stripping only Cf from it would be
   inconsistent. But the UI shows it in a `<pre>`, where the browser
   applies bidi overrides, and `GET .../console-log` hands it to any
   client. Options: (a) keep (documented); (b) strip Cf (and perhaps C0
   but tab and newline, and C1) when the API serves it, leaving the file
   raw; (c) only in the UI. I recommend (b).
2. **Two other reports of jail-side output have no cleanup at all.** A
   failed command in a template build jail (`RunBuildCommand`,
   `jexec -l` running e.g. pkg inside the build jail) reports the first
   line of its stderr raw in the error; `jail -c`'s first stderr line is
   raw too (jail(8)'s own text, low risk). Option: run both through
   `cleanLine`. I recommend it for the build jail at least.
3. **`skepctl`'s `clean`** maps C0 and C1 to spaces but keeps Cf. Jail
   output reaches it already cleaned; other server text does not.
   Option: map Cf there as well. Low priority.

### 19.5 Changes to the 17.5 hearst list (for approval)

The list was approved under amendment q, but amendment q changes the
expected results of some tests:

- **Test 2:** add the new tests to the filter:
  `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|ShutdownStderr|DiscardAppends|StderrLastLine|StderrTail|LastStderrLine|CleanLine|LastLine' -test.v`.
- **Test 4:** at the stop the log grows by at most about 96 KiB plus
  markers (was 64 KiB).
- **Test 6:** after a stop the console log has `--- shutdown ---`, the
  base system's shutdown lines, then `--- shutdown stderr ---` (and
  whatever rc.shutdown wrote to stderr).
- **Test 7:** the layout ends with the `--- shutdown stderr ---` line.
- **Test 8:** the expectation flips: the stderr line follows `---
  shutdown stderr ---`.
- **Test 10:** if the stop succeeds, the `fake ok` line appears after
  `--- shutdown stderr ---`, raw (with its ESC byte; the console log is
  not cleaned). Proposed: make the line `printf '\033[31mfake
  \342\200\256ok\n' >&2` so a failing stop also shows U+202E removed from
  the task error and from `jail_remove_failed` on FreeBSD.
- **Proposed new test 11:** a test jail whose `/etc/rc.shutdown.local`
  writes about 1 MiB to stderr: the stop succeeds, and the stderr section
  is `skep: N bytes discarded` (N about 1 MiB less 32 KiB) and the last
  32 KiB, ending with the last line written.

### 19.6 Commits

`7335875` feat(hostops): strip Unicode format characters from reported
jail output; `a1f4f2b` feat(hostops): append rc.shutdown's stderr to the
jail's console log; and the docs commit with this section.

### 19.7 Andy's answers (2026-11-21), recorded as amendment r

1. **19.3:** all eight changes approved as built.
2. **19.4 point 1, the console log:** when the API serves it, strip
   Unicode format characters (Cf) and C0/C1 control characters except
   `\n` and `\t`. The file on disk stays as written. Document it in
   jails.md and in the OpenAPI description.
3. **19.4 point 2:** apply the existing `cleanLine` cleanup to
   `RunBuildCommand`'s first stderr line in build errors, and to
   `jail -c`'s first stderr line.
4. **19.4 point 3:** `skepctl`'s `clean` also strips Cf characters.
5. **19.5:** every change to the 17.5 hearst list approved, including
   U+202E in test 10's `printf` and the new test 11. Added:
   - **Test 12:** a test jail whose rc writes U+202E and a C1 character
     to its output. `console.log` on disk keeps them; the API's
     console-log response has them stripped.

No code was written in this session; points 2–4 of this list are the
next build.

## 20. Amendment r built (2026-11-21, merrick only)

Points 2–4 of section 19.7, as decided in amendment r. Nothing ran on
hearst; no API calls against it; no deploy; no PR. The code fitted the
amendment, with one naming difference (20.1 point 2); nothing was
stopped on.

### 20.1 Built

1. **The console log is cleaned when served** (`9471713`). New
   `hostops.CleanText`: removes category Cf and C0, DEL, and C1 controls
   (`unicode.IsControl`) except `\n` and `\t`; not trimmed or cut; a
   byte that is not UTF-8 becomes U+FFFD (`strings.Map`; JSON encoding
   would do the same). `containers.Service.ConsoleLog`, in the agent,
   applies it, so the agent's internal API, `skep-api`'s
   `GET /containers/{id}/console-log`, the web UI, and any client get the
   cleaned text. `console.log` itself is untouched (rc's output, both
   shutdown sections). `truncated` is still measured on the file's
   bytes, before the cleanup. `cleanLine` and `CleanText` share one rune
   test, `unsafeRune`.
2. **The other reports of jail-side output** (`9471713`). The code has no
   `RunBuildCommand`: the command a template build runs in its jail is
   `BuildJailExec` (`jexec -l skep-build-<id> ...`), whose
   `CommandError.Stderr` (the first stderr line) feeds `templates`'
   `template_build_failed` message and its `stderr` detail. New
   `cleanStderr` runs that line through `cleanLine` (control and Cf
   characters and non-UTF-8 bytes removed, trimmed, cut to 200 bytes).
   It is applied to `BuildJailExec` and to both `jail -c` calls: a
   container start's, now in `createJail`, and a template build jail's,
   now in `createBuildJail`. Other commands' first stderr lines (zfs,
   ifconfig, pkg and tar on the host, and so on) are unchanged.
3. **`skepctl`'s `clean`** (`27a51d1`) also removes category Cf (removed,
   not turned into a space); C0, DEL, and C1 still become spaces.
4. **Docs** (this commit): jails.md (the console log section, the
   `jail -c` error), jail-templates.md (`template_build_failed`),
   skepctl.md (text output), the security overview, the OpenAPI
   console-log description (web types regenerated), and an
   implementation note in ADR 0010.

### 20.2 Tests (merrick)

- New, hostops (`clean_test.go`): `TestCleanText` (14 cases: CR, ANSI,
  other C0 and DEL, C1, bidi overrides and isolates, other Cf, an RLO
  file name, no trim or cut, other scripts kept, non-UTF-8 to U+FFFD,
  empty); `TestCleanTextEveryRune` (every rune: control and Cf removed
  except `\n` and `\t`, every other rune kept); `TestCleanStderr` (nil,
  a non-command error, a dirty line, a wrapped command error cut to 200
  bytes on a character boundary); `TestCreateJailCleansStderr` (the
  `jail -c` command line, environment and timeout unchanged; the
  cleaned line also in `startFailed`'s error); `TestCreateBuildJailCleansStderr`;
  `TestBuildJailExecCleansStderr`.
- New, hostops (`exec_unix_test.go`, real `/bin/sh`):
  `TestCleanLineFirstStderrLine`: a failing shell writes ESC, U+202E,
  U+202C, a C1 CSI, and CR on its first stderr line; `runCommand` reports
  it raw, `cleanStderr` leaves `[31mfake okx`, and the error has no
  control or Cf character.
- New, containers (`service_test.go`): `TestConsoleLogIsCleanedWhenServed`:
  the fake host's jails read the console log from a real file; with CR,
  ESC, BEL, U+202E, U+202C, U+2066/U+2069, U+200B, a C1 CSI, NEL, DEL,
  and U+FEFF in it, the response (and its JSON round trip) is exactly
  the expected text with tabs and newlines kept, **and the file on disk
  still holds every byte**; a full-size tail whose last bytes are
  U+202E is `truncated` with 30 bytes fewer text; a short one is not.
- New and changed, skepctl (`main_test.go`): `TestClean` (cases and every
  Cf rune); the hardware warning of the login test now has U+202E too,
  and the text output must have neither ESC nor U+202E.
- Mutation checks run by hand (files restored from a scratch copy each
  time): serving the raw text fails `TestConsoleLogIsCleanedWhenServed`;
  measuring `truncated` after the cleanup fails it too (after a fix to
  the test: its first full-size case had the U+202E outside the tail and
  missed this); dropping `cleanStderr` from `createJail`,
  `createBuildJail`, or `BuildJailExec` fails the matching test; dropping
  tab from `CleanText`'s exceptions fails both `CleanText` tests; dropping Cf from `unsafeRune` fails eight tests;
  dropping Cf from `skepctl`'s `clean` fails `TestClean` and the login
  test.
- Results: `go test ./... -count=1` passed (40 packages with tests);
  `make lint` passed (gofmt, go vet for Linux and FreeBSD, staticcheck
  for both); `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`
  compiled; `make web-check` passed three times in a row (15 files, 112
  tests each).

### 20.3 Changes not in the request (for approval)

1. `jail -c` moved into two helpers, `createJail` (container start) and
   `createBuildJail` (template build jail), so the cleanup is testable
   without root: both `jail -c` call sites sit behind the root-owned
   parent check, which refuses as an unprivileged user before `jail -c`
   runs. Same arguments, environment, and timeout.
2. The **build jail's** `jail -c` is cleaned too: amendment r says
   "`jail -c`'s first stderr line", and section 19.4 named the container
   start's; both are applied.
3. `cleanStderr` uses `cleanLine` as decided, so these lines are now also
   trimmed and cut to 200 bytes (they were uncut first lines before), and
   non-UTF-8 bytes are removed.
4. `CleanText` also removes DEL (U+007F), which is neither C0 nor C1 but
   is a control character to `unicode.IsControl` and to `cleanLine`.
5. Non-UTF-8 bytes in the served console log become U+FFFD in `text`
   (the JSON response would carry U+FFFD for them anyway).
6. The cleanup is in the agent's containers service, not in `skep-api`'s
   handler: one place, and the agent's internal API never hands the raw
   bytes out either.
7. skepctl.md now says how text output treats control and format
   characters (it did not mention it before).

### 20.4 Open points (not built; for a decision)

1. Nothing new from the amendment. Noted only: the first stderr lines of
   the build's host-side `pkg install`, `pkg info --raw`, and `tar -x`
   (`stepFailed`) are still raw. They are host commands reading
   downloaded or repository data, not text the jail writes; cleaning them
   too would be a one-line change in `stepFailed`. Low priority.

### 20.5 Changes to the hearst list (17.5, 19.5, and test 12; for approval)

- **Test 2:** the new hostops tests `TestCleanText`,
  `TestCleanTextEveryRune`, `TestCleanStderr`, `TestCreateJailCleansStderr`,
  `TestCreateBuildJailCleansStderr`, and `TestBuildJailExecCleansStderr`
  do not match the filter (only `TestCleanLineFirstStderrLine` does,
  through `CleanLine`). Proposed: replace `CleanLine` with `Clean` and
  add `BuildJailExec`:
  `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|ShutdownStderr|DiscardAppends|StderrLastLine|StderrTail|LastStderrLine|Clean|LastLine|BuildJailExec' -test.v`.
  None needs root.
- **Test 10:** the "raw, with its ESC byte" expectation now holds for
  `console.log` on disk (`sudo cat -v`) only; `GET .../console-log`
  shows `[31mfake ok` with no ESC and no U+202E. Proposed: check both.
- **Tests 5–9:** unchanged. They read ASCII text, which the cleanup keeps
  (test 7's `skep: N bytes discarded` lines included); if read through
  the API, carriage returns are gone.
- **Test 12:** unchanged; this build is what it checks.
- **Test 11:** unchanged.

### 20.6 Commits

`9471713` feat(jails): clean the served console log and jail-side stderr
lines; `27a51d1` feat(skepctl): strip Unicode format characters in table
output; and the docs commit with this section.

### 20.7 Andy's answers (2026-11-21), recorded as amendment s

1. **20.3:** all seven changes approved as built.
2. **20.4 point 1:** apply `cleanStderr` (`cleanLine`) to the first
   stderr line of the host-side `pkg install`, `pkg info --raw`, and
   `tar -x` failures in template build errors.
3. **20.5:** both changes approved:
   - **Test 2:** the filter becomes
     `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|ShutdownStderr|DiscardAppends|StderrLastLine|StderrTail|LastStderrLine|Clean|LastLine|BuildJailExec' -test.v`.
   - **Test 10:** checks both `console.log` on disk (`sudo cat -v`, the
     ESC byte kept) and the API's console-log response (no ESC, no
     U+202E).

No code was written in this session; point 2 of this list is the next
build.

## 21. Amendment s built (2026-11-21, merrick only)

Point 2 of section 20.7, as decided in amendment s. Nothing ran on
hearst; no API calls against it; no deploy; no PR. The code fitted the
amendment; nothing was stopped on.

### 21.1 Built

1. **The build's host-side stderr lines are cleaned** (`b97480b`).
   `PkgInstall` (`pkg install`), `PkgScripts` (`pkg info --raw`), and
   `ExtractDist` (`tar -x`) in `hostops/templates_pkg.go` return a failure
   through `cleanStderr`, so the first stderr line that `templates`'
   `stepFailed` puts into the `template_build_failed` message and its
   `stderr` detail has no control or format characters, no non-UTF-8
   bytes, and is at most 200 bytes. `tar -x` moved into a new
   `extractArchive` (same arguments, environment, and timeout).
2. **Docs** (this commit): jail-templates.md (the `template_build_failed`
   row), the security overview, and an implementation note in ADR 0010.
   The OpenAPI description does not describe these errors; unchanged.

### 21.2 Tests (merrick)

- New, hostops (`clean_test.go`): `TestPkgInstallCleansStderr` (the pkg
  command line unchanged), `TestPkgScriptsCleansStderr` (no packages
  returned with the error), `TestExtractArchiveCleansStderr` (the tar
  command line, path, environment, and timeout unchanged; a success
  returns nil). Each feeds the dirty line of amendment r's tests (ESC,
  U+202E, U+202C, a C1 CSI, U+200B, CR) and checks the cleaned line and an
  error with no control or format character.
- Mutation checks run by hand (the file restored from a scratch copy each
  time): dropping `cleanStderr` from `PkgInstall`, `PkgScripts`, or
  `extractArchive` fails exactly the matching test.
- Results: `go test ./... -count=1` passed (40 packages with tests);
  `make lint` passed (gofmt, go vet for Linux and FreeBSD, staticcheck
  for both); `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`
  compiled; `make web-check` passed three times in a row (15 files, 112
  tests each).

### 21.3 Changes not in the request (for approval)

1. `tar -x` moved into `extractArchive`, so the cleanup is testable as an
   unprivileged user: `ExtractDist` refuses a dist directory not owned by
   root before `tar` runs (the same pattern as 20.3 point 1).

### 21.4 The hearst list for run 1 (current)

Section 17.5 with every change of 19.5, 20.5, and amendment s applied.
Amendment s changes nothing on it: the three new tests match `Clean` in
test 2's filter (`...CleansStderr`).

**SR-01:**

1. Before the deploy, on the current build: the review's baseline proof (13.9), for the before/after comparison.
2. The cross-compiled hostops test binary as an unprivileged user: `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|ShutdownStderr|DiscardAppends|StderrLastLine|StderrTail|LastStderrLine|Clean|LastLine|BuildJailExec' -test.v`.
3. After the deploy, a test jail's rendered `jail.conf` has no `exec.consolelog`.
4. `( dd if=/dev/zero bs=1m count=50 2>/dev/null; sleep 600 ) &` in `/etc/rc.local`: after the start `console.log` stays at most about 320 KiB, `sudo procstat -f` shows no descriptor on it, `df /` is unchanged; at the stop it grows by at most about 96 KiB plus markers; remove the test lines.
5. `rc.local` prints about 1 MiB in the foreground: the console log is head + `skep: N bytes discarded` + tail, and `GET .../console-log` ends with rc's last line.
6. A minimal jail with sshd: `sshd`, `cron`, and `syslogd` still run after the start and a minute later, and SSH works; after a stop the log has `--- shutdown ---`, the base shutdown lines, then `--- shutdown stderr ---` (and anything rc.shutdown wrote to stderr).
7. `/etc/rc.shutdown.local` prints about 8 MiB to stdout: the stop succeeds, the jail is gone, and the log is rc's output + `--- shutdown ---` + 32 KiB + `skep: N bytes discarded` (N about 8 MiB less 64 KiB) + 32 KiB ending with the last line + `--- shutdown stderr ---`.
8. `/etc/rc.shutdown.local` writes one line to stdout and one to stderr: after a stop the stdout line follows `--- shutdown ---` and the stderr line follows `--- shutdown stderr ---`.
9. A second start of the same jail truncates its console log (the shutdown sections are gone).
10. Probe: `/etc/rc.shutdown.local` ends with `printf '\033[31mfake \342\200\256ok\n' >&2; exit 1`; record whether `jail -r` exits non-zero and the jail is gone. If the stop fails: the task error ends with `jail output, last line of stderr: "..."` with no ESC or U+202E, and the agent log has `jail_remove_failed` with `jail_output`. If it succeeds: `sudo cat -v` of `console.log` shows the `fake ok` line after `--- shutdown stderr ---` with its ESC and U+202E bytes, and `GET .../console-log` shows it with no ESC and no U+202E. Clean up afterwards.
11. `/etc/rc.shutdown.local` writes about 1 MiB to stderr: the stop succeeds, and the stderr section is `skep: N bytes discarded` (N about 1 MiB less 32 KiB) + the last 32 KiB, ending with the last line written.
12. A test jail whose rc writes U+202E and a C1 character to its output: `console.log` on disk keeps them, and the API's console-log response has them stripped.

**SR-04 (unchanged):**

- S1. The agent's start log shows `max_locked_memory_bytes` equal to `sysctl -n hw.physmem` / 4 (about 1.5 GiB).
- S2. A create with `limits.locked_memory` above that is 422 `container_locked_memory_above_max`.
- S3. Two `mlock` jails with 1 GiB and 768 MiB: the first starts with exactly the `memorylocked` rule in `rctl`; the second start is 409 `container_locked_memory_exhausted` and never creates its jail; after the first stops, the second starts; stop and delete both.
- S4. Optional: `mlocktest` in the first jail locks up to its limit and fails past it; `vm.stats.vm.v_wire_count` returns to its baseline after the stop.

Conditions (amendment o): after Andy's Proxmox snapshot; `skep-api` stopped during the `sqlite3` edit; the epair and all test data removed afterwards; every change on hearst recorded in the progress log.

### 21.5 Commits

`b97480b` feat(hostops): clean the stderr line of the build's pkg and tar
failures; and the docs commit with this section.

### 21.6 Andy's answer (2026-11-21)

1. **21.3 point 1:** approved as built (the `extractArchive` helper, and
   `ExtractDist` refusing a dist directory not owned by root before `tar`
   runs).

## 22. Run 1 plan

Hearst validation run 1 of 3 (M7b step 10, ADR 0010). The section below
is copied verbatim from the "Hearst list for run 1" section of
`~/stop-report.md` (the amendment s stop report, 2026-11-21); only its
heading was demoted one level to fit this section.

### Hearst list for run 1 (current)

§17.5 with §19.5, §20.5 and amendment s applied. Amendment s adds nothing: its tests match `Clean` in test 2's filter (checked with `go test -list`).

**SR-01:**

1. Before the deploy, on the current build: the review's baseline proof (13.9), for the before/after comparison.
2. The cross-compiled hostops test binary as an unprivileged user: `-test.run 'ConsoleLog|MarkOverflow|ShutdownOutput|ShutdownLog|ShutdownStderr|DiscardAppends|StderrLastLine|StderrTail|LastStderrLine|Clean|LastLine|BuildJailExec' -test.v`.
3. After the deploy, a test jail's rendered `jail.conf` has no `exec.consolelog`.
4. `( dd if=/dev/zero bs=1m count=50 2>/dev/null; sleep 600 ) &` in `/etc/rc.local`: after the start `console.log` stays at most about 320 KiB, `sudo procstat -f` shows no descriptor on it, `df /` is unchanged; at the stop it grows by at most about 96 KiB plus markers; remove the test lines.
5. `rc.local` prints about 1 MiB in the foreground: the console log is head + `skep: N bytes discarded` + tail, and `GET .../console-log` ends with rc's last line.
6. A minimal jail with sshd: `sshd`, `cron`, and `syslogd` still run after the start and a minute later, and SSH works; after a stop the log has `--- shutdown ---`, the base shutdown lines, then `--- shutdown stderr ---` (and anything rc.shutdown wrote to stderr).
7. `/etc/rc.shutdown.local` prints about 8 MiB to stdout: the stop succeeds, the jail is gone, and the log is rc's output + `--- shutdown ---` + 32 KiB + `skep: N bytes discarded` (N about 8 MiB less 64 KiB) + 32 KiB ending with the last line + `--- shutdown stderr ---`.
8. `/etc/rc.shutdown.local` writes one line to stdout and one to stderr: after a stop the stdout line follows `--- shutdown ---` and the stderr line follows `--- shutdown stderr ---`.
9. A second start of the same jail truncates its console log (the shutdown sections are gone).
10. Probe: `/etc/rc.shutdown.local` ends with `printf '\033[31mfake \342\200\256ok\n' >&2; exit 1`; record whether `jail -r` exits non-zero and the jail is gone. If the stop fails: the task error ends with `jail output, last line of stderr: "..."` with no ESC or U+202E, and the agent log has `jail_remove_failed` with `jail_output`. If it succeeds: `sudo cat -v` of `console.log` shows the `fake ok` line after `--- shutdown stderr ---` with its ESC and U+202E bytes, and `GET .../console-log` shows it with no ESC and no U+202E. Clean up afterwards.
11. `/etc/rc.shutdown.local` writes about 1 MiB to stderr: the stop succeeds, and the stderr section is `skep: N bytes discarded` (N about 1 MiB less 32 KiB) + the last 32 KiB, ending with the last line written.
12. A test jail whose rc writes U+202E and a C1 character to its output: `console.log` on disk keeps them, and the API's console-log response has them stripped.

**SR-04 (unchanged):**

- S1. The agent's start log shows `max_locked_memory_bytes` equal to `sysctl -n hw.physmem` / 4 (about 1.5 GiB).
- S2. A create with `limits.locked_memory` above that is 422 `container_locked_memory_above_max`.
- S3. Two `mlock` jails with 1 GiB and 768 MiB: the first starts with exactly the `memorylocked` rule in `rctl`; the second start is 409 `container_locked_memory_exhausted` and never creates its jail; after the first stops, the second starts; stop and delete both.
- S4. Optional: `mlocktest` in the first jail locks up to its limit and fails past it; `vm.stats.vm.v_wire_count` returns to its baseline after the stop.

Conditions (amendment o): after Andy's Proxmox snapshot; `skep-api` stopped during the `sqlite3` edit; the epair and all test data removed afterwards; every change on hearst recorded in the progress log.

## 23. Run 1 results (hearst, 2026-11-21)

Hearst validation run 1 of 3. Baseline on `b5b0546`, then `8c81e35`
(the head of `feat/m7b-templates` after section 22) deployed with
`scripts/deploy-test.sh`. All API calls as `s7oper` from merrick, curl
pinned to hearst's key (`779cc5ec…`, equal to `skep-api tls
fingerprint`), no `-k`; one login for the run. Record (commands and
output): [step10-hearst-run1.txt](step10-hearst-run1.txt).

### 23.1 Pre-check and host changes

- `b5b0546` deployed and running, `/health` ok, `virtual_interrupt_delivery`
  0, no jails, epairs, or RCTL rules, only the host's devfs, `/` 17 %.
- **Differences from the expected state:** `if_epair` was **not loaded**.
  Hearst was rebooted on 2026-11-19 at 11:27 by Andy for pkg upgrades
  (`last reboot`; it was loaded live only at step 8, never persisted). The
  M7 reserve reboot is now used. Hearst runs **15.1-RELEASE-p7**
  (`freebsd-version -kru`: p7, p7, p7), not p6 as the status doc says.
- **Host change, approved by Andy during the run:** `sudo kldload if_epair`
  (live only, no loader.conf or rc.conf change), after recording uptime,
  `last reboot`, and `virtual_interrupt_delivery` = 0. It is still loaded.
- The test credentials' layout was settled with Andy before the first
  successful login (two refused logins as `s7oper` from merrick at
  09:21 and 09:22 local time will show in the API log).

### 23.2 Results

| Test | Result | Notes |
|---|---|---|
| Pre-check | Done | See 23.1 |
| SR-01 1 (baseline, `b5b0546`) | **Reproduced** | `console.log` 52,429,725 bytes after the start; jailed `sleep 600` held fd 1 and 2 on the host's `console.log` (`procstat -f`); `df /` used +51 MiB; jail.conf had `exec.consolelog`. Jail deleted, run directory gone, `df /` back |
| Deploy `8c81e35` | Pass | Running inodes = installed (agent 3044883, API 3044885); `-version` of agent, API, skepctl = `8c81e35`; `/health` ok |
| SR-04 S1 | Pass | `jail locked-memory total`: `max_locked_memory_bytes` 1599455232 = `hw.physmem` 6397820928 / 4, `configured: false` |
| SR-01 2 | Pass | hostops test binary at `8c81e35` as unprivileged `aba`: 33 of 33 matched tests passed (`-test.list` = 33), none skipped |
| SR-01 3 | Pass | Rendered jail.conf has no `exec.consolelog` (`grep -c` 0) |
| SR-01 4 | Pass | After the start `console.log` 327,712 bytes (320 KiB + 32) with `skep: 52102045 bytes discarded`; `sleep` holds fd 1/2 on a pipe; `fstat` finds no process on `console.log`; `df /` used +1.5 MiB (logs), not 50 MiB. The stop added 145 bytes (`--- shutdown ---` … `--- shutdown stderr ---`, `Terminated`) |
| SR-01 5 | Pass | 256 KiB head + `skep: 759887 bytes discarded` + tail with rc.local's last line; `GET .../console-log` (64 KiB, `truncated: true`) ends with rc's last line (the date line) |
| SR-01 6 | Pass | `minimal-ssh-151`, DHCP NIC on bridge0 (10.79.60.108): `sshd`, `cron`, `syslogd` running after the start and 60 s later; SSH from merrick with the test key: `uid=0`, `r1-t6-ssh`. After the stop: `--- shutdown ---`, `Stopping sshd.`, `Stopping cron.`, …, `--- shutdown stderr ---`, `Terminated`; epair gone |
| SR-01 7 | Pass | Stop succeeded, jail gone. Log: rc's 900 bytes, `--- shutdown ---`, 32 KiB, `skep: 8061073 bytes discarded`, 32,768 bytes ending with the last line printed, `--- shutdown stderr ---`. The lines were 62 bytes, so the output was 7.75 MiB; N is exactly the total less 65,536 |
| SR-01 8 | Pass | The stdout line follows `--- shutdown ---`; the stderr line follows `--- shutdown stderr ---` |
| SR-01 9 | Pass | The second start truncated the log from 66,519 to 900 bytes; no shutdown markers, no `skep:` lines, no test 7 lines left |
| SR-01 10 (probe) | Stop **succeeds** | **`jail -r` exited 0 and the jail was gone**: the stop task succeeded with no error, `jls -j` finds no jail, state `stopped`. `/etc/rc.d/local` runs `rc.shutdown.local` in its own shell, so its `exit 1` does not reach jail(8). On disk the line after `--- shutdown stderr ---` keeps ESC (`1b`) and U+202E (`e2 80 ae`) (`od`; `cat -v` shows `^[[31mfake M-.ok`); `GET .../console-log` shows `[31mfake ok` with no ESC and no U+202E. The failing-stop branch (task error, `jail_remove_failed`) could not be reached this way |
| SR-01 11 | Pass | Stop succeeded; the stderr section is `skep: 1032226 bytes discarded` + exactly 32,768 bytes, ending with the last line written (the test's last line, then the shell's `Terminated`); N + 32,768 = the 1,064,994 bytes written |
| SR-01 12 | Pass | U+202E (`e2 80 ae`) and C1 CSI U+009B (`c2 9b`) kept on disk for stdout and stderr; the API's text has both removed |
| SR-04 S2 | Pass | `locked_memory` 1599455233: 422 `container_locked_memory_above_max`, details 1599455233 / 1599455232; nothing created |
| SR-04 S3 | Pass | A (1 GiB) started with exactly `memorylocked:deny=1073741824` and `allow.mlock` true; B (768 MiB) start 409 `container_locked_memory_exhausted` (running 1073741824 + 805306368 > 1599455232), no jail made; after A stopped, B started with exactly `memorylocked:deny=805306368`; both stopped and deleted |
| SR-04 S4 | **Pass, with a finding** | See 23.3. As run (`sudo jexec … mlocktest`): every size, even 16 MiB, refused (RLIMIT_MEMLOCK 64 KiB). With the test process's own limit raised in the jail (`ulimit -l unlimited`): 1000 and 1024 MiB locked (`memorylocked=1048576000` during the hold), 1025 and 1030 MiB refused (ENOMEM); `v_wire_count` 236935 before, 236923 after, 236451 after the stops |
| Cleanup | Done | No containers, jails, epairs, RCTL rules, snapshots, or clones; only the host devfs; `/var/run/skep/jails` empty; no test files in `/tmp`; templates and their `@skep-base` unchanged; `/` 17 %; `/health` ok. Differences from the pre-check: the deploy and `if_epair` loaded |

No security property failed. Agent and API logs during the run: only the
two expected refusals (S2, S3) at WARN; no ERROR.

### 23.3 Finding: RLIMIT_MEMLOCK caps a jail's locked memory below `limits.locked_memory`

Read-only diagnosis on hearst: `/etc/login.conf` (and the template's)
gives the `default` class `memorylocked=64K` and the `daemon` class
`memorylocked=128M`. The running agent has RLIMIT_MEMLOCK 131072 KB soft
and hard (`procstat -l`), and processes the jail's `/etc/rc` starts
inherit it (the jail's `cron`: 131072 KB). `sudo` and `jexec` (with or
without `-l`) give 64 KiB. FreeBSD's mlock(2) checks RLIMIT_MEMLOCK before
RCTL, so a jail service that does not raise its own limit can lock at
most 128 MiB per process whatever `limits.locked_memory` says; jailed root
can raise its own limit (shown above), which is what the RCTL rule then
bounds. Step 9 recorded "unlimited" under `jexec -l`; this differs now
(the 2026-11-19 pkg upgrade is a possible cause; not established).
`/etc/login.conf` is dated 2026-11-16 and `/etc/login.conf.db`
2026-09-29. Nothing was changed. For a decision: document it, have the
agent set the jail's RLIMIT_MEMLOCK to its `locked_memory` (or unlimited)
when it starts `/etc/rc`, or leave it.

**Correction (run 2, section 24.4; amendment u).** Two statements above
are wrong. The jail's `/etc/rc` processes do not inherit the agent's
128 MiB: the agent starts `/etc/rc` with `jexec -l -U root`, which applies
the jail's `root` login class (unlimited), and `rc.local` and its children
run unlimited. The 128 MiB of `cron` and `syslogd` comes from rc.subr,
which re-applies the `daemon` class (`limits -C daemon`) to every service
it starts. And step 9's "unlimited" was not a host change: it was measured
with `jexec -U root` (the `root` class), while run 1 used `jexec` without
`-U`, which keeps sudo's 64 KiB; the pkg upgrade is not a cause. The
conclusion stands that one rc.d service can lock at most 128 MiB unless
it raises its own limit.

### 23.4 Affects runs 2 and 3

- `if_epair` is loaded live only; another reboot unloads it again.
- `sudo jexec` gives a 64 KiB RLIMIT_MEMLOCK; any mlock test must raise
  the process's own limit in the jail, or be decided after 23.3.
- Test 10's failing-stop branch did not occur on 15.1-p7; provoking a
  failing `jail -r` needs another method, if wanted.
- DHCP gave the test jail 10.79.60.108 (lease on the VLAN 60 DHCP
  server; nothing static used).

### 23.5 Commits

`8c81e35` (section 22, deployed) and the commit with this section and the
record.

### 23.6 Andy's answers (2026-11-21), recorded as amendment t

1. **The three process deviations** of run 1 are approved: tests 4 and 5
   on one jail and tests 7 to 11 on another; S4's test process raising its
   own `ulimit -l` inside the test jail; test 7's 62-byte lines.
2. **S4 finding (23.3): no code change in M7b yet.** Document in jails.md
   that a jail process's RLIMIT_MEMLOCK comes from the jail's login
   classes (`default` 64 KiB; rc.d services started through rc.subr get
   `daemon`, 128 MiB) and applies before the RCTL `memorylocked` rule, so
   one process can lock at most its class limit even when
   `limits.locked_memory` is higher. Backlog: a memlock limit provided by
   the agent, its design pending run 2's probe.
3. **Run 2 adds two probes** (test jails only, no code):
   - **(a) memlock:** find out why step 9 recorded the limit as
     unlimited, and whether an RLIMIT_MEMLOCK set on the `/etc/rc`
     process (as started through `jexec -l`) survives into an rc.d
     service started by rc.subr; report what an agent-provided limit
     would actually need to reach a service.
   - **(b) test 10's failing branch:** in a test jail, make
     `/etc/rc.shutdown` itself exit 1 after doing its normal work, and
     record whether `jail -r` fails; if it does, check the task error and
     `jail_remove_failed` as in test 10.

Only the answers were recorded in this session; the jails.md text of
point 2 has not been written yet.

## 24. Run 2 results (hearst, 2026-11-21)

Hearst validation run 2 of 3: SR-03, SR-06, SR-09, SR-11, SR-I3, and the
two probes of amendment t. No deploy; hearst ran `8c81e35` throughout (the
branch head `e1743a6` has the same code). API calls from merrick, curl
pinned to hearst's key (`779cc5ec…`, equal to `skep-api tls fingerprint`),
no `-k`; one login per user for the run. Record (commands and output):
[step10-hearst-run2.txt](step10-hearst-run2.txt).

### 24.1 Pre-check, approvals during the run, and host changes

- Pre-check as expected: `8c81e35` (agent, API, skepctl), `/health` ok,
  `virtual_interrupt_delivery` 0, `if_epair` loaded, no jails, epairs,
  RCTL rules, or jail devfs, `/var/run/skep/jails` empty, `/` 17 %,
  `freebsd-version -kru` 15.1-RELEASE-p7 ×3, uptime 1 day 22:40 (no reboot
  since 2026-11-19).
- **Approved by Andy during the run, before any of it was done:**
  1. SR-06's state is in the agent's database
     (`/var/db/skep/agent/agent.db`, table `containers`), not `api.db`. The
     agent's start-up sweep removes every `failed` jail, so the test ran in
     two windows: **window 1**, `skep_api` stopped and the agent running,
     one-row `UPDATE` of the live `agent.db` (sqlite3 `.timeout 5000`; the
     agent reads the row on every request); **window 2**, both services
     stopped, then started (the sweep). Both databases were backed up with
     `.backup` first.
  2. "SR-13" in the request is SR-I3 of 13.9.
  3. SR-11 with temporary roles `s7snaprole` (`container:read`,
     `container:snapshot`) and `s7writerole` (`container:read`,
     `container:write`) and users `s7snap` and `s7write`, random passwords
     never printed or stored. Roles cannot be created through the API, so
     they were inserted into and deleted from `api.db` with sqlite3 (run as
     `skep`) while `skep_api` was stopped, in windows 1 and 2.
  4. One extra `agent.db` row in window 2: test container `r2-vmrow` given
     the VM `fbsd1`'s MAC (see 24.3, finding 2). `fbsd1` itself was not
     touched.
- **Changes on hearst, all reverted:** the hand-made `epair0` (its a end
  given `02:52:32:00:00:03` and added to `bridge0`, then removed and
  destroyed); `skep_api` stopped/started twice and `skep_agent` once;
  the `agent.db` rows above (on test containers, all deleted afterwards);
  the two roles and two users (deleted); `agent.db.pre-run2` and
  `api.db.pre-run2` (removed after the checks); `/tmp/r2-hostops.test`
  and `/tmp/r2-epair` (removed); test containers `r2-mac`, `r2-vmmac`
  (refused, never made), `r2-pa`, `r2-pb`, `r2-q1`, `r2-q2`, `r2-s`,
  `r2-vmrow`, and the snapshot `r2-s11` (all deleted). Inside test jails:
  `rc.local`, `rc.shutdown.local`, `/etc/rc.shutdown` (r2-pb), and cron
  restarts (r2-pa). No `jail -r` on the host except the approved one for
  r2-pb.

### 24.2 Results

| Test | Result | Notes |
|---|---|---|
| Pre-check | Done | See 24.1 |
| SR-03 422/400 on create | Pass | `58:9c:fc:00:00:01` (universal), `01:00:5e:00:00:01` and `03:00:00:00:00:01` (multicast), `00:00:00:00:00:00`: 422 `container_mac_invalid` with the rule's message; `02:00:00:00:00` (5 bytes): 400 `invalid_request`; nothing created |
| SR-03 update and clone | Pass | Universal MAC on update and on clone: 422 `container_mac_invalid`; the NIC kept `02:52:32:00:00:03` |
| SR-03 MAC on the bridge (hand-made epair) | Pass | `epair0a` with `ether 02:52:32:00:00:03` as a member of `bridge0`: the start task failed `container_mac_in_use`, "already present on bridge0 (the member epair0a of bridge0)"; no jail and no Skep epair made. Epair removed |
| SR-03 bridge formats | Pass | `ifconfig bridge0`: `ether 58:9c:fc:10:9f:32`, `member:` lines. `ifconfig bridge0 addr` with the jail running: `02:52:32:00:00:03 Vlan0 epair0a 1191 flags=0<>` (the parser reads fields 1 and 3; 15.1 prints `Vlan0`, the code comment says `Vlan1`). The bridge's own address is universal, so the rule refuses it before the bridge check (not testable) |
| SR-03 MAC held by a Skep VM | Pass (at start, with a planted row) | Create with `fbsd1`'s `02:1e:08:e9:9d:7c`: **400 `invalid_request`** "already used by another network interface" (store-level check, finding 2). With the row planted in window 2, start: 409 `container_mac_in_use`, `held_by_kind: vm`, `held_by_name: fbsd1`; no jail made |
| SR-03 MAC stored before the rules | Pass | `r2-mac` given `58:9c:fc:00:00:02` in window 2: start 422 `container_mac_invalid` at submission; no jail made |
| SR-06 quarantined, agent running (window 1) | Pass | `r2-q1` (jail running, JID 16) recorded `failed`: GET and list show `failed`, not running; start, stop, restart: 409 `container_quarantined`; still `failed` after |
| SR-06 delete | Pass | Delete task: "removing the quarantined jail without running any of its code", dataset destroyed; a watcher polling the jail root every 50 ms never saw the `rc.shutdown.local` marker; jail, dataset, run directory, devfs gone |
| SR-06 sweep (window 2) | Pass | `r2-q2` (running) recorded `failed`; at agent start `jail_quarantine_removed`, summary `removed_without_exec_stop: 1`; state `stopped` with the sweep detail; no marker, no `--- shutdown ---` in its log, devfs gone. Afterwards a normal start and stop of `r2-q2` wrote the marker |
| SR-06 marker control | Pass | `r2-s`: a normal stop wrote the marker and `r2 rc.shutdown.local ran` in the shutdown section |
| SR-09 (as root) | Pass | Both tests passed in 0.01 s; FIFO, directory, symlink, and the device node (made: it **ran**, refused with "operation not supported") all refused; `WriteJailFilesRefusesFIFO` passed. Binary sha256 `828d1fba…` |
| SR-11 | Pass | Rollback of a stopped jail to its snapshot: `s7snap` 403 (`permission: container:write`), `s7write` 403 (`container:snapshot`), `s7viewer` 403 (`container:write`), each an `authz.deny` audit entry; `s7oper` and `s7admin` succeeded. Users deleted (the deleted user's session then 401), roles deleted, `GET /roles` shows only the built-ins |
| SR-I3 | Pass | In a running jail (`/dev/zfs` present, `allow.mount` and `allow.mount.zfs` false, `enforce_statfs` 2): `zfs list` "no datasets available", `zpool list` "no pools available", `zfs set skep:container_id=x`, `zfs snapshot`, and `zfs get` on its own dataset "dataset does not exist"; on the host the property is still the container ID and no snapshot was made |
| Probe (a) memlock | Done | See 24.4 |
| Probe (b) `/etc/rc.shutdown` exit 1 | **`jail -r` fails** | See 24.5 and finding 1 |
| Cleanup | Done | No containers, jails, epairs, RCTL rules, jail devfs, or test files; `/var/run/skep/jails` empty; ZFS list equal to the pre-check; built-in roles only; users as before; both services running `8c81e35`, `/health` ok; `if_epair` loaded; `/` 17 %. The three sessions logged out |

No security property failed. Agent and API logs during the run: the
expected refusals at WARN, the probe (b) `jail_remove_failed` (WARN) and
its `task failed` (ERROR, the only ERROR), and the `orphaned_devfs_removed`
after the manual `jail -r`.

### 24.3 Findings (functional, not security)

1. **A jail whose `/etc/rc.shutdown` exits non-zero cannot be stopped or
   deleted through the API** (probe b). `jail -r` exits 1 and jail(8)
   does not remove the jail; the stop task fails and the state stays
   `running` (rc.shutdown has stopped cron; `syslogd` still runs). A new
   stop runs rc.shutdown again, and a delete of a running container is
   refused (`notStopped`, by reading the code; not tried on hearst). Only
   a host `jail -r` frees it. Also: the task error the API returns is the
   engine's generic `task_failed` "the task failed; see the agent log"
   (`internal/tasks/engine.go:611`, the hostops error has no code), so the
   `jail output, last line of stderr: "..."` text (amendment p) is only in
   the agent log; and that last line is jail(8)'s own
   `jail: skep-…: /bin/sh /etc/rc.shutdown jail: failed`, not what
   rc.shutdown wrote. The console log gets no shutdown section (amendment
   q, kept until this probe). For a decision: give the failure a
   `container_*` code so the API shows it; whether a stop whose
   `exec.stop` failed should fall back to removal without `exec.stop`
   (jail root controls `/etc/rc.shutdown`, so today it can make its jail
   unremovable through Skep); and whether to append rc.shutdown's output
   when `jail -r` fails.
2. **A VM's MAC is refused at create with 400 `invalid_request`**, not 409
   `container_mac_in_use`: the store's cross-table uniqueness check
   (`internal/database/containerstore.go:52`, the same in `vmstore.go:129`)
   runs before the start-time check, which is therefore defence in depth.
   For a decision: map that store error to `container_mac_in_use` (409) so
   the UI hint applies.
3. Nit: `learntMACs`' comment shows `Vlan1`; 15.1 prints `Vlan0`. The
   parser does not use that field.

### 24.4 Probe (a): RLIMIT_MEMLOCK

- **Step 9's "unlimited" and run 1's 64 KiB are different commands, not a
  host change.** `jexec -U root` (with or without `-l`) applies the jail's
  `root` login class: `unlimited`. `jexec` without `-U` (run 1) keeps the
  caller's limit, and `sudo` gives 64 KiB (`sudo sh -c 'ulimit -l'`).
  Step 9 recorded "the jail's root under jexec -l" (its exact command was
  not kept), which matches `-U root`. The jail's and the host's
  `login.conf.db` give the same classes: `root` infinity, `default` 64 KiB,
  `daemon` 131072 KiB; the 23.3 guess about the 2026-11-19 pkg upgrade is
  not needed.
- **`/etc/rc` already runs unlimited.** The agent starts it with
  `jexec -l -U root` (`jailRCArgs`): `rc.local` recorded `unlimited` soft
  and hard, and the `sleep` it left had `infinity`. 23.3 said the jail's
  rc processes inherit the agent's 128 MiB; they do not. The 128 MiB of
  `cron` and `syslogd` comes from rc.subr.
- **rc.subr re-applies the login class to every service it starts with
  `command`** (`limits -C ${name}_login_class:-daemon $_limits`, rc.subr
  line 1549). `cron` restarted from a shell with 256 MiB or unlimited came
  back with 128 MiB; with `cron_limits="-l 300m"`, 300 MiB; with
  `cron_login_class=root`, unlimited. `rc.local` (a `start_cmd`, no
  `limits` wrapper) keeps the caller's limit (262144 KiB in, 262144 out).
- **What an agent-provided limit would need to reach a service:** a limit
  set on the `/etc/rc` process does not survive into rc.subr services, and
  `/etc/rc` is already unlimited. It would need the jail's own
  configuration: `<name>_limits` or `<name>_login_class` in the jail's
  `rc.conf`, or the `daemon` class in its `login.conf` (plus `cap_mkdb`).
  All are writes into the jail's tree, which Skep does not do after the
  first start, and choices the jail's administrator can already make.
- **Recommendation:** drop the backlog item for an agent-provided memlock
  limit. Correct the jails.md note of amendment t point 2 before writing it:
  rc.d services get the jail's `daemon` class (128 MiB) through rc.subr,
  and a jail administrator raises it per service with
  `<name>_limits="-l <size>"` or `<name>_login_class`; `/etc/rc`,
  `rc.local` and their children, and `jexec -U root`, get the `root` class
  (unlimited); `default` (64 KiB) applies to other logins and to `jexec`
  without `-U`; RCTL `memorylocked` (`limits.locked_memory`) remains the
  jail-wide bound.

### 24.5 Probe (b): `/etc/rc.shutdown` exits 1

In `r2-pb`, the last line of `/etc/rc.shutdown` (`exit 0`) was replaced
with `echo r2pb rc.shutdown exiting 1 >&2; exit 1`, after its normal work.
Stop: **`jail -r` exited 1 and the jail was not removed** (still JID 14).
The task failed with the generic `task_failed` (finding 1); the agent log
has `jail_remove_failed` with `exit_code` 1 and `jail_output`
`jail: skep-…: /bin/sh /etc/rc.shutdown jail: failed`, and `task failed`
with `jail output, last line of stderr: "…"`. No shutdown section in
`console.log`. State `running`; `syslogd` still running in the jail.
Removed with the approved `sudo jail -r skep-<id>` (no host `jail.conf`,
so no `exec.stop`); it left the jail's devfs, which the Skep delete
unmounted (`orphaned_devfs_removed`) before destroying the dataset.

### 24.6 Affects run 3

- Nothing left on hearst; `if_epair` still loaded live only.
- Finding 1 needs a decision before any run-3 test of a failing stop, and
  the jails.md memlock note should use 24.4.
- The SR-03 learnt-table refusal for a device that is not a member (only
  `vtnet1`'s universal neighbours were learnt) and the bridge's own
  address remain untested; neither can be reached without another device
  or a host change.

### 24.7 Commits

The commit with this section and the record.

### 24.8 Andy's answers (2026-11-21), recorded as amendment u

1. **Run 2's changes 1–5** (24.1 and the stop report) are approved.
2. **Probe (a): the recommendation is accepted.** The backlog item "a
   memlock limit provided by the agent" is dropped; 23.3 is corrected (see
   the correction there). Amendment t's jails.md note follows 24.4:
   services get the `daemon` class (128 MiB) through rc.subr, raised per
   service by the jail's administrator; `/etc/rc`, `rc.local`, and
   `jexec -U root` get the `root` class (unlimited); `default` (64 KiB)
   applies elsewhere; RCTL `memorylocked` stays the jail-wide bound.
3. **Finding 1 is fixed in M7b: Skep must always be able to remove its
   jails.**
   - a. If `jail -r` fails or times out, the agent appends rc.shutdown's
     stdout and stderr sections to `console.log` as for a successful stop,
     then a line `--- jail -r failed: <cleaned last stderr line> ---`, then
     removes the jail with `jail -R` (no `exec.stop`), then runs the normal
     post-removal cleanup (devfs by fsid, epairs, RCTL rules, run
     directory).
   - b. The stop then succeeds; the agent logs `container_stop_forced` with
     the cleaned jail output, and the task result carries it if the task
     model supports warnings (probe; if not, the log and `console.log` are
     enough).
   - c. If `jail -R` also fails, stop and delete fail with a coded error
     `container_remove_failed` whose message includes the cleaned last
     stderr line, so the API shows it instead of the generic
     `task_failed`.
   - d. Probe whether `exec.stop` has a timeout today. If rc.shutdown can
     hang indefinitely, add a 2-minute timeout (the same as the start) and
     treat it as a failure under (a).
4. **Finding 2:** a create or update with a MAC held by a Skep VM returns
   409 `container_mac_in_use` with `held_by_kind: vm`, matching the
   start-time check.
5. **SR-03's learnt-table non-member case and the bridge's own address**
   cannot be reached on hearst; they are covered by the merrick unit
   tests.

Only the answers were recorded in this session; no code was written.

## 25. Amendment u built (2026-11-21, merrick only)

Points 2–4 of amendment u. No hearst access, no deploy, no PR. Branch
`feat/m7b-templates`.

### 25.1 Probes before the build

- **Does `exec.stop` have a timeout today (3d)? Yes.** The rendered
  jail.conf has `exec.timeout = <start timeout>` (`renderJailConfMode`,
  `containers.start_timeout_seconds`, default 120 s, the same 2 minutes as
  the start), which jail(8) applies to `exec.stop`, and the runner kills
  `jail -r` itself after start + stop + 30 s (`removeJail`). rc.shutdown
  cannot hang indefinitely, so no timeout was added; the runner's own
  timeout is now treated as a failure under 3a. What jail(8) does when
  `exec.timeout` expires during `exec.stop` (that the removal then fails)
  is from the manual, not observed: run 3, test u3.
- **How task errors reach the API.** `tasks.Engine.outcome` keeps the
  code only of a `*tasks.Failure`; anything else becomes `task_failed`
  "see the agent log" (`internal/tasks/engine.go`). The containers
  service's `failure()` turns a `*models.Error` into a `Failure`, so a
  hostops `*models.Error` with a new code reaches `GET /tasks/{id}`. The
  stop path already called `failure(err)`; the old `jail -r` error was a
  `*CommandError`, hence `task_failed`.
- **Warnings in the task model (3b): none.** A task has a result or an
  error; `tasks.Reporter` has only `Progress`, always level `info`. So a
  forced stop is in the agent log (`container_stop_forced`) and the
  console log only, as 3b allows.
- **The MAC check (4).** `insertNICs` (create via the submission
  companion, update via `UpdateConfig` in the task) refused a VM's or
  another jail's MAC with `invalid_request`. Clone also goes through
  `normalizeNICs` with request MACs.

### 25.2 Built

- **Runner** (`exec_unix.go`, `templates_pkg.go`, `command.go`): new
  `EnvCommand.OutputOnFailure`. On a failure or a timeout the run still
  returns the marked stdout head/tail and writes the stderr tail to
  `StderrTail` (amendment q's layout). A timeout is now a `*TimeoutError`
  with the same message as before and, with `StderrLastLine`, the cleaned
  last stderr line. A caller's cancellation still returns only
  `ctx.Err()`.
- **`removeJail`** (`jails_host.go`) takes the jail's root. When `jail -r`
  fails (`*CommandError`), times out (`*TimeoutError`), or exits 0 but the
  jail is still there, it logs `jail_remove_failed`, and for a removal
  with `exec.stop` appends, in one write, `--- shutdown ---` + stdout,
  `--- shutdown stderr ---` + stderr, then
  `--- jail -r failed: <line> ---`. `<line>` is the cleaned last stderr
  line (control and Cf characters removed, at most 200 bytes), `no jail
  output on stderr`, `timed out after <d>, last line of stderr: <line>`
  (or `, no jail output on stderr`), or `the jail was still there after
  jail -r exited 0`. Then `forceRemove`: `jail -R skep-<id>` (env
  `jailEnv`, 30 s, own context detached from the caller's cancellation so
  a task that spent its time in `jail -r` still removes the jail), a
  `jls` check that it is gone, the devfs on `<root>/dev` unmounted by fsid
  (`removeLeftoverDevfs`), epairs destroyed, RCTL rules removed, and
  `container_stop_forced` at WARN with `jail_output`. Cleanup failures
  after a successful `jail -R` are logged, not failures. The console log
  and the run directory stay.
- **`container_remove_failed`** (`pkg/models`, 409 in `httpkit`): when
  `jail -R` fails or leaves the jail, the message is `the jail could not
  be removed: jail -r failed (jail output: "<line>"), and jail -R then
  failed (exit status N, last line of stderr: "<line>"); it may still be
  running`, both lines cleaned; logged as `container_remove_failed` at
  ERROR. Stop and restart return it through `failure()`. The delete of a
  quarantined jail keeps the code and adds `; the jail stays
  quarantined`; other discard failures stay `container_quarantined`.
- **Finding 2** (`containers/network.go`, `tasks.go`, `snapshots.go`,
  `database/containerstore.go`): `checkMACsFree` after `normalizeNICs` in
  create, update, and clone, sharing `macHeld` with the start's
  `checkMACs`: 409 `container_mac_in_use`, details `slot`,
  `mac_address`, `held_by_kind`, `held_by_id`, `held_by_name` (and
  `container_id` on update). The refusal goes through `s.refuse`, so it
  is audited like the other submission refusals. `insertNICs` returns the
  same code with `held_by_kind` from its in-transaction check.
- **jails.md login-class note** (point 2), following 24.4: `/etc/rc`,
  `rc.local`, and `jexec -U root` get `root` (unlimited); rc.subr gives
  services `daemon` (128 MiB), raised per service with `<name>_limits` or
  `<name>_login_class`; `default` (64 KiB) elsewhere; RCTL
  `memorylocked` stays the jail-wide bound.
- **Docs**: jails.md (stop, forced removal, delete, MAC addresses, the
  memlock note), security overview, architecture overview, OpenAPI (stop
  and delete descriptions, `ContainerNICSpec.mac_address`, the
  `VMConflict` code list), `web/src/api/schema.d.ts` regenerated, ADR 0010
  implementation note.

### 25.3 Tests (merrick)

New or rewritten tests:

| Test | What it checks |
|---|---|
| `hostops` `TestRemoveJailFailureForcesRemoval` (4 cases, real `/bin/sh` standing in for jail(8)) | jail -r fails: stop succeeds; console.log = rc output + both sections + failure line with the cleaned line (ESC/CR, 400-byte line, U+202E/U+202C, empty stderr); order jail -r → jail -R → devfs unmount (only the jail's FSID) → `rctl -r`; jail -R's env/timeout; `jail_remove_failed` and `container_stop_forced` logged without control characters. Replaces `TestRemoveJailFailureReportsLastStderrLine` |
| `TestRemoveJailForcedRemovalFails` | jail -r and jail -R both fail: `container_remove_failed` with both cleaned lines (probe b's jail(8) line); no `container_stop_forced`; no unmount or rctl; console log still has the sections and the failure line. Also a jail -R that exits 0 but leaves the jail |
| `TestRemoveJailTimeoutForcesRemoval` | a hanging rc.shutdown (300 ms runner timeout): what it wrote is appended, the line says `timed out after 300ms, last line of stderr: ...`, jail -R runs |
| `TestRemoveJailCancelledIsNotForced` | a cancelled removal: no jail -R, nothing appended |
| `TestDiscardFailureForcesRemovalWithoutLog` | a removal without exec.stop is forced too and appends nothing |
| `TestRemoveJailLeftJailForcesRemoval` | jail -r exits 0, jail still there: sections + failure line, jail -R |
| `TestFinishStartRefusalWithFailedRemovalIsQuarantined` (changed) | quarantine now needs jail -r (a `*TimeoutError`) and jail -R to fail |
| `containers` `TestStopRemoveFailedIsCoded` | stop and restart tasks fail with `container_remove_failed` and the lines, not `task_failed`; container still running; a later stop works (hostfake `FailStop`) |
| `TestDeleteRemoveFailedIsCoded` | delete of a quarantined jail: `container_remove_failed`, "stays quarantined", still `failed` |
| `TestMACHeldRefusedAtCreateUpdateClone` | VM-held MAC (upper-case in the request) at create, update, clone: `container_mac_in_use`, `held_by_kind vm`, `held_by_name vm1`, API reply 409 with the details; jail-held MAC at create: `held_by_kind container`; nothing created or changed; the jail's own MAC is fine |
| `database` `TestContainerStoreMACHeld` | the store's check: coded on insert and on `UpdateConfig`, for a VM and a jail |

**Each change broken on purpose** (patched, tests run, file restored;
script in the scratchpad):

| Break | Failed |
|---|---|
| runner returns nothing on failure | FailureForcesRemoval, ForcedRemovalFails, TimeoutForcesRemoval |
| no jail -R fallback (return the jail -r error) | FailureForcesRemoval, ForcedRemovalFails, TimeoutForcesRemoval, DiscardFailureForcesRemovalWithoutLog |
| no failure line | FailureForcesRemoval, ForcedRemovalFails, TimeoutForcesRemoval, LeftJailForcesRemoval |
| timeout as an untyped error | TimeoutForcesRemoval |
| jail -R failure with another code | ForcedRemovalFails |
| jail -R failure ignored | ForcedRemovalFails, FinishStartRefusalWithFailedRemovalIsQuarantined |
| cancellation forced | CancelledIsNotForced |
| no devfs unmount after jail -R | FailureForcesRemoval |
| no RCTL cleanup after jail -R | FailureForcesRemoval |
| delete wraps `container_remove_failed` as quarantined | DeleteRemoveFailedIsCoded |
| stop returns the error without `failure()` | StopRemoveFailedIsCoded |
| no submission MAC check | MACHeldRefusedAtCreateUpdateClone |
| store check back to `invalid_request` | ContainerStoreMACHeld |
| hostfake ignores `FailStop` | StopRemoveFailedIsCoded |

**Checks:** `make web-check` 3 runs, each exit 0 (typecheck, `check-types`
against the regenerated schema, 15 files / 112 tests). `go test ./...`
exit 0 (40 packages ok). `make lint` exit 0 (gofmt, vet for Linux and
FreeBSD, staticcheck for both; also run by the pre-push hook on
`d4066c1`). `GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`
built an ELF FreeBSD binary (compiled only). `make test` (race): **one
run failed and I did not keep its output** (only the last three lines,
`FAIL`, were shown); the next `make test` and three full
`go test -race -count=1 ./...` runs passed, and the new and touched
tests passed 15 times in a row under `-race`. The failing package is
unknown.

### 25.4 Changes not in the request (for approval)

1. **The fallback covers every removal**, not only a stop: the removal
   after a failed `/etc/rc`, and the discards without `exec.stop`
   (refused, quarantined, half-started jails; these append nothing). So a
   jail is now quarantined only when `jail -R` fails too, and a
   quarantined jail's delete and the start-up sweep use `jail -R` when
   `jail -r` fails. `jail -R` runs none of the jail's code, so
   amendment n's guarantee holds.
2. **`jail -r` exiting 0 but leaving the jail** (previously an uncoded
   error) is forced the same way, with its own failure-line text.
3. **A jail-held MAC** at create, update, and clone also gets 409
   `container_mac_in_use` (`held_by_kind container`), not only a VM-held
   one; and **clone** gets the check as well as create and update.
4. **The store's MAC check** returns `container_mac_in_use` instead of
   `invalid_request` (same transaction, race backstop).
5. **The runner's timeout is a typed `*TimeoutError`** for every command
   (same message); nothing matched on its type before.
6. **`container_remove_failed` → 409** in `httpkit` (when a request is
   refused with it; in practice it ends tasks).
7. **A web UI hint** for `container_remove_failed` (`container.ts`, with
   a test).
8. **hostfake `FailStop`**, a test knob.
9. **Delete of a quarantined jail** reports `container_remove_failed`
   (with "the jail stays quarantined") rather than `container_quarantined`
   when both commands failed, as 3c says for delete.

### 25.5 Open points (not built; for a decision)

1. **"run directory" in 3a.** Normal stop cleanup does not remove the
   run directory (it holds `console.log`, which a stop must keep and
   which now has the failure line); only delete removes it. The forced
   path therefore keeps it too. If 3a meant removing it, the failure line
   would be lost.
2. **The timeout's failure line** reads `timed out after 2m40s, last line
   of stderr: <line>`; 3a's format names only the stderr line.
3. **A VM create with a jail-held MAC** is not refused: `vmstore.go`
   counts only `vm_networks`. Out of scope here; a small change if wanted.
4. **24.3 nit 3** (`learntMACs` comment says `Vlan1`): not touched.

### 25.6 Hearst tests for run 3 (proposed, for approval)

On `d4066c1` or later, deployed with `scripts/deploy-test.sh`; test jails
only.

- **u1 (failing rc.shutdown, probe b again):** in a test jail make
  `/etc/rc.shutdown` end with `echo r3 exiting 1 >&2; exit 1`. Stop:
  task **succeeds**; `jls -j` no jail; state `stopped`; console.log has
  `--- shutdown ---`, `--- shutdown stderr ---` with `r3 exiting 1` and
  jail(8)'s line, then `--- jail -r failed: jail: skep-…: /bin/sh
  /etc/rc.shutdown jail: failed ---`; agent log `jail_remove_failed`
  then `container_stop_forced` (WARN); no devfs on `<root>/dev`
  (`mount -p`), no epair, no RCTL rules (`rctl jail:skep-<id>` empty);
  `orphaned_devfs_removed` logged by the forced cleanup. Then start the
  jail again (state `running`) and delete it after a normal stop.
- **u2 (forced removal with control characters):** as u1 but
  rc.shutdown writes `printf 'x\033[31m\342\200\256y\n' >&2` last: on
  disk the stderr section keeps ESC and U+202E (`od`), the failure line
  does not; the API's console-log has neither.
- **u3 (hanging rc.shutdown, exec.timeout):** `containers.start_timeout_seconds`
  is 120, so use a test jail whose rc.shutdown runs `sleep 600`: record
  what jail(8) does at `exec.timeout` (120 s): whether `jail -r` fails
  (then forced, failure line from jail(8)) or kills it and succeeds; the
  stop finishes within about 2 minutes either way, the jail is gone, and
  the line says which. Needs ~3 minutes.
- **u4 (`container_remove_failed`):** not reachable on hearst without
  making `jail -R` fail (it needs a jail the kernel cannot remove); left
  to the merrick tests unless Andy has a method.
- **u5 (delete of a quarantined jail still works):** as SR-06's delete
  (run 2), to confirm the discard path with the new fallback.
- **u6 (MAC held by a VM at create and update):** create with `fbsd1`'s
  MAC: **409 `container_mac_in_use`**, `held_by_kind vm`,
  `held_by_name fbsd1`, nothing created; update of a test jail's NIC to
  that MAC: same 409, the NIC unchanged; clone with it: same 409. Another
  jail's MAC at create: 409 with `held_by_kind container`.
- **u7 (memlock note, documentation check):** in a running test jail,
  `jexec -U root <jail> sh -c 'ulimit -l'` unlimited, `procstat -l` of
  `cron` 131072 KB, and with `cron_limits="-l 300m"` and a cron restart,
  300 MiB: confirms the jails.md text (24.4 already showed it; repeat only
  if wanted).
- **Unchanged:** the hostops test binary on hearst as an unprivileged
  user should include `-test.run 'RemoveJail|DiscardFailure|Quarantined'`
  (these use `/bin/sh`, no root).

### 25.7 Commits

`14ca73c` fix(jails): remove a jail with jail -R when jail -r fails;
`e01f3ea` fix(containers): refuse a held MAC at create, update, and clone
with 409; `d4066c1` docs(jails): forced removal, MAC 409, memlock login
classes; and the commit with this section.

### 25.8 Andy's answers (2026-11-21), recorded as amendment v

1. **The changes of 25.4** (1 to 9) are approved as built.
2. **25.5:** the run directory stays after a forced stop; the timeout's
   failure line is approved as built; a VM create that does not refuse a
   MAC address a jail holds goes to the backlog ("before public
   release"). Point 4 (the `Vlan1` comment nit) was not answered and stays
   open.
3. **25.6:** u1 to u7 and the `-test.run` addition are approved for run 3;
   u4 stays merrick-only.
4. **Test logs** (after the race investigation, `42d69b9`): CLAUDE.md now
   requires every `make test` or `go test` run to tee its full output to
   `~/skep-test-logs/<UTC timestamp>-<target>.log`, outside the repo, so an
   intermittent failure keeps its package and test name.
5. **25.5 point 4** (the `Vlan1` comment nit): fixed as a comment-only
   change; `learntMACs`' comment now shows 15.1's `Vlan0` and says the
   field is not read. The test fixtures' `Vlan1` lines are unchanged (the
   parser ignores the field).

Only the answers were recorded in this session; no code was written.

## 26. Run 3 results (hearst, 2026-11-21)

Hearst validation run 3 of 3: the hostops test binary, amendment u tests
u1, u2, u3, u5, u6, u7 (25.6; u4 stays merrick-only), and the step-10
list (sections 7, 10, and 12). `8d65b9f`, the pushed head of
`feat/m7b-templates`, deployed with `scripts/deploy-test.sh`. API calls
from merrick, curl pinned to hearst's key (`779cc5ec…`, equal to `skep-api
tls fingerprint`), no `-k`; at most one login per user per script. The web
UI ran in Playwright 1.65.0 (Chromium, already installed on merrick),
pinned to the same key with `--ignore-certificate-errors-spki-list` and
`ignoreHTTPSErrors` off. Record (commands and output):
[step10-hearst-run3.txt](step10-hearst-run3.txt).

### 26.1 Pre-check, approval during the run, and host changes

- Pre-check as expected: `8c81e35` (agent, API, skepctl), `/health` ok,
  `virtual_interrupt_delivery` 0, `if_epair` loaded, no jails, epairs,
  RCTL rules, or jail devfs, `/var/run/skep/jails` empty, no test files in
  `/tmp`, `/` 17 %, `freebsd-version -kru` 15.1-RELEASE-p7 ×3, uptime
  2 days (last boot 2026-11-19 11:27), built-in roles only.
- **Approved by Andy during the run, before it was done:** the static
  parts of items c and e need a bridge in `network.bridges`, which held
  only `bridge0`. `agent.json` was backed up (sha256 `490bc087…` before),
  `"bridge1"` added (one-line diff, sha256 `7999ba60…`), `bridge1` created
  with no member and no address, and the services restarted (API stop,
  agent stop, agent start, API start) with no test jail running, as Andy
  required. After the static tests, with no jail running, `agent.json` was
  copied back from the backup (sha256 `490bc087…` again, mode, owner, and
  mtime kept by `cp -p`), the services restarted the same way, the backup
  removed, and `bridge1` destroyed.
- **u5 used run 2's SR-06 method** (25.6 says "as SR-06's delete"):
  `skep_api` stopped, `agent.db` backed up with `.backup`, a one-row
  `UPDATE` setting the running test container `r3-u5` to `failed`, the
  API started; the backup was removed after the delete.
- **Changes on hearst, all reverted:** the deploy (kept); `skep_api` and
  `skep_agent` restarted three times (deploy, bridge1 in, bridge1 out) and
  `skep_api` once more (u5); `agent.json` and `bridge1` as above;
  `agent.db.pre-run3` (removed); `/tmp/r3-hostops.test` (removed); the
  throwaway template `r3-tpl` (built and deleted); test containers `r3-u1`,
  `r3-u2`, `r3-u3`, `r3-u5`, `r3-u6`, `r3-u7`, `r3-dhcp`, `r3-clone`,
  `r3-edit`, `r3-edit2`, `r3-fail`, `r3-admclone`, `r3-ui`, `r3-ui2`,
  `r3-st1`, `r3-st2`, `r3-st3`, and their snapshots (all deleted). Inside
  test jails: `/etc/rc.shutdown` (u1, u2, u3), `rc.shutdown.local` (u5),
  `rc.local` (5d), `rc.conf` (`cron_limits`, u7), files under `/root` (5e),
  a busy loop (5f). Two hand `jail -r` on `r3-edit` (5i, approved). No
  reboot; no loader.conf, sysctl, kld, pf, login.conf, or api.json change;
  nothing static on VLAN 60 (DHCP leases 10.79.60.112 to .114).

### 26.2 Results

| Test | Result | Notes |
|---|---|---|
| Pre-check | Done | See 26.1 |
| Deploy `8d65b9f` | Pass | Running inodes = installed (agent 3045005, API 3045006); `-version` of agent, API, skepctl = `8d65b9f`; `/health` ok |
| hostops binary, `RemoveJail\|DiscardFailure\|Quarantined` | Pass | As unprivileged `aba`: 8 of 8 matched tests passed (12 with subtests), none skipped; sha256 `f9c0a79b…`; log `~/skep-test-logs/20261121T163449Z-hearst-hostops-test-run3.log` |
| u1 failing rc.shutdown | Pass | `exit 1` after rc.shutdown's normal work: stop task **succeeded**, no jail, state `stopped`; log: `--- shutdown ---` (Stopping cron …), `--- shutdown stderr ---` (`r3 exiting 1`, `Terminated`, jail(8)'s line), then `--- jail -r failed: jail: skep-…: /bin/sh /etc/rc.shutdown jail: failed ---`; agent log `jail_remove_failed` (exit 1), `orphaned_devfs_removed`, `jail_rctl_removed`, `container_stop_forced` (WARN); no devfs, epair, or RCTL rule left. Started again (`running`, new epair, rule back), later stopped normally and deleted |
| u2 control characters | Pass | On disk the stderr section keeps ESC (`1b`) and U+202E (`e2 80 ae`); the failure line has neither; the API's text has neither (`x[31my`). The failure line is jail(8)'s own last line, so no jail text reached it (see 26.4, item 6). First attempt's API check read the wrong field; repeated on a new jail |
| u3 hanging rc.shutdown | Pass | **See 26.3.** jail(8) gave up at `exec.timeout` (120 s): `jail -r` exited 1 with `… /etc/rc.shutdown jail: timed out`, the jail left; forced removal; stop succeeded 121 s after submission |
| u5 quarantined delete | Pass | State `failed`; start, stop, restart 409 `container_quarantined`; delete: "removing the quarantined jail without running any of its code"; the 50 ms watcher never saw the `rc.shutdown.local` marker; jail, dataset, run directory gone. `jail -r` without `exec.stop` succeeded, so no fallback was needed |
| u6 MAC held at create, update, clone | Pass | `fbsd1`'s MAC (upper case in the request) at create, update, clone: 409 `container_mac_in_use`, `held_by_kind vm`, `held_by_name fbsd1`; another jail's MAC at create: 409, `held_by_kind container`; nothing created, the NIC unchanged, no snapshot made; each refusal audited `denied` |
| u7 memlock note | Pass | `jexec -U root … ulimit -l` unlimited, plain `jexec` 64; cron 131072 KB; with `cron_limits="-l 300m"` in the jail's `rc.conf` and a restart, 307200 KB. First attempt used `sysrc`, which minimal-151 lacks; the rc.conf part was repeated with `echo >>` |
| 5a template build, build jail sysv | Pass | `r3-tpl` (pkgbase, FreeBSD-set-minimal + FreeBSD-ssh) built in 43 s as `s7admin`; the build jail as the kernel reported it (polled every 0.1 s): `sysvshm`, `sysvsem`, `sysvmsg` = `disable`, `securelevel` 2, `children.max` 0, `enforce_statfs` 2, no IP; release 15.1-RELEASE-p7, `readonly=on`, `@skep-base` |
| 5b has_sshd and the key | Pass | `has_sshd`: minimal-151 false, minimal-ssh-151 true, r3-tpl true (false while unbuilt). Key with minimal-151: 422 `container_ssh_unavailable`; with r3-tpl: created, `rc.conf` and `authorized_keys` written, SSH as root with the key worked |
| 5c DHCP jail on bridge0 | Pass | Create, start (lease 10.79.60.112, `host.hostname` set, sshd listening), SSH, restart (new JID, console log has only the new start), stop (`--- shutdown ---` with sshd and cron), no epair or jail devfs after the stop |
| 5c static jail on bridge1 | Pass | (bridge1 window) `ifconfig_skep0="inet 192.0.2.11/24"` and `resolv.conf` written; `r3-st1` and its clone `r3-st2` (192.0.2.12) ping each other; restart; `r3-st3` with 192.0.2.11 refused at start 409 `container_address_in_use` while `r3-st1` ran |
| 5d failed start | Pass | `rc.local` blocking in the foreground: after 122 s `container_start_failed` "… /etc/rc failed or outlasted the start timeout … (its output is in the console log): hostops: jexec timed out after 2m0s"; `state_detail` the same; jail, epair, devfs, rules gone; disk and API console log end with rc's lines, rc.local's two marker lines, then the shutdown sections; agent `jail_rc_failed`. The UI showed the code and the hint (5j) |
| 5e snapshots, rollback, clones (DHCP) | Pass | `consistent` when stopped, `crash_consistent` when running; rollback while running 409 `container_not_stopped`; rollback to the latest removed the newer file; to an older one 409 `snapshot_not_latest` without `destroy_newer`, then success destroying `r3-b` (named in the result); `started_once` stayed true; clone from a snapshot: new MAC, own lease, hostname = name, origin = the snapshot, the source's host keys (documented); snapshot and source delete 409 `snapshot_has_clones` while the clone exists |
| 5e static clones | Pass | Never-started source: 422 `clone_addressing_required` (`slots [0]`) without new addressing; with 192.0.2.12/24 accepted and written. **Started source: 409 `container_started`**, running or stopped; without addressing 422 `clone_addressing_required` (26.4, item 2). UI: clone-blocked notice, Clone disabled |
| 5f /metrics vs rctl -u | Pass | `memoryuse`, `maxproc`, `openfiles` equal `resident_memory_bytes`, `processes`, `open_file_slots`; disk equals `zfs get used,referenced`; `cpu_time_seconds` and `cpu_percent` trail by up to one 30 s sample. With `cpu_percent` 50 and a busy loop, pcpu swung 8 to 91 (documented). First attempt looked before the first sample (26.4, item 5) |
| 5g per-role | Pass | Reads 200 for all three. `s7viewer`: create, update, start, stop, restart, delete, snapshot, rollback, snapshot delete, clone, template build and delete: 403 with the missing permission, 14 `authz.deny` entries; nothing changed. `s7oper`: container writes 202; template build and delete 403 `storage:write`. `s7admin`: container writes, clone, deletes 202 |
| 5h edit rules | Pass, with a finding | Never started: hostname, `sysvipc`, limits, key, NICs, nameservers all change, and the files follow (rc.conf, resolv.conf removed with the nameservers, authorized_keys removed with the key and back). Running: 409 `container_not_stopped`. Started: hostname, options, limits accepted and applied at the next start (`sysv*` `disable`, `maxproc:deny=80`); key change or removal, nameservers, adding or removing a NIC: 409 `container_started`; a new MAC on slot 0 accepted and used; an unlisted bridge 422 `bridge_not_allowed`; a quota below what the root references: **202, then the task fails `invalid_request`** (26.4, item 1). First attempt omitted `nameservers` (26.4, item 4) |
| 5i devfs leftovers | Pass | After a hand `jail -r`: devfs still on `<root>/dev`, RCTL rule still there, state `stopped` with the "removed outside Skep" detail. API start: `orphaned_devfs_removed` (the old fsid) before `jail -c`, one devfs after; normal stop: none. Second hand `jail -r`, then API delete: `orphaned_devfs_removed`, rule removed, dataset gone, no devfs |
| 5j UI against the real API | Pass | TLS controls: no pin and a wrong pin refused (`ERR_CERT_AUTHORITY_INVALID`), the pin loads. 30 checks passed across three scripts: list, create dialog (key field follows `has_sshd`), create, edit dialog never started (NICs; Nameservers only with an all-static NIC set) and started (bridge only), start, console log, usage card (sample time shown), snapshot take and delete, clone notice, limit-removal confirmation ("Keep editing" sends nothing), the failed start's code and hint, delete; the static notice; viewer sees no actions. The first script's edit-dialog check expected Nameservers with no NIC and left the dialog open, failing the next four steps; they passed in the second script. No console errors |
| Cleanup | Done | No containers, jails, epairs, RCTL rules, or jail devfs; `/var/run/skep/jails` empty; ZFS list equal to the pre-check (25 entries); template list as before; `agent.json` sha256 as before, backup removed; `bridge1` gone; built-in roles and the same 12 users; `/` 17 %; `/health` ok; sessions logged out. Differences from the pre-check: the deploy |

No security property failed. Agent and API logs during the run: WARN
only (35 `task refused`, 14 `request denied`, 6 `orphaned_devfs_removed`,
4 `jail_remove_failed`, 4 `container_stop_forced`, 2 `jail_rc_failed`), no
ERROR.

### 26.3 u3: what jail(8) does at `exec.timeout`

With `exec.timeout = 120` in the rendered jail.conf and `/etc/rc.shutdown`
ending in `sleep 600` (after rc.shutdown's own 90 s watchdog has been
killed, line 108, so only `exec.timeout` applies): at +60 s the jail still
ran `/bin/sh /etc/rc.shutdown jail` and its `sleep 600` under `jail -f …
-r`. At 120 s **jail(8) gave up on `exec.stop`, printed `jail:
skep-<id>: /bin/sh /etc/rc.shutdown jail: timed out`, exited 1, and did
not remove the jail**. The agent took that as a failure under amendment u
3a: it appended the shutdown sections and `--- jail -r failed: jail:
skep-<id>: /bin/sh /etc/rc.shutdown jail: timed out ---`, removed the jail
with `jail -R`, and cleaned up. The stop task succeeded 121 s after it was
submitted; the runner's own timeout (start + stop + 30 s = 160 s) was not
reached. A hang inside an rc.d script's stop (before line 108) would be
killed by rc.shutdown's watchdog at 90 s instead (not run; from reading
the jail's `/etc/rc.shutdown`).

### 26.4 Findings and backlog items (functional, not security)

1. **A `disk_quota` below what the root references is accepted (202) and
   the update task then fails with the generic `invalid_request`** "the
   disk quota is below what the jail's root already uses (N bytes)". The
   limits stay unchanged. jails.md says such a quota "is refused", which
   is true but late. For a decision: refuse at submission, or give it a
   code (`container_quota_below_used`) so the UI can hint.
2. **Cloning a started static jail without addressing** gets 422
   `clone_addressing_required` "give each new addressing in nics", which
   can never succeed for that source (with addressing it is 409
   `container_started`). Nit: check `container_started` first for a
   started source with a static NIC.
3. **Nameservers on a jail with no NIC:** the API accepts them and writes
   `resolv.conf` (5h); the UI offers the field only when at least one NIC
   exists and all are static. Harmless; noted for consistency.
4. **An update that leaves `nameservers` out keeps the stored ones**, so
   a PUT that adds a DHCP NIC to a never-started jail with nameservers is
   refused 400 "nameservers are for static addressing only" although it
   does not mention nameservers; it must send `"nameservers": []`. The UI
   sends the full set. Same family as the clone backlog item of amendment
   m (empty list vs omitted).
5. **`/metrics` is a sample, up to 30 s old** (`monitoring.DefaultInterval`;
   the response has `observed_at`). A jail started within the last
   interval is missing, and just after a stop it still shows `running`
   with its last figures. The UI shows the sample time; jails.md's Usage
   paragraph does not mention the sampling. Doc nit.
6. **The failure line comes from jail(8):** with a failing or hanging
   rc.shutdown on 15.1 the last line of `jail -r`'s stderr is always
   jail(8)'s own (`… jail: failed` or `… jail: timed out`), so the jail's
   text never reached the failure line on hearst; its cleaning stays
   covered by the merrick tests.
7. **Two JSON records on one `daemon.log` line** (u2: `jail_rctl_removed`
   and `container_stop_forced` at 11:36:19). Seen once; cause not
   established (the daemon(8)-to-syslog path). Backlog: check.
8. **Docs still say the UI is pending validation against a real agent**
   (jails.md status paragraph, web-ui.md). Not changed in this run.
9. **`web/playwright.config.ts` sets `ignoreHTTPSErrors: true`**, also in
   its external mode meant for hearst. This run used a separate script
   pinned with `--ignore-certificate-errors-spki-list`. Backlog: use the
   SPKI pin in external mode.
10. Every jail start logs `route: message indicates error: File exists`
    for the loopback routes (base rc in a VNET jail). Cosmetic; not
    investigated.

### 26.5 Commits

The commit with this section and the record.

### 26.6 Andy's answers (2026-11-21), recorded as amendment w

1. **Run 3's changes outside the request** are approved: the temporary
   `bridge1` entry in `agent.json`, u5 reusing the SR-06 approval of
   run 2, and the extra test jails.
2. **Fixed before merge:** items 1, 5, 8, 9 (below).
3. **Backlog:** items 2, 3, 4, 6, 7, 10, added to the backlog list in 13.8.

Status of the 26.4 items:

| Item | Decision | Status |
|---|---|---|
| 1. `disk_quota` below usage: 202, then generic `invalid_request` | Fix before merge: refused at submission, 422, coded (`container_disk_quota_below_usage` or similar) | Open |
| 2. Started static clone without addressing: 422 `clone_addressing_required` | Backlog | Backlog (13.8) |
| 3. Nameservers on a jail with no NIC: API accepts, UI hides | Backlog | Backlog (13.8) |
| 4. Omitted `nameservers` kept on PUT | Backlog (API design: PUT vs merge semantics) | Backlog (13.8) |
| 5. `/metrics` is a sample up to 30 s old | Fix before merge: say so in jails.md | Open |
| 6. Failure line is always jail(8)'s on 15.1 | Backlog | Backlog (13.8) |
| 7. Two JSON records on one `daemon.log` line | Backlog | Backlog (13.8) |
| 8. Docs say the UI is pending validation | Fix before merge: jails.md and web-ui.md state the hearst validation (2026-11-21, §26) | Open |
| 9. Playwright config uses `ignoreHTTPSErrors` | Fix before merge: trust by the TLS key pin | Open |
| 10. `route: ... File exists` loopback lines | Backlog | Backlog (13.8) |

Only the answers were recorded in this session; no code was written.

## 27. Amendment w built (2026-11-21, merrick only)

Built on `feat/m7b-templates` from amendment w (26.6): points 1, 5, and 8.
**Point 9 is not built** (27.4). No hearst access, no deploy, no PR.

### 27.1 Probe before the build

- The quota refusal came from ZFS inside the task: `runUpdate` calls
  `SetJailQuota`, and `hostops` turned `zfs set refquota=`'s "size is
  less than current used or reserved space" into `invalid_request`.
  `normalizeUpdate` (submission) checked only the 64 MiB minimum.
- The containers service already holds `hostops.ZFS`, whose `Inventory`
  lists every dataset of a pool with `referenced` (`zfs list -p`). The
  monitoring sampler reads a jail root's `disk_referenced_bytes` the same
  way, and that read ran on hearst in run 3 (5f: equal to `zfs get
  used,referenced`). So the check needs no new host operation.
- Refusals at submission already go through `s.refuse` (audited
  `denied`), and `httpkit.StatusForCode` maps a code to its status. The
  UI's `containerErrorHint` covers container codes and the edit dialog
  shows it with the API's message, so there was a place for the hint.
- The hostfake's new jail roots referenced 0 bytes (its `@skep-base`
  snapshot carried none), unlike a ZFS clone, so the check could not be
  tested on the fake without fixing that.
- `web/playwright.config.ts`: `use.ignoreHTTPSErrors: true` (browser
  contexts **and** the Node-side `request` fixture) and
  `webServer.ignoreHTTPSErrors: true` (the readiness poll); the local
  stack's key is generated by skep-api at its first start, inside a temp
  directory `scripts/e2e-stack.sh` creates after the config is loaded.

Amendment w's points 1, 5, and 8 fit the code; point 9 did not (27.4).

### 27.2 Built

- **(1) Quota below usage.** New code `container_disk_quota_below_usage`
  (422) with details `container_id`, `disk_quota`, `referenced_bytes`,
  built by `models.DiskQuotaBelowUsage`. `normalizeUpdate`, after the
  not-stopped check, compares a **new, non-zero** quota with the root's
  `referenced` from `zfs.Inventory`. Below it, the update is refused before
  any task exists, and the refusal is audited `denied`. Equal is
  accepted, as ZFS accepts it. Removing or keeping the quota reads
  nothing. A root missing from the inventory is left to the task.
  `SetJailQuota` (host and fake) returns the same code when ZFS refuses,
  so a root that grows between the request and the task fails the task
  with the code instead of `invalid_request`. OpenAPI: the PUT
  description, `VMUnprocessable`'s list, `ContainerLimits.disk_quota`;
  `web/src/api/schema.d.ts` regenerated; UI hint "The jail's root
  already holds more than this quota (the message gives the amount;
  snapshots do not count). Give a larger quota, or remove data inside the
  jail first."
- **(5)** jails.md, Usage: the figures are a 30 s sample
  (`monitoring.DefaultInterval`, not configurable), `observed_at` says
  when; a jail started since the sample is missing, one stopped since
  still shows `running` with its last figures.
- **(8)** jails.md's status paragraph and web-ui.md (a new paragraph
  under the sections table) say the Containers section was validated
  against a real agent on hearst on 2026-11-21 (section 26).
- jails.md's `disk_quota` row names the new code; ADR 0010 has an
  implementation note for amendment w.

### 27.3 Tests (merrick)

| Check | Result | Log (`~/skep-test-logs/`) |
|---|---|---|
| `TestUpdateQuotaBelowUsage` (new): 100 MiB and referenced − 1 (with a hostname change) refused with the code and details; no update task, 2 `denied` audit entries, no `zfs set refquota` for them, limits and hostname unchanged; exactly referenced accepted and set; removal accepted | Pass | `20261121T181235Z-go-test-quota.log` |
| **Broken on purpose:** the comparison changed to `quota < 0 && …` | `TestUpdateQuotaBelowUsage` **failed** (`err = <nil>, want container_disk_quota_below_usage`); reverted | `20261121T181247Z-go-test-quota-broken.log` |
| `TestFailDiskQuotaBelowUsage` (new, httpkit): 422, code and `referenced_bytes` in the body | Pass | in the `go test ./...` log |
| **Broken on purpose:** the status mapped to 400 | **failed** (`status = 400, want 422`); reverted | `20261121T181248Z-go-test-httpkit-broken.log` |
| `TestSetJailQuota` (hostops): ZFS's refusal gives the new code | Pass | in the `go test ./...` log |
| Web: hint unit test and an edit-dialog test (422 shows the message with the bytes and the hint) | Pass; with the hint's `case` renamed on purpose both **failed**; reverted | (vitest run by hand, not logged) |
| `go test ./... -count=1` | Pass, 41 packages ok, exit 0 | `20261121T181403Z-go-test-all.log` |
| `make lint` (gofmt, vet Linux and FreeBSD, staticcheck Linux and FreeBSD) | Pass, exit 0 | `20261121T181420Z-make-lint.log` |
| hostops tests compile for FreeBSD (`GOOS=freebsd GOARCH=amd64 go test -c ./internal/hostops/`) | Pass, FreeBSD ELF | `20261121T181432Z-hostops-freebsd-test-compile.log` |
| `make web-check` ×3 (typecheck, generated types current, vitest) | Pass ×3, 114 tests each | `20261121T181441Z-…-run1.log`, `20261121T181451Z-…-run2.log`, `20261121T181501Z-…-run3.log` |

Pending FreeBSD validation: the check against real ZFS's `referenced`
(27.6). `make web-e2e` was not run (unchanged by this build).

### 27.4 Point 9 not built: the pin cannot be expressed in the configuration alone

Chromium accepts the pin (`launchOptions.args:
["--ignore-certificate-errors-spki-list=<base64 SHA-256 of the SPKI>"]`,
run 3's method; `skep-api tls fingerprint` prints the same hash as
`sha256:<hex>`). But the e2e tests also call the API through
Playwright's **Node-side `request` client** (the `request` fixture, 21
uses in `web/e2e`). In Playwright 1.65 its only TLS options are
`ignoreHTTPSErrors` and `clientCertificates`. It has no CA, pin, or
`checkServerIdentity` option (checked in
`playwright-core/types/types.d.ts`). `use.ignoreHTTPSErrors` covers both
clients, so turning it off breaks every `request` call, and nothing in
the configuration can pin them. Two smaller issues: the local stack's key
does not exist when the config is loaded, and `webServer`'s readiness
poll uses Node as well.

Options (for a decision):

- **(a) Pin the browser, keep `ignoreHTTPSErrors` for the Node client
  only.** `use.ignoreHTTPSErrors: false`, the Chromium flag from
  `SKEP_E2E_PIN` (required in external mode, refused if missing), a
  `request` fixture override that keeps `ignoreHTTPSErrors: true`, and
  the two `browser.newContext({ ignoreHTTPSErrors: true })` in
  `ui.spec.ts` made plain. The admin's password still goes unverified
  over the Node client, which against hearst means the lab network.
- **(b) (a) plus a pin check before the run:** a `globalSetup` that
  connects with Node `tls`, hashes the peer's SPKI, and aborts unless it
  equals the pin. The Node client's later connections are still
  unverified.
- **(c) The Node client trusts skep-api's certificate:** a wrapper
  (`make web-e2e` and an external-mode script) fetches the certificate,
  refuses it unless its SPKI hash equals the pin, and starts Playwright
  with `NODE_EXTRA_CA_CERTS` pointing at it (Node reads it only at
  start, so the config cannot set it). Chromium is pinned with the flag,
  and `ignoreHTTPSErrors` disappears everywhere. For the local stack the
  wrapper first makes the key (`skep-api tls regenerate -dev -data-dir
  <dir>/api`) and passes the directory to `e2e-stack.sh`. The Node
  client then trusts by certificate, not by key: after `tls regenerate`
  it has to be fetched and checked again. Untested: whether Node accepts
  skep-api's self-signed leaf as an anchor for its own names (hearst's
  certificate lists its interface addresses).

Recommendation: **(c)**. It is the only option with no
`ignoreHTTPSErrors` left anywhere, and both clients refuse a changed key.

### 27.5 Changes not in the request (for approval)

1. `SetJailQuota` (host and hostfake) returns
   `container_disk_quota_below_usage` instead of `invalid_request` when
   ZFS refuses the quota: the race after acceptance. The real path is
   pending FreeBSD validation, and the unit test uses ZFS's real stderr.
2. Hostfake fidelity: a template's `@skep-base` snapshot references the
   template's 132 MiB, and a new jail root references its origin's data,
   as `CloneJail` already did. Needed to test (1). Every other test still
   passes.
3. `models.DiskQuotaBelowUsage` in `pkg/models`, so hostops and
   containers build the same error.
4. New `internal/httpkit/fail_test.go` (the status of the code).
5. jails.md's `disk_quota` row and ADR 0010's implementation note.

### 27.6 Open point and hearst test

- **Open (not built):** a **create** with a quota below the template's
  referenced size (for example 64 MiB on minimal-151, about 78 MiB) is
  still accepted, and its task fails at `SetJailQuota` after the clone.
  It now fails with the new code, and the dataset is destroyed as
  before. Amendment w covers updates only. A check at create would read
  the template's `@skep-base` referenced. For a decision: build it or put
  it in the backlog.
- **Hearst test needed:** yes, short. The comparison is against the
  `referenced` of real ZFS, and only the inventory read has run on
  hearst. Proposed steps, after a deploy of this head, as `s7admin`,
  curl pinned (`--pinnedpubkey`, no `-k`):
  1. `POST /containers` `{"name":"r4-q","template_id":<minimal-151>}`
     (no NIC, no quota), wait for the task;
     `zfs get -Hp referenced <pool>/skep/jail/<id>` → R (expect about 78
     MiB).
  2. `PUT /containers/<id>` `{"limits":{"disk_quota":"64M"}}`: expect 422
     `container_disk_quota_below_usage`, `details.referenced_bytes` = R,
     `details.disk_quota` = 67108864. `GET /tasks` (newest first) shows
     no new `container.update` task, the access/agent audit has a `denied` entry, and
     `zfs get -Hp refquota` is still `0`.
  3. `PUT` with `"disk_quota": R` (bytes): 202, the task succeeds,
     `refquota` = R. Then `"disk_quota": R − 1` is 422. (R does not change
     on a jail that never ran.)
  4. `PUT` `{"limits":{}}`: 202, `refquota` back to `0`.
  5. UI (Playwright pinned as in run 3): edit dialog, quota 64M, Save:
     the message with R and the hint are shown, the dialog stays open.
  6. Delete `r4-q`, check no dataset is left.
  7. **Create check (amendment x, added in section 28):** T = `zfs get
     -Hp referenced` of minimal-151's template dataset (the one whose
     `skep:template_id` is its ID; expect about 78 MiB, the same as R).
     `POST /containers` `{"name":"r4-q2","template_id":<minimal-151>,
     "limits":{"disk_quota":"64M"}}`: expect 422
     `container_disk_quota_below_usage`, `details.referenced_bytes` = T,
     `details.disk_quota` = 67108864, `details.template_id` = minimal-151's
     ID; no new `container.create` task in `GET /tasks`, no container
     `r4-q2` in `GET /containers`, no new dataset under `<pool>/skep/jail`,
     and a `denied` audit entry. Then `"disk_quota": T` (bytes): 202, the
     create task succeeds, and `refquota` of the new root = T. Delete
     `r4-q2`.
  8. **UI create (amendment x):** in the create dialog (Playwright pinned
     as in run 3), minimal-151, disk quota 64M, Create: the message with T
     and the hint ("for a new jail, the template's size") are shown, and
     nothing is created.

  The race (the root grows between the request and the task) is not
  practical on hearst. It stays covered by the unit tests.

### 27.7 Commits

`c3eb1c0` fix(containers): refuse a disk quota below usage at submission
with 422; `f06511b` docs(jails): /metrics is a 30 s sample; UI validated
on hearst; quota code; and the commit with this section and ADR 0010's
note.

### 27.8 Andy's answers (2026-11-21), recorded as amendment x

1. **The changes outside the request (27.5, items 1–5) are approved.**
2. **Point 9: option (c)** of 27.4. If Node rejects skep-api's
   self-signed certificate as a trust anchor, fall back to option (b)
   and report.
3. **The open point of 27.6:** a create whose `limits.disk_quota` is
   below the template's size is refused at submission with 422
   `container_disk_quota_below_usage`; the details carry `disk_quota` and
   the template's referenced bytes.

Only the answers were recorded in this commit; the build follows in
section 28.

## 28. Amendment x built (2026-11-21, merrick only)

Amendment x (27.8) was recorded (`14a348f`) and both points were built
in the same session (CLAUDE.md workflow: context under ~120k). No hearst
access, no deploy, no PR.

### 28.1 Built

- **Create check.** `normalizeCreate` reads the template dataset's
  `referenced` from `zfs.Inventory` (matched by name and
  `skep:template_id`). The template is read-only after its build, so that
  is what the new root references from the start. A non-zero
  `limits.disk_quota` below it is refused before anything is stored, with
  422 `container_disk_quota_below_usage`. Details: `template_id`,
  `disk_quota`, `referenced_bytes` (`models.DiskQuotaBelowTemplate`, the
  same code as the update check). Equal is accepted. The refusal is
  audited `denied`. OpenAPI (create description, `disk_quota`),
  regenerated web types, jails.md's `disk_quota` row. The UI hint now
  fits both: "The quota is below what the jail's root holds: for a new
  jail, the template's size (the message gives the amount; snapshots do
  not count). Give a larger quota, or remove data inside the jail
  first."
- **Point 9, option (c).** The probe came first. Against an HTTPS server
  with a certificate made by `skep-api tls regenerate`, Node's `https`
  and Playwright 1.65's `request` client both refused it without
  `NODE_EXTRA_CA_CERTS` (`DEPTH_ZERO_SELF_SIGNED_CERT`, "self-signed
  certificate") and accepted it with the file (200, 200). **Node accepts
  skep-api's self-signed leaf as an anchor, so there was no fallback to
  (b).** Built:
  - `scripts/web-e2e.sh`: local mode makes the stack's key first (`tls
    regenerate -dev`), takes the pin from `tls fingerprint`, checks the
    certificate file against it, and passes the directory to
    `e2e-stack.sh` (new `SKEP_E2E_DIR`; the script removes only a
    directory it made). External mode (`SKEP_E2E_EXTERNAL=1`) needs
    `SKEP_E2E_LISTEN` and `SKEP_E2E_PIN`, fetches the served certificate,
    and refuses it unless its key matches. Both modes start Playwright
    with `SKEP_E2E_PIN` and `NODE_EXTRA_CA_CERTS`.
  - `scripts/e2e-trust.mjs`: `check` (a file against the pin) and `fetch`
    (reads the peer certificate, sends nothing, compares the SPKI SHA-256
    with the pin, writes the PEM with mode 0600 only on a match).
  - `web/playwright.config.ts`: no `ignoreHTTPSErrors` anywhere; Chromium
    launched with `--ignore-certificate-errors-spki-list=<base64 of the
    pin>`; the config throws unless `SKEP_E2E_PIN` (`sha256:<hex>`) and
    `NODE_EXTRA_CA_CERTS` are set. The `webServer` health check is
    verified by Node with the same certificate.
  - `web/e2e/ui.spec.ts`: the two `browser.newContext({
    ignoreHTTPSErrors: true })` are plain `newContext()`.
  - New `web/e2e/tls-pin.spec.ts`: Chromium launched without the pin,
    and with another key's pin (32 zero bytes), fails with `ERR_CERT_*`;
    the pinned page and the `request` client get 200.
  - `make web-e2e` and CI's browser-test step run `scripts/web-e2e.sh`;
    web/README.md describes the trust and the external mode.

### 28.2 Tests (merrick)

| Check | Result | Log (`~/skep-test-logs/`) |
|---|---|---|
| `TestCreateQuotaBelowTemplate` (new): 64 MiB and template − 1 refused with the code and details; no container, no create task, no `zfs clone`, 2 `denied` entries; exactly the template's size accepted | Pass | `20261121T183637Z-go-test-create-quota.log` |
| **Broken on purpose:** the create comparison changed to `quota < 0 && …` | **failed** (`err = <nil>, want container_disk_quota_below_usage`); reverted | `20261121T183638Z-go-test-create-quota-broken.log` |
| Web: create-dialog test (422 shows the message with the bytes and the hint); hint regex updated in the two amendment w tests | Pass | in the web-check logs |
| `make web-e2e` (first run) | **1 failed**: `ui.spec.ts` still expected Containers to say "Not built yet" (stale since step 10, not TLS); the TLS test passed | `20261121T183908Z-make-web-e2e.log` |
| `make web-e2e` after switching that check to Networking | Pass: 8 passed, 1 skipped (`clone-note.spec.ts`, needs `SKEP_E2E_CLONE_*`), including `tls-pin.spec.ts`; the temp directory was removed | `20261121T184011Z-make-web-e2e.log` |
| Trust controls: `check` right/wrong pin (0/1); `fetch` right pin (0, same certificate, mode 600) / wrong pin (1, no file) / malformed pin (1); external wrapper with a wrong pin refused before Playwright; `playwright test` without the wrapper refuses to start | All as expected | `20261121T184109Z-e2e-trust-controls.log` |
| `go test ./... -count=1` | Pass, 41 packages | `20261121T184138Z-go-test-all.log` |
| `make lint` | Pass | `20261121T184155Z-make-lint.log` |
| hostops tests compile for FreeBSD | Pass | `20261121T184209Z-hostops-freebsd-test-compile.log` |
| `make web-check` ×3 | Pass ×3, 115 tests each | `20261121T184209Z-…-run1.log`, `20261121T184219Z-…-run2.log`, `20261121T184229Z-…-run3.log` |

Not run: the CI workflow itself (it runs at the PR); the external mode
against hearst (no hearst access in this session). Pending FreeBSD
validation: the create check against real ZFS (27.6 steps 7 and 8).

### 28.3 Self-approved (CLAUDE.md, "Unrequested changes")

1. `ui.spec.ts`'s planned-section check moved from Containers to
   Networking (a stale test that failed `make web-e2e`).
2. `e2e-stack.sh` accepts `SKEP_E2E_DIR`; `make web-e2e` and CI's
   browser-test step call the wrapper (test tooling).
3. The new trust-control test `tls-pin.spec.ts` and the helper
   `e2e-trust.mjs`.
4. The UI hint's text, changed to fit create as well as update.
5. Docs: web/README.md's browser-test section,
   jails.md's `disk_quota` row, ADR 0010's implementation note.

Nothing needs approval: the create refusal's code, status, and details
are as amendment x decided (`template_id` in the details is in addition
to the two named, as the update's carry `container_id`).

### 28.4 Commits

`14a348f` docs(decisions): record amendment x; `48e4f87`
fix(containers): refuse a create whose disk quota is below the
template's size; `9059cf9` test(web): trust skep-api by its key pin in
the browser tests; `dedcc2d` docs(jails): name the create-time disk quota
refusal; and the commit with this section.
