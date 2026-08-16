package analyze

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// deepDiveCapable is §6 step 8's component set.
var deepDiveCapable = map[string]bool{"zfs": true, "smart": true, "kernel": true, "ras": true}

// noDeepDiveSuffix is appended verbatim (§6 step 8) to a NEW finding whose
// component has no deep collector.
const noDeepDiveSuffix = " (no deep-dive available for this component)"

// isNewFinding implements §6 step 8's NEW predicate: severity != "info" and
// ${STATE_DIR}/active-alerts/<key>.json does not exist. Any stat error
// (including a missing active-alerts/ directory) counts as "does not
// exist" — a fresh STATE_DIR makes every finding NEW.
func isNewFinding(stateDir string, f report.Finding) bool {
	if f.Severity == "info" || f.Key == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(stateDir, "active-alerts", f.Key+".json"))
	return err != nil
}

// selectCandidate implements the §6 step 8 candidate order: a deferred
// deep-queue entry outranks a fresh finding; otherwise the first NEW
// deep-dive-capable finding in severity order (alert before watch), ties
// broken by report order.
func selectCandidate(stateDir string, findings []report.Finding) (string, bool) {
	newDeepCapable := map[string]bool{}
	for _, f := range findings {
		if isNewFinding(stateDir, f) && deepDiveCapable[f.Component] {
			newDeepCapable[f.Key] = true
		}
	}
	if len(newDeepCapable) == 0 {
		return "", false
	}

	for _, qf := range queuedKeysByAge(stateDir) {
		if newDeepCapable[qf] {
			return qf, true
		}
	}

	bestKey := ""
	bestRank := -1
	for _, f := range findings {
		if !newDeepCapable[f.Key] {
			continue
		}
		rank := 0
		if f.Severity == "alert" {
			rank = 1
		}
		if rank > bestRank {
			bestRank = rank
			bestKey = f.Key
		}
	}
	return bestKey, bestRank >= 0
}

// queuedKeysByAge returns the deep-queue/ file names (== finding keys),
// oldest mtime first. A missing directory yields nil.
func queuedKeysByAge(stateDir string) []string {
	dir := filepath.Join(stateDir, "deep-queue")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type aged struct {
		name  string
		mtime int64
	}
	ordered := make([]aged, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ordered = append(ordered, aged{name: e.Name(), mtime: info.ModTime().UnixNano()})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].mtime < ordered[j].mtime })
	names := make([]string, len(ordered))
	for i, a := range ordered {
		names[i] = a.name
	}
	return names
}

// manageDeepQueue implements the queueing/eviction bookkeeping of §6 step
// 8: every other NEW deep-dive-capable finding is queued; the consumed
// candidate's queue file is removed; queue files whose key is absent from
// the current report are removed as stale. Any error here is non-fatal —
// deep-queue bookkeeping never gates analysis (§5) — but it is `slog`
// noted, not silently swallowed (§5 row 10: "$STATE_DIR unwritable or
// absent ... skipped with an slog note").
func manageDeepQueue(stateDir string, findings []report.Finding, candidateKey string, logger *slog.Logger) {
	dir := filepath.Join(stateDir, "deep-queue")
	reportKeys := map[string]bool{}
	for _, f := range findings {
		if f.Key != "" {
			reportKeys[f.Key] = true
		}
	}

	for _, f := range findings {
		if f.Key == "" || f.Key == candidateKey {
			continue
		}
		if !isNewFinding(stateDir, f) || !deepDiveCapable[f.Component] {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Warn("deep-queue bookkeeping skipped", "reason", "mkdir failed")
			return
		}
		if err := atomicWriteFile(dir, f.Key, []byte(f.Component+"\n"), 0o644); err != nil {
			logger.Warn("deep-queue bookkeeping skipped", "reason", "write failed")
		}
	}

	if candidateKey != "" {
		if err := os.Remove(filepath.Join(dir, candidateKey)); err != nil && !os.IsNotExist(err) {
			logger.Warn("deep-queue bookkeeping skipped", "reason", "remove candidate failed")
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("deep-queue bookkeeping skipped", "reason", "read dir failed")
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !reportKeys[e.Name()] {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				logger.Warn("deep-queue bookkeeping skipped", "reason", "remove stale failed")
			}
		}
	}
}

// runDeepDive performs §6 steps 8-11. Any failure along this path is
// non-fatal (§5): the caller already holds the validated triage report
// and this function only ever enriches it in place or leaves it untouched.
func runDeepDive(ctx context.Context, cfg *config.Config, o Options, d Deps, rep *report.Report, nonce string, histLines []string, pid int, cleanup *[]string, logger *slog.Logger) {
	appendNoDeepDiveSuffix(rep.Findings, cfg.StateDir)

	candidateKey, ok := selectCandidate(cfg.StateDir, rep.Findings)
	manageDeepQueue(cfg.StateDir, rep.Findings, candidateKey, logger)
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
		logger.Info("deep-dive failed, keeping triage report")
		return
	}

	// The candidate is sent to deep dive as-is (its dedup key included, for
	// the operator's own reference in the prompt) but the MERGE below
	// never trusts a key the model echoes back — this is the finding we
	// sent, identified by our own pointer (§6 step 11).
	findingJSON, err := json.Marshal(candidate)
	if err != nil {
		logger.Info("deep-dive failed, keeping triage report")
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

	// §6 step 10: PROMPT_MAX_BYTES applies to deep dive exactly as it does to
	// triage — a deep collect can reach FACTS_MAX_BYTES (262144), and an
	// unbudgeted argv string that large fails execve outright or hits
	// agy's silent-empty-answer cliff. Reduce a COPY, never deepFacts itself.
	deepDivePrompt, err := buildDeepDivePrompt(cfg, string(findingJSON), deepFacts, histLines, nonce, candidate.Component)
	if err != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	deepPromptPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("sentinel-deep-%d.txt", pid))
	if err := os.WriteFile(deepPromptPath, []byte(deepDivePrompt), 0o600); err != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	*cleanup = append(*cleanup, deepPromptPath)

	// §6 step 10: deep dive gets its OWN schema, not report.schema.json —
	// requiring the full report shape let the model copy a 16-hex key
	// wrong and silently lose the enrichment (key mismatch).
	deepDiveSchemaPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("deepdive.schema-%d.json", pid))
	if err := os.WriteFile(deepDiveSchemaPath, deepDiveSchemaJSON, 0o600); err != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	*cleanup = append(*cleanup, deepDiveSchemaPath)

	cctx, cancel2 := context.WithTimeout(ctx, cfg.AgyHardTimeout)
	defer cancel2()
	out, rerr := d.RunAgy(cctx, o, deepPromptPath, deepDiveSchemaPath)
	if rerr != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	// No retry at deep dive (§6 step 10) — an envelope failure here is just
	// another non-fatal enrichment failure, same as any other deep dive
	// problem (§5: "deep dive fails in any way ... non-fatal").
	response, everr := decodeAgyEnvelope(out)
	if everr != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	normalized := normalizeAgyOutput([]byte(response))
	deepDiveRep, verr := validateDeepDiveResponse(normalized)
	if verr != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}

	origAnalysis, origRecommendation, origHeadline := candidate.Analysis, candidate.Recommendation, rep.Headline
	candidate.Analysis = deepDiveRep.Analysis
	candidate.Recommendation = deepDiveRep.Recommendation
	if deepDiveRep.Headline != "" {
		// §6 step 11: the optional headline REPLACES triage's — the
		// notification title must not stay frozen on the shallow tick
		// view once the deep collect reveals something worse.
		rep.Headline = deepDiveRep.Headline
	}

	raw, merr := json.Marshal(rep)
	if merr != nil {
		candidate.Analysis, candidate.Recommendation, rep.Headline = origAnalysis, origRecommendation, origHeadline
		logger.Info("deep-dive failed, keeping triage report")
		return
	}
	if _, verr := report.Validate(raw); verr != nil {
		candidate.Analysis, candidate.Recommendation, rep.Headline = origAnalysis, origRecommendation, origHeadline
		logger.Info("deep-dive failed, keeping triage report")
	}
}

// deepDiveSchemaJSON is the deep dive RPC payload schema (§6 step 10): this
// document is never emitted to a user, so D3's "one schema, normative for
// everything the system emits" does not apply to it — it exists only so
// the model cannot copy/fabricate the full report shape (key, status,
// headline, ...) the way the old full-report deep dive schema let it.
//
//go:embed prompt/deepdive.schema.json
var deepDiveSchemaJSON []byte

// deepDiveResponse is the deep dive RPC payload (§6 step 10): analysis and
// recommendation for the one candidate finding, identified by the candidate
// analyze itself sent — never by a key the model echoes back — plus an
// optional headline that, when present, replaces triage's (§6 step 11).
type deepDiveResponse struct {
	Analysis       string `json:"analysis"`
	Recommendation string `json:"recommendation"`
	Headline       string `json:"headline,omitempty"`
}

// validateDeepDiveResponse is the hand-written bounds check for prompt/deepdive.schema.json
// (same D3 pattern as report.Validate: the schema file is what's handed to
// agy --json-schema, Go enforces it at runtime). No DisallowUnknownFields,
// consistent with report.Validate's own convention.
func validateDeepDiveResponse(raw []byte) (*deepDiveResponse, error) {
	var r deepDiveResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("deepdive: invalid JSON: %w", err)
	}
	if n := len([]rune(r.Analysis)); n < 1 || n > 1200 {
		return nil, fmt.Errorf("deepdive: analysis: length %d runes out of bounds [1,1200]", n)
	}
	if n := len([]rune(r.Recommendation)); n < 1 || n > 800 {
		return nil, fmt.Errorf("deepdive: recommendation: length %d runes out of bounds [1,800]", n)
	}
	if r.Headline != "" {
		if n := len([]rune(r.Headline)); n > 80 {
			return nil, fmt.Errorf("deepdive: headline: length %d runes exceeds maxLength 80", n)
		}
	}
	return &r, nil
}

// appendNoDeepDiveSuffix implements §6 step 8's last bullet: a NEW finding
// whose component has no deep collector gets the fixed suffix appended to
// its explanation (truncated first so the result stays <= 800 runes), and
// no analysis. Called only when DEEP_ENABLED=1 and status != OK (the
// caller gates this — "no suffix at all" when deep dives are switched off
// deliberately, per the contract's step-8 clarification).
func appendNoDeepDiveSuffix(findings []report.Finding, stateDir string) {
	for i, f := range findings {
		if !isNewFinding(stateDir, f) || deepDiveCapable[f.Component] {
			continue
		}
		max := 800 - len([]rune(noDeepDiveSuffix))
		expl := []rune(f.Explanation)
		if len(expl) > max {
			expl = expl[:max]
		}
		findings[i].Explanation = string(expl) + noDeepDiveSuffix
	}
}

// atomicWriteFile implements C4's write pattern: create-temp, write, sync,
// close, rename, matching internal/collect's atomicWrite.
func atomicWriteFile(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}
