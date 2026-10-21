package integration

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// logCapture is an slog handler that records everything greenlight logs, so
// that the tests can assert on it. Derived handlers share one store.
type logCapture struct {
	store *logStore
	attrs []slog.Attr
}

type logStore struct {
	mu      sync.Mutex
	records []record
}

type record struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

func newLogCapture() *logCapture {
	return &logCapture{store: &logStore{}}
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	entry := record{Level: r.Level, Message: r.Message, Attrs: map[string]string{}}
	for _, attr := range c.attrs {
		entry.Attrs[attr.Key] = attr.Value.String()
	}
	r.Attrs(func(attr slog.Attr) bool {
		entry.Attrs[attr.Key] = attr.Value.String()
		return true
	})

	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.store.records = append(c.store.records, entry)
	return nil
}

func (c *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{store: c.store, attrs: append(append([]slog.Attr{}, c.attrs...), attrs...)}
}

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// countWhere counts records that match the level, the message substring and,
// when attr is not empty, an attribute containing value.
func (c *logCapture) countWhere(level slog.Level, message, attr, value string) int {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	total := 0
	for _, entry := range c.store.records {
		if entry.Level != level || !strings.Contains(entry.Message, message) {
			continue
		}
		if attr != "" && !strings.Contains(entry.Attrs[attr], value) {
			continue
		}
		total++
	}
	return total
}
