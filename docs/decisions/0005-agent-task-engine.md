# ADR 0005: The agent's task engine

- Status: Accepted
- Date: 2026-11-12 (proposed and accepted)

## Summary

Every operation the agent performs on the host runs as a task. A task is a
row in `agent.db` with one of six states, a history of events, and audit
entries naming who asked for it. The engine is deliberately conservative:
it never resumes or retries work on its own, and every state change is
written in the same transaction as its event and its audit entry.

The six points that were put to review, and their outcomes, are under
[Decisions](#decisions). The engine is implemented in `internal/tasks`.

## Context

Long-running operations are asynchronous and return a task ID. Milestone 2
builds the engine before any privileged operation exists, so that the first
one lands on tested machinery. ADR 0004 fixes three
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

- **A pending task** becomes `cancelled` immediately, whatever its type:
  nothing has run, so there is nothing unsafe to abandon.
- **Each task type declares whether it is cancellable.** Cancelling a
  running task of a non-cancellable type is refused up front with
  `not_cancellable` (HTTP 409). The request is not accepted and then
  ignored; the task is untouched and the refusal is audited.
- **A running, cancellable task** has the request recorded and its context
  cancelled.
  The handler stops at its next safe point. If it stops because of the
  cancel, the task becomes `cancelled`. If it had already finished, the
  task keeps its real outcome: a cancel can lose the race, and the response
  shows the state the task actually reached.
- **A terminal task** is unchanged, and the request returns its state.
  Cancelling twice is harmless and writes no further audit entry.

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
- Along with the key, the agent stores a fingerprint of the request: task
  type, resource, payload, and actor. The payload is canonicalised first,
  so key order and whitespace do not matter.
- **Same key, same request:** the agent returns the existing task, whatever
  state it is in, and creates nothing. This holds after the task is
  terminal: replaying the key of a failed task returns the failed task, not
  a second attempt.
- **Same key, different request:** rejected with
  `idempotency_key_conflict` (HTTP 409). A key is never rebound. Because
  the actor is part of the fingerprint, the same key sent by a different
  actor is a conflict, not a replay, and does not reveal the other actor's
  task.
- Keys share one namespace per agent.
- Two simultaneous submissions with one key produce one task. Transactions
  take the database write lock when they begin, so the lookup and the
  insert cannot interleave; the `UNIQUE` constraint on the key is the
  backstop.
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
| Submission refused as invalid (unknown type, bad payload) or because the agent cannot take it (queue full, shutting down) | Phase `requested`, outcome `failure`, no task ID; no task is created |
| Submission refused by a rule (resource busy, idempotency conflict) | Phase `requested`, outcome `denied`, no task ID; no task is created |
| Task reaches a terminal state | Phase `finished`, the terminal status, outcome `success` only for `succeeded` |
| Cancel requested | Its own entry, action `task.cancel`, naming the actor who cancelled, who may not be the actor who submitted; outcome `denied` when refused |

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
- **One unfinished task per resource.** A submission for a resource that
  already has a pending or running task is refused with `resource_busy`
  (HTTP 409), and the error names the task that holds the resource. It is
  not queued, for the same reason pending tasks do not survive a restart:
  an operation that runs later, after another one has changed the
  resource, acts on a state the requester never saw. A unique index in the
  database enforces this as well.
- **A bounded queue.** Accepted tasks wait for a free worker in a queue of
  fixed size. When it is full, new submissions are refused with
  `queue_full`; an idempotent replay is still answered.
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

## Decisions

Accepted on 2026-11-12 with these outcomes.

1. **Pending tasks at restart are marked `interrupted`.** Accepted.
2. **A second task on a busy resource is refused**, with `resource_busy`
   (HTTP 409), not queued. This changed the proposal, which ran tasks for
   one resource in order. The reason is the same as for the first decision:
   stale intent.
3. **One idempotency namespace per agent.** Accepted, with the actor
   included in the request fingerprint, so the same key from a different
   actor is a conflict.
4. **Cancellation is cooperative and does not roll back.** Accepted, with
   an addition: each task type declares whether it is cancellable, and
   cancelling a running task of a non-cancellable type is refused up front.
5. **`cancelled` and `interrupted` are audited with outcome `failure`** and
   the exact status alongside. Accepted.
6. **Milestone 2 scope.** Accepted: tasks are read-only over the API, and
   `discovery.refresh`, submitted by `system`, is the only task type.

One reading of decision 4 was made during implementation and is worth
confirming: a *pending* task of a non-cancellable type can still be
cancelled, because its handler has not started. "Non-cancellable" protects
a handler that is running.

## Implementation status

| Part | State |
|---|---|
| Engine: states, conditional transitions, bounded workers and queue, timeouts, panic handling | Done (`internal/tasks`) |
| Store: one transaction per state change with its event and audit entries | Done (`internal/database`) |
| Database rules: legal transitions, immutable finished tasks and request fields, one unfinished task per resource, append-only events | Done (migration `agent/0002`) |
| Startup sweep and graceful shutdown | Done |
| Cancellation, including refusal for non-cancellable types | Done in the engine; not reachable over the network until Milestone 4 |
| Idempotency keys | Done in the engine; not reachable over the network until Milestone 4 |
| Audit writer with mandatory actor and peer UID | Done (`internal/audit`, `internal/database`) |
| `discovery.refresh`, submitted once at agent start | Done |
| `GET /api/v1/tasks`, `GET /api/v1/tasks/{id}` | Done, read-only |
| Task submission and cancellation over the agent socket | Milestone 4 |
| Task and audit retention | Not designed |

The agent refreshes its inventory once at start. A periodic refresh is
deliberately left out until retention exists, because every task adds rows
to `tasks`, `task_events`, and `audit_log` that nothing removes yet.

## FreeBSD validation

Validated on FreeBSD 15.1-RELEASE-p6 (`hearst`, 2026-11-12):

- The engine, store, and audit test suites, cross-compiled and run
  natively, including the restart sweep, shutdown, cancellation,
  idempotency, and the concurrent-submission test.
- The agent itself: the startup `discovery.refresh` task ran as `system`,
  stored the observed node state, and wrote a `requested` and a `finished`
  audit entry with the agent's UID. Task history survived a clean restart
  and a `kill -9`.

Pending FreeBSD validation: an interruption of a real in-flight task on a
real agent. `discovery.refresh` finishes in a few milliseconds, so the
`kill -9` run had nothing in flight; the sweep itself is covered by the
native engine tests. This becomes testable with the first long-running
task type.
