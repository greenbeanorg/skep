package internalapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/containers"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

const ctrID = "c0c0c0c0-1111-4222-8333-444455556666"

type fakeContainers struct {
	sub    containers.Submission
	create models.ContainerCreate
	update models.ContainerUpdate
	action models.ContainerAction
	clone  models.ContainerClone
	// calls records the snapshot operations: "op id snapID".
	calls []string
}

const snapID = "5a5a5a5a-1111-4222-8333-444455556666"

func (f *fakeContainers) Snapshots(_ context.Context, id string) (models.SnapshotList, error) {
	f.calls = append(f.calls, "list "+id)
	return models.SnapshotList{Items: []models.Snapshot{{ID: snapID, Name: "s1"}}}, nil
}

func (f *fakeContainers) Snapshot(_ context.Context, id, sid string) (models.Snapshot, error) {
	f.calls = append(f.calls, "get "+id+" "+sid)
	return models.Snapshot{ID: sid, Name: "s1"}, nil
}

func (f *fakeContainers) CreateSnapshot(_ context.Context, id string, req models.SnapshotCreate, sub containers.Submission) (models.SnapshotCreated, error) {
	f.calls, f.sub = append(f.calls, "create "+id+" "+req.Name), sub
	return models.SnapshotCreated{Snapshot: models.Snapshot{ID: snapID, Name: req.Name}, Task: models.Task{ID: "t-4"}}, nil
}

func (f *fakeContainers) RollbackSnapshot(_ context.Context, id, sid string, req models.SnapshotRollback, sub containers.Submission) (models.Task, error) {
	f.calls, f.sub = append(f.calls, fmt.Sprintf("rollback %s %s %t", id, sid, req.DestroyNewer)), sub
	return models.Task{ID: "t-5"}, nil
}

func (f *fakeContainers) DeleteSnapshot(_ context.Context, id, sid string, sub containers.Submission) (models.Task, error) {
	f.calls, f.sub = append(f.calls, "delete "+id+" "+sid), sub
	return models.Task{}, &models.Error{Code: models.CodeSnapshotHasClones, Message: "clones"}
}

func (f *fakeContainers) CloneContainer(_ context.Context, id string, req models.ContainerClone, sub containers.Submission) (models.ContainerCreated, error) {
	f.clone, f.sub = req, sub
	if len(req.NICs) == 0 {
		return models.ContainerCreated{}, &models.Error{Code: models.CodeCloneAddressingRequired, Message: "needs addressing"}
	}
	return models.ContainerCreated{Container: models.Container{ID: ctrID, Name: req.Name}, Task: models.Task{ID: "t-6"}}, nil
}

func (f *fakeContainers) Containers(_ context.Context, opts models.ContainerListOptions) (models.ContainerList, error) {
	return models.ContainerList{Items: []models.Container{{ID: ctrID, Name: "web1"}}, NextCursor: opts.Cursor + "n"}, nil
}

func (f *fakeContainers) Container(_ context.Context, id string) (models.Container, error) {
	if id != ctrID {
		return models.Container{}, models.ErrNotFound
	}
	return models.Container{ID: ctrID, Name: "web1", State: models.ContainerRunning}, nil
}

func (f *fakeContainers) ConsoleLog(context.Context, string) (models.ContainerConsoleLog, error) {
	return models.ContainerConsoleLog{Text: "Starting cron.\n"}, nil
}

func (f *fakeContainers) CreateContainer(_ context.Context, req models.ContainerCreate, sub containers.Submission) (models.ContainerCreated, error) {
	f.create, f.sub = req, sub
	if req.SSHAuthorizedKey == "x" {
		return models.ContainerCreated{}, &models.Error{Code: models.CodeContainerSSHUnavailable, Message: "no sshd"}
	}
	return models.ContainerCreated{Container: models.Container{ID: ctrID, Name: req.Name}, Task: models.Task{ID: "t-1", Type: models.TaskContainerCreate}}, nil
}

func (f *fakeContainers) UpdateContainer(_ context.Context, _ string, upd models.ContainerUpdate, sub containers.Submission) (models.Task, error) {
	f.update, f.sub = upd, sub
	return models.Task{}, &models.Error{Code: models.CodeContainerStarted, Message: "started"}
}

func (f *fakeContainers) DeleteContainer(_ context.Context, _ string, sub containers.Submission) (models.Task, error) {
	f.sub = sub
	return models.Task{ID: "t-3", Type: models.TaskContainerDelete}, nil
}

func (f *fakeContainers) Action(_ context.Context, _ string, act models.ContainerAction, sub containers.Submission) (models.Task, error) {
	f.action, f.sub = act, sub
	return models.Task{ID: "t-2", Type: models.TaskContainerStart}, nil
}

func TestContainersThroughTheSocket(t *testing.T) {
	fc := &fakeContainers{}
	path := startWith(t, &fakeBackend{}, me(), internalapi.Options{Containers: fc})
	c := newClient(t, path)
	ctx := callerCtx()

	if list, err := c.Containers(ctx, models.ContainerListOptions{Cursor: "a"}); err != nil || len(list.Items) != 1 || list.NextCursor != "an" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if got, err := c.Container(ctx, ctrID); err != nil || got.State != models.ContainerRunning {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if log, err := c.ContainerConsoleLog(ctx, ctrID); err != nil || log.Text != "Starting cron.\n" {
		t.Fatalf("console log = %+v, %v", log, err)
	}
	created, err := c.CreateContainer(ctx, models.ContainerCreate{Name: "web1", TemplateID: ctrID})
	if err != nil || created.Task.ID != "t-1" || fc.create.Name != "web1" || fc.sub.Actor.Name == "" || fc.sub.PeerUID == 0 && fc.sub.Actor.ID == "" {
		t.Fatalf("create = %+v, %v; sub %+v", created, err, fc.sub)
	}
	_, err = c.CreateContainer(ctx, models.ContainerCreate{Name: "web1", SSHAuthorizedKey: "x"})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeContainerSSHUnavailable {
		t.Fatalf("refused create: %v", err)
	}
	host := "h"
	if _, err := c.UpdateContainer(ctx, ctrID, models.ContainerUpdate{Hostname: &host}); !errors.As(err, &me) || me.Code != models.CodeContainerStarted || *fc.update.Hostname != "h" {
		t.Fatalf("update: %v", err)
	}
	if task, err := c.ContainerAction(ctx, ctrID, models.ContainerAction{Action: "start"}); err != nil || task.ID != "t-2" || fc.action.Action != "start" {
		t.Fatalf("action = %+v, %v", task, err)
	}
	if task, err := c.DeleteContainer(ctx, ctrID); err != nil || task.ID != "t-3" {
		t.Fatalf("delete = %+v, %v", task, err)
	}

	// Snapshots and clones.
	if list, err := c.ContainerSnapshots(ctx, ctrID); err != nil || len(list.Items) != 1 {
		t.Fatalf("snapshots = %+v, %v", list, err)
	}
	if snap, err := c.ContainerSnapshot(ctx, ctrID, snapID); err != nil || snap.ID != snapID {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	if got, err := c.CreateContainerSnapshot(ctx, ctrID, models.SnapshotCreate{Name: "s2"}); err != nil || got.Task.ID != "t-4" {
		t.Fatalf("create snapshot = %+v, %v", got, err)
	}
	if task, err := c.RollbackContainerSnapshot(ctx, ctrID, snapID, models.SnapshotRollback{DestroyNewer: true}); err != nil || task.ID != "t-5" {
		t.Fatalf("rollback = %+v, %v", task, err)
	}
	if _, err := c.DeleteContainerSnapshot(ctx, ctrID, snapID); !errors.As(err, &me) || me.Code != models.CodeSnapshotHasClones {
		t.Fatalf("delete snapshot: %v", err)
	}
	want := []string{"list " + ctrID, "get " + ctrID + " " + snapID, "create " + ctrID + " s2",
		"rollback " + ctrID + " " + snapID + " true", "delete " + ctrID + " " + snapID}
	if strings.Join(fc.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", fc.calls)
	}
	if _, err := c.CloneContainer(ctx, ctrID, models.ContainerClone{Name: "web2"}); !errors.As(err, &me) || me.Code != models.CodeCloneAddressingRequired {
		t.Fatalf("refused clone: %v", err)
	}
	created, err = c.CloneContainer(ctx, ctrID, models.ContainerClone{Name: "web2", SnapshotID: snapID,
		NICs: []models.ContainerNICSpec{{IPv4: &models.ContainerIPv4{Mode: models.IPv4DHCP}}}})
	if err != nil || created.Task.ID != "t-6" || fc.clone.SnapshotID != snapID || fc.clone.NICs[0].IPv4.Mode != models.IPv4DHCP {
		t.Fatalf("clone = %+v, %v (%+v)", created, err, fc.clone)
	}

	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}}
	for _, bad := range []struct{ method, path string }{
		{http.MethodGet, agentproto.Prefix + "/containers/bad%21id"},
		{http.MethodGet, agentproto.Prefix + "/containers/" + ctrID + "/snapshots/bad%21id"},
		{http.MethodGet, agentproto.Prefix + "/containers?limit=0"},
	} {
		if resp, _ := raw(t, path, bad.method, bad.path, hdr); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s = %d", bad.method, bad.path, resp.StatusCode)
		}
	}
	if resp, _ := raw(t, path, http.MethodPost, agentproto.Prefix+"/containers/"+ctrID+"/console-log", hdr); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST console-log = %d", resp.StatusCode)
	}
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/containers/"+ctrID+"/actions", hdr); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET actions = %d", resp.StatusCode)
	}
}
