package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

func tableOf(e *env) []route {
	h := &handler{auth: e.authn, users: e.users, audit: e.access, history: e.access}
	return h.routes()
}

// TestEveryRouteDeclaresWhatItRequires fails when a route is added without
// a permission. Enforcement denies such a route for everyone; this test
// makes sure nobody ships one.
func TestEveryRouteDeclaresWhatItRequires(t *testing.T) {
	routes := tableOf(shared(t))
	if len(routes) == 0 {
		t.Fatal("no routes")
	}
	var public []string
	seen := map[string]bool{}
	for _, rt := range routes {
		name := rt.method + " " + rt.path
		if seen[name] {
			t.Errorf("%s is declared twice", name)
		}
		seen[name] = true
		switch {
		case rt.requires == "":
			t.Errorf("%s has no permission", name)
		case rt.requires == requirePublic:
			public = append(public, name)
		case rt.requires == requireSession:
			// Only the caller's own session and password.
			if !strings.HasPrefix(rt.path, "/auth/") {
				t.Errorf("%s needs only a session; routes outside /auth/ need a permission", name)
			}
		case !auth.KnownPermission(rt.requires):
			t.Errorf("%s requires %q, which is not a declared permission", name, rt.requires)
		}
		if rt.handler == nil {
			t.Errorf("%s has no handler", name)
		}
	}
	if strings.Join(public, ", ") != "GET /health, POST /auth/login" {
		t.Errorf("public routes = %v; only the health status and the login may be public", public)
	}
	// Every declared permission is enforced somewhere.
	used := map[string]bool{}
	for _, rt := range routes {
		used[rt.requires] = true
	}
	for _, p := range auth.Permissions {
		if !used[p] {
			t.Errorf("permission %s is declared but no route requires it", p)
		}
	}
}

// concrete turns a route into a request that reaches its handler.
func concrete(e *env, rt route) request {
	path := strings.NewReplacer("/nodes/{id}", "/nodes/node-1", "/tasks/{id}", "/tasks/task-1",
		"/users/{id}", "/users/"+e.ids[auth.RoleViewer]).Replace(rt.path)
	rq := request{method: rt.method, path: Prefix + path}
	switch rt.method + " " + rt.path {
	case "POST /users":
		rq.body = `{"username":"x"}` // refused as invalid, after the permission check
	case "PUT /users/{id}":
		rq.body = `{}`
	case "PUT /auth/password":
		rq.body = `{"current_password":"x","new_password":"y"}`
	case "DELETE /users/{id}":
		rq.path = Prefix + "/users/no-such-user"
	}
	return rq
}

// TestPermissionMatrix calls every route as every built-in role and checks
// that the answer is 403 exactly when the role lacks the route's
// permission, and that every denial is audited.
func TestPermissionMatrix(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	roles, err := e.users.Roles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]auth.Role{}
	for _, r := range roles {
		byID[r.ID] = r
	}

	denials := 0
	for _, rt := range tableOf(e) {
		if rt.requires == requirePublic || rt.requires == requireSession {
			continue
		}
		for _, role := range []string{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
			rq := concrete(e, rt)
			rq.role = role
			rec := e.serve(h, rq)
			allowed := byID[role].Allows(rt.requires)
			name := fmt.Sprintf("%s %s as %s (needs %s)", rt.method, rt.path, role, rt.requires)
			if allowed && (rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized) {
				t.Errorf("%s: status %d, want it allowed", name, rec.Code)
			}
			if !allowed {
				denials++
				body := wantError(t, rec, http.StatusForbidden, auth.CodeForbidden)
				if body.Details["permission"] != rt.requires {
					t.Errorf("%s: details = %v", name, body.Details)
				}
				n := e.auditCount(t, `action = 'authz.deny' AND outcome = 'denied' AND actor_id = ? AND details LIKE ? AND details LIKE ?`,
					e.ids[role], `%"permission":"`+rt.requires+`"%`, `%"route":"`+Prefix+rt.path+`"%`)
				if n < 1 {
					t.Errorf("%s: the denial was not audited", name)
				}
			}
		}
	}
	if denials == 0 {
		t.Fatal("the matrix denied nothing; the test is not testing")
	}
	if n := e.auditCount(t, `action = 'authz.deny'`); n != denials {
		t.Errorf("%d denials audited, %d expected", n, denials)
	}

	// What ADR 0006 says about the three roles, spelled out.
	status := func(role, method, path string) int {
		return e.serve(h, request{method: method, path: Prefix + path, role: role}).Code
	}
	for _, tt := range []struct {
		role, method, path string
		want               int
	}{
		{auth.RoleViewer, http.MethodGet, "/system", http.StatusOK},
		{auth.RoleViewer, http.MethodGet, "/tasks", http.StatusOK},
		{auth.RoleViewer, http.MethodGet, "/roles", http.StatusOK},
		{auth.RoleViewer, http.MethodGet, "/users", http.StatusForbidden},
		{auth.RoleViewer, http.MethodGet, "/audit", http.StatusForbidden},
		{auth.RoleOperator, http.MethodGet, "/users", http.StatusOK},
		{auth.RoleOperator, http.MethodGet, "/audit", http.StatusOK},
		{auth.RoleOperator, http.MethodDelete, "/users/" + e.ids[auth.RoleViewer], http.StatusForbidden},
		{auth.RoleAdmin, http.MethodGet, "/audit", http.StatusOK},
	} {
		if got := status(tt.role, tt.method, tt.path); got != tt.want {
			t.Errorf("%s %s as %s: %d, want %d", tt.method, tt.path, tt.role, got, tt.want)
		}
	}
	// The viewer still exists: the operator's DELETE was refused before
	// anything happened.
	if status(auth.RoleViewer, http.MethodGet, "/system") != http.StatusOK {
		t.Error("a denied DELETE had an effect")
	}
}

// TestDenyByDefault: a route whose requirement is missing or unknown is
// refused for everyone, the administrator included.
func TestDenyByDefault(t *testing.T) {
	e := shared(t)
	h := &handler{auth: e.authn, users: e.users, audit: e.access, log: quietLog()}
	for _, requires := range []string{"", "vm:delete", "*", "Public", "admin"} {
		called := false
		rt := route{method: http.MethodGet, path: "/future", requires: requires,
			handler: func(http.ResponseWriter, *http.Request) { called = true }}
		req := httptest.NewRequest(http.MethodGet, Prefix+"/future", nil)
		req.RemoteAddr = "192.0.2.7:1000"
		req.Header.Set("Authorization", "Bearer "+e.tokens[auth.RoleAdmin])
		rec := httptest.NewRecorder()
		h.serve(rt, rec, req)
		if called || rec.Code != http.StatusForbidden {
			t.Errorf("requirement %q: handler called=%v, status %d; want a refusal", requires, called, rec.Code)
		}
		// And without a session it is a 401, not an open door.
		req = httptest.NewRequest(http.MethodGet, Prefix+"/future", nil)
		req.RemoteAddr = "192.0.2.7:1000"
		rec = httptest.NewRecorder()
		h.serve(rt, rec, req)
		if called || rec.Code != http.StatusUnauthorized {
			t.Errorf("requirement %q without a session: called=%v, status %d", requires, called, rec.Code)
		}
	}
}

func TestUserManagement(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)
	as := func(role, method, path, body string) *httptest.ResponseRecorder {
		return e.serve(h, request{method: method, path: Prefix + path, role: role, body: body})
	}
	admin := auth.RoleAdmin

	// Create.
	rec := as(admin, http.MethodPost, "/users", `{"username":"dana","display_name":"Dana","password":"`+testPassword+`","role":"operator"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	dana := decode[models.User](t, rec)
	if dana.Username != "dana" || dana.Role != "operator" || dana.Disabled || dana.ID == "" ||
		rec.Header().Get("Location") != Prefix+"/users/"+dana.ID {
		t.Errorf("created user = %+v, Location %q", dana, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), testPassword) {
		t.Errorf("the response contains password material: %s", rec.Body)
	}
	// The default role is the least privileged one.
	rec = as(admin, http.MethodPost, "/users", `{"username":"erin","password":"`+testPassword+`"}`)
	if erin := decode[models.User](t, rec); erin.Role != "viewer" {
		t.Errorf("default role = %q", erin.Role)
	}

	for name, tt := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"duplicate":      {`{"username":"dana","password":"` + testPassword + `"}`, http.StatusConflict, auth.CodeUsernameTaken},
		"bad username":   {`{"username":"Dana Scully","password":"` + testPassword + `"}`, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"short password": {`{"username":"fox","password":"short"}`, http.StatusBadRequest, httpkit.CodeInvalidRequest},
		"unknown role":   {`{"username":"fox","password":"` + testPassword + `","role":"root"}`, http.StatusBadRequest, auth.CodeUnknownRole},
		"unknown field":  {`{"username":"fox","password":"` + testPassword + `","is_admin":true}`, http.StatusBadRequest, httpkit.CodeInvalidRequest},
	} {
		wantError(t, as(admin, http.MethodPost, "/users", tt.body), tt.status, tt.code)
		_ = name
	}

	// Read.
	list := decode[models.UserList](t, as(admin, http.MethodGet, "/users", ""))
	if len(list.Items) != 5 || list.Items[0].Username != "dana" {
		t.Errorf("list = %+v", list.Items)
	}
	if got := decode[models.User](t, as(auth.RoleOperator, http.MethodGet, "/users/"+dana.ID, "")); got.ID != dana.ID {
		t.Errorf("get = %+v", got)
	}
	wantError(t, as(admin, http.MethodGet, "/users/no-such-user", ""), http.StatusNotFound, httpkit.CodeNotFound)

	// Dana logs in; a role change ends her session and applies at once.
	login := e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("dana", testPassword, "bearer")})
	token := decode[models.SessionResponse](t, login).Token
	asDana := func(method, path string) int {
		return e.serve(h, request{method: method, path: Prefix + path, headers: map[string]string{"Authorization": "Bearer " + token}}).Code
	}
	if asDana(http.MethodGet, "/users") != http.StatusOK {
		t.Fatal("an operator cannot list users")
	}
	rec = as(admin, http.MethodPut, "/users/"+dana.ID, `{"role":"viewer","display_name":"Dana S."}`)
	if got := decode[models.User](t, rec); rec.Code != http.StatusOK || got.Role != "viewer" || got.DisplayName != "Dana S." {
		t.Fatalf("update: %d %+v", rec.Code, got)
	}
	if asDana(http.MethodGet, "/system") != http.StatusUnauthorized {
		t.Error("the session survived a role change")
	}
	login = e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("dana", testPassword, "bearer")})
	token = decode[models.SessionResponse](t, login).Token
	if asDana(http.MethodGet, "/users") != http.StatusForbidden || asDana(http.MethodGet, "/system") != http.StatusOK {
		t.Error("the new role is not what the next session gets")
	}

	// An administrator sets a password: the user's sessions end.
	const newPassword = "an entirely different passphrase"
	if rec := as(admin, http.MethodPut, "/users/"+dana.ID, `{"password":"`+newPassword+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("set password: %d %s", rec.Code, rec.Body)
	}
	if asDana(http.MethodGet, "/system") != http.StatusUnauthorized {
		t.Error("the session survived an administrator's password reset")
	}
	if code := e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("dana", newPassword, "bearer")}).Code; code != http.StatusOK {
		t.Errorf("login with the new password: %d", code)
	}
	wantError(t, as(admin, http.MethodPut, "/users/"+dana.ID, `{"password":"short","disabled":true}`), http.StatusBadRequest, httpkit.CodeInvalidRequest)
	if got := decode[models.User](t, as(admin, http.MethodGet, "/users/"+dana.ID, "")); got.Disabled {
		t.Error("a refused update was applied in part")
	}

	// Disable: no more logins, same answer as a wrong password.
	as(admin, http.MethodPut, "/users/"+dana.ID, `{"disabled":true}`)
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("dana", newPassword, "bearer")})
	wantError(t, rec, http.StatusUnauthorized, auth.CodeInvalidCredentials)

	// Delete.
	if rec := as(admin, http.MethodDelete, "/users/"+dana.ID, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rec.Code)
	}
	wantError(t, as(admin, http.MethodGet, "/users/"+dana.ID, ""), http.StatusNotFound, httpkit.CodeNotFound)
	wantError(t, as(admin, http.MethodDelete, "/users/"+dana.ID, ""), http.StatusNotFound, httpkit.CodeNotFound)

	// The last administrator is protected, also from itself.
	self := e.ids[admin]
	wantError(t, as(admin, http.MethodDelete, "/users/"+self, ""), http.StatusConflict, auth.CodeLastAdmin)
	wantError(t, as(admin, http.MethodPut, "/users/"+self, `{"disabled":true}`), http.StatusConflict, auth.CodeLastAdmin)
	wantError(t, as(admin, http.MethodPut, "/users/"+self, `{"role":"viewer"}`), http.StatusConflict, auth.CodeLastAdmin)
	if as(admin, http.MethodGet, "/system", "").Code != http.StatusOK {
		t.Error("a refused change ended the administrator's session")
	}

	// All of it is in the audit log, with the administrator as the actor.
	for action, want := range map[string]int{"user.create": 2, "user.update": 2, "user.password.set": 1, "user.delete": 1} {
		if n := e.auditCount(t, `action = ? AND outcome = 'success' AND actor_id = ? AND source_ip = '192.0.2.7'`, action, self); n != want {
			t.Errorf("%s: %d audit entries, want %d", action, n, want)
		}
	}
	if n := e.auditCount(t, `outcome = 'denied' AND details LIKE '%last_admin%'`); n != 3 {
		t.Errorf("%d last-administrator refusals audited, want 3", n)
	}
}

func TestRolesAndAuditEndpoints(t *testing.T) {
	e := freshEnv(t)
	h := e.handler(newBackend(), nil)

	roles := decode[models.RoleList](t, e.serve(h, request{method: http.MethodGet, path: Prefix + "/roles", role: auth.RoleViewer}))
	if len(roles.Items) != 3 {
		t.Fatalf("roles = %+v", roles.Items)
	}
	for _, r := range roles.Items {
		if !r.BuiltIn || len(r.Permissions) == 0 || r.Description == "" {
			t.Errorf("role = %+v", r)
		}
	}

	// Produce some entries, then page through all of them.
	for range 3 {
		e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("nobody", testPassword, "")})
	}
	var all []models.AuditEntry
	cursor := ""
	for page := 0; ; page++ {
		path := Prefix + "/audit?limit=4"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := e.serve(h, request{method: http.MethodGet, path: path, role: auth.RoleOperator})
		list := decode[models.AuditList](t, rec)
		if len(list.Items) == 0 || len(list.Items) > 4 {
			t.Fatalf("page %d has %d entries", page, len(list.Items))
		}
		all = append(all, list.Items...)
		if cursor = list.NextCursor; cursor == "" {
			break
		}
		if page > 50 {
			t.Fatal("paging does not end")
		}
	}
	if total := e.auditCount(t, `1 = 1`); len(all) != total {
		t.Errorf("paged through %d entries, the log has %d", len(all), total)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("entries are not newest first: %d after %d", all[i].ID, all[i-1].ID)
		}
	}
	if all[0].Action != "auth.login" || all[0].Outcome != "failure" || all[0].ActorName != "nobody" || all[0].SourceIP != "192.0.2.7" {
		t.Errorf("newest entry = %+v", all[0])
	}
	raw, _ := json.Marshal(all)
	for _, secret := range []string{testPassword, "argon2", e.tokens[auth.RoleAdmin], auth.HashToken(e.tokens[auth.RoleAdmin])} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the audit log exposes %q", secret)
		}
	}
	for _, bad := range []string{"?limit=0", "?limit=x", "?limit=100000", "?cursor=abc", "?cursor=-1"} {
		wantError(t, e.serve(h, request{method: http.MethodGet, path: Prefix + "/audit" + bad, role: auth.RoleAdmin}),
			http.StatusBadRequest, httpkit.CodeInvalidRequest)
	}
}
