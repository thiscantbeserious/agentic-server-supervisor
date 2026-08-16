package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/dedup"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

var (
	ErrStateDir  = errors.New("state dir not writable")
	ErrBadInput  = errors.New("invalid input json")
	ErrUnknownID = errors.New("unknown outbox id")
)

// severityRank orders info < watch < alert (S.3d) — the single ranking
// used both to detect escalation and to pick the outgoing status (g).
var severityRank = map[string]int{"info": 0, "watch": 1, "alert": 2}

// validKeyRe is dedup.Key's own output shape (C6, report.schema.json). A
// finding's supplied key is honoured only when it matches; anything else
// is recomputed rather than joined into a path (S.3d write containment).
var validKeyRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

type Decision struct {
	Notify          bool          `json:"notify"`
	Reason          string        `json:"reason"`
	TickSeq         int64         `json:"tick_seq"`
	SuppressedCount int           `json:"suppressed_count"`
	ActiveCount     int           `json:"active_count"`
	Heartbeat       bool          `json:"heartbeat"`
	Report          report.Report `json:"report"`
}

type Store struct {
	cfg *config.Config
}

// C4: dirs 0o700.
const dirMode = 0o700

func New(cfg *config.Config) (*Store, error) {
	// Probe STATE_DIR is writable (S.6: missing/not-a-dir/not-writable -> 69).
	testFile := filepath.Join(cfg.StateDir, ".probe")
	if err := os.WriteFile(testFile, []byte{}, 0o600); err != nil {
		return nil, ErrStateDir
	}
	os.Remove(testFile)

	for _, dir := range []string{"active-alerts", "history", "outbox"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, dir), dirMode); err != nil {
			return nil, ErrStateDir
		}
	}

	// New deliberately does NOT seed the heartbeat file. A supervisor that
	// has never run must read as unhealthy — seeding it here (even with
	// "empty" content) gives it a fresh mtime the moment `sentinel health`
	// happens to call New(), which makes health report HEALTHY on a
	// container that never ticked. Only Process ever creates or rewrites
	// heartbeat; Health() is naturally "missing file -> error" until then.
	return &Store{cfg: cfg}, nil
}

func (s *Store) now() int64 {
	if !s.cfg.Now.IsZero() {
		return s.cfg.Now.Unix()
	}
	return time.Now().Unix()
}

// resolveTickSeq is S.3(a): cfg.TickSeq if > 0, else report.meta.tick_seq
// if present, else 0. This is the single tick_seq used everywhere in this
// Process call — the outgoing Decision, the history filename, and the
// ActiveAlert tick_seq_first/last bookkeeping — never written back to disk
// (state owns no tick-seq file; that's runtime's, S-D3).
func resolveTickSeq(cfg *config.Config, rep *report.Report) int64 {
	if cfg.TickSeq > 0 {
		return cfg.TickSeq
	}
	if rep.Meta != nil && rep.Meta.TickSeq > 0 {
		return rep.Meta.TickSeq
	}
	return 0
}

func (s *Store) Process(raw []byte) (*Decision, error) {
	now := s.now()

	// S.2: "not an object, or no findings array -> exit 65". Input
	// validation is deliberately permissive beyond that — S.7 requires a
	// state failure never lose an alert ("tick sends the report
	// unfiltered"), so an input that report.Validate would reject (e.g. an
	// 81-rune headline) must still be processed. What must validate against
	// report.schema.json is the OUTGOING decision.report (S-D6), not this.
	var probe struct {
		Findings *[]json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Findings == nil {
		return nil, ErrBadInput
	}
	var rep report.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, ErrBadInput
	}
	if rep.Resolved == nil {
		rep.Resolved = []string{}
	}

	// S.3(b): history stores the INPUT document (status/headline/body/
	// resolved), not whatever step (g) below mutates rep into — snapshot
	// it now, before any of c/d/e/f/g touch rep. Findings are annotated
	// separately below (into `annotated`, in original order); Status/
	// Headline/Body/Resolved must survive byte-for-byte.
	origStatus, origHeadline, origBody := rep.Status, rep.Headline, rep.Body
	origResolved := rep.Resolved

	tickSeq := resolveTickSeq(s.cfg, &rep)
	decision := &Decision{TickSeq: tickSeq}

	// c) stale expiry, first, silent.
	s.expireStaleAlerts(now)

	// d) per finding, in input order.
	notified := make([]report.Finding, 0, len(rep.Findings))
	annotated := make([]report.Finding, len(rep.Findings)) // full input order, for history (S.3b)
	touchedKeys := make(map[string]bool, len(rep.Findings))
	suppressedCount := 0
	reason := ""

	for i, f := range rep.Findings {
		// A supplied key is trusted only if it already matches dedup.Key's
		// own output shape. `key` is joined straight into a filesystem path
		// below (loadAlert/saveAlert), so an attacker-shaped value such as
		// "../../pwned" or "../history/x" must never reach that join — it
		// escapes $STATE_DIR (S.2, A1) or drops a non-conforming file into
		// a sibling directory analyze/history readers trust. Recomputing
		// is free: dedup.Key always conforms.
		key := f.Key
		if !validKeyRe.MatchString(key) {
			key = dedup.Key(f.Component, f.Evidence) // C6: Key normalizes internally; never double-normalize
		}
		touchedKeys[key] = true

		alert, exists := s.loadAlert(key)
		isNotify := false
		findingReason := ""
		if !exists {
			alert = &ActiveAlert{
				Key:          key,
				Component:    f.Component,
				EvidenceCore: dedup.EvidenceCore(f.Evidence),
				Headline:     rep.Headline,
				Severity:     f.Severity,
				FirstSeen:    now,
				LastSeen:     now,
				TickSeqFirst: tickSeq,
				TickSeqLast:  tickSeq,
				Occurrences:  1,
			}
			isNotify = true
			findingReason = "new_finding"
		} else {
			alert.LastSeen = now
			alert.Occurrences++
			alert.TickSeqLast = tickSeq

			if severityRank[f.Severity] > severityRank[alert.Severity] {
				alert.Severity = f.Severity
				isNotify = true
				findingReason = "escalation"
			} else {
				if f.Severity != alert.Severity {
					// De-escalation never notifies on its own, but it does
					// lower the stored severity and switch the renotify
					// window (S.3d).
					alert.Severity = f.Severity
				}
				window := s.cfg.RenotifyAlertSec
				if alert.Severity != "alert" {
					window = s.cfg.RenotifyWatchSec
				}
				if now-alert.LastNotified >= int64(window) {
					isNotify = true
					findingReason = "renotify"
				}
			}
		}

		if isNotify {
			alert.LastNotified = now
			alert.NotifyCount++
			if reason == "" {
				reason = findingReason
			}
		} else {
			suppressedCount++
		}
		if err := s.saveAlert(alert); err != nil {
			return nil, fmt.Errorf("state: save active alert: %w", err)
		}

		f.Key = alert.Key
		f.FirstSeen = alert.FirstSeen
		f.Occurrences = alert.Occurrences
		annotated[i] = f
		if isNotify {
			notified = append(notified, f)
		}
	}

	// e) resolved / all-clear — only alerts NOT touched in step (d) (S-D7).
	var allClear []string
	for _, res := range rep.Resolved {
		res = strings.TrimSpace(strings.ToLower(res))
		if res == "" {
			continue
		}
		alertFiles, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "active-alerts"))
		for _, af := range alertFiles {
			alert, err := s.loadAlertByFile(filepath.Join(s.cfg.StateDir, "active-alerts", af.Name()))
			if err != nil || touchedKeys[alert.Key] {
				continue
			}
			if strings.TrimSpace(strings.ToLower(alert.Headline)) != res {
				continue
			}
			// S.3(e): "A key that was never notified is deleted without an
			// all-clear." — always delete on a match, but only surface it
			// as an all-clear when the operator was actually told about it.
			if alert.NotifyCount > 0 {
				found := false
				for _, existing := range allClear {
					if existing == alert.Headline {
						found = true
						break
					}
				}
				if !found {
					allClear = append(allClear, alert.Headline)
				}
			}
			// Delete by the directory entry name (guaranteed a single path
			// element by os.ReadDir), never by alert.Key — that field comes
			// from the stored record's own JSON body, which may be stale,
			// hand-edited, or (on an older build, before the step-(d)
			// adoption guard) attacker-shaped, and can disagree with the
			// filename it was read from.
			os.Remove(filepath.Join(s.cfg.StateDir, "active-alerts", af.Name()))
		}
	}

	// f) heartbeat due-check.
	hbPath := filepath.Join(s.cfg.StateDir, "heartbeat")
	loc := s.cfg.Loc
	if loc == nil {
		loc = time.UTC // defensive: config.Load always resolves this, but Process must not panic if a caller skips it
	}
	hbTime := time.Unix(now, 0).In(loc)
	hbStr := hbTime.Format("2006-01-02")
	hbData, _ := os.ReadFile(hbPath)
	hbCurrent := strings.TrimSpace(string(hbData))
	hbDue := hbCurrent != hbStr && hbTime.Hour() >= s.cfg.HeartbeatHour

	// g) message assembly — first matching rule.
	switch {
	case len(notified) > 0:
		decision.Notify = true
		decision.Reason = reason

		highest := 0
		for _, f := range notified {
			if r := severityRank[f.Severity]; r > highest {
				highest = r
			}
		}
		statusByRank := []string{"OK", "WATCH", "ALERT"}
		rep.Status = statusByRank[highest]
		rep.Findings = notified
		rep.Resolved = allClear
		if rep.Resolved == nil {
			rep.Resolved = []string{}
		}

	case len(allClear) > 0:
		decision.Notify = true
		decision.Reason = "all_clear"
		rep.Status = "OK"
		rep.Findings = []report.Finding{}
		rep.Resolved = allClear
		rep.Headline = allClearHeadline(allClear)
		var body strings.Builder
		for i, ac := range allClear {
			if i > 0 {
				body.WriteByte('\n')
			}
			body.WriteString("- " + ac)
		}
		rep.Body = body.String()

	case hbDue:
		decision.Notify = true
		decision.Reason = "heartbeat"
		decision.Heartbeat = true
		rep.Status = "OK"
		rep.Headline = "Daily heartbeat: all clear"
		rep.Findings = []report.Finding{}
		rep.Resolved = []string{}
		rep.Body = heartbeatBody(s.cfg.StateDir, now)

	default:
		decision.Reason = "suppressed"
		rep.Status = "OK"
		rep.Findings = []report.Finding{}
		rep.Resolved = []string{}
	}

	// f, continued) content advances to today only when a notification was
	// actually emitted this tick; the file is rewritten every Process call
	// regardless, so mtime always tracks liveness (S.3f).
	hbContent := hbCurrent
	if decision.Notify {
		hbContent = hbStr
	}
	if err := writeAtomic(s.cfg.StateDir, "heartbeat", []byte(hbContent+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("state: write heartbeat: %w", err)
	}

	decision.Report = rep
	decision.SuppressedCount = suppressedCount

	alertFiles, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "active-alerts"))
	decision.ActiveCount = len(alertFiles)

	// b) history write — after (d), because it needs the post-update
	// annotations; every finding (notified AND suppressed), ORIGINAL INPUT
	// status/headline/body/resolved and finding order, never verbatim
	// (findings are annotated) and never decision.report — decision.report
	// is what step (g) mutated rep into, not the input (S.3b).
	if err := s.writeAnnotatedHistory(now, tickSeq, origStatus, origHeadline, origBody, origResolved, rep.Meta, annotated); err != nil {
		return nil, fmt.Errorf("state: write history: %w", err)
	}

	// S-D6: the outgoing document must validate against report.schema.json.
	outBytes, err := json.Marshal(decision.Report)
	if err != nil {
		return nil, fmt.Errorf("state: marshal decision.report: %w", err)
	}
	if _, err := report.Validate(outBytes); err != nil {
		return nil, fmt.Errorf("state: decision.report failed schema validation (bug): %w", err)
	}

	return decision, nil
}

// allClearHeadline is S.3(g) rule 2: "Resolved: <first>" (+N more) when
// more than one, truncated to 80 runes so the schema bound holds.
func allClearHeadline(allClear []string) string {
	headline := allClear[0]
	if len(allClear) > 1 {
		headline += fmt.Sprintf(" (+%d more)", len(allClear)-1)
	}
	full := "Resolved: " + headline
	runes := []rune(full)
	if len(runes) > 80 {
		full = string(runes[:80])
	}
	return full
}

// heartbeatBody is S.3(g) rule 3: "No open findings. <k> ticks since
// <RFC3339 UTC of the oldest kept history entry>." k = number of kept
// history files that exist BEFORE this tick's own entry is written. now is
// this Process call's clock (C9: never time.Now(), always cfg.Now/the
// caller's clock) — used as the fallback "since" when history/ is still
// empty (a fresh $STATE_DIR's first heartbeat), a case the contract text
// doesn't spell out explicitly.
func heartbeatBody(stateDir string, now int64) string {
	histDir := filepath.Join(stateDir, "history")
	files, _ := os.ReadDir(histDir) // ReadDir returns entries sorted by name (== chronological)
	k := len(files)
	since := time.Unix(now, 0).UTC()
	if k > 0 {
		epochStr, _, _ := strings.Cut(files[0].Name(), "-")
		if epoch, err := strconv.ParseInt(epochStr, 10, 64); err == nil {
			since = time.Unix(epoch, 0).UTC()
		}
	}
	return fmt.Sprintf("No open findings. %d ticks since %s.", k, since.Format(time.RFC3339))
}

func (s *Store) writeAnnotatedHistory(now, tickSeq int64, status, headline, body string, resolved []string, meta *report.Meta, annotated []report.Finding) error {
	histRep := report.Report{
		Status:   status,
		Headline: headline,
		Body:     body,
		Findings: annotated,
		Resolved: resolved,
		Meta:     meta,
	}
	if histRep.Findings == nil {
		histRep.Findings = []report.Finding{}
	}
	if histRep.Resolved == nil {
		histRep.Resolved = []string{}
	}

	name := fmt.Sprintf("%010d-%06d.json", now, tickSeq)
	data, err := json.Marshal(histRep)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := writeAtomic(s.cfg.StateDir, filepath.Join("history", name), data, 0o644); err != nil {
		return err
	}

	s.rotateHistory()
	return nil
}

func (s *Store) rotateHistory() {
	histDir := filepath.Join(s.cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)

	if len(files) > s.cfg.HistoryKeep {
		sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
		toDelete := len(files) - s.cfg.HistoryKeep
		for i := 0; i < toDelete; i++ {
			os.Remove(filepath.Join(histDir, files[i].Name()))
		}
	}
}

func (s *Store) expireStaleAlerts(now int64) {
	alertDir := filepath.Join(s.cfg.StateDir, "active-alerts")
	files, _ := os.ReadDir(alertDir)

	for _, f := range files {
		alert, err := s.loadAlertByFile(filepath.Join(alertDir, f.Name()))
		if err != nil {
			os.Remove(filepath.Join(alertDir, f.Name()))
			continue
		}
		if now-alert.LastSeen > int64(s.cfg.StaleAlertSec) {
			os.Remove(filepath.Join(alertDir, f.Name()))
		}
	}
}

func (s *Store) History(n int) ([]json.RawMessage, error) {
	histDir := filepath.Join(s.cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)

	sort.Slice(files, func(i, j int) bool { return files[i].Name() > files[j].Name() }) // newest first

	// Unparsable files are skipped and DON'T count toward n (S.7): stop
	// only once n valid entries are collected or the directory is
	// exhausted, not after scanning the first n directory positions —
	// otherwise a single corrupt file among the newest N silently shrinks
	// the trend window analyze reads.
	result := make([]json.RawMessage, 0, min(n, len(files)))
	for _, f := range files {
		if len(result) >= n {
			break
		}
		data, err := os.ReadFile(filepath.Join(histDir, f.Name()))
		if err == nil && json.Valid(data) {
			result = append(result, json.RawMessage(data))
		}
	}
	return result, nil
}

func (s *Store) Health() error {
	hbPath := filepath.Join(s.cfg.StateDir, "heartbeat")
	info, err := os.Stat(hbPath)
	if err != nil {
		return err
	}

	now := time.Now()
	if !s.cfg.Now.IsZero() {
		now = s.cfg.Now
	}

	if now.Sub(info.ModTime()) > 3*s.cfg.TickInterval {
		return errors.New("heartbeat stale")
	}
	return nil
}
