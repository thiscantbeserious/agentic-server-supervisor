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

// agy error classes (D7): distinguished so the fallback names the right
// reason and so a dead binary/timeout/non-zero exit never retries.
var (
	errAgyMissing = errors.New("agy: binary not found")
	errAgyTimeout = errors.New("agy: killed by hard timeout")
	errAgyFailed  = errors.New("agy: exited non-zero or unusable")
	// errAgyUnauth is agy's stderr containing an OAuth prompt (§6 step 4).
	// Headless mode cannot complete an OAuth flow, so this persists until a
	// human re-authenticates — reason agy_unauth, never retried (same as
	// agy_missing/agy_failed/agy_timeout: a retry cannot fix an
	// unauthenticated binary any more than it can fix a dead one, D7).
	errAgyUnauth = errors.New("agy: not authenticated")
	// errAgyEmptySystemic marks the two agy_empty sub-cases that are NOT
	// retry-eligible (t4-review round 4, routed to main and implemented on
	// their reasoning pending main's ruling): status != "SUCCESS" or
	// input_tokens == 0 both mean the prompt never reached a model that
	// actually answered — a retry re-runs the identical broken invocation
	// and doubles the outage window, exactly what D7 forbids for a dead
	// binary. An empty `response` WITH status SUCCESS and non-zero tokens
	// is the one agy_empty sub-case that's plausibly transient and stays
	// retry-eligible.
	errAgyEmptySystemic = errors.New("agy: envelope reports failure or zero input tokens")
)

// agyEnvelope is agy's --output-format json wrapper (§6 step 4, mandatory
// since t4's live-validation round). agy has an open upstream defect
// (antigravity-cli#76): --print silently drops stdout in non-TTY contexts
// — exactly how sentinel spawns it — returning exit 0 with nothing, so a
// caller cannot otherwise tell "no response" from "response lost". The
// envelope makes that distinguishable: a dropped prompt reports
// status="SUCCESS" with an empty response and zero tokens.
type agyEnvelope struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Usage    struct {
		InputTokens int64 `json:"input_tokens"`
	} `json:"usage"`
}

// decodeAgyEnvelope implements §6 step 4's envelope check, shared by both
// triage (agyAttempt) and deep dive (runDeepDive) — every real agy
// invocation now goes through --output-format json, deep dive included, so
// both call sites face the same antigravity-cli#76 empty-stdout risk.
func decodeAgyEnvelope(out []byte) (string, error) {
	var env agyEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("%w: envelope: %v", errAgyEmptySystemic, err)
	}
	// status != "SUCCESS" or input_tokens == 0 both mean the prompt never
	// reached a model that answered — systemic, not retry-eligible.
	if env.Status != "SUCCESS" || env.Usage.InputTokens == 0 {
		return "", fmt.Errorf("%w: status=%q input_tokens=%d", errAgyEmptySystemic, env.Status, env.Usage.InputTokens)
	}
	// SUCCESS with tokens spent but nothing came back: plausibly a
	// transient antigravity-cli#76 drop, retry-eligible.
	if strings.TrimSpace(env.Response) == "" {
		return "", fmt.Errorf("empty response (status=%q input_tokens=%d)", env.Status, env.Usage.InputTokens)
	}
	return env.Response, nil
}

// normalizeAgyOutput implements §6 step 4's normalisation: trim space,
// strip a leading ```json or ``` fence line and a trailing fence line.
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

// --- agy subprocess (DefaultDeps.RunAgy) ---

// limitedBuffer caps captured stdout at maxBytes (§6 step 4), silently
// discarding anything beyond the cap rather than growing unbounded on a
// misbehaving or malicious agy invocation.
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

// runAgy is DefaultDeps' RunAgy: exec agy --print under AgyHardTimeout,
// stdin from promptPath, a minimal env (§6 step 4).
// runAgy is DefaultDeps' RunAgy. The prompt is passed as an ARGV argument,
// never on stdin (§6 step 4, t4's live-validation defect): agy 1.1.13's
// print mode ignores stdin entirely — a piped-in prompt produces a
// hallucinated answer to an empty question, while the same text as an
// argument answers correctly. The prompt file at promptPath is still
// written (for debugging and the attempt-2 CORRECTION append) but is read
// here and passed by value.
func runAgy(ctx context.Context, cfg *config.Config, promptPath, schemaPath string) ([]byte, error) {
	if _, err := exec.LookPath(cfg.AgyBin); err != nil {
		return nil, fmt.Errorf("%w: %v", errAgyMissing, err)
	}

	// §6 step 4: "$AGY_HOME must exist before agy is spawned." The debug
	// path (`sentinel analyze`) has no runtime preflight to seed it, and
	// agy will not start at all without its $HOME existing.
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
	// Cancellation is not an analyzer failure (§6 step 4, live-gate round
	// 6): on SIGTERM during `tick --loop` the parent context is cancelled,
	// and classifying that as agy_failed fabricates an ALERT fallback for
	// a shutdown that has nothing wrong with it. Checked BEFORE
	// DeadlineExceeded and BEFORE the non-zero-exit branch, since a
	// cancelled context can also make cmd.Run() return a non-nil error.
	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("%w", context.Canceled)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w", errAgyTimeout)
	}
	if runErr != nil {
		if isAgyAuthFailure(agyErr.String()) {
			// Headless mode cannot complete an OAuth flow, so this state
			// persists until a human re-authenticates — "analyzer exited
			// non-zero" would send the 3am reader to check a healthy
			// binary instead of naming the actual fix (§6 step 4).
			return nil, fmt.Errorf("%w: stderr %d bytes", errAgyUnauth, agyErr.Len())
		}
		return nil, fmt.Errorf("%w: %v (stderr %d bytes)", errAgyFailed, runErr, agyErr.Len())
	}
	return out.Bytes(), nil
}

// isAgyAuthFailure checks agy's stderr for the OAuth prompt headless mode
// cannot complete (§6 step 4). Never logged (C7: agy stdout/stderr content
// stays out of every log line) — only checked in-process for classification.
func isAgyAuthFailure(stderr string) bool {
	return strings.Contains(stderr, "Authentication required") ||
		strings.Contains(stderr, "accounts.google.com/o/oauth2")
}

// minimalAgyEnv is the §6 step 4 minimal env: PATH, HOME(=AGY_HOME),
// TMPDIR, TZ, LANG, AGY_* only — never the process's full environment,
// which could carry the notify/mailrise secrets C7 forbids logging.
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
