// Package logging provides the one slog.Handler used by the whole binary,
// emitting exactly the C7 line format to the given writer (stderr in
// production):
//
//	<RFC3339-UTC> <LEVEL> <component> <message> [k=v ...]
//
// Callers set the component via logger.With("component", "collect"). Never
// log $AGY_SECRET_DIR contents, APPRISE_KEY, MAILRISE_PASS, TELEGRAM_*
// values, prompt or facts content, or agy stdout (C7), the handler cannot
// enforce that; it is a discipline for every call site.
package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

const componentKey = "component"

// ParseLevel maps a LOG_LEVEL string to its slog.Level. internal/config's
// Load already restricts LOG_LEVEL to exactly DEBUG/INFO/WARN/ERROR (exit
// 78 otherwise), this is a pure mapping of that validated value, not a
// second validator, so anything else (including "") falls back to
// LevelInfo rather than erroring again.
func ParseLevel(s string) slog.Level {
	switch s {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type handler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	attrs []slog.Attr
}

// New builds a slog.Handler that writes the C7 line format to w, filtering
// out records below level.
func New(w io.Writer, level slog.Leveler) slog.Handler {
	return &handler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	component := "-"
	var kv []string

	collect := func(a slog.Attr) bool {
		if a.Key == componentKey {
			component = a.Value.String()
			return true
		}
		kv = append(kv, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
		return true
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool { return collect(a) })

	var buf bytes.Buffer
	buf.WriteString(r.Time.UTC().Format(time.RFC3339))
	buf.WriteByte(' ')
	buf.WriteString(r.Level.String())
	buf.WriteByte(' ')
	buf.WriteString(component)
	buf.WriteByte(' ')
	buf.WriteString(r.Message)
	for _, s := range kv {
		buf.WriteByte(' ')
		buf.WriteString(s)
	}
	buf.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf.Bytes())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{mu: h.mu, w: h.w, level: h.level, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *handler) WithGroup(_ string) slog.Handler {
	// Groups are not used anywhere in this codebase (C7 is a flat k=v line).
	return h
}
