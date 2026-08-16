package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// PROBE D — the cross-component obligation: history on disk must carry
// occurrences/first_seen for EVERY finding (notified AND suppressed),
// filenames exactly <unix,10>-<tick_seq,6>.json, no stray temp files.
func TestProbeD_HistoryAnnotation(t *testing.T) {
	cfg := probeCfg(t)
	cfg.TickSeq = 289
	s, _ := New(cfg)
	cfg.Now = time.Unix(1755248461, 0)

	rep := report.Report{Status: "ALERT", Headline: "ZFS checksum errors on hotstore",
		Body: "Checksum errors.", Resolved: []string{},
		Findings: []report.Finding{
			{Severity: "alert", Component: "zfs", Evidence: "cksum_errors=7", Explanation: "a"},
			{Severity: "watch", Component: "smart", Evidence: "reallocated=5", Explanation: "b"},
		}}
	b, _ := json.Marshal(rep)
	if _, err := s.Process(b); err != nil { t.Fatal(err) }
	// Second tick: first finding suppressed, still must carry annotations.
	cfg.Now = time.Unix(1755248761, 0)
	if _, err := s.Process(b); err != nil { t.Fatal(err) }

	histDir := filepath.Join(cfg.StateDir, "history")
	ents, _ := os.ReadDir(histDir)
	nameRe := regexp.MustCompile(`^[0-9]{10}-[0-9]{6}\.json$`)
	for _, e := range ents {
		if !nameRe.MatchString(e.Name()) {
			t.Errorf("history filename %q does not match ^<unix,10>-<tick_seq,6>.json$ "+
				"(analyze sorts on this name and filters *.json)", e.Name())
		}
		fi, _ := e.Info()
		t.Logf("history entry %s mode=%v", e.Name(), fi.Mode().Perm())
	}

	// newest file must carry annotations on BOTH findings
	newest := ents[len(ents)-1].Name()
	data, _ := os.ReadFile(filepath.Join(histDir, newest))
	var got report.Report
	if err := json.Unmarshal(data, &got); err != nil { t.Fatal(err) }
	for i, f := range got.Findings {
		t.Logf("history findings[%d]: key=%q first_seen=%d occurrences=%d", i, f.Key, f.FirstSeen, f.Occurrences)
		if f.Key == "" || f.FirstSeen == 0 || f.Occurrences == 0 {
			t.Errorf("history findings[%d] missing annotation (key=%q first_seen=%d occurrences=%d); "+
				"analyze's trend projection drops zero values via omitempty", i, f.Key, f.FirstSeen, f.Occurrences)
		}
	}
	if len(got.Findings) != 2 {
		t.Errorf("history holds %d findings, want 2 (suppressed findings must survive: computeResolved diffs them)", len(got.Findings))
	}
}

// PROBE E — C4 modes: dirs 0700, files 0600 under outbox/, 0644 elsewhere.
func TestProbeE_Modes(t *testing.T) {
	cfg := probeCfg(t)
	s, _ := New(cfg)
	cfg.Now = time.Unix(1000, 0)
	b, _ := json.Marshal(report.Report{Status: "OK", Headline: "h", Body: "b",
		Findings: []report.Finding{}, Resolved: []string{}})
	s.Process(b)
	s.OutboxAdd([]byte(`{"status":"OK","headline":"h","body":"b","findings":[],"resolved":[]}`))

	filepath.WalkDir(cfg.StateDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == cfg.StateDir { return nil }
		fi, _ := d.Info()
		perm := fi.Mode().Perm()
		rel, _ := filepath.Rel(cfg.StateDir, p)
		want := os.FileMode(0o644)
		if d.IsDir() { want = 0o700 } else if filepath.Dir(rel) == "outbox" { want = 0o600 }
		if perm != want {
			t.Errorf("C4 mode: %s is %v, want %v", rel, perm, want)
		}
		return nil
	})
}
