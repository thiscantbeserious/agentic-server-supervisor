package state

import (
	"testing"
	"time"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/report"
)

// Process must not panic when cfg.Loc is nil, config.Load always resolves
// it, but Process is a public API and must degrade gracefully (UTC),
// not crash a whole tick, if some future caller skips Load.
func TestProcess_NilLocDoesNotPanic(t *testing.T) {
	cfg := &config.Config{
		StateDir: t.TempDir(), HistoryKeep: 50, RenotifyAlertSec: 3600, RenotifyWatchSec: 21600,
		StaleAlertSec: 86400, HeartbeatHour: 8, OutboxMax: 50, OutboxSMTPAfter: 3,
		TickInterval: 5 * time.Minute, Now: time.Unix(1000, 0), // Loc left nil deliberately
	}
	s := newStore(t, cfg)
	b := marshalReport(t, &report.Report{Status: "OK", Headline: "H", Body: "b"})
	if _, err := s.Process(b); err != nil {
		t.Fatalf("Process with nil cfg.Loc: %v", err)
	}
}
