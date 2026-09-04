package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// --- apprise stub (httptest.Server recording the last body) ---

type appriseStub struct {
	mu       sync.Mutex
	status   int
	lastBody []byte
	lastPath string
	requests int
	srv      *httptest.Server
}

func newAppriseStub(t *testing.T, status int) *appriseStub {
	t.Helper()
	s := &appriseStub{status: status}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.lastPath = r.URL.Path
		s.lastBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(s.status)
		s.mu.Unlock()
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *appriseStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// --- SMTP stub: a net.Listen goroutine speaking the real protocol ---

type smtpStub struct {
	mu       sync.Mutex
	sawAuth  bool
	rcptTo   string
	dataText string
	ln       net.Listener
}

func newSMTPStub(t *testing.T) *smtpStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpStub{ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *smtpStub) addr() (string, string) {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	return host, port
}

func (s *smtpStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpStub) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := conn

	write := func(msg string) { w.Write([]byte(msg + "\r\n")) }
	write("220 stub.mailrise.local ESMTP")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-stub.mailrise.local")
			write("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.sawAuth = true
			s.mu.Unlock()
			write("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			s.mu.Lock()
			s.rcptTo = line
			s.mu.Unlock()
			write("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>")
			var data strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				data.WriteString(dl)
			}
			s.mu.Lock()
			s.dataText = data.String()
			s.mu.Unlock()
			write("250 OK: queued")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 Bye")
			return
		default:
			write("500 unrecognized command")
		}
	}
}

// --- config helper ---

func notifyTestConfig(t *testing.T, apprise *appriseStub, smtp *smtpStub) *config.Config {
	t.Helper()
	t.Setenv("SENTINEL_HOSTNAME", "bam")
	if apprise != nil {
		t.Setenv("APPRISE_URL", apprise.srv.URL)
	} else {
		t.Setenv("APPRISE_URL", "http://127.0.0.1:1")
	}
	t.Setenv("APPRISE_KEY", "sentinel")
	if smtp != nil {
		host, port := smtp.addr()
		t.Setenv("MAILRISE_HOST", host)
		t.Setenv("MAILRISE_PORT", port)
	}
	t.Setenv("MAILRISE_USER", "testuser")
	t.Setenv("MAILRISE_PASS", "testpass")
	t.Setenv("SENTINEL_MAIL_FROM", "sentinel@mailrise.xyz")
	t.Setenv("SENTINEL_MAIL_TO", "omv@mailrise.xyz")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func readFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// --- 1: TestFlags ---

func TestFlags(t *testing.T) {
	cfg := notifyTestConfig(t, newAppriseStub(t, 200), nil)
	ctx := context.Background()

	cases := []struct {
		name string
		args []string
		in   []byte
		want int
	}{
		{"help", []string{"--help"}, nil, 0},
		{"unknown flag", []string{"--bogus"}, nil, 64},
		{"two positional args", []string{"a", "b"}, nil, 64},
		{"dry-run and seed-config", []string{"--dry-run", "--seed-config"}, nil, 64},
		{"seed-config with file", []string{"--seed-config", "file.json"}, nil, 64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout bytes.Buffer
			got, _ := Run(ctx, cfg, c.args, bytes.NewReader(c.in), &stdout)
			if got != c.want {
				t.Errorf("Run(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}

// --- 8: TestDryRun ---

func TestDryRun(t *testing.T) {
	stub := newAppriseStub(t, 200)
	cfg := notifyTestConfig(t, stub, nil)
	raw := readFixtureBytes(t, "report-ok.json")

	var stdout bytes.Buffer
	code, err := Run(context.Background(), cfg, []string{"--dry-run"}, bytes.NewReader(raw), &stdout)
	if err != nil || code != 0 {
		t.Fatalf("Run() = %d, %v", code, err)
	}
	if stub.count() != 0 {
		t.Errorf("stub received %d requests, want 0", stub.count())
	}
	var p Payload
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout is not the payload JSON: %v (%q)", err, stdout.String())
	}
	if p.Title == "" {
		t.Error("dry-run payload has no title")
	}
}

// --- 9: TestInvalidReport ---

func TestInvalidReport(t *testing.T) {
	stub := newAppriseStub(t, 200)
	cfg := notifyTestConfig(t, stub, nil)
	raw := readFixtureBytes(t, "report-invalid.json")

	var stdout bytes.Buffer
	code, err := Run(context.Background(), cfg, nil, bytes.NewReader(raw), &stdout)
	if code != 65 || err == nil {
		t.Fatalf("Run() = %d, %v, want 65 and an error", code, err)
	}
	if stub.count() != 0 {
		t.Errorf("stub received %d requests, want 0", stub.count())
	}
}

// --- 10: TestSendFailures ---

func TestSendFailures(t *testing.T) {
	r := loadFixture(t, "report-ok.json")

	t.Run("502", func(t *testing.T) {
		stub := newAppriseStub(t, 502)
		cfg := notifyTestConfig(t, stub, nil)
		err := Send(context.Background(), cfg, r, false)
		if !errors.Is(err, ErrSend) {
			t.Fatalf("err = %v, want ErrSend", err)
		}
		if !strings.HasPrefix(err.Error(), "http 502: ") {
			t.Errorf("error text = %q, want prefix %q", err.Error(), "http 502: ")
		}
	})

	t.Run("closed server", func(t *testing.T) {
		stub := newAppriseStub(t, 200)
		stub.srv.Close() // now genuinely unreachable
		cfg := notifyTestConfig(t, stub, nil)
		err := Send(context.Background(), cfg, r, false)
		if !errors.Is(err, ErrSend) {
			t.Fatalf("err = %v, want ErrSend", err)
		}
		if !strings.HasPrefix(err.Error(), "transport: ") {
			t.Errorf("error text = %q, want prefix %q", err.Error(), "transport: ")
		}
	})

	t.Run("204 is failure not success", func(t *testing.T) {
		stub := newAppriseStub(t, 204)
		cfg := notifyTestConfig(t, stub, nil)
		err := Send(context.Background(), cfg, r, false)
		if !errors.Is(err, ErrSend) {
			t.Fatalf("204 must be treated as delivery failure, got err=%v", err)
		}
	})
}

// TestPostFailureLogKeys asserts N.3.5's two distinct log keys for a
// failed POST, "http=<code>" or "transport=<err>", not one generic
// "error" key a log-scraping alert can't distinguish by cause.
func TestPostFailureLogKeys(t *testing.T) {
	r := loadFixture(t, "report-ok.json")

	t.Run("http", func(t *testing.T) {
		cfg := notifyTestConfig(t, newAppriseStub(t, 502), nil)
		var logBuf bytes.Buffer
		logWriter = &logBuf
		defer func() { logWriter = nil }()

		Send(context.Background(), cfg, r, false)
		if !strings.Contains(logBuf.String(), "http=502") {
			t.Errorf("stderr = %q, want it to contain http=502", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "error=") {
			t.Errorf("stderr still uses the generic error= key: %q", logBuf.String())
		}
	})

	t.Run("transport", func(t *testing.T) {
		stub := newAppriseStub(t, 200)
		stub.srv.Close()
		cfg := notifyTestConfig(t, stub, nil)
		var logBuf bytes.Buffer
		logWriter = &logBuf
		defer func() { logWriter = nil }()

		Send(context.Background(), cfg, r, false)
		if !strings.Contains(logBuf.String(), "transport=") {
			t.Errorf("stderr = %q, want it to contain transport=", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "error=") {
			t.Errorf("stderr still uses the generic error= key: %q", logBuf.String())
		}
	})
}

// TestAppriseKeyNeverLeaks asserts APPRISE_KEY cannot leak. It sits in
// the URL path of every apprise request, so both a deliberate message
// (the 204 case) and an incidental one (a *url.Error's Error() embeds the
// full request URL) can leak it into a returned error or a log line,
// C7 / N.3.5 require redaction at every site that logs, wraps, or
// returns an error that can reach a caller. A distinctive
// non-default key is required: the default ("sentinel") is indistinguishable
// from ordinary log text and would make this test pass vacuously.
func TestAppriseKeyNeverLeaks(t *testing.T) {
	const secretKey = "kEyThatMustNotLeak9999"
	r := loadFixture(t, "report-ok.json")

	cases := []struct {
		name       string
		key        string
		makeConfig func(t *testing.T) *config.Config
	}{
		{"204", secretKey, func(t *testing.T) *config.Config {
			return notifyTestConfig(t, newAppriseStub(t, 204), nil)
		}},
		{"non-2xx", secretKey, func(t *testing.T) *config.Config {
			return notifyTestConfig(t, newAppriseStub(t, 502), nil)
		}},
		{"transport refused", secretKey, func(t *testing.T) *config.Config {
			stub := newAppriseStub(t, 200)
			stub.srv.Close()
			return notifyTestConfig(t, stub, nil)
		}},
		// Round 2 review: net/url percent-encodes the key into a
		// *url.Error's Error() text, so a literal string match alone only
		// catches keys with no character Go escapes. These two exercise
		// exactly the encoding this component must not leak.
		{"transport refused, key with space", "key with space 9999", func(t *testing.T) *config.Config {
			stub := newAppriseStub(t, 200)
			stub.srv.Close()
			return notifyTestConfig(t, stub, nil)
		}},
		{"transport refused, non-ASCII key", "schlüssel9999", func(t *testing.T) *config.Config {
			stub := newAppriseStub(t, 200)
			stub.srv.Close()
			return notifyTestConfig(t, stub, nil)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.makeConfig(t)
			cfg.AppriseKey = c.key

			var logBuf bytes.Buffer
			logWriter = &logBuf
			defer func() { logWriter = nil }()

			err := Send(context.Background(), cfg, r, false)
			if err == nil {
				t.Fatal("expected a delivery error")
			}
			// Check the key itself AND its percent-encoded form, the
			// encoded form is what a *url.Error actually prints, and is
			// the exact gap a plain literal-match redactor misses.
			encoded := (&url.URL{Path: c.key}).EscapedPath()
			if strings.Contains(err.Error(), c.key) || strings.Contains(err.Error(), encoded) {
				t.Errorf("returned error leaks APPRISE_KEY (raw or percent-encoded): %q", err.Error())
			}
			if strings.Contains(logBuf.String(), c.key) || strings.Contains(logBuf.String(), encoded) {
				t.Errorf("stderr log leaks APPRISE_KEY (raw or percent-encoded): %q", logBuf.String())
			}
		})
	}

	t.Run("seed-config transport", func(t *testing.T) {
		dir := t.TempDir()
		cfgFile := filepath.Join(dir, "sentinel.cfg")
		if err := os.WriteFile(cfgFile, []byte("tgram://x/y\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SENTINEL_HOSTNAME", "bam")
		t.Setenv("APPRISE_URL", "http://127.0.0.1:1")
		t.Setenv("APPRISE_KEY", secretKey)
		t.Setenv("APPRISE_CONFIG_FILE", cfgFile)
		t.Setenv("MAILRISE_USER", "u")
		t.Setenv("MAILRISE_PASS", "p")
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		_, serr := SeedConfig(context.Background(), cfg)
		if serr == nil {
			t.Fatal("expected a transport error against an unreachable apprise")
		}
		if strings.Contains(serr.Error(), secretKey) {
			t.Errorf("SeedConfig error leaks APPRISE_KEY: %q", serr.Error())
		}
	})
}

// --- 11: TestSMTPFallback ---

func TestSMTPFallback(t *testing.T) {
	appriseStubSrv := newAppriseStub(t, 200)
	smtp := newSMTPStub(t)
	cfg := notifyTestConfig(t, appriseStubSrv, smtp)
	r := loadFixture(t, "report-ok.json")

	if err := Send(context.Background(), cfg, r, true); err != nil {
		t.Fatalf("Send(smtpFallback=true): %v", err)
	}
	smtp.mu.Lock()
	sawAuth, rcptTo, dataText := smtp.sawAuth, smtp.rcptTo, smtp.dataText
	smtp.mu.Unlock()

	if !sawAuth {
		t.Error("SMTP stub never saw AUTH")
	}
	if !strings.Contains(rcptTo, "<omv@mailrise.xyz>") {
		t.Errorf("RCPT TO = %q, want it to carry <omv@mailrise.xyz>", rcptTo)
	}
	payload := BuildPayload(r, cfg)
	if !strings.Contains(dataText, "Subject:") || !strings.Contains(dataText, subjectEncodedForTest(payload.Title)) {
		t.Errorf("DATA block missing the title-carrying Subject: %q", dataText)
	}
	if appriseStubSrv.count() != 0 {
		t.Errorf("apprise stub received %d requests, want 0 (SMTP fallback must not also POST)", appriseStubSrv.count())
	}

	t.Run("unconfigured", func(t *testing.T) {
		t.Setenv("MAILRISE_PASS", "")
		cfg2, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		err = Send(context.Background(), cfg2, r, true)
		if err == nil || err.Error() != "smtp fallback unconfigured" {
			t.Fatalf("err = %v, want exactly %q", err, "smtp fallback unconfigured")
		}
		if !errors.Is(err, ErrSend) {
			t.Error("unconfigured smtp fallback must still be ErrSend")
		}
	})
}

// TestSMTPBody_NoMarkdown is 31cec31 item 1, end-to-end through Send: the
// DATA block must be the plain-text body (N.3.6), never payload.Body, the
// cksum fixture's markdown-heavy content (**Findings**, _Analysis:_,
// backtick-wrapped evidence) is exactly what a real mailrise message
// forwarded literally before this fix.
func TestSMTPBody_HTML(t *testing.T) {
	appriseStubSrv := newAppriseStub(t, 200)
	smtp := newSMTPStub(t)
	cfg := notifyTestConfig(t, appriseStubSrv, smtp)
	r := loadFixture(t, "report-watch-zfs-cksum.json")

	if err := Send(context.Background(), cfg, r, true); err != nil {
		t.Fatalf("Send(smtpFallback=true): %v", err)
	}
	smtp.mu.Lock()
	dataText := smtp.dataText
	smtp.mu.Unlock()

	// 2593e07: the SMTP path now sends text/html, not text/plain, mailrise
	// selects the notification format from Content-Type.
	if !strings.Contains(dataText, "Content-Type: text/html; charset=utf-8") {
		t.Errorf("SMTP message is not Content-Type: text/html: %s", dataText)
	}
	for _, bad := range []string{"**Findings**", "_Analysis:_", "_Recommendation:_", "**WATCH"} {
		if strings.Contains(dataText, bad) {
			t.Errorf("SMTP DATA block still carries markdown syntax %q: %s", bad, dataText)
		}
	}
	if !strings.Contains(dataText, "<b>Evidence:</b>") {
		t.Errorf("SMTP DATA block missing the HTML Evidence heading: %s", dataText)
	}
	want := BuildHTMLBody(cfg, r)
	// net/smtp's DATA writer canonicalizes line endings to CRLF; compare
	// content, not wire line-ending format.
	normalized := strings.ReplaceAll(dataText, "\r\n", "\n")
	if !strings.Contains(normalized, want) {
		t.Errorf("SMTP DATA block does not carry BuildHTMLBody's output: got %q want substring %q", normalized, want)
	}
}

// TestSMTPBody_StripsUnsafeButKeepsFidelity is the 0bdf468 amendment: the
// HTML path must still drop invalid UTF-8 and control characters the way
// Sanitize always did for the markdown path, html.EscapeString alone
// only handles the five XML entities, not NUL/BEL/invalid UTF-8. RFC 5321 §2.3.1
// forbids NUL in SMTP DATA, and a message declaring charset=utf-8 must
// not carry invalid UTF-8. This is checked in the SAME test as the
// fidelity guarantee (cksum_errors, <mce>, A&B surviving as escaped
// text) so neither property can be "fixed" by breaking the other.
func TestSMTPBody_StripsUnsafeButKeepsFidelity(t *testing.T) {
	appriseStubSrv := newAppriseStub(t, 200)
	smtp := newSMTPStub(t)
	cfg := notifyTestConfig(t, appriseStubSrv, smtp)

	evidence := "cksum_errors=1 <mce> A&B \x00NUL\x07BEL" + string([]byte{0xff, 0xfe}) + "invalid-utf8"
	r := report.Report{
		Status: "ALERT", Headline: "h", Body: "b",
		Findings: []report.Finding{{Severity: "alert", Component: "kernel", Evidence: evidence, Explanation: "exp"}},
		Resolved: []string{},
	}

	if err := Send(context.Background(), cfg, r, true); err != nil {
		t.Fatalf("Send(smtpFallback=true): %v", err)
	}
	smtp.mu.Lock()
	dataText := smtp.dataText
	smtp.mu.Unlock()

	// --- stripping half ---
	if !utf8.ValidString(dataText) {
		t.Errorf("SMTP message body is NOT valid UTF-8 (declared charset=utf-8): %q", dataText)
	}
	if strings.Contains(dataText, "\x00") {
		t.Error("SMTP DATA block still carries a NUL byte (RFC 5321 §2.3.1 forbids NUL in DATA)")
	}
	for _, r := range dataText {
		if r != '\n' && r != '\r' && unicode.IsControl(r) {
			t.Errorf("SMTP DATA block carries a control character other than CR/LF: %q (%U)", r, r)
		}
	}

	// --- fidelity half, same test ---
	for _, want := range []string{"cksum_errors=1", "&lt;mce&gt;", "A&amp;B"} {
		if !strings.Contains(dataText, want) {
			t.Errorf("SMTP DATA block lost fidelity, missing %q: %q", want, dataText)
		}
	}
}

func subjectEncodedForTest(title string) string {
	// ASCII-only titles pass through mime.QEncoding unencoded; every fixture
	// title used here is ASCII, so a plain substring check is meaningful.
	return title
}

// --- 12: TestNoWrites ---

func TestNoWrites(t *testing.T) {
	root := t.TempDir()
	before := snapshotTree(t, root)

	stub := newAppriseStub(t, 200)
	smtp := newSMTPStub(t)
	cfg := notifyTestConfig(t, stub, smtp)
	r := loadFixture(t, "report-ok.json")

	if err := Send(context.Background(), cfg, r, false); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), cfg, r, true); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if _, err := Run(context.Background(), cfg, []string{"--dry-run"}, bytes.NewReader(readFixtureBytes(t, "report-ok.json")), &stdout); err != nil {
		t.Fatal(err)
	}

	after := snapshotTree(t, root)
	if before != after {
		t.Errorf("test root changed:\nbefore=%v\nafter=%v", before, after)
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(namesOf(entries), ",")
}

func namesOf(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

// --- 13: TestRetryByteIdentical ---

func TestRetryByteIdentical(t *testing.T) {
	cfg := notifyTestConfig(t, newAppriseStub(t, 200), nil)
	raw := readFixtureBytes(t, "report-watch-zfs-cksum.json")

	var r1, r2 report.Report
	if err := json.Unmarshal(raw, &r1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &r2); err != nil {
		t.Fatal(err)
	}
	p1 := BuildPayload(r1, cfg)
	p2 := BuildPayload(r2, cfg)
	b1, _ := json.Marshal(p1)
	b2, _ := json.Marshal(p2)
	if !bytes.Equal(b1, b2) {
		t.Errorf("rendering the same bytes twice produced different payloads:\n%s\n%s", b1, b2)
	}
}

// --- 14: TestRawAlertRoundTrip ---

func TestRawAlertRoundTrip(t *testing.T) {
	cfg := notifyTestConfig(t, newAppriseStub(t, 200), nil)

	longMsg := strings.Repeat("kernel panic detail segment ", 150) // ~4000+ runes
	longMsg = longMsg[:4000]
	body := "Raw kernel alert, sent without analysis (LLM-free path).\n\n" +
		time.Now().UTC().Format(time.RFC3339) + " crit " + longMsg + "\x07" + string([]byte{0xff, 0xfe}) +
		"\n\nA full analysis follows in the next report if the analyzer is available."

	rawReport := report.Report{
		Status:   "ALERT",
		Headline: "1 critical kernel event(s) on bam",
		Body:     body,
		Findings: []report.Finding{{
			Severity: "alert", Component: "kernel",
			Evidence:    longMsg + "\x07",
			Explanation: "Kernel logged a priority-2 (crit) message. Sent unanalysed on the LLM-free critical path.",
			Key:         "3f9a1c7d0b2e4551",
		}},
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: "bam", TickSeq: 412, Raw: true},
	}
	if err := Validate(rawReport); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	p := BuildPayload(rawReport, cfg)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("payload did not marshal: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if runes := len([]rune(p.Body)); runes > cfg.NotifyBodyMax+20 {
		t.Errorf("body is %d runes, want within NotifyBodyMax (%d) + truncation suffix", runes, cfg.NotifyBodyMax)
	}
}

// --- 15: TestSeedConfig ---

func TestSeedConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "sentinel.cfg")
	if err := os.WriteFile(cfgFile, []byte("# comment\ntgram://token/chatid\n\ntgram://token2/chatid2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	t.Setenv("SENTINEL_HOSTNAME", "bam")
	t.Setenv("APPRISE_URL", srv.URL)
	t.Setenv("APPRISE_KEY", "sentinel")
	t.Setenv("APPRISE_CONFIG_FILE", cfgFile)
	t.Setenv("MAILRISE_USER", "u")
	t.Setenv("MAILRISE_PASS", "p")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	urls, err := SeedConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("SeedConfig: %v", err)
	}
	if urls != 2 {
		t.Errorf("urls = %d, want 2", urls)
	}
	if gotPath != "/add/sentinel" {
		t.Errorf("path = %q, want /add/sentinel", gotPath)
	}
	if !bytes.Contains(gotBody, []byte("tgram://token/chatid")) {
		t.Errorf("multipart body did not carry the config file verbatim: %q", gotBody)
	}

	srv.Close()
	if _, err := SeedConfig(context.Background(), cfg); err == nil {
		t.Error("SeedConfig against a closed server must error")
	}
}

// TestSeedConfig_204IsFailure asserts N.3.1's 204 rule ("the key was not
// registered") applies to /add/{key} exactly as it does to /notify/{key}
// , a 204 there means apprise did NOT accept the config, the one outcome
// SeedConfig exists to prevent silently.
func TestSeedConfig_204IsFailure(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "sentinel.cfg")
	if err := os.WriteFile(cfgFile, []byte("tgram://token/chatid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("SENTINEL_HOSTNAME", "bam")
	t.Setenv("APPRISE_URL", srv.URL)
	t.Setenv("APPRISE_KEY", "sentinel")
	t.Setenv("APPRISE_CONFIG_FILE", cfgFile)
	t.Setenv("MAILRISE_USER", "u")
	t.Setenv("MAILRISE_PASS", "p")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := SeedConfig(context.Background(), cfg); err == nil {
		t.Error("SeedConfig must treat a 204 response as failure, not success")
	}
}

// --- 18: TestE2E (gated), see e2e_test.go for the full test body.

// A server that accepts the TCP connection and never sends its SMTP
// greeting must not hold Send: the dial timeout is satisfied the moment
// the connection opens, so only a deadline on the connection itself
// bounds the conversation. Without it the outbox drain, and with it the
// whole tick, would block for as long as mailrise stays wedged.
func TestSMTPFallback_HungServerIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Deliberately deferred inside the loop: every accepted
			// connection must stay open, unanswered, until the test ends.
			defer c.Close()
		}
	}()

	appriseStubSrv := newAppriseStub(t, 200)
	cfg := notifyTestConfig(t, appriseStubSrv, newSMTPStub(t))
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg.MailriseHost = host
	cfg.MailrisePort, _ = strconv.Atoi(port)
	cfg.NotifyTimeout = time.Second
	r := loadFixture(t, "report-ok.json")

	// Send runs in a goroutine so a regression fails here, in seconds,
	// rather than hanging the whole package to its test timeout.
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- Send(context.Background(), cfg, r, true) }()
	select {
	case err = <-done:
	case <-time.After(cfg.NotifyTimeout + 3*time.Second):
		t.Fatalf("Send did not return within NOTIFY_TIMEOUT+3s against a hung SMTP server: the conversation is unbounded")
	}
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send against a hung SMTP server returned nil, want an error")
	}
	// Dial and conversation share one deadline, so the whole call is
	// bounded by NOTIFY_TIMEOUT itself, not by two of them in sequence.
	if elapsed > cfg.NotifyTimeout+time.Second {
		t.Fatalf("Send took %v against a hung SMTP server, want at most NOTIFY_TIMEOUT=%v plus scheduling margin", elapsed, cfg.NotifyTimeout)
	}
}

// The dial and the conversation share ONE deadline. With a dial that
// consumes most of NOTIFY_TIMEOUT, a conversation deadline started after
// the dial would allow close to 2 x NOTIFY_TIMEOUT in total. A shared
// deadline ends the whole call at NOTIFY_TIMEOUT. The liveness window
// (C4) counts one NOTIFY_TIMEOUT per outbox item, so the difference is
// the difference between that derivation being true and false.
func TestSMTPFallback_DialAndConversationShareOneDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never greeted, same as above
		}
	}()

	const dialCost = 700 * time.Millisecond
	smtpDialControl = func(network, address string, c syscall.RawConn) error {
		time.Sleep(dialCost)
		return nil
	}
	defer func() { smtpDialControl = nil }()

	appriseStubSrv := newAppriseStub(t, 200)
	cfg := notifyTestConfig(t, appriseStubSrv, newSMTPStub(t))
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg.MailriseHost = host
	cfg.MailrisePort, _ = strconv.Atoi(port)
	cfg.NotifyTimeout = time.Second
	r := loadFixture(t, "report-ok.json")

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- Send(context.Background(), cfg, r, true) }()
	select {
	case err = <-done:
	case <-time.After(cfg.NotifyTimeout + 3*time.Second):
		t.Fatal("Send did not return: the conversation is unbounded")
	}
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send returned nil against a server that never greets")
	}
	// Shared deadline: ~1.0 s. Sequential deadlines: dial 0.7 s + a fresh
	// 1.0 s for the conversation = ~1.7 s. The bound sits between them.
	if elapsed > cfg.NotifyTimeout+300*time.Millisecond {
		t.Fatalf("Send took %v: dial and conversation are not sharing one NOTIFY_TIMEOUT deadline", elapsed)
	}
}

// A cancelled context ends a conversation blocked in I/O at once, not at
// NOTIFY_TIMEOUT: shutdown gives an active tick five seconds and then
// cancels, and an outbox drain must honor that.
func TestSMTPFallback_CancelInterruptsConversation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never greeted
		}
	}()

	appriseStubSrv := newAppriseStub(t, 200)
	cfg := notifyTestConfig(t, appriseStubSrv, newSMTPStub(t))
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg.MailriseHost = host
	cfg.MailrisePort, _ = strconv.Atoi(port)
	cfg.NotifyTimeout = 10 * time.Second
	r := loadFixture(t, "report-ok.json")

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- Send(ctx, cfg, r, true) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return within 3s of ctx cancellation: cancellation does not reach the SMTP conversation")
	}
	if err == nil {
		t.Fatal("Send returned nil after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Send took %v after a cancel at 300ms, want well under NOTIFY_TIMEOUT=%v", elapsed, cfg.NotifyTimeout)
	}
}
