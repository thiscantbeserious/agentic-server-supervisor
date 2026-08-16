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
// recommendation guard (§6 step 11b, deepdive.go).
package analyze

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

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

func newLogger(level slog.Level) *slog.Logger {
	w := logWriter
	if w == nil {
		w = os.Stderr
	}
	return slog.New(logging.New(w, level)).With("component", "analyze")
}

// Run performs §6. It returns a non-nil, valid report in every case EXCEPT
// a cancelled context (§1, §6 step 4): on errors.Is(err, context.Canceled)
// it returns (nil, err) and authors nothing, because a shutdown is not an
// analyzer failure and must not fabricate an ALERT. This is now a
// contract-level guarantee (§1), not just a code comment — callers
// (`tick`) MUST nil-check before marshaling; the SIGTERM path this
// exception exists to clean up is exactly where a nil-panic would land.
// Every other non-nil error means the returned document is the fallback.
// Run never panics and never writes outside the paths in §8.
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
	// findings needs findings that do not exist until AFTER the triage
	// call below, so it is never part of the prompt — only newest (kept
	// for computeResolved post-call) is needed here.
	hist := loadHistoryReports(cfg.StateDir, cfg.HistoryN)
	histLines := historyProjectionLines(hist)
	newest := newestHistory(hist)

	triagePrompt, err := buildTriagePrompt(cfg, o.Facts, histLines, nonce)
	if err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: assemble triage: %w", err)
	}

	promptPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("sentinel-prompt-%d.txt", pid))
	schemaPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("report.schema-%d.json", pid))
	cleanup = append(cleanup, promptPath, schemaPath)

	if err := os.WriteFile(promptPath, []byte(triagePrompt), 0o600); err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: write prompt: %w", err)
	}
	if err := os.WriteFile(schemaPath, report.SchemaJSON, 0o600); err != nil {
		return buildFallback(cfg, o.Seq, "internal_error", o.Facts, logger), fmt.Errorf("analyze: write schema: %w", err)
	}

	rep, reason, err := runTriage(ctx, o, d, promptPath, schemaPath, triagePrompt, logger)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// §6 step 4: "Cancellation is not an analyzer failure ...
			// return the cancellation error, author no report." A SIGTERM
			// during `tick --loop` must not fabricate an ALERT fallback,
			// log spurious warnings, or drive state/outbox writes during
			// shutdown — the one deliberate exception to "Run always
			// returns a non-nil report" (§9), because there is nothing
			// to report: the tick simply didn't happen.
			return nil, err
		}
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
