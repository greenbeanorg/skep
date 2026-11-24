# Proposal: journals left by an earlier install and `remove -revert-host`

Status: proposal, no code.

## Today

Every `skep-setup apply`, `rollback` and `remove` writes a journal under
`/var/db/skep/setup/`. `remove -revert-host` plans its reverts from every
apply journal that was not rolled back (`internal/setup/remove.go`,
`revertHost`): each `sysrc` line an apply added is removed if it still has
setup's value, and reported otherwise.

The journals survive `pkg delete` and `remove -purge-data`. After a
reinstall, the revert plan therefore also lists the lines an **earlier**
install's applies added, including lines that the earlier install's own
`remove -revert-host` already removed, or that the operator changed or
removed since (finding 1 of PR #52, 2026-11-22; the lines seen are in the
clean-install check's results on PR #54). Nothing wrong is removed (a
changed line is reported, never removed), but the plan is longer and less
clear than it should be.

## Options

| Option | How | Size | For | Against |
|---|---|---|---|---|
| **A. Leave it** | Document the extra lines | 0 | Nothing changes; never removes a wrong line | The plan stays confusing after a reinstall |
| **B. A completed revert settles the applies before it** | `revertHost` ignores apply journals older than the newest completed, not rolled back `remove -revert-host` journal: that remove already settled their lines (removed them or reported them) | About 20 lines plus tests | No new state; the journals stay as the record; rolling back that remove brings the older applies back into scope, as it should | An earlier install removed **without** `-revert-host` still has its lines in scope, which is correct (they are still setup's lines) but may surprise |
| **C. Archive the journals on remove** | `remove` (or only `-purge-data`) moves the journals to `setup/archive-<timestamp>/`; `-revert-host` reads only the active ones | About 40 lines | A reinstall starts with a clean journal directory | A remove without `-revert-host` followed by a reinstall leaves the earlier lines unreverted forever; a later rollback of the remove must move them back |
| **D. Clear the journals in the package's deinstall script** | `pkg delete` moves or deletes `/var/db/skep/setup` | About 10 lines of shell | Simple | pkg does not stop on a failing deinstall script (packaging.md); deleting the record of host changes is the opposite of what the journal is for; a `pkg upgrade` must not trigger it |

## Recommendation

**B.** It uses what the journals already record, adds no state, and keeps
the rule that only an unchanged line is removed. The order of journals is
the order of runs, so "older than the last completed revert" is exact.
A rollback of that remove re-opens exactly the applies it had settled.

Reject D: the journal is the only record of what setup did to `rc.conf` and
`loader.conf`, and a package script is the wrong place to discard it.

## Risks of B

- Journal order must be by run time, not by directory name sorting, if the
  two can ever differ (check `journal.go`'s listing before building).
- An interrupted remove (incomplete journal) must not count as completed.
- A revert that left a line because it had changed was "settled" by being
  reported; after B a later plan no longer mentions it. The plan should say
  how many earlier applies it ignored and why, so nothing disappears
  silently.

## Questions for Andy

1. Accept B?
2. Should the plan list the ignored earlier applies by journal ID, or only
   count them?
