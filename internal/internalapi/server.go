package internalapi

import (
	"log/slog"
	"net/http"
	"time"
)

// NewServer returns an http.Server for the internal API with the
// connection hook and the limits it needs. Serve it on a Listener.
func NewServer(handler http.Handler, requestTimeout time.Duration, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ConnContext:       ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout + 5*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
