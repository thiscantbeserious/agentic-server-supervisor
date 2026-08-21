package logging

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"testing"
)

var lineRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z [A-Z]+ \S+ .+$`)

func TestHandlerLineFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(&buf, slog.LevelInfo)).With("component", "collect")
	logger.Info("section done", "section", "kernel", "bytes", 128)

	line := buf.String()
	if line == "" {
		t.Fatal("expected a log line, got empty output")
	}
	// Trailing newline expected, trim for the regex/field checks.
	trimmed := line[:len(line)-1]
	if !lineRe.MatchString(trimmed) {
		t.Fatalf("line %q does not match <ts> <LEVEL> <component> <message> [k=v ...]", trimmed)
	}
	if !bytes.Contains(buf.Bytes(), []byte(" INFO collect section done section=kernel bytes=128")) {
		t.Fatalf("line = %q, missing expected fixed fields", trimmed)
	}
}

func TestHandlerLevels(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		logger := slog.New(New(&buf, slog.LevelDebug)).With("component", "state")
		logger.Log(context.Background(), tc.level, "msg")
		if !bytes.Contains(buf.Bytes(), []byte(" "+tc.want+" state msg")) {
			t.Errorf("level %v: line = %q, want level token %q", tc.level, buf.String(), tc.want)
		}
	}
}

func TestHandlerRespectsMinLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(&buf, slog.LevelWarn)).With("component", "notify")
	logger.Info("should be suppressed")
	if buf.Len() != 0 {
		t.Fatalf("expected no output below the configured level, got %q", buf.String())
	}
	logger.Warn("should appear")
	if buf.Len() == 0 {
		t.Fatal("expected output at the configured level")
	}
}

// ParseLevel is the mapping every LOG_LEVEL-honoring construction site
// (internal/state, internal/analyze, cmd/sentinel) routes through instead
// of hardcoding slog.LevelInfo, this is the one place that mapping can
// be wrong for all three at once.
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},      // unset default (config.Load's own default)
		{"bogus", slog.LevelInfo}, // config.Load already rejects this at exit 78; pure mapping falls back rather than erroring again
	}
	for _, tc := range cases {
		if got := ParseLevel(tc.in); got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestHandlerMissingComponentDefaultsToDash(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(&buf, slog.LevelInfo))
	logger.Info("no component set")
	if !bytes.Contains(buf.Bytes(), []byte(" INFO - no component set")) {
		t.Fatalf("line = %q, expected \"-\" placeholder for missing component", buf.String())
	}
}

func TestHandlerNoAttrsNoTrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(&buf, slog.LevelInfo)).With("component", "health")
	logger.Info("ok")
	line := buf.String()
	trimmed := line[:len(line)-1]
	if bytes.HasSuffix([]byte(trimmed), []byte(" ")) {
		t.Fatalf("line %q must not have trailing whitespace when there are no extra attrs", trimmed)
	}
	if trimmed[len(trimmed)-2:] != "ok" {
		t.Fatalf("line %q should end exactly with the message when there are no k=v pairs", trimmed)
	}
}
