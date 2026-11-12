package api

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
)

const testPassword = "correct horse battery staple"

// env is a real identity stack on a scratch api.db: the handlers are
// tested against the code that runs in skep-api, not against fakes.
type env struct {
	db     *sql.DB
	users  *auth.Users
	authn  *auth.Authenticator
	access *apidb.AccessLog
	// tokens are bearer tokens of one user per built-in role.
	tokens map[string]string
	ids    map[string]string
}

var (
	sharedOnce sync.Once
	sharedEnv  *env
	sharedErr  error
)

// newEnv builds an identity stack with an admin, an operator, and a
// viewer, each with a bearer session.
func newEnv(dir string) (*env, error) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(dir, "api", "api.db"))
	if err != nil {
		return nil, err
	}
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		return nil, err
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hasher, err := auth.NewHasher(auth.MinParams, 2, 5*time.Second)
	if err != nil {
		return nil, err
	}
	e := &env{db: db, access: apidb.NewAccessLog(db), tokens: map[string]string{}, ids: map[string]string{}}
	store := apidb.NewUserStore(db)
	e.users = auth.NewUsers(store, hasher, e.access, quiet)
	throttle := auth.DefaultThrottleConfig()
	// The tests log in often from one address; the limits themselves are
	// tested in internal/auth and in TestLoginIsThrottled.
	throttle.PerAddress = 1000
	e.authn, err = auth.NewAuthenticator(auth.AuthenticatorOptions{
		Users: store, Sessions: apidb.NewSessionStore(db), Hasher: hasher, Audit: e.access,
		Throttle: throttle,
		Session:  auth.SessionConfig{IdleTimeout: auth.DefaultIdleTimeout, AbsoluteTimeout: auth.DefaultAbsoluteTimeout},
		Logger:   quiet,
	})
	if err != nil {
		return nil, err
	}
	for _, role := range []string{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
		u, err := e.users.Create(ctx, auth.Actor{Name: "local:0"}, auth.NewUser{
			Username: "test-" + role, Password: testPassword, RoleID: role,
		})
		if err != nil {
			return nil, err
		}
		e.ids[role] = u.ID
		res, err := e.authn.Login(ctx, auth.LoginRequest{
			Username: u.Username, Password: testPassword, Kind: auth.BearerSession, SourceIP: "192.0.2.200",
		})
		if err != nil {
			return nil, err
		}
		e.tokens[role] = res.Token
	}
	return e, nil
}

// shared returns the identity stack the read-only tests share. Tests that
// change users or sessions use freshEnv instead.
func shared(t *testing.T) *env {
	t.Helper()
	sharedOnce.Do(func() {
		dir, err := os.MkdirTemp("", "skep-api-test")
		if err != nil {
			sharedErr = err
			return
		}
		sharedEnv, sharedErr = newEnv(dir)
	})
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	return sharedEnv
}

func freshEnv(t *testing.T) *env {
	t.Helper()
	e, err := newEnv(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.db.Close() })
	return e
}

// handler returns the API on top of this identity stack.
func (e *env) handler(b Backend, logBuf *bytes.Buffer) http.Handler {
	if logBuf == nil {
		logBuf = &bytes.Buffer{}
	}
	return NewHandler(Options{
		Backend: b, Auth: e.authn, Users: e.users, Audit: e.access, AuditLog: e.access,
		Logger: slog.New(slog.NewJSONHandler(logBuf, nil)),
	})
}

// request describes one call in a test.
type request struct {
	method, path string
	// role selects whose bearer token is sent; "" sends none.
	role    string
	body    string
	headers map[string]string
	cookie  string
}

func (e *env) serve(h http.Handler, rq request) *httptest.ResponseRecorder {
	var body io.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	}
	req := httptest.NewRequest(rq.method, rq.path, body)
	req.RemoteAddr = "192.0.2.7:51234"
	if rq.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if rq.role != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[rq.role])
	}
	if rq.cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: rq.cookie})
	}
	for k, v := range rq.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// auditCount counts access audit entries that match.
func (e *env) auditCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
