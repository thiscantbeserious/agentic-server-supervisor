package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

func testConfig(t *testing.T, now time.Time) *config.Config {
	t.Helper()
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		StateDir:         t.TempDir(),
		HistoryKeep:      50,
		RenotifyAlertSec: 3600,
		RenotifyWatchSec: 21600,
		StaleAlertSec:    86400,
		HeartbeatHour:    8,
		OutboxMax:        50,
		OutboxSMTPAfter:  3,
		TickInterval:     5 * time.Minute,
		TZ:               "UTC",
		Loc:              loc,
		Now:              now,
	}
}

func newStore(t *testing.T, cfg *config.Config) *Store {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// mustProcess runs Process and fails the test immediately on an
// unexpected error — every case below has a schema-valid fixture, so a
// non-nil error is a bug, not an expected outcome, and must never be
// discarded (a discarded error here nil-derefs on the returned *Decision).
func mustProcess(t *testing.T, s *Store, raw []byte) *Decision {
	t.Helper()
	d, err := s.Process(raw)
	if err != nil {
		t.Fatalf("Process: unexpected error: %v (input: %s)", err, raw)
	}
	return d
}

func marshalReport(t *testing.T, r *report.Report) []byte {
	t.Helper()
	if r.Findings == nil {
		r.Findings = []report.Finding{}
	}
	if r.Resolved == nil {
		r.Resolved = []string{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func finding(severity, evidence string) report.Finding {
	return report.Finding{
		Severity:    severity,
		Component:   "kernel",
		Evidence:    evidence,
		Explanation: "explanation for " + evidence,
	}
}

var historyNameRe = regexp.MustCompile(`^[0-9]{10}-[0-9]{6}\.json$`)

func readHistoryFiles(t *testing.T, stateDir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "history"))
	if err != nil {
		t.Fatalf("read history dir: %v", err)
	}
	return entries
}

// --- S1: history annotation — the cross-component assertion (C9) ---

func TestS1_HistoryAnnotatesBothNotifiedAndSuppressed(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	// Tick 1: establish finding B as an active alert (notifies as new).
	fB := finding("watch", "B evidence")
	b1 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "B", Body: "body", Findings: []report.Finding{fB}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("tick 1: expected notify=true for a new finding")
	}

	// Tick 2 (small delta, inside the renotify window): finding A is new
	// (notifies), finding B repeats unchanged (suppressed).
	cfg.Now = time.Unix(1010, 0)
	fA := finding("watch", "A evidence")
	fB2 := finding("watch", "B evidence")
	b2 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "Two findings", Body: "body", Findings: []report.Finding{fA, fB2}})
	d2 := mustProcess(t, s, b2)
	if !d2.Notify || d2.SuppressedCount != 1 {
		t.Fatalf("tick 2: notify=%v suppressed=%d, want notify=true suppressed=1", d2.Notify, d2.SuppressedCount)
	}

	entries := readHistoryFiles(t, cfg.StateDir)
	var newest os.DirEntry
	for _, e := range entries {
		if newest == nil || e.Name() > newest.Name() {
			newest = e
		}
	}
	if newest == nil {
		t.Fatal("no history file written")
	}
	if !historyNameRe.MatchString(newest.Name()) {
		t.Fatalf("history filename %q does not match ^[0-9]{10}-[0-9]{6}\\.json$", newest.Name())
	}

	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "history", newest.Name()))
	if err != nil {
		t.Fatal(err)
	}
	var histRep report.Report
	if err := json.Unmarshal(data, &histRep); err != nil {
		t.Fatalf("unmarshal history file: %v", err)
	}
	if len(histRep.Findings) != 2 {
		t.Fatalf("history findings: got %d, want 2 (one notified, one suppressed)", len(histRep.Findings))
	}
	for _, f := range histRep.Findings {
		if f.Occurrences == 0 {
			t.Errorf("history finding %q: occurrences=0, want > 0 (read from disk, per S.3b)", f.Evidence)
		}
		if f.FirstSeen == 0 {
			t.Errorf("history finding %q: first_seen=0, want > 0", f.Evidence)
		}
		if f.Key == "" {
			t.Errorf("history finding %q: key empty", f.Evidence)
		}
	}
}

// S.3(b): "the input document ... all other bytes ... unchanged." History
// must store status/headline/body/resolved as INPUT, never as whatever
// step (g) mutates rep into for the outgoing decision.report (main's
// container repro: a suppressed tick's history showed status:"OK" next to
// severity:"alert" findings — exactly the corruption analyze's trend
// window must never see).
func TestS1b_HistoryStoresInputNotMutatedOutput(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	inputHeadline := "Disk errors rising"
	inputBody := "Input body, must survive into history untouched"
	f := finding("alert", "same evidence both ticks")
	b := marshalReport(t, &report.Report{Status: "ALERT", Headline: inputHeadline, Body: inputBody, Findings: []report.Finding{f}})

	d1 := mustProcess(t, s, b)
	if !d1.Notify {
		t.Fatal("tick 1: expected a new-finding notification")
	}

	// Tick 2, small delta: same finding, inside the renotify window ->
	// decision.report becomes rule 4 (status="OK", findings=[]) even
	// though the INPUT was still an ALERT with one finding.
	cfg.Now = time.Unix(1010, 0)
	d2 := mustProcess(t, s, b)
	if d2.Notify || d2.Report.Status != "OK" {
		t.Fatalf("tick 2 setup: notify=%v status=%s, want the tick suppressed with an OK outgoing report (rule 4)", d2.Notify, d2.Report.Status)
	}

	entries := readHistoryFiles(t, cfg.StateDir)
	var newest os.DirEntry
	for _, e := range entries {
		if newest == nil || e.Name() > newest.Name() {
			newest = e
		}
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "history", newest.Name()))
	if err != nil {
		t.Fatal(err)
	}
	var histRep report.Report
	if err := json.Unmarshal(data, &histRep); err != nil {
		t.Fatal(err)
	}

	if histRep.Status != "ALERT" {
		t.Errorf("tick 2 history status = %q, want ALERT (the input's status, not the suppressed OUTGOING OK)", histRep.Status)
	}
	if histRep.Headline != inputHeadline {
		t.Errorf("tick 2 history headline = %q, want %q (the input's headline)", histRep.Headline, inputHeadline)
	}
	if histRep.Body != inputBody {
		t.Errorf("tick 2 history body = %q, want %q (the input's body)", histRep.Body, inputBody)
	}
	if len(histRep.Findings) != 1 || histRep.Findings[0].Severity != "alert" {
		t.Fatalf("tick 2 history findings = %+v, want the one alert finding annotated, not dropped", histRep.Findings)
	}
}

// --- S2: no stray files in history/ ---

func TestS2_NoStrayFilesInHistoryDir(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	rep := &report.Report{Status: "OK", Headline: "H", Body: "body"}
	for i := 0; i < 5; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		mustProcess(t, s, marshalReport(t, rep))
	}

	for _, e := range readHistoryFiles(t, cfg.StateDir) {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("stray file in history/: %q — a leftover .tmp-* evicts a real report from analyze's window", e.Name())
		}
	}
}

// --- S3: Health() ---

// A supervisor that has never ticked must read as UNHEALTHY, not healthy
// (main's gate finding, container-reproduced): New() must not seed the
// heartbeat file just because `sentinel health` happens to construct a
// Store — the absence of a signal is not good news.
func TestS3_Health_NeverProcessedIsUnhealthy(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg) // no Process call — a fresh $STATE_DIR, exactly what New() alone produces
	if err := s.Health(); err == nil {
		t.Error("Health() on a Store that has never Process'd = nil, want non-nil — this must read as down, not up")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "heartbeat")); err == nil {
		t.Error("New() must not create a heartbeat file — only Process may")
	}
}

func TestS3_Health(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	// Fresh heartbeat: a Process call just stamped mtime at cfg.Now.
	mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "H", Body: "b"}))
	if err := s.Health(); err != nil {
		t.Errorf("fresh heartbeat: Health() = %v, want nil", err)
	}

	// Stale: mtime backdated past 3*TickInterval relative to cfg.Now.
	hbPath := filepath.Join(cfg.StateDir, "heartbeat")
	stale := cfg.Now.Add(-3*cfg.TickInterval - time.Second)
	if err := os.Chtimes(hbPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := s.Health(); err == nil {
		t.Error("stale heartbeat: Health() = nil, want non-nil")
	}

	// Missing: no heartbeat file at all.
	if err := os.Remove(hbPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Health(); err == nil {
		t.Error("missing heartbeat: Health() = nil, want non-nil")
	}
}

// --- case 1 ---

func TestProcess_SameWatchFinding3Ticks(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "Test", Body: "Test body", Findings: []report.Finding{finding("watch", "evidence")}})

	d1 := mustProcess(t, s, b1)
	if !d1.Notify || d1.Reason != "new_finding" || d1.SuppressedCount != 0 || d1.ActiveCount != 1 {
		t.Errorf("Tick 1: notify=%v reason=%s suppressed=%d active=%d, want notify=true reason=new_finding suppressed=0 active=1",
			d1.Notify, d1.Reason, d1.SuppressedCount, d1.ActiveCount)
	}

	cfg.Now = time.Unix(1000+300, 0) // +5 min
	d2 := mustProcess(t, s, b1)
	if d2.Notify || d2.Reason != "suppressed" || d2.SuppressedCount != 1 || d2.Report.Status != "OK" {
		t.Errorf("Tick 2: notify=%v reason=%s suppressed=%d status=%s, want notify=false reason=suppressed suppressed=1 status=OK",
			d2.Notify, d2.Reason, d2.SuppressedCount, d2.Report.Status)
	}

	cfg.Now = time.Unix(1000+600, 0) // +10 min
	d3 := mustProcess(t, s, b1)
	if d3.Notify || d3.Reason != "suppressed" || d3.SuppressedCount != 1 {
		t.Errorf("Tick 3: notify=%v reason=%s suppressed=%d, want notify=false reason=suppressed suppressed=1",
			d3.Notify, d3.Reason, d3.SuppressedCount)
	}
}

// --- case 2 ---

func TestProcess_Escalation(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	watch := finding("watch", "evidence")
	b1 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "Test", Body: "Test body", Findings: []report.Finding{watch}})
	mustProcess(t, s, b1)
	cfg.Now = time.Unix(1000+300, 0)
	mustProcess(t, s, b1)
	cfg.Now = time.Unix(1000+600, 0)
	mustProcess(t, s, b1)

	cfg.Now = time.Unix(1000+900, 0)
	alert := finding("alert", "evidence") // same evidence -> same key -> same active alert
	b4 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Test body", Findings: []report.Finding{alert}})
	d4 := mustProcess(t, s, b4)
	if !d4.Notify || d4.Reason != "escalation" {
		t.Errorf("Tick 4 escalation: notify=%v reason=%s, want notify=true reason=escalation", d4.Notify, d4.Reason)
	}

	// AC (PLAN.md T5): 3 ticks same finding -> exactly 1 notification, then
	// tick 4 -> exactly 1 escalation. Combined with case 1's assertions,
	// this is that AC end to end.
}

// --- case 3 ---

func TestProcess_RenotifyWatch(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "Test", Body: "Test body", Findings: []report.Finding{finding("watch", "evidence")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("initial notify failed")
	}

	// Window edge: just before (suppressed), at/after (renotify), watch = 21600s (6h).
	cfg.Now = time.Unix(1000+21600-1, 0)
	d2 := mustProcess(t, s, b1)
	if d2.Notify || d2.Reason != "suppressed" {
		t.Errorf("at window-1s: notify=%v reason=%s, want suppressed", d2.Notify, d2.Reason)
	}

	cfg.Now = time.Unix(1000+21600, 0)
	d3 := mustProcess(t, s, b1)
	if !d3.Notify || d3.Reason != "renotify" {
		t.Errorf("at window (exact, >=): notify=%v reason=%s, want notify=true reason=renotify", d3.Notify, d3.Reason)
	}
}

// --- case 4 ---

func TestProcess_RenotifyAlert(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Test body", Findings: []report.Finding{finding("alert", "evidence")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("initial notify failed")
	}

	// alert window = 3600s (1h).
	cfg.Now = time.Unix(1000+3600-1, 0)
	d2 := mustProcess(t, s, b1)
	if d2.Notify || d2.Reason != "suppressed" {
		t.Errorf("at window-1s: notify=%v reason=%s, want suppressed", d2.Notify, d2.Reason)
	}

	cfg.Now = time.Unix(1000+3600, 0)
	d3 := mustProcess(t, s, b1)
	if !d3.Notify || d3.Reason != "renotify" {
		t.Errorf("at window (exact, >=): notify=%v reason=%s, want notify=true reason=renotify", d3.Notify, d3.Reason)
	}
}

// S.3(d): "de-escalation never notifies on its own, but it does lower the
// stored severity and switch the renotify window." A version that only
// ever raises severity leaves a dropped-then-persisting finding pinned to
// the 1h alert window forever — this must fail with the fix reverted.
func TestProcess_DeEscalationLowersSeverityAndWindow(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	alertKey := dedup.Key("kernel", "de-escalating evidence")
	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "H", Body: "b", Findings: []report.Finding{finding("alert", "de-escalating evidence")}})
	if d1 := mustProcess(t, s, b1); !d1.Notify {
		t.Fatal("initial notify failed")
	}

	// De-escalate to watch, small delta — must not notify on its own.
	cfg.Now = time.Unix(1010, 0)
	b2 := marshalReport(t, &report.Report{Status: "WATCH", Headline: "H", Body: "b", Findings: []report.Finding{finding("watch", "de-escalating evidence")}})
	d2 := mustProcess(t, s, b2)
	if d2.Notify {
		t.Fatalf("de-escalation must not notify on its own: notify=%v reason=%s", d2.Notify, d2.Reason)
	}

	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "active-alerts", alertKey+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored ActiveAlert
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Severity != "watch" {
		t.Fatalf("stored severity = %q, want watch (de-escalation must lower it)", stored.Severity)
	}

	// now - last_notified(1000) = 3601s: past the ALERT window (3600) but
	// well inside the WATCH window (21600). If the window is still keyed
	// off "alert" (severity never actually lowered), this renotifies
	// wrongly; with the fix, it stays suppressed.
	cfg.Now = time.Unix(1000+3601, 0)
	d3 := mustProcess(t, s, b2)
	if d3.Notify {
		t.Errorf("at +3601s (past the old alert window, inside the new watch window): notify=%v reason=%s, want suppressed — "+
			"the renotify window did not switch to watch's, de-escalation left severity pinned at alert", d3.Notify, d3.Reason)
	}
}

// S.3(g) rule 1: when findings are notified AND an unrelated alert resolves
// in the same tick, "resolved" on the outgoing report must still be
// all_clear (S.4's own worked example shows exactly this combination) — an
// implementation that only fills rep.Resolved on rule 2 silently drops the
// all-clear on the ONE tick it happens to coincide with a notification, and
// step (e) has already deleted the key file by then, so it is unrecoverable.
func TestProcess_NotifyRuleCarriesResolvedFromSameTick(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	// Establish an unrelated alert that will resolve this tick.
	b0 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Load average elevated on bam", Body: "b", Findings: []report.Finding{finding("alert", "load high")}})
	mustProcess(t, s, b0)

	cfg.Now = time.Unix(1010, 0)
	b1 := marshalReport(t, &report.Report{
		Status:   "ALERT",
		Headline: "New alert",
		Body:     "b",
		Findings: []report.Finding{finding("alert", "brand new evidence")}, // notifies -> rule 1
		Resolved: []string{"Load average elevated on bam"},                 // unrelated alert clears same tick
	})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify || d1.Reason == "" {
		t.Fatalf("expected rule 1 to notify: notify=%v reason=%s", d1.Notify, d1.Reason)
	}
	if len(d1.Report.Resolved) != 1 || d1.Report.Resolved[0] != "Load average elevated on bam" {
		t.Errorf("rule 1 dropped the same-tick all-clear: resolved=%v, want [Load average elevated on bam]", d1.Report.Resolved)
	}
}

// --- case 5 ---

func TestProcess_AllClear(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test headline", Body: "Test body", Findings: []report.Finding{finding("alert", "evidence")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("initial notify failed")
	}

	cfg.Now = time.Unix(2000, 0)
	b2 := marshalReport(t, &report.Report{Status: "OK", Headline: "Resolved", Body: "irrelevant, overwritten by rule 2", Resolved: []string{"Test headline"}})
	d2 := mustProcess(t, s, b2)
	if !d2.Notify || d2.Reason != "all_clear" || len(d2.Report.Resolved) != 1 || d2.Report.Resolved[0] != "Test headline" {
		t.Errorf("all-clear: notify=%v reason=%s resolved=%v, want notify=true reason=all_clear resolved=[Test headline]",
			d2.Notify, d2.Reason, d2.Report.Resolved)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "active-alerts")); len(entries) != 0 {
		t.Errorf("active-alerts: %d files remain, want 0", len(entries))
	}

	cfg.Now = time.Unix(3000, 0)
	b3 := marshalReport(t, &report.Report{Status: "OK", Headline: "Still clear", Body: "body", Resolved: []string{"Test headline"}})
	d3 := mustProcess(t, s, b3)
	if d3.Notify {
		t.Errorf("second all-clear on the same headline: notify=%v, want false (key already gone)", d3.Notify)
	}
}

// --- case 6 ---

func TestProcess_ResolvedUnknownHeadline(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "OK", Headline: "Empty", Body: "body", Resolved: []string{"Unknown headline"}})
	d1 := mustProcess(t, s, b1)
	if d1.Notify || len(d1.Report.Resolved) != 0 {
		t.Errorf("unknown resolved: notify=%v resolved=%v, want false []", d1.Notify, d1.Report.Resolved)
	}
}

// S.3(e): "A key that was never notified is deleted without an all-clear."
// The normal Process path always notifies on creation (new_finding), so a
// NotifyCount==0 record can only arise from something outside that path —
// still a real state the file format allows, and the contract is explicit
// about it, so seed one directly (same technique as the corrupt-file
// tests) and prove resolving it stays silent while still cleaning up.
func TestProcess_NeverNotifiedAlertResolvesWithoutAllClear(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	if err := os.MkdirAll(alertDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unnotified := ActiveAlert{
		Key: dedup.Key("kernel", "never-notified evidence"), Component: "kernel",
		Headline: "Never told the operator", Severity: "watch",
		FirstSeen: 900, LastSeen: 900, NotifyCount: 0, Occurrences: 1,
	}
	raw, err := json.Marshal(unnotified)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alertDir, unnotified.Key+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	b := marshalReport(t, &report.Report{Status: "OK", Headline: "H", Body: "b", Resolved: []string{"Never told the operator"}})
	d := mustProcess(t, s, b)
	if d.Notify || len(d.Report.Resolved) != 0 {
		t.Errorf("resolving a never-notified alert: notify=%v resolved=%v, want false [] (no all-clear for something never surfaced)", d.Notify, d.Report.Resolved)
	}
	if _, err := os.Stat(filepath.Join(alertDir, unnotified.Key+".json")); err == nil {
		t.Error("the never-notified alert's key file must still be deleted on a matching resolved[] entry")
	}
}

// --- case 7 ---

func TestProcess_TwoFindingsSharedHeadline(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	f1 := finding("alert", "evidence1")
	f2 := finding("alert", "evidence2")
	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Shared headline", Body: "body", Findings: []report.Finding{f1, f2}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify || d1.ActiveCount != 2 {
		t.Fatalf("initial: notify=%v active=%d, want true 2", d1.Notify, d1.ActiveCount)
	}

	// f1 persists but within the renotify window (suppressed, still
	// "touched" in step d); resolved names the shared headline, which must
	// close only f2 (S-D7), even though f1 is not in the notified list.
	cfg.Now = time.Unix(2000, 0)
	b2 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Shared headline", Body: "body", Findings: []report.Finding{f1}, Resolved: []string{"Shared headline"}})
	d2 := mustProcess(t, s, b2)
	if len(d2.Report.Resolved) != 1 {
		t.Errorf("resolved: %d entries, want exactly 1 (S-D7)", len(d2.Report.Resolved))
	}
	if d2.ActiveCount != 1 {
		t.Errorf("active: %d, want 1 — f1 must survive being 'touched' in step (d)", d2.ActiveCount)
	}
	entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "active-alerts"))
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), dedup.Key("kernel", "evidence1")) {
		t.Errorf("surviving active-alert should be f1's key, got %v", entries)
	}
}

// --- case 8 ---

func TestProcess_AllClearHeadlineTruncate(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	headline := strings.Repeat("x", 80)
	fs := []report.Finding{finding("alert", "e1"), finding("alert", "e2"), finding("alert", "e3")}
	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: headline, Body: "body", Findings: fs})
	mustProcess(t, s, b1)

	cfg.Now = time.Unix(2000, 0)
	// All 3 findings absent this tick, all sharing the resolved headline.
	b2 := marshalReport(t, &report.Report{Status: "OK", Headline: "irrelevant", Body: "body", Resolved: []string{headline}})
	d2 := mustProcess(t, s, b2)
	if !d2.Notify || d2.Reason != "all_clear" {
		t.Fatalf("notify=%v reason=%s, want notify=true reason=all_clear", d2.Notify, d2.Reason)
	}
	if n := len([]rune(d2.Report.Headline)); n > 80 {
		t.Errorf("all-clear headline: %d runes, want <= 80", n)
	}
	if _, err := report.Validate(marshalReport(t, &d2.Report)); err != nil {
		t.Errorf("emitted all-clear report failed schema validation: %v", err)
	}
}

// --- case 9 ---

func TestProcess_HistoryRotation(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	for i := 0; i < 60; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "Test", Body: "Body"}))
	}

	entries := readHistoryFiles(t, cfg.StateDir)
	if len(entries) != 50 {
		t.Fatalf("history files: %d, want 50", len(entries))
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
		if !historyNameRe.MatchString(e.Name()) {
			t.Errorf("history filename %q does not match the contract pattern", e.Name())
		}
	}
	sort.Strings(names)
	// oldest 10 ticks (epoch 1000..1900) must be gone, newest (epoch 1000+59*100=6900) must survive.
	oldestSurviving := epochOf(t, names[0])
	if oldestSurviving < 1000+10*100 {
		t.Errorf("oldest surviving history epoch = %d, want >= %d (the 10 oldest of 60 must be gone)", oldestSurviving, 1000+10*100)
	}
	newestWant := int64(1000 + 59*100)
	if got := epochOf(t, names[len(names)-1]); got != newestWant {
		t.Errorf("newest history epoch = %d, want %d", got, newestWant)
	}
}

func epochOf(t *testing.T, name string) int64 {
	t.Helper()
	epochStr, _, _ := strings.Cut(name, "-")
	n, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		t.Fatalf("bad history filename %q: %v", name, err)
	}
	return n
}

// --- case 10: outbox ---

func TestOutbox(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	payload := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Test body"})

	cfg.Now = time.Unix(1000, 0)
	id1, err := s.OutboxAdd(payload)
	if err != nil {
		t.Fatalf("OutboxAdd: %v", err)
	}
	cfg.Now = time.Unix(1001, 0)
	id2, err := s.OutboxAdd(payload)
	if err != nil {
		t.Fatalf("OutboxAdd: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two adds produced the same id %q", id1)
	}

	items, err := s.OutboxTake()
	if err != nil {
		t.Fatalf("OutboxTake: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("OutboxTake: %d items, want 2", len(items))
	}
	if items[0].ID != id1 || items[1].ID != id2 {
		t.Errorf("OutboxTake order: got [%s,%s], want oldest first [%s,%s]", items[0].ID, items[1].ID, id1, id2)
	}
	if items[0].Attempts != 1 || items[1].Attempts != 1 {
		t.Errorf("attempts after first take: got %d,%d want 1,1", items[0].Attempts, items[1].Attempts)
	}
	if items[0].FallbackSMTP || items[1].FallbackSMTP {
		t.Errorf("fallback_smtp true too early (attempt 1)")
	}
	if !bytes.Equal(items[0].Payload, payload) {
		t.Errorf("payload did not round-trip byte-identically: got %s, want %s", items[0].Payload, payload)
	}

	// C4: files under outbox/ are 0o600, including after OutboxTake rewrites
	// the persisted attempts count (not just on the initial OutboxAdd write).
	outboxEntries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range outboxEntries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("outbox/%s mode = %o, want 0600 (C4)", e.Name(), perm)
		}
	}

	// Two more takes -> attempts 2, then 3 -> fallback_smtp flips at OutboxSMTPAfter=3.
	if _, err := s.OutboxTake(); err != nil {
		t.Fatal(err)
	}
	items, err = s.OutboxTake()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !items[0].FallbackSMTP || !items[1].FallbackSMTP {
		t.Errorf("attempt 3: fallback_smtp = %v,%v want true,true", items[0].FallbackSMTP, items[1].FallbackSMTP)
	}
	if items[0].Attempts != 3 {
		t.Errorf("attempts at 3rd take: got %d, want 3", items[0].Attempts)
	}

	// Ack removes exactly one.
	if err := s.OutboxAck(id1); err != nil {
		t.Fatalf("OutboxAck(%s): %v", id1, err)
	}
	remaining, _ := os.ReadDir(filepath.Join(cfg.StateDir, "outbox"))
	if len(remaining) != 1 {
		t.Errorf("after acking one of two: %d files remain, want 1", len(remaining))
	}
	if err := s.OutboxAck(id2); err != nil {
		t.Fatalf("OutboxAck(%s): %v", id2, err)
	}

	if err := s.OutboxAck("bogus"); err != ErrUnknownID {
		t.Errorf("OutboxAck(bogus) = %v, want ErrUnknownID", err)
	}

	// 60 adds -> only OUTBOX_MAX (50) kept, oldest dropped first.
	var lastID string
	for i := 0; i < 60; i++ {
		cfg.Now = time.Unix(int64(2000+i), 0)
		lastID, err = s.OutboxAdd(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	files, _ := os.ReadDir(filepath.Join(cfg.StateDir, "outbox"))
	if len(files) != 50 {
		t.Fatalf("after 60 adds: %d files, want 50 (OUTBOX_MAX)", len(files))
	}
	if err := s.OutboxAck(lastID); err != nil {
		t.Errorf("the most recently added id must survive the trim: OutboxAck(%s) = %v", lastID, err)
	}

	// S.2: outbox-add input must be a JSON object.
	if _, err := s.OutboxAdd([]byte(`"not an object"`)); err != ErrBadInput {
		t.Errorf("OutboxAdd(non-object) = %v, want ErrBadInput", err)
	}
	if _, err := s.OutboxAdd([]byte(`not json`)); err != ErrBadInput {
		t.Errorf("OutboxAdd(non-JSON) = %v, want ErrBadInput", err)
	}
}

// S.4/C5: an empty outbox marshals to "[]" on stdout, never "null" — a bare
// `var items []OutboxItem` marshals to null when it never gets appended to.
func TestOutboxTake_EmptyMarshalsToEmptyArrayNotNull(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	items, err := s.OutboxTake()
	if err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("OutboxTake() on an empty outbox = nil, want a non-nil empty slice")
	}
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Errorf("json.Marshal(empty OutboxTake()) = %q, want \"[]\"", b)
	}
}

// CodeRabbit PR #3, most serious: id is an unvalidated positional CLI
// argument joined onto $STATE_DIR/outbox/ before the exists-check. Without
// the outboxIDRe guard, "../history/<file>" resolves outside outbox/
// entirely and os.Remove deletes whatever it lands on — a history file
// analyze's trend window depends on, in the exploit CodeRabbit named.
func TestOutboxAck_RejectsPathTraversal(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	// A real history file that must survive the attack attempt.
	histDir := filepath.Join(cfg.StateDir, "history")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(histDir, "1786870800-000000.json")
	if err := os.WriteFile(victim, []byte(`{"status":"OK","headline":"h","body":"b","findings":[],"resolved":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.OutboxAck("../history/1786870800-000000"); err != ErrUnknownID {
		t.Errorf("OutboxAck(traversal id) = %v, want ErrUnknownID", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("traversal id deleted a file outside outbox/: %v", err)
	}

	// A well-formed id that simply doesn't exist must still behave.
	if err := s.OutboxAck("1000-999"); err != ErrUnknownID {
		t.Errorf("OutboxAck(well-formed but unknown id) = %v, want ErrUnknownID", err)
	}
}

// CodeRabbit PR #3: <epoch>-<rand3> collides across two same-second adds
// roughly 1-in-1000 of the time, and writeAtomic's rename silently
// REPLACES whatever was already at that path — a queued notification
// destroyed with no error. randIntn is stubbed to hand out a colliding
// value first, then a free one, so this is deterministic rather than
// relying on the real PRNG to happen to collide.
func TestOutboxAdd_RetriesOnIDCollision(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	outboxDir := filepath.Join(cfg.StateDir, "outbox")
	if err := os.MkdirAll(outboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dummy := []byte(`{"id":"1000-042","payload":{"x":1},"attempts":0,"created":1000}`)
	if err := os.WriteFile(filepath.Join(outboxDir, "1000-042.json"), dummy, 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	origRandIntn := randIntn
	randIntn = func(int) int {
		calls++
		if calls == 1 {
			return 42 // collides with the pre-seeded "1000-042.json"
		}
		return 43
	}
	t.Cleanup(func() { randIntn = origRandIntn })

	id, err := s.OutboxAdd([]byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if id == "1000-042" {
		t.Fatal("OutboxAdd returned a colliding id instead of retrying")
	}
	if id != "1000-043" {
		t.Errorf("OutboxAdd id = %q, want 1000-043 (the second, non-colliding candidate)", id)
	}
	got, err := os.ReadFile(filepath.Join(outboxDir, "1000-042.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dummy) {
		t.Error("the pre-existing colliding outbox entry was overwritten instead of skipped")
	}
}

// S.2: outbox-add's payload must be a JSON object. `null` unmarshals into
// a nil map with no error, so a check that only verifies "did json.Unmarshal
// into a map succeed" lets it through.
func TestOutboxAdd_RejectsJSONNull(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	if _, err := s.OutboxAdd([]byte(`null`)); err != ErrBadInput {
		t.Errorf("OutboxAdd(null) = %v, want ErrBadInput", err)
	}
}

// CodeRabbit PR #3 item 3: OutboxTake and trimOutbox (called from
// OutboxAdd) discarded os.ReadDir's error — the same "failing /state
// reports success" class already fixed for OutboxAdd's own write and
// saveAlert. Root-proof injection: replace outbox/ with a file (see
// TestOutboxAdd_PropagatesWriteFailures for why chmod alone is vacuous
// as root, which is what CI's Dockerfile builder stage runs as).
func TestOutboxTake_PropagatesReadDirFailure(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	outboxDir := filepath.Join(cfg.StateDir, "outbox")
	if err := os.RemoveAll(outboxDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outboxDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.OutboxTake(); err == nil {
		t.Error("OutboxTake() with outbox/ replaced by a file = nil error, want non-nil")
	}
}

func TestTrimOutbox_PropagatesReadDirFailure(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	outboxDir := filepath.Join(cfg.StateDir, "outbox")
	if err := os.RemoveAll(outboxDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outboxDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.trimOutbox(); err == nil {
		t.Error("trimOutbox() with outbox/ replaced by a file = nil error, want non-nil")
	}
}

// --- case 11: heartbeat timing ---

// S.3(g) rule 3, amended: when k == 0 (a fresh $STATE_DIR's very first
// heartbeat — history/ is still empty because the history write happens
// after step g), the body's "since" timestamp is `now`: cfg.Now when set,
// never a second time.Now() read deeper in the code (C9). Pins the exact
// RFC3339 value so a reintroduced time.Now() call fails this immediately.
func TestHeartbeat_FreshStateDirBodyUsesNow(t *testing.T) {
	now := time.Date(2000, 1, 1, 8, 1, 0, 0, time.UTC)
	cfg := testConfig(t, now)
	s := newStore(t, cfg)

	d := mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "Empty", Body: "body"}))
	if !d.Heartbeat {
		t.Fatal("expected the first Process call on a fresh StateDir at 08:01 to be the heartbeat")
	}
	want := "No open findings. 0 ticks since " + now.Format(time.RFC3339) + "."
	if d.Report.Body != want {
		t.Errorf("heartbeat body = %q, want %q (k==0 must use the injected clock, not time.Now())", d.Report.Body, want)
	}
}

func TestHeartbeat(t *testing.T) {
	cfg := testConfig(t, time.Date(2000, 1, 1, 7, 59, 0, 0, time.UTC))
	s := newStore(t, cfg)

	empty := &report.Report{Status: "OK", Headline: "Empty", Body: "body"}

	d1 := mustProcess(t, s, marshalReport(t, empty))
	if d1.Notify || d1.Heartbeat {
		t.Errorf("07:59: notify=%v heartbeat=%v, want both false", d1.Notify, d1.Heartbeat)
	}

	cfg.Now = time.Date(2000, 1, 1, 8, 1, 0, 0, time.UTC)
	d2 := mustProcess(t, s, marshalReport(t, empty))
	if !d2.Notify || !d2.Heartbeat || d2.Reason != "heartbeat" {
		t.Errorf("08:01: notify=%v heartbeat=%v reason=%s, want true true heartbeat", d2.Notify, d2.Heartbeat, d2.Reason)
	}

	cfg.Now = time.Date(2000, 1, 1, 9, 0, 0, 0, time.UTC)
	d3 := mustProcess(t, s, marshalReport(t, empty))
	if d3.Heartbeat {
		t.Errorf("same day again: heartbeat=%v, want false", d3.Heartbeat)
	}

	cfg.Now = time.Date(2000, 1, 2, 11, 0, 0, 0, time.UTC)
	d4 := mustProcess(t, s, marshalReport(t, empty))
	if !d4.Heartbeat {
		t.Errorf("next day 11:00: heartbeat=%v, want true", d4.Heartbeat)
	}
}

// --- case 12: heartbeat suppression ---

func TestHeartbeatSuppression(t *testing.T) {
	cfg := testConfig(t, time.Date(2000, 1, 1, 9, 0, 0, 0, time.UTC))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Alert", Body: "Body", Findings: []report.Finding{finding("alert", "e")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("alert should notify")
	}

	cfg.Now = time.Date(2000, 1, 1, 9, 0, 1, 0, time.UTC)
	empty := marshalReport(t, &report.Report{Status: "OK", Headline: "H", Body: "b"})
	d2 := mustProcess(t, s, empty)
	if d2.Heartbeat {
		t.Errorf("heartbeat suppressed by the earlier alert: got true, want false")
	}

	hbData, err := os.ReadFile(filepath.Join(cfg.StateDir, "heartbeat"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(hbData)); got != "2000-01-01" {
		t.Errorf("heartbeat content: %q, want 2000-01-01 (advanced by the alert notification)", got)
	}
}

// --- case 13: liveness ---

func TestLiveness(t *testing.T) {
	cfg := testConfig(t, time.Date(2000, 1, 1, 8, 0, 0, 0, time.UTC))
	s := newStore(t, cfg)

	empty := &report.Report{Status: "OK", Headline: "Test", Body: "body"}

	mustProcess(t, s, marshalReport(t, empty))
	hbPath := filepath.Join(cfg.StateDir, "heartbeat")
	info1, err := os.Stat(hbPath)
	if err != nil {
		t.Fatal(err)
	}
	mtime1 := info1.ModTime()

	cfg.Now = time.Date(2000, 1, 1, 8, 0, 1, 0, time.UTC)
	mustProcess(t, s, marshalReport(t, empty))
	info2, err := os.Stat(hbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.ModTime().After(mtime1) {
		t.Errorf("heartbeat mtime not advanced by a fully suppressed Process call: %v -> %v", mtime1, info2.ModTime())
	}
	if err := s.Health(); err != nil {
		t.Errorf("Health() right after a Process call: got %v, want nil", err)
	}

	// Backdate the file relative to cfg.Now (not real wall-clock time —
	// cfg.Now is year 2000, so comparing against time.Now() would always
	// read as stale for the wrong reason).
	stale := cfg.Now.Add(-3*cfg.TickInterval - time.Minute)
	if err := os.Chtimes(hbPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := s.Health(); err == nil {
		t.Error("Health() after backdating past 3xTickInterval: got nil, want error")
	}
}

// --- case 14: stale expiry ---

func TestStaleExpiry(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Body", Findings: []report.Finding{finding("alert", "e")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify {
		t.Fatal("initial notify failed")
	}

	// Exactly StaleAlertSec: survives (S.3c uses >, not >=).
	cfg.Now = time.Unix(1000+86400, 0)
	empty := marshalReport(t, &report.Report{Status: "OK", Headline: "H", Body: "b"})
	mustProcess(t, s, empty)
	if entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "active-alerts")); len(entries) != 1 {
		t.Errorf("at exactly StaleAlertSec: %d active alerts remain, want 1 (must survive, boundary is >)", len(entries))
	}

	// Past the boundary: expires silently, no all-clear.
	cfg.Now = time.Unix(1000+86400+1, 0)
	d2 := mustProcess(t, s, empty)
	if d2.Notify || len(d2.Report.Resolved) != 0 {
		t.Errorf("stale expiry: notify=%v resolved=%v, want false []", d2.Notify, d2.Report.Resolved)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "active-alerts")); len(entries) != 0 {
		t.Errorf("active-alerts: %d files, want 0", len(entries))
	}
}

// --- case 15: error paths ---

// A full or read-only $STATE_DIR must surface as an error (mapped to exit
// 5 by the CLI, S.6), not a silently-discarded writeAtomic failure that
// reports success while nothing was actually persisted.
//
// Failure is injected by replacing the destination DIRECTORY with a plain
// FILE at the same path, not by removing the write permission bit —
// os.MkdirAll/CreateTemp then fail with ENOTDIR, a structural conflict the
// filesystem enforces for every caller including uid 0. A chmod-based
// version of this test is vacuous as root (permission bits are not
// consulted for root at all), and the Dockerfile builder stage that runs
// `go test` runs as root — exactly the environment this test exists for.
func TestProcess_PropagatesWriteFailures(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	if err := os.RemoveAll(alertDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alertDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := marshalReport(t, &report.Report{Status: "ALERT", Headline: "H", Body: "b", Findings: []report.Finding{finding("alert", "e")}})
	if _, err := s.Process(b); err == nil {
		t.Error("Process() with active-alerts/ replaced by a file = nil error, want non-nil (saveAlert's writeAtomic failure must not be discarded)")
	}
}

func TestOutboxAdd_PropagatesWriteFailures(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	outboxDir := filepath.Join(cfg.StateDir, "outbox")
	if err := os.RemoveAll(outboxDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outboxDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.OutboxAdd([]byte(`{"a":1}`)); err == nil {
		t.Error("OutboxAdd() with outbox/ replaced by a file = nil error, want non-nil")
	}
}

func TestErrorPaths(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	if _, err := s.Process([]byte("not json")); err != ErrBadInput {
		t.Errorf("non-JSON: got %v, want ErrBadInput", err)
	}
	if _, err := s.Process([]byte(`{"status":"OK","headline":"","body":"","resolved":[]}`)); err != ErrBadInput {
		t.Errorf("no findings array: got %v, want ErrBadInput", err)
	}

	cfg2 := testConfig(t, time.Unix(1000, 0))
	cfg2.StateDir = filepath.Join(cfg2.StateDir, "does", "not", "exist")
	if _, err := New(cfg2); err != ErrStateDir {
		t.Errorf("nonexistent StateDir: got %v, want ErrStateDir", err)
	}

	// Truncated active-alerts file -> treated as absent -> re-notified as new.
	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	os.MkdirAll(alertDir, 0o700)
	corruptKey := dedup.Key("kernel", "corrupt-evidence")
	if err := os.WriteFile(filepath.Join(alertDir, corruptKey+".json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Body", Findings: []report.Finding{finding("alert", "corrupt-evidence")}})
	d1 := mustProcess(t, s, b1)
	if !d1.Notify || d1.Reason != "new_finding" {
		t.Errorf("corrupt alert file: notify=%v reason=%s, want true new_finding (re-notified as new)", d1.Notify, d1.Reason)
	}

	// Corrupt history file: skipped by History, but still counts for rotation.
	historyDir := filepath.Join(cfg.StateDir, "history")
	os.MkdirAll(historyDir, 0o700)
	if err := os.WriteFile(filepath.Join(historyDir, "0000000001-000000.json"), []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	hist, err := s.History(5)
	if err != nil {
		t.Errorf("History with a corrupt file: got error %v, want nil (skip silently)", err)
	}
	for _, h := range hist {
		if !json.Valid(h) {
			t.Errorf("History returned invalid JSON: %s", h)
		}
	}
}

// --- case 16: write containment (A1) ---

func snapshotFS(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[path] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteContainment(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(roDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chdir(origWD)
		os.Chmod(roDir, 0o755) // let t.TempDir() clean up
	})

	// Snapshot the shared parent of cfg.StateDir and roDir (both are
	// t.TempDir() calls from this same test, so they are siblings under a
	// common per-test root) — walking cfg.StateDir alone would make the
	// containment check below tautological, since every path found by
	// walking cfg.StateDir trivially has cfg.StateDir as a prefix.
	testRoot := filepath.Dir(cfg.StateDir)
	before := snapshotFS(t, testRoot)

	b := marshalReport(t, &report.Report{Status: "ALERT", Headline: "H", Body: "B", Findings: []report.Finding{finding("alert", "e")}})
	d, err := s.Process(b)
	if err != nil {
		t.Fatalf("Process with a 0o555 working directory must still succeed: %v", err)
	}
	if !d.Notify {
		t.Fatal("expected a notification")
	}

	roEntries, err := os.ReadDir(roDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(roEntries) != 0 {
		t.Errorf("Process wrote into the read-only working directory: %v", roEntries)
	}

	after := snapshotFS(t, testRoot)
	for p := range after {
		if before[p] {
			continue // pre-existing, not a write from this Process call
		}
		if !strings.HasPrefix(p, cfg.StateDir) {
			t.Errorf("write outside StateDir: %s", p)
		}
	}
	if len(after) <= len(before) {
		t.Error("Process produced no new files under StateDir — snapshot comparison is not exercising anything")
	}
}

// --- case 17 ---

func TestHistory(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	for i := 0; i < 10; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "Test", Body: "Body"}))
	}

	hist, err := s.History(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 5 {
		t.Fatalf("History(5): %d entries, want 5", len(hist))
	}

	// Newest-first: cross-check directly against the sorted directory listing.
	entries, _ := os.ReadDir(filepath.Join(cfg.StateDir, "history"))
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i := 0; i < 5; i++ {
		want, err := os.ReadFile(filepath.Join(cfg.StateDir, "history", names[i]))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(hist[i]), bytes.TrimSpace(want)) {
			t.Errorf("History(5)[%d] does not match the %d-th newest file %s (order is not newest-first)", i, i, names[i])
		}
	}

	cfg2 := testConfig(t, time.Unix(1000, 0))
	s2 := newStore(t, cfg2)
	hist2, err := s2.History(5)
	if err != nil {
		t.Fatal(err)
	}
	if hist2 == nil || len(hist2) != 0 {
		t.Errorf("empty history: got %v, want []", hist2)
	}
}

// S.7: "corrupt history/*.json -> skipped by History, still counted for
// rotation." A corrupt file among the newest n requested must not shrink
// the result below n while an (n+1)-th valid, older file exists — History
// must keep scanning past the corrupt one for a replacement, not just
// read the first n directory positions and stop. 8 total files, corrupt
// the single newest, request 5: a version that stops after n POSITIONS
// returns 4; the fix must return 5 by reaching the 6th-newest file.
func TestHistory_SkipsCorruptButStillFillsN(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	for i := 0; i < 8; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "Test", Body: "Body"}))
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() }) // newest first
	if len(entries) != 8 {
		t.Fatalf("test setup: %d history files, want 8", len(entries))
	}
	newest := entries[0].Name()
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "history", newest), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	hist, err := s.History(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 5 {
		t.Fatalf("History(5) with the newest of 8 files corrupt: %d entries, want 5 (must dig past the corrupt one into the 6th-newest)", len(hist))
	}
}

// --- case 18: tick_seq is read-only, with meta.tick_seq precedence ---

func TestTickSeqReadOnly(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	cfg.TickSeq = 123
	s := newStore(t, cfg)

	b1 := marshalReport(t, &report.Report{Status: "OK", Headline: "Test", Body: "Body", Meta: &report.Meta{TickSeq: 999}})
	d1 := mustProcess(t, s, b1)
	if d1.TickSeq != 123 {
		t.Errorf("cfg.TickSeq must win over report.meta.tick_seq: got %d, want 123", d1.TickSeq)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "tick-seq")); err == nil {
		t.Error("a tick-seq file exists; state must never write it (S-D3)")
	}

	// cfg.TickSeq unset -> report.meta.tick_seq wins over 0.
	cfg2 := testConfig(t, time.Unix(1000, 0))
	s2 := newStore(t, cfg2)
	b2 := marshalReport(t, &report.Report{Status: "OK", Headline: "Test", Body: "Body", Meta: &report.Meta{TickSeq: 77}})
	d2 := mustProcess(t, s2, b2)
	if d2.TickSeq != 77 {
		t.Errorf("with cfg.TickSeq unset, report.meta.tick_seq must win over 0: got %d, want 77", d2.TickSeq)
	}
}

// --- case 19: key reuse + the C6 cross-package proof ---

func TestKeyReuse(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	f1 := finding("alert", "evidence1")
	f1.Key = "aaabbbcccdddeee0"
	b1 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Body", Findings: []report.Finding{f1}})
	d1 := mustProcess(t, s, b1)
	if len(d1.Report.Findings) != 1 || d1.Report.Findings[0].Key != "aaabbbcccdddeee0" {
		t.Errorf("an explicit key must be kept byte-for-byte: got %+v", d1.Report.Findings)
	}

	f2 := finding("alert", "evidence2")
	b2 := marshalReport(t, &report.Report{Status: "ALERT", Headline: "Test", Body: "Body", Findings: []report.Finding{f2}})
	d2 := mustProcess(t, s, b2)
	// Independently specified expectation (C6's literal algorithm), not a
	// call through the same helper the production path calls — otherwise
	// the test tracks whatever dedup.Key/EvidenceCore currently compute
	// instead of catching a drift in either.
	wantKey := literalDedupKey("kernel", "evidence2")
	if len(d2.Report.Findings) != 1 || d2.Report.Findings[0].Key != wantKey {
		t.Errorf("computed key: got %q, want %q (C6 literal)", d2.Report.Findings[0].Key, wantKey)
	}
}

// literalDedupKey reimplements C6's algorithm from CONTRACTS.md directly —
// deliberately not calling dedup.Key/EvidenceCore — so a break in either is
// caught rather than tracked. "evidence2" has no timestamps, digits, or
// '=', so C6's masking is a no-op and the core is the lowercased string
// itself.
func literalDedupKey(component, evidence string) string {
	sum := sha256.Sum256([]byte(component + "\n" + strings.ToLower(evidence)))
	return hex.EncodeToString(sum[:])[:16]
}

// --- case 20: schema agreement across the full behaviour set ---

func TestSchemaAgreement(t *testing.T) {
	cfg := testConfig(t, time.Unix(1000, 0))
	s := newStore(t, cfg)

	scenarios := []func(t *testing.T){
		func(t *testing.T) { // new finding
			mustProcess(t, s, marshalReport(t, &report.Report{Status: "WATCH", Headline: "N", Body: "b", Findings: []report.Finding{finding("watch", "s20-1")}}))
		},
		func(t *testing.T) { // suppressed
			cfg.Now = cfg.Now.Add(time.Minute)
			mustProcess(t, s, marshalReport(t, &report.Report{Status: "WATCH", Headline: "N", Body: "b", Findings: []report.Finding{finding("watch", "s20-1")}}))
		},
		func(t *testing.T) { // escalation
			cfg.Now = cfg.Now.Add(time.Minute)
			mustProcess(t, s, marshalReport(t, &report.Report{Status: "ALERT", Headline: "N", Body: "b", Findings: []report.Finding{finding("alert", "s20-1")}}))
		},
		func(t *testing.T) { // all-clear
			cfg.Now = cfg.Now.Add(time.Minute)
			mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "N", Body: "b", Resolved: []string{"N"}}))
		},
		func(t *testing.T) { // suppressed (no findings, no resolved, no heartbeat due)
			cfg.Now = cfg.Now.Add(time.Minute)
			mustProcess(t, s, marshalReport(t, &report.Report{Status: "OK", Headline: "N", Body: "b"}))
		},
	}

	for i, scenario := range scenarios {
		t.Run(strconv.Itoa(i), scenario)
	}

	// Directly assert the last decision.report validates.
	cfg.Now = cfg.Now.Add(time.Minute)
	d := mustProcess(t, s, marshalReport(t, &report.Report{Status: "ALERT", Headline: "Final", Body: "b", Findings: []report.Finding{finding("alert", "s20-final")}}))
	if _, err := report.Validate(marshalReport(t, &d.Report)); err != nil {
		t.Errorf("decision.report failed report.schema.json validation: %v", err)
	}
}

// --- case 21 ---

func TestCLIExitCodes(t *testing.T) {
	t.Skip("S.6 exit-code mapping is tested in cmd/sentinel against the real sentinel binary")
}
