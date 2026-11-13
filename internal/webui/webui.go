// Package webui serves the built web interface from skep-api's TLS
// listener, on the same origin as the API (ADR 0008, decision 4).
//
// The files are embedded in the binary: a build with the tag skep_webui
// carries web/dist as built by "make build" or "make build-freebsd"; a
// build without it carries only a placeholder page that says so, so that
// "go build ./..." and "go test ./..." need no Node.js. Embedded reports
// which one this binary is.
//
// Routing, for every path outside /api (internal/api dispatches those):
//
//   - /assets/... are the hashed build files, cached for a year.
//   - Other files that exist in the build (theme.js, favicons) are served
//     as they are, not cached.
//   - Any other GET or HEAD answers index.html, not cached, so the
//     interface can route client-side paths such as /nodes/node-1 and a
//     new release is picked up at the next load. A path whose last
//     segment looks like a file name (has an extension) is 404 instead,
//     so a missing file is reported as missing and not as a page.
//   - Other methods get 405.
//
// The files are public and contain no data; the login page must load
// without a session. Nothing here touches the access audit log.
package webui

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/httpkit"
)

// Security headers of the interface's responses. The API's own headers
// (set by httpkit.WithSecurityHeaders) are stricter and stay as they are;
// these replace them for the interface's files only. No inline script or
// style is allowed.
const (
	csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	// assetCacheControl is for the hashed files under /assets: their name
	// changes with their content.
	assetCacheControl = "public, max-age=31536000, immutable"
	noStore           = "no-store"
)

// Handler returns the handler of the interface for the embedded files. It
// expects to be mounted at the root, with the API dispatched before it.
func Handler() http.Handler { return handlerFS(files) }

// Embedded reports whether this binary carries the built interface, as
// opposed to the placeholder page.
func Embedded() bool { return embedded }

type handler struct {
	fs fs.FS
	// index is index.html, read once: it is the answer to most requests.
	index []byte
}

// handlerFS is Handler for any file system, for tests.
func handlerFS(fsys fs.FS) http.Handler {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		// The embed of either variant always contains index.html; a file
		// system without one is a programming error.
		panic("webui: the embedded files have no index.html: " + err.Error())
	}
	return &handler{fs: fsys, index: index}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpkit.WriteError(w, r, http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed,
			"method "+r.Method+" is not allowed on this path")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", csp)
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
	hdr.Set("Cache-Control", noStore)

	name, ok := cleanPath(r.URL.Path)
	if !ok {
		httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound, "no such file")
		return
	}
	if name != "" && name != "index.html" {
		if data, err := fs.ReadFile(h.fs, name); err == nil {
			if strings.HasPrefix(name, "assets/") {
				hdr.Set("Cache-Control", assetCacheControl)
			}
			serve(w, r, name, data)
			return
		}
		if path.Ext(name) != "" {
			// A file that does not exist, not a page the interface
			// could route.
			httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound, "no such file: /"+name)
			return
		}
	}
	serve(w, r, "index.html", h.index)
}

// cleanPath turns a request path into a name inside the file system, or
// "" for the root. path.Clean resolves anything that climbs out of the
// tree. A name with a hidden segment (starting with ".") is refused: the
// build has no such files ("all:dist" would embed them if it had), and
// they are neither served nor answered with a page.
func cleanPath(p string) (name string, ok bool) {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	return p, true
}

// serve writes one file with its content type. The modification time is
// zero: the hashed files never change under their name, and index.html is
// never cached, so Last-Modified would add nothing.
func serve(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	w.Header().Set("Content-Type", contentType(name))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// contentType is the media type of a built file. The common ones are fixed
// here so that they do not depend on the host's mime.types; the rest
// falls back to the Go table.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
