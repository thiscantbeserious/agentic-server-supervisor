package state

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// TestResolvedSeam_AnalyzeKeyClosesStateAlert is the cross-package proof
// for the resolved[] seam (contracts/analyze.md §6 step 7, contracts/
// state.md S.3(e)): analyze renders every resolved[] entry as the
// finding's 16-hex dedup.Key, and state matches it against the stored
// `key` of every active alert with exact string equality (after a
// ^[0-9a-f]{16}$ shape guard). The two components must agree on what a
// resolved[] entry IS, or an analyzer-produced resolution matches nothing
// and every all-clear is silently dropped — exactly the bug the old
// evidence/headline dual-match was patching around.
//
// Hand-writing a resolved[] string here would just re-encode whichever
// side's assumption happens to be convenient. Instead this drives
// analyze's real Run() (only its RunAgy exec seam is stubbed, per C9) over
// two ticks sharing one $STATE_DIR: tick 1 goes through state.Process and
// establishes the alert (so active-alerts/ holds a real stored key, not a
// fixture); tick 2 goes through analyze.Run, which reads that same
// history and computes resolved[] as the real production code would
// when the finding stops recurring; that report's bytes — never
// constructed by hand — are then fed into the SAME store's Process.
func TestResolvedSeam_AnalyzeKeyClosesStateAlert(t *testing.T) {
	stateDir := t.TempDir()
	const evidence = "smartd[123]: Device: /dev/sda, 1 Currently unreadable (pending) sectors"

	// --- tick 1: a real finding, processed through state.Process (not a
	// hand-built ActiveAlert), so alert.Evidence is whatever production
	// code actually stores. ---
	sCfg := testConfig(t, time.Unix(1000, 0))
	sCfg.StateDir = stateDir
	store := newStore(t, sCfg)

	tick1 := report.Report{
		Status: "WATCH", Headline: "Disk health degraded", Body: "b",
		Findings: []report.Finding{{
			Severity: "watch", Component: "smart", Evidence: evidence, Explanation: "e",
		}},
		Resolved: []string{},
	}
	b1, err := json.Marshal(tick1)
	if err != nil {
		t.Fatal(err)
	}
	d1 := mustProcess(t, store, b1)
	if !d1.Notify || d1.Reason != "new_finding" {
		t.Fatalf("tick 1 setup: notify=%v reason=%s, want new_finding", d1.Notify, d1.Reason)
	}
	key := d1.Report.Findings[0].Key
	if _, ok := store.loadAlert(key); !ok {
		t.Fatalf("tick 1 setup: alert %s not saved", key)
	}

	// --- tick 2: analyze.Run, pointed at the SAME $STATE_DIR, sees the
	// finding has stopped recurring and computes resolved[] for real. ---
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("AGY_HOME", t.TempDir())
	t.Setenv("SENTINEL_HOSTNAME", "test-host")
	t.Setenv("AGY_BIN", "agy")
	aCfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Empty findings deliberately, not even an unrelated "meta" info
	// finding: a second finding in the SAME tick would legitimately win
	// step (g)'s reason ("new_finding" outranks "all_clear" by input
	// order), which would make this test assert the wrong thing about
	// reason rather than about the resolved-matching fix under test.
	cleanTick := report.Report{
		Status: "OK", Headline: "All systems normal", Body: "Nothing to report.",
		Findings: []report.Finding{},
		Resolved: []string{},
	}
	modelResponse, err := json.Marshal(cleanTick)
	if err != nil {
		t.Fatal(err)
	}
	// RunAgy's real contract (§6 step 4) is the agy --output-format json
	// envelope, not the bare model response — wrap it the same way
	// analyze's own test helpers (mustEnvelope) do, so this exercises the
	// real decode path rather than a shortcut only this test would take.
	envelope := struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Usage    struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}{Status: "SUCCESS", Response: string(modelResponse)}
	envelope.Usage.InputTokens = 1
	stubOut, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	deps := analyze.Deps{
		RunAgy: func(ctx context.Context, o analyze.Options, promptPath, schemaPath string) ([]byte, error) {
			return stubOut, nil
		},
	}
	f := &facts.Facts{Meta: facts.Meta{
		SchemaVersion: facts.SchemaVersion, Hostname: "bam", TickSeq: 2,
		Mode: "tick", Window: "10m", CollectorErrors: []facts.CollectorError{},
	}}

	rep2, err := analyze.Run(context.Background(), analyze.Options{Cfg: aCfg, Facts: f, Seq: 2}, deps)
	if err != nil {
		t.Fatalf("analyze.Run: %v", err)
	}
	if len(rep2.Resolved) != 1 {
		t.Fatalf("analyze.Run: resolved = %v, want exactly one entry (the smart finding dropped out)", rep2.Resolved)
	}
	// Setup guard: this must be the 16-hex key the whole test exists to
	// exercise, not a headline or evidence snippet (which would make the
	// test vacuous — matching those shapes never touched the seam).
	if rep2.Resolved[0] != key {
		t.Fatalf("setup guard: resolved[0] = %q, want the tick-1 alert's own key %q", rep2.Resolved[0], key)
	}

	b2, err := json.Marshal(rep2)
	if err != nil {
		t.Fatal(err)
	}

	// --- tick 3: feed analyze's REAL output back into the SAME state
	// store. If state's key-matching regressed, this resolved[] entry
	// would match nothing — the alert would still be sitting in
	// active-alerts/ right now. ---
	d3 := mustProcess(t, store, b2)
	if _, ok := store.loadAlert(key); ok {
		t.Errorf("alert %s still open after analyze's real resolved[] output — S.3(e) key-matching regressed", key)
	}
	if d3.Reason != "all_clear" || len(d3.Report.Resolved) != 1 {
		t.Errorf("tick 3: reason=%s resolved=%v, want reason=all_clear with exactly the one closed headline", d3.Reason, d3.Report.Resolved)
	}
}
