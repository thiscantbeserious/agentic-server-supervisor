package state

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

type OutboxEntry struct {
	ID       string          `json:"id"`
	Payload  json.RawMessage `json:"payload"`
	Attempts int             `json:"attempts"`
	Created  int64           `json:"created"`
}

type OutboxItem struct {
	ID           string          `json:"id"`
	Payload      json.RawMessage `json:"payload"`
	Attempts     int             `json:"attempts"`
	FallbackSMTP bool            `json:"fallback_smtp"`
}

func (s *Store) OutboxAdd(payload []byte) (string, error) {
	now := s.now()

	// Generate ID: <epoch>-<rand3>
	r := rand.Intn(1000)
	id := fmt.Sprintf("%d-%03d", now, r)

	entry := OutboxEntry{
		ID:       id,
		Payload:  json.RawMessage(payload),
		Attempts: 1,
		Created:  now,
	}

	data, _ := json.Marshal(entry)
	writeAtomic(s.cfg.StateDir, filepath.Join("outbox", id+".json"), data, 0644)

	// Enforce OUTBOX_MAX
	s.trimOutbox()

	return id, nil
}

func (s *Store) OutboxTake() ([]OutboxItem, error) {
	outboxDir := filepath.Join(s.cfg.StateDir, "outbox")
	files, _ := os.ReadDir(outboxDir)

	var items []OutboxItem

	// Sort by name (oldest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	for _, f := range files {
		path := filepath.Join(outboxDir, f.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var entry OutboxEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}

		// Increment attempts
		entry.Attempts++

		// Persist the incremented attempts
		data, _ = json.Marshal(entry)
		writeAtomic(s.cfg.StateDir, filepath.Join("outbox", f.Name()), data, 0644)

		fallback := entry.Attempts >= s.cfg.OutboxSMTPAfter

		items = append(items, OutboxItem{
			ID:           entry.ID,
			Payload:      entry.Payload,
			Attempts:     entry.Attempts,
			FallbackSMTP: fallback,
		})
	}

	return items, nil
}

func (s *Store) OutboxAck(id string) error {
	path := filepath.Join(s.cfg.StateDir, "outbox", id+".json")
	if _, err := os.Stat(path); err != nil {
		return ErrUnknownID
	}

	return os.Remove(path)
}

func (s *Store) trimOutbox() {
	outboxDir := filepath.Join(s.cfg.StateDir, "outbox")
	files, _ := os.ReadDir(outboxDir)

	if len(files) > s.cfg.OutboxMax {
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() < files[j].Name()
		})

		toDelete := len(files) - s.cfg.OutboxMax
		for i := 0; i < toDelete; i++ {
			os.Remove(filepath.Join(outboxDir, files[i].Name()))
		}
	}
}
