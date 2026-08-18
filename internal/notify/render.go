// render.go: the payload renderer. Everything that turns a report.Report
// into the four-field apprise Payload, per contracts/notify.md N.3.
package notify

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// Payload is the exact four-field JSON body posted to apprise (N.3.1).
type Payload struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Type   string `json:"type"`
	Format string `json:"format"`
}

// stripUnsafe removes what may never appear in the wire body regardless of
// markup: invalid UTF-8, and control characters other than '\n'. It is the
// non-markdown half of Sanitize, extracted so every path — markdown
// (Sanitize, sanitizeEvidence) and HTML (htmlStyle, below) — shares one
// copy of the rule rather than three that could drift.
func stripUnsafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case utf8.RuneError:
			return -1
		case '\n':
			return '\n'
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// Sanitize drops Telegram markdown metacharacters and control characters so
// no report text can break the parser at the notification layer.
// ponytail: strip instead of escape — a mangled log line is acceptable,
// a permanently rejected Telegram message is not.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`', '_', '*', '[', ']':
			return -1
		}
		return r
	}, stripUnsafe(s))
}

// sanitizeEvidence is N.3.3's amended evidence rule: evidence is rendered
// verbatim inside a markdown code span, where every metacharacter except
// a backtick is already literal — so unlike Sanitize, only the backtick
// is touched (replaced with a plain quote so it can't close the span
// early). Control characters and invalid UTF-8 are still removed via
// stripUnsafe. Stripping `_` here (as Sanitize would) corrupted the one
// field analyze guarantees is "copied verbatim from FACTS":
// cksum_errors=1 reached the operator as cksumerrors=1, breaking their
// ability to grep their own logs for what the alert named.
func sanitizeEvidence(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '`' {
			return '\''
		}
		return r
	}, stripUnsafe(s))
}

// TruncRunes returns s cut to max runes, appending ellipsis when it cut.
func TruncRunes(s string, max int, ellipsis string) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + ellipsis
}

var wsRunRe = regexp.MustCompile(`\s+`)

// oneLine collapses newlines and runs of spaces — applied to every field
// except body (N.3.3).
func oneLine(s string) string {
	return strings.TrimSpace(wsRunRe.ReplaceAllString(s, " "))
}

func typeForStatus(status string) string {
	switch status {
	case "OK":
		return "success"
	case "WATCH":
		return "warning"
	case "ALERT":
		return "failure"
	default:
		return ""
	}
}

// BuildPayload renders one report into the exact four-field Payload N.3.1
// defines. Callers must have already run Validate — BuildPayload does not
// re-check the enum/required-field shape, only sanitizes and truncates.
func BuildPayload(r report.Report, cfg *config.Config) Payload {
	host := cfg.Hostname
	if r.Meta != nil && r.Meta.Hostname != "" {
		host = r.Meta.Hostname
	}
	host = oneLine(Sanitize(host))
	headline := TruncRunes(oneLine(Sanitize(r.Headline)), 80, "")

	title := fmt.Sprintf("[%s] %s: %s", r.Status, host, headline)
	title = TruncRunes(title, 120, "")

	return Payload{
		Title:  title,
		Body:   buildBody(r, cfg),
		Type:   typeForStatus(r.Status),
		Format: "markdown",
	}
}

// gapLine is N.3.3's vertical separator: U+2800 BRAILLE PATTERN BLANK, on
// a line of its own, between every section. Measured against a real
// Telegram client on 2026-08-18, not decorative — do not replace it with
// an empty line, an ASCII space, a U+00A0 NBSP, or a U+200B zero-width
// space:
//   - An empty line collapses to nothing on both delivery paths.
//   - A space or NBSP collapses on the apprise (markdown) path but
//     SURVIVES over SMTP — a gap that works on the fallback and vanishes
//     on the primary, the worst failure available, because it looks
//     correct in whichever path you happen to test.
//   - U+2800 is a printable character, not whitespace, so nothing trims
//     it, and it survives both. U+200B was also tried and rejected as
//     likelier to be stripped later by a client that treats it as
//     zero-width formatting rather than content.
const gapLine = "⠀"

// bodyStyle parameterizes the ONE skeleton N.3.3/N.3.6 share, so the two
// delivery paths cannot drift apart the way payload.Body vs. a hand-built
// text body already did once. heading wraps a label line ("Evidence:");
// code wraps one evidence line; prose sanitizes every non-evidence
// report-derived field; evidence sanitizes evidence lines specifically.
type bodyStyle struct {
	heading  func(label string) string
	code     func(s string) string
	prose    func(s string) string
	evidence func(s string) string
}

var markdownStyle = bodyStyle{
	heading:  func(label string) string { return "**" + label + "**" },
	code:     func(s string) string { return "`" + s + "`" },
	prose:    Sanitize,
	evidence: sanitizeEvidence,
}

// htmlEscapeSafe composes stripUnsafe with html.EscapeString: strip what
// may never appear in the wire body (invalid UTF-8, control chars other
// than '\n') BEFORE escaping, so the escaped output is always valid UTF-8
// with no embedded NUL — RFC 5321 §2.3.1 forbids NUL in SMTP DATA, and a
// message declaring charset=utf-8 must not carry invalid UTF-8 regardless.
func htmlEscapeSafe(s string) string {
	return html.EscapeString(stripUnsafe(s))
}

var htmlStyle = bodyStyle{
	heading:  func(label string) string { return "<b>" + label + "</b>" },
	code:     func(s string) string { return "<code>" + s + "</code>" },
	prose:    htmlEscapeSafe,
	evidence: htmlEscapeSafe,
}

// buildSkeleton is N.3.3's body assembly, shared verbatim by the markdown
// path (buildBody) and the HTML path (BuildHTMLBody) — same section
// order, same GAP placement, same alert-only/non-empty-only gating,
// differing only in style.heading/code/prose/evidence.
func buildSkeleton(r report.Report, cfg *config.Config, st bodyStyle, truncMarker string) string {
	// strings.Join treats a multi-line element the same as pre-splitting
	// it (both produce identical "\n"-joined output), so r.Body's own
	// internal newlines (Sanitize/html.EscapeString preserve '\n') need
	// no special handling here.
	lines := []string{st.prose(r.Body)}

	for _, f := range r.Findings {
		sev := strings.ToUpper(oneLine(st.prose(f.Severity)))
		comp := oneLine(st.prose(f.Component))
		expl := oneLine(st.prose(f.Explanation))

		lines = append(lines, gapLine, st.heading(sev+" "+comp+":"), expl)

		lines = append(lines, gapLine, st.heading("Evidence:"))
		evLines := strings.Split(f.Evidence, "\n")
		if len(evLines) > 3 {
			evLines = evLines[:3]
		}
		for _, ev := range evLines {
			ev = TruncRunes(st.evidence(ev), 200, "")
			lines = append(lines, st.code(ev))
		}

		// analysis answers "how bad is this" — needed at 3am for an
		// alert, not for a watch read over coffee. The watch case keeps
		// evidence and recommendation; the full analysis still lives in
		// the report, in history, and in the next prompt.
		if f.Severity == "alert" {
			if analysis := oneLine(st.prose(f.Analysis)); analysis != "" {
				lines = append(lines, gapLine, st.heading("Analysis:"), analysis)
			}
		}
		if rec := oneLine(st.prose(f.Recommendation)); rec != "" {
			lines = append(lines, gapLine, st.heading("Recommendation:"), rec)
		}
	}

	if len(r.Resolved) > 0 {
		lines = append(lines, gapLine, st.heading("Resolved:"))
		for _, res := range r.Resolved {
			lines = append(lines, oneLine(st.prose(res)))
		}
	}

	return truncateBody(strings.Join(lines, "\n"), cfg.NotifyBodyMax, truncMarker)
}

// buildBody assembles the markdown body deterministically per N.3.3.
func buildBody(r report.Report, cfg *config.Config) string {
	return buildSkeleton(r, cfg, markdownStyle, "\n\n_…truncated_")
}

// BuildHTMLBody is N.3.6: the same skeleton as N.3.3, rendered as HTML for
// delivery over SMTP with Content-Type: text/html; charset=utf-8 — mailrise
// selects the notification format from Content-Type, so this renders bold
// and monospace on Telegram exactly like the apprise path (verified live
// 2026-08-18). Every report-derived string is escaped with html.EscapeString
// before any tag is added; nothing goes through Sanitize. Escaping is
// lossless where Sanitize's strip was destructive: cksum_errors=1 survives,
// and a literal '<' in kernel evidence (e.g. "<mce>") arrives as '<' in the
// rendered client instead of truncating or mangling the message.
func BuildHTMLBody(cfg *config.Config, r report.Report) string {
	return buildSkeleton(r, cfg, htmlStyle, "\n\n_…truncated_")
}

// truncateBody is N.3.3 step 4 / N.3.6: truncate to cfg.NotifyBodyMax
// runes; if it was cut, append marker.
func truncateBody(out string, max int, marker string) string {
	runes := []rune(out)
	if len(runes) > max {
		return string(runes[:max]) + marker
	}
	return out
}
