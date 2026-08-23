// guard.go: the deterministic deny-list over the recommendation field,
// the last check before model output reaches text an operator might paste
// into a root shell.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"regexp"
	"strings"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/dedup"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/report"
)

// The deny-list below covers only the recommendation field, deliberately.
// Body text is narrative ("the ssh daemon logged three failed password
// attempts" is a factual report, not a proposal), and applying these
// patterns there destroyed legitimate reports on day one. A recommendation
// is different in kind: it is a command proposal a tired operator may paste
// into a root shell, so the patterns are broad and false positives are
// accepted, a suppressed suggestion costs one visible meta finding, a
// missed one can cost the host.
//
// Hard-won shape rules, each from a real false-positive class:
//   - danger tokens are word-bounded ("dd" must not match "add");
//   - a TLD-shaped suffix is only dangerous when it is not operational
//     vocabulary, on the target every systemd unit is "<name>.service",
//     and a guard that cannot say "restart smartd.service" destroys the
//     output it protects. "sh" is deliberately absent from the safe set:
//     .sh is a live TLD widely used to host payloads;
//   - interpreters (sh, bash, python, ...) are blocked alongside fetch
//     verbs, because blocking the download while allowing "sh payload.sh"
//     closes only half the path. "node" is excluded: it is ordinary
//     storage vocabulary here and absent from the target host;
//   - a redirect must have a path-shaped target: a naked ">" is the
//     comparison operator in this domain ("if cksum_errors > 1" is the
//     exact conditional shape recommendations are asked to take).
//
// This is a mitigation, not a proof: "fetch the script from evil.example
// and run it with the shell" is not catchable by substring matching. The
// residual risk is accepted because a human evaluates every recommendation
// and the supervisor itself executes nothing. Any change here must pass
// both the attack table and the operational-prose table in the tests,
// three consecutive false-positive classes came from testing against the
// attack table alone.
var (
	uriSchemeRe   = regexp.MustCompile(`://`)
	bareDomainRe  = regexp.MustCompile(`(?i)\b[a-z0-9-]+\.([a-z]{2,})\b`)
	dangerTokenRe = regexp.MustCompile(`(?i)\b(curl|wget|nc|netcat|ncat|scp|ssh|iwr|invoke-webrequest|base64|chmod|dd|mkfs|sh|bash|zsh|python|python3|perl|ruby|eval)\b|\brm\s+-rf\b`)
	redirectRe    = regexp.MustCompile(`(?i)>>?\s*["']?[/~$]|>>?\s*[a-z0-9_.-]+\.(sh|conf|cfg|service|log|json|txt|py|pl|rb)\b`)
	safeSuffix    = map[string]bool{
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

// guardRecommendations blanks any recommendation matching the deny-list and
// appends one watch finding recording the withholding. The record is never
// dropped: if the report is at the findings cap, the first "info"
// (all-clear) finding is evicted to make room, or the last finding when the
// report carries no "info" finding, losing an existing finding is better
// than silently losing the fact that a dangerous proposal was suppressed.
// Idempotent, and a no-op when no finding carries a recommendation.
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

// recomputeStatus re-derives status from the highest finding severity.
// Appending the guard's watch finding can raise the highest severity, and
// a report whose status disagrees with its findings fails validation.
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
