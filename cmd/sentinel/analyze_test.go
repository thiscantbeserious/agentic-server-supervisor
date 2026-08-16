package main

import (
	"bytes"
	"os/exec"
	"testing"
)

// runBinStdin is runBin plus piped stdin — `sentinel analyze` is the only
// subcommand that reads stdin (contracts/analyze.md §1), so this helper
// lives next to its own tests rather than in the shared main_test.go.
func runBinStdin(t *testing.T, bin string, env []string, stdin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = bytes.NewBufferString(stdin)
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

// Case 14 (contracts/analyze.md §10): debug-mode input errors.
func TestAnalyzeDebug_EmptyStdin(t *testing.T) {
	bin := buildSentinel(t)
	stdout, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), "", "analyze")
	if code != 65 {
		t.Fatalf("empty stdin: exit code = %d, want 65", code)
	}
	if stdout != "" {
		t.Fatalf("empty stdin: stdout = %q, want empty", stdout)
	}
}

func TestAnalyzeDebug_NonJSONStdin(t *testing.T) {
	bin := buildSentinel(t)
	stdout, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), "not json at all", "analyze")
	if code != 65 {
		t.Fatalf("non-JSON stdin: exit code = %d, want 65", code)
	}
	if stdout != "" {
		t.Fatalf("non-JSON stdin: stdout = %q, want empty", stdout)
	}
}

func TestAnalyzeDebug_UnknownFlag(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), `{"meta":{}}`, "analyze", "--nope")
	if code != 64 {
		t.Fatalf("analyze --nope: exit code = %d, want 64", code)
	}
}

func TestAnalyzeDebug_PositionalArg(t *testing.T) {
	bin := buildSentinel(t)
	_, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), `{"meta":{}}`, "analyze", "extra-positional")
	if code != 64 {
		t.Fatalf("analyze extra-positional: exit code = %d, want 64", code)
	}
}

// End-to-end: agy missing (not on PATH in the hermetic baseEnv) drives the
// full stdin -> analyze.Run -> stdout fallback path, exit 3.
func TestAnalyzeDebug_AgyMissingProducesFallbackOnExit3(t *testing.T) {
	bin := buildSentinel(t)
	facts := `{"meta":{"schema_version":"1","hostname":"h","tick_seq":1,"collector_errors":[]},"kernel":{"count":0,"truncated":false,"dropped_entries":0,"entries":[]}}`
	stdout, _, code := runBinStdin(t, bin, baseEnv(t, t.TempDir()), facts, "analyze")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (fallback: agy not on PATH)", code)
	}
	if stdout == "" {
		t.Fatal("expected a fallback report on stdout")
	}
}
