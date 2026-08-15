package main

import (
	"bytes"
	"fmt"
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
