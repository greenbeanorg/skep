# ADR 0005: The agent's task engine

- Status: **Proposed, awaiting review.** No code implements this yet.
- Date: 2026-11-12

## Summary

Every operation the agent performs on the host runs as a task. A task is a
row in `agent.db` with one of six states, a history of events, and audit
entries naming who asked for it. The engine is deliberately conservative:
it never resumes or retries work on its own, and every state change is
written in the same transaction as its event and its audit entry.

Six points need your decision. They are listed under
[Decisions for review](#decisions-for-review).

## Context

Long-running operations are asynchronous and return a task ID. Milestone 2
builds the engine before any privileged operation exists, so that the first
one (Milestone 6) lands on tested machinery. ADR 0004 fixes three
constraints: the agent owns `agent.db`, every task carries a mandatory
actor asserted by the API, and the agent keeps its own append-only audit
log.

The `tasks`, `task_events`, and `audit_log` tables already exist from
migration `agent/0001`.

## States and transitions

```
              submit
                |
                v
   cancel   +---------+
  +---------| pending |
  |         +----+----+
  |              | a worker starts it
  |              v
  |         +---------+
  |         | running |
  |         +----+----+
  |              |
  |    +---------+----------+-------------+
  v    v         v          v             v
cancelled    succeeded    failed     interrupted
```

| From | To | When |
|---|---|---|
| (none) | `pending` | The agent accepted the request |
| `pending` | `running` | A worker picked it up |
| `pending` | `cancelled` | Cancelled before it started |
| `running` | `succeeded` | The handler returned without error |
| `running` | `failed` | The handler returned an error, or exceeded its time limit |
| `running` | `cancelled` | A cancel was requested and the handler stopped because of it |
| `pending`, `running` | `interrupted` | The agent stopped or restarted (see below) |

`succeeded`, `failed`, `cancelled`, and `interrupted` are terminal. A
terminal task never changes again. There is no transition back to `pending`
and the engine has no retry: retrying means submitting a new task.

Transitions are enforced twice. The engine changes state with a conditional
update (`... WHERE id = ? AND status = ?`), so two writers cannot both win.
A database trigger also rejects any transition not in the table and any
change to a terminal task, so a bug elsewhere cannot corrupt history.

## Persistence

The task row is the truth about a task. Nothing about a task's state is
held only in memory.

**One transaction per state change.** Each transition writes, atomically:

1. the new status and timestamps on the `tasks` row,
2. a `task_events` row recording the change, and
3. an `audit_log` row where the change is auditable (see Audit).

If any of the three fails, none is written. A task therefore cannot be
`running` without an event saying when it started, or finish without an
audit entry.

**Events.** `task_events` is the task's ordered history. There are two
kinds: `state` events, written by the engine with each transition, and
`progress` events, written by the handler as it works (for example
"creating volume"). Events are never updated. Handlers must not put secrets
in events, payloads, or results; all three are readable through the API.

**Result and error.** A succeeded task stores a JSON result. A failed,
cancelled, or interrupted task stores a stable error code and a message.
The message is written for an administrator and contains no internal
detail that the log already has.

**Schema changes.** Migration `agent/0002` adds what the engine needs and
changes nothing that exists:

- `tasks`: the verified peer UID of the submitter, a hash of the request
  (for idempotency), the time a cancel was requested, and an error code.
- `task_events`: the event kind, and a trigger that rejects updates.
- `audit_log`: a phase (`requested` or `finished`) and the task's terminal
  status.
- Triggers on `tasks` for legal transitions and immutable terminal rows.

**Code layout.** `internal/tasks` holds the engine and depends on a small
`Store` interface. The SQLite implementation is the only code that knows
the tables. This keeps persistence replaceable, as the cluster layer may
need.

## Restart and shutdown

**In-flight work is never resumed.** On start, before the agent accepts any
request, it marks every task that is not terminal as `interrupted`, in one
transaction, with a state event and an audit entry each. The error code
says the agent restarted. The agent then starts with an empty queue.

The agent cannot know how far an interrupted handler got. It might have
created half of a resource. Guessing and continuing is how a management
plane damages a host, so the engine does not guess. What exists on the host
is established afterwards by observing it, and any cleanup or retry is a
new task that someone, or later the controller, submits deliberately.

Two consequences for handlers. A handler must leave things in a state that
later observation can make sense of. And an operation must be safe to
submit again after an interruption, or must fail cleanly when it finds its
own leftovers.

**Graceful shutdown.** On SIGTERM the agent stops accepting tasks, cancels
the context of running handlers, and waits up to the shutdown timeout.
Tasks that stop in time are recorded as `interrupted`, not `cancelled`:
nobody asked for them to be cancelled. Anything still running when the
timeout expires is caught by the startup sweep. A crash is handled the same
way, by the sweep alone.

## Cancellation

Cancellation is a request, made with an actor like any other operation.

- **A pending task** becomes `cancelled` immediately.
- **A running task** has the request recorded and its context cancelled.
  The handler stops at its next safe point. If it stops because of the
  cancel, the task becomes `cancelled`. If it had already finished, the
  task keeps its real outcome: a cancel can lose the race, and the response
  shows the state the task actually reached.
- **A terminal task** is unchanged, and the request returns its state.
  Cancelling twice is harmless.

Cancellation is cooperative. The engine never kills a handler. A handler
may finish a step that is unsafe to abandon before it honours the cancel.

Cancellation does not roll back. A cancelled task may have changed the
host, exactly like an interrupted one, and the same rule applies: observe,
then decide.

## Idempotency keys

A submission may carry an idempotency key, which `skep-api` passes through
from the client. It makes a retried request safe when the client does not
know whether the first attempt arrived.

- The key is optional, limited to 128 characters from `[A-Za-z0-9._:-]`.
- Along with the key, the agent stores a hash of the request: task type,
  resource, payload, and actor.
- **Same key, same request:** the agent returns the existing task, whatever
  state it is in, and creates nothing. This holds after the task is
  terminal: replaying the key of a failed task returns the failed task, not
  a second attempt.
- **Same key, different request:** rejected with a conflict error. A key is
  never rebound.
- The existing `UNIQUE` constraint on the key decides races: two
  simultaneous submissions with one key produce one task.
- A key lives as long as its task row. There is no expiry until task
  retention is designed.

A new attempt after a failure needs a new key. That is deliberate: it makes
a retry a visible, separate decision.

## Audit

Every task writes to the agent's operation audit log. The mandatory fields
from ADR 0004 apply to each entry: `actor_id`, `actor_name` (asserted by
the API), and `peer_uid` (verified by the kernel).

| When | Entry |
|---|---|
| Submission accepted | Phase `requested`, outcome `success`, the task ID |
| Submission refused (invalid, outside host confinement, unknown type) | Phase `requested`, outcome `denied` or `failure`, no task ID; no task is created |
| Task reaches a terminal state | Phase `finished`, the terminal status, outcome `success` only for `succeeded` |
| Cancel requested | Its own entry, naming the actor who cancelled, who may not be the actor who submitted |

Details:

- The action is the task type, for example `vm.start`, and the entry names
  the resource. "Who did what to this VM" is one query.
- The `finished` entry carries the actor and peer UID of the original
  submission. This is also true when the startup sweep writes it: the
  record says whose task was interrupted.
- An idempotent replay creates no task and no new entry.
- A submission without an actor is refused before anything is written. It
  cannot be audited under the schema, and that is the intent.
- Tasks the agent starts itself use the actor `system` and the agent's own
  UID.

Because the audit entry is in the same transaction as the state change, an
operation cannot run without a `requested` entry or end without a
`finished` one.

## Execution

- **Typed handlers.** A task type is registered in code with a payload
  type, a validator, and a handler. An unknown type is refused. There is no
  generic "run this" task (ADR 0004).
- **Bounded concurrency.** A fixed number of workers, set in configuration.
- **One task per resource at a time.** Tasks naming the same resource run
  in submission order, never concurrently.
- **Time limits.** Each task type declares a maximum duration. Exceeding it
  cancels the handler's context and the task becomes `failed` with a
  timeout error code.
- **Panics.** A handler panic is caught, logged, and recorded as `failed`.

## Scope of Milestone 2

- The engine, the store, migration `agent/0002`, and the audit writer.
- One real task type, `discovery.refresh`, which runs host discovery and
  stores the result in the node's observed columns. It is read-only on the
  host and gives the engine something real to run.
- `GET /api/v1/tasks` and `GET /api/v1/tasks/{id}`, read-only, with
  pagination.

**No task can be submitted or cancelled over the network in Milestone 2.**
The API is still unauthenticated until Milestone 4, so there is no actor to
record. Until then, only the agent itself submits tasks, as `system`.

## Alternatives considered

- **Resume tasks after a restart, using checkpoints.** Fewer manual
  retries, but every handler would need to be provably resumable from every
  point, and a wrong resume acts on a host whose state the agent has not
  re-read. Rejected.
- **Automatic retries with backoff.** Hides failures and repeats operations
  that may not be safe to repeat. Left to the caller, and later to the
  controller, where the decision can be made with knowledge of the
  resource.
- **A job queue library.** The engine is small, the semantics above are
  specific, and the standard library is enough. No dependency is added.
- **Tasks in memory, history in the database.** Simpler, but a crash would
  lose the fact that something was in flight, which is exactly what must
  not be lost.

## Decisions for review

1. **Pending tasks at restart.** Recommended: mark them `interrupted` as
   well. They never started, so nothing on the host is half-done, but
   running a queued operation after an outage of unknown length, on a host
   that may have changed, is the kind of surprise this design avoids. The
   alternative is to keep them queued and run them after the restart.
2. **Two tasks for the same resource.** Recommended: run them in order.
   The alternative is to refuse the second as "resource busy", which is
   stricter but pushes queueing onto every client.
3. **Idempotency key scope.** Recommended: one namespace per agent, with a
   conflict if a key is reused for a different request or by a different
   actor. Scoping keys per actor instead would need the `tasks` table
   rebuilt to change its unique constraint, for little gain.
4. **Cancellation is cooperative and does not roll back.** Recommended as
   described.
5. **Audit outcome for `cancelled` and `interrupted`.** Recommended: both
   are recorded with outcome `failure` (the operation did not complete as
   requested) and the exact status alongside. The alternative is to widen
   the outcome values, which changes the meaning of a column the API-side
   log also uses.
6. **Milestone 2 scope.** Recommended as above: tasks are read-only over
   the API, `discovery.refresh` is the only task type, and submission and
   cancellation over the network wait for Milestone 4.
