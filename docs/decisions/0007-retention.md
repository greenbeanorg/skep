# ADR 0007: Retention of tasks, task events, and audit entries

- Status: **Accepted and implemented** (Milestone 4c). The review answers
  and four amendments are under [Decisions](#decisions) and are worked
  into the text. What the implementation added or decided beyond the text
  is under [Implementation notes](#implementation-notes).
- Date: 2026-11-12 (proposed and accepted)
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
  archive file. The database enforces that rule itself. For the agent's
  log, removal is off by default: sealing is automatic, removing live
  entries is the operator's choice.

| # | Decision | Recommendation |
|---|---|---|
| 1 | Task and event retention | Delete finished tasks, with their events, after 90 days; never fewer than 7 days; keep the newest task of each resource |
| 2 | How pruning runs | A daily system task, itself audited, in small batches |
| 3 | Audit retention model | Seal monthly archive segments, whose membership is a range of entry IDs; remove live rows only when sealed and verified: never by default for the agent's log, after 90 days for the API's access log |
| 4 | How the database enforces it | Replace the unconditional no-delete trigger with one that permits a delete only for rows inside a sealed segment |
| 5 | Tamper evidence | Segments are hash-chained; each seal is also written to syslog |
| 6 | Archive files | Skep never deletes them |
| 7 | The API's access audit log | The same mechanism, run by `skep-api` on `api.db`; throttled login attempts aggregated per address and minute |

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
- Milestone 4 added login events, including failed attempts that an
  unauthenticated client can generate at will (rate-limited, ADR 0006).
  Throttled attempts, the ones an attacker can produce fastest, are now
  aggregated into one entry per source address and minute (amendment b).
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

1. **Seal.** After a calendar month (UTC) has ended, the entries of that
   month are written to an archive segment. The entries **stay** in the
   live table. From this moment the month exists twice, and the file can
   be backed up or copied off the host.
2. **Remove.** The live rows of a segment are removed when the segment's
   month ended more than `retention.audit_live_days` ago, **and** the
   segment is sealed, **and** the file has just been re-read and still
   matches its recorded hash. Removal works on whole segments, never on
   single entries.
   - **The agent's audit log: `audit_live_days` defaults to `0`**, which
     means seal, never remove. Removing live entries is opt-in: an
     operator who wants it sets a number of days.
   - **The API's access audit log: `audit_live_days` defaults to `90`.**
     Its entries are sealed and archived like the agent's before they are
     removed (Decision 7).

If an archive file is missing or does not match at removal time, nothing
is removed, the task fails with a specific error code, and the health
check reports the audit archive as degraded. The live rows are the only
good copy in that case and they stay.

### Membership is by entry ID, not by time

A segment is a **contiguous range of entry IDs**. The ID, assigned by the
database in insertion order, decides which segment an entry belongs to.
Timestamps are informational: they choose where a boundary falls and are
recorded in the header, but they never decide membership. Clock skew, a
clock stepped backwards, or an entry written while the month turned
therefore cannot put an entry in two segments, in none, or out of order.

- The segment for month M starts at the ID after the previous segment's
  last ID (or the first ID in the table) and ends at the highest ID whose
  `occurred_at` is before the end of M, as found when the seal runs.
  Every entry inside that range belongs to the segment, whatever its own
  timestamp says; every entry after it belongs to a later segment.
- The header records the earliest and latest `occurred_at` in the range,
  for information. An entry whose timestamp lies outside the month it is
  labelled with is not an error.
- The deletion trigger (Decision 4) already works on ID ranges; so does
  removal, which takes whole segments.

### When sealing is late

The health check reports the audit archive as **degraded** when the
previous month is still unsealed **7 days into the new month**: for
example on the 8th, if last month's segment does not exist. Sealing runs
daily, so this means it has been failing for about a week. It stays
degraded until the seal succeeds. For the agent's log the agent's health
check reports it; for the access log, `skep-api` reports it in the health
document it returns to an authenticated caller.

### A segment

- A file `audit-<node id>-<YYYY-MM>-<first id>-<last id>.jsonl.gz` in
  `/var/db/skep/agent/audit-archive/` (root, `0700`; files `0400`).
- The first line is a header: format version, node ID, the month, the
  first and last entry ID, the number of entries, the earliest and latest
  `occurred_at` (informational), and the hash of the previous segment. Then one JSON object per entry, in ID order, with a
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
`/var/db/skep/api/audit-archive/` owned by `skep`, a timer in `skep-api`.
`skep-api` has no task engine, so its run is recorded as an access audit
entry (action `audit.archive`) instead of a task. Unlike the agent's log,
**the access log keeps 90 days live** by default: entries older than that
are removed once their segment is sealed and verified (Decision 3).

**Throttled login attempts are aggregated** (amendment b). A refused
attempt costs the attacker almost nothing, so one row per attempt would
let an unauthenticated client grow the log as fast as it can send
requests. Instead, throttled attempts are written as **one entry per
source address per minute**, with the number of attempts, the limits that
refused them, and the distinct well-formed usernames (capped). Failed
logins that were actually checked, successful logins, and every other
event are still one entry each. This changes shipped code and was
implemented on its own, before the rest of this ADR; the details are in
[ADR 0006's implementation notes](0006-api-split-authentication-tls.md#implementation-notes).

As ADR 0004 already says, a compromised API can damage its own log and its
own archive. The chain and the syslog anchor make that detectable; they do
not prevent it. The agent's log stays out of its reach.

Sessions are not covered here: expired sessions are deleted by ADR 0006's
sweep and are not audit data.

## Configuration (sketch)

`agent.json`:

```json
"retention": {
  "tasks_days": 90,
  "audit_live_days": 0,
  "audit_archive": true
}
```

`api.json`:

```json
"retention": {
  "audit_live_days": 90,
  "audit_archive": true
}
```

- `tasks_days` (agent only): `0` keeps tasks forever; otherwise at least 7.
- `audit_archive: false`: no segments are sealed and no audit entry is
  ever removed (option A of Decision 3).
- `audit_live_days`: `0` seals segments but never removes live rows (the
  agent's default); otherwise at least 35, so a month can always be
  sealed before it is due for removal. The API's default is 90.

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
- With the default settings the agent's database still grows with its
  audit log (amendment a); only tasks and events are pruned. Operators
  who need it bounded opt in to removal.
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

FreeBSD validation: see [Implementation notes](#implementation-notes) for
what ran on a real host and what is still pending.

## Decisions

Accepted on 2026-11-12 with these answers to the five review questions.

1. **Tasks: 90 days, with a 7-day floor** (Decision 1), as proposed.
2. **Audit segments are sealed monthly** (Decision 3). Removal of live
   entries follows amendment (a) instead of the proposed 400 days.
3. **"Append-only" is redefined** as the live table plus sealed segments,
   **with the trigger as proposed** (Decision 4).
4. **Per-entry hash chaining is a follow-up** (Decision 5), not part of
   this work.
5. **Skep never deletes archive segments** (Decision 6).

Four amendments were made at acceptance:

- **a. Defaults.** The agent's audit log: `audit_live_days = 0`, so
  segments are sealed and live entries are never removed; removal is
  opt-in. The API's access log: 90 days live, sealed and archived before
  removal. The proposal had 400 days for both.
- **b. Throttled login attempts are aggregated** into one access audit
  entry per source address per minute, with a count, not one row per
  attempt. Recorded in ADR 0006's implementation notes too, and
  implemented now, separately, because it fixes shipped code.
- **c. Segment membership is defined by entry ID ranges.** Timestamps are
  informational, so clock skew cannot misplace an entry.
- **d. Late sealing is visible.** The health check reports the audit
  archive as degraded if the previous month is still unsealed 7 days into
  the new month.

Details filled in while applying the amendments, open to correction:

- How a boundary is chosen with ID ranges: the highest ID whose
  `occurred_at` is before the end of the month, at the time of sealing;
  removal works on whole segments, using the segment's month for the age.
- Where late sealing is reported: the agent's health check for its own
  log; `skep-api`'s authenticated health document for the access log.
- What an aggregated entry holds: the count, the limits that refused the
  attempts, and up to ten distinct well-formed usernames.

## Implementation notes

Implemented in Milestone 4c. This section records what the code does where
the text above left room, and what was added. Each point is open to
correction like the details listed under Decisions.

### Where things are

| Part | Location |
|---|---|
| Schema: `audit_segments`, the conditional delete trigger, the task delete rules | `migrations/sqlite/agent/0003_retention.sql`, `migrations/sqlite/api/0003_retention.sql` |
| Segment format, seal, verify, remove, health | `internal/auditarchive` |
| Task pruning | `database.TaskStore.PruneFinished` |
| The `maintenance.retention` task and its timer | `internal/retention`, wired in `cmd/skep-agent` |
| The access log's daily run | `cmd/skep-api/retention.go` |
| `audit verify`, `audit export` | `internal/auditcmd`, both commands |
| Native test | `tests/integration/retention` |

### The schema enforces more than the sketch

- **Events leave only with their task.** The ADR asked for a trigger that
  protects the events of an unfinished task. The trigger refuses every
  direct delete of an event whose task still exists, finished or not; the
  cascade works because the task row is gone by then.
- **Nothing is inserted into a sealed range.** A row inserted with an
  explicit old ID would count as archived, and be removable, without
  being in any file. A trigger refuses it.
- **A segment must match the live table when it is sealed**: its last
  entry exists and its count equals the number of entries in its range.
  Together with "starts right after the previous one" this is checked by
  the database at the moment of the insert, not only by the code.
- **One segment per month, months strictly increasing**, and each row
  carries the previous row's hash; the database refuses a row that does
  not. `audit_segments` also records the node ID and a sequence number.

One consequence of "months strictly increasing": if the clock runs far
ahead, a segment is sealed for a future month, and after the clock is
corrected nothing more is sealed until that month has passed. Nothing is
lost, since unsealed entries cannot be removed, and the health check
reports it after 7 days. The alternative, month labels that go backwards,
was judged worse for whoever reads the archive.

### The segment file

- Line 1 is the header: `format` (1), `log` (`agent` or `api`), `node_id`,
  `month`, `first_id`, `last_id`, `entries`, `earliest_occurred_at`,
  `latest_occurred_at`, `previous_hash`.
- Every further line is one entry: all columns of `audit_log` in a fixed
  order, `NULL` as `null`, and `details` as JSON, not as a string.
- The hash is the SHA-256 of these uncompressed bytes. The file is one
  gzip stream; anything appended to it changes the hash or fails to
  decompress.
- Written as `.tmp-<name>` with mode `0400`, synced, renamed, the
  directory synced, then read back.

### Interruptions

| Stopped after | State | Next run |
|---|---|---|
| the temporary file is written | `.tmp-` file, no seal | removes the temporary file and writes the segment again |
| the rename | a file with the final name, no seal | writes the segment again over it: a file that is not in the index is not a segment |
| the seal row | sealed | nothing to do |
| a removal batch | the segment is partly removed from the live table | removes the rest |

In one case the unrecorded file is not overwritten: an entry with an old
timestamp arrived before the next run, so the range and with it the file
name grew. The old file stays, is never deleted by Skep, and `audit
verify` lists it as a stray file.

### What a run does when the archive is not intact

The daily run verifies every segment first.

- **A file that does not match the index, or a broken chain:** the run
  fails (`audit_archive_invalid`), seals nothing, removes nothing, and
  health is degraded until a run succeeds.
- **A file that is absent:** always reported in the run's result and in
  the log. It fails the run (`audit_archive_missing`) only when that
  segment's live entries are due for removal; they are then kept. Reason:
  decision 6 lets the operator move segments to other storage. With the
  agent's default, which never removes, moving old files must not turn
  every daily run into a failure. `audit verify` on the command line is
  strict and needs `--allow-missing`.
- Tasks are pruned in the same run regardless: they do not depend on the
  archive.

### Other details

- **Removal age.** A segment's live entries are removed when the end of
  its month plus `audit_live_days` has passed, in batches of 500 entries
  per transaction.
- **Late sealing.** Degraded from the 8th day of a month when no segment
  exists for the previous month or later and an unsealed entry is stamped
  before the current month. An entry written after its month was sealed,
  with an old stamp, belongs to the next segment and does not count.
- **Health and HTTP.** As with the other checks, a degraded audit archive
  makes `GET /api/v1/health` answer `503`. The agent's check is
  `audit_archive`; skep-api's is `access_audit_archive`.
- **The retention task** uses the resource `maintenance/retention`, so two
  runs never overlap and, by the "newest task of each resource" rule, the
  last run is always on record. It is submitted after the startup
  discovery task and then every 24 hours. What it removed is in its
  result and, through `tasks.Audited`, in its `finished` audit entry,
  also when it fails.
- **The `finished` audit entry already carried the error code**
  (`details.error_code`); the check asked for in decision 1 found nothing
  missing.
- **The node ID in skep-api's segments** comes from the agent, asked as
  the actor `skep-api`. When the agent is down, the ID recorded with the
  previous segment is used; with no previous segment the run fails and is
  tried again after an hour.
- **Each skep-api run is one access audit entry** (`audit.archive`), also
  when it had nothing to do: about one a day and one per start.
- **Syslog.** Each service writes its seals itself at `daemon.notice`
  (`audit segment sealed: log=... month=... first_id=... last_id=...
  entries=... sha256=... previous=...`), separately from its JSON log,
  which `daemon(8)` forwards at `info`. Not in dev mode.
- **Configuration.** The archive directory is `audit-archive` next to the
  database file and is not a setting. `retention.tasks_days` in
  `api.json` is refused.
- **The audit commands do not migrate.** On a database from before this
  milestone they say that nothing is archived yet, and change nothing.
- **The clock** is injected everywhere (`Now`); nothing in retention reads
  the system clock directly, and no test changes it.

### Not done

- `VACUUM` is not run; shrinking a database file is a manual step
  (documented in the service guide).
- Per-entry hash chaining (decision 5, option C) and an API for archived
  entries remain follow-ups.
- The archive directories are not yet part of a backup design.

### Validation

Unit tests (Linux, with the race detector), for both logs where it
applies: every delete the triggers must refuse, with the reason checked;
overlapping, non-contiguous, out-of-order, and chain-breaking segments;
an upgrade of a database that already holds entries; month and year
boundaries to the millisecond with an injected clock; skewed timestamps;
an interruption after the file write, after the rename, after the seal
row, and between removal batches; ten ways a segment file can stop
matching the index; pruning that never touches unfinished tasks or the
newest task of a resource; a replayed idempotency key before and after
its task is deleted; the task run through the real engine on both sides
of a month boundary.

FreeBSD 15.1-RELEASE-p6 (hearst), under the rc.d services, see
[tests/integration/retention](../../tests/integration/retention/README.md):

| What | State |
|---|---|
| Archive directory ownership and modes (`root:wheel 0700` and `skep:skep 0700`, files `0400`), closed to the other account | Validated on UFS (2026-11-12) |
| Seal: create, write, sync, rename, directory sync, read back | Validated on UFS (2026-11-12) |
| The retention task under the real service, across a restart; an altered segment | Validated on UFS (2026-11-12) |
| Syslog notice of each seal, from root and from the unprivileged account | Validated (2026-11-12) |
| The same with the data directory on a ZFS dataset (in the file-backed test pool `skeptest`) | Validated (2026-11-13) |
| A ZFS-root host | **Pending FreeBSD validation** (the lab VM is UFS-root) |
