// history.go: the report window. Loads recent reports from state, projects
// them into the compact form the prompts carry, and computes which previous
// findings are resolved this tick.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// historyFinding is the compact projection of a past finding carried in the
// prompt. Evidence, occurrences and first_seen are load-bearing: the dedup
// key deliberately masks digits, so "cksum_errors=1" and "cksum_errors=7"
// share a key, the key alone proves recurrence but can never prove
// growth. The model is asked to compare counters across ticks; without the
// evidence text that comparison is impossible and it answers from
// imagination.
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

// loadHistoryReports returns the newest n reports from the state history,
// oldest first. Filenames sort chronologically as strings by construction.
// Only *.json is read: atomic writes leave .tmp-* files in the same
// directory, and letting one into the window would evict a real report.
// Unreadable or unparseable files are skipped; a missing directory yields
// nil.
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

// historyProjectionLines renders each history report as one compact JSON
// line for the prompt.
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

// newestEligible walks hist (oldest first) from the end backward and
// returns the newest entry that is not a degraded fallback tick. A
// degraded entry never looked at the world, so it carries no information
// about whether a finding open before it is still open, "compare against
// it anyway" is indistinguishable from "assume nothing changed" for every
// finding the outage didn't touch, exactly the #39 defect. The second
// return value is true when hist was non-empty but every entry in it was
// degraded, the walk-back's residual limit (contracts/analyze.md §6 step
// 7): the caller logs that case, it is the one where the orphaning this
// fix targets can still happen.
func newestEligible(hist []report.Report) (*report.Report, bool) {
	for i := len(hist) - 1; i >= 0; i-- {
		if !isDegraded(hist[i]) {
			return &hist[i], false
		}
	}
	return nil, len(hist) > 0
}

func isDegraded(r report.Report) bool {
	return r.Meta != nil && r.Meta.Degraded
}

// computeResolved returns which of the previous eligible report's findings
// are gone this tick, as their 16-hex dedup.Key (contracts/analyze.md §6
// step 7, CONTRACTS.md C5/C6). Computed in Go, overwriting whatever the
// model emitted: set arithmetic over data we already hold does not belong
// in a probabilistic component. newest is chosen by newestEligible, the
// newest entry that is not a degraded fallback tick, never unconditionally
// the newest entry, a degraded entry was skipped over on the way here.
//
// Keys, not evidence: evidence used to be truncated to 80 runes because
// findings have no headline of their own, which made two alerts agreeing
// in their first 80 runes indistinguishable and forced `state` to match on
// headline-or-evidence to compensate. The key is exact, already computed
// in this step, and needs no truncation, dedup.Key's output is always
// 16 hex chars, well under the schema's 80-rune resolved[] bound.
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
		// info never resolves, it just is: the quiet-tick finding's key
		// changes with every model rephrasing, and letting those keys into
		// the diff pollutes the 20-entry cap and makes state announce that
		// normality was "resolved".
		if f.Key == "" || f.Severity == "info" || currentKeys[f.Key] {
			continue
		}
		out = append(out, f.Key)
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

// truncRunes keeps at most n leading runes.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
