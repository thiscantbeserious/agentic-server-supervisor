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
		b.WriteString("\n\n**Findings**")
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
				line = TruncRunes(Sanitize(line), 200, "")
				b.WriteString("\n  `" + line + "`")
			}

			if analysis := oneLine(Sanitize(f.Analysis)); analysis != "" {
				b.WriteString("\n  _Analysis:_ " + analysis)
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

	out := b.String()
	runes := []rune(out)
	if len(runes) > cfg.NotifyBodyMax {
		out = string(runes[:cfg.NotifyBodyMax]) + "\n\n_…truncated_"
	}
	return out
}
