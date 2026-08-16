// agy.go: the agy subprocess. Spawning, minimal environment, output capture,
// the JSON envelope check, and the error classes that decide whether a
// failure is worth retrying. Nothing else in the package execs anything.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
)

// agy failure classes. The split exists because retrying is only ever
// useful when the model answered badly: a missing binary, a hard timeout,
// a non-zero exit, or an unauthenticated agy will fail identically on the
// second attempt, and retrying those only doubles the outage window.
var (
	errAgyMissing = errors.New("agy: binary not found")
	errAgyTimeout = errors.New("agy: killed by hard timeout")
	errAgyFailed  = errors.New("agy: exited non-zero or unusable")

	// errAgyUnauth: agy's stderr shows an OAuth prompt. Headless mode
	// cannot complete an OAuth flow, so this persists until a human
	// re-authenticates; the fallback names that fix instead of sending a
	// 3am reader to check a healthy binary.
	errAgyUnauth = errors.New("agy: not authenticated")

	// errAgyEmptySystemic: the envelope reports a failed call or zero
	// input tokens — the prompt never reached a model that answered, so a
	// retry would re-run the identical broken invocation. An empty response
	// from a call that did spend tokens is the one empty-output case that
	// is plausibly transient and stays retry-eligible.
	errAgyEmptySystemic = errors.New("agy: envelope reports failure or zero input tokens")
)

// agyEnvelope is agy's --output-format json wrapper. agy has an open
// upstream defect (antigravity-cli#76): --print silently drops stdout in
// non-TTY contexts — exactly how sentinel spawns it — returning exit 0
// with nothing, so a caller cannot otherwise tell "no response" from
// "response lost". The envelope makes that distinguishable: a dropped
// prompt reports status="SUCCESS" with an empty response and zero tokens.
type agyEnvelope struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Usage    struct {
		InputTokens int64 `json:"input_tokens"`
	} `json:"usage"`
}

// decodeAgyEnvelope unwraps agy's --output-format json envelope and rejects
// answers that never happened. agy has an upstream defect where print mode
// silently drops stdout in non-TTY contexts — exactly how this package
// spawns it — returning exit 0 with nothing, so a bare read cannot tell
// "no response" from "response lost". The envelope makes it distinguishable:
// a failed status or zero input tokens means the prompt never reached the
// model (not retryable); a successful, token-spending call with an empty
// response is plausibly a transient drop (retryable).
func decodeAgyEnvelope(out []byte) (string, error) {
	var env agyEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("%w: envelope: %v", errAgyEmptySystemic, err)
	}
	if env.Status != "SUCCESS" || env.Usage.InputTokens == 0 {
		return "", fmt.Errorf("%w: status=%q input_tokens=%d", errAgyEmptySystemic, env.Status, env.Usage.InputTokens)
	}
	if strings.TrimSpace(env.Response) == "" {
		return "", fmt.Errorf("empty response (status=%q input_tokens=%d)", env.Status, env.Usage.InputTokens)
	}
	return env.Response, nil
}

// normalizeAgyOutput trims whitespace and strips a single leading ```json
// (or ```) fence line and trailing fence line — the two decorations models
// habitually add around JSON they were told to return bare.
func normalizeAgyOutput(out []byte) []byte {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if first == "```json" || first == "```" {
			lines = lines[1:]
		}
	}
	if len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "```" {
			lines = lines[:len(lines)-1]
		}
	}
	return []byte(strings.TrimSpace(strings.Join(lines, "\n")))
}

// limitedBuffer caps captured stdout, silently discarding the excess, so a
// misbehaving or malicious agy cannot grow the buffer without bound.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// runAgy executes one agy call. The prompt travels as an argv argument, not
// stdin: agy's print mode ignores stdin entirely, and a piped-in prompt
// produces a hallucinated answer to an empty question. The prompt file is
// still written for debugging and the retry append, but it is passed by
// value. The environment is reduced to PATH, HOME, TMPDIR, TZ, LANG and
// AGY_* — never the full process environment, which carries notification
// secrets that must not leak into a subprocess.
//
// A cancelled context is reported as cancellation, checked before the
// timeout and exit-code branches, because a SIGTERM mid-call also makes
// cmd.Run return an error and must not be misread as an agy failure.
func runAgy(ctx context.Context, cfg *config.Config, promptPath, schemaPath string) ([]byte, error) {
	if _, err := exec.LookPath(cfg.AgyBin); err != nil {
		return nil, fmt.Errorf("%w: %v", errAgyMissing, err)
	}

	// agy will not start at all without its $HOME existing, and the debug
	// path (`sentinel analyze`) has no runtime preflight to seed it.
	if err := os.MkdirAll(cfg.AgyHome, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create AGY_HOME: %v", errAgyFailed, err)
	}

	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read prompt: %v", errAgyFailed, err)
	}

	cmd := exec.CommandContext(ctx, cfg.AgyBin, "--print", string(promptBytes),
		"--json-schema", schemaPath,
		"--output-format", "json",
		"--print-timeout", cfg.AgyPrintTimeout.String())
	cmd.Stdin = nil // agy's print mode does not read stdin at all
	var out bytes.Buffer
	var agyErr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, max: 1 << 20}
	cmd.Stderr = &agyErr
	cmd.Env = minimalAgyEnv(cfg)
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	// SIGTERM also surfaces as a non-nil cmd.Run error; check cancellation first.
	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("%w", context.Canceled)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w", errAgyTimeout)
	}
	if runErr != nil {
		if isAgyAuthFailure(agyErr.String()) {
			return nil, fmt.Errorf("%w: stderr %d bytes", errAgyUnauth, agyErr.Len())
		}
		return nil, fmt.Errorf("%w: %v (stderr %d bytes)", errAgyFailed, runErr, agyErr.Len())
	}
	return out.Bytes(), nil
}

// isAgyAuthFailure detects the OAuth prompt in agy's stderr. The stderr
// text itself is never logged — log lines must not carry subprocess output —
// only this in-process check reads it.
func isAgyAuthFailure(stderr string) bool {
	return strings.Contains(stderr, "Authentication required") ||
		strings.Contains(stderr, "accounts.google.com/o/oauth2")
}

// minimalAgyEnv builds the minimal env passed to agy: PATH, HOME(=AGY_HOME),
// TMPDIR, TZ, LANG, AGY_* only — never the process's full environment,
// which could carry notification secrets that must not leak into a
// subprocess.
func minimalAgyEnv(cfg *config.Config) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + cfg.AgyHome,
		"TMPDIR=" + cfg.TmpDir,
		"TZ=" + cfg.TZ,
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AGY_") {
			env = append(env, kv)
		}
	}
	return env
}
