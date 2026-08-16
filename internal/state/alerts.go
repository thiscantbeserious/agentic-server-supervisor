package state

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func (s *Store) loadAlert(key string) (*ActiveAlert, bool) {
	path := filepath.Join(s.cfg.StateDir, "active-alerts", key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var alert ActiveAlert
	if err := json.Unmarshal(data, &alert); err != nil {
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

	return &alert, nil
}

func (s *Store) saveAlert(alert *ActiveAlert) error {
	data, _ := json.Marshal(alert)
	return writeAtomic(s.cfg.StateDir, filepath.Join("active-alerts", alert.Key+".json"), data, 0644)
}
