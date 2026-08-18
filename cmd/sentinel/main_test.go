package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sharedBin/buildOnce compile the binary exactly once for the whole test
// binary run (not once per test function) into a package-level temp dir —
// buildSentinel used to shell out to "go build" from 13 separate test
// functions, which is the same compile repeated 13 times for no reason.
var (
	buildOnce sync.Once
	sharedBin string
	buildErr  error
)

// buildSentinel returns the path to the sentinel binary, compiling it on
// the first call only. Table-driven CLI tests run it as a subprocess so
// they exercise the real os.Exit / exit-code mapping (C2) end to end.
func buildSentinel(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "sentinel-bin-")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(dir, "sentinel")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = wd
		var out []byte
		out, buildErr = cmd.CombinedOutput()
		if buildErr != nil {
			buildErr = fmt.Errorf("go build failed: %w\n%s", buildErr, out)
			return
		}
		sharedBin = bin
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return sharedBin
}

func runBin(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// baseEnv builds a hermetic child environment (C9): only PATH/HOME (needed
// to exec the binary) plus the C3 vars each test cares about. It must NOT
// inherit the test process's ambient environment — a stray TICK_WINDOW,
// LOG_LEVEL, TZ or SENTINEL_NOW on a dev machine or CI runner would
// silently change what these subprocess tests exercise.
func baseEnv(t *testing.T, stateDir string) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"STATE_DIR=" + stateDir,
		"HOST_ROOT=" + t.TempDir(),
		"HOST_PROC=" + t.TempDir(),
	}
}

// N9: baseEnv must not leak the test process's ambient environment into
// the subprocess — an ambient LOG_LEVEL (or TICK_WINDOW, TZ, SENTINEL_NOW)
// on a dev machine or CI runner must not change what the child sees.
func TestBaseEnvIsHermetic(t *testing.T) {
	t.Setenv("LOG_LEVEL", "BOGUS-AMBIENT-VALUE")
	bin := buildSentinel(t)
	_, stderr, code := runBin(t, bin, baseEnv(t, t.TempDir()), "health")
	if code == 78 {
		t.Fatalf("ambient LOG_LEVEL leaked into the hermetic subprocess env, stderr=%q", stderr)
	}
}

func TestVersion(t *testing.T) {
	bin := buildSentinel(t)
	stdout, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "--version")
	if code != 0 {
		t.Fatalf("--version exit code = %d, want 0", code)
	}
	if stdout == "" {
		t.Fatal("--version produced no output")
	}
}

func TestNoArgs(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()))
	if code != 64 {
		t.Fatalf("no args exit code = %d, want 64", code)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "bogus")
	if code != 64 {
		t.Fatalf("unknown subcommand exit code = %d, want 64", code)
	}
}

func TestCollectUnknownFlag(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "collect", "--nope")
	if code != 64 {
		t.Fatalf("collect --nope exit code = %d, want 64", code)
	}
}

func TestCollectBadDeepValue(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "collect", "--deep", "bogus")
	if code != 64 {
		t.Fatalf("collect --deep bogus exit code = %d, want 64", code)
	}
}

// D8: a usage error must say only that — not a correct usage diagnostic
// followed by a false "not yet implemented" claim about the subcommand.
func TestUsageErrorsDoNotClaimNotImplemented(t *testing.T) {
	bin := buildSentinel(t)
	cases := [][]string{
		{"collect", "--deep", "bogus"},
		{"tick", "--loop", "--once"},
		{"tick", "extra-positional"},
		{"state", "bogus"},
		{"state", "outbox-ack"},
		{"notify", "a", "b"},
		{"health", "extra-positional"},
		{"analyze", "extra-positional"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, stderr, code := runBin(t, bin, baseEnv(t, t.TempDir()), args...)
			if code != 64 {
				t.Fatalf("%v exit code = %d, want 64", args, code)
			}
			if strings.Contains(stderr, "not yet implemented") {
				t.Fatalf("%v: usage error must not also claim \"not yet implemented\", stderr=%q", args, stderr)
			}
		})
	}
}

func TestCollectDeepValidValueParsesButIsUnimplemented(t *testing.T) {
	bin := buildSentinel(t)
	_, stderr, code := runBin(t, bin, baseEnv(t, t.TempDir()), "collect", "--deep", "zfs")
	if code == 64 {
		t.Fatalf("collect --deep zfs should parse (not a usage error), got exit 64, stderr=%s", stderr)
	}
}

func TestTickLoopAndOnceMutuallyExclusive(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "tick", "--loop", "--once")
	if code != 64 {
		t.Fatalf("tick --loop --once exit code = %d, want 64", code)
	}
}

func TestTickPositionalArgRejected(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "tick", "extra-positional")
	if code != 64 {
		t.Fatalf("tick with a positional arg exit code = %d, want 64", code)
	}
}

func TestStateUnknownSubcommand(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "state", "bogus")
	if code != 64 {
		t.Fatalf("state bogus exit code = %d, want 64", code)
	}
}

func TestStateOutboxAckRequiresID(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "state", "outbox-ack")
	if code != 64 {
		t.Fatalf("state outbox-ack (no id) exit code = %d, want 64", code)
	}
}

func TestNotifyUnknownFlag(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "notify", "--nope")
	if code != 64 {
		t.Fatalf("notify --nope exit code = %d, want 64", code)
	}
}

func TestHealthRunsAndDoesNotUsageError(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBin(t, bin, baseEnv(t, t.TempDir()), "health")
	if code == 64 {
		t.Fatalf("health should parse (no flags), got usage-error exit 64")
	}
}

func TestTickStateDirFlagOverridesEnv(t *testing.T) {
	bin := buildSentinel(t)
	env := append(baseEnv(t, t.TempDir()), "STATE_DIR=/env-dir")
	_, stderr, _ := runBin(t, bin, env, "tick", "--once", "--state-dir", "/flag-dir")
	if !strings.Contains(stderr, "/flag-dir") {
		t.Fatalf("stderr = %q, expected the --state-dir flag value /flag-dir to win over $STATE_DIR", stderr)
	}
	if strings.Contains(stderr, "/env-dir") {
		t.Fatalf("stderr = %q, --state-dir flag did not override $STATE_DIR", stderr)
	}
}

func TestTickWithoutStateDirFlagUsesEnv(t *testing.T) {
	bin := buildSentinel(t)
	env := append(baseEnv(t, t.TempDir()), "STATE_DIR=/env-dir")
	_, stderr, _ := runBin(t, bin, env, "tick", "--once")
	if !strings.Contains(stderr, "/env-dir") {
		t.Fatalf("stderr = %q, expected $STATE_DIR to be used when --state-dir is absent", stderr)
	}
}

// TestTickOnceRunsStartupPreflight is round-3 item 5: R2's startup
// sequence ("preflight, the read-only lint, agy-home seeding") is
// required "once before --loop starts ticking, AND once before the
// single tick in --once" — before this fix, main.go only ran it inside
// Loop(), so a --once invocation with neither journal directory mounted
// silently skipped a check --loop would have enforced. baseEnv gives a
// real, writable $STATE_DIR (so state.New succeeds) but never sets
// HOST_JOURNAL_DIR/HOST_JOURNAL_VOLATILE_DIR, so both fall back to their
// C3 defaults (/host/journal, /host/journal-volatile) — real container
// paths that do not exist on a dev/CI host, which is exactly the
// "neither journal directory is readable and non-empty" case StartupPreflight
// must catch.
func TestTickOnceRunsStartupPreflight(t *testing.T) {
	bin := buildSentinel(t)
	_, stderr, code := runBin(t, bin, baseEnv(t, t.TempDir()), "tick", "--once")
	if code != 78 {
		t.Fatalf("code = %d, stderr = %q, want 78 (StartupPreflight must run before a --once tick, not only inside Loop)", code, stderr)
	}
}

// fullTickEnv is baseEnv plus everything a REAL `tick --once` needs to run
// to completion against the real binary: a stubbed journalctl/sensors on
// PATH (testdata/bin, same self-contained stubs internal/collect and
// internal/runtime already use), a non-empty journal dir so
// StartupPreflight's readable-and-non-empty check passes, and a local
// httptest recorder standing in for apprise so notify.Send actually
// succeeds instead of timing out against a real network.
func fullTickEnv(t *testing.T, stateDir string) []string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	stubBin := filepath.Join(wd, "testdata", "bin")

	hostProc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostProc, "uptime"), []byte("1234.5 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	journalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(journalDir, ".keep"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	apprise := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(apprise.Close)

	return []string{
		"PATH=" + stubBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"STATE_DIR=" + stateDir,
		"TMPDIR=" + t.TempDir(),
		"HOST_ROOT=" + t.TempDir(),
		"HOST_PROC=" + hostProc,
		"HOST_JOURNAL_DIR=" + journalDir,
		"HOST_JOURNAL_VOLATILE_DIR=" + t.TempDir(),
		"HOST_RASDAEMON=" + t.TempDir(),
		"AGY_HOME=" + t.TempDir(),
		"AGY_SECRET_DIR=" + t.TempDir(),
		"AGY_BIN=agy-does-not-exist", // agy missing -> analyze falls back; tick must still complete
		"SENTINEL_HOSTNAME=bam",
		"APPRISE_URL=" + apprise.URL,
		"MAILRISE_USER=u",
		"MAILRISE_PASS=changeme", // never actually used (this test only exercises the apprise path); matches TestNoSecretsInRepo's placeholder exemption
	}
}

// TestTickOnceAllocatesTickSeq is main's ruling on t6-review's escalated
// defect: R3.1's tick-seq counter is scoped to the sentinel-tick COMMAND
// ("owned exclusively by tick"), not to --loop — the old code hardcoded
// seq=1 for every --once invocation, so an operator debugging against the
// live /state volume on bam would silently write seq-1 records into
// active-alerts the loop was tracking at seq 400+, corrupting the trend
// data analyze reads back out of history/. This MUST drive the real
// binary: internal/runtime's own table (E12) calls Tick() directly with a
// supplied seq, so the seam where main decides what to pass is invisible
// to every in-process test.
func TestTickOnceAllocatesTickSeq(t *testing.T) {
	bin := buildSentinel(t)
	stateDir := t.TempDir()
	env := fullTickEnv(t, stateDir)

	// agy isn't on PATH (AGY_BIN points nowhere), so analyze legitimately
	// falls back and the tick reports exit 3 (C2: "3 | analyze failed").
	// That's an expected, valid outcome here — a usage/config-error code
	// (64/65/69/78) would mean the run never reached a real tick at all,
	// which is what actually matters for this test.
	stdout1, stderr1, code1 := runBin(t, bin, env, "tick", "--once")
	if code1 == 64 || code1 == 65 || code1 == 69 || code1 == 78 {
		t.Fatalf("run 1: code = %d (usage/config error, tick never ran), stderr = %q", code1, stderr1)
	}
	seq1 := tickSeqFromStdout(t, stdout1)
	if seq1 != 1 {
		t.Fatalf("run 1: meta.tick_seq = %d, want 1 (stdout=%s)", seq1, stdout1)
	}

	stdout2, stderr2, code2 := runBin(t, bin, env, "tick", "--once")
	if code2 == 64 || code2 == 65 || code2 == 69 || code2 == 78 {
		t.Fatalf("run 2: code = %d (usage/config error, tick never ran), stderr = %q", code2, stderr2)
	}
	seq2 := tickSeqFromStdout(t, stdout2)
	if seq2 != 2 {
		t.Fatalf("run 2: meta.tick_seq = %d, want 2 — the counter must advance across separate --once invocations (stdout=%s)", seq2, stdout2)
	}

	if _, err := os.Stat(filepath.Join(stateDir, "tick-seq")); err != nil {
		t.Errorf("$STATE_DIR/tick-seq must exist after a --once run: %v", err)
	}
}

func tickSeqFromStdout(t *testing.T, stdout string) int64 {
	t.Helper()
	var doc struct {
		Meta struct {
			TickSeq int64 `json:"tick_seq"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &doc); err != nil {
		t.Fatalf("stdout is not the expected report JSON: %v (stdout=%q)", err, stdout)
	}
	return doc.Meta.TickSeq
}

func TestTZDataEmbedded(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "time/tzdata"`) {
		t.Fatal(`cmd/sentinel/main.go must blank-import "time/tzdata" so TZ resolution works in the ` +
			`debian-slim runtime image (CGO_ENABLED=0) without depending on system zoneinfo being present`)
	}
}

func TestNonUTCTimezoneLoads(t *testing.T) {
	bin := buildSentinel(t)
	env := append(baseEnv(t, t.TempDir()), "TZ=Europe/Berlin")
	_, stderr, code := runBin(t, bin, env, "health")
	if code == 78 {
		t.Fatalf("TZ=Europe/Berlin should load successfully, got exit 78: %s", stderr)
	}
}

func TestConfigErrorExits78AndNamesVariable(t *testing.T) {
	bin := buildSentinel(t)
	env := append(baseEnv(t, t.TempDir()), "TICK_INTERVAL=not-a-number")
	_, stderr, code := runBin(t, bin, env, "collect")
	if code != 78 {
		t.Fatalf("bad TICK_INTERVAL exit code = %d, want 78, stderr=%s", code, stderr)
	}
	if !bytes.Contains([]byte(stderr), []byte("TICK_INTERVAL")) {
		t.Fatalf("stderr = %q, expected it to name TICK_INTERVAL", stderr)
	}
}

// C2 maps a recovered panic to exit 1. guard is the seam that makes that path
// reachable without a panic hook in the production dispatch: main wraps run in
// it, and this test wraps a deliberately panicking func.
// TestLogLevelForSubcommandError_HonorsLogLevel is the T5 fix for a T2
// foundation gap (t5-review2, routed through main): main.go:107 hardcoded
// slog.LevelInfo for every subcommand-error log regardless of
// cfg.LogLevel. run() never holds a *config.Config (each subcommand loads
// its own), so logLevelForSubcommandError re-reads it via a second, cheap
// config.Load() call — this drives that exact function in-process, the
// same one logSubcommandError calls, rather than a hand-built level.
func TestLogLevelForSubcommandError_HonorsLogLevel(t *testing.T) {
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("HOST_ROOT", t.TempDir())
	t.Setenv("HOST_PROC", t.TempDir())

	t.Setenv("LOG_LEVEL", "DEBUG")
	if got := logLevelForSubcommandError(); got != slog.LevelDebug {
		t.Errorf("LOG_LEVEL=DEBUG: logLevelForSubcommandError() = %v, want LevelDebug", got)
	}

	t.Setenv("LOG_LEVEL", "ERROR")
	if got := logLevelForSubcommandError(); got != slog.LevelError {
		t.Errorf("LOG_LEVEL=ERROR: logLevelForSubcommandError() = %v, want LevelError", got)
	}
}

func TestGuardRecoversPanicAsExitOne(t *testing.T) {
	var stderr bytes.Buffer
	code := guard(&stderr, func() int { panic("boom") })
	if code != 1 {
		t.Errorf("guard() = %d, want 1 for a recovered panic (C2)", code)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want it to name the panic value", stderr.String())
	}
}

func TestGuardPassesThroughNormalExitCodes(t *testing.T) {
	var stderr bytes.Buffer
	for _, want := range []int{0, 64, 78} {
		if got := guard(&stderr, func() int { return want }); got != want {
			t.Errorf("guard() = %d, want %d", got, want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when nothing panicked", stderr.String())
	}
}
