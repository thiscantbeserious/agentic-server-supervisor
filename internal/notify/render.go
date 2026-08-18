// render.go: the payload renderer. Everything that turns a report.Report
// into the four-field apprise Payload, per contracts/notify.md N.3.
package notify

import (
	"fmt"
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

// Sanitize drops Telegram markdown metacharacters and control characters so
// no report text can break the parser at the notification layer.
// ponytail: strip instead of escape — a mangled log line is acceptable,
// a permanently rejected Telegram message is not.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`', '_', '*', '[', ']':
			return -1
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

// sanitizeEvidence is N.3.3's amended evidence rule: evidence is rendered
// verbatim inside a markdown code span, where every metacharacter except
// a backtick is already literal — so unlike Sanitize, only the backtick
// is touched (replaced with a plain quote so it can't close the span
// early). Control characters and invalid UTF-8 are still removed.
// Stripping `_` here (as Sanitize would) corrupted the one field analyze
// guarantees is "copied verbatim from FACTS": cksum_errors=1 reached the
// operator as cksumerrors=1, breaking their ability to grep their own
// logs for what the alert named.
func sanitizeEvidence(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '`':
			return '\''
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

// stripControl is N.3.6's plain-text sanitizer (the SMTP path): nothing
// declares that body's format to a parser, so nothing needs escaping for
// one — only control characters and invalid UTF-8 are removed. `_ * [ ]`
// and backticks all pass through unchanged, which makes evidence over
// SMTP byte-identical to what the collector saw.
func stripControl(s string) string {
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

// buildBody assembles the markdown body deterministically per N.3.3.
func buildBody(r report.Report, cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(Sanitize(r.Body))

	if len(r.Findings) > 0 {
		b.WriteString("\n")
		// The header labels a list — with exactly one finding it labels a
		// list of one and costs a line on a phone screen for nothing.
		if len(r.Findings) > 1 {
			b.WriteString("\n**Findings**")
		}
		for _, f := range r.Findings {
			sev := strings.ToUpper(oneLine(Sanitize(f.Severity)))
			comp := oneLine(Sanitize(f.Component))
			expl := oneLine(Sanitize(f.Explanation))
			fmt.Fprintf(&b, "\n- **%s · %s** — %s", sev, comp, expl)

			lines := strings.Split(f.Evidence, "\n")
			if len(lines) > 3 {
				lines = lines[:3]
			}
			for _, line := range lines {
				line = TruncRunes(sanitizeEvidence(line), 200, "")
				b.WriteString("\n  `" + line + "`")
			}

			// analysis answers "how bad is this" — needed at 3am for an
			// alert, not for a watch read over coffee. The watch case
			// keeps evidence and recommendation; the full analysis still
			// lives in the report, in history, and in the next prompt.
			if f.Severity == "alert" {
				if analysis := oneLine(Sanitize(f.Analysis)); analysis != "" {
					b.WriteString("\n  _Analysis:_ " + analysis)
				}
			}
			if rec := oneLine(Sanitize(f.Recommendation)); rec != "" {
				b.WriteString("\n  _Recommendation:_ " + rec)
			}
		}
	}

	if len(r.Resolved) > 0 {
		b.WriteString("\n\n**Resolved**")
		for _, res := range r.Resolved {
			b.WriteString("\n- " + oneLine(Sanitize(res)))
		}
	}

	return truncateBody(b.String(), cfg.NotifyBodyMax, "\n\n_…truncated_")
}

// BuildTextBody is N.3.6: the same content and order as buildBody, with
// markup removed rather than reformatted — the plain-text body sent over
// SMTP (N.5.1), where nothing declares the body's format to a parser.
func BuildTextBody(cfg *config.Config, r report.Report) string {
	var b strings.Builder
	b.WriteString(stripControl(r.Body))

	if len(r.Findings) > 0 {
		b.WriteString("\n")
		if len(r.Findings) > 1 {
			b.WriteString("\nFINDINGS")
		}
		for _, f := range r.Findings {
			sev := strings.ToUpper(oneLine(stripControl(f.Severity)))
			comp := oneLine(stripControl(f.Component))
			expl := oneLine(stripControl(f.Explanation))
			fmt.Fprintf(&b, "\n- %s · %s — %s", sev, comp, expl)

			lines := strings.Split(f.Evidence, "\n")
			if len(lines) > 3 {
				lines = lines[:3]
			}
			for _, line := range lines {
				line = TruncRunes(stripControl(line), 200, "")
				b.WriteString("\n    " + line)
			}

			if f.Severity == "alert" {
				if analysis := oneLine(stripControl(f.Analysis)); analysis != "" {
					b.WriteString("\n    Analysis: " + analysis)
				}
			}
			if rec := oneLine(stripControl(f.Recommendation)); rec != "" {
				b.WriteString("\n    Recommendation: " + rec)
			}
		}
	}

	if len(r.Resolved) > 0 {
		b.WriteString("\n\nRESOLVED")
		for _, res := range r.Resolved {
			b.WriteString("\n- " + oneLine(stripControl(res)))
		}
	}

	// The text body claims no markdown syntax at all (N.3.6), so its own
	// truncation marker must not smuggle a "_..._" italics pair back in.
	return truncateBody(b.String(), cfg.NotifyBodyMax, "\n\n...truncated")
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
