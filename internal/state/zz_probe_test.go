package state

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

func probeCfg(t *testing.T) *config.Config {
	return &config.Config{
		StateDir: t.TempDir(), HistoryKeep: 50,
		RenotifyAlertSec: 3600, RenotifyWatchSec: 21600, StaleAlertSec: 86400,
		HeartbeatHour: 8, OutboxMax: 50, OutboxSMTPAfter: 3,
		TickInterval: 5 * time.Minute, TZ: "UTC", Loc: time.UTC,
	}
}

func mustJSON(r report.Report) []byte { b, _ := json.Marshal(r); return b }

// PROBE A — S.3(f): heartbeat must fire at 08:01 after a 07:59 tick.
func TestProbeA_HeartbeatNeverFires(t *testing.T) {
	cfg := probeCfg(t)
	s, _ := New(cfg)
	empty := mustJSON(report.Report{Status: "OK", Headline: "Nothing", Body: "Nothing to report.",
		Findings: []report.Finding{}, Resolved: []string{}})

	cfg.Now = time.Date(2000, 1, 1, 7, 59, 0, 0, time.UTC)
	d1, err := s.Process(empty)
	if err != nil { t.Fatal(err) }
	t.Logf("07:59 -> heartbeat=%v reason=%q", d1.Heartbeat, d1.Reason)

	cfg.Now = time.Date(2000, 1, 1, 8, 1, 0, 0, time.UTC)
	d2, err := s.Process(empty)
	if err != nil { t.Fatal(err) }
	if !d2.Heartbeat {
		t.Errorf("08:01 after a 07:59 tick: heartbeat=%v reason=%q, want heartbeat=true reason=heartbeat",
			d2.Heartbeat, d2.Reason)
	}
}

// PROBE B — S.3(g) rule 1: resolved = all_clear when a notification coincides.
func TestProbeB_AllClearDroppedByRule1(t *testing.T) {
	cfg := probeCfg(t)
	s, _ := New(cfg)
	cfg.Now = time.Unix(1000, 0)

	// Tick 1: open an alert whose headline we will later resolve.
	old := mustJSON(report.Report{Status: "ALERT", Headline: "Load average elevated on bam",
		Body: "Load is high.", Resolved: []string{},
		Findings: []report.Finding{{Severity: "alert", Component: "resources",
			Evidence: "load average: 42.0", Explanation: "high load"}}})
	if _, err := s.Process(old); err != nil { t.Fatal(err) }

	// Tick 2: a DIFFERENT new finding notifies, and the old headline resolves.
	cfg.Now = time.Unix(2000, 0)
	next := mustJSON(report.Report{Status: "ALERT", Headline: "ZFS checksum errors on hotstore",
		Body: "Checksum errors.", Resolved: []string{"Load average elevated on bam"},
		Findings: []report.Finding{{Severity: "alert", Component: "zfs",
			Evidence: "cksum_errors=7", Explanation: "cksum"}}})
	d, err := s.Process(next)
	if err != nil { t.Fatal(err) }
	t.Logf("notify=%v reason=%q resolved=%v", d.Notify, d.Reason, d.Report.Resolved)
	if len(d.Report.Resolved) != 1 {
		t.Errorf("report.resolved = %v (len %d), want the 1 all-clear entry (S.3 g rule 1: `resolved` = all_clear)",
			d.Report.Resolved, len(d.Report.Resolved))
	}
}

// PROBE C — S.4: fallback_smtp true once attempts >= OUTBOX_SMTP_AFTER (3).
func TestProbeC_OutboxAttemptsOffByOne(t *testing.T) {
	cfg := probeCfg(t)
	s, _ := New(cfg)
	cfg.Now = time.Unix(1000, 0)
	if _, err := s.OutboxAdd([]byte(`{"status":"OK","headline":"h","body":"b","findings":[],"resolved":[]}`)); err != nil {
		t.Fatal(err)
	}
	for take := 1; take <= 3; take++ {
		items, _ := s.OutboxTake()
		if len(items) != 1 { t.Fatalf("take %d: %d items", take, len(items)) }
		t.Logf("take %d -> attempts=%d fallback_smtp=%v", take, items[0].Attempts, items[0].FallbackSMTP)
		if take == 1 && items[0].Attempts != 1 {
			t.Errorf("first take: attempts=%d, want 1 (S.9 case 10)", items[0].Attempts)
		}
		if take < 3 && items[0].FallbackSMTP {
			t.Errorf("take %d: fallback_smtp already true at attempts=%d, want it to flip only at attempt 3", take, items[0].Attempts)
		}
	}
}
