# M10 backlog batch (#58), live on smith — 2026-11-23

Unattended run; Andy away; hearst off. Package from `chore/m10-backlog` 571f7b2
(`make stage` on merrick, `pkg create` on smith), `pkg add -f`, then
`skep-setup upgrade -yes` (health ok). No guests on smith. Answers
`-mgmt vtnet0 -pool tank` (smith's agent.json). Logs in
[m10-backlog-live-20261123/](m10-backlog-live-20261123/).

## `apply -admin NAME` for an existing user that cannot administer

Two temporary API users made by `j4a_users.py` (random passwords, sent once,
never printed or stored): `s4-disabled` (role admin, then disabled) and
`s4-viewer` (role viewer). `apply` ran without `-yes` and with stdin closed, so
it stopped at the confirmation and changed nothing.

| User | Result |
|---|---|
| `s4-disabled` | **warn** api: "first administrator s4-disabled exists but is disabled (skep-api admin exists: user "s4-disabled" exists (role admin, disabled)); setup never changes an existing user, …"; summary "1 warnings"; "not confirmed: nothing was changed", exit 1 |
| `s4-viewer` | **warn** api: "… exists but has the role viewer, not admin (… (role viewer)) …"; same ending |

Both users were deleted afterwards (HTTP 204 each; none left).

## Data-directory refusal

`/var/db/skep` was `drwxr-xr-x root:wheel 755`. `chmod g+w` (775) inside a
subshell whose `trap` runs `chmod 755` on exit:

| Command | Result |
|---|---|
| `check` | "/var/db/skep is writable by its group or others (0775); setup will not use Skep's data directory (chmod go-w /var/db/skep)", **exit 2** |
| `apply` | the same, **exit 2** |
| `upgrade -yes` | "refused: … ; nothing was changed", **exit 3**; both services kept running (same PIDs) |

Restored: `drwxr-xr-x root:wheel 755`, identical to before (checked by
comparing the `stat` line).

**Finding (for Andy):** `check` and `apply` (and `plan`, same path) report this
refusal with exit 2, the usage code, because the error comes from loading the
journals before the plan exists and has no exit code of its own; `upgrade`
uses 3 (refused). Making them 3 would change an exit code, which needs your
approval; not changed.

End state of smith: Skep 571f7b2 (this branch) running, health ok; no test
users; `/var/db/skep` mode restored; edk2-bhyve reinstalled (see #56's
record); no guests.
