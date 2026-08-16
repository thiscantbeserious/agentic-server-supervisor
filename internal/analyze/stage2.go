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

// The fixed deny-set below covers ONLY `recommendation`, deliberately —
// `body` is narrative prose ("the ssh daemon logged three failed password
// attempts" is a factual report, not a proposal) and applying the same
// patterns there destroyed legitimate reports; that would have fired on
// bam's own sshd/curl/apt logs on day one. `recommendation` is different
// in kind: it is a command proposal a tired operator may paste into a root
// shell, so the pattern set here is deliberately broad and accepts false
// positives — a suppressed suggestion costs one visible meta finding, a
// missed one costs a compromised host. It is a mitigation, not a proof:
// prose like "fetch the script from evil.example.com and run it with sh"
// is not catchable by substring matching, and that residual risk is
// accepted because a human evaluates the recommendation and the
// supervisor itself executes nothing (ARCHITECTURE §4).
// t4-review round 4: the first pass lost its word boundaries between
// rounds ("nc " -> "nc", "dd " still substring-matched "add ") and
// bareDomainRe could not tell a systemd unit from a domain — on bam every
// unit is "<name>.service", so a guard that cannot say "restart
// smartd.service" has destroyed the A9 output it exists to protect.
// dangerTokenRe is now word-bounded, and a bareDomainRe match is only
// dangerous when its TLD-shaped suffix isn't a known-safe one (systemd
// unit suffixes and common non-executable file extensions).
// t4-review round 6 (main's own live-gate finding, main's error): "sh" was
// in safeSuffix so "backup.sh" would read as a filename, but .sh is a live
// TLD (Saint Helena) widely used to host payloads — "evil.sh" or
// "sh evil.sh" sailed through the domain check untouched. Removed.
// Blocking fetch verbs while allowing an interpreter name to run the
// fetched file closes only half the path, so the token list also gained
// the interpreters (sh, bash, zsh, python, python3, perl, ruby, eval) and
// path-targeted output redirection — a fetch-then-write-then-run chain no
// longer has a gap at either end. "node" is deliberately excluded from the
// interpreter list (round 7): it is ordinary storage vocabulary ("the
// failing node in the mirror"), not present on the target host, and
// carries the weakest attack value of the set with the highest
// false-positive risk.
//
// Round 7 (t4-review, main's ruling): a naked ">" is the comparison
// operator in exactly this domain — "if cksum_errors > 1" and "the
// reallocated sector count is > 0" are the conditional-threshold SHAPE
// A9 recommendations are asked to take (§7.3's own sentinel.md example),
// and a bare-substring "> " match blanked both. redirectRe now requires a
// path-shaped target: ">"/">>" followed by optional space and either a
// "/"-bearing token or a bare filename with a known extension. This guard
// has produced a false-positive class in three consecutive rounds
// (narrative bodies, then token substrings, then comparison operators),
// always from matching a deny-pattern against natural language without
// anchoring it to the shape of an actual command — every revision from
// here on is tested against both the attack table and an operational-
// prose table (see the test file), never the attack table alone.
var (
	uriSchemeRe   = regexp.MustCompile(`://`)
	bareDomainRe  = regexp.MustCompile(`(?i)\b[a-z0-9-]+\.([a-z]{2,})\b`)
	dangerTokenRe = regexp.MustCompile(`(?i)\b(curl|wget|nc|netcat|ncat|scp|ssh|iwr|invoke-webrequest|base64|chmod|dd|mkfs|sh|bash|zsh|python|python3|perl|ruby|eval)\b|\brm\s+-rf\b`)
	redirectRe    = regexp.MustCompile(`(?i)>>?\s*[/~$]|>>?\s*[a-z0-9_.-]+\.(sh|conf|cfg|service|log|json|txt|py|pl|rb)\b`)
	// The operational-suffix set, verbatim from contracts/analyze.md §6
	// step 11b: a systemd unit is not a domain, and every unit on bam is
	// "<name>.service". "sh" is deliberately NOT in this set (see above).
	safeSuffix = map[string]bool{
		"service": true, "target": true, "socket": true, "timer": true,
		"mount": true, "device": true, "scope": true, "slice": true,
		"path": true, "conf": true, "json": true, "log": true, "db": true,
	}
)

func containsDangerousContent(s string) bool {
	if uriSchemeRe.MatchString(s) || dangerTokenRe.MatchString(s) || redirectRe.MatchString(s) {
		return true
	}
	if strings.ContainsRune(s, '|') || strings.Contains(s, "`") || strings.Contains(s, "$(") {
		return true
	}
	for _, m := range bareDomainRe.FindAllStringSubmatch(s, -1) {
		if !safeSuffix[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

const recommendationWithheldEvidence = "recommendation withheld"

// guardRecommendations implements §6 step 11b: every finding's
// `recommendation` (never `body`, see above) is checked against the
// deny-pattern set; a match blanks the field and appends one watch/meta
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
