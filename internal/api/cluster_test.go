package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Step 8c-1 through the public API: a user change is written through the
// cluster and answered once the copy has it; without quorum it is 503
// no_quorum with details.outcome, an administrator's login answers a
// degraded session, and no password hash reaches a log line or the access
// audit.
func TestUsersThroughTheClusterAndDegradedLogin(t *testing.T) {
	e := freshEnv(t)
	var logs bytes.Buffer
	h := e.handler(newBackend(), &logs)
	rec := e.serve(h, request{method: http.MethodPost, path: Prefix + "/users", role: auth.RoleAdmin,
		body: `{"username":"nora","password":"` + testPassword + `","role":"operator"}`})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec = e.serve(h, request{method: http.MethodPut, path: Prefix + "/users/" + e.ids[auth.RoleViewer], role: auth.RoleAdmin,
		body: `{"password":"another passphrase entirely"}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("password: %d %s", rec.Code, rec.Body)
	}
	// The viewer's session ended with its password change (the copy's
	// epoch rule).
	if rec := e.serve(h, request{method: http.MethodGet, path: Prefix + "/auth/session", role: auth.RoleViewer}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the viewer's session after its password change: %d", rec.Code)
	}

	e.node.Stop()
	for deadline := time.Now().Add(15 * time.Second); e.stack.Copy.Quorum(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no quorum was never noticed")
		}
	}
	body := wantError(t, e.serve(h, request{method: http.MethodPost, path: Prefix + "/users", role: auth.RoleAdmin,
		body: `{"username":"olga","password":"` + testPassword + `"}`}), http.StatusServiceUnavailable, auth.CodeNoQuorum)
	if body.Details["outcome"] != auth.OutcomeNotWritten {
		t.Fatalf("details: %v", body.Details)
	}
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login",
		body: `{"username":"test-admin","password":"` + testPassword + `","session":"bearer"}`})
	var sess models.SessionResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &sess) != nil || !sess.Session.Degraded ||
		sess.Session.ExpiresAt.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Fatalf("administrator login without quorum: %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login",
		body: `{"username":"nora","password":"` + testPassword + `","session":"bearer"}`}), http.StatusServiceUnavailable, auth.CodeNoQuorum)
	if strings.Contains(logs.String(), "$argon2id$") {
		t.Error("a log line carries a password hash")
	}
	if n := e.auditCount(t, `details LIKE '%argon2id%'`); n != 0 {
		t.Errorf("%d access audit entries carry a hash", n)
	}
}
