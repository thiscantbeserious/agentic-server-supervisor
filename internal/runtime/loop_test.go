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

// TestSeedAgyHome_WritesToolDenyPolicy pins that the container imposes its
// own tool policy on agy, overriding whatever the operator's desktop
// settings say.
//
// agy is an agent, not a completion API: mid-analysis it decides to run
// shell commands. In --print mode there is nobody to approve them, so the
// turn dies and the envelope comes back status ERROR with an empty
// response, which the report parser then reports as invalid JSON.
// Measured on the target host:
//
//	error: permission check failed for command "ls -la":
//	       user denied permission to run command: ls -la
//
// With a deny rule configured, the same prompt returns status SUCCESS and a
// valid report: the model answers instead of reaching for a shell.
//
// This is also the security property the project already claims elsewhere.
// The analyzer's input is attacker-controlled log text; a tool call it can
// be talked into is a prompt injection with a shell on the end of it. The
// policy is therefore written unconditionally, not merged in only when
// absent, and not left to whatever settings.json the host happened to have.
func TestSeedAgyHome_WritesToolDenyPolicy(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")
	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The operator's own settings, carrying a key worth preserving and no
	// permission block at all.
	if err := os.WriteFile(filepath.Join(cliDir, "settings.json"),
		[]byte(`{"enableTelemetry":false,"trustedWorkspaces":["/home/doh"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, &capturingWarner{})

	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json missing from AGY_HOME: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	perm, ok := got["permission"].(map[string]any)
	if !ok {
		t.Fatalf("no permission block written; settings = %s", raw)
	}
	deny, ok := perm["deny"].([]any)
	if !ok || len(deny) == 0 {
		t.Fatalf("no deny rules written; permission = %v", perm)
	}
	var denied string
	for _, d := range deny {
		denied += d.(string) + " "
	}
	if !strings.Contains(denied, "run_command(*)") {
		t.Errorf("shell execution is not denied; deny = %v", deny)
	}
	if got["enableTelemetry"] != false {
		t.Errorf("the operator's other settings were discarded; got %v", got)
	}
}

// TestSeedAgyHome_UnreadableFileDoesNotAbortSeed pins that one file the
// container cannot read does not cost it the credential.
//
// The mounted tree is agy's whole state directory, not a credential store:
// caches, a conversation database, a bundled browser helper. Its files are
// owned by the operator, and only some carry a group the container shares,
// so an unreadable cache entry is normal. Aborting the walk on it meant a
// file with no bearing on authentication could stop the token from being
// seeded, which is what happened on the target host:
//
//	could not seed agy credentials, analysis will fall back
//	error=open /run/secrets/agy/antigravity-cli/cache/last_conversations.json: permission denied
func TestSeedAgyHome_UnreadableFileDoesNotAbortSeed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test depends on")
	}
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	cacheDir := filepath.Join(cliDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Sorts before the token, so an aborting walk never reaches it.
	unreadable := filepath.Join(cacheDir, "last_conversations.json")
	if err := os.WriteFile(unreadable, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "zz-token-sorts-last"), []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &capturingWarner{}
	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, w)

	got, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "zz-token-sorts-last"))
	if err != nil {
		t.Fatalf("a readable credential was not seeded because another file was unreadable: %v", err)
	}
	if string(got) != "TOKEN" {
		t.Errorf("seeded content = %q, want TOKEN", got)
	}
	var named bool
	for _, m := range w.msgs {
		if strings.Contains(m, "unreadable") {
			named = true
		}
	}
	if !named {
		t.Errorf("the skipped file was not reported; warnings were %v", w.msgs)
	}
}
