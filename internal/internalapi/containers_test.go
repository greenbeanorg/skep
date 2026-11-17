package internalapi_test

import (
	"context"
	"errors"
	"net/http"
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

	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}}
	for _, bad := range []struct{ method, path string }{
		{http.MethodGet, agentproto.Prefix + "/containers/bad%21id"},
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
