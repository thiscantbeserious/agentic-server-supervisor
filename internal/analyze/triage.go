// triage.go: the first model call. Up to four attempts over all facts
// inside one shared time budget, each retry carrying a correction when
// there is something concrete to correct, and the classification of agy
// failures into fallback reasons.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// maxTriageAttempts bounds the triage phase: one attempt and three
// retries. A failed attempt is usually the model's own choice, a shell
// command it was told it does not have, a self-correction turn that
// concatenated into invalid JSON, and a re-roll rarely repeats it, so each
// retry trades one cheap call for a fallback ALERT that a human then reads.
const maxTriageAttempts = 4

// TriageBudgetTimeouts is how many AGY_HARD_TIMEOUTs the whole triage
// phase shares, retries included, and the term the health window counts
// for it: a tick is bounded by configuration alone, so a retry must not
// be able to add a full AGY_HARD_TIMEOUT of its own. An attempt that
// spends its own hard timeout leaves room for at most one more; an
// attempt started near the end of the budget is cut by the phase
// deadline and classified agy_timeout like any other.
const TriageBudgetTimeouts = 2

func triageBudget(cfg *config.Config) time.Duration {
	return TriageBudgetTimeouts * cfg.AgyHardTimeout
}

// deniedToolMarker is the envelope error agy emits when the model asked to
// run a command and the tool-deny policy refused. In print mode the
// refusal ends the turn with no report, so this is the one failure whose
// correction must say what happened rather than what was wrong with the
// answer.
const deniedToolMarker = "permission check failed for command"

// runTriage performs the triage attempts. Every failed attempt is retried
// while the budget lasts, except the two whose outcome a retry cannot
// change: an unauthenticated agy (the fix is a login) and an envelope
// with zero input tokens (the prompt never reached the model). print mode
// is stateless, so a retry that only repeats the prompt is a re-roll;
// when the failure was the model's own doing the retry prompt appends the
// concrete correction, the validator's message or the refused command.
func runTriage(ctx context.Context, o Options, d Deps, promptPath, schemaPath, promptText, nonce string, logger *slog.Logger) (*report.Report, string, error) {
	phaseCtx, cancel := context.WithTimeout(ctx, triageBudget(o.Cfg))
	defer cancel()

	var (
		reason string
		err    error
	)
	for attempt := 1; attempt <= maxTriageAttempts; attempt++ {
		if attempt > 1 {
			if errors.Is(ctx.Err(), context.Canceled) {
				// A shutdown between two attempts authors nothing, the
				// same as one inside an attempt.
				return nil, "", context.Canceled
			}
			if phaseCtx.Err() != nil {
				// The budget is spent: the last attempt's reason stands
				// rather than a retry that could only time out.
				break
			}
			logger.Info("triage retrying", "attempt", attempt, "reason", reason, "error", err)
			correction, cerr := retryCorrection(nonce, reason, err)
			if cerr != nil {
				return nil, "internal_error", fmt.Errorf("analyze: build correction: %w", cerr)
			}
			// Rewritten from the base prompt every time, so corrections
			// never accumulate: the model only needs the last failure.
			if werr := os.WriteFile(promptPath, []byte(promptText+correction), 0o600); werr != nil {
				return nil, "internal_error", fmt.Errorf("analyze: write correction prompt: %w", werr)
			}
		}
		var rep *report.Report
		rep, reason, err = agyAttempt(phaseCtx, o, d, promptPath, schemaPath, attempt, logger)
		if rep != nil {
			return rep, "", nil
		}
		if errors.Is(err, context.Canceled) || !retryable(reason, err) {
			return nil, reason, err
		}
	}
	return nil, reason, err
}

// retryable rules out the two failures a retry cannot change.
func retryable(reason string, err error) bool {
	return reason != "agy_unauth" && !errors.Is(err, errAgyPromptNotDelivered)
}

// retryCorrection is the block appended to the retry prompt after the
// failure described by reason and err: the denied-tool correction when
// agy's envelope error names a refused command, the validator correction
// after an answer that failed parsing or validation, nothing otherwise.
// The refused command is agy-derived text and the validator message echoes
// model-supplied values; both are quoted into the prompt as data inside
// the run's nonce fences, in the bounded one-line form agyErrorText
// produces for the refusal, and never reach a report.
func retryCorrection(nonce, reason string, err error) (string, error) {
	// The marker is trusted only inside agy's own envelope error, read
	// structurally. A validator message echoes model-supplied values, and
	// facts content reaches the model, so a marker found by searching a
	// wrapped message could have been planted; it never selects this
	// branch.
	if text, ok := envelopeErrorOf(err); ok {
		if i := strings.Index(text, deniedToolMarker); i >= 0 {
			return buildCorrection(nonce, "", text[i:])
		}
	}
	if reason == "invalid_json" || reason == "schema_invalid" {
		return buildCorrection(nonce, err.Error(), "")
	}
	return "", nil
}

// agyAttempt runs one agy call and classifies the outcome. On success the
// report is non-nil; otherwise reason names the failure for the fallback,
// and for parse/validation failures err carries the raw validator message
// so the correction block can quote it verbatim.
func agyAttempt(ctx context.Context, o Options, d Deps, promptPath, schemaPath string, attempt int, logger *slog.Logger) (*report.Report, string, error) {
	cctx, cancel := context.WithTimeout(ctx, o.Cfg.AgyHardTimeout)
	defer cancel()

	out, err := d.RunAgy(cctx, o, promptPath, schemaPath)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Cancellation propagates as-is, no reason classification, no
			// fallback, no retry (the caller, runTriage/Run, checks
			// errors.Is(err, context.Canceled) and authors no report).
			return nil, "", fmt.Errorf("analyze: agy attempt %d: %w", attempt, err)
		}
		reason := classifyAgyErr(err)
		logger.Warn("triage", "attempt", attempt, "rc", "error", "reason", reason)
		return nil, reason, fmt.Errorf("analyze: agy attempt %d: %w", attempt, err)
	}
	logger.Info("triage", "attempt", attempt, "rc", 0, "bytes", len(out))

	// Decode the envelope first: a failed status, an empty/whitespace
	// response, or zero input_tokens is no answer (agy_empty), not a model
	// that legitimately said nothing. Only after this check does
	// normalisation/decoding touch the model's own answer.
	response, structuredOutput, everr := decodeAgyEnvelope(out)
	if everr != nil {
		return nil, "agy_empty", fmt.Errorf("analyze: agy attempt %d: %w", attempt, everr)
	}

	// structured_output, when present, is agy's own schema-validated
	// result: prefer it over response, which a newer agy can pollute by
	// concatenating every internal self-correction turn into one string
	// (found live 2026-08-22, contracts/analyze.md §6 step 4). Still run
	// it through report.Validate, no exemption for having come from the
	// cleaner field. Absent, empty, or invalid ⇒ fall through to response
	// unchanged, the path an agy without this field always takes.
	if trimmed := bytes.TrimSpace(structuredOutput); len(trimmed) > 0 {
		if rep, verr := report.Validate(trimmed); verr == nil {
			logger.Info("triage", "attempt", attempt, "via", "structured_output")
			return rep, "", nil
		}
	}

	normalized := normalizeAgyOutput([]byte(response))
	rep, verr := report.Validate(normalized)
	if verr == nil {
		logger.Info("triage", "attempt", attempt, "via", "response")
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

// buildFallback wraps Fallback with re-validation and the log line
// operators grep for during an outage. cause is the concrete error that
// drove this tick to a fallback (a parse/validate error, a missing binary,
// a timeout, ...); every call site already holds one, since Run only ever
// calls buildFallback alongside a non-nil error return. Logging it here,
// not just the reason code, is what makes an invalid_json/schema_invalid
// occurrence self-diagnosing: the reason code alone cannot distinguish a
// wrapped answer from a truncated one from a schema violation, and C7
// permits it: cause is the validator's own message or, for an agy
// failure, the envelope's error field in the bounded one-line form C7
// names, never prompt or facts content and never any other agy stdout.
// It reaches this log line only; the report carries the reason code.
func buildFallback(cfg *config.Config, seq int64, reason string, f *facts.Facts, cause error, logger *slog.Logger) *report.Report {
	rep := Fallback(cfg, seq, reason, f)
	if raw, err := json.Marshal(rep); err == nil {
		if v, verr := report.Validate(raw); verr == nil {
			rep = v
		}
	}
	logger.Warn("fallback report built", "reason", reason, "error", cause)
	return rep
}
