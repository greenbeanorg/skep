package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><html><body>app</body></html>")},
		"theme.js":             {Data: []byte("document.documentElement.dataset.theme='light'")},
		"assets/index-abc.js":  {Data: []byte("console.log('app')")},
		"assets/index-abc.css": {Data: []byte("body{}")},
		".hidden":              {Data: []byte("secret")},
	}
}

func get(t *testing.T, h http.Handler, method, path string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestHandlerRoutes(t *testing.T) {
	h := handlerFS(testFS())
	cases := map[string]struct {
		method, path string
		status       int
		contentType  string
		cache        string
		body         string
	}{
		"root":          {"GET", "/", 200, "text/html; charset=utf-8", "no-store", "app"},
		"index by name": {"GET", "/index.html", 200, "text/html; charset=utf-8", "no-store", "app"},
		"app path":      {"GET", "/nodes/node-1", 200, "text/html; charset=utf-8", "no-store", "app"},
		"login":         {"GET", "/login?next=%2Ftasks", 200, "text/html; charset=utf-8", "no-store", "app"},
		"deep app path": {"GET", "/users/x/y/z", 200, "text/html; charset=utf-8", "no-store", "app"},
		"root file":     {"GET", "/theme.js", 200, "text/javascript; charset=utf-8", "no-store", "dataset"},
		"asset js":      {"GET", "/assets/index-abc.js", 200, "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", "console"},
		"asset css":     {"GET", "/assets/index-abc.css", 200, "text/css; charset=utf-8", "public, max-age=31536000, immutable", "body"},
		"missing asset": {"GET", "/assets/old-xyz.js", 404, "application/json; charset=utf-8", "no-store", "not_found"},
		"missing file":  {"GET", "/favicon.ico", 404, "application/json; charset=utf-8", "no-store", "not_found"},
		"hidden file":   {"GET", "/.hidden", 404, "application/json; charset=utf-8", "no-store", "not_found"},
		"climbing":      {"GET", "/assets/../.hidden", 404, "application/json; charset=utf-8", "no-store", "not_found"},
		"head root":     {"HEAD", "/", 200, "text/html; charset=utf-8", "no-store", ""},
		"head asset":    {"HEAD", "/assets/index-abc.js", 200, "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", ""},
		"post":          {"POST", "/", 405, "application/json; charset=utf-8", "", "method_not_allowed"},
		"put app path":  {"PUT", "/nodes/node-1", 405, "application/json; charset=utf-8", "", "method_not_allowed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, body := get(t, h, tc.method, tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d; body %q", resp.StatusCode, tc.status, body)
			}
			if ct := resp.Header.Get("Content-Type"); ct != tc.contentType {
				t.Errorf("Content-Type %q, want %q", ct, tc.contentType)
			}
			if tc.cache != "" {
				if cc := resp.Header.Get("Cache-Control"); cc != tc.cache {
					t.Errorf("Cache-Control %q, want %q", cc, tc.cache)
				}
			}
			if !strings.Contains(body, tc.body) {
				t.Errorf("body %q does not contain %q", body, tc.body)
			}
			if tc.status == 405 && resp.Header.Get("Allow") != "GET, HEAD" {
				t.Errorf("Allow %q, want GET, HEAD", resp.Header.Get("Allow"))
			}
		})
	}
}

func TestHandlerSecurityHeaders(t *testing.T) {
	h := handlerFS(testFS())
	for _, p := range []string{"/", "/tasks/abc", "/assets/index-abc.js", "/theme.js", "/favicon.ico"} {
		resp, _ := get(t, h, "GET", p)
		want := map[string]string{
			"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
				"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
			"Referrer-Policy":            "no-referrer",
			"X-Content-Type-Options":     "nosniff",
			"Cross-Origin-Opener-Policy": "same-origin",
		}
		for k, v := range want {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", p, k, got, v)
			}
		}
	}
}

// TestPlaceholder checks the variant this package was built with: without
// the skep_webui tag the embedded page is the placeholder, which says so and
// carries the marker CI greps release binaries for.
func TestPlaceholder(t *testing.T) {
	resp, body := get(t, Handler(), "GET", "/")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if Embedded() {
		if strings.Contains(body, "skep-webui-placeholder") {
			t.Error("a build with the interface serves the placeholder")
		}
		return
	}
	if !strings.Contains(body, "skep-webui-placeholder") || !strings.Contains(body, "was not built into this binary") {
		t.Errorf("the placeholder does not say what it is: %q", body)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "style=") {
		t.Error("the placeholder must satisfy the CSP: no inline script or style")
	}
}

func TestContentType(t *testing.T) {
	for name, want := range map[string]string{
		"a.html": "text/html; charset=utf-8", "a.js": "text/javascript; charset=utf-8", "a.mjs": "text/javascript; charset=utf-8",
		"a.css": "text/css; charset=utf-8", "a.svg": "image/svg+xml", "a.woff2": "font/woff2", "a.png": "image/png",
		"a.ico": "image/x-icon", "a.json": "application/json; charset=utf-8", "a.bin": "application/octet-stream",
	} {
		if got := contentType(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}
