# Design note: role management

Status: **design note only**, 2026-11-22. No code, no API change. It lists
what custom roles would need and the questions a future ADR must answer.
Nothing here is decided.

## Where things stand

From [ADR 0006](../decisions/0006-api-split-authentication-tls.md)
Decision 5 and the code:

- **One global role per user** (`users.role_id`), a role being a set of
  `resource:action` permission strings. Deny by default: `Role.Allows`
  (`internal/auth/permissions.go`) refuses any permission not declared in
  that file, even if a role holds the string.
- **Three built-in roles**, created by migration: `admin` (stored as
  `*`: every permission, including later ones), `operator`, `viewer`.
  SQLite triggers refuse any update or delete of a built-in row;
  migrations that add permissions drop and recreate the trigger
  (`api/0004`, `api/0005`).
- **The schema already allows custom roles** (`roles.built_in = 0`), but
  nothing creates them: `GET /api/v1/roles` (`role:read`) is the only
  role endpoint, and its description says roles cannot be edited in this
  release. On `hearst`, test roles were made with `sqlite3` on `api.db`
  under the standing approvals and deleted after each run (for example a
  role with only `container:read` and `container:snapshot` in step 10).
- **`user:manage`** (admin only) can create users and change a user's
  role, to any role, `admin` included. So `user:manage` is already
  equivalent to `admin`.
- **The last administrator is protected**: disabling, deleting, or
  demoting the only enabled `admin` is refused (`last_admin`).
- A role change ends the user's sessions; roles are read per request.
- The agent makes no authorization decision (ADR 0004); everything below
  is `skep-api` only.

The 21 declared permissions:
`system:read`, `node:read`, `task:read`, `task:cancel`, `user:read`,
`user:manage`, `role:read`, `audit:read`, `vm:read`, `vm:write`,
`vm:delete`, `vm:power`, `vm:console`, `vm:snapshot`, `storage:read`,
`storage:write`, `container:read`, `container:write`, `container:delete`,
`container:power`, `container:snapshot`.

## What role management would add

### API (sketch)

| Request | Purpose |
|---|---|
| `POST /api/v1/roles` | Create a custom role: `name`, `description`, `permissions` |
| `GET /api/v1/roles/{id}` | Read one (today only the list exists) |
| `PUT /api/v1/roles/{id}` | Change a custom role's description or permissions; name change optional |
| `DELETE /api/v1/roles/{id}` | Delete a custom role that no user holds |
| `GET /api/v1/permissions` | The declared permissions with a one-line meaning each, so that a UI or CLI does not hard-code them |

Likely errors (names to be decided; each is a new contract): role name
taken (409), built-in role (409 or 403), role in use by users (409, with
the count), unknown permission (422), a grant the caller may not make
(403, see question 2).

### Host command

`skep-api admin role list|create|set|delete`, alongside `admin create`
and `reset-password`: runs as `skep` (root switches to it), works without
a running `skep-api`, audited with the actor `local:<uid>`. Useful for
recovery and for scripted setup. The same validation as the API.

### Clients

`skepctl role ...` (public API only, as every `skepctl` command), and the
web interface's Users and Permissions section: a role editor with the
permission list grouped by resource, and the "dangerous" permissions
marked (question 6).

## Questions for the ADR

### Permissions and escalation

1. **Which permission manages roles?** A new `role:manage`, or
   `user:manage` (which is already admin-equivalent, as above)? A
   separate `role:manage` is only meaningful together with question 2;
   otherwise anyone who can edit a role they hold can grant themselves
   everything.
2. **May a caller grant what they do not hold?** Options: (a) only
   holders of `*` (admins) manage roles; (b) anyone with `role:manage`,
   but a role may only receive permissions the caller holds, and a caller
   may not edit the role they hold; (c) no rule. (a) is the simplest and
   matches today, where only admin can manage users. (b) allows
   delegation, but `user:manage` would then also need the same rule for
   assigning roles, or it bypasses it.
3. **May a custom role hold `*`?** Recommended no: `*` is how the
   built-in `admin` gets permissions added in later releases, and the
   last-administrator protection is defined on the built-in `admin` role.
4. **New permissions in later releases.** Built-in roles get them by
   migration. Custom roles do not, so a custom "operator-like" role
   silently lacks the new feature after an upgrade. Is that acceptable
   (it is the safe direction), and should the release notes and the role
   editor say which permissions are new?
5. **Permission strings never declared, or removed later.** Refuse
   unknown strings at write time (recommended). If a later release
   renames or removes a permission, a migration must rewrite custom roles
   too, and say so in its audit entry.
6. **Dangerous permissions.** Some grant more than they look:
   `storage:write` makes the root agent fetch and install packages;
   `vm:console` is a login on the guest; `container:write` lets a user
   create jails without limits on shared host resources (jails guide,
   "Limits are optional"); `user:manage` is admin-equivalent. Should the
   API refuse some of them in custom roles, require a confirmation flag,
   or only document and mark them?
7. **Dependent permissions.** Some actions need two (`vm:write` and
   `vm:snapshot` for a clone; `container:write` and `container:snapshot`
   for a rollback or clone). Should the API warn about a role that holds
   one without the other, or leave it?

### Lifecycle and safety

8. **Deleting a role in use.** Refuse while any user holds it
   (`ON DELETE RESTRICT` already does at the database level), or reassign
   to `viewer`? Refusing is recommended.
9. **Editing a role in use.** Takes effect on the next request of each
   holder. Should holders' sessions end, as a role change of a user does
   today? Recommended yes for removals (a removed permission must not
   linger in a cached client state), optional for additions.
10. **The last administrator.** Unchanged if custom roles cannot hold `*`
    (question 3). If they can, the protection must count them too.
11. **Limits.** A maximum number of custom roles and of permissions per
    role (the latter is bounded by the declared list anyway), and name
    rules (as usernames, with the built-in names reserved).
12. **IDs.** Built-in roles use their names as IDs; custom roles would
    get UUIDs (the schema's convention for new rows). Is a rename allowed,
    given that the audit log records role IDs and names?

### Audit

13. **What each write records.** Proposed: `role.create`, `role.update`
    (permissions before and after, as sorted lists, and which users hold
    the role), `role.delete`, each with the actor, in the access audit
    log; denials as `authz.deny` as today; host commands with
    `local:<uid>`. Is a diff enough, or should the full role be recorded?
14. **Assignments.** A user's role change is already audited. Should a
    role edit also write one entry per affected user, so that "what could
    user X do at time T" is answerable from the log alone?
15. **Retention.** Role entries follow the access log's retention (90
    days live, then sealed segments, ADR 0007). Nothing new, unless role
    history should stay live longer.

### Scope

16. **Per-resource scoping** (ADR 0006 option C: role bindings with a
    scope such as `/vms/{id}`) stays out. The ADR should confirm that the
    custom-role design does not make it harder: a later
    `role_bindings (user_id, role_id, scope)` table, with `users.role_id`
    becoming the binding at `/`, still works if custom roles are plain
    rows of `roles`.
17. **Multiple roles per user.** Out as well, for the same reason; a
    binding table would allow it later.
18. **The cluster.** Roles live in the local `api.db` today. In a cluster
    (Milestone 8) they belong in the cluster store. The API should not
    promise per-node roles.

## What it would touch

- `internal/auth` (declared permissions with descriptions, validation),
  `internal/apidb` (role store: insert, update, delete, in-use count),
  `internal/api` (routes, each registered with its permission, as the
  route-coverage test requires), `cmd/skep-api/admin.go` (host commands).
- A migration only if columns are added (for example a `sensitive` flag,
  or `updated_by`); the `roles` table itself already fits.
- `api/openapi.yaml`, the generated web types, the web Users and
  Permissions section, `skepctl`, the security overview, and the
  administration docs.
- Tests: route coverage, escalation rules (question 2) as table tests,
  built-in immutability through the API, delete-in-use, audit entries,
  and the host commands under the `skep` account.
