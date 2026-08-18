package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestBodyOrder(t *testing.T) {
	cfg := testCfg(t)
	r := loadFixture(t, "report-watch-zfs-cksum.json")
	p := BuildPayload(r, cfg)

	iExpl := strings.Index(p.Body, "ZFS detected and repaired")
	iAnalysis := strings.Index(p.Body, "_Analysis:_")
	iRec := strings.Index(p.Body, "_Recommendation:_")
	if iExpl < 0 || iAnalysis < 0 || iRec < 0 {
		t.Fatalf("body missing expected sections: %s", p.Body)
	}
	if !(iExpl < iAnalysis && iAnalysis < iRec) {
		t.Errorf("wrong order: explanation=%d analysis=%d recommendation=%d", iExpl, iAnalysis, iRec)
	}

	// Absent optionals render no label.
	r2 := report.Report{
		Status: "WATCH", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "watch", Component: "zfs", Evidence: "e", Explanation: "exp"}},
		Resolved: []string{},
	}
	p2 := BuildPayload(r2, cfg)
	if strings.Contains(p2.Body, "_Analysis:_") || strings.Contains(p2.Body, "_Recommendation:_") {
		t.Errorf("absent optionals rendered a label: %s", p2.Body)
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
	// "[" "]" ("[ALERT] host: headline") — that markup is the renderer's
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
	// The renderer's OWN markdown structure legitimately contains backticks,
	// asterisks and underscores (**Findings**, `evidence`, _Analysis:_) —
	// check only the parts that came from report text, not the template.
	if strings.Contains(p.Body, "reveal") {
		bodyLines := strings.Split(p.Body, "\n")
		if len(bodyLines) == 0 || strings.ContainsAny(bodyLines[0], "`_*[]") {
			t.Errorf("sanitized body line still carries markdown metacharacters: %q", bodyLines[0])
		}
	}
	if strings.Contains(p.Title, "ignore previous instructions") == false {
		t.Fatalf("setup guard: title should still carry the (harmless as data) injection phrase: %q", p.Title)
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
