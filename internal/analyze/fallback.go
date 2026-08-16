// fallback.go: the LLM-free report. Built whenever no valid model report
// could be produced, it carries the raw high-priority kernel lines so an
// analyzer outage can never hide a hardware event.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// noKernelLinesPlaceholder is used verbatim when the fallback has no
// protected kernel line to show.
const noKernelLinesPlaceholder = "no emerg or crit kernel lines in this tick"

// reasonPhrase maps machine failure codes to the human phrases the report
// carries. The code goes to the log; only the phrase enters report text,
// because the notifier strips underscores from report strings and
// "agy_missing" would reach the operator as "agymissing".
var reasonPhrase = map[string]string{
	"agy_missing":    "analyzer binary not found",
	"agy_failed":     "analyzer exited non-zero",
	"agy_timeout":    "analyzer timed out",
	"invalid_json":   "analyzer output was not valid JSON",
	"schema_invalid": "analyzer output failed schema validation",
	"internal_error": "analyzer internal failure",
	"agy_empty":      "analyzer returned no answer",
	"agy_unauth":     "analyzer not authenticated",
}

// Fallback builds the report used when no valid model report exists: an
// ALERT carrying the raw high-priority kernel lines of this tick, so the
// operator still sees hardware events during an analyzer outage. code is
// the machine-readable failure reason; the report text carries only the
// mapped human phrase, because the notifier strips underscores from report
// strings and "agy_missing" would arrive as "agymissing". The result always
// passes report.Validate.
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

// protectedKernelLines returns this tick's high-priority kernel lines,
// oldest first, capped — keeping the newest when the cap binds. A forward
// walk that stops at the limit would keep the oldest lines and drop the
// incident happening right now.
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

// truncLinesKeepNewest fits lines into a rune budget by dropping whole
// lines from the oldest end, never splitting a line, so the newest line
// survives whenever the budget binds. If even the single newest line does
// not fit, its trailing runes are kept.
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

// truncRunesSuffix keeps at most n trailing runes.
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
