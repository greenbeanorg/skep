package httpkit

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

// RequestIDHeader carries the request ID in both directions.
const RequestIDHeader = "X-Request-ID"

type contextKey int

const requestIDKey contextKey = iota

// clientRequestIDRE limits which client-supplied request IDs are accepted,
// so arbitrary input never reaches the logs.
var clientRequestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidRequestID reports whether id is of the form a client-supplied
// request ID must have: 1 to 64 characters from A-Z a-z 0-9 . _ -.
func ValidRequestID(id string) bool { return clientRequestIDRE.MatchString(id) }

// RequestID returns the ID of the request ctx belongs to, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}

// WithRequestIDValue returns ctx carrying id as the request ID: a
// forwarded request keeps the origin's (step 8e-1, D27).
func WithRequestIDValue(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithRequestID assigns every request an ID, reusing a well-formed one from
// the client, and echoes it in the response.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !clientRequestIDRE.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// WithSecurityHeaders sets headers appropriate for a JSON-only API.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code and size of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Hijack hands the connection to a WebSocket; the access log records 101.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	if err == nil && s.status == 0 {
		s.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// WithAccessLog logs one structured line per request.
func WithAccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
			slog.String("request_id", RequestID(r.Context())),
		)
	})
}

// WithRecovery turns a handler panic into a 500 error envelope.
func WithRecovery(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.ErrorContext(r.Context(), "http: handler panic",
				"panic", rec,
				"request_id", RequestID(r.Context()),
				"stack", string(debug.Stack()))
			WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
