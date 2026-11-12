-- agent/0002_task_engine: what the task engine needs (ADR 0005).
--
-- Adds columns and rules; changes nothing that 0001 created. The rules are
-- enforced here as well as in the engine, so a bug elsewhere cannot corrupt
-- task history.

-- The verified UID of the socket peer that submitted the task, the request
-- fingerprint used to check idempotent replays, cancellation bookkeeping,
-- and a stable error code. peer_uid and request_hash are mandatory; SQLite
-- cannot add a NOT NULL column without a default, so the insert trigger
-- below enforces it.
ALTER TABLE tasks ADD COLUMN peer_uid INTEGER CHECK (peer_uid IS NULL OR peer_uid >= 0);
ALTER TABLE tasks ADD COLUMN request_hash TEXT;
ALTER TABLE tasks ADD COLUMN cancellable INTEGER NOT NULL DEFAULT 0 CHECK (cancellable IN (0, 1));
ALTER TABLE tasks ADD COLUMN cancel_requested_at TEXT;
ALTER TABLE tasks ADD COLUMN error_code TEXT;

-- A resource has at most one task that is not finished. A second task for a
-- busy resource is refused, never queued.
CREATE UNIQUE INDEX tasks_one_active_per_resource
    ON tasks (resource_type, resource_id)
    WHERE status IN ('pending', 'running')
      AND resource_type IS NOT NULL AND resource_id IS NOT NULL;

CREATE TRIGGER tasks_insert_rules BEFORE INSERT ON tasks
WHEN NEW.status <> 'pending'
  OR NEW.peer_uid IS NULL
  OR NEW.request_hash IS NULL OR NEW.request_hash = ''
BEGIN
    SELECT RAISE(ABORT, 'tasks: a new task must be pending and carry peer_uid and request_hash');
END;

-- Legal transitions:
--   pending -> running | cancelled | interrupted
--   running -> succeeded | failed | cancelled | interrupted
CREATE TRIGGER tasks_transition_rules BEFORE UPDATE OF status ON tasks
WHEN NOT (
       (OLD.status = 'pending' AND NEW.status IN ('running', 'cancelled', 'interrupted'))
    OR (OLD.status = 'running' AND NEW.status IN ('succeeded', 'failed', 'cancelled', 'interrupted'))
)
BEGIN
    SELECT RAISE(ABORT, 'tasks: illegal status transition');
END;

-- A finished task never changes again.
CREATE TRIGGER tasks_terminal_immutable BEFORE UPDATE ON tasks
WHEN OLD.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
BEGIN
    SELECT RAISE(ABORT, 'tasks: a finished task is immutable');
END;

-- What was asked, and by whom, never changes.
CREATE TRIGGER tasks_request_immutable BEFORE UPDATE ON tasks
WHEN NEW.id IS NOT OLD.id
  OR NEW.type IS NOT OLD.type
  OR NEW.node_id IS NOT OLD.node_id
  OR NEW.resource_type IS NOT OLD.resource_type
  OR NEW.resource_id IS NOT OLD.resource_id
  OR NEW.idempotency_key IS NOT OLD.idempotency_key
  OR NEW.actor_id IS NOT OLD.actor_id
  OR NEW.actor_name IS NOT OLD.actor_name
  OR NEW.peer_uid IS NOT OLD.peer_uid
  OR NEW.request_hash IS NOT OLD.request_hash
  OR NEW.payload IS NOT OLD.payload
  OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'tasks: the request fields of a task are immutable');
END;

-- Events are either state changes written by the engine or progress
-- messages written by a handler. They are never edited.
ALTER TABLE task_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'progress'
    CHECK (kind IN ('state', 'progress'));

CREATE TRIGGER task_events_no_update BEFORE UPDATE ON task_events
BEGIN
    SELECT RAISE(ABORT, 'task_events is append-only');
END;

-- Each task writes a 'requested' entry when it is submitted and a
-- 'finished' entry when it ends. task_status holds the exact terminal
-- status; outcome is 'success' only for a succeeded task.
ALTER TABLE audit_log ADD COLUMN phase TEXT NOT NULL DEFAULT 'requested'
    CHECK (phase IN ('requested', 'finished'));
ALTER TABLE audit_log ADD COLUMN task_status TEXT
    CHECK (task_status IS NULL OR task_status IN ('succeeded', 'failed', 'cancelled', 'interrupted'));

CREATE INDEX audit_log_task ON audit_log (task_id);
