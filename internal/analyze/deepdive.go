// deepdive.go: the second model call. Selecting which new finding earns the
// deep dive, the deferred-candidate queue, the focused second prompt, and
// merging the returned analysis into the triage report. Every failure on
// this path is non-fatal: the triage report is already valid and enrichment
// never becomes a gate.
//
// The binding spec is contracts/analyze.md.
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

// deepDiveCapable lists the components that have a deep collector.
var deepDiveCapable = map[string]bool{"zfs": true, "smart": true, "kernel": true, "ras": true}

// noDeepDiveSuffix is appended verbatim to a NEW finding whose component
// has no deep collector.
const noDeepDiveSuffix = " (no deep-dive available for this component)"

// isNewFinding reports whether a finding has not been seen as an active
// alert before. Any stat error counts as "new" — a fresh state directory
// makes every finding new, which is the safe direction.
func isNewFinding(stateDir string, f report.Finding) bool {
	if f.Severity == "info" || f.Key == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(stateDir, "active-alerts", f.Key+".json"))
	return err != nil
}

// selectCandidate picks the one finding that gets this tick's deep dive:
// a previously deferred finding from the queue outranks a fresh one
// (oldest first, so deferrals cannot starve), otherwise the first new
// deep-dive-capable finding, alerts before watches, ties broken by report
// order.
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

// manageDeepQueue keeps the deferred-candidate queue honest: every new
// deep-dive-capable finding that was not chosen is queued, the consumed
// candidate's entry is removed, and entries for findings no longer in the
// report are dropped as stale. Errors here never fail the tick — queue
// bookkeeping must not gate analysis — but each one leaves a log line
// rather than vanishing.
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

// runDeepDive performs the second model call and merges the result into the
// triage report. Every failure path returns with the report untouched: the
// caller already holds a valid report and enrichment is never a gate.
//
// The merge trusts only our own pointer to the candidate finding, never a
// key echoed back by the model. The returned headline, when present,
// replaces the triage headline: the headline becomes the notification
// title, and triage wrote it knowing only the shallow tick facts — if the
// deep collection reveals something worse, a stale headline misleads the
// operator at exactly the wrong moment.
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

	findingJSON, err := json.Marshal(candidate)
	if err != nil {
		logger.Info("deep-dive failed, keeping triage report")
		return
	}

	// Deliberately not using "component" as the attr key: the log handler
	// diverts any attr with that exact name into the line's component slot
	// (already "analyze" for this package), which would silently replace
	// it with "zfs"/"kernel"/etc. "target" avoids the collision.
	logger.Info("deep-dive", "target", candidate.Component, "key", candidateKey)

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
	// No retry at deep dive — an envelope failure is just another
	// non-fatal enrichment failure.
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

//go:embed prompt/deepdive.schema.json
var deepDiveSchemaJSON []byte

// deepDiveResponse is the deep-dive call's answer: analysis and
// recommendation for the one candidate finding, plus an optional
// replacement headline. It is deliberately not a full report: an earlier
// version required the report shape, and the model would copy the 16-hex
// finding key with one digit wrong, silently losing the enrichment to a
// key mismatch. The response now contains nothing worth copying.
type deepDiveResponse struct {
	Analysis       string `json:"analysis"`
	Recommendation string `json:"recommendation"`
	Headline       string `json:"headline,omitempty"`
}

// validateDeepDiveResponse enforces the same bounds as
// prompt/deepdive.schema.json. The schema file is what agy receives; Go
// enforces it at runtime because model output is unvalidated input either
// way. Keep the two in lockstep.
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

// appendNoDeepDiveSuffix marks new findings whose component has no deep
// collector, so the operator knows the missing analysis is a capability
// gap, not an omission. The explanation is truncated first so the result
// stays within the schema's length bound. Callers skip this entirely when
// deep dives are disabled: the operator switched the feature off and does
// not need every finding annotated with that fact.
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

// atomicWriteFile writes via create-temp, write, sync, close, rename, so a
// crash mid-write can never leave a torn or partial file for a later tick
// to read.
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
