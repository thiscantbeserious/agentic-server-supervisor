// Package runtime is the tick/loop orchestrator: `sentinel tick` and
// `sentinel health` (R1-R8). It calls collect/analyze/state/notify as
// in-process Go functions (C8) — only journalctl, sensors -j and agy are
// exec'd, each under context.WithTimeout, inside the packages that own
// them. There is no exit-code round-tripping between components.
//
// The binding spec is contracts/runtime.md.
package runtime

import (
	"io"
	"log/slog"
	"os"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/logging"
)

// logWriter lets tests capture this package's exact C7 lines; the zero
// value means stderr.
var logWriter io.Writer

func newLogger(cfg *config.Config) *slog.Logger {
	w := logWriter
	if w == nil {
		w = os.Stderr
	}
	return slog.New(logging.New(w, logging.ParseLevel(cfg.LogLevel))).With("component", "runtime")
}
