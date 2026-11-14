package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TaskStore is the SQLite implementation of tasks.Store. It is the only
// code that knows the tasks and task_events tables.
//
// Every state change runs in one transaction together with its state event
// and its audit entries. Transactions take the write lock when they begin
// (see Open), so the read-then-write steps below cannot interleave.
type TaskStore struct {
	db *sql.DB
}

// NewTaskStore returns the task store for the agent database.
func NewTaskStore(db *sql.DB) *TaskStore { return &TaskStore{db: db} }

var _ tasks.Store = (*TaskStore)(nil)

const taskColumns = `id, COALESCE(node_id, ''), type, status,
	COALESCE(resource_type, ''), COALESCE(resource_id, ''), COALESCE(idempotency_key, ''),
	actor_id, actor_name, peer_uid, COALESCE(request_id, ''), request_hash, payload,
	cancellable, cancel_requested_at, result, COALESCE(error_code, ''), COALESCE(error, ''),
	created_at, started_at, finished_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (tasks.Task, error) {
	var (
		t                                   tasks.Task
		status, payload, created            string
		peerUID                             int64
		cancellable                         int
		cancelAt, result, started, finished sql.NullString
	)
	err := row.Scan(&t.ID, &t.NodeID, &t.Type, &status,
		&t.ResourceType, &t.ResourceID, &t.IdempotencyKey,
		&t.Actor.ID, &t.Actor.Name, &peerUID, &t.RequestID, &t.RequestHash, &payload,
		&cancellable, &cancelAt, &result, &t.ErrorCode, &t.ErrorMessage,
		&created, &started, &finished)
	if err != nil {
		return tasks.Task{}, err
	}
	t.Status = models.TaskStatus(status)
	t.PeerUID = uint32(peerUID)
	t.Payload = json.RawMessage(payload)
	t.Cancellable = cancellable == 1
	if result.Valid {
		t.Result = json.RawMessage(result.String)
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return tasks.Task{}, fmt.Errorf("created_at: %w", err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{cancelAt, &t.CancelRequestedAt}, {started, &t.StartedAt}, {finished, &t.FinishedAt}} {
		if !f.src.Valid {
			continue
		}
		v, err := parseTime(f.src.String)
		if err != nil {
			return tasks.Task{}, err
		}
		*f.dst = &v
	}
	return t, nil
}

// inTx runs fn in a transaction and rolls back on error.
func (s *TaskStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() {
		if err != nil {
			if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

func getTask(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, where string, args ...any) (tasks.Task, bool, error) {
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE `+where, args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tasks.Task{}, false, nil
	case err != nil:
		return tasks.Task{}, false, fmt.Errorf("database: read task: %w", err)
	}
	return t, true, nil
}

func insertStateEvent(ctx context.Context, tx *sql.Tx, id string, at time.Time, from, to models.TaskStatus, level string) error {
	data, _ := json.Marshal(map[string]string{"from": string(from), "to": string(to)})
	msg := "task is " + string(to)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO task_events (task_id, kind, level, message, data, created_at) VALUES (?, 'state', ?, ?, ?, ?)`,
		id, level, msg, string(data), formatTime(at))
	if err != nil {
		return fmt.Errorf("database: write task event: %w", err)
	}
	return nil
}

// Create implements tasks.Store.
func (s *TaskStore) Create(ctx context.Context, t tasks.Task, sourceIP string, companion tasks.Companion) (out tasks.Task, created bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if t.IdempotencyKey != "" {
			existing, found, err := getTask(ctx, tx, `idempotency_key = ?`, t.IdempotencyKey)
			if err != nil {
				return err
			}
			if found {
				if existing.RequestHash != t.RequestHash {
					return &models.Error{
						Code:    models.CodeIdempotencyConflict,
						Message: "this idempotency key was already used for a different request",
					}
				}
				out = existing
				return nil
			}
		}
		if t.ResourceType != "" {
			busy, found, err := getTask(ctx, tx,
				`resource_type = ? AND resource_id = ? AND status IN ('pending', 'running') LIMIT 1`,
				t.ResourceType, t.ResourceID)
			if err != nil {
				return err
			}
			if found {
				return &models.Error{
					Code:    models.CodeResourceBusy,
					Message: "another task is already in progress for this resource",
					Details: map[string]any{"task_id": busy.ID},
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (id, node_id, type, status, resource_type, resource_id,
				idempotency_key, actor_id, actor_name, peer_uid, request_id, request_hash,
				payload, cancellable, created_at)
			VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, nullable(t.NodeID), t.Type, nullable(t.ResourceType), nullable(t.ResourceID),
			nullable(t.IdempotencyKey), t.Actor.ID, t.Actor.Name, int64(t.PeerUID),
			nullable(t.RequestID), t.RequestHash, string(t.Payload), boolInt(t.Cancellable),
			formatTime(t.CreatedAt)); err != nil {
			return fmt.Errorf("database: insert task: %w", err)
		}
		if err := insertStateEvent(ctx, tx, t.ID, t.CreatedAt, "", models.TaskPending, "info"); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, audit.Entry{
			ActorID: t.Actor.ID, ActorName: t.Actor.Name, PeerUID: t.PeerUID,
			Action: t.Type, ResourceType: t.ResourceType, ResourceID: t.ResourceID,
			TaskID: t.ID, Phase: audit.Requested, Outcome: audit.Success,
			SourceIP: sourceIP, RequestID: t.RequestID,
		}, t.CreatedAt); err != nil {
			return err
		}
		// What the task reserves is written with it, or neither is.
		if companion != nil {
			if err := companion(ctx, tx); err != nil {
				return err
			}
		}
		stored, _, err := getTask(ctx, tx, `id = ?`, t.ID)
		out, created = stored, true
		return err
	})
	if err != nil {
		return tasks.Task{}, false, err
	}
	return out, created, nil
}

// Transition implements tasks.Store.
func (s *TaskStore) Transition(ctx context.Context, id string, tr tasks.Transition) (out tasks.Task, ok bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		applied, err := applyTransition(ctx, tx, id, tr)
		if err != nil || !applied {
			return err
		}
		for _, e := range tr.Extra {
			if err := insertAudit(ctx, tx, e, tr.At); err != nil {
				return err
			}
		}
		t, _, err := getTask(ctx, tx, `id = ?`, id)
		out, ok = t, true
		return err
	})
	if err != nil {
		return tasks.Task{}, false, err
	}
	return out, ok, nil
}

// applyTransition changes a task's state if it is still in tr.From, and
// writes the state event and, for a terminal state, the "finished" audit
// entry carrying the original submitter's identity.
func applyTransition(ctx context.Context, tx *sql.Tx, id string, tr tasks.Transition) (bool, error) {
	at := formatTime(tr.At)
	var res sql.Result
	var err error
	switch {
	case tr.To == models.TaskRunning:
		res, err = tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, started_at = ? WHERE id = ? AND status = ?`,
			string(tr.To), at, id, string(tr.From))
	case tr.To.Terminal():
		var result any
		if tr.To == models.TaskSucceeded && len(tr.Result) > 0 {
			result = string(tr.Result)
		}
		res, err = tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, finished_at = ?, result = ?, error_code = ?, error = ?
			 WHERE id = ? AND status = ?`,
			string(tr.To), at, result, nullable(tr.ErrorCode), nullable(tr.ErrorMessage),
			id, string(tr.From))
	default:
		return false, fmt.Errorf("database: unsupported transition to %q", tr.To)
	}
	if err != nil {
		return false, fmt.Errorf("database: update task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}

	level := "info"
	if tr.To == models.TaskFailed || tr.To == models.TaskInterrupted {
		level = "error"
	}
	if err := insertStateEvent(ctx, tx, id, tr.At, tr.From, tr.To, level); err != nil {
		return false, err
	}
	if !tr.To.Terminal() {
		return true, nil
	}

	t, _, err := getTask(ctx, tx, `id = ?`, id)
	if err != nil {
		return false, err
	}
	outcome := audit.Failure
	if tr.To == models.TaskSucceeded {
		outcome = audit.Success
	}
	entry := audit.Entry{
		ActorID: t.Actor.ID, ActorName: t.Actor.Name, PeerUID: t.PeerUID,
		Action: t.Type, ResourceType: t.ResourceType, ResourceID: t.ResourceID,
		TaskID: t.ID, Phase: audit.Finished, Outcome: outcome, TaskStatus: string(tr.To),
		RequestID: t.RequestID,
	}
	// The entry outlives the task (ADR 0007): it carries the error code,
	// and whatever the task says belongs on the record.
	if len(tr.AuditDetails) > 0 || tr.ErrorCode != "" {
		entry.Details = map[string]any{}
		for k, v := range tr.AuditDetails {
			entry.Details[k] = v
		}
		if tr.ErrorCode != "" {
			entry.Details["error_code"] = tr.ErrorCode
		}
	}
	return true, insertAudit(ctx, tx, entry, tr.At)
}

// RequestCancel implements tasks.Store.
func (s *TaskStore) RequestCancel(ctx context.Context, id string, at time.Time, entry audit.Entry) (out tasks.Task, ok bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET cancel_requested_at = COALESCE(cancel_requested_at, ?) WHERE id = ? AND status = 'running'`,
			formatTime(at), id)
		if err != nil {
			return fmt.Errorf("database: record cancel request: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_events (task_id, kind, level, message, created_at) VALUES (?, 'state', 'info', 'cancel requested', ?)`,
			id, formatTime(at)); err != nil {
			return fmt.Errorf("database: write task event: %w", err)
		}
		if err := insertAudit(ctx, tx, entry, at); err != nil {
			return err
		}
		t, _, err := getTask(ctx, tx, `id = ?`, id)
		out, ok = t, true
		return err
	})
	if err != nil {
		return tasks.Task{}, false, err
	}
	return out, ok, nil
}

// InterruptAll implements tasks.Store.
func (s *TaskStore) InterruptAll(ctx context.Context, at time.Time, code, message string) (out []tasks.Task, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, status FROM tasks WHERE status IN ('pending', 'running') ORDER BY created_at, id`)
		if err != nil {
			return fmt.Errorf("database: find unfinished tasks: %w", err)
		}
		type active struct {
			id     string
			status models.TaskStatus
		}
		var found []active
		for rows.Next() {
			var a active
			if err := rows.Scan(&a.id, &a.status); err != nil {
				rows.Close()
				return fmt.Errorf("database: find unfinished tasks: %w", err)
			}
			found = append(found, a)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range found {
			if _, err := applyTransition(ctx, tx, a.id, tasks.Transition{
				From: a.status, To: models.TaskInterrupted, At: at,
				ErrorCode: code, ErrorMessage: message,
			}); err != nil {
				return err
			}
			t, _, err := getTask(ctx, tx, `id = ?`, a.id)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddProgress implements tasks.Store.
func (s *TaskStore) AddProgress(ctx context.Context, id string, at time.Time, level, message string, data any) error {
	var doc any
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("database: encode event data: %w", err)
		}
		doc = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_events (task_id, kind, level, message, data, created_at) VALUES (?, 'progress', ?, ?, ?, ?)`,
		id, level, message, doc, formatTime(at))
	if err != nil {
		return fmt.Errorf("database: write task event: %w", err)
	}
	return nil
}

// GetByKey implements tasks.Store.
func (s *TaskStore) GetByKey(ctx context.Context, key string) (tasks.Task, bool, error) {
	return getTask(ctx, s.db, `idempotency_key = ?`, key)
}

// Get implements tasks.Store.
func (s *TaskStore) Get(ctx context.Context, id string) (tasks.Task, error) {
	t, found, err := getTask(ctx, s.db, `id = ?`, id)
	if err != nil {
		return tasks.Task{}, err
	}
	if !found {
		return tasks.Task{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
	}
	return t, nil
}

// List implements tasks.Store. Tasks are ordered newest first; the cursor
// is the position of the last task of the previous page.
func (s *TaskStore) List(ctx context.Context, opts models.TaskListOptions) ([]tasks.Task, string, error) {
	var (
		where []string
		args  []any
	)
	if opts.Status != "" {
		where = append(where, `status = ?`)
		args = append(args, string(opts.Status))
	}
	if opts.Cursor != "" {
		createdAt, id, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, "", models.Invalid("invalid cursor")
		}
		where = append(where, `(created_at, id) < (?, ?)`)
		args = append(args, createdAt, id)
	}
	query := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	limit := opts.Limit
	if limit <= 0 {
		limit = tasks.DefaultPageSize
	}
	args = append(args, limit+1) // one extra row tells us whether a next page exists

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("database: list tasks: %w", err)
	}
	defer rows.Close()
	var out []tasks.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, "", fmt.Errorf("database: list tasks: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("database: list tasks: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(formatTime(last.CreatedAt), last.ID)
	}
	return out, next, nil
}

func encodeCursor(createdAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + id))
}

func decodeCursor(c string) (createdAt, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", err
	}
	createdAt, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return "", "", errors.New("malformed cursor")
	}
	if _, err := parseTime(createdAt); err != nil {
		return "", "", err
	}
	return createdAt, id, nil
}

// Events implements tasks.Store.
func (s *TaskStore) Events(ctx context.Context, id string) ([]models.TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, level, message, data, created_at FROM task_events WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("database: list task events: %w", err)
	}
	defer rows.Close()
	out := []models.TaskEvent{}
	for rows.Next() {
		var (
			e       models.TaskEvent
			data    sql.NullString
			created string
		)
		if err := rows.Scan(&e.ID, &e.Kind, &e.Level, &e.Message, &data, &created); err != nil {
			return nil, fmt.Errorf("database: list task events: %w", err)
		}
		if data.Valid {
			e.Data = json.RawMessage(data.String)
		}
		if e.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("database: list task events: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: list task events: %w", err)
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maxPruneBatch bounds one PruneFinished call: the IDs are bound as
// statement parameters.
const maxPruneBatch = 1000

// PruneFinished deletes up to limit finished tasks whose finished_at is
// before cutoff, oldest first, with their events, in one transaction. It
// returns how many tasks and events it deleted; zero tasks means nothing
// is left to delete.
//
// The newest finished task of each resource is kept whatever its age, so
// "the last operation on this resource" always has an answer. Unfinished
// tasks are never candidates, and the schema refuses to delete one.
func (s *TaskStore) PruneFinished(ctx context.Context, cutoff time.Time, limit int) (tasksDeleted, eventsDeleted int64, err error) {
	if limit < 1 || limit > maxPruneBatch {
		return 0, 0, fmt.Errorf("database: prune batch must be between 1 and %d", maxPruneBatch)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id FROM tasks t
			WHERE t.status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
			  AND t.finished_at < ?
			  AND (t.resource_type IS NULL OR EXISTS (
			        SELECT 1 FROM tasks n
			        WHERE n.resource_type = t.resource_type AND n.resource_id = t.resource_id
			          AND n.finished_at IS NOT NULL
			          AND (n.finished_at > t.finished_at OR (n.finished_at = t.finished_at AND n.id > t.id))))
			ORDER BY t.finished_at, t.id LIMIT ?`, formatTime(cutoff), limit)
		if err != nil {
			return fmt.Errorf("database: find tasks to prune: %w", err)
		}
		var ids []any
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("database: find tasks to prune: %w", err)
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("database: find tasks to prune: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		in := "(?" + strings.Repeat(", ?", len(ids)-1) + ")"
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE task_id IN `+in, ids...).Scan(&eventsDeleted); err != nil {
			return fmt.Errorf("database: count events to prune: %w", err)
		}
		// The events go with their tasks through the cascade.
		res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id IN `+in, ids...)
		if err != nil {
			return fmt.Errorf("database: prune tasks: %w", err)
		}
		tasksDeleted, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return tasksDeleted, eventsDeleted, nil
}
