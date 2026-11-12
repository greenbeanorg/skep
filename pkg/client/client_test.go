package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// testServer is a TLS 1.3 server with a handler and a request counter.
type testServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newTestServer(t *testing.T, h http.HandlerFunc, maxVersion uint16) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requests.Add(1)
		h(w, r)
	}))
	ts.TLS = &tls.Config{MaxVersion: maxVersion}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) pin() string { return PublicKeyPin(ts.Certificate()) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"hearst":                   "https://hearst:8443",
		"Hearst.Lab:9443":          "https://hearst.lab:9443",
		"https://10.79.20.170":     "https://10.79.20.170:8443",
		"https://10.79.20.170/":    "https://10.79.20.170:8443",
		" https://[::1]:8443 ":     "https://[::1]:8443",
		"[fe80::1]":                "https://[fe80::1]:8443",
		"https://localhost:8443/":  "https://localhost:8443",
		"HTTPS://LOCALHOST:1/":     "https://localhost:1",
		"https://hearst:8443":      "https://hearst:8443",
		"https://hearst.lab.:8443": "https://hearst.lab.:8443",
	} {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "http://hearst", "ftp://hearst", "https://user:pw@hearst", "https://hearst/api/v1",
		"https://hearst?x=1", "https://hearst#x", "https://:8443",
	} {
		if got, err := NormalizeServer(in); err == nil {
			t.Errorf("NormalizeServer(%q) = %q, want an error", in, got)
		}
	}
}

func TestValidPin(t *testing.T) {
	good := "sha256:" + strings.Repeat("ab", 32)
	if !ValidPin(good) {
		t.Error("a valid pin was refused")
	}
	for _, bad := range []string{"", strings.Repeat("ab", 32), "sha256:" + strings.Repeat("AB", 32),
		"sha256:" + strings.Repeat("ab", 31), "sha1:" + strings.Repeat("ab", 32), good + "\n"} {
		if ValidPin(bad) {
			t.Errorf("ValidPin(%q) = true", bad)
		}
	}
}

func TestNewRequiresAPin(t *testing.T) {
	for _, pin := range []string{"", "nope", "sha256:"} {
		if _, err := New(Options{Server: "localhost", Pin: pin}); err == nil {
			t.Errorf("New with pin %q succeeded", pin)
		}
	}
}

func TestFetchCertificateSendsNothing(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {}, 0)
	cert, err := FetchCertificate(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if PublicKeyPin(cert) != ts.pin() {
		t.Errorf("fetched pin %s, server pin %s", PublicKeyPin(cert), ts.pin())
	}
	if n := ts.requests.Load(); n != 0 {
		t.Errorf("FetchCertificate sent %d requests", n)
	}
	if _, err := FetchCertificate(context.Background(), "http://"+ts.Listener.Addr().String()); err == nil {
		t.Error("a plain HTTP address was accepted")
	}
}

func TestPinnedConnection(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system" || r.Header.Get("Authorization") != "Bearer tok" {
			writeJSON(w, http.StatusTeapot, nil)
			return
		}
		writeJSON(w, http.StatusOK, models.System{NodeID: "node-1", Hostname: "hearst"})
	}, 0)

	c, err := New(Options{Server: ts.URL, Pin: ts.pin(), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sys, err := c.System(context.Background())
	if err != nil || sys.NodeID != "node-1" {
		t.Fatalf("System = %+v, %v", sys, err)
	}

	// Another key: refused in the handshake, before any request.
	before := ts.requests.Load()
	other := "sha256:" + strings.Repeat("00", 32)
	c2, err := New(Options{Server: ts.URL, Pin: other, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_, err = c2.System(context.Background())
	var km *KeyMismatchError
	if !errors.As(err, &km) || km.Pinned != other || km.Presented != ts.pin() {
		t.Fatalf("error = %v, want a key mismatch", err)
	}
	if ts.requests.Load() != before {
		t.Error("a request reached a server with the wrong key")
	}
}

func TestRefusesTLS12(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {}, tls.VersionTLS12)
	if _, err := FetchCertificate(context.Background(), ts.URL); err == nil {
		t.Error("FetchCertificate accepted TLS 1.2")
	}
	c, _ := New(Options{Server: ts.URL, Pin: ts.pin()})
	defer c.Close()
	if _, err := c.System(context.Background()); err == nil {
		t.Error("the client accepted TLS 1.2")
	}
	if ts.requests.Load() != 0 {
		t.Error("a request was sent over TLS 1.2")
	}
}

func TestErrors(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/system":
			writeJSON(w, http.StatusUnauthorized, models.ErrorResponse{Error: models.ErrorBody{Code: "unauthenticated", Message: "no session", RequestID: "req-1"}})
		case "/api/v1/audit":
			w.Header().Set("Retry-After", "7")
			writeJSON(w, http.StatusTooManyRequests, models.ErrorResponse{Error: models.ErrorBody{Code: "rate_limited", Message: "slow down"}})
		case "/api/v1/tasks":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		default:
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>proxy error</html>"))
		}
	}, 0)
	c, _ := New(Options{Server: ts.URL, Pin: ts.pin(), Token: "tok"})
	defer c.Close()
	ctx := context.Background()

	_, err := c.System(ctx)
	if !IsUnauthenticated(err) || !strings.Contains(err.Error(), "req-1") {
		t.Errorf("401: %v", err)
	}
	_, err = c.Audit(ctx, AuditOptions{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "rate_limited" || ae.RetryAfter != 7*time.Second || IsUnauthenticated(err) {
		t.Errorf("429: %#v", err)
	}
	_, err = c.Tasks(ctx, models.TaskListOptions{})
	if !errors.As(err, &ae) || ae.Status != http.StatusFound {
		t.Errorf("redirect: %v, want it returned, not followed", err)
	}
	_, err = c.Task(ctx, "x")
	if !errors.As(err, &ae) || ae.Status != http.StatusBadGateway || ae.Code != "http_502" {
		t.Errorf("non-JSON error: %#v", err)
	}
}

func TestRequests(t *testing.T) {
	var seen []string
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/v1/auth/login":
			var req models.LoginRequest
			if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&req) != nil ||
				req.Session != "bearer" || req.Username != "andy" || req.Password != "pw" || r.Header.Get("Authorization") != "" {
				writeJSON(w, http.StatusBadRequest, nil)
				return
			}
			writeJSON(w, http.StatusOK, models.SessionResponse{User: models.User{Username: "andy"}, Token: "tok"})
		case "/api/v1/auth/logout":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/tasks":
			writeJSON(w, http.StatusOK, models.TaskList{Items: []models.Task{{ID: "t1"}}, NextCursor: "c2"})
		case "/api/v1/audit":
			writeJSON(w, http.StatusOK, models.AuditList{Items: []models.AuditEntry{{ID: 9}}})
		default:
			writeJSON(w, http.StatusOK, map[string]any{})
		}
	}, 0)
	ctx := context.Background()
	anon, _ := New(Options{Server: ts.URL, Pin: ts.pin(), UserAgent: "skepctl/test"})
	defer anon.Close()
	res, err := anon.Login(ctx, "andy", "pw")
	if err != nil || res.Token != "tok" {
		t.Fatalf("Login = %+v, %v", res, err)
	}
	c, _ := New(Options{Server: ts.URL, Pin: ts.pin(), Token: res.Token})
	defer c.Close()
	if err := c.Logout(ctx); err != nil {
		t.Error(err)
	}
	list, err := c.Tasks(ctx, models.TaskListOptions{Status: models.TaskRunning, Limit: 5, Cursor: "c1"})
	if err != nil || list.NextCursor != "c2" {
		t.Errorf("Tasks = %+v, %v", list, err)
	}
	if _, err := c.Audit(ctx, AuditOptions{Limit: 3, Cursor: "10"}); err != nil {
		t.Error(err)
	}
	if _, err := c.Hardware(ctx, "node/1"); err != nil {
		t.Error(err)
	}
	want := []string{
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/logout",
		"GET /api/v1/tasks?cursor=c1&limit=5&status=running",
		"GET /api/v1/audit?cursor=10&limit=3",
		"GET /api/v1/nodes/node%2F1/hardware",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}

	// A login whose response has no token is an error.
	ts2 := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, models.SessionResponse{})
	}, 0)
	c3, _ := New(Options{Server: ts2.URL, Pin: ts2.pin()})
	defer c3.Close()
	if _, err := c3.Login(ctx, "andy", "pw"); err == nil {
		t.Error("a login without a token succeeded")
	}
}
