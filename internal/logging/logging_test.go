package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestInfoGoesToStdoutAndWarningsToStderr(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	logger, err := NewWithWriters("debug", "text", out, errOut)
	if err != nil {
		t.Fatal(err)
	}

	logger.Debug("a debug message")
	logger.Info("an info message")
	logger.Warn("a warning")
	logger.Error("an error")

	if !strings.Contains(out.String(), "a debug message") || !strings.Contains(out.String(), "an info message") {
		t.Fatalf("debug and info must go to stdout:\n%s", out.String())
	}
	if strings.Contains(out.String(), "a warning") || strings.Contains(out.String(), "an error") {
		t.Fatalf("warnings and errors must not go to stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "a warning") || !strings.Contains(errOut.String(), "an error") {
		t.Fatalf("warnings and errors must go to stderr:\n%s", errOut.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	logger, err := NewWithWriters("warn", "text", out, errOut)
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("hidden")
	logger.Warn("shown")

	if out.Len() != 0 {
		t.Fatalf("info must be filtered out at level warn:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "shown") {
		t.Fatalf("the warning is missing:\n%s", errOut.String())
	}
}

func TestJSONFormatAndAttributes(t *testing.T) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	logger, err := NewWithWriters("info", "json", out, errOut)
	if err != nil {
		t.Fatal(err)
	}

	logger.With("version", "1.2.3").Info("structured", "object", "ns/name")

	text := out.String()
	if !strings.HasPrefix(strings.TrimSpace(text), "{") {
		t.Fatalf("expected JSON output:\n%s", text)
	}
	for _, want := range []string{`"version":"1.2.3"`, `"object":"ns/name"`, `"msg":"structured"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %s in:\n%s", want, text)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"":      slog.LevelInfo,
		"WARN":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for name, want := range tests {
		got, err := ParseLevel(name)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", name, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Fatal("expected an error for an unknown level")
	}
}

func TestUnknownFormat(t *testing.T) {
	if _, err := NewWithWriters("info", "xml", &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}
