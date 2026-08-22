package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// analyzeConfigSharingStateDir builds an analyze.Options.Cfg that reads
// and writes the same $STATE_DIR the state.Store under test uses, so a
// history entry analyze's Run writes through state.Process on tick N is
// the one loaded back by analyze's own history window on tick N+1, the
// real production wiring. DEEP_ENABLED=0 keeps the repro to one agy call
// per tick; the defect under test lives entirely in the triage/resolve
// path, not the deep dive.
func analyzeConfigSharingStateDir(t *testing.T, stateDir string) *config.Config {
	t.Helper()
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("AGY_HOME", t.TempDir())
	t.Setenv("SENTINEL_HOSTNAME", "test-host")
	t.Setenv("AGY_BIN", "agy")
	t.Setenv("DEEP_ENABLED", "0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// agyEnvelope mirrors analyze's unexported wire envelope (agy.go); the
// cross-package test cannot reach analyze's unexported test helpers, so
// it builds the envelope by hand from the documented shape
// (contracts/analyze.md §6 step 4).
func agyEnvelope(t *testing.T, rep report.Report) []byte {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal fixture report: %v", err)
	}
	env := struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Usage    struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}{Status: "SUCCESS", Response: string(b)}
	env.Usage.InputTokens = 1
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

const outageKernelMsg = "kernel: SENTINEL-TEST: forced test error line, open across an outage"

// TestResolvedAcrossOutage_GenuineAllClear_MustCloseTheAlert reproduces
// issue #39 end to end through the real seam: analyze.Run followed by
// state.Process, three ticks in a row, sharing one $STATE_DIR exactly as
// runtime wires them in production.
//
//  1. A real analyzer tick reports a kernel finding. state opens the alert.
//  2. agy fails; analyze falls back (meta.degraded=true, only its own
//     synthetic "analyzer unavailable" finding, never K). state must not
//     resolve anything from a tick that never looked at the world.
//  3. agy recovers and the underlying condition has genuinely cleared:
//     the model reports no findings. computeResolved's walk-back
//     (contracts/analyze.md §6 step 7) must skip tick 2's degraded,
//     empty-of-K entry and diff against tick 1's real one instead, so K
//     is named in resolved[] and state deletes active-alerts/<key>.json.
//     Before that walk-back existed, computeResolved compared only the
//     newest history entry unconditionally, tick 2's degraded one, K was
//     never named resolved, and the alert was only ever removed 24h
//     later by STALE_ALERT_SEC, silently (issue #39).
func TestResolvedAcrossOutage_GenuineAllClear_MustCloseTheAlert(t *testing.T) {
	stateDir := t.TempDir()
	key := dedup.Key("kernel", outageKernelMsg)
	alertPath := filepath.Join(stateDir, "active-alerts", key+".json")

	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	stateCfg := &config.Config{
		StateDir: stateDir, HistoryKeep: 50, RenotifyAlertSec: 3600,
		RenotifyWatchSec: 21600, StaleAlertSec: 86400, HeartbeatHour: 8,
		OutboxMax: 50, OutboxSMTPAfter: 3, TickInterval: 5 * time.Minute,
		TZ: "UTC", Loc: loc, Now: time.Unix(1_700_000_000, 0).UTC(),
	}
	store := newStore(t, stateCfg)

	kernelFinding := report.Finding{
		Severity: "watch", Component: "kernel", Evidence: outageKernelMsg,
		Explanation: "A synthetic kernel error line appeared this tick.",
	}
	if kernelFinding.Evidence == "" {
		t.Fatal("test setup: fixture evidence is empty")
	}

	// --- tick 1: real report, the alert opens ---
	analyzeCfg1 := analyzeConfigSharingStateDir(t, stateDir)
	rec := []report.Report{{
		Status: "WATCH", Headline: "Kernel error observed", Body: "b",
		Findings: []report.Finding{kernelFinding}, Resolved: []string{},
	}}
	d := analyze.Deps{RunAgy: func(ctx context.Context, o analyze.Options, promptPath, schemaPath string) ([]byte, error) {
		return agyEnvelope(t, rec[0]), nil
	}}
	f := &facts.Facts{Meta: facts.Meta{SchemaVersion: facts.SchemaVersion, Hostname: "bam", TickSeq: 1, Mode: "tick", Window: "10m", CollectorErrors: []facts.CollectorError{}}}
	rep1, err := analyze.Run(context.Background(), analyze.Options{Cfg: analyzeCfg1, Facts: f, Seq: 1}, d)
	if err != nil {
		t.Fatalf("tick 1 analyze.Run: unexpected error: %v", err)
	}
	if len(rep1.Findings) != 1 || rep1.Findings[0].Key != key {
		t.Fatalf("test setup: tick 1 report does not carry the expected finding key: %+v", rep1.Findings)
	}
	b1, err := json.Marshal(rep1)
	if err != nil {
		t.Fatal(err)
	}
	mustProcess(t, store, b1)
	if _, err := os.Stat(alertPath); err != nil {
		t.Fatalf("test setup: tick 1 must open the alert file: %v", err)
	}

	// --- tick 2: agy fails, analyze falls back ---
	stateCfg.Now = stateCfg.Now.Add(5 * time.Minute)
	analyzeCfg2 := analyzeConfigSharingStateDir(t, stateDir)
	dFail := analyze.Deps{RunAgy: func(ctx context.Context, o analyze.Options, promptPath, schemaPath string) ([]byte, error) {
		return nil, os.ErrNotExist // agy binary unreachable this tick
	}}
	rep2, err := analyze.Run(context.Background(), analyze.Options{Cfg: analyzeCfg2, Facts: f, Seq: 2}, dFail)
	if err == nil {
		t.Fatal("test setup: tick 2 must simulate an analyzer failure (non-nil error)")
	}
	if rep2 == nil || rep2.Meta == nil || !rep2.Meta.Degraded {
		t.Fatalf("test setup: tick 2 report must carry meta.degraded=true, got %+v", rep2)
	}
	// The fallback carries its own synthetic "analyzer unavailable" meta
	// finding, never K: a fallback tick never looked at kernel/zfs/etc.
	// state, so nothing it says can be evidence about K's status either
	// way, which is exactly the case computeResolved must not mistake for
	// "K is gone".
	for _, fnd := range rep2.Findings {
		if fnd.Key == key {
			t.Fatalf("test setup: tick 2 fallback must not carry K's key, got %+v", rep2.Findings)
		}
	}
	b2, err := json.Marshal(rep2)
	if err != nil {
		t.Fatal(err)
	}
	mustProcess(t, store, b2)
	if _, err := os.Stat(alertPath); err != nil {
		t.Fatalf("test setup: alert must still be open after a mid-outage tick that saw nothing: %v", err)
	}

	// --- tick 3: agy recovers, the condition has genuinely cleared ---
	stateCfg.Now = stateCfg.Now.Add(5 * time.Minute)
	analyzeCfg3 := analyzeConfigSharingStateDir(t, stateDir)
	clean := report.Report{Status: "OK", Headline: "All clear", Body: "b", Findings: []report.Finding{}, Resolved: []string{}}
	dClean := analyze.Deps{RunAgy: func(ctx context.Context, o analyze.Options, promptPath, schemaPath string) ([]byte, error) {
		return agyEnvelope(t, clean), nil
	}}
	rep3, err := analyze.Run(context.Background(), analyze.Options{Cfg: analyzeCfg3, Facts: f, Seq: 3}, dClean)
	if err != nil {
		t.Fatalf("tick 3 analyze.Run: unexpected error: %v", err)
	}
	if len(rep3.Findings) != 0 {
		t.Fatalf("test setup: tick 3 must report no findings (the condition genuinely cleared): %+v", rep3.Findings)
	}
	b3, err := json.Marshal(rep3)
	if err != nil {
		t.Fatal(err)
	}
	d3 := mustProcess(t, store, b3)

	// The real assertion: a finding open before the outage and genuinely
	// cleared after it must be reported resolved and its alert file
	// deleted, not left to expire silently 24h later via STALE_ALERT_SEC.
	//
	// state.Process (S.3e/g rule 2) renders resolved[] as the *stored
	// headline*, never the raw key, that translation is the documented
	// human-facing output, so the assertion checks for the tick 1
	// headline ("Kernel error observed"), not the hex key: matching on
	// the key here would fail for the right bug for the wrong reason.
	found := false
	for _, r := range d3.Report.Resolved {
		if r == rep1.Headline {
			found = true
		}
	}
	if !found {
		t.Errorf("tick 3 report.resolved[] = %v, want it to contain %q: the finding open across the outage was never announced resolved", d3.Report.Resolved, rep1.Headline)
	}
	if _, err := os.Stat(alertPath); err == nil {
		t.Errorf("active-alerts/%s.json still exists after a genuine all-clear: orphaned, will expire silently after STALE_ALERT_SEC with no all-clear ever sent (issue #39)", key)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error stat-ing alert file: %v", err)
	}
}
