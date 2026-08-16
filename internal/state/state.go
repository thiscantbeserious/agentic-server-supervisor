package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

type Decision struct {
	Notify          bool           `json:"notify"`
	Reason          string         `json:"reason"`
	TickSeq         int64          `json:"tick_seq"`
	SuppressedCount int            `json:"suppressed_count"`
	ActiveCount     int            `json:"active_count"`
	Heartbeat       bool           `json:"heartbeat"`
	Report          report.Report  `json:"report"`
}

type Store struct {
	cfg *config.Config
}

func New(cfg *config.Config) (*Store, error) {
	// Probe STATE_DIR is writable
	testFile := filepath.Join(cfg.StateDir, ".probe")
	err := os.WriteFile(testFile, []byte{}, 0644)
	if err != nil {
		return nil, ErrStateDir
	}
	os.Remove(testFile)

	// Create required directories
	for _, dir := range []string{"active-alerts", "history", "outbox"} {
		p := filepath.Join(cfg.StateDir, dir)
		os.MkdirAll(p, 0755)
	}

	// Initialize heartbeat file
	hbPath := filepath.Join(cfg.StateDir, "heartbeat")
	if _, err := os.Stat(hbPath); err != nil {
		os.WriteFile(hbPath, []byte("\n"), 0644)
	}

	return &Store{cfg: cfg}, nil
}

func (s *Store) now() int64 {
	if !s.cfg.Now.IsZero() {
		return s.cfg.Now.Unix()
	}
	return time.Now().Unix()
}

func (s *Store) tickSeq() int64 {
	if s.cfg.TickSeq > 0 {
		return s.cfg.TickSeq
	}
	return 0
}

func (s *Store) Process(raw []byte) (*Decision, error) {
	now := s.now()

	// Parse and validate input
	rep, err := report.Validate(raw)
	if err != nil {
		return nil, ErrBadInput
	}

	decision := &Decision{
		TickSeq: s.tickSeq(),
	}

	// Determine tick_seq for history filename (S.3a)
	histTickSeq := decision.TickSeq
	if histTickSeq == 0 && rep.Meta != nil && rep.Meta.TickSeq > 0 {
		histTickSeq = rep.Meta.TickSeq
	}

	// c) Stale expiry - do this first
	s.expireStaleAlerts(now)

	// d) Process findings - build suppressed list for annotation later
	notified := []report.Finding{}
	suppressedFinding := make(map[string]*report.Finding) // key -> suppressed finding
	allClear := []string{}
	suppressedCount := 0

	for _, f := range rep.Findings {
		key := f.Key
		if key == "" {
			key = dedup.Key(f.Component, dedup.EvidenceCore(f.Evidence))
		}

		alert, exists := s.loadAlert(key)
		if !exists {
			// New finding
			alert = &ActiveAlert{
				Key:          key,
				Component:    f.Component,
				EvidenceCore: dedup.EvidenceCore(f.Evidence),
				Headline:     rep.Headline,
				Severity:     f.Severity,
				FirstSeen:    now,
				LastSeen:     now,
				LastNotified: now,
				NotifyCount:  1,
				Occurrences:  1,
				TickSeqFirst: decision.TickSeq,
				TickSeqLast:  decision.TickSeq,
			}
			s.saveAlert(alert)
			decision.Notify = true
			decision.Reason = "new_finding"
			notified = append(notified, f)
		} else {
			// Existing finding
			alert.LastSeen = now
			alert.Occurrences++
			alert.TickSeqLast = decision.TickSeq

			severity := map[string]int{"info": 0, "watch": 1, "alert": 2}
			oldRank := severity[alert.Severity]
			newRank := severity[f.Severity]

			if newRank > oldRank {
				// Escalation
				alert.Severity = f.Severity
				alert.LastNotified = now
				alert.NotifyCount++
				s.saveAlert(alert)
				decision.Notify = true
				decision.Reason = "escalation"
				notified = append(notified, f)
			} else {
				// Check renotify window
				window := s.cfg.RenotifyAlertSec
				if alert.Severity != "alert" {
					window = s.cfg.RenotifyWatchSec
				}

				if now-alert.LastNotified >= int64(window) {
					// Renotify
					alert.LastNotified = now
					alert.NotifyCount++
					s.saveAlert(alert)
					if decision.Reason == "" {
						decision.Notify = true
						decision.Reason = "renotify"
					}
					notified = append(notified, f)
				} else {
					// Suppressed
					s.saveAlert(alert)
					suppressedCount++
					fCopy := f
					suppressedFinding[key] = &fCopy
				}
			}
		}
	}

	// e) Resolved / all-clear - only consider alerts NOT touched in step (d)
	// Collect keys of notified findings
	notifiedKeys := make(map[string]bool)
	for _, f := range notified {
		key := f.Key
		if key == "" {
			key = dedup.Key(f.Component, dedup.EvidenceCore(f.Evidence))
		}
		notifiedKeys[key] = true
	}

	for _, res := range rep.Resolved {
		res = strings.TrimSpace(strings.ToLower(res))
		if res == "" {
			continue
		}

		// Find matching active alert not touched in step (d)
		alertFiles, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "active-alerts"))
		for _, f := range alertFiles {
			alert, err := s.loadAlertByFile(filepath.Join(s.cfg.StateDir, "active-alerts", f.Name()))
			if err != nil {
				continue
			}

			// Skip if this alert was touched in step (d)
			if notifiedKeys[alert.Key] {
				continue
			}

			normHeadline := strings.TrimSpace(strings.ToLower(alert.Headline))
			if normHeadline == res {
				// Match - add to all-clear and delete
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
	}

	// f) Heartbeat check
	hbPath := filepath.Join(s.cfg.StateDir, "heartbeat")
	hbTime := time.Unix(now, 0).In(s.cfg.Loc)
	hbStr := hbTime.Format("2006-01-02")

	hbData, _ := os.ReadFile(hbPath)
	hbCurrent := strings.TrimSpace(string(hbData))
	hbDue := false

	if hbCurrent != hbStr && hbTime.Hour() >= s.cfg.HeartbeatHour {
		hbDue = true
	}

	// g) Message assembly
	switch {
	case len(notified) > 0:
		// Rule 1: notified non-empty
		decision.Notify = true

		highest := 0
		severityRank := map[string]int{"info": 0, "watch": 1, "alert": 2}
		for _, f := range notified {
			if severityRank[f.Severity] > highest {
				highest = severityRank[f.Severity]
			}
		}

		statusMap := []string{"OK", "WATCH", "ALERT"}
		rep.Status = statusMap[highest]
		rep.Findings = notified
		rep.Resolved = []string{}

	case len(allClear) > 0:
		// Rule 2: all_clear non-empty
		decision.Notify = true
		decision.Reason = "all_clear"
		rep.Status = "OK"
		rep.Findings = []report.Finding{}
		rep.Resolved = allClear

		headline := allClear[0]
		if len(allClear) > 1 {
			suffix := fmt.Sprintf(" (+%d more)", len(allClear)-1)
			full := headline + suffix
			if len([]rune(full)) > 80 {
				runes := []rune(headline)
				keep := 80 - len([]rune(suffix)) - 1
				if keep > 0 {
					headline = string(runes[:keep]) + suffix
				} else {
					headline = string(runes[:min(len(runes), 80)])
				}
			} else {
				headline = full
			}
		}
		if len([]rune(headline)) > 80 {
			runes := []rune(headline)
			headline = string(runes[:80])
		}
		rep.Headline = "Resolved: " + headline
		rep.Body = ""
		for _, ac := range allClear {
			rep.Body += "- " + ac + "\n"
		}
		rep.Body = strings.TrimSuffix(rep.Body, "\n")

		// Update heartbeat
		os.WriteFile(hbPath, []byte(hbStr+"\n"), 0644)

	case hbDue:
		// Rule 3: heartbeat due
		decision.Notify = true
		decision.Reason = "heartbeat"
		decision.Heartbeat = true
		rep.Status = "OK"
		rep.Headline = "Daily heartbeat: all clear"
		rep.Findings = []report.Finding{}
		rep.Resolved = []string{}

		// Count history files for body
		histDir := filepath.Join(s.cfg.StateDir, "history")
		histFiles, _ := os.ReadDir(histDir)
		histCount := len(histFiles)
		oldest := ""
		if histCount > 0 {
			oldest = histFiles[0].Name()
		}

		rep.Body = fmt.Sprintf("No open findings. %d ticks since", histCount)
		if oldest != "" {
			// Parse oldest timestamp - format is XXXXXXXXXX-XXXXXX.json
			parts := strings.Split(oldest, "-")
			if len(parts) >= 1 {
				rep.Body = fmt.Sprintf("No open findings. %d ticks since %s.", histCount, oldest[:10])
			}
		}

		// Update heartbeat
		os.WriteFile(hbPath, []byte(hbStr+"\n"), 0644)

	default:
		// Rule 4: suppressed
		decision.Reason = "suppressed"
		rep.Status = "OK"
		rep.Findings = []report.Finding{}
		rep.Resolved = []string{}
	}

	decision.Report = *rep
	decision.SuppressedCount = suppressedCount

	// Count active alerts
	alertFiles, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "active-alerts"))
	decision.ActiveCount = len(alertFiles)

	// Always update heartbeat mtime
	os.WriteFile(hbPath, []byte(hbStr+"\n"), 0644)

	// b) History write - AFTER step (d), with annotations
	s.writeAnnotatedHistory(now, histTickSeq, rep, suppressedFinding)

	return decision, nil
}

func (s *Store) writeAnnotatedHistory(now int64, tickSeq int64, rep *report.Report, suppressedFinding map[string]*report.Finding) {
	// Merge notified and suppressed findings with annotations from active-alert records
	allFindings := append([]report.Finding{}, rep.Findings...)

	// Add suppressed findings in their original order
	for _, f := range rep.Findings {
		key := f.Key
		if key == "" {
			key = dedup.Key(f.Component, dedup.EvidenceCore(f.Evidence))
		}
		// Note: this won't duplicate notified findings since they're already in allFindings
	}

	// Now add suppressedFindings (those that weren't in the notified list)
	for key, suppressedF := range suppressedFinding {
		allFindings = append(allFindings, *suppressedF)
		_ = key // for clarity
	}

	// Annotate all findings with key/first_seen/occurrences
	for i := range allFindings {
		key := allFindings[i].Key
		if key == "" {
			key = dedup.Key(allFindings[i].Component, dedup.EvidenceCore(allFindings[i].Evidence))
		}

		if alert, exists := s.loadAlert(key); exists {
			allFindings[i].Key = alert.Key
			allFindings[i].FirstSeen = alert.FirstSeen
			allFindings[i].Occurrences = alert.Occurrences
		} else {
			allFindings[i].Key = key
			allFindings[i].FirstSeen = now
			allFindings[i].Occurrences = 1
		}
	}

	// Create annotated history document
	histRep := report.Report{
		Status:   rep.Status,
		Headline: rep.Headline,
		Body:     rep.Body,
		Findings: allFindings,
		Resolved: rep.Resolved,
		Meta:     rep.Meta,
	}

	// Write atomically with cleanup of temp files
	histPath := filepath.Join("history", fmt.Sprintf("%010d-%06d.json", now, tickSeq))
	data, _ := json.Marshal(histRep)
	writeAtomic(s.cfg.StateDir, histPath, data, 0644)

	// Clean up any stray temp files from the history dir
	histDir := filepath.Join(s.cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".tmp-") {
			os.Remove(filepath.Join(histDir, f.Name()))
		}
	}

	// Rotate history: delete all but the HISTORY_KEEP newest
	s.rotateHistory()
}

func (s *Store) rotateHistory() {
	histDir := filepath.Join(s.cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)

	if len(files) > s.cfg.HistoryKeep {
		// Sort by name descending (newest first), then delete oldest
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() > files[j].Name()
		})

		toDelete := len(files) - s.cfg.HistoryKeep
		for i := 0; i < toDelete; i++ {
			os.Remove(filepath.Join(histDir, files[len(files)-1-i].Name()))
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

	if len(files) == 0 {
		return []json.RawMessage{}, nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() > files[j].Name()
	})

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

	mtime := info.ModTime()
	now := time.Now()
	if s.cfg.Now != (time.Time{}) {
		now = time.Unix(s.cfg.Now.Unix(), 0)
	}

	threshold := 3 * s.cfg.TickInterval
	if now.Sub(mtime) > threshold {
		return errors.New("heartbeat stale")
	}

	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
