package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type ActiveAlert struct {
	Key          string `json:"key"`
	Component    string `json:"component"`
	EvidenceCore string `json:"evidence_core"`
	Headline     string `json:"headline"`
	Severity     string `json:"severity"`
	FirstSeen    int64  `json:"first_seen"`
	LastSeen     int64  `json:"last_seen"`
	LastNotified int64  `json:"last_notified"`
	NotifyCount  int    `json:"notify_count"`
	Occurrences  int    `json:"occurrences"`
	TickSeqFirst int64  `json:"tick_seq_first"`
	TickSeqLast  int64  `json:"tick_seq_last"`
}

// Both loaders enforce alert.Key against the authoritative identifier the
// record was actually found under (the lookup key here, the filename in
// loadAlertByFile) before handing it back. Every downstream join,
// saveAlert, step (e)'s unlink, expireStaleAlerts, trusts alert.Key
// verbatim into a path, so this is the one place that has to check it: a
// body whose "key" field disagrees with reality is corrupt per S.7
// ("deleted, finding treated as new"), same as unparsable JSON, and every
// caller already handles that outcome.

func (s *Store) loadAlert(key string) (*ActiveAlert, bool) {
	path := filepath.Join(s.cfg.StateDir, "active-alerts", key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var alert ActiveAlert
	if err := json.Unmarshal(data, &alert); err != nil || alert.Key != key {
		os.Remove(path)
		return nil, false
	}

	return &alert, true
}

func (s *Store) loadAlertByFile(path string) (*ActiveAlert, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var alert ActiveAlert
	if err := json.Unmarshal(data, &alert); err != nil {
		return nil, err
	}

	expectedKey := strings.TrimSuffix(filepath.Base(path), ".json")
	if alert.Key != expectedKey {
		os.Remove(path)
		return nil, os.ErrInvalid
	}

	return &alert, nil
}

func (s *Store) saveAlert(alert *ActiveAlert) error {
	data, _ := json.Marshal(alert)
	return WriteAtomic(s.cfg.StateDir, filepath.Join("active-alerts", alert.Key+".json"), data, 0644)
}
