# ADR 0007: Retention of tasks, task events, and audit entries

- Status: **Proposed** (not implemented; nothing in this document exists in
  the code yet)
- Date: 2026-11-12
- Depends on: [ADR 0004](0004-privilege-boundary.md) (two audit logs, each
  append-only), [ADR 0005](0005-agent-task-engine.md) (tasks, events, and
  the audit trail of a task).

## Summary

Today nothing is ever removed from `tasks`, `task_events`, or either
`audit_log`. This ADR proposes how each may shrink without weakening what
the audit log promises.

The two kinds of data are treated differently, because they are different
things:

- **Tasks and task events are operational history.** They are deleted by
  age. What happened stays in the audit log.
- **The audit log is the record.** An entry is never changed, and it leaves
  the live database only after it is in a sealed, verified, hash-chained
  archive file. The database enforces that rule itself.

| # | Decision | Recommendation |
|---|---|---|
| 1 | Task and event retention | Delete finished tasks, with their events, after 90 days; never fewer than 7 days; keep the newest task of each resource |
| 2 | How pruning runs | A daily system task, itself audited, in small batches |
| 3 | Audit retention model | Seal monthly archive segments; remove live rows only when sealed and verified, after 400 days |
| 4 | How the database enforces it | Replace the unconditional no-delete trigger with one that permits a delete only for rows inside a sealed segment |
| 5 | Tamper evidence | Segments are hash-chained; each seal is also written to syslog |
| 6 | Archive files | Skep never deletes them |
| 7 | The API's access audit log | The same mechanism, run by `skep-api` on `api.db` |

## Context

What the schema guarantees today (`migrations/sqlite/agent/0001`, `0002`,
and `api/0001`):

- `audit_log` in both databases: triggers reject every `UPDATE` and every
  `DELETE`.
- `task_events`: a trigger rejects every `UPDATE`. Rows are deleted only by
  the cascade from their task.
- `tasks`: a finished task cannot be updated. Nothing prevents a `DELETE`,
  and no code performs one.
- Audit entries hold a copy of the task ID, not a foreign key, precisely so
  that they can outlive the task.
- An idempotency key lives as long as its task row (ADR 0005).

Why this needs a decision now:

- ADR 0005 holds back a periodic inventory refresh because every task adds
  rows that nothing removes.
- Milestone 4 adds login events, including failed attempts that an
  unauthenticated client can generate at will (rate-limited, ADR 0006).
- "Append-only" and "bounded" are in tension, and resolving it by quietly
  dropping a trigger later would be exactly the silent weakening the
  standing rules forbid.

Rough size, to frame the problem. These are estimates, not measurements: a
task with a handful of events and its two audit entries is on the order of
1 to 2 KB. A node running a few hundred tasks a day grows by tens of
megabytes a year, a busy one by a few hundred. Nothing here is urgent; it
must be designed before it is.

## Decision 1: tasks and task events

| Option | Notes |
|---|---|
| A. Keep forever | No design needed; unbounded; blocks periodic tasks indefinitely. |
| B. Delete finished tasks older than a fixed age | One knob, easy to explain and to test. |
| C. Keep the last N tasks, or the last N per resource | Bounded size, but a burst of activity pushes recent history out, and "how far back can I look" has no fixed answer. |
| D. Two ages: progress events first, the task row later | Saves the most space, since events are the bulk. Two knobs and a task detail view that silently loses its middle. |

**Recommendation: B**, with these rules:

- A task is eligible when it is **terminal** and its `finished_at` is older
  than `retention.tasks_days`, default **90**. `0` means keep forever.
- **Minimum 7 days.** A shorter value is a configuration error. Reason: an
  idempotency key is forgotten with its task. A client that replays a key
  after the task is gone creates a second task. Seven days is far beyond
  any retry window, and the limit makes that an explicit property instead
  of an accident.
- **The newest finished task of each resource is kept** whatever its age,
  so "the last operation on this VM" always has an answer.
- **A task that is not finished is never deleted.** A new trigger enforces
  it: `BEFORE DELETE ON tasks WHEN OLD.status IN ('pending', 'running')`
  aborts. A second trigger rejects deleting events of an unfinished task.
- Events go with their task through the existing cascade. They are never
  deleted separately, so a task that exists always has its full history.
- After deletion, `GET /api/v1/tasks/{id}` returns 404. The audit log still
  says who asked for what, on which resource, when, and how it ended. To
  make that sufficient, the `finished` audit entry must carry the task's
  error code in its details; this is checked, and added if missing, when
  this ADR is implemented.

D is not excluded later if event volume turns out to matter. It is an
additive change.

## Decision 2: how pruning runs

| Option | Notes |
|---|---|
| A. A background goroutine that deletes silently | Simple; leaves no trace that history was removed, or how much. |
| B. A system task of its own type, on a timer | Uses the machinery that exists: it has a state, a time limit, and audit entries, and it shows up in the task list. Costs one task row and two audit entries per run. |
| C. An external cron job or `periodic(8)` script running SQL | Splits the rules between the agent and a script; the script would need to open the agent's database. |

**Recommendation: B.**

- Task type `maintenance.retention`, actor `system`, not cancellable,
  submitted once a day and once after start (after the startup sweep).
  A plain timer in the agent; it is not the cluster scheduler.
- It deletes in batches of at most 500 tasks per transaction, so no
  transaction holds the write lock for long and other tasks are not
  delayed.
- Its result, and therefore its `finished` audit entry, records the cutoff
  date and the number of tasks and events removed. Removal of history is
  itself on the record.
- It also performs the audit work of Decision 3 in the same run.
- It does not `VACUUM`. SQLite reuses freed pages, so the file stops
  growing; shrinking it is a manual, documented operation.
- If it fails, nothing is lost: the next run starts from the same rules.

With this in place the reason ADR 0005 gave for not refreshing the
inventory periodically no longer applies. Whether to add a periodic
refresh is a separate decision and is not made here.

## Decision 3: the audit log

| Option | Notes |
|---|---|
| A. Keep every entry in the live table forever | The guarantee is trivially intact. Unbounded. A valid choice for a long time on a small node, and it remains available as a setting. |
| B. Drop the no-delete trigger and delete old entries | Bounded, and the end of the append-only guarantee: after that, an absent entry proves nothing. Rejected. |
| C. Archive, verify, then remove | The record is the live table plus sealed archive files. An entry is removed from the table only when it is provably in a file. |
| D. One database file per period, attached for queries | Rotation without row deletes. But migrations, triggers, and transactions spanning attached files get complicated, and the task engine's "state change and audit entry in one transaction" rule would cross database files. |
| E. Forward to an external system (remote syslog, a log service) and prune locally | Good as an addition; as the only copy it makes the guarantee depend on infrastructure Skep does not control and that may not exist. |

**Recommendation: C**, with A as the behaviour when archiving is disabled.

### Sealing is separate from removal

Two steps, on purpose far apart in time:

1. **Seal.** After a calendar month (UTC) has ended, its entries are
   written to an archive segment. The entries **stay** in the live table.
   From this moment the month exists twice, and the file can be backed up
   or copied off the host.
2. **Remove.** Entries are removed from the live table when they are older
   than `retention.audit_live_days`, default **400** (a full year of
   look-back in the UI and API, plus margin), **and** their segment is
   sealed, **and** the file has just been re-read and still matches its
   recorded hash. `0` means never remove.

If an archive file is missing or does not match at removal time, nothing
is removed, the task fails with a specific error code, and the agent's
health check reports the audit archive as degraded. The live rows are the
only good copy in that case and they stay.

### A segment

- A file `audit-<node id>-<YYYY-MM>-<first id>-<last id>.jsonl.gz` in
  `/var/db/skep/agent/audit-archive/` (root, `0700`; files `0400`).
- The first line is a header: format version, node ID, the month, the
  first and last entry ID, the number of entries, and the hash of the
  previous segment. Then one JSON object per entry, in ID order, with a
  fixed field order.
- The segment's hash is the SHA-256 of the uncompressed bytes, so it does
  not depend on the compression.
- It is written to a temporary name, synced, renamed, and then read back:
  the entry count, the IDs, and the hash are compared with the live table
  before anything is recorded.
- Only then a row is inserted into a new table, `audit_segments`: month,
  first ID, last ID, entry count, file name, hash, previous hash, sealed
  at. That insert is the seal.
- A month with no entries gets no segment. Segment ID ranges never overlap
  and never have gaps between them; the seal step checks both.

### Reading old entries

The API serves the live table only. Archived entries are read on the host
with a command (`skep-agent audit export`), which also works when the
agent is not running. An API for archived entries is not proposed now.

## Decision 4: how the database enforces it

The unconditional rule "no `DELETE` on `audit_log`" is replaced by a
conditional one that the database, not the application, evaluates.

| Option | Notes |
|---|---|
| A. Keep the unconditional trigger; the pruning code drops and recreates it around its delete | The guarantee then rests on application code behaving. A bug or an injected statement in that window deletes anything. Rejected. |
| B. A trigger that allows a delete only for a row covered by a sealed segment | The database refuses to remove an entry that is not in the index of sealed segments, whoever asks. |
| C. Never delete rows; move them to a second table in the same file | No space is saved. |

**Recommendation: B.** In migration `agent/0003`:

```sql
DROP TRIGGER audit_log_no_delete;

CREATE TRIGGER audit_log_delete_only_archived BEFORE DELETE ON audit_log
WHEN NOT EXISTS (
    SELECT 1 FROM audit_segments s
    WHERE OLD.id BETWEEN s.first_id AND s.last_id
)
BEGIN
    SELECT RAISE(ABORT, 'audit_log: an entry may be removed only after it is archived');
END;
```

together with triggers that make `audit_segments` insert-only (no update,
no delete) and reject a new segment whose range overlaps an existing one
or does not start right after the last one. The update trigger on
`audit_log` is unchanged: an entry is never modified, archived or not.

What the trigger cannot check is the file on disk; SQLite cannot read it.
That check is in the code path of Decision 3, and it fails safe: no match,
no removal.

### The guarantee, restated

After this ADR the audit log promises:

1. An entry is never modified.
2. An entry is never removed from the live table unless it is inside a
   sealed segment, and the database enforces this.
3. The index of sealed segments is never modified or shortened.
4. The segments form a hash chain, so a missing, reordered, or altered
   segment is detectable (Decision 5).

"Append-only" then describes the record as a whole, the live table plus
the segments, and no longer the table alone. The wording in
`docs/security/overview.md` and ADR 0004 changes accordingly when this is
implemented.

## Decision 5: tamper evidence

| Option | Notes |
|---|---|
| A. None beyond file permissions | A changed archive file is undetectable. |
| B. Chain the segments: each records the hash of the one before | Cheap; detects altered, missing, and reordered segments, given a trustworthy copy of the latest hash. |
| C. Chain every entry: each row stores the hash of the previous row | Detects tampering inside the live table too. Adds a read and a hash to every audited transaction and makes inserts strictly serial (they already are, in SQLite). |
| D. Sign segments with a node key | Needs key storage that does not exist yet; arrives naturally with the node's mutual-TLS identity. |

**Recommendation: B now**, with two additions:

- **Each seal is logged to syslog** at `notice`, with the month, the ID
  range, the count, and the hash. On a host that forwards syslog, that is
  an off-host anchor for the chain at no cost.
- **`skep-agent audit verify`** walks the index and the files and reports
  the first break. It also runs at the start of every retention task.

C is worth having and is proposed as a follow-up, not folded in here: it
changes the hot path of every task transition and deserves its own
measurement. D waits for node keys.

The limit, stated plainly: none of this holds against root on the host,
who can rewrite the database, the files, and the index together. It
detects accidents, bugs, and partial tampering, and with an off-host
anchor it detects complete tampering too. That is the same boundary ADR
0004 draws.

## Decision 6: archive files

| Option | Notes |
|---|---|
| A. Skep deletes segments older than a configured age | Bounded disk use; Skep then destroys audit records on a timer, and how long they must be kept is a legal and organisational question Skep cannot answer. |
| B. Skep never deletes a segment | Disk use grows by one small compressed file a month. Removing or moving them is the operator's decision and action. |

**Recommendation: B.** `audit verify` accepts a segment that is listed in
the index but absent from disk only when told to (`--allow-missing`), and
always reports it. A segment moved to other storage can be verified there
against the hash in the index.

## Decision 7: the API's access audit log

`api.db` has its own append-only `audit_log` (logins, denials). It grows
with unauthenticated traffic, so it needs retention more than the agent's.

| Option | Notes |
|---|---|
| A. The same mechanism, run by `skep-api` on its own database | Each process keeps custody of its own log, as ADR 0004 requires. |
| B. The agent archives both | The agent would have to open `api.db`, which ADR 0004 rules out. |
| C. Plain age-based deletion for the access log | Simpler, and gives up the guarantee for the log that records who logged in. |

**Recommendation: A**: migration `api/0003`, archive directory
`/var/db/skep/api/audit-archive/` owned by `skep`, the same defaults, a
timer in `skep-api`. `skep-api` has no task engine, so its run is recorded
as an access audit entry (action `audit.archive`) instead of a task.

As ADR 0004 already says, a compromised API can damage its own log and its
own archive. The chain and the syslog anchor make that detectable; they do
not prevent it. The agent's log stays out of its reach.

Sessions are not covered here: expired sessions are deleted by ADR 0006's
sweep and are not audit data.

## Configuration (sketch)

```json
"retention": {
  "tasks_days": 90,
  "audit_live_days": 400,
  "audit_archive": true
}
```

- `tasks_days`: `0` keeps tasks forever; otherwise at least 7.
- `audit_archive: false`: no segments are sealed and no audit entry is
  ever removed (option A of Decision 3).
- `audit_live_days`: `0` seals segments but never removes live rows;
  otherwise at least 35, so a month can always be sealed before it is due
  for removal.

## Alternatives considered for the whole

- **Do nothing until it hurts.** Cheap now. But the first time it hurts is
  on someone's production node, and the pressure then is to drop the
  trigger.
- **Move history out of SQLite altogether** (append-only files from the
  start). Loses the single-transaction rule that ties a state change to
  its audit entry, which ADR 0005 relies on.

## Consequences

- Two new migrations (`agent/0003`, `api/0003`), one new task type, one new
  table per database, and an `audit` subcommand on each binary.
- The meaning of "append-only" is redefined in the documentation. The
  unconditional no-delete trigger, which is simple to reason about, is
  replaced by a conditional one. That is the real cost of this ADR, and
  the reason the alternative of keeping everything (`audit_archive: false`)
  stays available.
- Task history older than the retention age is gone; integrations that
  want it longer must read it before then or rely on the audit log.
- Backups of a node should include the archive directory. This is added
  to the backup design when that milestone starts.

## Testing and FreeBSD validation

Unit tests (Linux and native): the triggers refuse every delete that is
not covered by a segment; overlapping and non-contiguous segments are
refused; sealing is idempotent and survives an interruption at every step
(file written but not recorded; recorded but not yet removed); removal
refuses a missing or altered file; pruning never touches unfinished tasks
or the newest task of a resource; a replayed idempotency key behaves as
documented before and after its task is deleted.

Pending FreeBSD validation, all of it: file creation, sync, and rename
semantics on UFS and ZFS; ownership and modes of the archive directory
under rc.d; the retention task under the real service across a restart.

## Questions for review

1. Decision 1: 90 days for tasks, with a 7-day floor?
2. Decision 3: 400 days of audit entries in the live database, sealed
   monthly?
3. Decision 4: is redefining "append-only" as "live table plus sealed
   segments" acceptable, with the trigger as proposed?
4. Decision 5: per-entry hash chaining as a follow-up, or part of this
   work?
5. Decision 6: Skep never deletes archive segments?
