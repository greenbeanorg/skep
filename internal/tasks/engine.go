package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Defaults and limits.
const (
	DefaultWorkers   = 4
	DefaultQueueSize = 64
	DefaultPageSize  = 50
	MaxPageSize      = 200
	// storeTimeout bounds the database writes that record a task's outcome.
	// They use their own context so that a cancelled task is still recorded.
	storeTimeout = 10 * time.Second
)

// Options configures an Engine.
type Options struct {
	Store Store
	// Audit records refused submissions, which have no task to attach to.
	Audit audit.Writer
	// NodeID is the local node, recorded on every task.
	NodeID string
	// Workers is the number of tasks that may run at once.
	Workers int
	// QueueSize is the number of pending tasks that may wait for a worker.
	QueueSize int
	Logger    *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

type engineState int

const (
	stateNew engineState = iota
	stateStarted
	stateStopping
)

// runState tracks one running task.
type runState struct {
	cancel          context.CancelFunc
	cancelRequested bool
}

// Engine accepts, runs, and records tasks.
type Engine struct {
	store  Store
	audit  audit.Writer
	nodeID string
	log    *slog.Logger
	now    func() time.Time

	workers int
	queue   chan string
	wg      sync.WaitGroup

	// baseCtx is cancelled at shutdown and is the parent of every handler.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu      sync.Mutex
	state   engineState
	defs    map[string]Definition
	running map[string]*runState
}

// New returns an Engine. Register task types, then call Start.
func New(opts Options) (*Engine, error) {
	if opts.Store == nil || opts.Audit == nil {
		return nil, errors.New("tasks: a store and an audit writer are required")
	}
	if opts.NodeID == "" {
		return nil, errors.New("tasks: node ID must not be empty")
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		store:      opts.Store,
		audit:      opts.Audit,
		nodeID:     opts.NodeID,
		log:        opts.Logger,
		now:        opts.Now,
		workers:    opts.Workers,
		queue:      make(chan string, opts.QueueSize),
		baseCtx:    ctx,
		baseCancel: cancel,
		defs:       map[string]Definition{},
		running:    map[string]*runState{},
	}, nil
}

// Register adds a task type. It must be called before Start.
func (e *Engine) Register(def Definition) error {
	if !typeRE.MatchString(def.Type) || len(def.Type) > maxTypeLength {
		return fmt.Errorf("tasks: invalid task type %q", def.Type)
	}
	if def.Run == nil {
		return fmt.Errorf("tasks: task type %q has no handler", def.Type)
	}
	if def.Timeout <= 0 {
		return fmt.Errorf("tasks: task type %q must declare a timeout", def.Type)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateNew {
		return errors.New("tasks: task types must be registered before Start")
	}
	if _, dup := e.defs[def.Type]; dup {
		return fmt.Errorf("tasks: task type %q is already registered", def.Type)
	}
	e.defs[def.Type] = def
	return nil
}

// Start marks every task left unfinished by a previous run as interrupted,
// then starts the workers. Unfinished work is never resumed: the engine
// cannot know how far it got.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateNew {
		return errors.New("tasks: the engine was already started")
	}

	interrupted, err := e.store.InterruptAll(ctx, e.now().UTC(), CodeAgentRestart,
		"the agent restarted before this task finished; it was not resumed")
	if err != nil {
		return fmt.Errorf("tasks: recover unfinished tasks: %w", err)
	}
	for _, t := range interrupted {
		e.log.Warn("task interrupted by agent restart",
			"task_id", t.ID, "type", t.Type, "resource_type", t.ResourceType, "resource_id", t.ResourceID)
	}

	for range e.workers {
		e.wg.Add(1)
		go e.worker()
	}
	e.state = stateStarted
	e.log.Info("task engine started", "workers", e.workers, "queue_size", cap(e.queue), "interrupted", len(interrupted))
	return nil
}

// Shutdown stops accepting tasks, cancels running handlers, and waits for
// them until ctx is done. Tasks that stop are recorded as interrupted.
// Tasks still running when ctx expires are recorded at the next Start.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if e.state != stateStarted {
		e.state = stateStopping
		e.mu.Unlock()
		e.baseCancel()
		return nil
	}
	e.state = stateStopping
	close(e.queue) // safe: Submit only sends while holding mu in stateStarted
	e.mu.Unlock()
	e.baseCancel()

	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("tasks: shutdown: tasks still running: %w", ctx.Err())
	}
}

func (e *Engine) stopping() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == stateStopping
}

// Submit validates a request and queues it as a pending task.
//
// A request without an actor is refused before anything is written. Any
// other refusal is recorded in the audit log.
//
// If the request carries an idempotency key that was used before for the
// same request (type, resource, payload, and actor), the existing task is
// returned, in whatever state it is, and created is false. The same key
// with a different request is a CodeIdempotencyConflict error. A resource
// that already has an unfinished task yields CodeResourceBusy; the second
// task is refused, not queued.
func (e *Engine) Submit(ctx context.Context, req Request) (task Task, created bool, err error) {
	if req.Actor.ID == "" || req.Actor.Name == "" {
		return Task{}, false, audit.ErrActorRequired
	}

	def, payload, verr := e.validate(req)
	if verr != nil {
		e.refuse(ctx, req, audit.Failure, verr)
		return Task{}, false, verr
	}
	id, err := ids.NewUUID()
	if err != nil {
		return Task{}, false, fmt.Errorf("tasks: generate ID: %w", err)
	}
	t := Task{
		ID:             id,
		NodeID:         e.nodeID,
		Type:           req.Type,
		Status:         models.TaskPending,
		ResourceType:   req.ResourceType,
		ResourceID:     req.ResourceID,
		IdempotencyKey: req.IdempotencyKey,
		Actor:          req.Actor,
		PeerUID:        req.PeerUID,
		RequestID:      req.RequestID,
		RequestHash:    fingerprint(req.Type, req.ResourceType, req.ResourceID, req.Actor.ID, payload),
		Payload:        payload,
		Cancellable:    def.Cancellable,
		CreatedAt:      e.now().UTC(),
	}

	// mu is held across the insert so that the queue capacity check, the
	// insert, and the send are one step, and Shutdown cannot close the
	// queue in between.
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state != stateStarted {
		rerr := &models.Error{Code: models.CodeShuttingDown, Message: "the agent is not accepting tasks"}
		e.refuse(ctx, req, audit.Failure, rerr)
		return Task{}, false, rerr
	}
	// A full queue refuses new tasks, but an idempotent replay creates
	// nothing and must still be answered.
	if len(e.queue) == cap(e.queue) {
		if req.IdempotencyKey != "" {
			existing, found, err := e.store.GetByKey(ctx, req.IdempotencyKey)
			if err != nil {
				return Task{}, false, err
			}
			if found && existing.RequestHash == t.RequestHash {
				return existing, false, nil
			}
		}
		rerr := &models.Error{Code: models.CodeQueueFull, Message: "too many tasks are waiting; try again later"}
		e.refuse(ctx, req, audit.Failure, rerr)
		return Task{}, false, rerr
	}

	stored, created, err := e.store.Create(ctx, t, req.SourceIP)
	if err != nil {
		var me *models.Error
		if errors.As(err, &me) {
			e.refuse(ctx, req, audit.Denied, me)
		}
		return Task{}, false, err
	}
	if !created {
		return stored, false, nil
	}
	e.queue <- stored.ID // never blocks: capacity was checked under mu
	e.log.Info("task accepted", "task_id", stored.ID, "type", stored.Type,
		"actor_id", stored.Actor.ID, "resource_type", stored.ResourceType, "resource_id", stored.ResourceID)
	return stored, true, nil
}

// validate checks a request against the rules and its task type.
func (e *Engine) validate(req Request) (Definition, json.RawMessage, *models.Error) {
	if !typeRE.MatchString(req.Type) || len(req.Type) > maxTypeLength {
		return Definition{}, nil, models.Invalid("invalid task type")
	}
	e.mu.Lock()
	def, known := e.defs[req.Type]
	e.mu.Unlock()
	if !known {
		return Definition{}, nil, models.Invalid("unknown task type " + req.Type)
	}
	if (req.ResourceType == "") != (req.ResourceID == "") {
		return Definition{}, nil, models.Invalid("resource type and resource ID must be set together")
	}
	if req.ResourceType != "" && (!nameRE.MatchString(req.ResourceType) || !nameRE.MatchString(req.ResourceID)) {
		return Definition{}, nil, models.Invalid("invalid resource type or ID")
	}
	if req.IdempotencyKey != "" && !nameRE.MatchString(req.IdempotencyKey) {
		return Definition{}, nil, models.Invalid("invalid idempotency key: use 1 to 128 characters from A-Z a-z 0-9 . _ : -")
	}
	payload, err := canonicalPayload(req.Payload)
	if err != nil {
		return Definition{}, nil, models.Invalid(err.Error())
	}
	if def.Validate != nil {
		if err := def.Validate(payload); err != nil {
			return Definition{}, nil, models.Invalid("invalid payload: " + err.Error())
		}
	}
	return def, payload, nil
}

// refuse records a submission that did not become a task.
func (e *Engine) refuse(ctx context.Context, req Request, outcome audit.Outcome, cause *models.Error) {
	action := req.Type
	if !typeRE.MatchString(action) || len(action) > maxTypeLength {
		action = "task.submit" // never write an arbitrary string as the action
	}
	entry := audit.Entry{
		ActorID:   req.Actor.ID,
		ActorName: req.Actor.Name,
		PeerUID:   req.PeerUID,
		Action:    action,
		Phase:     audit.Requested,
		Outcome:   outcome,
		SourceIP:  req.SourceIP,
		RequestID: req.RequestID,
		Details:   map[string]any{"error_code": cause.Code, "error": cause.Message},
	}
	if nameRE.MatchString(req.ResourceType) && nameRE.MatchString(req.ResourceID) {
		entry.ResourceType, entry.ResourceID = req.ResourceType, req.ResourceID
	}
	e.log.Warn("task refused", "type", action, "actor_id", req.Actor.ID, "error_code", cause.Code)
	if err := e.audit.Write(ctx, entry); err != nil {
		e.log.Error("tasks: write audit entry for refused task", "error", err)
	}
}

// Cancel asks for a task to be cancelled and returns the task as it is
// after the request.
//
// A pending task becomes cancelled at once. A running task has the request
// recorded and its context cancelled; it becomes cancelled when its handler
// stops, unless the handler had already finished, in which case it keeps
// its real outcome. Cancelling a running task whose type is not cancellable
// is refused with CodeNotCancellable. A finished task is returned unchanged.
//
// Cancellation is cooperative and does not roll anything back.
func (e *Engine) Cancel(ctx context.Context, id string, req CancelRequest) (Task, error) {
	if req.Actor.ID == "" || req.Actor.Name == "" {
		return Task{}, audit.ErrActorRequired
	}
	now := e.now().UTC()

	// The task can change state between the read and the write. Each write
	// is conditional, so a lost race is detected and the loop re-reads.
	for range 5 {
		t, err := e.store.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		entry := audit.Entry{
			ActorID:      req.Actor.ID,
			ActorName:    req.Actor.Name,
			PeerUID:      req.PeerUID,
			Action:       "task.cancel",
			ResourceType: "task",
			ResourceID:   t.ID,
			TaskID:       t.ID,
			Phase:        audit.Requested,
			Outcome:      audit.Success,
			SourceIP:     req.SourceIP,
			RequestID:    req.RequestID,
			Details:      map[string]any{"task_type": t.Type, "task_status": string(t.Status)},
		}

		switch {
		case t.Status.Terminal():
			return t, nil

		case t.Status == models.TaskPending:
			cancelled, ok, err := e.store.Transition(ctx, id, Transition{
				From:         models.TaskPending,
				To:           models.TaskCancelled,
				At:           now,
				ErrorCode:    CodeCancelled,
				ErrorMessage: "cancelled before it started",
				Extra:        []audit.Entry{entry},
			})
			if err != nil {
				return Task{}, err
			}
			if ok {
				e.log.Info("task cancelled before it started", "task_id", id, "cancelled_by", req.Actor.ID)
				return cancelled, nil
			}

		case !t.Cancellable:
			rerr := &models.Error{
				Code:    models.CodeNotCancellable,
				Message: "a running " + t.Type + " task cannot be cancelled",
			}
			entry.Outcome = audit.Denied
			entry.Details["error_code"] = rerr.Code
			if err := e.audit.Write(ctx, entry); err != nil {
				e.log.Error("tasks: write audit entry for refused cancel", "error", err)
			}
			return Task{}, rerr

		default: // running and cancellable
			updated, ok, err := e.store.RequestCancel(ctx, id, now, entry)
			if err != nil {
				return Task{}, err
			}
			if ok {
				e.mu.Lock()
				if rs := e.running[id]; rs != nil {
					rs.cancelRequested = true
					rs.cancel()
				}
				e.mu.Unlock()
				e.log.Info("task cancel requested", "task_id", id, "cancelled_by", req.Actor.ID)
				return updated, nil
			}
		}
	}
	return Task{}, fmt.Errorf("tasks: cancel %s: the task kept changing state", id)
}

// Get returns one task.
func (e *Engine) Get(ctx context.Context, id string) (Task, error) {
	return e.store.Get(ctx, id)
}

// Events returns a task's event history, oldest first.
func (e *Engine) Events(ctx context.Context, id string) ([]models.TaskEvent, error) {
	return e.store.Events(ctx, id)
}

// List returns a page of tasks, newest first.
func (e *Engine) List(ctx context.Context, opts models.TaskListOptions) ([]Task, string, error) {
	if opts.Status != "" && !opts.Status.Valid() {
		return nil, "", models.Invalid("unknown task status " + string(opts.Status))
	}
	switch {
	case opts.Limit == 0:
		opts.Limit = DefaultPageSize
	case opts.Limit < 0 || opts.Limit > MaxPageSize:
		return nil, "", models.Invalid(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
	}
	return e.store.List(ctx, opts)
}

func (e *Engine) worker() {
	defer e.wg.Done()
	for id := range e.queue {
		e.runOne(id)
	}
}

// storeCtx returns a context for recording an outcome. It is independent
// of the task's context, which may already be cancelled.
func storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

// runOne runs one queued task to a terminal state.
func (e *Engine) runOne(id string) {
	sctx, scancel := storeCtx()
	defer scancel()

	// Tasks still queued at shutdown never start.
	if e.stopping() {
		if _, _, err := e.store.Transition(sctx, id, Transition{
			From:         models.TaskPending,
			To:           models.TaskInterrupted,
			At:           e.now().UTC(),
			ErrorCode:    CodeAgentShutdown,
			ErrorMessage: "the agent shut down before this task started",
		}); err != nil {
			e.log.Error("tasks: interrupt queued task", "task_id", id, "error", err)
		}
		return
	}

	// Register the cancel function before the task becomes running, so a
	// cancel request that sees it running always finds it here.
	runCtx, cancel := context.WithCancel(e.baseCtx)
	rs := &runState{cancel: cancel}
	e.mu.Lock()
	e.running[id] = rs
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.running, id)
		e.mu.Unlock()
	}()

	task, ok, err := e.store.Transition(sctx, id, Transition{
		From: models.TaskPending, To: models.TaskRunning, At: e.now().UTC(),
	})
	if err != nil {
		e.log.Error("tasks: start task", "task_id", id, "error", err)
		return
	}
	if !ok {
		return // cancelled while it waited
	}

	e.mu.Lock()
	def := e.defs[task.Type]
	e.mu.Unlock()

	timeoutCtx, timeoutCancel := context.WithTimeout(runCtx, def.Timeout)
	result, runErr := e.invoke(timeoutCtx, def, task)
	timedOut := errors.Is(timeoutCtx.Err(), context.DeadlineExceeded)
	timeoutCancel()

	e.mu.Lock()
	cancelRequested := rs.cancelRequested
	e.mu.Unlock()

	tr := e.outcome(task, result, runErr, timedOut, cancelRequested)
	fctx, fcancel := storeCtx()
	defer fcancel()
	if _, ok, err := e.store.Transition(fctx, id, tr); err != nil || !ok {
		e.log.Error("tasks: record task outcome", "task_id", id, "status", tr.To, "applied", ok, "error", err)
		return
	}
	e.log.Info("task finished", "task_id", id, "type", task.Type, "status", tr.To, "error_code", tr.ErrorCode)
}

// invoke runs the handler and turns a panic into an error.
func (e *Engine) invoke(ctx context.Context, def Definition, task Task) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("tasks: handler panic", "task_id", task.ID, "type", task.Type,
				"panic", r, "stack", string(debug.Stack()))
			err = &Failure{Code: CodePanic, Message: "the task failed with an internal error"}
		}
	}()
	return def.Run(ctx, task, &reporter{engine: e, taskID: task.ID})
}

// outcome decides the terminal state of a task from what its handler did.
func (e *Engine) outcome(task Task, result any, runErr error, timedOut, cancelRequested bool) Transition {
	tr := Transition{From: models.TaskRunning, At: e.now().UTC()}

	if runErr == nil {
		// A handler that finished keeps its real outcome, even if a cancel
		// or a shutdown arrived too late to stop it.
		raw, err := json.Marshal(result)
		if err != nil {
			e.log.Error("tasks: encode task result", "task_id", task.ID, "error", err)
			tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskFailed, CodeFailed, "the task result could not be stored"
			return tr
		}
		tr.To, tr.Result = models.TaskSucceeded, raw
		return tr
	}

	var failure *Failure
	switch {
	case errors.As(runErr, &failure):
		tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskFailed, failure.Code, failure.Message
	case e.stopping() && errors.Is(runErr, context.Canceled):
		// Nobody asked for this task to be cancelled: it is interrupted.
		tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskInterrupted, CodeAgentShutdown,
			"the agent shut down while this task was running; it was not resumed"
	case cancelRequested && errors.Is(runErr, context.Canceled):
		tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskCancelled, CodeCancelled, "cancelled while running"
	case timedOut:
		tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskFailed, CodeTimeout, "the task exceeded its time limit"
	default:
		// The cause may contain internal detail; it goes to the log only.
		e.log.Error("task failed", "task_id", task.ID, "type", task.Type, "error", runErr)
		tr.To, tr.ErrorCode, tr.ErrorMessage = models.TaskFailed, CodeFailed, "the task failed; see the agent log"
	}
	return tr
}

// reporter writes a task's progress events.
type reporter struct {
	engine *Engine
	taskID string
}

func (r *reporter) Progress(message string, data any) {
	ctx, cancel := storeCtx()
	defer cancel()
	if err := r.engine.store.AddProgress(ctx, r.taskID, r.engine.now().UTC(), "info", message, data); err != nil {
		r.engine.log.Error("tasks: record progress", "task_id", r.taskID, "error", err)
	}
}
