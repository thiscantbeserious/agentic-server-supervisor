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

func (s *Store) loc() *time.Location {
	loc, _ := time.LoadLocation(s.cfg.TZ)
	return loc
}

func (s *Store) tickSeq() int64 {
	if s.cfg.TickSeq > 0 {
		return s.cfg.TickSeq
	}
	return 0
}

func (s *Store) Process(raw []byte) (*Decision, error) {
	now := s.now()
	loc := s.loc()

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

	// b) History rotation - store raw input
	historyPath := filepath.Join(s.cfg.StateDir, "history", fmt.Sprintf("%010d-%06d.json", now, histTickSeq))
	if err := writeAtomic(s.cfg.StateDir, filepath.Join("history", fmt.Sprintf("%010d-%06d.json", now, histTickSeq)), raw, 0644); err == nil {
		// Rotate history: delete all but the HISTORY_KEEP newest
		s.rotateHistory()
	}

	// c) Stale expiry
	s.expireStaleAlerts(now)

	// d) Process findings
	notified := []report.Finding{}
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

			// Annotate finding
			f.Key = key
			f.FirstSeen = alert.FirstSeen
			f.Occurrences = 1
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

				f.Key = key
				f.FirstSeen = alert.FirstSeen
				f.Occurrences = alert.Occurrences
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

					f.Key = key
					f.FirstSeen = alert.FirstSeen
					f.Occurrences = alert.Occurrences
					notified = append(notified, f)
				} else {
					// Suppressed
					s.saveAlert(alert)
					suppressedCount++
					f.Key = key
					f.FirstSeen = alert.FirstSeen
					f.Occurrences = alert.Occurrences
				}
			}
		}
	}

	// e) Resolved / all-clear
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

			// Check if this alert was touched in step (d)
			touched := false
			for _, notif := range notified {
				if notif.Key == alert.Key {
					touched = true
					break
				}
			}

			if !touched {
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
	}

	// f) Heartbeat check
	hbPath := filepath.Join(s.cfg.StateDir, "heartbeat")
	hbTime := time.Unix(now, 0).In(loc)
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
				headline = headline[:min(len([]rune(headline)), 80-len([]rune(suffix))-1)] + suffix
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
		oldest := ""
		if len(histFiles) > 0 {
			oldest = histFiles[0].Name()
		}

		rep.Body = fmt.Sprintf("No open findings. %d ticks since", len(histFiles))
		if oldest != "" {
			// Parse oldest timestamp
			parts := strings.Split(oldest, "-")
			if len(parts) >= 1 {
				rep.Body = fmt.Sprintf("No open findings. %d ticks since %s.", len(histFiles), oldest[:10])
			}
		}

		// Update heartbeat
		os.WriteFile(hbPath, []byte(hbStr+"\n"), 0644)

	default:
		// Rule 4: suppressed
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

	// Annotate notified and suppressed findings in history
	s.annotateHistory(historyPath, rep)

	return decision, nil
}

func (s *Store) annotateHistory(histPath string, rep *report.Report) {
	// Read the history file and annotate findings with key/first_seen/occurrences
	data, err := os.ReadFile(histPath)
	if err != nil {
		return
	}

	var histRep report.Report
	if err := json.Unmarshal(data, &histRep); err != nil {
		return
	}

	// Annotate all findings (notified and suppressed)
	for i, f := range histRep.Findings {
		key := f.Key
		if key == "" {
			key = dedup.Key(f.Component, dedup.EvidenceCore(f.Evidence))
		}

		if alert, exists := s.loadAlert(key); exists {
			histRep.Findings[i].Key = alert.Key
			histRep.Findings[i].FirstSeen = alert.FirstSeen
			histRep.Findings[i].Occurrences = alert.Occurrences
		} else {
			histRep.Findings[i].Key = key
			histRep.Findings[i].FirstSeen = time.Unix(s.now(), 0).Unix()
			histRep.Findings[i].Occurrences = 1
		}
	}

	// Write annotated version back atomically
	annotated, _ := json.Marshal(histRep)
	writeAtomic(s.cfg.StateDir, filepath.Join("history", filepath.Base(histPath)), annotated, 0644)
}

func (s *Store) rotateHistory() {
	histDir := filepath.Join(s.cfg.StateDir, "history")
	files, _ := os.ReadDir(histDir)

	if len(files) > s.cfg.HistoryKeep {
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() < files[j].Name()
		})

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
		if err == nil {
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
