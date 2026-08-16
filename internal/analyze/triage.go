package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// runTriage performs §6 steps 4-6: agy attempt 1, normalise + validate,
// attempt 2 with the CORRECTION suffix (now carrying the real validation
// error, §6 step 5) on parse/validate failure only (D7 — a dead binary,
// non-zero exit or hard timeout never retries). agy_empty splits in two
// (t4-review round 4, routed to main, implemented on their reasoning
// pending main's ruling): status != "SUCCESS" or input_tokens == 0 is
// systemic — the prompt never reached a model that answered, so retrying
// re-runs the identical broken invocation and only doubles the outage
// window (errAgyEmptySystemic, checked via errors.Is below). An empty
// response WITH a successful, token-spending call is plausibly a
// transient antigravity-cli#76 drop and stays retry-eligible.
func runTriage(ctx context.Context, o Options, d Deps, promptPath, schemaPath, promptText string, logger *slog.Logger) (*report.Report, string, error) {
	rep, reason, err := agyAttempt(ctx, o, d, promptPath, schemaPath, 1, logger)
	if rep != nil {
		return rep, "", nil
	}
	retryEligible := reason == "invalid_json" || reason == "schema_invalid" ||
		(reason == "agy_empty" && !errors.Is(err, errAgyEmptySystemic))
	if err != nil && !retryEligible {
		// dead binary / non-zero / timeout / systemic agy_empty: no retry (D7).
		return nil, reason, err
	}

	logger.Info("triage invalid, retrying")
	correction, cerr := buildCorrection(err.Error())
	if cerr != nil {
		return nil, "internal_error", fmt.Errorf("analyze: build correction: %w", cerr)
	}
	retryPrompt := promptText + correction
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
		if errors.Is(err, context.Canceled) {
			// Cancellation propagates as-is — no reason classification, no
			// fallback, no retry (the caller, runTriage/Run, checks
			// errors.Is(err, context.Canceled) and authors no report).
			return nil, "", fmt.Errorf("analyze: agy attempt %d: %w", attempt, err)
		}
		reason := classifyAgyErr(err)
		logger.Warn("triage", "attempt", attempt, "rc", "error", "reason", reason)
		return nil, reason, fmt.Errorf("analyze: agy attempt %d: %w", attempt, err)
	}
	logger.Info("triage", "attempt", attempt, "rc", 0, "bytes", len(out))

	// Decode the envelope first (§6 step 4): status != "SUCCESS", an
	// empty/whitespace response, or zero input_tokens is a dropped prompt
	// (agy_empty), not a model that legitimately said nothing. Only after
	// this check does normalisation/decoding touch the model's own answer.
	response, everr := decodeAgyEnvelope(out)
	if everr != nil {
		return nil, "agy_empty", fmt.Errorf("analyze: agy attempt %d: %w", attempt, everr)
	}

	normalized := normalizeAgyOutput([]byte(response))
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
	case errors.Is(err, errAgyUnauth):
		return "agy_unauth"
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
