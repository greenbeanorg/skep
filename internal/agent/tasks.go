package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TaskDiscoveryRefresh is the task type that refreshes the node inventory.
const TaskDiscoveryRefresh = "discovery.refresh"

// NodeResource is the resource type of tasks that operate on a node.
const NodeResource = "node"

// NodeStore records what discovery observed on a node.
type NodeStore interface {
	SaveObserved(ctx context.Context, nodeID string, hw models.Hardware) error
}

// TaskReader is the read side of the task engine.
type TaskReader interface {
	Get(ctx context.Context, id string) (tasks.Task, error)
	Events(ctx context.Context, id string) ([]models.TaskEvent, error)
	List(ctx context.Context, opts models.TaskListOptions) ([]tasks.Task, string, error)
}

// TaskCanceller is the cancel side of the task engine. The engine
// implements it together with TaskReader.
type TaskCanceller interface {
	Cancel(ctx context.Context, id string, req tasks.CancelRequest) (tasks.Task, error)
}

// CancelTask asks for a task to be cancelled on behalf of req.Actor and
// returns the task as it is after the request. The actor is mandatory; the
// engine records it, with the verified peer, in the operation audit log.
// Whether the actor may do this was decided by skep-api: the agent makes
// no authorization decision.
func (s *Service) CancelTask(ctx context.Context, id string, req tasks.CancelRequest) (models.Task, error) {
	canceller, ok := s.tasks.(TaskCanceller)
	if s.tasks == nil || !ok {
		return models.Task{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
	}
	t, err := canceller.Cancel(ctx, id, req)
	if err != nil {
		return models.Task{}, err
	}
	return t.Public(), nil
}

// Tasks returns a page of tasks, newest first. List entries are summaries:
// a task's result and events are returned by Task only.
func (s *Service) Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	list := models.TaskList{Items: []models.Task{}}
	if s.tasks == nil {
		return list, nil
	}
	page, next, err := s.tasks.List(ctx, opts)
	if err != nil {
		return models.TaskList{}, err
	}
	for _, t := range page {
		summary := t.Public()
		summary.Result = nil
		list.Items = append(list.Items, summary)
	}
	list.NextCursor = next
	return list, nil
}

// Task returns one task with its event history.
func (s *Service) Task(ctx context.Context, id string) (models.TaskDetail, error) {
	if s.tasks == nil {
		return models.TaskDetail{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
	}
	t, err := s.tasks.Get(ctx, id)
	if err != nil {
		return models.TaskDetail{}, err
	}
	events, err := s.tasks.Events(ctx, id)
	if err != nil {
		return models.TaskDetail{}, err
	}
	return models.TaskDetail{Task: t.Public(), Events: events}, nil
}

// RefreshResult is the result of a discovery.refresh task.
type RefreshResult struct {
	Platform     string                   `json:"platform"`
	Hostname     string                   `json:"hostname"`
	Capabilities models.CapabilitySummary `json:"capabilities"`
	Warnings     int                      `json:"warnings"`
	CollectedAt  time.Time                `json:"collected_at"`
}

// RefreshDefinition returns the discovery.refresh task type. The task runs
// a fresh, read-only discovery pass and stores the result in the node's
// observed columns. It changes nothing on the host.
func (s *Service) RefreshDefinition() tasks.Definition {
	return tasks.Definition{
		Type:        TaskDiscoveryRefresh,
		Cancellable: true,
		Timeout:     2 * time.Minute,
		Run:         s.refresh,
	}
}

func (s *Service) refresh(ctx context.Context, _ tasks.Task, report tasks.Reporter) (any, error) {
	report.Progress("discovering the host", nil)
	s.mu.Lock()
	hw, err := s.discoverLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	if s.nodes != nil {
		report.Progress("storing the observed state", nil)
		if err := s.nodes.SaveObserved(ctx, s.nodeID, hw); err != nil {
			return nil, err
		}
	}
	s.log.Info("host discovered",
		"node_id", s.nodeID, "hostname", hw.Host.Hostname, "platform", hw.Platform,
		"os", hw.Host.OS.Name, "release", hw.Host.OS.Release, "arch", hw.Host.Architecture,
		"bhyve", hw.Hypervisors.Bhyve.Status, "qemu", hw.Hypervisors.QEMU.Status,
		"zfs", hw.Storage.ZFS.Status)

	return RefreshResult{
		Platform: hw.Platform,
		Hostname: hw.Host.Hostname,
		Capabilities: models.CapabilitySummary{
			Bhyve: hw.Hypervisors.Bhyve.Capability,
			QEMU:  hw.Hypervisors.QEMU.Capability,
			ZFS:   hw.Storage.ZFS.Capability,
		},
		Warnings:    len(hw.Warnings),
		CollectedAt: hw.CollectedAt,
	}, nil
}
