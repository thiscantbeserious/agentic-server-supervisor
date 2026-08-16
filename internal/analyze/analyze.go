// Package analyze implements `sentinel analyze` (contracts/analyze.md):
// the LLM stage. It runs agy against the embedded sentinel.md prompt, with
// history windowing, in-process schema validation with one retry, a
// deterministic fallback, and the two-stage (A9) deep-dive analysis.
//
// This package is the security boundary between attacker-controlled log
// text and an LLM prompt: the FACTS/HISTORY/RESOLVED/FINDING/DEEP fences
// and the per-run nonce (§6, §7) are load-bearing, not decoration. The
// model has no tools and executes nothing; the worst case of a successful
// prompt injection here is wrong text in a report (ARCHITECTURE design
// principle 4) — and even that is bounded by the deterministic
// recommendation guard (§6 step 11b, stage2.go).
package analyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/logging"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// Options is the in-process seam (C8).
type Options struct {
	Cfg   *config.Config
	Facts *facts.Facts
	Seq   int64
}

// Deps are the two seams the tests replace (§9). Not interfaces — one
// implementation each.
type Deps struct {
	RunAgy      func(ctx context.Context, o Options, promptPath, schemaPath string) ([]byte, error)
	CollectDeep func(ctx context.Context, component string) (*facts.Facts, error)
}

// DefaultDeps wires the real agy subprocess and the in-process deep
// collect (§6 step 9).
func DefaultDeps(cfg *config.Config) Deps {
	return Deps{
		RunAgy: func(ctx context.Context, o Options, promptPath, schemaPath string) ([]byte, error) {
			return runAgy(ctx, o.Cfg, promptPath, schemaPath)
		},
		CollectDeep: func(ctx context.Context, component string) (*facts.Facts, error) {
			// Seq is not threaded through here: DefaultDeps is built once
			// from cfg alone (§9 signature) and deep facts' meta.tick_seq is
			// informational only — it never gates or identifies anything
			// (the finding's dedup key already does that job).
			return collect.Run(ctx, collect.Options{Cfg: cfg, DeepComponent: component})
		},
	}
}

// logWriter is where the package's slog output goes; tests redirect it to
// capture the C7-format lines the contract requires (e.g.
// "fallback report built reason=agy_timeout"). Production leaves it at the
// zero value, which defaults to os.Stderr in newLogger.
var logWriter io.Writer

func newLogger() *slog.Logger {
	w := logWriter
	if w == nil {
		w = os.Stderr
	}
	return slog.New(logging.New(w, slog.LevelInfo)).With("component", "analyze")
}

// agy error classes (D7): distinguished so the fallback names the right
// reason and so a dead binary/timeout/non-zero exit never retries.
var (
	errAgyMissing = errors.New("agy: binary not found")
	errAgyTimeout = errors.New("agy: killed by hard timeout")
	errAgyFailed  = errors.New("agy: exited non-zero or unusable")
)

// Run performs §6. It always returns a non-nil, valid report; a non-nil
// error means the returned document is the fallback. It never panics and
// never writes outside the paths in §8.
func Run(ctx context.Context, o Options, d Deps) (*report.Report, error) {
	cfg := o.Cfg
	logger := newLogger()
	pid := os.Getpid()

	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			os.Remove(p)
		}
	}()

	// §9: "It never panics and never writes outside the paths in §8." A
	// nil RunAgy (misconstructed Deps) is not a documented failure mode,
	// but the alternative is a nil-pointer panic on the first call below —
	// treat it the same as agy being missing rather than crashing.
	if d.RunAgy == nil {
		return buildFallback(cfg, o.Seq, "agy_missing", o.Facts, logger), errors.New("analyze: Deps.RunAgy is nil")
	}

	// internal_error (not agy_failed) below: these paths fail before agy
	// is ever invoked, so blaming "the analyzer exited non-zero" would send
	// a 3am reader to check agy's health for a fault that is ours.
	nonce, err := newNonce()
	if err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: nonce: %w", err)
	}

	// resolved is output-only (commit ba631ca): historyKeys \ this tick's
	// findings needs findings that do not exist until AFTER the stage-1
	// call below, so it is never part of the prompt — only newest (kept
	// for computeResolved post-call) is needed here.
	hist := loadHistoryReports(cfg.StateDir, cfg.HistoryN)
	histLines := historyProjectionLines(hist)
	newest := newestHistory(hist)

	stage1Prompt, err := assembleStage1(cfg, o.Facts, histLines, nonce)
	if err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: assemble stage1: %w", err)
	}

	promptPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("sentinel-prompt-%d.txt", pid))
	schemaPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("report.schema-%d.json", pid))
	cleanup = append(cleanup, promptPath, schemaPath)

	if err := os.WriteFile(promptPath, []byte(stage1Prompt), 0o600); err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: write prompt: %w", err)
	}
	if err := os.WriteFile(schemaPath, report.SchemaJSON, 0o600); err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: write schema: %w", err)
	}

	rep, reason, err := runStage1(ctx, o, d, promptPath, schemaPath, stage1Prompt, logger)
	if err != nil {
		return buildFallback(cfg, o.Seq, reason, o.Facts, logger), err
	}

	// §6 step 7: inject keys, meta and resolved; drop any model-supplied
	// key/first_seen/occurrences/meta/resolved.
	for i := range rep.Findings {
		rep.Findings[i].Key = dedup.Key(rep.Findings[i].Component, rep.Findings[i].Evidence)
		rep.Findings[i].FirstSeen = 0
		rep.Findings[i].Occurrences = 0
	}
	rep.Meta = &report.Meta{Hostname: cfg.Hostname, TickSeq: o.Seq}
	rep.Resolved = computeResolved(newest, rep.Findings)

	if !cfg.DeepEnabled || rep.Status == "OK" {
		guardRecommendations(rep)
		return rep, nil
	}

	runDeepDive(ctx, cfg, o, d, rep, nonce, histLines, pid, &cleanup, logger)
	guardRecommendations(rep)

	return rep, nil
}

// runStage1 performs §6 steps 4-6: agy attempt 1, normalise + validate,
// attempt 2 with the CORRECTION suffix (now carrying the real validation
// error, §6 step 5) on parse/validate failure only (D7 — a dead binary,
// non-zero exit or hard timeout never retries).
func runStage1(ctx context.Context, o Options, d Deps, promptPath, schemaPath, promptText string, logger *slog.Logger) (*report.Report, string, error) {
	rep, reason, err := agyAttempt(ctx, o, d, promptPath, schemaPath, 1, logger)
	if rep != nil {
		return rep, "", nil
	}
	if err != nil && reason != "invalid_json" && reason != "schema_invalid" {
		// dead binary / non-zero / timeout: no retry (D7).
		return nil, reason, err
	}

	logger.Info("stage1 invalid, retrying")
	retryPrompt := promptText + buildCorrectionBlock(err.Error())
	if werr := os.WriteFile(promptPath, []byte(retryPrompt), 0o600); werr != nil {
		return nil, "internal_error", fmt.Errorf("analyze: write correction prompt: %w", werr)
	}

	rep2, reason2, err2 := agyAttempt(ctx, o, d, promptPath, schemaPath, 2, logger)
	if rep2 != nil {
		return rep2, "", nil
	}
	if err2 != nil {
		return nil, reason2, err2
	}
	return nil, reason, err
}

// agyAttempt runs one d.RunAgy call and classifies its outcome. A non-nil
// report means success; otherwise reason names why, and err is non-nil and,
// for invalid_json/schema_invalid, carries the concrete validation error
// text (§6 step 5's ${VALIDATION_ERROR}) rather than a wrapped/prefixed
// message, so the CORRECTION block can quote it directly.
func agyAttempt(ctx context.Context, o Options, d Deps, promptPath, schemaPath string, attempt int, logger *slog.Logger) (*report.Report, string, error) {
	cctx, cancel := context.WithTimeout(ctx, o.Cfg.AgyHardTimeout)
	defer cancel()

	out, err := d.RunAgy(cctx, o, promptPath, schemaPath)
	if err != nil {
		reason := classifyAgyErr(err)
		logger.Warn("stage1", "attempt", attempt, "rc", "error", "reason", reason)
		return nil, reason, fmt.Errorf("analyze: agy attempt %d: %w", attempt, err)
	}
	logger.Info("stage1", "attempt", attempt, "rc", 0, "bytes", len(out))

	normalized := normalizeAgyOutput(out)
	rep, verr := report.Validate(normalized)
	if verr == nil {
		return rep, "", nil
	}
	reason := "invalid_json"
	if len(normalized) > 0 && json.Valid(normalized) {
		reason = "schema_invalid"
	}
	return nil, reason, verr
}

func classifyAgyErr(err error) string {
	switch {
	case errors.Is(err, errAgyMissing):
		return "agy_missing"
	case errors.Is(err, errAgyTimeout):
		return "agy_timeout"
	default:
		return "agy_failed"
	}
}

// buildFallback builds the §5 fallback, re-validates it (contract:
// "passed through report.Validate before being returned"), and logs the
// C7 line the contract names.
func buildFallback(cfg *config.Config, seq int64, reason string, f *facts.Facts, logger *slog.Logger) *report.Report {
	rep := Fallback(cfg, seq, reason, f)
	if raw, err := json.Marshal(rep); err == nil {
		if v, verr := report.Validate(raw); verr == nil {
			rep = v
		}
	}
	logger.Warn("fallback report built", "reason", reason)
	return rep
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

// runDeepDive performs §6 steps 8-11. Any failure along this path is
// non-fatal (§5): the caller already holds the validated stage-1 report
// and this function only ever enriches it in place or leaves it untouched.
func runDeepDive(ctx context.Context, cfg *config.Config, o Options, d Deps, rep *report.Report, nonce string, histLines []string, pid int, cleanup *[]string, logger *slog.Logger) {
	appendNoDeepDiveSuffix(rep.Findings, cfg.StateDir)

	candidateKey, ok := selectCandidate(cfg.StateDir, rep.Findings)
	manageDeepQueue(cfg.StateDir, rep.Findings, candidateKey)
	if !ok || d.CollectDeep == nil || d.RunAgy == nil {
		return
	}

	var candidate *report.Finding
	for i := range rep.Findings {
		if rep.Findings[i].Key == candidateKey {
			candidate = &rep.Findings[i]
			break
		}
	}
	if candidate == nil {
		return
	}

	dctx, cancel := context.WithTimeout(ctx, cfg.DeepTimeout)
	defer cancel()
	deepFacts, err := d.CollectDeep(dctx, candidate.Component)
	if err != nil || deepFacts == nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}

	// The candidate is sent to stage 2 as-is (its dedup key included, for
	// the operator's own reference in the prompt) but the MERGE below
	// never trusts a key the model echoes back — this is the finding we
	// sent, identified by our own pointer (§6 step 11).
	findingJSON, err := json.Marshal(candidate)
	if err != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	deepJSON, err := json.Marshal(deepFacts)
	if err != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}

	// "component" is deliberately not used as an attr key here: the C7
	// handler (internal/logging) special-cases any attr literally named
	// "component" and diverts it into the line's own component SLOT
	// instead of printing it as k=v (that slot is already "analyze" for
	// this whole package) — using it here would silently overwrite the
	// line's component with "zfs"/"kernel"/etc, exactly the bug this
	// comment exists to prevent a future edit from reintroducing.
	logger.Info("deep-dive", "target", candidate.Component, "key", candidateKey)

	stage2Prompt, err := assembleStage2(cfg, string(findingJSON), string(deepJSON), histLines, nonce, candidate.Component)
	if err != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	deepPromptPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("sentinel-deep-%d.txt", pid))
	if err := os.WriteFile(deepPromptPath, []byte(stage2Prompt), 0o600); err != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	*cleanup = append(*cleanup, deepPromptPath)

	// §6 step 10: stage 2 gets its OWN schema, not report.schema.json —
	// requiring the full report shape let the model copy a 16-hex key
	// wrong and silently lose the enrichment (key mismatch).
	stage2SchemaPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("stage2.schema-%d.json", pid))
	if err := os.WriteFile(stage2SchemaPath, stage2SchemaJSON, 0o600); err != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	*cleanup = append(*cleanup, stage2SchemaPath)

	cctx, cancel2 := context.WithTimeout(ctx, cfg.AgyHardTimeout)
	defer cancel2()
	out, rerr := d.RunAgy(cctx, o, deepPromptPath, stage2SchemaPath)
	if rerr != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	// No retry at stage 2 (§6 step 10).
	normalized := normalizeAgyOutput(out)
	stage2Rep, verr := validateStage2(normalized)
	if verr != nil {
		logger.Info("deep-dive failed, keeping stage1")
		return
	}

	origAnalysis, origRecommendation, origHeadline := candidate.Analysis, candidate.Recommendation, rep.Headline
	candidate.Analysis = stage2Rep.Analysis
	candidate.Recommendation = stage2Rep.Recommendation
	if stage2Rep.Headline != "" {
		// §6 step 11: the optional headline REPLACES stage 1's — the
		// notification title must not stay frozen on the shallow tick
		// view once the deep collect reveals something worse.
		rep.Headline = stage2Rep.Headline
	}

	raw, merr := json.Marshal(rep)
	if merr != nil {
		candidate.Analysis, candidate.Recommendation, rep.Headline = origAnalysis, origRecommendation, origHeadline
		logger.Info("deep-dive failed, keeping stage1")
		return
	}
	if _, verr := report.Validate(raw); verr != nil {
		candidate.Analysis, candidate.Recommendation, rep.Headline = origAnalysis, origRecommendation, origHeadline
		logger.Info("deep-dive failed, keeping stage1")
	}
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
func runAgy(ctx context.Context, cfg *config.Config, promptPath, schemaPath string) ([]byte, error) {
	if _, err := exec.LookPath(cfg.AgyBin); err != nil {
		return nil, fmt.Errorf("%w: %v", errAgyMissing, err)
	}

	promptFile, err := os.Open(promptPath)
	if err != nil {
		return nil, fmt.Errorf("%w: open prompt: %v", errAgyFailed, err)
	}
	defer promptFile.Close()

	cmd := exec.CommandContext(ctx, cfg.AgyBin, "--print",
		"--json-schema", schemaPath,
		"--print-timeout", cfg.AgyPrintTimeout.String())
	cmd.Stdin = promptFile
	var out bytes.Buffer
	var agyErr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, max: 1 << 20}
	cmd.Stderr = &agyErr
	cmd.Env = minimalAgyEnv(cfg)
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w", errAgyTimeout)
	}
	if runErr != nil {
		return nil, fmt.Errorf("%w: %v (stderr %d bytes)", errAgyFailed, runErr, agyErr.Len())
	}
	return out.Bytes(), nil
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
