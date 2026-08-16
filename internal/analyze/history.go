package analyze

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// truncRunes truncates s to at most n runes, keeping the prefix. Used for
// the HISTORY evidence projection (160 runes) and the resolved-evidence
// rendering (120 runes) — unlike the fallback's newest-preserving
// truncation (fallback.go's truncLinesKeepNewest), these are plain prefix
// truncations of a single already-short string.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// historyFinding and historyProjection are the compact per-report
// projection injected into HISTORY (§6 step 2): {status, headline,
// findings:[{severity,component,key,evidence,occurrences,first_seen}],
// resolved}.
//
// evidence/occurrences/first_seen are the load-bearing fix from the design
// review (Fable + agy, independently): dedup.EvidenceCore deliberately
// masks digits (C6), so "cksum_errors=1" and "cksum_errors=7" share the
// same key — the key alone proves recurrence and can NEVER prove growth.
// sentinel.md's Trend section asks the model to compare counters between
// HISTORY and FACTS; without the evidence text that comparison is
// impossible and the model answers from imagination.
type historyFinding struct {
	Severity    string `json:"severity"`
	Component   string `json:"component"`
	Key         string `json:"key"`
	Evidence    string `json:"evidence"`
	Occurrences int    `json:"occurrences,omitempty"`
	FirstSeen   int64  `json:"first_seen,omitempty"`
}

type historyProjection struct {
	Status   string           `json:"status"`
	Headline string           `json:"headline"`
	Findings []historyFinding `json:"findings"`
	Resolved []string         `json:"resolved"`
}

// loadHistoryReports returns the newest n parsed history documents from
// ${stateDir}/history/*.json, oldest first. Newest is determined by
// sort.Strings of the filenames (the <unix-seconds,10>-<tick_seq,6>.json
// naming written by state sorts chronologically as strings). The "*.json"
// filter matters: state writes atomically via ".tmp-*" files in the same
// directory (C4), and letting one into the window evicts a real report.
// Unreadable or unparseable files are skipped silently (§6 step 2); a
// missing dir yields nil.
func loadHistoryReports(stateDir string, n int) []report.Report {
	dir := filepath.Join(stateDir, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if n > 0 && len(names) > n {
		names = names[len(names)-n:]
	}

	out := make([]report.Report, 0, len(names))
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// historyProjectionLines renders each parsed history report as one
// compact JSON line for the HISTORY prompt section (§6 step 2), oldest
// first — hist is expected already in that order (loadHistoryReports's).
func historyProjectionLines(hist []report.Report) []string {
	lines := make([]string, 0, len(hist))
	for _, r := range hist {
		proj := historyProjection{
			Status: r.Status, Headline: r.Headline,
			Findings: []historyFinding{}, Resolved: r.Resolved,
		}
		for _, f := range r.Findings {
			proj.Findings = append(proj.Findings, historyFinding{
				Severity: f.Severity, Component: f.Component, Key: f.Key,
				Evidence:    truncRunes(f.Evidence, 160),
				Occurrences: f.Occurrences, FirstSeen: f.FirstSeen,
			})
		}
		if proj.Resolved == nil {
			proj.Resolved = []string{}
		}
		b, err := json.Marshal(proj)
		if err != nil {
			continue
		}
		lines = append(lines, string(b))
	}
	return lines
}

// newestHistory returns the last (= most recent) report in an
// oldest-first history slice, or nil if there is none.
func newestHistory(hist []report.Report) *report.Report {
	if len(hist) == 0 {
		return nil
	}
	return &hist[len(hist)-1]
}

// computeResolved is §6 step 7's Go-computed `resolved`: the set
// difference historyKeys \ currentKeys, using ONLY the newest history
// document, rendered as the past finding's evidence truncated to 120
// runes, sorted for determinism, capped at the schema's 20 items. This
// overwrites whatever the model emitted in its own "resolved" field —
// set arithmetic over data analyze already holds does not belong in a
// probabilistic component.
func computeResolved(newest *report.Report, current []report.Finding) []string {
	if newest == nil {
		return []string{}
	}
	currentKeys := make(map[string]bool, len(current))
	for _, f := range current {
		if f.Key != "" {
			currentKeys[f.Key] = true
		}
	}
	var out []string
	for _, f := range newest.Findings {
		if f.Key == "" || currentKeys[f.Key] {
			continue
		}
		// report.schema.json's resolved[] maxLength is 80, not 120 (main's
		// own error, corrected live-gate round 6): 120 makes
		// report.Validate reject the WHOLE report the first time a
		// resolved finding carries a typical kernel/ZED line. Truncating
		// to 80 can also produce "" for already-degenerate evidence, and
		// minLength:1 forbids that — skip empty results rather than emit
		// an invalid entry.
		ev := truncRunes(f.Evidence, 80)
		if ev == "" {
			continue
		}
		out = append(out, ev)
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	if out == nil {
		out = []string{}
	}
	return out
}
