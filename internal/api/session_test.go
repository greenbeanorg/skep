package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

const origin = "https://example.com" // httptest requests are for the host example.com

func loginBody(username, password, session string) string {
	b, _ := json.Marshal(models.LoginRequest{Username: username, Password: password, Session: session})
	return string(b)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	return nil
}

// TestEverythingNeedsASession: without a session only the health status
// and the login are reachable, and nothing is asked of the agent.
func TestEverythingNeedsASession(t *testing.T) {
	e := shared(t)
	b := newBackend()
	h := e.handler(b, nil)
	for _, rq := range []request{
		{method: http.MethodGet, path: "/api/v1/system"},
		{method: http.MethodGet, path: "/api/v1/nodes/node-1/hardware"},
		{method: http.MethodGet, path: "/api/v1/tasks"},
		{method: http.MethodGet, path: "/api/v1/tasks/task-1"},
		{method: http.MethodGet, path: "/api/v1/auth/session"},
		{method: http.MethodPost, path: "/api/v1/auth/logout"},
		{method: http.MethodPut, path: "/api/v1/auth/password", body: `{}`},
		{method: http.MethodHead, path: "/api/v1/system"},
	} {
		for name, headers := range map[string]map[string]string{
			"no credentials":   nil,
			"basic auth":       {"Authorization": "Basic YWRtaW46YWRtaW4="},
			"empty bearer":     {"Authorization": "Bearer "},
			"unknown bearer":   {"Authorization": "Bearer " + strings.Repeat("A", 43)},
			"bearer, no space": {"Authorization": "Bearer"},
		} {
			rq.headers = headers
			rec := e.serve(h, rq)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s, %s: status %d, want 401", rq.method, rq.path, name, rec.Code)
				continue
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: no WWW-Authenticate header", rq.method, rq.path)
			}
			if rq.method != http.MethodHead {
				wantError(t, rec, http.StatusUnauthorized, auth.CodeUnauthenticated)
			}
		}
	}
	if b.caller.ActorID != "" {
		t.Errorf("the backend was called for an unauthenticated request: %+v", b.caller)
	}
}

func TestHealthShowsOnlyTheStatusWithoutASession(t *testing.T) {
	e := shared(t)
	b := newBackend()
	b.health = models.Health{Status: models.HealthOK, Version: "9.9.9", UptimeSeconds: 77,
		Checks: map[string]models.HealthCheck{"database": {Status: models.HealthOK}}}
	h := e.handler(b, nil)

	rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/health"})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Errorf("unauthenticated health: %d %s, want exactly the status", rec.Code, rec.Body)
	}
	// An invalid token is no session either.
	rec = e.serve(h, request{method: http.MethodGet, path: "/api/v1/health", headers: map[string]string{"Authorization": "Bearer nope"}})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "version") {
		t.Errorf("health with a bad token: %d %s", rec.Code, rec.Body)
	}
	b.health.Status = models.HealthDegraded
	b.health.Checks["database"] = models.HealthCheck{Status: models.HealthDegraded, Error: "database is not reachable"}
	rec = e.serve(h, request{method: http.MethodGet, path: "/api/v1/health"})
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"status":"degraded"}` {
		t.Errorf("unauthenticated degraded health: %d %s", rec.Code, rec.Body)
	}
	// With a session: the whole document.
	rec = e.serve(h, request{method: http.MethodGet, path: "/api/v1/health", role: auth.RoleViewer})
	full := decode[models.Health](t, rec)
	if rec.Code != http.StatusServiceUnavailable || full.Version != "9.9.9" || full.Checks["database"].Error == "" {
		t.Errorf("authenticated health = %d %+v", rec.Code, full)
	}
}

// TestLoginGivesOneKindOfSession: a cookie login never shows the token to
// JavaScript, a bearer login sets no cookie, and each token works only
// through its own channel.
func TestLoginGivesOneKindOfSession(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	login := func(session string) *httptest.ResponseRecorder {
		return e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login",
			body: loginBody("test-admin", testPassword, session), headers: map[string]string{"Origin": origin}})
	}

	// Cookie, the default.
	rec := login("")
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie login: %d %s", rec.Code, rec.Body)
	}
	c := sessionCookie(t, rec)
	if c == nil || c.Name != "__Host-skep_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
		c.Path != "/" || c.Domain != "" || len(c.Value) != 43 || c.MaxAge <= 0 || c.MaxAge > 12*3600 {
		t.Fatalf("session cookie = %+v", c)
	}
	if strings.Contains(rec.Body.String(), c.Value) || strings.Contains(rec.Body.String(), `"token"`) {
		t.Errorf("a cookie login returned the token in the body: %s", rec.Body)
	}
	body := decode[models.SessionResponse](t, rec)
	if body.User.Username != "test-admin" || body.User.Role != "admin" || body.Session.Kind != "cookie" ||
		body.Session.IdleTimeoutSeconds != 1800 || len(body.Permissions) != 1 || body.Permissions[0] != "*" {
		t.Errorf("login body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), "password") {
		t.Errorf("the login response leaks password material: %s", rec.Body)
	}
	if got := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", cookie: c.Value}); got.Code != http.StatusOK {
		t.Errorf("GET with the session cookie: %d", got.Code)
	}
	if got := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", headers: map[string]string{"Authorization": "Bearer " + c.Value}}); got.Code != http.StatusUnauthorized {
		t.Errorf("a cookie session's token as a bearer token: %d, want 401", got.Code)
	}

	// Bearer, only on request.
	rec = login("bearer")
	if rec.Code != http.StatusOK || sessionCookie(t, rec) != nil || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("bearer login: %d, Set-Cookie %q; want no cookie", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	body = decode[models.SessionResponse](t, rec)
	if len(body.Token) != 43 || body.Session.Kind != "bearer" {
		t.Fatalf("bearer login body = %+v", body)
	}
	if got := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", headers: map[string]string{"Authorization": "Bearer " + body.Token}}); got.Code != http.StatusOK {
		t.Errorf("GET with the bearer token: %d", got.Code)
	}
	if got := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", cookie: body.Token}); got.Code != http.StatusUnauthorized {
		t.Errorf("a bearer session's token in the cookie: %d, want 401", got.Code)
	}
	// An Authorization header decides alone: a valid cookie next to a
	// bad bearer token does not help.
	if got := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", cookie: c.Value, headers: map[string]string{"Authorization": "Bearer nope"}}); got.Code != http.StatusUnauthorized {
		t.Errorf("bad bearer token with a good cookie: %d, want 401", got.Code)
	}

	// GET /auth/session describes the caller, never the token.
	rec = e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/session", cookie: c.Value})
	who := decode[models.SessionResponse](t, rec)
	if who.User.ID != e.ids[auth.RoleAdmin] || who.Session.Kind != "cookie" || who.Token != "" {
		t.Errorf("session = %+v", who)
	}
	if n := e.auditCount(t, `action = 'auth.login' AND outcome = 'success' AND actor_name = 'test-admin' AND source_ip = '192.0.2.7'`); n != 2 {
		t.Errorf("%d successful logins audited, want 2", n)
	}
}

func TestLoginRefusals(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	post := func(body string, headers map[string]string) *httptest.ResponseRecorder {
		return e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login", body: body, headers: headers})
	}

	wrong := post(loginBody("test-admin", "not the right password", ""), nil)
	unknown := post(loginBody("nobody", testPassword, ""), nil)
	a := wantError(t, wrong, http.StatusUnauthorized, auth.CodeInvalidCredentials)
	b := wantError(t, unknown, http.StatusUnauthorized, auth.CodeInvalidCredentials)
	if a.Message != b.Message {
		t.Errorf("a wrong password and an unknown user answer differently: %q / %q", a.Message, b.Message)
	}
	if sessionCookie(t, wrong) != nil {
		t.Error("a failed login set a cookie")
	}
	if n := e.auditCount(t, `action = 'auth.login' AND outcome = 'failure' AND source_ip = '192.0.2.7'`); n != 2 {
		t.Errorf("%d failed logins audited, want 2", n)
	}

	for name, tt := range map[string]struct {
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		"not json":          {`username=test-admin`, nil, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"form content type": {loginBody("test-admin", testPassword, ""), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType, httpkit.CodeInvalidRequest},
		"text content type": {loginBody("test-admin", testPassword, ""), map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType, httpkit.CodeInvalidRequest},
		"unknown field":     {`{"username":"test-admin","password":"x","admin":true}`, nil, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"two objects":       {loginBody("test-admin", testPassword, "") + `{}`, nil, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"unknown kind":      {loginBody("test-admin", testPassword, "jwt"), nil, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"huge body":         {`{"username":"` + strings.Repeat("a", 100000) + `"}`, nil, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"another site":      {loginBody("test-admin", testPassword, ""), map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden, auth.CodeForbidden},
		"plain http origin": {loginBody("test-admin", testPassword, ""), map[string]string{"Origin": "http://example.com"}, http.StatusForbidden, auth.CodeForbidden},
	} {
		rec := post(tt.body, tt.headers)
		wantError(t, rec, tt.status, tt.code)
		if sessionCookie(t, rec) != nil {
			t.Errorf("%s: a refused login set a cookie", name)
		}
	}
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/login"}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/login: %d", rec.Code)
	}
}

func TestLoginBeforeSetup(t *testing.T) {
	e := freshEnv(t)
	// Remove the administrator the fixture created: a host right after
	// installation.
	if _, err := e.db.Exec(`DELETE FROM users WHERE role_id = 'admin'`); err != nil {
		t.Fatal(err)
	}
	rec := e.serve(e.handler(newBackend(), nil), request{method: http.MethodPost, path: "/api/v1/auth/login",
		body: loginBody("test-viewer", testPassword, "")})
	body := wantError(t, rec, http.StatusServiceUnavailable, auth.CodeSetupRequired)
	if !strings.Contains(body.Message, "skep-api admin create") {
		t.Errorf("message = %q, want it to name the command", body.Message)
	}
}

func TestLoginIsThrottled(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	bad := request{method: http.MethodPost, path: "/api/v1/auth/login", body: loginBody("test-admin", "not the right password", "")}
	for i := range 5 {
		if rec := e.serve(h, bad); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	// The sixth attempt is refused even with the right password.
	rec := e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login", body: loginBody("test-admin", testPassword, "")})
	wantError(t, rec, http.StatusTooManyRequests, auth.CodeRateLimited)
	if ra := rec.Header().Get("Retry-After"); ra != "1" && ra != "2" {
		t.Errorf("Retry-After = %q, want about one second", ra)
	}
	if sessionCookie(t, rec) != nil {
		t.Error("a throttled login set a cookie")
	}
	if n := e.auditCount(t, `action = 'auth.login' AND outcome = 'denied' AND details LIKE '%rate_limited%'`); n != 1 {
		t.Errorf("%d throttled logins audited, want 1", n)
	}
	// Existing sessions are not affected: this is not a lockout.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", role: auth.RoleAdmin}); rec.Code != http.StatusOK {
		t.Errorf("an existing session during the throttle: %d", rec.Code)
	}
}

// TestOriginCheck: a request that is authenticated by cookie and changes
// something must carry a matching Origin. Bearer requests are exempt: a
// browser does not attach them by itself.
func TestOriginCheck(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	rec := e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login", body: loginBody("test-admin", testPassword, "")})
	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("no cookie")
	}
	for name, headers := range map[string]map[string]string{
		"no origin":      nil,
		"another site":   {"Origin": "https://evil.example"},
		"same host http": {"Origin": "http://example.com"},
		"null origin":    {"Origin": "null"},
		"other port":     {"Origin": "https://example.com:8443"},
	} {
		rec := e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/logout", cookie: c.Value, headers: headers})
		wantError(t, rec, http.StatusForbidden, auth.CodeForbidden)
		_ = name
	}
	if n := e.auditCount(t, `action = 'auth.origin_check' AND outcome = 'denied' AND actor_name = 'test-admin'`); n != 5 {
		t.Errorf("%d origin refusals audited, want 5", n)
	}
	// The session survived every refused request, and reads need no Origin.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/session", cookie: c.Value}); rec.Code != http.StatusOK {
		t.Fatalf("GET after refused POSTs: %d", rec.Code)
	}
	rec = e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/logout", cookie: c.Value, headers: map[string]string{"Origin": origin}})
	if rec.Code != http.StatusNoContent {
		t.Errorf("logout with a matching Origin: %d %s", rec.Code, rec.Body)
	}
	// Bearer: no Origin needed.
	rec = e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/logout", role: auth.RoleViewer})
	if rec.Code != http.StatusNoContent {
		t.Errorf("bearer logout without an Origin: %d", rec.Code)
	}
}

func TestLogout(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	rec := e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login", body: loginBody("test-operator", testPassword, "")})
	c := sessionCookie(t, rec)

	rec = e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/logout", cookie: c.Value, headers: map[string]string{"Origin": origin}})
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("logout: %d %q", rec.Code, rec.Body)
	}
	cleared := sessionCookie(t, rec)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 || !cleared.Secure || !cleared.HttpOnly {
		t.Errorf("logout did not clear the cookie: %+v", cleared)
	}
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", cookie: c.Value}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session works after logout: %d", rec.Code)
	}
	// The operator's other session (the fixture's bearer token) is untouched.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", role: auth.RoleOperator}); rec.Code != http.StatusOK {
		t.Errorf("logout ended another session: %d", rec.Code)
	}
	if n := e.auditCount(t, `action = 'auth.logout' AND actor_name = 'test-operator' AND details LIKE '%"session_kind":"cookie"%'`); n != 1 {
		t.Errorf("%d logouts audited, want 1", n)
	}
}

func TestChangeOwnPassword(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	const newPassword = "an entirely different passphrase"
	change := func(current, next string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(models.PasswordChange{CurrentPassword: current, NewPassword: next})
		return e.serve(h, request{method: http.MethodPut, path: "/api/v1/auth/password", role: auth.RoleViewer, body: string(b)})
	}

	wantError(t, change("not the current password", newPassword), http.StatusUnauthorized, auth.CodeInvalidCredentials)
	wantError(t, change(testPassword, "short"), http.StatusBadRequest, httpkit.CodeInvalidRequest)
	// Refusals changed nothing.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/session", role: auth.RoleViewer}); rec.Code != http.StatusOK {
		t.Fatalf("the session ended after a refused change: %d", rec.Code)
	}

	if rec := change(testPassword, newPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	// Every session of the user ended, the one that made the change
	// included.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/session", role: auth.RoleViewer}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session survived the password change: %d", rec.Code)
	}
	login := func(password string) int {
		return e.serve(h, request{method: http.MethodPost, path: "/api/v1/auth/login", body: loginBody("test-viewer", password, "bearer")}).Code
	}
	if login(testPassword) != http.StatusUnauthorized || login(newPassword) != http.StatusOK {
		t.Error("the old password still works, or the new one does not")
	}
	// Other users are untouched.
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/auth/session", role: auth.RoleAdmin}); rec.Code != http.StatusOK {
		t.Errorf("another user's session ended: %d", rec.Code)
	}
	if n := e.auditCount(t, `action = 'user.password.set' AND outcome = 'success' AND actor_name = 'test-viewer'`); n != 1 {
		t.Errorf("%d password changes audited, want 1", n)
	}
	if n := e.auditCount(t, `action = 'session.revoke' AND details LIKE '%password change%'`); n != 1 {
		t.Errorf("%d revocations audited, want 1", n)
	}
	if n := e.auditCount(t, `action = 'user.password.set' AND outcome = 'failure'`); n != 1 {
		t.Errorf("%d failed password changes audited, want 1", n)
	}
}

func TestDisabledUserLosesAccessAtOnce(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", role: auth.RoleOperator}); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	yes := true
	if _, err := e.users.Update(context.Background(), auth.Actor{Name: "local:0"}, e.ids[auth.RoleOperator], auth.UserUpdate{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if rec := e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", role: auth.RoleOperator}); rec.Code != http.StatusUnauthorized {
		t.Errorf("a disabled user's token: %d, want 401", rec.Code)
	}
}

func TestSourceAddressAndTrustedProxies(t *testing.T) {
	e := shared(t)
	b := newBackend()
	call := func(proxies []string, remote string, xff ...string) string {
		var prefixes []netip.Prefix
		for _, p := range proxies {
			prefixes = append(prefixes, netip.MustParsePrefix(p))
		}
		h := NewHandler(Options{Backend: b, Auth: e.authn, Users: e.users, Audit: e.access,
			Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), TrustedProxies: prefixes})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+e.tokens[auth.RoleAdmin])
		for _, v := range xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return b.caller.SourceIP
	}
	for name, tt := range map[string]struct {
		proxies []string
		remote  string
		xff     []string
		want    string
	}{
		"no proxies: the header is ignored":     {nil, "203.0.113.5:1000", []string{"10.9.9.9"}, "203.0.113.5"},
		"untrusted peer: the header is ignored": {[]string{"10.0.0.0/8"}, "203.0.113.5:1000", []string{"10.9.9.9"}, "203.0.113.5"},
		"trusted proxy: the client it names":    {[]string{"10.0.0.0/8"}, "10.0.0.1:1000", []string{"198.51.100.7"}, "198.51.100.7"},
		"the client cannot prepend an address":  {[]string{"10.0.0.0/8"}, "10.0.0.1:1000", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7"},
		"a chain of trusted proxies":            {[]string{"10.0.0.0/8"}, "10.0.0.1:1000", []string{"198.51.100.7, 10.0.0.2"}, "198.51.100.7"},
		"trusted proxy without the header":      {[]string{"10.0.0.0/8"}, "10.0.0.1:1000", nil, "10.0.0.1"},
		"garbage in the header":                 {[]string{"10.0.0.0/8"}, "10.0.0.1:1000", []string{"not-an-address"}, "10.0.0.1"},
		"ipv4-mapped peer":                      {nil, "[::ffff:192.0.2.9]:1000", nil, "192.0.2.9"},
	} {
		if got := call(tt.proxies, tt.remote, tt.xff...); got != tt.want {
			t.Errorf("%s: source address %q, want %q", name, got, tt.want)
		}
	}
}
