package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/peercred"
	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeBackend struct{ health string }

func (f fakeBackend) Health(context.Context) models.Health {
	h := models.Health{Status: models.HealthOK}
	if f.health != "" {
		h.Record("raft", true, f.health)
	}
	return h
}
func (fakeBackend) Status(context.Context) Status { return Status{NodeID: "n1", NodeName: "smith"} }
func (fakeBackend) RaftConfiguration(context.Context) (RaftConfiguration, error) {
	return RaftConfiguration{Servers: []store.Server{{ID: "n1", Address: store.NoAddress, Voter: true}}}, nil
}

const apiUID = 409

func call(h http.Handler, method, path string, peer *peercred.Cred) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if peer != nil {
		req = req.WithContext(WithPeer(req.Context(), *peer))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every operation is tried as root, as the API account, as the service
// account itself and as another user: it answers exactly the callers its
// table entry names.
func TestEveryOperationAdmitsOnlyItsCallers(t *testing.T) {
	h := NewHandler(fakeBackend{}, HandlerOptions{APIUID: apiUID})
	callers := []struct {
		name string
		uid  uint32
		may  func(Operation) bool
	}{
		{"root", 0, func(o Operation) bool { return o.Root }},
		{"skep", apiUID, func(o Operation) bool { return o.API }},
		{"skep-cluster", 410, func(Operation) bool { return false }},
		{"another user", 1001, func(Operation) bool { return false }},
	}
	for _, op := range Operations {
		if !op.Root && !op.API {
			t.Errorf("%s: nobody may call it", op.Name)
		}
		for _, c := range callers {
			rec := call(h, op.Method, op.Path, &peercred.Cred{UID: c.uid, GID: c.uid})
			want := http.StatusForbidden
			if c.may(op) {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Errorf("%s as %s: %d, want %d", op.Name, c.name, rec.Code, want)
			}
			if want == http.StatusForbidden {
				var e models.ErrorResponse
				if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code != CodeForbidden {
					t.Errorf("%s as %s: body %s", op.Name, c.name, rec.Body)
				}
			}
		}
		// Without a verified peer nothing answers.
		if rec := call(h, op.Method, op.Path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s without a peer: %d", op.Name, rec.Code)
		}
		// Every operation of 8a is a read.
		if rec := call(h, http.MethodPost, op.Path, &peercred.Cred{UID: 0}); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s by POST: %d", op.Name, rec.Code)
		}
	}
}

// The table of 8a, pinned: a change to who may call what fails here and
// must be made on purpose.
func TestOperationTable(t *testing.T) {
	want := map[string][2]bool{ // root, skep
		"health":             {true, true},
		"status":             {true, true},
		"raft-configuration": {true, false},
	}
	if len(Operations) != len(want) {
		t.Fatalf("%d operations, want %d", len(Operations), len(want))
	}
	for _, op := range Operations {
		w, ok := want[op.Name]
		if !ok || w != [2]bool{op.Root, op.API} || op.Method != http.MethodGet {
			t.Errorf("operation %+v is not in the reviewed table", op)
		}
	}
}

func TestAPIAccountThatIsRootIsNotTheAPI(t *testing.T) {
	// If the API account were UID 0, a root-only operation stays
	// root-only and nothing opens to "the API".
	h := NewHandler(fakeBackend{}, HandlerOptions{APIUID: 0})
	if rec := call(h, http.MethodGet, PathRaftConfiguration, &peercred.Cred{UID: 0}); rec.Code != http.StatusOK {
		t.Fatalf("root: %d", rec.Code)
	}
}

func TestUnknownPathAndUnhealthy(t *testing.T) {
	h := NewHandler(fakeBackend{health: "no leader"}, HandlerOptions{APIUID: apiUID})
	root := &peercred.Cred{UID: 0}
	if rec := call(h, http.MethodGet, Prefix+"/join", root); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	if rec := call(h, http.MethodGet, PathHealth, root); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy: %d", rec.Code)
	}
}

func serveOn(t *testing.T, allowed []uint32, apiUID uint32) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.sock")
	ln, err := Listen(ListenOptions{Path: path, AllowedUIDs: allowed, MaxConnections: 4})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewHandler(fakeBackend{}, HandlerOptions{APIUID: apiUID}), ConnContext: ConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); ln.Close() })
	return path
}

func TestSocketModeAndAllowedPeer(t *testing.T) {
	me := uint32(os.Getuid())
	if me == 0 {
		t.Skip("runs as an unprivileged user")
	}
	// This user stands in for the API account.
	path := serveOn(t, []uint32{0, me}, me)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket: %v, mode %v", err, info.Mode().Perm())
	}
	c := NewClient(path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if st, err := c.Status(ctx); err != nil || st.NodeName != "smith" {
		t.Fatalf("status over the socket: %+v, %v", st, err)
	}
	if h, err := c.Health(ctx); err != nil || h.Status != models.HealthOK {
		t.Fatalf("health over the socket: %+v, %v", h, err)
	}
	// The root-only operation is refused to the API account, over the
	// real socket with the kernel's credentials.
	_, err = c.RaftConfiguration(ctx)
	var se *StatusError
	if !errors.As(err, &se) || se.HTTPStatus != http.StatusForbidden || se.Code != CodeForbidden {
		t.Fatalf("raft-configuration as the API account: %v", err)
	}
}

func TestSocketRefusesOtherUID(t *testing.T) {
	me := uint32(os.Getuid())
	if me == 0 {
		t.Skip("runs as an unprivileged user")
	}
	// Only root and some other account are allowed: this user is
	// disconnected before any request is read.
	path := serveOn(t, []uint32{0, me + 1}, me+1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := NewClient(path).Status(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a peer outside the allow-list got: %v", err)
	}
}

func TestListenRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, err := Listen(ListenOptions{Path: filepath.Join(dir, "s"), MaxConnections: 1}); err == nil {
		t.Fatal("listened with an empty allow-list")
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := Listen(ListenOptions{Path: file, AllowedUIDs: []uint32{0}, MaxConnections: 1}); err == nil {
		t.Fatal("replaced a regular file with a socket")
	}
}

// The documented table of operations is the code's.
func TestDocumentedOperationTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "architecture", "skep-cluster.md"))
	if err != nil {
		t.Fatal(err)
	}
	yn := map[bool]string{true: "yes", false: "no"}
	rows := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| `") && strings.Contains(line, Prefix+"/") {
			rows++
		}
	}
	if rows != len(Operations) {
		t.Errorf("the document lists %d operations, the code has %d", rows, len(Operations))
	}
	for _, op := range Operations {
		row := "| `" + op.Name + "` | `" + op.Method + " " + op.Path + "` | " + yn[op.Root] + " | " + yn[op.API] + " |"
		if !strings.Contains(string(b), row) {
			t.Errorf("docs/architecture/skep-cluster.md lacks the row %q", row)
		}
	}
}
