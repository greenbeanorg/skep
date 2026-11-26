package internalapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/peercred"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeEngine is the drain and the key lookup.
type fakeEngine struct {
	drain   *tasks.Drain
	sets    []tasks.DrainActor
	cleared int
}

func (f *fakeEngine) SetDrain(_ context.Context, reason string, by tasks.DrainActor) (tasks.Drain, int, error) {
	f.sets = append(f.sets, by)
	d := tasks.Drain{Reason: reason, ExpiresAt: time.Date(2026, 11, 26, 12, 1, 0, 0, time.UTC)}
	f.drain = &d
	return d, 2, nil
}
func (f *fakeEngine) ClearDrain(context.Context, tasks.DrainActor) { f.drain, f.cleared = nil, f.cleared+1 }
func (f *fakeEngine) CurrentDrain(context.Context) (tasks.Drain, bool, int) {
	if f.drain == nil {
		return tasks.Drain{}, false, 0
	}
	return *f.drain, true, 2
}
func (f *fakeEngine) LookupKey(_ context.Context, key string) (tasks.Task, bool, error) {
	if key != "k-1" {
		return tasks.Task{}, false, nil
	}
	return tasks.Task{ID: "task-1", Type: "test.ok", Status: models.TaskSucceeded}, true, nil
}

const apiUID = 409

func tableHandler(eng *fakeEngine) http.Handler {
	return internalapi.NewHandler(&fakeBackend{}, internalapi.Options{Engine: eng, APIUIDs: []uint32{apiUID}, Logger: quiet()})
}

func callAs(h http.Handler, method, path string, peer *peercred.Cred, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, agentproto.Prefix+path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, agentproto.Prefix+path, nil)
	}
	agentproto.Caller{ActorID: "u-1", ActorName: "andy", RequestID: "req-1"}.SetHeaders(req.Header)
	if peer != nil {
		req = req.WithContext(internalapi.WithPeer(req.Context(), *peer))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The table, pinned: a change to who may call the drain fails here and
// must be made on purpose. Every other operation is open to both.
func TestAgentOperationTable(t *testing.T) {
	rootOnly := map[string]bool{"drain-set": true, "drain-clear": true}
	seen := map[string]bool{}
	for _, op := range internalapi.Operations {
		if seen[op.Method+" "+op.Path] || seen[op.Name] {
			t.Errorf("%s (%s %s) is twice in the table", op.Name, op.Method, op.Path)
		}
		seen[op.Method+" "+op.Path], seen[op.Name] = true, true
		want := [2]bool{true, !rootOnly[op.Name]}
		if [2]bool{op.Root, op.API} != want {
			t.Errorf("operation %+v is not in the reviewed table", op)
		}
	}
	if len(internalapi.Operations) != 45 {
		t.Errorf("%d operations; the reviewed table has 45", len(internalapi.Operations))
	}
}

// D2: who may call what, driven from the table, for the operations this
// handler mounts (the core and the engine's): root, the API account and
// another UID. A refused caller never reaches the handler.
func TestAgentOperationsAdmitOnlyTheirCallers(t *testing.T) {
	h := tableHandler(&fakeEngine{})
	callers := []struct {
		name string
		uid  uint32
		may  func(internalapi.Operation) bool
	}{
		{"root", 0, func(o internalapi.Operation) bool { return o.Root }},
		{"skep", apiUID, func(o internalapi.Operation) bool { return o.API }},
		{"another user", 1001, func(internalapi.Operation) bool { return false }},
	}
	tested := 0
	for _, op := range internalapi.Operations {
		if op.Part != "" && op.Part != "engine" {
			continue
		}
		tested++
		path := strings.NewReplacer("{id}", "task-1", "{key}", "k-1").Replace(op.Path)
		body := ""
		if op.Method == http.MethodPost && op.Path == "/drain" {
			body = `{"reason":"upgrade"}`
		}
		for _, c := range callers {
			rec := callAs(h, op.Method, path, &peercred.Cred{UID: c.uid}, body)
			if c.may(op) == (rec.Code == http.StatusForbidden) {
				t.Errorf("%s as %s: %d %s", op.Name, c.name, rec.Code, rec.Body)
			}
		}
		if rec := callAs(h, op.Method, path, nil, body); rec.Code != http.StatusForbidden {
			t.Errorf("%s without a peer: %d", op.Name, rec.Code)
		}
	}
	if tested != 10 {
		t.Errorf("%d core and engine operations tested, want 10", tested)
	}
	// A path of the table with another method is a JSON 405; an unknown
	// path a 404.
	if rec := callAs(h, http.MethodPut, "/drain", &peercred.Cred{UID: 0}, ""); rec.Code != http.StatusMethodNotAllowed ||
		rec.Header().Get("Allow") != "GET, HEAD, POST, DELETE" || !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Errorf("PUT /drain: %d %q %s", rec.Code, rec.Header().Get("Allow"), rec.Body)
	}
	if rec := callAs(h, http.MethodGet, "/nothing", &peercred.Cred{UID: 0}, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

// V6 (HTTP part) and D3: the API account's UID is refused on POST and
// DELETE /drain whatever its headers say; root sets and clears; GET is
// open to both; the answer carries the reason, the expiry and the
// unfinished tasks.
func TestDrainRoutes(t *testing.T) {
	eng := &fakeEngine{}
	h := tableHandler(eng)
	skep := &peercred.Cred{UID: apiUID}
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		rec := callAs(h, m, "/drain", skep, `{"reason":"upgrade"}`)
		var e models.ErrorResponse
		if rec.Code != http.StatusForbidden || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code != "forbidden" {
			t.Fatalf("%s as skep: %d %s", m, rec.Code, rec.Body)
		}
	}
	if len(eng.sets) != 0 || eng.cleared != 0 {
		t.Fatal("a refused caller reached the engine")
	}
	rec := callAs(h, http.MethodPost, "/drain", &peercred.Cred{UID: 0}, `{"reason":"upgrade"}`)
	var st internalapi.DrainStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.Reason != "upgrade" || st.ExpiresAt == nil || st.UnfinishedTasks != 2 {
		t.Fatalf("set as root: %d %s", rec.Code, rec.Body)
	}
	if eng.sets[0].PeerUID != 0 || eng.sets[0].Actor.Name != "andy" || eng.sets[0].RequestID != "req-1" {
		t.Errorf("drain actor: %+v", eng.sets[0])
	}
	if rec := callAs(h, http.MethodPost, "/drain", &peercred.Cred{UID: 0}, `{"reason":"upgrade","x":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := callAs(h, http.MethodGet, "/drain", skep, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reason":"upgrade"`) {
		t.Errorf("GET as skep: %d %s", rec.Code, rec.Body)
	}
	for range 2 { // the second clear finds no drain and answers the same
		if rec := callAs(h, http.MethodDelete, "/drain", &peercred.Cred{UID: 0}, ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "reason") {
			t.Errorf("DELETE as root: %d %s", rec.Code, rec.Body)
		}
	}
	// D7: the task by idempotency key, 404 without one.
	if rec := callAs(h, http.MethodGet, "/tasks/by-key/k-1", skep, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"task-1"`) {
		t.Errorf("by-key: %d %s", rec.Code, rec.Body)
	}
	if rec := callAs(h, http.MethodGet, "/tasks/by-key/k-2", skep, ""); rec.Code != http.StatusNotFound {
		t.Errorf("by-key unknown: %d", rec.Code)
	}
	if rec := callAs(h, http.MethodPost, "/tasks/task-1/cancel", skep, ""); rec.Code != http.StatusOK {
		t.Errorf("cancel beside by-key: %d %s", rec.Code, rec.Body)
	}
}

// The documented table is the code's.
func TestDocumentedAgentOperationTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	yn := map[bool]string{true: "yes", false: "no"}
	rows := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| `") && strings.Contains(line, " "+agentproto.Prefix+"/") {
			rows++
		}
	}
	if rows != len(internalapi.Operations) {
		t.Errorf("the document lists %d operations, the code has %d", rows, len(internalapi.Operations))
	}
	for _, op := range internalapi.Operations {
		row := "| `" + op.Name + "` | `" + op.Method + " " + agentproto.Prefix + op.Path + "` | " + yn[op.Root] + " | " + yn[op.API] + " |"
		if !strings.Contains(string(b), row) {
			t.Errorf("docs/architecture/overview.md lacks the row %q", row)
		}
	}
}
