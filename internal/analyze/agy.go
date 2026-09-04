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

// agy failure classes. The split exists for the fallback's reason code
// and for the two failures a retry cannot change: an unauthenticated agy
// and a prompt that never reached the model.
var (
	errAgyMissing = errors.New("agy: binary not found")
	errAgyTimeout = errors.New("agy: killed by hard timeout")
	errAgyFailed  = errors.New("agy: exited non-zero or unusable")

	// errAgyUnauth: agy's stderr shows an OAuth prompt. Headless mode
	// cannot complete an OAuth flow, so this persists until a human
	// re-authenticates; the fallback names that fix instead of sending a
	// 3am reader to check a healthy binary, and no retry can change it.
	errAgyUnauth = errors.New("agy: not authenticated")

	// errAgyPromptNotDelivered: the envelope reports zero input tokens,
	// so the prompt never reached a model at all (too large, malformed
	// invocation, dropped stdout), a fault of the invocation that a retry
	// would repeat identically. A failed status with tokens spent is a
	// turn that went wrong after the model saw the prompt and stays
	// retry-eligible.
	errAgyPromptNotDelivered = errors.New("agy: envelope reports zero input tokens")
)

// agyEnvelope is agy's --output-format json wrapper. agy has an open
// upstream defect (antigravity-cli#76): --print silently drops stdout in
// non-TTY contexts, exactly how sentinel spawns it, returning exit 0
// with nothing, so a caller cannot otherwise tell "no response" from
// "response lost". The envelope makes that distinguishable: a dropped
// prompt reports status="SUCCESS" with an empty response and zero tokens.
//
// StructuredOutput is undocumented by the upstream headless docs and
// absent from the envelope verified against agy 1.1.13 on 2026-08-16
// (contracts/analyze.md §6 step 4); it was found live on 2026-08-22
// against a newer agy pulled by deploy/agy-build-args.sh's unpinned
// manifest resolution. When present it is agy's own schema-validated
// result. Response stays the fallback for an agy without this field.
type agyEnvelope struct {
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Error            string          `json:"error"`
	Usage            struct {
		InputTokens int64 `json:"input_tokens"`
	} `json:"usage"`
}

// decodeAgyEnvelope unwraps agy's --output-format json envelope and rejects
// answers that never happened. agy has an upstream defect where print mode
// silently drops stdout in non-TTY contexts, exactly how this package
// spawns it, returning exit 0 with nothing, so a bare read cannot tell
// "no response" from "response lost". The envelope makes it distinguishable:
// zero input tokens means the prompt never reached the model
// (errAgyPromptNotDelivered, not retryable); any other failed or empty
// envelope is a turn that went wrong and is retried.
//
// It returns both response (agy's free-text answer, subject to fence
// normalisation) and structuredOutput (agy's own schema-validated result,
// if this agy version emits one); the caller decides which wins.
func decodeAgyEnvelope(out []byte) (response string, structuredOutput []byte, err error) {
	var env agyEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return "", nil, fmt.Errorf("envelope: %v", err)
	}
	if env.Status != "SUCCESS" || env.Usage.InputTokens == 0 {
		class := error(nil)
		if env.Usage.InputTokens == 0 {
			class = errAgyPromptNotDelivered
		}
		// The error field is surfaced only from a document that is an
		// envelope, one that carries a status.
		if reason := agyErrorText(env.Error); reason != "" && env.Status != "" {
			return "", nil, &envelopeError{Text: reason, err: wrapClass(class, fmt.Sprintf("status=%q input_tokens=%d error=%q", env.Status, env.Usage.InputTokens, reason))}
		}
		return "", nil, wrapClass(class, fmt.Sprintf("status=%q input_tokens=%d", env.Status, env.Usage.InputTokens))
	}
	if strings.TrimSpace(env.Response) == "" {
		return "", nil, fmt.Errorf("empty response (status=%q input_tokens=%d)", env.Status, env.Usage.InputTokens)
	}
	return env.Response, env.StructuredOutput, nil
}

// wrapClass prefixes detail with the failure class when there is one.
// envelopeError carries the bounded text of agy's envelope error field
// alongside the classified error, so a caller can read the refusal
// structurally instead of parsing it back out of a wrapped message.
type envelopeError struct {
	Text string
	err  error
}

func (e *envelopeError) Error() string { return e.err.Error() }
func (e *envelopeError) Unwrap() error { return e.err }

// envelopeErrorOf returns the envelope error text carried by err, if any.
func envelopeErrorOf(err error) (string, bool) {
	var e *envelopeError
	if errors.As(err, &e) {
		return e.Text, true
	}
	return "", false
}

func wrapClass(class error, detail string) error {
	if class == nil {
		return errors.New(detail)
	}
	return fmt.Errorf("%w: %s", class, detail)
}

// normalizeAgyOutput trims whitespace and strips a single leading ```json
// (or ```) fence line and trailing fence line, the two decorations models
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
// AGY_*, never the full process environment, which carries notification
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
	// Checked BEFORE runErr, because agy exits 0 when it has no
	// credentials. Measured on the target host:
	//
	//	Error: authentication required. Run 'agy' to log in, then retry.
	//	{"status":"ERROR","error":"authentication failed or timed out",...}
	//	exit 0
	//
	// Guarding this behind runErr != nil made the branch unreachable and
	// sent an unauthenticated run downstream as malformed output, so the
	// operator saw a schema fault where the truth was "log in".
	if isAgyAuthFailure(agyErr.String()) || isAgyAuthFailure(out.String()) {
		return nil, fmt.Errorf("%w: stderr %d bytes", errAgyUnauth, agyErr.Len())
	}
	if runErr != nil {
		// agy exits non-zero with an empty stderr and the reason in the
		// stdout envelope's error field; without it the log says only
		// "exit status 1" and the cause is lost.
		if reason := envelopeErrorText(out.Bytes()); reason != "" {
			return nil, &envelopeError{Text: reason, err: fmt.Errorf("%w: %v (stderr %d bytes, agy: %s)", errAgyFailed, runErr, agyErr.Len(), reason)}
		}
		return nil, fmt.Errorf("%w: %v (stderr %d bytes)", errAgyFailed, runErr, agyErr.Len())
	}
	return out.Bytes(), nil
}

// envelopeErrorText returns the bounded error text of an agy envelope in
// out, or "" when out is not an envelope or carries no error. Nothing
// else from stdout is ever surfaced: a panic trace or any other shape
// stays out of the log.
func envelopeErrorText(out []byte) string {
	var env agyEnvelope
	if err := json.Unmarshal(out, &env); err != nil || env.Status == "" {
		// Not an envelope: agy always sets status. Any other JSON that
		// happens to carry an "error" key is unknown stdout and stays out.
		return ""
	}
	return agyErrorText(env.Error)
}

// agyErrorText makes agy's error field safe for a log line: one line, no
// control characters, at most 200 runes. The field is agy's own
// diagnostic but can quote model output or prompt content, so it is the
// one piece of subprocess output that reaches a log line, and only in
// this bounded form, after the authentication check has run.
func agyErrorText(s string) string {
	s = oneLineText(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// oneLineText is agyErrorText's strip without its bound: newlines and
// line separators become spaces, control, C1, zero-width and bidi
// characters are removed, surrounding space trimmed.
func oneLineText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n', r == '\r', r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		case r == 0x85, r == 0x2028, r == 0x2029: // NEL, LINE/PARAGRAPH SEPARATOR
			return ' '
		case r >= 0x80 && r <= 0x9f: // the other C1 controls
			return -1
		case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // zero-width and bidi controls
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// isAgyAuthFailure detects the OAuth prompt in agy's stderr. The stderr
// text itself is never logged, log lines must not carry subprocess output,
// only this in-process check reads it. The single exception is the
// envelope's own error field, surfaced bounded by agyErrorText and only
// after this check has ruled out the OAuth shape.
func isAgyAuthFailure(s string) bool {
	// Case-insensitive: the binary writes "authentication required", the
	// matcher was written for "Authentication required", and the mismatch
	// meant the phrase never matched the phrase.
	l := strings.ToLower(s)
	return strings.Contains(l, "authentication required") ||
		strings.Contains(l, "authentication failed or timed out") ||
		strings.Contains(l, "accounts.google.com/o/oauth2")
}

// minimalAgyEnv builds the minimal env passed to agy: PATH, HOME(=AGY_HOME),
// TMPDIR, TZ, LANG, AGY_* only, never the process's full environment,
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
