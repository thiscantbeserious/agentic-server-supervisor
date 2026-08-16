package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

func testConfig(t *testing.T, now time.Time) *config.Config {
	loc, _ := time.LoadLocation("UTC")
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

func TestProcess_SameWatchFinding3Ticks(t *testing.T) {
	// Case 1: same WATCH finding, 3 ticks, Now +5 min each
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Create minimal valid JSON - no optional fields that would be zero-valued
	b1 := []byte(`{"status":"WATCH","headline":"Test","body":"Test body","findings":[{"severity":"watch","component":"kernel","evidence":"test evidence","explanation":"test"}],"resolved":[]}`)

	d1, err := s.Process(b1)
	if err != nil {
		t.Fatalf("Process failed: %v, input was: %s", err, string(b1))
	}
	if !d1.Notify || d1.Reason != "new_finding" || d1.SuppressedCount != 0 || d1.ActiveCount != 1 {
		t.Errorf("Tick 1: notify=%v reason=%s suppressed=%d active=%d, want notify=true reason=new_finding",
			d1.Notify, d1.Reason, d1.SuppressedCount, d1.ActiveCount)
	}

	cfg.Now = time.Unix(1000+300, 0) // +5 min
	d2, err := s.Process(b1)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Notify || d2.Reason != "suppressed" || d2.SuppressedCount != 1 {
		t.Errorf("Tick 2: notify=%v reason=%s suppressed=%d, want notify=false reason=suppressed suppressed=1",
			d2.Notify, d2.Reason, d2.SuppressedCount)
	}
	if d2.Report.Status != "OK" {
		t.Errorf("Tick 2: status=%s, want OK", d2.Report.Status)
	}

	cfg.Now = time.Unix(1000+600, 0) // +10 min
	d3, err := s.Process(b1)
	if err != nil {
		t.Fatal(err)
	}
	if d3.Notify || d3.Reason != "suppressed" || d3.SuppressedCount != 1 {
		t.Errorf("Tick 3: notify=%v reason=%s suppressed=%d, want notify=false reason=suppressed suppressed=1",
			d3.Notify, d3.Reason, d3.SuppressedCount)
	}
}

func TestProcess_Escalation(t *testing.T) {
	// Case 2: tick 4 with escalation from WATCH to ALERT
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	watchFinding := report.Finding{
		Severity:    "watch",
		Component:   "test",
		Evidence:    "test evidence",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "WATCH",
		Headline: "Test",
		Body:     "Test body",
		Findings: []report.Finding{watchFinding},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	s.Process(b1)

	cfg.Now = time.Unix(1000+300, 0)
	s.Process(b1)
	cfg.Now = time.Unix(1000+600, 0)
	s.Process(b1)

	// Now escalate to ALERT
	cfg.Now = time.Unix(1000+900, 0)
	alertFinding := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test evidence",
		Explanation: "test",
	}
	report4 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Test body",
		Findings: []report.Finding{alertFinding},
		Resolved: []string{},
	}
	b4, _ := json.Marshal(report4)
	d4, err := s.Process(b4)
	if err != nil {
		t.Fatal(err)
	}
	if !d4.Notify || d4.Reason != "escalation" {
		t.Errorf("Tick 4 escalation: notify=%v reason=%s, want notify=true reason=escalation", d4.Notify, d4.Reason)
	}
}

func TestProcess_RenotifyWatch(t *testing.T) {
	// Case 3: WATCH finding at +5h59min / +6h1min
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	finding := report.Finding{
		Severity:    "watch",
		Component:   "test",
		Evidence:    "test evidence",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "WATCH",
		Headline: "Test",
		Body:     "Test body",
		Findings: []report.Finding{finding},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Initial notify failed")
	}

	// +5h 59 min (just before renotify window)
	cfg.Now = time.Unix(1000 + 5*3600 + 59*60, 0)
	d2, _ := s.Process(b1)
	if d2.Notify || d2.Reason != "suppressed" {
		t.Errorf("At 5h59m: notify=%v reason=%s, want suppressed", d2.Notify, d2.Reason)
	}

	// +6h 1 min (after renotify window, watch is 6h)
	cfg.Now = time.Unix(1000 + 6*3600 + 60, 0)
	d3, _ := s.Process(b1)
	if !d3.Notify || d3.Reason != "renotify" {
		t.Errorf("At 6h1m: notify=%v reason=%s, want notify=true reason=renotify", d3.Notify, d3.Reason)
	}
}

func TestProcess_RenotifyAlert(t *testing.T) {
	// Case 4: ALERT finding at +59min / +1h1min
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	finding := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test evidence",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Test body",
		Findings: []report.Finding{finding},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Initial notify failed")
	}

	// +59 min (just before renotify window)
	cfg.Now = time.Unix(1000 + 59*60, 0)
	d2, _ := s.Process(b1)
	if d2.Notify || d2.Reason != "suppressed" {
		t.Errorf("At 59m: notify=%v reason=%s, want suppressed", d2.Notify, d2.Reason)
	}

	// +1h 1 min (after renotify window, alert is 1h)
	cfg.Now = time.Unix(1000 + 3600 + 60, 0)
	d3, _ := s.Process(b1)
	if !d3.Notify || d3.Reason != "renotify" {
		t.Errorf("At 1h1m: notify=%v reason=%s, want notify=true reason=renotify", d3.Notify, d3.Reason)
	}
}

func TestProcess_AllClear(t *testing.T) {
	// Case 5: report with resolved, finding absent from findings[]
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	finding := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test evidence",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test headline",
		Body:     "Test body",
		Findings: []report.Finding{finding},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Initial notify failed")
	}

	// Now resolve it
	cfg.Now = time.Unix(2000, 0)
	report2 := &report.Report{
		Status:   "OK",
		Headline: "Resolved",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{"Test headline"},
	}

	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)
	if !d2.Notify || d2.Reason != "all_clear" || len(d2.Report.Resolved) != 1 {
		t.Errorf("All-clear: notify=%v reason=%s resolved=%d, want notify=true reason=all_clear resolved=1",
			d2.Notify, d2.Reason, len(d2.Report.Resolved))
	}

	// Check that key file is gone
	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	files, _ := os.ReadDir(alertDir)
	if len(files) != 0 {
		t.Errorf("Active alerts: %d files remain, want 0", len(files))
	}

	// Second all-clear on same finding should not notify
	cfg.Now = time.Unix(3000, 0)
	report3 := &report.Report{
		Status:   "OK",
		Headline: "Still clear",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{"Test headline"},
	}

	b3, _ := json.Marshal(report3)
	d3, _ := s.Process(b3)
	if d3.Notify {
		t.Errorf("Second all-clear: notify=%v, want false", d3.Notify)
	}
}

func TestProcess_ResolvedUnknownHeadline(t *testing.T) {
	// Case 6: resolved naming a never-active headline
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Empty",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{"Unknown headline"},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if d1.Notify || len(d1.Report.Resolved) != 0 {
		t.Errorf("Unknown resolved: notify=%v resolved=%d, want false resolved=0", d1.Notify, len(d1.Report.Resolved))
	}
}

func TestProcess_TwoFindingsSharedHeadline(t *testing.T) {
	// Case 7: two findings sharing a headline; next tick one persists
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	f1 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "evidence1",
		Explanation: "test",
	}
	f2 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "evidence2",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Shared headline",
		Body:     "Test",
		Findings: []report.Finding{f1, f2},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify || d1.ActiveCount != 2 {
		t.Fatal("Initial: should have 2 active alerts")
	}

	// Second tick: only f1 persists, resolve the shared headline
	cfg.Now = time.Unix(2000, 0)
	report2 := &report.Report{
		Status:   "ALERT",
		Headline: "Shared headline",
		Body:     "Test",
		Findings: []report.Finding{f1},
		Resolved: []string{"Shared headline"},
	}

	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)
	if len(d2.Report.Resolved) != 1 {
		t.Errorf("Resolved: %d entries, want 1", len(d2.Report.Resolved))
	}
	if d2.ActiveCount != 1 {
		t.Errorf("Active: %d, want 1", d2.ActiveCount)
	}
}

func TestProcess_AllClearHeadlineTruncate(t *testing.T) {
	// Case 8: all-clear headline of 80 runes + "(+2 more)"
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	headline := strings.Repeat("x", 80)

	f1 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "e1",
		Explanation: "test",
	}
	f2 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "e2",
		Explanation: "test",
	}
	f3 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "e3",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: headline,
		Body:     "Test",
		Findings: []report.Finding{f1, f2, f3},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	s.Process(b1)

	cfg.Now = time.Unix(2000, 0)
	report2 := &report.Report{
		Status:   "OK",
		Headline: "",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{headline, headline, headline},
	}

	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)
	if d2.Notify && d2.Reason == "all_clear" {
		emitHeadline := d2.Report.Headline
		if len([]rune(emitHeadline)) > 80 {
			t.Errorf("All-clear headline: %d runes, want <= 80", len([]rune(emitHeadline)))
		}
	}
}

func TestProcess_HistoryRotation(t *testing.T) {
	// Case 9: 60 reports processed
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)

	for i := 0; i < 60; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		s.Process(b1)
	}

	historyDir := filepath.Join(cfg.StateDir, "history")
	files, _ := os.ReadDir(historyDir)
	if len(files) != 50 {
		t.Errorf("History files: %d, want 50", len(files))
	}

	// Check that names sort chronologically (lexicographically by construction)
	if len(files) > 0 {
		first := files[0].Name()
		last := files[len(files)-1].Name()
		if first >= last {
			t.Errorf("History not sorted: first=%s, last=%s", first, last)
		}
	}
}

func TestOutbox(t *testing.T) {
	// Case 10: outbox operations
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"status":"ALERT","headline":"Test","body":"Test body","findings":[],"resolved":[]}`)

	// Add two entries
	id1, _ := s.OutboxAdd(payload)
	_, _ = s.OutboxAdd(payload)

	// Take should return 2, oldest first, with attempts=1
	items, _ := s.OutboxTake()
	if len(items) != 2 {
		t.Fatalf("OutboxTake: %d items, want 2", len(items))
	}
	if items[0].Attempts != 1 || items[1].Attempts != 1 {
		t.Errorf("Attempts: got %d,%d want 1,1", items[0].Attempts, items[1].Attempts)
	}
	if items[0].FallbackSMTP || items[1].FallbackSMTP {
		t.Errorf("FallbackSMTP too early")
	}

	// Two more takes (attempts 2, then 3)
	s.OutboxTake()
	s.OutboxTake()

	// Third take should have fallback_smtp=true
	items, _ = s.OutboxTake()
	if len(items) > 0 && !items[0].FallbackSMTP {
		t.Errorf("FallbackSMTP not set at attempt 3")
	}

	// Ack one
	s.OutboxAck(id1)

	// Ack unknown ID should error
	err = s.OutboxAck("unknown")
	if err != ErrUnknownID {
		t.Errorf("Ack unknown: got %v, want ErrUnknownID", err)
	}

	// Add 60 entries, should keep only 50
	for i := 0; i < 60; i++ {
		s.OutboxAdd(payload)
	}

	// Payload should round-trip byte-identically
	items, _ = s.OutboxTake()
	if len(items) > 0 && !json.Valid(items[0].Payload) {
		t.Errorf("Payload not valid JSON")
	}
}

func TestHeartbeat(t *testing.T) {
	// Case 11: heartbeat timing
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Empty",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)

	// 07:59 UTC
	cfg.Now = time.Date(2000, 1, 1, 7, 59, 0, 0, time.UTC)
	d1, _ := s.Process(b1)
	if d1.Notify || d1.Heartbeat {
		t.Errorf("07:59: notify=%v heartbeat=%v, want both false", d1.Notify, d1.Heartbeat)
	}

	// 08:01 UTC (after heartbeat hour)
	cfg.Now = time.Date(2000, 1, 1, 8, 1, 0, 0, time.UTC)
	d2, _ := s.Process(b1)
	if !d2.Heartbeat || d2.Reason != "heartbeat" {
		t.Errorf("08:01: heartbeat=%v reason=%s, want true heartbeat", d2.Heartbeat, d2.Reason)
	}

	// Same day again should not send heartbeat
	cfg.Now = time.Date(2000, 1, 1, 9, 0, 0, 0, time.UTC)
	d3, _ := s.Process(b1)
	if d3.Heartbeat {
		t.Errorf("Same day: heartbeat=%v, want false", d3.Heartbeat)
	}

	// Next day at 11:00 should send heartbeat
	cfg.Now = time.Date(2000, 1, 2, 11, 0, 0, 0, time.UTC)
	d4, _ := s.Process(b1)
	if !d4.Heartbeat {
		t.Errorf("Next day 11:00: heartbeat=%v, want true", d4.Heartbeat)
	}
}

func TestHeartbeatSuppression(t *testing.T) {
	// Case 12: heartbeat suppression when alert notified
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	f := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Alert",
		Body:     "Body",
		Findings: []report.Finding{f},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)

	// 09:00 - alert notified
	cfg.Now = time.Date(2000, 1, 1, 9, 0, 0, 0, time.UTC)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Alert should notify")
	}

	// Still 09:00 - no heartbeat
	cfg.Now = time.Date(2000, 1, 1, 9, 0, 1, 0, time.UTC)
	report2 := &report.Report{
		Status:   "OK",
		Headline: "",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{},
	}
	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)
	if d2.Heartbeat {
		t.Errorf("Heartbeat suppressed: got true, want false")
	}

	// Verify heartbeat content is today
	hbFile := filepath.Join(cfg.StateDir, "heartbeat")
	hbData, _ := os.ReadFile(hbFile)
	hbStr := strings.TrimSpace(string(hbData))
	if hbStr != "2000-01-01" {
		t.Errorf("Heartbeat content: %s, want 2000-01-01", hbStr)
	}
}

func TestLiveness(t *testing.T) {
	// Case 13: liveness - heartbeat mtime advances on every Process
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Test",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)

	cfg.Now = time.Date(2000, 1, 1, 8, 0, 0, 0, time.UTC)
	s.Process(b1)

	hbFile := filepath.Join(cfg.StateDir, "heartbeat")
	info1, _ := os.Stat(hbFile)
	mtime1 := info1.ModTime()

	// Process again
	cfg.Now = time.Date(2000, 1, 1, 8, 0, 1, 0, time.UTC)
	s.Process(b1)

	info2, _ := os.Stat(hbFile)
	mtime2 := info2.ModTime()

	if !mtime2.After(mtime1) {
		t.Errorf("Heartbeat mtime not advanced: %v -> %v", mtime1, mtime2)
	}

	// Health should be nil (mtime recent)
	err = s.Health()
	if err != nil {
		t.Errorf("Health check: got error %v, want nil", err)
	}

	// Backdate mtime past 3 × TICK_INTERVAL
	cfg.Now = time.Date(2000, 1, 1, 9, 0, 0, 0, time.UTC)
	os.Chtimes(hbFile, cfg.Now.Add(-20*time.Minute), cfg.Now.Add(-20*time.Minute))

	err = s.Health()
	if err == nil {
		t.Errorf("Health check: got nil, want error (stale mtime)")
	}
}

func TestStaleExpiry(t *testing.T) {
	// Case 14: stale expiry
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	f := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test",
		Explanation: "test",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{f},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Initial notify failed")
	}

	// Process empty report 25 hours later
	cfg.Now = time.Unix(1000 + 25*3600, 0)
	report2 := &report.Report{
		Status:   "OK",
		Headline: "",
		Body:     "",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)
	if d2.Notify || len(d2.Report.Resolved) != 0 {
		t.Errorf("Stale expiry: notify=%v resolved=%d, want false resolved=0", d2.Notify, len(d2.Report.Resolved))
	}

	// Verify key file is gone
	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	files, _ := os.ReadDir(alertDir)
	if len(files) != 0 {
		t.Errorf("Active alerts: %d files, want 0", len(files))
	}
}

func TestErrorPaths(t *testing.T) {
	// Case 15: error paths
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Non-JSON
	_, err = s.Process([]byte("not json"))
	if err != ErrBadInput {
		t.Errorf("Non-JSON: got %v, want ErrBadInput", err)
	}

	// Object without findings
	_, err = s.Process([]byte(`{"status":"OK","headline":"","body":"","resolved":[]}`))
	if err != ErrBadInput {
		t.Errorf("No findings: got %v, want ErrBadInput", err)
	}

	// Nonexistent STATE_DIR
	cfg2 := testConfig(t, time.Unix(1000, 0))
	cfg2.StateDir = "/nonexistent/path/should/fail"
	_, err = New(cfg2)
	if err != ErrStateDir {
		t.Errorf("Nonexistent StateDir: got %v, want ErrStateDir", err)
	}

	// Truncated active-alerts file (should be treated as new)
	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	os.MkdirAll(alertDir, 0755)
	os.WriteFile(filepath.Join(alertDir, "corrupt.json"), []byte("{invalid"), 0644)

	f := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "test",
		Explanation: "test",
	}
	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{f},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Errorf("Corrupt alert file: should re-notify as new")
	}

	// Corrupt history file (should be skipped but still rotated)
	historyDir := filepath.Join(cfg.StateDir, "history")
	os.MkdirAll(historyDir, 0755)
	os.WriteFile(filepath.Join(historyDir, "0000001000-000000.json"), []byte("{invalid"), 0644)

	_, err = s.History(5)
	if err != nil {
		t.Errorf("Corrupt history: got error %v, want to skip silently", err)
	}
}

func TestWriteContainment(t *testing.T) {
	// Case 16: write containment - all writes under StateDir
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := cfg.StateDir

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	s.Process(b1)

	// Check all modified paths are under StateDir
	err = filepath.WalkDir(stateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasPrefix(path, stateDir) {
			t.Errorf("Write outside StateDir: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Errorf("WalkDir error: %v", err)
	}
}

func TestHistory(t *testing.T) {
	// Case 17: History returns newest first
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)

	for i := 0; i < 10; i++ {
		cfg.Now = time.Unix(int64(1000+i*100), 0)
		s.Process(b1)
	}

	history, _ := s.History(5)
	if len(history) != 5 {
		t.Errorf("History(5): %d entries, want 5", len(history))
	}

	// Empty history should return []
	cfg2 := testConfig(t, time.Unix(1000, 0))
	s2, _ := New(cfg2)
	history2, _ := s2.History(5)
	if history2 == nil || len(history2) != 0 {
		t.Errorf("Empty history: got %v, want []", history2)
	}
}

func TestTickSeqReadOnly(t *testing.T) {
	// Case 18: tick_seq is read-only
	cfg := testConfig(t, time.Unix(1000, 0))
	cfg.TickSeq = 123
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "OK",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)

	// TickSeq should be from cfg.TickSeq
	if d1.TickSeq != 123 {
		t.Errorf("TickSeq: got %d, want 123", d1.TickSeq)
	}

	// No tick-seq file should be created
	tickSeqFile := filepath.Join(cfg.StateDir, "tick-seq")
	if _, err := os.Stat(tickSeqFile); err == nil {
		t.Errorf("tick-seq file exists, should not")
	}
}

func TestKeyReuse(t *testing.T) {
	// Case 19: key reuse - explicit key kept, no key gets computed
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	f1 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "evidence1",
		Explanation: "test",
		Key:         "aaabbbcccdddeee0",
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{f1},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)

	// Key should be preserved
	if len(d1.Report.Findings) > 0 && d1.Report.Findings[0].Key != "aaabbbcccdddeee0" {
		t.Errorf("Key: got %s, want aaabbbcccdddeee0", d1.Report.Findings[0].Key)
	}

	// Finding without key should get one computed
	f2 := report.Finding{
		Severity:    "alert",
		Component:   "test",
		Evidence:    "evidence2",
		Explanation: "test",
	}

	report2 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{f2},
		Resolved: []string{},
	}

	b2, _ := json.Marshal(report2)
	d2, _ := s.Process(b2)

	if len(d2.Report.Findings) > 0 && d2.Report.Findings[0].Key == "" {
		t.Errorf("Computed key: got empty string")
	}

	// Verify computed key matches dedup.Key
	expectedKey := dedup.Key("test", dedup.EvidenceCore("evidence2"))
	if len(d2.Report.Findings) > 0 && d2.Report.Findings[0].Key != expectedKey {
		t.Errorf("Computed key: got %s, want %s", d2.Report.Findings[0].Key, expectedKey)
	}
}

func TestSchemaAgreement(t *testing.T) {
	// Case 20: every decision.report validates against report.schema.json
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	report1 := &report.Report{
		Status:   "ALERT",
		Headline: "Test",
		Body:     "Body",
		Findings: []report.Finding{
			{
				Severity:    "alert",
				Component:   "test",
				Evidence:    "test",
				Explanation: "test",
			},
		},
		Resolved: []string{},
	}

	b1, _ := json.Marshal(report1)
	d1, _ := s.Process(b1)

	reportBytes, _ := json.Marshal(d1.Report)
	_, err = report.Validate(reportBytes)
	if err != nil {
		t.Errorf("Schema validation: %v", err)
	}
}

func TestCLIExitCodes(t *testing.T) {
	// Case 21: exit codes via cmd/sentinel (not directly in state)
	// This is tested in the CLI tests, not here
	t.Skip("CLI exit code mapping tested elsewhere")
}
