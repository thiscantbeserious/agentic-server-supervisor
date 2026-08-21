package state

import (
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// TestCrossPackage_AnalyzeAndStateDeriveTheSameKey is S.9 case 19's other
// half and C9's "cross-package agreement is asserted, not assumed": one
// test proves analyze and state derive the identical key from the same
// evidence. literalDedupKey (used elsewhere in this package) only proves
// state matches the C6 algorithm in isolation, it never calls analyze.
//
// analyze.Fallback is the cheapest real, LLM-free path through analyze
// that computes a key (fallback.go: key := dedup.Key("meta", raw)). A
// single short, low-priority kernel line makes raw == the finding's
// evidence exactly (no truncation), so the finding analyze hands back can
// be fed into state.Process, with its key stripped, so state must
// recompute it, and the two keys compared directly.
func TestCrossPackage_AnalyzeAndStateDeriveTheSameKey(t *testing.T) {
	cfg := &config.Config{RawAlertMaxPriority: 2, RawAlertMaxLines: 20, Hostname: "host"}
	line := "kernel: nvme0n1: I/O error, dev nvme0n1, sector 12345"
	f := &facts.Facts{
		Kernel: &facts.Section[facts.KernelData]{
			Data: facts.KernelData{
				Entries: []facts.Entry{{Priority: 2, Identifier: "kernel", Message: "nvme0n1: I/O error, dev nvme0n1, sector 12345"}},
			},
		},
	}

	analyzeRep := analyze.Fallback(cfg, 1, "agy_missing", f)
	if len(analyzeRep.Findings) != 1 {
		t.Fatalf("analyze.Fallback: %d findings, want 1", len(analyzeRep.Findings))
	}
	af := analyzeRep.Findings[0]
	if af.Evidence != line {
		t.Fatalf("test setup: analyze's evidence = %q, want %q (must match exactly, no truncation, for this test to be meaningful)", af.Evidence, line)
	}
	if af.Key == "" {
		t.Fatal("test setup: analyze.Fallback did not set a key")
	}

	stateCfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, stateCfg)
	// Key deliberately stripped: state must recompute it from Component+Evidence.
	fWithoutKey := report.Finding{Severity: af.Severity, Component: af.Component, Evidence: af.Evidence, Explanation: af.Explanation}
	b := marshalReport(t, &report.Report{Status: "ALERT", Headline: "H", Body: "b", Findings: []report.Finding{fWithoutKey}})
	d := mustProcess(t, s, b)

	if len(d.Report.Findings) != 1 {
		t.Fatalf("state: %d findings, want 1", len(d.Report.Findings))
	}
	if d.Report.Findings[0].Key != af.Key {
		t.Errorf("key mismatch: analyze computed %q, state computed %q for the same (component, evidence)", af.Key, d.Report.Findings[0].Key)
	}
}
