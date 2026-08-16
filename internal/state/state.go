package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Seed the heartbeat file so its mtime exists from the first Process
	// call; empty content is "absent" for the S.3(f) due-check.
	hbPath := filepath.Join(cfg.StateDir, "heartbeat")
	if _, err := os.Stat(hbPath); err != nil {
		writeAtomic(cfg.StateDir, "heartbeat", []byte("\n"), 0o644)
	}

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
		key := f.Key
		if key == "" {
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
		s.saveAlert(alert)

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
			os.Remove(filepath.Join(s.cfg.StateDir, "active-alerts", alert.Key+".json"))
		}
	}

	// f) heartbeat due-check.
	hbPath := filepath.Join(s.cfg.StateDir, "heartbeat")
	hbTime := time.Unix(now, 0).In(s.cfg.Loc)
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
		rep.Body = heartbeatBody(s.cfg.StateDir)

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
	writeAtomic(s.cfg.StateDir, "heartbeat", []byte(hbContent+"\n"), 0o644)

	decision.Report = rep
	decision.SuppressedCount = suppressedCount

	alertFiles, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "active-alerts"))
	decision.ActiveCount = len(alertFiles)

	// b) history write — after (d), because it needs the post-update
	// annotations; every finding (notified AND suppressed), original
	// input order, never verbatim and never decision.report (S.3b).
	s.writeAnnotatedHistory(now, tickSeq, &rep, annotated)

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
// history files that exist BEFORE this tick's own entry is written.
func heartbeatBody(stateDir string) string {
	histDir := filepath.Join(stateDir, "history")
	files, _ := os.ReadDir(histDir) // ReadDir returns entries sorted by name (== chronological)
	k := len(files)
	since := time.Now().UTC()
	if k > 0 {
		epochStr, _, _ := strings.Cut(files[0].Name(), "-")
		if epoch, err := strconv.ParseInt(epochStr, 10, 64); err == nil {
			since = time.Unix(epoch, 0).UTC()
		}
	}
	return fmt.Sprintf("No open findings. %d ticks since %s.", k, since.Format(time.RFC3339))
}

func (s *Store) writeAnnotatedHistory(now, tickSeq int64, rep *report.Report, annotated []report.Finding) {
	histRep := report.Report{
		Status:   rep.Status,
		Headline: rep.Headline,
		Body:     rep.Body,
		Findings: annotated,
		Resolved: rep.Resolved,
		Meta:     rep.Meta,
	}
	if histRep.Findings == nil {
		histRep.Findings = []report.Finding{}
	}
	if histRep.Resolved == nil {
		histRep.Resolved = []string{}
	}

	name := fmt.Sprintf("%010d-%06d.json", now, tickSeq)
	data, _ := json.Marshal(histRep)
	writeAtomic(s.cfg.StateDir, filepath.Join("history", name), data, 0o644)

	s.rotateHistory()
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

	if n > len(files) {
		n = len(files)
	}
	result := make([]json.RawMessage, 0, n)
	for i := 0; i < n; i++ {
		data, err := os.ReadFile(filepath.Join(histDir, files[i].Name()))
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
