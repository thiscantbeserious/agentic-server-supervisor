// rawalert.go: the LLM-free critical-path raw alert (R3.3/R3.4). Runs
// before analysis so a crashing or quota-blocked agy can never delay or
// swallow a critical kernel event (ARCHITECTURE design principle 4).
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// RawCandidate is one kernel entry eligible for the raw-alert path.
type RawCandidate struct {
	Entry facts.Entry
	Key   string // dedup.Key("kernel", Entry.Message)
}

// Candidates returns kernel entries with Priority <= maxPriority, newest
// first (R3.3). A missing or failed kernel section yields no candidates —
// callers must probe kernelScanFailed separately to tell "no crit lines"
// apart from "the scan itself failed".
func Candidates(f *facts.Facts, maxPriority int) []RawCandidate {
	if f == nil || f.Kernel == nil || f.Kernel.Err != "" {
		return nil
	}
	entries := f.Kernel.Data.Entries
	out := make([]RawCandidate, 0, len(entries))
	// Entries are stored oldest-first (journal/collect convention); walk
	// backwards so candidates come out newest first, per R3.3.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Priority > maxPriority {
			continue
		}
		out = append(out, RawCandidate{Entry: e, Key: dedup.Key("kernel", e.Message)})
	}
	return out
}

// kernelScanFailed distinguishes "the kernel section carries an error, or
// facts shape drift" (R3.3's own failure case) from "zero candidates".
func kernelScanFailed(f *facts.Facts) (bool, string) {
	if f == nil {
		return true, "no facts document"
	}
	if f.Kernel == nil {
		return true, "no kernel section in facts"
	}
	if f.Kernel.Err != "" {
		return true, f.Kernel.Err
	}
	return false, ""
}

const rawAlertsDir = "raw-alerts"

// scanFailedMarkerKey is R3.3's "reserved key scan-failed" — the one
// marker in the raw-alerts/ directory not shaped like a dedup.Key,
// deliberately: it identifies "the scan itself is broken" rather than any
// one candidate, and must never collide with a real kernel-message key.
const scanFailedMarkerKey = "scan-failed"

func markerPath(stateDir, key string) string {
	return filepath.Join(stateDir, rawAlertsDir, key)
}

// isSuppressed is R3.3: a candidate is suppressed iff its marker exists
// and its mtime is newer than repeat. RAW_ALERT_REPEAT_SECONDS=0
// suppresses nothing.
func isSuppressed(stateDir, key string, now time.Time, repeat time.Duration) bool {
	if repeat <= 0 {
		return false
	}
	info, err := os.Stat(markerPath(stateDir, key))
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) < repeat
}

// writeMarker records that key was sent NOW, so a later tick within
// RAW_ALERT_REPEAT_SECONDS suppresses it (R3.3): content is the RFC3339
// UTC timestamp, atomic temp+rename per C4.
func writeMarker(stateDir, key string, now time.Time) error {
	dir := filepath.Join(stateDir, rawAlertsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(now.UTC().Format(time.RFC3339)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, markerPath(stateDir, key))
}

// sweepMarkers unlinks markers older than ttl, at the start of each tick
// (R3.3).
func sweepMarkers(stateDir string, now time.Time, ttl time.Duration) {
	dir := filepath.Join(stateDir, rawAlertsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > ttl {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

var priorityNames = map[int]string{
	0: "emerg", 1: "alert", 2: "crit", 3: "err",
	4: "warning", 5: "notice", 6: "info", 7: "debug",
}

func priorityName(p int) string {
	if n, ok := priorityNames[p]; ok {
		return n
	}
	return fmt.Sprintf("priority-%d", p)
}

// BuildRawReport is R3.4: a valid report.Report for the given (already
// marker-filtered and RAW_ALERT_MAX_LINES-capped) candidates. droppedByCap
// is how many additional unsuppressed candidates did not fit the cap.
func BuildRawReport(cfg *config.Config, seq int64, cands []RawCandidate, droppedByCap int) *report.Report {
	var body strings.Builder
	body.WriteString("Raw kernel alert, sent without analysis (LLM-free path).\n\n")
	findings := make([]report.Finding, 0, len(cands))
	for _, c := range cands {
		// report.schema.json bounds evidence at 1000 runes and body at
		// 2000 (C5) — a single crafted kernel line must not blow either,
		// the same discipline analyze.Fallback applies to its own
		// RAWLINES (contracts/analyze.md §5).
		msg := truncRunesR(c.Entry.Message, 1000)
		fmt.Fprintf(&body, "%s %s %s\n", c.Entry.TS, priorityName(c.Entry.Priority), msg)
		findings = append(findings, report.Finding{
			Severity:  "alert",
			Component: "kernel",
			Evidence:  msg,
			Explanation: fmt.Sprintf(
				"Kernel logged a priority-%d (%s) message. Sent unanalysed on the LLM-free critical path.",
				c.Entry.Priority, priorityName(c.Entry.Priority)),
			Key: c.Key,
		})
	}
	if droppedByCap > 0 {
		fmt.Fprintf(&body, "\n… (%d more suppressed)\n", droppedByCap)
	}
	body.WriteString("\nA full analysis follows in the next report if the analyzer is available.")

	headline := truncRunesR(fmt.Sprintf("%d critical kernel event(s) on %s", len(cands), cfg.Hostname), 80)

	return &report.Report{
		Status:   "ALERT",
		Headline: headline,
		Body:     truncRunesR(body.String(), 2000),
		Findings: findings,
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: cfg.Hostname, TickSeq: seq, Raw: true},
	}
}

// buildScanFailedReport is R3.3's own failure case: the kernel section
// itself failed (error, or facts shape drift). The safety path fails
// loud, never silent.
func buildScanFailedReport(cfg *config.Config, seq int64, reason string) *report.Report {
	headline := "Raw-alert scan failed — critical kernel events may be unseen"
	body := "The kernel section of this tick's facts could not be scanned for critical events: " + reason +
		". Hardware alerts do not depend on this path alone - the deterministic smartd/ZED mail paths are unaffected."
	key := dedup.Key("meta", "raw-alert scan failed")
	return &report.Report{
		Status:   "ALERT",
		Headline: truncRunesR(headline, 80),
		Body:     truncRunesR(body, 2000),
		Findings: []report.Finding{{
			Severity:    "alert",
			Component:   "meta",
			Evidence:    truncRunesR(reason, 1000),
			Explanation: "Raw-alert scan failed; critical kernel events may be unseen this tick.",
			Key:         key,
		}},
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: cfg.Hostname, TickSeq: seq, Raw: true},
	}
}

// scanRawAlerts is the full R3.3 orchestration for step 1b: sweep expired
// markers, detect a scan failure, or select/cap/mark candidates. It
// returns the report to send (nil ⇒ nothing to send this tick), the count
// of findings it carries, and whether this was the scan-failure case (the
// safety path — always visible in the exit code, delivery success or
// not).
func scanRawAlerts(cfg *config.Config, f *facts.Facts, now time.Time) (rep *report.Report, findingCount int, scanFailed bool) {
	sweepMarkers(cfg.StateDir, now, time.Duration(cfg.RawAlertMarkerTTLHours)*time.Hour)

	if failed, reason := kernelScanFailed(f); failed {
		// R3.3 (amended 64e57f3): the scan-failure alert is marker-
		// suppressed like any other raw alert, under the reserved key
		// "scan-failed" — but scanFailed is still true, unconditionally,
		// so the exit code stays non-zero on EVERY failing tick even on
		// a tick where the human channel is throttled. "Fails loud,
		// never silent" holds for the exit code, the log line and
		// sentinel health; only the notification is throttled, because
		// alert fatigue loses events as effectively as a swallowed alert.
		repeat := time.Duration(cfg.RawAlertRepeatSeconds) * time.Second
		if isSuppressed(cfg.StateDir, scanFailedMarkerKey, now, repeat) {
			return nil, 0, true
		}
		writeMarker(cfg.StateDir, scanFailedMarkerKey, now)
		rep := buildScanFailedReport(cfg, cfg.TickSeq, reason)
		return rep, len(rep.Findings), true
	}

	candidates := Candidates(f, cfg.RawAlertMaxPriority)
	if len(candidates) == 0 {
		return nil, 0, false
	}

	unsuppressed := make([]RawCandidate, 0, len(candidates))
	for _, c := range candidates {
		if !isSuppressed(cfg.StateDir, c.Key, now, time.Duration(cfg.RawAlertRepeatSeconds)*time.Second) {
			unsuppressed = append(unsuppressed, c)
		}
	}
	if len(unsuppressed) == 0 {
		return nil, 0, false
	}

	maxLines := cfg.RawAlertMaxLines
	capped := unsuppressed
	dropped := 0
	if len(unsuppressed) > maxLines {
		capped = unsuppressed[:maxLines]
		dropped = len(unsuppressed) - maxLines
	}

	for _, c := range capped {
		writeMarker(cfg.StateDir, c.Key, now)
	}

	return BuildRawReport(cfg, cfg.TickSeq, capped, dropped), len(capped), false
}
