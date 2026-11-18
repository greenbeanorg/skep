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
