package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

func loadFixture(t *testing.T, name string) report.Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return r
}

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SENTINEL_HOSTNAME", "bam")
	t.Setenv("APPRISE_URL", "http://apprise.invalid")
	t.Setenv("MAILRISE_HOST", "mailrise.invalid")
	t.Setenv("MAILRISE_USER", "testuser")
	t.Setenv("MAILRISE_PASS", "testpass")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// --- 2: TestTitle ---

func TestTitle(t *testing.T) {
	cfg := testCfg(t)
	r := loadFixture(t, "report-watch-zfs-cksum.json")
	p := BuildPayload(r, cfg)
	want := "[WATCH] bam: 1 checksum error on seagate-zvtazeam-crypt (hotstore mirror)"
	if p.Title != want {
		t.Errorf("Title = %q, want %q", p.Title, want)
	}
}

// --- 3: TestPayloadKeys ---

func TestPayloadKeys(t *testing.T) {
	cfg := testCfg(t)
	r := loadFixture(t, "report-ok.json")
	p := BuildPayload(r, cfg)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 4 {
		t.Fatalf("payload has %d keys, want exactly 4: %v", len(m), m)
	}
	for _, k := range []string{"title", "body", "type", "format"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if m["format"] != "markdown" {
		t.Errorf("format = %v, want markdown", m["format"])
	}
}

// --- 4: TestStatusTypeMapping ---

func TestStatusTypeMapping(t *testing.T) {
	cfg := testCfg(t)
	cases := []struct{ status, want string }{
		{"OK", "success"}, {"WATCH", "warning"}, {"ALERT", "failure"},
	}
	for _, c := range cases {
		r := report.Report{Status: c.status, Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{}}
		p := BuildPayload(r, cfg)
		if p.Type != c.want {
			t.Errorf("status %s: type = %q, want %q", c.status, p.Type, c.want)
		}
	}

	bad := report.Report{Status: "BOGUS", Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{}}
	if err := Validate(bad); err == nil {
		t.Error("unknown status must fail Validate")
	}
}

// --- 5: TestBodyOrder ---
//
// 2593e07's skeleton: GAP, heading, value, repeated per section, in
// order explanation -> Evidence -> Analysis (alert only) -> Recommendation.

func TestBodyOrder(t *testing.T) {
	cfg := testCfg(t)
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{{
			Severity: "alert", Component: "zfs", Evidence: "e",
			Explanation: "ZFS detected and repaired one checksum mismatch",
			Analysis:    "Trend analysis text", Recommendation: "Recommendation text",
		}},
		Resolved: []string{},
	}
	p := BuildPayload(r, cfg)

	iExpl := strings.Index(p.Body, "ZFS detected and repaired")
	iEvHeading := strings.Index(p.Body, "**Evidence:**")
	iAnalysisHeading := strings.Index(p.Body, "**Analysis:**")
	iAnalysis := strings.Index(p.Body, "Trend analysis text")
	iRecHeading := strings.Index(p.Body, "**Recommendation:**")
	iRec := strings.Index(p.Body, "Recommendation text")
	if iExpl < 0 || iEvHeading < 0 || iAnalysisHeading < 0 || iAnalysis < 0 || iRecHeading < 0 || iRec < 0 {
		t.Fatalf("body missing expected sections: %s", p.Body)
	}
	if !(iExpl < iEvHeading && iEvHeading < iAnalysisHeading && iAnalysisHeading < iAnalysis && iAnalysis < iRecHeading && iRecHeading < iRec) {
		t.Errorf("wrong order: expl=%d evidenceHeading=%d analysisHeading=%d analysis=%d recHeading=%d rec=%d",
			iExpl, iEvHeading, iAnalysisHeading, iAnalysis, iRecHeading, iRec)
	}

	// Absent optionals (watch severity: no analysis; empty recommendation) render no heading.
	r2 := report.Report{
		Status: "WATCH", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "watch", Component: "zfs", Evidence: "e", Explanation: "exp"}},
		Resolved: []string{},
	}
	p2 := BuildPayload(r2, cfg)
	if strings.Contains(p2.Body, "**Analysis:**") || strings.Contains(p2.Body, "**Recommendation:**") {
		t.Errorf("absent optionals rendered a heading: %s", p2.Body)
	}
}

// TestGapLineIsU2800 pins the exact codepoint, not just "whatever gapLine
// happens to equal", a self-referential check against the package
// constant would pass even if someone silently swapped it for a space.
// U+2800 was chosen because it is a printable character (nothing trims
// it) that still isn't itself pattern whitespace via unicode.IsSpace.
func TestGapLineIsU2800(t *testing.T) {
	const wantGap = "⠀" // BRAILLE PATTERN BLANK
	if gapLine != wantGap {
		t.Fatalf("gapLine = %U, want U+2800 BRAILLE PATTERN BLANK", []rune(gapLine))
	}
	r := []rune(gapLine)
	if len(r) != 1 {
		t.Fatalf("gapLine must be exactly one rune, got %d: %q", len(r), gapLine)
	}
	if r[0] != 0x2800 {
		t.Errorf("gapLine rune = U+%04X, want U+2800", r[0])
	}
	for _, forbidden := range []string{"", " ", " ", "​"} {
		if gapLine == forbidden {
			t.Errorf("gapLine must not be an empty string, ASCII space, NBSP, or zero-width space: got %q", gapLine)
		}
	}
}

// --- 2593e07: every heading is preceded by GAP, on both paths ---

func TestGapPrecedesEveryHeading(t *testing.T) {
	cfg := testCfg(t)
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{{
			Severity: "alert", Component: "zfs", Evidence: "e",
			Explanation: "exp", Analysis: "an", Recommendation: "rec",
		}},
		Resolved: []string{"closed"},
	}

	for name, body := range map[string]string{
		"markdown": BuildPayload(r, cfg).Body,
		"html":     BuildHTMLBody(cfg, r),
	} {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(body, "\n")
			gaps := 0
			for i, l := range lines {
				if l == gapLine {
					gaps++
					if i+1 >= len(lines) {
						t.Fatalf("GAP is the last line, no heading follows: %v", lines)
					}
					next := lines[i+1]
					if !strings.Contains(next, "Evidence:") && !strings.Contains(next, "Analysis:") &&
						!strings.Contains(next, "Recommendation:") && !strings.Contains(next, "Resolved:") &&
						!strings.Contains(next, "ZFS:") && !strings.Contains(next, ":") {
						t.Errorf("line after GAP is not a heading: %q", next)
					}
				}
			}
			// severity+component, Evidence, Analysis, Recommendation, Resolved = 5 GAPs.
			if gaps != 5 {
				t.Errorf("%s: found %d GAP lines, want 5 (one per heading section): %q", name, gaps, body)
			}
			if !strings.Contains(body, gapLine) {
				t.Fatal("setup guard: body must contain at least one GAP line")
			}
			if !utf8.ValidString(body) {
				t.Error("body is not valid UTF-8")
			}
		})
	}
}

// --- analysis is alert-only (unchanged rule, new skeleton) ---

func TestAnalysisOnlyForAlertSeverity(t *testing.T) {
	cfg := testCfg(t)
	base := report.Finding{
		Component: "zfs", Evidence: "e", Explanation: "exp",
		Analysis: "this is the analysis text", Recommendation: "rec",
	}

	watch := base
	watch.Severity = "watch"
	rWatch := report.Report{Status: "WATCH", Headline: "h", Body: "b", Findings: []report.Finding{watch}, Resolved: []string{}}
	pWatch := BuildPayload(rWatch, cfg)
	if strings.Contains(pWatch.Body, "Analysis:") || strings.Contains(pWatch.Body, "this is the analysis text") {
		t.Errorf("watch severity must not render analysis: %s", pWatch.Body)
	}
	// The watch case still keeps evidence and recommendation (N.3.3: "nothing actionable is lost").
	if !strings.Contains(pWatch.Body, "**Recommendation:**") {
		t.Errorf("watch severity must still render recommendation: %s", pWatch.Body)
	}

	alert := base
	alert.Severity = "alert"
	rAlert := report.Report{Status: "ALERT", Headline: "h", Body: "b", Findings: []report.Finding{alert}, Resolved: []string{}}
	pAlert := BuildPayload(rAlert, cfg)
	if !strings.Contains(pAlert.Body, "**Analysis:**\nthis is the analysis text") {
		t.Errorf("alert severity must render analysis: %s", pAlert.Body)
	}

	// Same rule on the HTML (SMTP) path.
	hWatch := BuildHTMLBody(cfg, rWatch)
	if strings.Contains(hWatch, "Analysis:") {
		t.Errorf("html body: watch severity must not render analysis: %s", hWatch)
	}
	hAlert := BuildHTMLBody(cfg, rAlert)
	if !strings.Contains(hAlert, "<b>Analysis:</b>\nthis is the analysis text") {
		t.Errorf("html body: alert severity must render analysis: %s", hAlert)
	}
}

// --- 2593e07: the Findings header is gone entirely, regardless of count ---

func TestFindingsHeaderGoneRegardlessOfCount(t *testing.T) {
	cfg := testCfg(t)
	one := report.Report{
		Status: "WATCH", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "watch", Component: "zfs", Evidence: "e1", Explanation: "exp1"}},
		Resolved: []string{},
	}
	two := report.Report{
		Status: "WATCH", Headline: "h", Body: "b",
		Findings: []report.Finding{
			{Severity: "watch", Component: "zfs", Evidence: "e1", Explanation: "exp1"},
			{Severity: "watch", Component: "smart", Evidence: "e2", Explanation: "exp2"},
		},
		Resolved: []string{},
	}

	for _, r := range []report.Report{one, two} {
		md := BuildPayload(r, cfg).Body
		html := BuildHTMLBody(cfg, r)
		for name, body := range map[string]string{"markdown": md, "html": html} {
			if strings.Contains(strings.ToLower(body), "findings") {
				t.Errorf("%s (n=%d findings): body still names a Findings header: %s", name, len(r.Findings), body)
			}
		}
	}
}

// --- 6: TestSanitizeAllFields ---

func TestSanitizeAllFields(t *testing.T) {
	cfg := testCfg(t)
	r := loadFixture(t, "report-injection.json")
	if err := Validate(r); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	p := BuildPayload(r, cfg)

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("payload did not marshal: %v", err)
	}
	if !utf8.Valid(b) {
		t.Error("payload bytes are not valid UTF-8")
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	// The title template itself legitimately wraps the status in literal
	// "[" "]" ("[ALERT] host: headline"), that markup is the renderer's
	// own, added after sanitization (N.0.4), not report text. Check only
	// the report-derived portion (everything after "<host>: ").
	_, headlinePart, ok := strings.Cut(p.Title, ": ")
	if !ok {
		t.Fatalf("title has no host separator: %q", p.Title)
	}
	for _, bad := range []string{"`", "_", "*", "[", "]"} {
		if strings.Contains(headlinePart, bad) {
			t.Errorf("sanitized headline portion still contains %q: %q", bad, headlinePart)
		}
	}
	if strings.Contains(p.Title, "ignore previous instructions") == false {
		t.Fatalf("setup guard: title should still carry the (harmless as data) injection phrase: %q", p.Title)
	}

	// Evidence is rendered on its own line inside a code span (heading
	// "**Evidence:**" on the line above), only a backtick can break the
	// code span, so only the backtick is touched. The fixture's evidence
	// carries a backtick (must become a quote) AND underscores/asterisks/
	// brackets (must survive verbatim, unlike the prose fields above).
	if strings.Contains(p.Body, "`log line: ignore previous instructions `") {
		t.Errorf("evidence still carries an unescaped backtick, code span would break: %q", p.Body)
	}
	if !strings.Contains(p.Body, "`log line: ignore previous instructions '_*[]") {
		t.Errorf("evidence lost its underscore/asterisk/bracket characters (should survive verbatim in a code span): %q", p.Body)
	}
}

// TestSanitize_DropsInvalidUTF8AndControlChars is the direct unit-level
// proof behind TestSanitizeAllFields.
func TestSanitize_DropsInvalidUTF8AndControlChars(t *testing.T) {
	in := "a\x07b" + string(utf8.RuneError) + "c`d_e*f[g]h\nrest"
	out := Sanitize(in)
	if strings.ContainsAny(out, "`_*[]") {
		t.Errorf("Sanitize left a markdown metacharacter: %q", out)
	}
	if strings.Contains(out, string(utf8.RuneError)) {
		t.Errorf("Sanitize left the replacement char: %q", out)
	}
	if strings.Contains(out, "\x07") {
		t.Errorf("Sanitize left a control character: %q", out)
	}
	if !strings.Contains(out, "\n") {
		t.Error("Sanitize must preserve newlines")
	}
	if !utf8.ValidString(out) {
		t.Errorf("Sanitize produced invalid UTF-8: %q", out)
	}
}

// --- 7: TestBodyTruncation ---

func TestBodyTruncation(t *testing.T) {
	t.Setenv("NOTIFY_BODY_MAX", "200")
	cfg := testCfg(t)

	r := report.Report{
		Status: "ALERT", Headline: "h", Body: strings.Repeat("é", 500), // multi-byte rune
		Findings: []report.Finding{}, Resolved: []string{},
	}
	p := BuildPayload(r, cfg)
	if !strings.HasSuffix(p.Body, "_…truncated_") {
		t.Fatalf("body not marked truncated: %q", p.Body[len(p.Body)-40:])
	}
	if !utf8.ValidString(p.Body) {
		t.Fatal("truncation split a multi-byte rune")
	}
	runes := []rune(p.Body)
	suffixRunes := len([]rune("\n\n_…truncated_"))
	if len(runes) > 200+suffixRunes {
		t.Errorf("body is %d runes, want <= %d (200 + suffix)", len(runes), 200+suffixRunes)
	}

	// Untruncated body carries no suffix.
	short := report.Report{Status: "OK", Headline: "h", Body: "short", Findings: []report.Finding{}, Resolved: []string{}}
	if p2 := BuildPayload(short, cfg); strings.Contains(p2.Body, "truncated") {
		t.Errorf("short body wrongly marked truncated: %q", p2.Body)
	}
}

func TestTruncRunes(t *testing.T) {
	if got := TruncRunes("hello", 10, "..."); got != "hello" {
		t.Errorf("no cut: got %q", got)
	}
	if got := TruncRunes("hello world", 5, "..."); got != "hello..." {
		t.Errorf("cut: got %q", got)
	}
	// multi-byte runes
	s := "日本語abc"
	if got := TruncRunes(s, 3, ""); got != "日本語" {
		t.Errorf("multi-byte cut: got %q", got)
	}
}

// --- 16: TestHostnameSource ---

func TestHostnameSource(t *testing.T) {
	cfg := testCfg(t) // cfg.Hostname == "bam" via SENTINEL_HOSTNAME
	if cfg.Hostname != "bam" {
		t.Fatalf("test setup: cfg.Hostname = %q, want bam", cfg.Hostname)
	}

	withMeta := report.Report{
		Status: "OK", Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{},
		Meta: &report.Meta{Hostname: "other-host"},
	}
	p := BuildPayload(withMeta, cfg)
	if !strings.Contains(p.Title, "other-host:") {
		t.Errorf("meta.hostname did not win: %q", p.Title)
	}

	withoutMeta := report.Report{Status: "OK", Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{}}
	p2 := BuildPayload(withoutMeta, cfg)
	if !strings.Contains(p2.Title, "bam:") {
		t.Errorf("cfg.Hostname fallback did not apply: %q", p2.Title)
	}

	// os.Hostname() must never be consulted: the real machine hostname (this
	// test's own host) must not appear anywhere BuildPayload could have used
	// it instead of cfg.Hostname/meta.hostname.
	real, err := os.Hostname()
	if err == nil && real != "bam" && real != "other-host" && strings.Contains(p.Title+p2.Title, real) {
		t.Errorf("payload leaked the real machine hostname %q", real)
	}
}

// --- evidence fidelity (cksum_errors must not become cksumerrors), both paths ---

func TestEvidenceSurvivesVerbatim_Underscores(t *testing.T) {
	cfg := testCfg(t)
	evidence := "zed1284: eid=41 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1"
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "alert", Component: "zfs", Evidence: evidence, Explanation: "exp"}},
		Resolved: []string{},
	}

	p := BuildPayload(r, cfg)
	if !strings.Contains(p.Body, "cksum_errors=1") {
		t.Errorf("markdown body corrupted evidence, want literal cksum_errors=1: %s", p.Body)
	}
	if strings.Contains(p.Body, "cksumerrors") {
		t.Errorf("markdown body dropped the underscore in cksum_errors: %s", p.Body)
	}

	html := BuildHTMLBody(cfg, r)
	if !strings.Contains(html, "cksum_errors=1") {
		t.Errorf("html body corrupted evidence, want literal cksum_errors=1: %s", html)
	}
}

// TestEvidenceAngleBracketsSurviveEscaping is 2593e07's core HTML-path
// claim: kernel evidence routinely carries '<'/'>'/'&' (e.g. "<mce>"), and
// html.EscapeString must turn those into their entity forms BEFORE the
// <code> tag is added, so the wire text stays valid HTML while the
// rendered client shows the operator the original characters back.
func TestEvidenceAngleBracketsSurviveEscaping(t *testing.T) {
	cfg := testCfg(t)
	evidence := "sd 0:0:0:0: [sda] <mce> A&B < C > D"
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "alert", Component: "kernel", Evidence: evidence, Explanation: "exp"}},
		Resolved: []string{},
	}
	html := BuildHTMLBody(cfg, r)

	for _, want := range []string{"&lt;mce&gt;", "A&amp;B", "&lt; C &gt; D"} {
		if !strings.Contains(html, want) {
			t.Errorf("html body missing escaped form %q: %s", want, html)
		}
	}
	// The raw, unescaped angle brackets must not appear inside the
	// evidence text itself (only as part of the <code>/</code> tags the
	// renderer adds, which this substring excludes).
	if strings.Contains(html, "<mce>") {
		t.Errorf("html body still carries a raw unescaped '<mce>': %s", html)
	}
}

// --- HTML body: only <b> and <code> tags, GAP-separated skeleton ---

var htmlTagRe = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*[^>]*>`)

func TestBuildHTMLBody_OnlyBAndCodeTags(t *testing.T) {
	cfg := testCfg(t)
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "plain body text",
		Findings: []report.Finding{
			{Severity: "alert", Component: "zfs", Evidence: "ev1", Explanation: "exp1", Analysis: "an1", Recommendation: "rec1"},
			{Severity: "watch", Component: "smart", Evidence: "ev2", Explanation: "exp2", Recommendation: "rec2"},
		},
		Resolved: []string{"closed one"},
	}
	body := BuildHTMLBody(cfg, r)

	for _, tag := range htmlTagRe.FindAllString(body, -1) {
		switch tag {
		case "<b>", "</b>", "<code>", "</code>":
		default:
			t.Errorf("html body contains a disallowed tag %q: %s", tag, body)
		}
	}
	if !strings.Contains(body, gapLine) {
		t.Error("html body missing GAP separators")
	}
	if !strings.Contains(body, "<b>Resolved:</b>\nclosed one") {
		t.Errorf("html body missing plain Resolved heading+entry: %s", body)
	}
	if !strings.Contains(body, "<b>Recommendation:</b>\nrec1") {
		t.Errorf("html body missing Recommendation heading+value: %s", body)
	}
	if strings.Contains(body, "**") {
		t.Errorf("html body still carries markdown bold syntax: %s", body)
	}
}

func TestBuildHTMLBody_Truncation(t *testing.T) {
	t.Setenv("NOTIFY_BODY_MAX", "200")
	cfg := testCfg(t)
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: strings.Repeat("x", 500),
		Findings: []report.Finding{}, Resolved: []string{},
	}
	html := BuildHTMLBody(cfg, r)
	if !strings.HasSuffix(html, "_…truncated_") {
		t.Fatalf("html body not marked truncated: %q", html[len(html)-40:])
	}
	if !utf8.ValidString(html) {
		t.Fatal("truncation split a multi-byte rune")
	}
}

// --- 2593e07: bullet/dot/dash/leading-space regression guard, both paths ---

func TestNoLegacySeparatorsOrIndentation(t *testing.T) {
	cfg := testCfg(t)
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{
			{Severity: "alert", Component: "zfs", Evidence: "ev1", Explanation: "exp1", Analysis: "an1", Recommendation: "rec1"},
			{Severity: "watch", Component: "smart", Evidence: "ev2", Explanation: "exp2", Recommendation: "rec2"},
		},
		Resolved: []string{"closed one", "closed two"},
	}

	for name, body := range map[string]string{
		"markdown": BuildPayload(r, cfg).Body,
		"html":     BuildHTMLBody(cfg, r),
	} {
		t.Run(name, func(t *testing.T) {
			for _, bad := range []string{"·", ", ", "- "} {
				if strings.Contains(body, bad) {
					t.Errorf("body still carries legacy separator %q: %s", bad, body)
				}
			}
			if strings.Contains(strings.ToLower(body), "findings") {
				t.Errorf("body still names a Findings header: %s", body)
			}
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, " ") {
					t.Errorf("line begins with a space (indentation does not survive a wrap): %q in %s", line, body)
				}
			}
		})
	}
}
