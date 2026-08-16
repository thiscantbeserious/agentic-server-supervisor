package analyze

import (
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// noKernelLinesPlaceholder is used verbatim (§5) when the fallback has no
// protected kernel line to show.
const noKernelLinesPlaceholder = "no emerg or crit kernel lines in this tick"

// reasonPhrase maps the machine-readable <CODE> (what slog records on
// stderr, C7) to the human <REASON> phrase the report document carries
// (§5, amended by commit 0651b69): notify strips "_" from every
// report-derived string (C8), so "reason: agy_missing" would reach a human
// as "reason: agymissing". The code never reaches report text; only the
// phrase does. D10 keeps no exception for the fallback.
var reasonPhrase = map[string]string{
	"agy_missing":    "analyzer binary not found",
	"agy_failed":     "analyzer exited non-zero",
	"agy_timeout":    "analyzer timed out",
	"invalid_json":   "analyzer output was not valid JSON",
	"schema_invalid": "analyzer output failed schema validation",
	"internal_error": "analyzer internal failure",
	"agy_empty":      "analyzer returned no answer",
}

// Fallback builds the exact §5 fallback document: the analyzer stage
// could not produce a valid report, so the deterministic path surfaces the
// raw high-priority kernel lines instead of losing them silently. Always
// returns a document that passes report.Validate. code is the machine
// <CODE> (already logged to stderr by the caller); the document itself
// only ever carries the mapped human phrase.
func Fallback(cfg *config.Config, seq int64, code string, f *facts.Facts) *report.Report {
	reason := reasonPhrase[code]
	if reason == "" {
		reason = code // defensive: an unmapped code still surfaces something rather than an empty phrase
	}
	lines := protectedKernelLines(cfg, f)
	raw := strings.Join(lines, "\n")
	if raw == "" {
		raw = noKernelLinesPlaceholder
		lines = nil
	}

	raw900 := truncLinesKeepNewest(lines, 900, raw)
	raw1500 := truncLinesKeepNewest(lines, 1500, raw)
	key := dedup.Key("meta", raw)

	return &report.Report{
		Status:   "ALERT",
		Headline: "Analyzer unavailable",
		Body: "The LLM analyzer could not produce a valid report (reason: " + reason +
			"). Raw kernel emerg and crit lines from this tick are listed below, unprocessed. " +
			"Hardware alerts do not depend on the analyzer - the deterministic paths (smartd, ZED, raw-alert) are unaffected.\n\n" +
			raw1500,
		Findings: []report.Finding{
			{
				Severity:    "alert",
				Component:   "meta",
				Evidence:    raw900,
				Explanation: "Analyzer unavailable (" + reason + "). Raw high-priority kernel lines are reported unfiltered so no hardware event is lost.",
				Key:         key,
			},
		},
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: cfg.Hostname, TickSeq: seq},
	}
}

// protectedKernelLines returns the protected (priority <= RawAlertMaxPriority)
// kernel lines, oldest first, capped at RawAlertMaxLines — but the cap keeps
// the NEWEST lines, not the oldest: entries are stored oldest-first, so a
// forward walk that breaks at the limit would keep the 20 oldest crit lines
// and drop the incident happening right now, the same inversion the T3
// journal record cap had (now contract-sanctioned in §5, commit 4dc1b00).
// We walk the entries backwards (newest first) collecting up to
// RawAlertMaxLines, then reverse the result back to chronological order for
// the human reading the report.
func protectedKernelLines(cfg *config.Config, f *facts.Facts) []string {
	switch {
	case f == nil || f.Kernel == nil:
		return nil
	case f.Kernel.Err != "":
		return []string{"kernel section unavailable: " + f.Kernel.Err}
	}

	entries := f.Kernel.Data.Entries
	var lines []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Priority > cfg.RawAlertMaxPriority {
			continue
		}
		lines = append(lines, e.Identifier+": "+e.Message)
		if len(lines) == cfg.RawAlertMaxLines {
			break
		}
	}
	reverseStrings(lines)
	return lines
}

// truncLinesKeepNewest fits lines (chronological, oldest first) into at
// most max runes by dropping whole lines from the OLDEST end first — never
// splitting a line mid-rune-budget, so the newest protected line is always
// present when the rune budget binds before the line-count cap does
// (test 16). If even the single newest line does not fit, its trailing
// (newest) runes are kept instead of its leading ones.
func truncLinesKeepNewest(lines []string, max int, fallback string) string {
	if len(lines) == 0 {
		return truncRunesSuffix(fallback, max)
	}

	var kept []string
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		n := len([]rune(l))
		add := n
		if len(kept) > 0 {
			add++ // "\n" separator once joined
		}
		if total+add > max {
			break
		}
		kept = append(kept, l)
		total += add
	}
	if len(kept) == 0 {
		return truncRunesSuffix(lines[len(lines)-1], max)
	}
	reverseStrings(kept)
	return strings.Join(kept, "\n")
}

func truncRunesSuffix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
