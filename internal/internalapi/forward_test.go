package internalapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/peercred"
)

// D39 (step 8e-1): the agent refuses, with 400 and before anything is
// recorded or run, an origin that is not a node ID, an envelope over 4096
// characters or with another character, and one without the other. A
// valid pair reaches the cancellation with both values.
func TestForwardedCallerHeaders(t *testing.T) {
	origin := "a1a1a1a1-0000-4000-8000-00000000000a"
	env := "v1.abc_DEF-123.xyz"
	for name, c := range map[string]struct {
		origin, env string
		code        int
	}{
		"both":            {origin, env, 200},
		"neither":         {"", "", 200},
		"origin only":     {origin, "", 400},
		"envelope only":   {"", env, 400},
		"upper-case UUID": {strings.ToUpper(origin), env, 400},
		"not a UUID":      {"star", env, 400},
		"4097 characters": {origin, strings.Repeat("a", 4097), 400},
		"4096 characters": {origin, strings.Repeat("a", 4096), 200},
		"a space":         {origin, "v1.a b.c", 400},
		"a colon":         {origin, "v1.a:b.c", 400},
	} {
		b := &fakeBackend{}
		h := internalapi.NewHandler(b, internalapi.Options{Logger: quiet()})
		req := httptest.NewRequest(http.MethodPost, agentproto.Prefix+"/tasks/task-1/cancel", nil)
		agentproto.Caller{ActorID: "u-1", ActorName: "andy", RequestID: "req-1"}.SetHeaders(req.Header)
		if c.origin != "" {
			req.Header.Set(agentproto.HeaderOriginNode, c.origin)
		}
		if c.env != "" {
			req.Header.Set(agentproto.HeaderEnvelope, c.env)
		}
		req = req.WithContext(internalapi.WithPeer(req.Context(), peercred.Cred{UID: 409}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			continue
		}
		b.mu.Lock()
		got := b.cancel
		b.mu.Unlock()
		if c.code == 400 && got.Actor.ID != "" {
			t.Errorf("%s: the cancellation ran", name)
		}
		if c.code == 200 && (got.OriginNode != c.origin || got.Envelope != c.env) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
