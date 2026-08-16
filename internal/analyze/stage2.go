package analyze

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// stage2SchemaJSON is the stage-2 RPC payload schema (§6 step 10): this
// document is never emitted to a user, so D3's "one schema, normative for
// everything the system emits" does not apply to it — it exists only so
// the model cannot copy/fabricate the full report shape (key, status,
// headline, ...) the way the old full-report stage-2 schema let it.
//
//go:embed stage2.schema.json
var stage2SchemaJSON []byte

// stage2Response is the stage-2 RPC payload (§6 step 10): analysis and
// recommendation for the one candidate finding, identified by the candidate
// analyze itself sent — never by a key the model echoes back — plus an
// optional headline that, when present, replaces stage 1's (§6 step 11).
type stage2Response struct {
	Analysis       string `json:"analysis"`
	Recommendation string `json:"recommendation"`
	Headline       string `json:"headline,omitempty"`
}

// validateStage2 is the hand-written bounds check for stage2.schema.json
// (same D3 pattern as report.Validate: the schema file is what's handed to
// agy --json-schema, Go enforces it at runtime). No DisallowUnknownFields,
// consistent with report.Validate's own convention.
func validateStage2(raw []byte) (*stage2Response, error) {
	var r stage2Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("stage2: invalid JSON: %w", err)
	}
	if n := len([]rune(r.Analysis)); n < 1 || n > 1200 {
		return nil, fmt.Errorf("stage2: analysis: length %d runes out of bounds [1,1200]", n)
	}
	if n := len([]rune(r.Recommendation)); n < 1 || n > 800 {
		return nil, fmt.Errorf("stage2: recommendation: length %d runes out of bounds [1,800]", n)
	}
	if r.Headline != "" {
		if n := len([]rune(r.Headline)); n > 80 {
			return nil, fmt.Errorf("stage2: headline: length %d runes exceeds maxLength 80", n)
		}
	}
	return &r, nil
}

// --- recommendation guard (§6 step 11b) ---

// The fixed deny-set below is URLs, pipes to a shell, and the
// command tokens that would let a report's "recommendation" (a proposal
// text delivered to a trusted human, never executed by this system) turn
// into a copy-pasteable attack. This is deliberately NOT a prompt
// instruction — sentinel.md cannot be relied on to police its own output,
// same principle as notify's markdown stripping (C8).
var (
	urlSchemeRe  = regexp.MustCompile(`(?i)\b(https?|ftp)://`)
	pipeShellRe  = regexp.MustCompile(`(?i)\|\s*(sh|bash)\b`)
	dangerTokens = []string{
		"curl", "wget", "nc ", "netcat", "base64", "chmod +x", "ssh ",
	}
)

func containsDangerousContent(s string) bool {
	if urlSchemeRe.MatchString(s) || pipeShellRe.MatchString(s) {
		return true
	}
	lower := strings.ToLower(s)
	for _, tok := range dangerTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

const recommendationWithheldEvidence = "recommendation withheld"

// safeBodyReplacement is used when body itself trips the guard. body is a
// schema-required field (minLength 1, §3.1), so "blank" cannot mean the
// empty string the way it does for the optional recommendation field —
// this fixed sentence keeps the document valid while removing the
// dangerous text.
const safeBodyReplacement = "The analyzer's summary was withheld because it proposed a command-like or URL-bearing action; this supervisor never executes actions, and the text was suppressed rather than risk a trusted operator copy-pasting it."

// guardRecommendations implements §6 step 11b: every finding's
// recommendation and the report body are checked against the deny-pattern
// set; a match blanks the offending field and appends one watch/meta
// finding naming the withholding. Idempotent to call on any report,
// including one where no finding has a recommendation at all (the common
// stage-1-only case) — a no-op in that case.
func guardRecommendations(rep *report.Report) {
	triggered := false
	for i := range rep.Findings {
		if rep.Findings[i].Recommendation != "" && containsDangerousContent(rep.Findings[i].Recommendation) {
			rep.Findings[i].Recommendation = ""
			triggered = true
		}
	}
	if containsDangerousContent(rep.Body) {
		rep.Body = safeBodyReplacement
		triggered = true
	}
	if !triggered {
		return
	}
	// §6 step 11b: the guard "appends the notice" unconditionally — it must
	// never be silently dropped because findings happened to be at the
	// schema's 20-item cap. A dangerous recommendation was just withheld;
	// losing the record of that is worse than losing whichever existing
	// finding is least important. Evict one to make room: prefer an "info"
	// finding (the lowest-severity, most likely to be the padding
	// "all clear" line), else the last finding in the slice.
	if len(rep.Findings) >= 20 {
		evict := len(rep.Findings) - 1
		for i, f := range rep.Findings {
			if f.Severity == "info" {
				evict = i
				break
			}
		}
		rep.Findings = append(rep.Findings[:evict], rep.Findings[evict+1:]...)
	}
	rep.Findings = append(rep.Findings, report.Finding{
		Severity:  "watch",
		Component: "meta",
		Evidence:  recommendationWithheldEvidence,
		Explanation: "The analyzer proposed a command-like or URL-bearing action. It was withheld: this " +
			"supervisor never executes actions, and a copy-pasteable proposal could mislead an operator " +
			"into running something unsafe.",
		Key: dedup.Key("meta", recommendationWithheldEvidence),
	})
	recomputeStatus(rep)
}

// recomputeStatus mirrors report.Validate's own status/highest-severity
// rule (C5) — guardRecommendations can raise the highest severity present
// by appending a watch finding, and the document must stay internally
// consistent (report.Validate would otherwise reject it).
func recomputeStatus(rep *report.Report) {
	rank := map[string]int{"info": 1, "watch": 2, "alert": 3}
	highest := 0
	for _, f := range rep.Findings {
		if r := rank[f.Severity]; r > highest {
			highest = r
		}
	}
	switch {
	case highest >= 3:
		rep.Status = "ALERT"
	case highest == 2:
		rep.Status = "WATCH"
	default:
		rep.Status = "OK"
	}
}
