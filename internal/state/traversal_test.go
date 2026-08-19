package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
)

// ---- containment regression suite: identifier-from-contents class ----
//
// This is the ONLY traversal/containment test file in this package —
// deliberately. Two classes of bug produced three real sinks across T5
// review (findings[].key adopted verbatim in step (d); alert.Key and
// outbox EntryID trusted from a stored record's own JSON body instead of
// the filename/lookup-key it was actually read under). All three are
// exercised here except the outbox one, which lives in
// TestOutboxTake_SkipsBodyIDFilenameMismatch (state_test.go) — porting it
// here too would be a second copy of the same containment property with
// nothing left to catch that the first copy wouldn't, which is exactly
// the kind of duplicate a future change could let rot unnoticed while its
// twin still looks green.
//
// Every test here is rooted several levels ABOVE $STATE_DIR (see
// nestedStore), not at it. A snapshot rooted at the state dir cannot
// observe an escape by construction — that tautology is what let this
// class through review more than once.
//
// Validated 15/15 FAIL on a build without the path-escape guard, PASS
// once it is present.

var hexKeyRe = regexp.MustCompile(`^[0-9a-f]{16}\.json$`)

// snap walks root and records path -> "size:mtime:mode" for every entry.
func snap(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		m[rel] = fmt.Sprintf("%d:%d:%v", fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return m
}

// diffOutside returns every path that appeared or changed under root but
// NOT under the state dir (both relative to root).
func diffOutside(before, after map[string]string, stateRel string) []string {
	var out []string
	for p, v := range after {
		if p == stateRel || strings.HasPrefix(p, stateRel+string(os.PathSeparator)) {
			continue
		}
		if before[p] != v {
			out = append(out, p)
		}
	}
	for p := range before {
		if p == stateRel || strings.HasPrefix(p, stateRel+string(os.PathSeparator)) {
			continue
		}
		if _, ok := after[p]; !ok {
			out = append(out, "DELETED "+p)
		}
	}
	sort.Strings(out)
	return out
}

// nestedStore builds a store whose StateDir sits 4 levels below root, so
// even ../../../.. traversals land inside the observed root.
func nestedStore(t *testing.T, now time.Time) (s *Store, root, stateRel string) {
	t.Helper()
	root = t.TempDir()
	stateRel = filepath.Join("a", "b", "c", "state")
	stateDir := filepath.Join(root, stateRel)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, now)
	cfg.StateDir = stateDir
	return newStore(t, cfg), root, stateRel
}

func reportWithKey(key, sev, comp, evidence string) []byte {
	f := map[string]any{
		"severity": sev, "component": comp, "evidence": evidence,
		"explanation":    "e",
		"analysis":       "a",
		"recommendation": "r",
	}
	if key != "" {
		f["key"] = key
	}
	b, _ := json.Marshal(map[string]any{
		"status": "WATCH", "headline": "probe headline", "body": "probe body",
		"findings": []any{f}, "resolved": []string{},
	})
	return b
}

// Class 1: findings[].key arriving in the report document (step (d)).
func TestTraversal_ReportKeyKeepsWritesInsideStateDir(t *testing.T) {
	cases := []struct {
		name, key string
	}{
		{"parent_escape", "../../canary"},
		{"single_parent", "../canary1"},
		{"into_history", "../history/x"},
		{"deep_escape", "../../../../deepcanary"},
		{"deeper_escape", "../../../../../../../../tmp/verydeepcanary"},
		{"url_encoded", "..%2fenc"},
		{"absolute", "/etc/absolutecanary"},
		{"subdir", "sub/dircanary"},
		{"dotdot_only", ".."},
		{"empty_component", "..//canary2"},
		{"backslash", `..\..\wincanary`},
		{"nul_ish", "aaaaaaaaaaaaaaaa/../../../nulcanary"},
		{"uppercase_hex", "ABCDEF0123456789"}, // not ^[0-9a-f]{16}$
		{"short_hex", "0123456789abcde"},
		{"long_hex", "0123456789abcdef0"},
	}

	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, root, stateRel := nestedStore(t, now)
			before := snap(t, root)

			// Do NOT abort on err. On a vulnerable build the write
			// happens first and the error is raised afterwards by output
			// validation; aborting here on the error would hide an
			// escape that already happened before the error fired.
			d, err := s.Process(reportWithKey(tc.key, "watch", "zfs", "probe evidence line for "+tc.name))
			after := snap(t, root)

			if esc := diffOutside(before, after, stateRel); len(esc) > 0 {
				t.Errorf("WRITE ESCAPED $STATE_DIR for key %q: %v", tc.key, esc)
			}

			// Nothing but conforming key files may exist in active-alerts,
			// and no subdirectories either.
			ents, _ := os.ReadDir(filepath.Join(root, stateRel, "active-alerts"))
			for _, e := range ents {
				if e.IsDir() {
					t.Errorf("key %q created a subdirectory %q in active-alerts", tc.key, e.Name())
					continue
				}
				if !hexKeyRe.MatchString(e.Name()) {
					t.Errorf("key %q produced non-conforming alert file %q", tc.key, e.Name())
				}
			}

			// The path the UNGUARDED code would have written. For keys
			// that resolve outside the observed root the snapshot cannot
			// see it, so assert directly that it does not exist.
			naive := filepath.Join(root, stateRel, "active-alerts", tc.key+".json")
			if !strings.HasPrefix(naive, filepath.Join(root, stateRel)+string(os.PathSeparator)) {
				if _, err := os.Stat(naive); err == nil {
					t.Errorf("WRITE ESCAPED (out of observed root) for key %q: %s exists", tc.key, naive)
					os.Remove(naive)
				}
			}

			// $STATE_DIR's own root may hold only the four contracted
			// names (S.5) — "../canary1" stays inside but still pollutes it.
			rootEnts, _ := os.ReadDir(filepath.Join(root, stateRel))
			allowed := map[string]bool{"heartbeat": true, "history": true, "active-alerts": true, "outbox": true}
			for _, e := range rootEnts {
				if !allowed[e.Name()] {
					t.Errorf("key %q dropped %q into $STATE_DIR root (S.5 lists only heartbeat/history/active-alerts/outbox)", tc.key, e.Name())
				}
			}

			// history/ must hold only the rotation file.
			hents, _ := os.ReadDir(filepath.Join(root, stateRel, "history"))
			histRe := regexp.MustCompile(`^[0-9]{10}-[0-9]{6}\.json$`)
			for _, e := range hents {
				if !histRe.MatchString(e.Name()) {
					t.Errorf("key %q polluted history/ with %q", tc.key, e.Name())
				}
			}

			// The finding must still be PROCESSED, under a recomputed
			// key — not silently dropped.
			if err != nil {
				t.Fatalf("key %q: Process errored (%v) instead of recomputing the key", tc.key, err)
			}
			if d == nil {
				t.Fatal("nil decision")
			}
			if !d.Notify || d.Reason != "new_finding" {
				t.Errorf("key %q: finding was dropped instead of recomputed: notify=%v reason=%q",
					tc.key, d.Notify, d.Reason)
			}
			if len(d.Report.Findings) != 1 {
				t.Fatalf("key %q: expected 1 emitted finding, got %d", tc.key, len(d.Report.Findings))
			}
			got := d.Report.Findings[0].Key
			want := dedup.Key("zfs", "probe evidence line for "+tc.name)
			if got != want {
				t.Errorf("key %q: emitted key %q, want recomputed %q", tc.key, got, want)
			}
		})
	}
}

// The guard must not just reject everything: a genuinely valid 16-hex key
// must be honoured byte-for-byte (S.9 case 19), under the same deep-root
// environment the escape rows run in.
func TestTraversal_LegitKeyHonouredUnderDeepRoot(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	s, root, stateRel := nestedStore(t, now)
	const k = "9f2c41ab77de0315"

	d, err := s.Process(reportWithKey(k, "alert", "zfs", "legit evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Report.Findings[0].Key != k {
		t.Errorf("legit key mangled: got %q want %q", d.Report.Findings[0].Key, k)
	}
	if _, err := os.Stat(filepath.Join(root, stateRel, "active-alerts", k+".json")); err != nil {
		t.Errorf("legit key did not produce active-alerts/%s.json: %v", k, err)
	}
}

// Class 2, sink A: step (e) deletes by the key read out of a STORED
// record's JSON body, not by the directory entry name. A crafted record
// therefore steers an unlink outside $STATE_DIR.
func TestTraversal_ResolvedDeleteUsesFilenameNotStoredKey(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	s, root, stateRel := nestedStore(t, now)
	stateDir := filepath.Join(root, stateRel)

	// active-alerts/../../victim.json resolves to <stateDir>/../victim.json
	// == root/a/b/c/victim.json — one level ABOVE the state dir.
	victim := filepath.Join(root, "a", "b", "c", "victim.json")
	if err := os.WriteFile(victim, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	alertDir := filepath.Join(stateDir, "active-alerts")
	os.MkdirAll(alertDir, 0755)
	rec, _ := json.Marshal(ActiveAlert{
		Key: "../../victim", Component: "zfs", Headline: "Doomed headline",
		Severity: "watch", FirstSeen: now.Unix() - 10, LastSeen: now.Unix() - 10,
		LastNotified: now.Unix() - 10, NotifyCount: 1, Occurrences: 1,
	})
	const recordFile = "aaaaaaaaaaaaaaaa.json"
	if err := os.WriteFile(filepath.Join(alertDir, recordFile), rec, 0644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{
		"status": "OK", "headline": "h", "body": "b",
		"findings": []any{}, "resolved": []string{"Doomed headline"},
	})
	d, err := s.Process(raw)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(victim); os.IsNotExist(err) {
		t.Errorf("UNLINK ESCAPED $STATE_DIR: %s was deleted via a stored record's key field", victim)
	}

	// The load-boundary fix (alerts.go) treats a body/filename key
	// mismatch as corrupt (S.7) rather than fabricating an all-clear from
	// contents it just declared untrustworthy — so reason must never be
	// "all_clear" here. (notify may still be true for an unrelated reason,
	// e.g. the daily heartbeat firing on this same Process call — that's
	// independent of this record and not what's under test.)
	if d.Reason == "all_clear" {
		t.Errorf("reason=%s, want anything but all_clear: a key/filename mismatch is corrupt (S.7), not a legitimate all-clear", d.Reason)
	}
	if _, err := os.Stat(filepath.Join(alertDir, recordFile)); !os.IsNotExist(err) {
		t.Errorf("the corrupt record (by filename %s) must be cleaned up, got err=%v", recordFile, err)
	}
}

// Class 2, sink B: step (d)'s "exists" branch loads an active alert via
// loadAlert(key) and, without the fix, would trust
// alert.Key from the record's own JSON body for the saveAlert rewrite
// immediately after — a record at a legitimate filename whose body
// claims "key":"../../pwned" would be rewritten straight back out to
// that escaped path on its very next occurrence, with no crafted input
// required.
//
// Reachability note (mutation-verified, not a gap in this test): step
// (c)'s expireStaleAlerts runs before step (d) on every Process call and
// already deletes this same corrupt record via its own loadAlertByFile
// check, so loadAlert's independent key check is currently unreachable
// under test — mutating it alone leaves this test green. See the comment
// on expireStaleAlerts (state.go) for the same fact from the other side.
// This test still proves what matters: the record never gets rewritten
// under its stored key, regardless of which of the two checks catches it.
func TestTraversal_ExistingAlertRewriteUsesValidatedKeyNotStoredKey(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	s, root, stateRel := nestedStore(t, now)
	stateDir := filepath.Join(root, stateRel)
	alertDir := filepath.Join(stateDir, "active-alerts")
	if err := os.MkdirAll(alertDir, 0755); err != nil {
		t.Fatal(err)
	}

	const filenameKey = "1111111111111111"
	rec, _ := json.Marshal(ActiveAlert{
		Key: "../../pwned", Component: "zfs", Headline: "Test",
		Severity: "watch", FirstSeen: now.Unix() - 10, LastSeen: now.Unix() - 10,
		Occurrences: 1,
	})
	if err := os.WriteFile(filepath.Join(alertDir, filenameKey+".json"), rec, 0644); err != nil {
		t.Fatal(err)
	}

	before := snap(t, root)

	// An input finding carrying the record's filename as its (valid,
	// well-formed) key — this is what puts Process on the "exists"
	// branch that reads the crafted record and, pre-fix, would have
	// rewritten it straight back out under its body's escaped key.
	d, err := s.Process(reportWithKey(filenameKey, "alert", "zfs", "mismatch-evidence"))
	after := snap(t, root)

	// alert.Key = "../../pwned" joined onto ".../state/active-alerts/"
	// resolves to ".../state/../pwned.json" == root/a/b/c/pwned.json —
	// still inside the observed root (four levels deep), so diffOutside
	// alone is sufficient proof here; no direct os.Stat needed.
	if esc := diffOutside(before, after, stateRel); len(esc) > 0 {
		t.Errorf("WRITE ESCAPED $STATE_DIR via saveAlert's stored-key rewrite: %v", esc)
	}

	if err != nil {
		t.Fatalf("Process: unexpected error: %v", err)
	}
	if len(d.Report.Findings) != 1 || d.Report.Findings[0].Key != filenameKey {
		t.Fatalf("finding must still be processed under the supplied (valid) key %q: %+v", filenameKey, d.Report.Findings)
	}
	if _, ok := s.loadAlert(filenameKey); !ok {
		t.Errorf("a correctly-keyed active-alerts record was not saved under %s", filenameKey)
	}
}
