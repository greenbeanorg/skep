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
