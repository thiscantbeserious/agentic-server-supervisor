package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RED output: minimal test set covering contract S.9 essentials
// Full test table will be completed in next iteration

func TestMinimal_NotifyLogic(t *testing.T) {
	// Verify: first finding notifies, subsequent findings respect window
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First find: WATCH - should notify
	b1 := []byte(`{"status":"WATCH","headline":"Load high","body":"5 min avg","findings":[{"severity":"watch","component":"resources","evidence":"load: 10.5","explanation":"high load"}],"resolved":[]}`)
	d1, err := s.Process(b1)
	if err != nil {
		t.Fatalf("Process tick 1: %v", err)
	}
	if !d1.Notify || d1.Reason != "new_finding" || d1.ActiveCount != 1 {
		t.Errorf("Tick 1: notify=%v reason=%s active=%d, want new_finding with 1 active", d1.Notify, d1.Reason, d1.ActiveCount)
	}

	// Same find at +5 min: still in WATCH window (6h) - suppress
	cfg.Now = time.Unix(1000+300, 0)
	d2, err := s.Process(b1)
	if err != nil {
		t.Fatalf("Process tick 2: %v", err)
	}
	if d2.Notify || d2.Reason != "suppressed" || d2.SuppressedCount != 1 {
		t.Errorf("Tick 2: notify=%v reason=%s suppressed=%d, want suppressed", d2.Notify, d2.Reason, d2.SuppressedCount)
	}

	// Escalate to ALERT at +10 min: notify escalation
	cfg.Now = time.Unix(1000+600, 0)
	b2 := []byte(`{"status":"ALERT","headline":"Load high","body":"5 min avg","findings":[{"severity":"alert","component":"resources","evidence":"load: 10.5","explanation":"high load"}],"resolved":[]}`)
	d3, err := s.Process(b2)
	if err != nil {
		t.Fatalf("Process tick 3: %v", err)
	}
	if !d3.Notify || d3.Reason != "escalation" {
		t.Errorf("Tick 3 escalation: notify=%v reason=%s, want escalation", d3.Notify, d3.Reason)
	}
}

func TestMinimal_AllClear(t *testing.T) {
	// Verify: resolve() generates exactly one all-clear
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Create and notify an alert
	b1 := []byte(`{"status":"ALERT","headline":"Checksum errors rising","body":"Mirror member failed","findings":[{"severity":"alert","component":"zfs","evidence":"eid=143 class=checksum","explanation":"errors accumulating"}],"resolved":[]}`)
	d1, _ := s.Process(b1)
	if !d1.Notify {
		t.Fatal("Initial alert should notify")
	}

	// Resolve it
	cfg.Now = time.Unix(2000, 0)
	b2 := []byte(`{"status":"OK","headline":"","body":"","findings":[],"resolved":["Checksum errors rising"]}`)
	d2, _ := s.Process(b2)
	if !d2.Notify || d2.Reason != "all_clear" || len(d2.Report.Resolved) != 1 {
		t.Errorf("All-clear: notify=%v reason=%s resolved=%d, want all_clear with 1 entry", d2.Notify, d2.Reason, len(d2.Report.Resolved))
	}

	// Verify key file deleted
	alertDir := filepath.Join(cfg.StateDir, "active-alerts")
	files, _ := os.ReadDir(alertDir)
	if len(files) != 0 {
		t.Errorf("Active alerts: %d files remain, want 0", len(files))
	}

	// Second resolve on same headline should not notify
	cfg.Now = time.Unix(3000, 0)
	b3 := []byte(`{"status":"OK","headline":"","body":"","findings":[],"resolved":["Checksum errors rising"]}`)
	d3, _ := s.Process(b3)
	if d3.Notify {
		t.Errorf("Second resolve: notify=%v, want false", d3.Notify)
	}
}

func TestMinimal_HistoryAnnotation(t *testing.T) {
	// Verify: history file contains annotated findings with key/first_seen/occurrences
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Process a finding
	b1 := []byte(`{"status":"ALERT","headline":"Test","body":"body","findings":[{"severity":"alert","component":"kernel","evidence":"SENTINEL-TEST","explanation":"test"}],"resolved":[]}`)
	_, _ = s.Process(b1)

	// Read history file from disk and verify annotation
	histDir := filepath.Join(cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)
	if len(files) == 0 {
		t.Fatal("No history file written")
	}

	histData, _ := os.ReadFile(filepath.Join(histDir, files[0].Name()))
	var histReport struct {
		Findings []struct {
			Key         string `json:"key"`
			FirstSeen   int64  `json:"first_seen"`
			Occurrences int    `json:"occurrences"`
		} `json:"findings"`
	}
	json.Unmarshal(histData, &histReport)

	if len(histReport.Findings) == 0 {
		t.Fatal("No findings in history")
	}
	if histReport.Findings[0].Key == "" {
		t.Error("Finding.key is empty in history (should be annotated)")
	}
	if histReport.Findings[0].FirstSeen == 0 {
		t.Error("Finding.first_seen is 0 in history (should be annotated)")
	}
	if histReport.Findings[0].Occurrences != 1 {
		t.Errorf("Finding.occurrences = %d, want 1", histReport.Findings[0].Occurrences)
	}

	// Verify no stray temp files
	tempFiles := 0
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".tmp-") {
			tempFiles++
		}
	}
	if tempFiles > 0 {
		t.Errorf("Stray temp files in history/: %d", tempFiles)
	}
}

func TestMinimal_Outbox(t *testing.T) {
	// Verify: outbox operations + SMTP fallback after 3 attempts
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	payload := []byte(`{"status":"ALERT","headline":"Test","body":"body","findings":[],"resolved":[]}`)

	// Add entry
	id1, _ := s.OutboxAdd(payload)

	// Take (attempts becomes 1)
	items, _ := s.OutboxTake()
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Errorf("Take 1: got %d items with attempts=%d, want 1 item attempts=1", len(items), items[0].Attempts)
	}
	if items[0].FallbackSMTP {
		t.Error("FallbackSMTP too early (before attempt 3)")
	}

	// Take again (attempts becomes 2)
	items, _ = s.OutboxTake()

	// Take again (attempts becomes 3, should enable SMTP fallback)
	items, _ = s.OutboxTake()
	if len(items) > 0 && !items[0].FallbackSMTP {
		t.Error("FallbackSMTP not set at attempt 3")
	}

	// Ack the entry
	err = s.OutboxAck(id1)
	if err != nil {
		t.Errorf("Ack: %v", err)
	}

	// Ack unknown should error
	err = s.OutboxAck("unknown-id")
	if err != ErrUnknownID {
		t.Errorf("Ack unknown: got %v, want ErrUnknownID", err)
	}

	// Verify payload round-trips as valid JSON
	s.OutboxAdd(payload)
	items, _ = s.OutboxTake()
	if len(items) > 0 && !json.Valid(items[0].Payload) {
		t.Error("Payload not valid JSON after round-trip")
	}
}

func TestMinimal_Health(t *testing.T) {
	// Verify: Health returns error when heartbeat mtime is stale
	cfg := testConfig(t, time.Unix(1000, 0))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b := []byte(`{"status":"OK","headline":"","body":"","findings":[],"resolved":[]}`)
	s.Process(b)

	// Fresh heartbeat should pass
	err = s.Health()
	if err != nil {
		t.Errorf("Health (fresh): got error %v, want nil", err)
	}

	// Backdate heartbeat past 3 × TICK_INTERVAL
	hbPath := filepath.Join(cfg.StateDir, "heartbeat")
	os.Chtimes(hbPath, cfg.Now.Add(-20*time.Minute), cfg.Now.Add(-20*time.Minute))

	err = s.Health()
	if err == nil {
		t.Errorf("Health (stale): got nil, want error")
	}
}
