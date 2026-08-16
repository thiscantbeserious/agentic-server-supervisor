package analyze

import (
	"os"
	"path/filepath"
	"sort"

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
// the current report are removed as stale. Any error here is logged and
// ignored — deep-queue bookkeeping is never fatal (§5).
func manageDeepQueue(stateDir string, findings []report.Finding, candidateKey string) {
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
		_ = os.MkdirAll(dir, 0o700)
		_ = atomicWriteFile(dir, f.Key, []byte(f.Component+"\n"), 0o644)
	}

	if candidateKey != "" {
		_ = os.Remove(filepath.Join(dir, candidateKey))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !reportKeys[e.Name()] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
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
