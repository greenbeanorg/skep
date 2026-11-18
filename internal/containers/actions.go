package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

var actionTasks = map[string]string{
	models.ContainerActionStart:   models.TaskContainerStart,
	models.ContainerActionStop:    models.TaskContainerStop,
	models.ContainerActionRestart: models.TaskContainerRestart,
}

// actionPayload is the payload of a power task.
type actionPayload struct {
	// TimeoutSeconds is stop.timeout for this stop: the seconds between
	// SIGTERM and SIGKILL. 0 means the configured default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

func (p actionPayload) timeout() time.Duration {
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// Action submits the power task an action asks for. What the container's
// state refuses is refused here, audited, before a task exists; the task
// checks again when it runs. There is no force stop: jail(8) always ends
// with SIGKILL (decision 8).
func (s *Service) Action(ctx context.Context, id string, act models.ContainerAction, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	taskType, ok := actionTasks[act.Action]
	if !ok {
		return models.Task{}, s.refuse(ctx, sub.request("container.action", id, nil),
			models.Invalid(`action must be "start", "stop", or "restart"`))
	}
	req := sub.request(taskType, id, nil)
	var p actionPayload
	if act.Timeout != nil {
		switch {
		case taskType == models.TaskContainerStart:
			return models.Task{}, s.refuse(ctx, req, models.Invalid("timeout applies to stop and restart only"))
		case *act.Timeout < 1 || *act.Timeout > models.MaxContainerStopTimeoutSeconds:
			return models.Task{}, s.refuse(ctx, req, models.Invalid(fmt.Sprintf("timeout must be between 1 and %d seconds", models.MaxContainerStopTimeoutSeconds)))
		}
		p.TimeoutSeconds = *act.Timeout
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return models.Task{}, err
	}
	req.Payload = payload
	if err := s.checkAction(ctx, id, taskType); err != nil {
		return models.Task{}, s.refuse(ctx, req, err)
	}
	task, _, err := s.engine.Submit(ctx, req)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// checkAction refuses an action the container's state does not allow now.
func (s *Service) checkAction(ctx context.Context, id, taskType string) error {
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !c.Provisioned {
		return notProvisioned(c)
	}
	if c.State == models.ContainerFailed {
		return quarantinedRefusal(c)
	}
	running, err := s.isRunning(ctx, id)
	if err != nil {
		return err
	}
	if taskType == models.TaskContainerStart {
		if running {
			return notStopped(c)
		}
		if len(c.NICs) > 0 {
			if err := s.jails.EpairAvailable(ctx); err != nil {
				return err
			}
		}
		if err := s.checkMACs(ctx, c); err != nil {
			return err
		}
		if err := s.checkAddressFree(ctx, c); err != nil {
			return err
		}
		return s.checkLimits(ctx, c.Limits, c.Options)
	}
	if !running {
		return &models.Error{Code: models.CodeContainerNotRunning, Message: "the jail is not running",
			Details: map[string]any{"container_id": c.ID}}
	}
	return nil
}

// quarantinedRefusal refuses an action on a quarantined jail (amendment
// n, SR-06): a start refused it before any of its code ran and its
// removal failed, so nothing of it may run, /etc/rc.shutdown included.
// Only a delete, or the agent's start-up sweep, removes it.
func quarantinedRefusal(c models.Container) error {
	return &models.Error{Code: models.CodeContainerQuarantined,
		Message: "the jail is quarantined: a start refused it before any of its code ran, and removing it failed; " +
			"Skep does not start or stop it (a stop would run its /etc/rc.shutdown). Delete it, or restart the agent, " +
			"to remove it without running any of its code",
		Details: map[string]any{"container_id": c.ID, "state_detail": c.StateDetail}}
}

// checkStopAtRunTime refuses, when a stop or restart task runs, a jail
// still recorded starting. Tasks on one container run one at a time, so
// no start is under way: the last one ended without recording its outcome
// (a database failure), and the jail may be one the start refused. It is
// handled like a quarantined jail; the agent's start-up sweep removes it
// without exec.stop (amendment n, SR-06).
func checkStopAtRunTime(c models.Container) error {
	if c.State == models.ContainerStarting {
		return quarantinedRefusal(c)
	}
	return nil
}

// ActionResult is the result of a power task.
type ActionResult struct {
	ContainerID string `json:"container_id"`
	State       string `json:"state"`
	JID         int    `json:"jid,omitempty"`
	// Epairs are the host ends of the jail's epairs by slot.
	Epairs map[int]string `json:"epairs,omitempty"`
}

func (s *Service) runStart(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	if err := s.checkAction(ctx, task.ResourceID, models.TaskContainerStart); err != nil {
		return nil, failure(err)
	}
	c, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	return s.start(ctx, c, report)
}

// start marks the container started for good, then starts the jail.
func (s *Service) start(ctx context.Context, c models.Container, report tasks.Reporter) (any, error) {
	// started_once is set before any code runs in the tree, so a crash
	// after jail -c can never leave it 0 (decision 13).
	if err := s.store.MarkStarting(ctx, c.ID, s.now().UTC()); err != nil {
		return nil, err
	}
	report.Progress("starting the jail (jail -c, the /dev guard, /etc/rc)", nil)
	st, err := s.jails.StartJail(ctx, s.spec(c, 0))
	at := s.now().UTC()
	// Recorded on its own context: what the host did must be recorded.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err != nil {
		state := models.ContainerStopped
		var me *models.Error
		if errors.As(err, &me) && me.Code == models.CodeContainerQuarantined {
			// The jail may still exist; it must never be observed running
			// or stopped with exec.stop (amendment n, SR-06).
			state = models.ContainerFailed
		}
		if serr := s.store.SaveObserved(rctx, c.ID, state, nil, "the last start failed: "+err.Error(), at); serr != nil {
			s.log.Error("containers: could not record a failed start", "container_id", c.ID, "state", state, "error", serr)
		}
		if serr := s.store.SaveEpairs(rctx, c.ID, nil); serr != nil {
			s.log.Warn("containers: could not clear the observed epairs", "container_id", c.ID, "error", serr)
		}
		return nil, failure(err)
	}
	jid := st.JID
	if err := s.store.SaveObserved(rctx, c.ID, models.ContainerRunning, &jid, "", at); err != nil {
		return nil, err
	}
	if err := s.store.SaveEpairs(rctx, c.ID, st.Epairs); err != nil {
		return nil, err
	}
	return ActionResult{ContainerID: c.ID, State: models.ContainerRunning, JID: jid, Epairs: st.Epairs}, nil
}

func (s *Service) runStop(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p actionPayload
	_ = json.Unmarshal(task.Payload, &p)
	if err := s.checkAction(ctx, task.ResourceID, models.TaskContainerStop); err != nil {
		return nil, failure(err)
	}
	c, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	if err := checkStopAtRunTime(c); err != nil {
		return nil, failure(err)
	}
	if err := s.stop(ctx, c, p.timeout(), report); err != nil {
		return nil, err
	}
	return ActionResult{ContainerID: c.ID, State: models.ContainerStopped}, nil
}

// stop records the wish and removes the jail: exec.stop, then SIGTERM,
// then SIGKILL after the timeout.
func (s *Service) stop(ctx context.Context, c models.Container, timeout time.Duration, report tasks.Reporter) error {
	at := s.now().UTC()
	if err := s.store.SetDesired(ctx, c.ID, models.ContainerStopped, at); err != nil {
		return err
	}
	if err := s.store.SaveObserved(ctx, c.ID, models.ContainerStopping, c.JID, "", at); err != nil {
		return err
	}
	report.Progress("stopping the jail (/etc/rc.shutdown, then SIGTERM and SIGKILL)", nil)
	err := s.jails.StopJail(ctx, s.spec(c, timeout))
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err != nil {
		// The jail may still be there; the next read observes it.
		if serr := s.store.SaveObserved(rctx, c.ID, models.ContainerUnknown, nil, "the last stop failed: "+err.Error(), s.now().UTC()); serr != nil {
			s.log.Warn("containers: could not record a failed stop", "container_id", c.ID, "error", serr)
		}
		return failure(err)
	}
	if err := s.store.SaveEpairs(rctx, c.ID, nil); err != nil {
		return err
	}
	return s.store.SaveObserved(rctx, c.ID, models.ContainerStopped, nil, "", s.now().UTC())
}

func (s *Service) runRestart(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p actionPayload
	_ = json.Unmarshal(task.Payload, &p)
	if err := s.checkAction(ctx, task.ResourceID, models.TaskContainerRestart); err != nil {
		return nil, failure(err)
	}
	c, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	if err := checkStopAtRunTime(c); err != nil {
		return nil, failure(err)
	}
	if err := s.stop(ctx, c, p.timeout(), report); err != nil {
		return nil, err
	}
	if err := s.checkLimits(ctx, c.Limits, c.Options); err != nil {
		return nil, failure(err)
	}
	return s.start(ctx, c, report)
}
