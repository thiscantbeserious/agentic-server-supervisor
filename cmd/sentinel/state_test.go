package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runBinStdin is defined in analyze_test.go (shared by every subcommand
// that reads stdin).

const validReport = `{"status":"ALERT","headline":"Test","body":"body","findings":[{"severity":"alert","component":"kernel","evidence":"e","explanation":"exp"}],"resolved":[]}`

// TestStateProcessCLI is case 21 (contracts/state.md S.6, S.9): `sentinel
// state process` reads a report.Report from stdin and writes decision.json
// to stdout with exit 0.
func TestStateProcessCLI(t *testing.T) {
	bin := buildSentinel(t)
	stateDir := t.TempDir()
	stdout, stderr, code := runBinStdin(t, bin, baseEnv(t, stateDir), validReport, "state", "process")
	if code != 0 {
		t.Fatalf("state process: exit %d, want 0, stderr=%s", code, stderr)
	}
	var decision map[string]any
	if err := json.Unmarshal([]byte(stdout), &decision); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (%s)", err, stdout)
	}
	if decision["notify"] != true {
		t.Errorf("decision.notify = %v, want true", decision["notify"])
	}
}

// S.6: process input that is not valid JSON / has no findings array -> 65.
func TestStateProcessCLIBadInputExits65(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), "not json", "state", "process")
	if code != 65 {
		t.Fatalf("state process (bad JSON): exit %d, want 65", code)
	}
}

// outbox-add prints only the id + "\n" (S.4).
func TestStateOutboxAddPrintsOnlyID(t *testing.T) {
	bin := buildSentinel(t)
	stateDir := t.TempDir()
	stdout, stderr, code := runBinStdin(t, bin, baseEnv(t, stateDir), `{"status":"ALERT"}`, "state", "outbox-add")
	if code != 0 {
		t.Fatalf("state outbox-add: exit %d, want 0, stderr=%s", code, stderr)
	}
	trimmed := strings.TrimSuffix(stdout, "\n")
	if trimmed == "" || strings.Contains(trimmed, "\n") || strings.ContainsAny(trimmed, "{}") {
		t.Fatalf("outbox-add stdout = %q, want exactly <id>\\n and nothing else", stdout)
	}

	// outbox-take must then return that one entry, oldest first.
	stdout2, stderr2, code2 := runBin(t, bin, baseEnv(t, stateDir), "state", "outbox-take")
	if code2 != 0 {
		t.Fatalf("state outbox-take: exit %d, want 0, stderr=%s", code2, stderr2)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(stdout2), &items); err != nil {
		t.Fatalf("outbox-take stdout not valid JSON array: %v", err)
	}
	if len(items) != 1 || items[0]["id"] != trimmed {
		t.Fatalf("outbox-take = %v, want one item with id %q", items, trimmed)
	}

	// outbox-ack removes it; unknown id -> exit 5.
	_, stderrAck, codeAck := runBin(t, bin, baseEnv(t, stateDir), "state", "outbox-ack", trimmed)
	if codeAck != 0 {
		t.Fatalf("state outbox-ack %s: exit %d, want 0, stderr=%s", trimmed, codeAck, stderrAck)
	}
	_, _, codeAckBogus := runBin(t, bin, baseEnv(t, stateDir), "state", "outbox-ack", "bogus-id")
	if codeAckBogus != 5 {
		t.Fatalf("state outbox-ack bogus-id: exit %d, want 5", codeAckBogus)
	}
}

// outbox-add input must be a JSON object (S.2) -> exit 65.
func TestStateOutboxAddBadInputExits65(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), "not json", "state", "outbox-add")
	if code != 65 {
		t.Fatalf("state outbox-add (bad JSON): exit %d, want 65", code)
	}
}

// history [n] defaults to 5, returns a JSON array, empty -> [].
func TestStateHistoryCLI(t *testing.T) {
	bin := buildSentinel(t)
	stateDir := t.TempDir()

	stdout, stderr, code := runBin(t, bin, baseEnv(t, stateDir), "state", "history")
	if code != 0 {
		t.Fatalf("state history (empty): exit %d, want 0, stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty history stdout = %q, want []", stdout)
	}

	if _, _, c := runBinStdin(t, bin, baseEnv(t, stateDir), validReport, "state", "process"); c != 0 {
		t.Fatalf("seeding a history entry via process failed with exit %d", c)
	}
	stdout2, stderr2, code2 := runBin(t, bin, baseEnv(t, stateDir), "state", "history", "1")
	if code2 != 0 {
		t.Fatalf("state history 1: exit %d, want 0, stderr=%s", code2, stderr2)
	}
	var hist []json.RawMessage
	if err := json.Unmarshal([]byte(stdout2), &hist); err != nil {
		t.Fatalf("history stdout not a JSON array: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("state history 1: got %d entries, want 1", len(hist))
	}
}

// S.6: unwritable/missing STATE_DIR -> exit 69.
func TestStateCLIStateDirUnwritableExits69(t *testing.T) {
	bin := buildSentinel(t)
	env := baseEnv(t, filepath.Join(t.TempDir(), "does", "not", "exist"))
	_, _, code := runBinStdin(t, bin, env, validReport, "state", "process")
	if code != 69 {
		t.Fatalf("state process with a missing STATE_DIR: exit %d, want 69", code)
	}
}

// `sentinel health`: exit 0 with a fresh heartbeat, exit 1 with a stale one
// (S.6 pins the "stale or missing" case to exit 1, C2).
func TestHealthCLI(t *testing.T) {
	bin := buildSentinel(t)
	stateDir := t.TempDir()

	// The real liveness signal (S-D4) is a Process call, which rewrites the
	// heartbeat file's mtime every time.
	if _, _, c := runBinStdin(t, bin, baseEnv(t, stateDir), validReport, "state", "process"); c != 0 {
		t.Fatalf("seeding heartbeat via process failed")
	}
	_, stderr, code := runBin(t, bin, baseEnv(t, stateDir), "health")
	if code != 0 {
		t.Fatalf("health right after a Process call: exit %d, want 0, stderr=%s", code, stderr)
	}

	// Backdate the heartbeat file far enough that even a generous default
	// TICK_INTERVAL (300s -> 900s threshold) reads as stale.
	hbPath := filepath.Join(stateDir, "heartbeat")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(hbPath, past, past); err != nil {
		t.Fatal(err)
	}
	_, _, code2 := runBin(t, bin, baseEnv(t, stateDir), "health")
	if code2 != 1 {
		t.Fatalf("health with a stale heartbeat: exit %d, want 1", code2)
	}
}
