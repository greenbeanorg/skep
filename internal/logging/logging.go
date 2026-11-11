// Package logging builds the structured logger used by Skep services.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a slog.Logger writing to w. Level is one of debug, info, warn,
// error; format is "json" or "text".
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: unknown level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("logging: unknown format %q", format)
	}
}
