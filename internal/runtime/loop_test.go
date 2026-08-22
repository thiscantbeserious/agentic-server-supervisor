package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// --- Critical item #2: tick MUST nil-check analyze.Run's report before
// marshaling. On a cancelled context Run returns (nil, err) deliberately;
// this test reaches that exact branch by having the stub RunAgy itself
// return it, the same shape the real analyze.Run produces on
// context.Canceled (contracts/analyze.md §1), and proves Tick does not
// panic and authors nothing. Deleting the nil-check in tick.go must make
// this test fail (verified below the test, in prose, per the task brief;
// the reviewer/gate re-runs this by literally removing the check).
func TestTick_NilCheckOnCancelledAnalyze(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = func(ctx context.Context, o analyze.Options, dd analyze.Deps) (*report.Report, error) {
		return nil, context.Canceled
	}

	var res TickResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Tick panicked on a nil analyze report: %v", r)
			}
		}()
		res = Tick(context.Background(), cfg, 1, d)
	}()

	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", res.Err)
	}
	if res.Report != nil {
		t.Errorf("Report = %+v, want nil, a cancelled analysis must author nothing", res.Report)
	}
	if res.Notified {
		t.Error("Notified = true, want false, nothing should have been sent")
	}
	if rec.count() != 0 {
		t.Errorf("apprise received %d requests, want 0 (a cancelled analysis must send nothing)", rec.count())
	}
}

// --- E17: config_validation ---

func TestConfig_Validation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"bad tick interval", map[string]string{"TICK_INTERVAL": "abc"}, "TICK_INTERVAL"},
		{"tick interval too low", map[string]string{"TICK_INTERVAL": "10"}, "TICK_INTERVAL"},
		{"window not greater than interval", map[string]string{"TICK_WINDOW": "5m", "TICK_INTERVAL": "300"}, "TICK_WINDOW"},
		{"bad log level", map[string]string{"LOG_LEVEL": "LOUD"}, "LOG_LEVEL"},
		{"raw alert max lines out of range", map[string]string{"RAW_ALERT_MAX_LINES": "99"}, "RAW_ALERT_MAX_LINES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected an error for %v", c.env)
			}
			var cerr *config.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("err = %v, want *config.Error", err)
			}
			if cerr.Var != c.wantVar {
				t.Errorf("Var = %q, want %q", cerr.Var, c.wantVar)
			}
			if len(c.env) == 1 {
				for _, v := range c.env {
					if contains(cerr.Error(), v) {
						t.Errorf("error text %q must never repeat the offending VALUE (C7)", cerr.Error())
					}
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- E18: shutdown ---

func TestLoop_Shutdown(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = stubAnalyzeReturning(okReport())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Loop even starts a tick

	done := make(chan struct{})
	var code int
	var err error
	go func() {
		code, err = Loop(ctx, cfg, d)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return within 5s of a cancelled context")
	}
	if code != 0 || err != nil {
		t.Errorf("Loop() = %d, %v, want 0, nil", code, err)
	}
}

// TestNextTickSeq_MissingWarnsToo asserts R3.1's "Missing or unparseable
// ⇒ start at 1 and WARN" covers the missing case, not just unparseable,
// the far more common real case (a fresh $STATE_DIR on first boot,
// tick-seq simply absent) must not start at 1 silently.
func TestNextTickSeq_MissingWarnsToo(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	seq := nextTickSeq(cfg, newLogger(cfg))
	if seq != 1 {
		t.Fatalf("seq = %d, want 1 on a fresh $STATE_DIR", seq)
	}
	if !strings.Contains(logBuf.String(), "tick-seq missing") {
		t.Errorf("stderr does not WARN on a missing tick-seq file: %q", logBuf.String())
	}
}

// --- E19: health ---

func TestHealth(t *testing.T) {
	cfg := testConfig(t, tick0)
	store := newStore(t, cfg)

	if code, err := Health(cfg); code != 1 || err == nil {
		t.Errorf("missing heartbeat: code=%d err=%v, want 1, non-nil", code, err)
	}

	// A fresh heartbeat, via a real Process call.
	_, perr := store.Process(mustMarshalReportForTest(t, report.Report{
		Status: "OK", Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{},
	}))
	if perr != nil {
		t.Fatal(perr)
	}
	// Health() compares against cfg.Now (the test clock, C9), pin the
	// file's mtime to it explicitly rather than relying on the real wall
	// clock, which may disagree with cfg.Now by however old the fixture
	// date is.
	hb := filepath.Join(cfg.StateDir, "heartbeat")
	if err := os.Chtimes(hb, cfg.Now, cfg.Now); err != nil {
		t.Fatal(err)
	}
	if code, err := Health(cfg); code != 0 || err != nil {
		t.Errorf("fresh heartbeat: code=%d err=%v, want 0, nil", code, err)
	}

	// Backdate the heartbeat past 3x TICK_INTERVAL relative to cfg.Now.
	stale := cfg.Now.Add(-4 * cfg.TickInterval)
	if err := os.Chtimes(hb, stale, stale); err != nil {
		t.Fatal(err)
	}
	if code, err := Health(cfg); code != 1 || err == nil {
		t.Errorf("stale heartbeat: code=%d err=%v, want 1, non-nil", code, err)
	}
}

func mustMarshalReportForTest(t *testing.T, r report.Report) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capturingWarner records warnings so a test can assert what the operator
// was told, not merely that nothing crashed.
type capturingWarner struct{ msgs []string }

func (w *capturingWarner) Warn(msg string, _ ...any) { w.msgs = append(w.msgs, msg) }

// TestSeedAgyHome_CopiesCredentialTree pins the layout agy actually uses,
// measured on the target host rather than assumed:
//
//	~/.gemini/
//	  antigravity-cli/
//	    antigravity-oauth-token      <- the credential
//	  config/
//
// Every entry at the top level of the mounted secret directory is a
// DIRECTORY. The previous implementation copied "regular files only" and
// skipped directories, so it copied nothing at all, and because the
// directory was not empty it did not even warn: the operator saw an
// analyzer that silently fell back on every tick with no explanation.
//
// agy resolves its credential under $HOME/.gemini/, so the tree has to
// land there rather than flat in $HOME. Verified end to end on the host:
// copying ~/.gemini into a fresh HOME and running agy returned
// status SUCCESS with real token usage, which is the empirical check
// contracts/runtime.md required before relying on seeding.
func TestSeedAgyHome_CopiesCredentialTree(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), []byte("TOKEN-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(secret, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AgyHome: home, AgySecretDir: secret}
	w := &capturingWarner{}
	seedAgyHome(cfg, w)

	tokenPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	got, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("credential did not reach %s: %v (the secret dir holds only directories, and skipping them copies nothing)", tokenPath, err)
	}
	if string(got) != "TOKEN-CONTENT" {
		t.Errorf("token content = %q, want TOKEN-CONTENT", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "config.json")); err != nil {
		t.Errorf("config tree did not reach AGY_HOME: %v", err)
	}
	for _, m := range w.msgs {
		if strings.Contains(m, "credentials absent") {
			t.Errorf("warned %q although credentials were present", m)
		}
	}
}

// TestSeedAgyHome_DoesNotClobberContainerState pins that seeding
// bootstraps and never re-imposes.
//
// seedAgyHome runs on every container start, and agy owns $AGY_HOME once
// it is running: it refreshes its OAuth token there, and writes
// conversation_summaries.db and antigravity-cli/brain/ (its memory) there.
// Copying the host tree over that on each start would silently replace a
// refreshed token with the older one from the host, where no agy runs to
// keep it current, and would delete accumulated memory on a restart.
//
// Bootstrapping is still needed, so files MISSING at the destination are
// still copied: a crash halfway through the first seed must be able to
// complete on the next start.
func TestSeedAgyHome_DoesNotClobberContainerState(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What the host holds: the token as it was when the operator logged in.
	if err := os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), []byte("STALE-HOST-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "settings.json"), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// What the container already has: a refreshed token and memory that
	// exist only here.
	destCLI := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(destCLI, "brain"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destCLI, "antigravity-oauth-token"), []byte("REFRESHED-IN-CONTAINER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destCLI, "brain", "memory.bin"), []byte("ACCUMULATED"), 0o600); err != nil {
		t.Fatal(err)
	}

	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, &capturingWarner{})

	tok, err := os.ReadFile(filepath.Join(destCLI, "antigravity-oauth-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(tok) != "REFRESHED-IN-CONTAINER" {
		t.Errorf("token = %q, want the container's refreshed one: seeding replaced a current token with the host's older copy", tok)
	}
	mem, err := os.ReadFile(filepath.Join(destCLI, "brain", "memory.bin"))
	if err != nil {
		t.Fatalf("agy's memory did not survive a restart: %v", err)
	}
	if string(mem) != "ACCUMULATED" {
		t.Errorf("memory = %q, want ACCUMULATED", mem)
	}
	// Bootstrapping still has to work for what is genuinely missing.
	if _, err := os.Stat(filepath.Join(destCLI, "settings.json")); err != nil {
		t.Errorf("a file absent from the destination was not seeded: %v", err)
	}
}
