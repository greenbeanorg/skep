package database

import (
	"context"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// A failed task is read back with the details its failure recorded in the
// "finished" audit entry; Public keeps only the reviewed keys. A task
// that succeeded, or failed without details, has none.
func TestTaskErrorDetailsFromTheFinishedEntry(t *testing.T) {
	ctx := context.Background()
	store := NewTaskStore(migratedDB(t))
	at := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	for id, tr := range map[string]tasks.Transition{
		"zvol": {From: models.TaskRunning, To: models.TaskFailed, At: at, ErrorCode: models.CodeDiskDeviceMissing, ErrorMessage: "x",
			AuditDetails: map[string]any{"dataset": "tank/skep/vm/a/disk0", "reason": "volmode_none", "volmode": "none",
				"command": "zfs set volmode=dev tank/skep/vm/a/disk0", "error": "zfs: secret-looking text"}},
		"plainfail": {From: models.TaskRunning, To: models.TaskFailed, At: at, ErrorCode: tasks.CodeTimeout, ErrorMessage: "late"},
		"ok":        {From: models.TaskRunning, To: models.TaskSucceeded, At: at, AuditDetails: map[string]any{"dataset": "d"}},
	} {
		if _, _, err := store.Create(ctx, tasks.Task{
			ID: id, Type: "vm.start", RequestHash: "h", Actor: models.TaskActor{ID: "u1", Name: "andy"},
			Payload: []byte(`{}`), CreatedAt: at,
		}, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := store.Transition(ctx, id, tasks.Transition{From: models.TaskPending, To: models.TaskRunning, At: at}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, ok, err := store.Transition(ctx, id, tr); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	got, err := store.Get(ctx, "zvol")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.ErrorDetails["error_code"]; ok || got.ErrorDetails["reason"] != "volmode_none" || got.ErrorDetails["error"] == nil {
		t.Errorf("stored details %v", got.ErrorDetails)
	}
	pub := got.Public().Error
	want := map[string]any{"dataset": "tank/skep/vm/a/disk0", "reason": "volmode_none", "volmode": "none",
		"command": "zfs set volmode=dev tank/skep/vm/a/disk0"}
	if pub == nil || len(pub.Details) != len(want) {
		t.Fatalf("public error %+v", pub)
	}
	for k, v := range want {
		if pub.Details[k] != v {
			t.Errorf("public %s = %v, want %v", k, pub.Details[k], v)
		}
	}
	if _, ok := pub.Details["error"]; ok {
		t.Error("the flagged key error is exposed")
	}

	list, _, err := store.List(ctx, models.TaskListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list {
		p := task.Public()
		switch task.ID {
		case "zvol":
			if p.Error == nil || p.Error.Details["reason"] != "volmode_none" {
				t.Errorf("list: %+v", p.Error)
			}
		case "plainfail":
			if p.Error == nil || p.Error.Details != nil {
				t.Errorf("list plainfail: %+v", p.Error)
			}
		case "ok":
			if p.Error != nil || task.ErrorDetails != nil {
				t.Errorf("list ok: %+v %v", p.Error, task.ErrorDetails)
			}
		}
	}
}
