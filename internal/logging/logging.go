// Package logging configures the structured logger.
//
// Info and debug records go to stdout, warn and error records go to stderr, so
// that a shell pipeline can separate normal output from problems.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds a logger for the given level ("debug", "info", "warn", "error")
// and format ("text" or "json").
func New(level, format string) (*slog.Logger, error) {
	return NewWithWriters(level, format, os.Stdout, os.Stderr)
}

// NewWithWriters is New with explicit writers. It exists for tests.
func NewWithWriters(level, format string, out, errOut io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var stdout, stderr slog.Handler
	switch strings.ToLower(format) {
	case "text", "":
		stdout, stderr = slog.NewTextHandler(out, opts), slog.NewTextHandler(errOut, opts)
	case "json":
		stdout, stderr = slog.NewJSONHandler(out, opts), slog.NewJSONHandler(errOut, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q, expected text or json", format)
	}
	return slog.New(&splitHandler{out: stdout, err: stderr}), nil
}

// ParseLevel maps a level name to an slog level.
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q, expected debug, info, warn or error", level)
	}
}

type splitHandler struct {
	out slog.Handler
	err slog.Handler
}

func (h *splitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.out.Enabled(ctx, level) || h.err.Enabled(ctx, level)
}

func (h *splitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelWarn {
		return h.err.Handle(ctx, record)
	}
	return h.out.Handle(ctx, record)
}

func (h *splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &splitHandler{out: h.out.WithAttrs(attrs), err: h.err.WithAttrs(attrs)}
}

func (h *splitHandler) WithGroup(name string) slog.Handler {
	return &splitHandler{out: h.out.WithGroup(name), err: h.err.WithGroup(name)}
}
