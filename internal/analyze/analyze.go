// Package analyze turns one tick's collected facts into a human-readable
// report by calling an LLM, and survives every way that call can fail.
//
// The pipeline makes at most two model calls per tick. The triage call
// receives all facts plus a window of recent reports and returns the full
// report: findings with severities, a status, a headline. If triage surfaces
// a new finding for a component that has a deep collector, the deep-dive
// call receives that one finding plus a focused deep collection and returns
// a grounded analysis and a conditional recommendation for it. If the model
// is unreachable or keeps returning garbage, a deterministic fallback report
// surfaces the raw high-priority kernel lines instead, so no hardware event
// is ever lost to an LLM outage.
//
// This package is the security boundary between attacker-controlled log
// text and an LLM prompt. Anything an attacker can write to a log on the
// monitored host ends up inside these prompts, so every payload is wrapped
// in fences marked with a fresh random nonce per run: injected text cannot
// forge a fence end it cannot predict, and the prompt instructs the model
// to treat everything inside the fences as data. The model has no tools and
// executes nothing; a successful injection is limited to wrong text in a
// report, and the recommendation field, the one field an operator might
// paste into a shell, additionally passes a deterministic deny-list guard.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/collect"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/dedup"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/facts"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/logging"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/report"
)

// Options is Run's per-tick input: configuration, the facts collected this
// tick, and the tick sequence number stamped into the report.
type Options struct {
	Cfg   *config.Config
	Facts *facts.Facts
	Seq   int64
}

// Deps holds the two operations tests replace: running agy and collecting
// deep facts. Plain function fields, not interfaces, there is exactly one
// real implementation of each.
type Deps struct {
	RunAgy      func(ctx context.Context, o Options, promptPath, schemaPath string) ([]byte, error)
	CollectDeep func(ctx context.Context, component string) (*facts.Facts, error)
}

// DefaultDeps wires the real agy subprocess and the in-process deep
// collector.
func DefaultDeps(cfg *config.Config) Deps {
	return Deps{
		RunAgy: func(ctx context.Context, o Options, promptPath, schemaPath string) ([]byte, error) {
			return runAgy(ctx, o.Cfg, promptPath, schemaPath)
		},
		CollectDeep: func(ctx context.Context, component string) (*facts.Facts, error) {
			// Seq is deliberately not threaded through: deep facts' tick number is
			// informational and nothing keys on it.
			return collect.Run(ctx, collect.Options{Cfg: cfg, DeepComponent: component})
		},
	}
}

// logWriter is where this package's log output goes. Tests point it at a
// buffer to assert on the exact lines the spec fixes; the zero value means
// stderr.
var logWriter io.Writer

func newLogger(level slog.Level) *slog.Logger {
	w := logWriter
	if w == nil {
		w = os.Stderr
	}
	return slog.New(logging.New(w, level)).With("component", "analyze")
}

// Run executes one analysis tick: triage call, optional deep dive, guard,
// and returns a validated report.
//
// Run returns a non-nil, valid report in every case except one: when ctx
// was cancelled it returns (nil, err) and authors nothing, because a
// shutdown is not an analyzer failure and must not fabricate an ALERT.
// Callers must nil-check before using the report. Any other non-nil error
// means the returned report is the deterministic fallback. Run never panics
// and writes only under TmpDir, the deep-dive queue in StateDir, and agy's
// home directory (created if absent, since agy refuses to start without it).
func Run(ctx context.Context, o Options, d Deps) (*report.Report, error) {
	cfg := o.Cfg
	logger := newLogger(logging.ParseLevel(cfg.LogLevel))
	pid := os.Getpid()

	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			os.Remove(p)
		}
	}()

	// A nil RunAgy (misconstructed Deps) is not a documented failure mode,
	// but the alternative is a nil-pointer panic on the first call below,
	// treat it the same as agy being missing rather than crashing.
	if d.RunAgy == nil {
		runAgyNilErr := errors.New("analyze: Deps.RunAgy is nil")
		return buildFallback(cfg, o.Seq, "agy_missing", o.Facts, runAgyNilErr, logger), runAgyNilErr
	}

	// internal_error (not agy_failed) below: these paths fail before agy
	// is ever invoked, so blaming "the analyzer exited non-zero" would send
	// a 3am reader to check agy's health for a fault that is ours.
	nonce, err := newNonce()
	if err != nil {
		wrapped := fmt.Errorf("analyze: nonce: %w", err)
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, wrapped, logger), wrapped
	}

	// The resolved set is output-only: it needs this tick's findings, which
	// do not exist until after the triage call below, so it is never part
	// of the prompt, only the eligible history report (kept for
	// computeResolved) is needed here.
	//
	// Read up to HISTORY_KEEP, not HISTORY_N: HISTORY_N bounds the prompt
	// window because that window costs prompt tokens, but the resolved
	// diff below is pure Go set arithmetic over files already on disk and
	// pays no token cost, so reusing the token-driven number here was an
	// accident of implementation, not a decision (issue #39). Reading up
	// to 50 small JSON files on a tick that already runs an LLM call is
	// not worth optimising against, and it is what lets the walk-back
	// below survive an outage longer than the ~25 minutes HISTORY_N alone
	// would cover.
	hist := loadHistoryReports(cfg.StateDir, cfg.HistoryKeep)
	promptHist := hist
	if len(promptHist) > cfg.HistoryN {
		promptHist = promptHist[len(promptHist)-cfg.HistoryN:]
	}
	histLines := historyProjectionLines(promptHist)
	newest, exhausted := newestEligible(hist)
	if exhausted {
		// Every retained entry is a degraded fallback tick: a continuous
		// outage longer than HISTORY_KEEP ticks. A finding open before it
		// cannot be proven resolved from anything on disk, and it will
		// orphan exactly as it did before this fix (contracts/analyze.md
		// §6 step 7 states this as the walk-back's residual limit). That
		// must never be silent (ARCHITECTURE §5): this WARN is the only
		// trace an operator has that the walk-back gave up.
		logger.Warn("resolve: walk-back exhausted history, no non-degraded entry to diff against", "retained", len(hist))
	}

	triagePrompt, err := buildTriagePrompt(cfg, o.Facts, histLines, nonce)
	if err != nil {
		wrapped := fmt.Errorf("analyze: assemble triage: %w", err)
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, wrapped, logger), wrapped
	}

	promptPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("sentinel-prompt-%d.txt", pid))
	schemaPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("report.schema-%d.json", pid))
	cleanup = append(cleanup, promptPath, schemaPath)

	if err := os.WriteFile(promptPath, []byte(triagePrompt), 0o600); err != nil {
		wrapped := fmt.Errorf("analyze: write prompt: %w", err)
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, wrapped, logger), wrapped
	}
	if err := os.WriteFile(schemaPath, report.SchemaJSON, 0o600); err != nil {
		wrapped := fmt.Errorf("analyze: write schema: %w", err)
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, wrapped, logger), wrapped
	}

	rep, reason, err := runTriage(ctx, o, d, promptPath, schemaPath, triagePrompt, logger)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A shutdown is not an analyzer failure: author no report.
			return nil, err
		}
		return buildFallback(cfg, o.Seq, reason, o.Facts, err, logger), err
	}

	// Inject keys, meta and resolved; drop any model-supplied
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
