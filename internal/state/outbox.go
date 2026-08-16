package state

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// outboxIDRe is the exact shape OutboxAdd emits (S.4: "<epoch>-<rand3>").
// OutboxAck joins its argument onto $STATE_DIR/outbox/ before checking it
// exists, so an unvalidated id is a path-traversal primitive (e.g.
// "../history/<file>" resolves outside outbox/ entirely) — reject anything
// that isn't this literal shape before it ever reaches filepath.Join.
var outboxIDRe = regexp.MustCompile(`^[0-9]+-[0-9]{3}$`)

// randIntn is a seam over math/rand.Intn so tests can force a deterministic
// id collision in OutboxAdd without depending on the real PRNG's sequence.
var randIntn = rand.Intn

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
	// S.2: "outbox-add stdin — an opaque JSON object; state validates only
	// that" -> not a JSON object is exit 65. Unmarshaling into a map alone
	// lets JSON `null` through (it's valid for any nullable type, so the
	// map ends up nil with no error) — decode into `any` and require the
	// concrete type to be a JSON object.
	var probe any
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", ErrBadInput
	}
	if _, ok := probe.(map[string]any); !ok {
		return "", ErrBadInput
	}

	now := s.now()

	// <epoch>-<rand3> (S.4) collides across two adds in the same second
	// roughly 1-in-1000 of the time; writeAtomic's rename would silently
	// REPLACE the existing entry, destroying a queued notification with no
	// error. Sequential by construction (S.5: no lock file, the tick loop
	// never runs two Process/OutboxAdd calls concurrently), so checking
	// "does this candidate already exist" and retrying is race-free here.
	var id string
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("%d-%03d", now, randIntn(1000))
		if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "outbox", candidate+".json")); os.IsNotExist(err) {
			id = candidate
			break
		}
	}
	if id == "" {
		return "", fmt.Errorf("state: could not find a free outbox id for %d", now)
	}

	entry := OutboxEntry{
		ID:       id,
		Payload:  json.RawMessage(payload),
		Attempts: 0,
		Created:  now,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("state: marshal outbox entry: %w", err)
	}
	if err := writeAtomic(s.cfg.StateDir, filepath.Join("outbox", id+".json"), data, 0o600); err != nil {
		return "", fmt.Errorf("state: write outbox entry: %w", err)
	}

	// Enforce OUTBOX_MAX
	if err := s.trimOutbox(); err != nil {
		return "", fmt.Errorf("state: trim outbox: %w", err)
	}

	return id, nil
}

func (s *Store) OutboxTake() ([]OutboxItem, error) {
	outboxDir := filepath.Join(s.cfg.StateDir, "outbox")
	files, err := os.ReadDir(outboxDir)
	if err != nil {
		return nil, fmt.Errorf("state: read outbox dir: %w", err)
	}

	items := []OutboxItem{} // S.4/C5: [] on stdout, never null

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

		// S.7: a body id that disagrees with the filename it was read
		// from is corrupt, same as unparsable JSON — "skipped by
		// OutboxTake, still counted against OUTBOX_MAX, removable by
		// OutboxAck [via the filename]". Without this, entry.ID (untrusted
		// body content) is handed to tick as the notification's id; if it
		// doesn't match outboxIDRe, OutboxAck can never acknowledge it and
		// it retries — and re-sends via SMTP past OUTBOX_SMTP_AFTER —
		// every tick forever.
		if entry.ID != strings.TrimSuffix(f.Name(), ".json") {
			continue
		}

		// Increment attempts
		entry.Attempts++

		// Persist the incremented attempts (C4: 0o600 under outbox/).
		data, err = json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("state: marshal outbox entry: %w", err)
		}
		if err := writeAtomic(s.cfg.StateDir, filepath.Join("outbox", f.Name()), data, 0o600); err != nil {
			return nil, fmt.Errorf("state: persist outbox attempts: %w", err)
		}

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
	// Path traversal guard: id is a positional CLI argument and gets
	// joined onto $STATE_DIR/outbox/ unchecked below. Without this,
	// "../history/<file>" (or any id containing a path separator) resolves
	// outside outbox/ and os.Remove deletes whatever it lands on — a
	// history file analyze's trend window depends on, for one.
	if !outboxIDRe.MatchString(id) {
		return ErrUnknownID
	}

	path := filepath.Join(s.cfg.StateDir, "outbox", id+".json")
	if _, err := os.Stat(path); err != nil {
		return ErrUnknownID
	}

	return os.Remove(path)
}

func (s *Store) trimOutbox() error {
	outboxDir := filepath.Join(s.cfg.StateDir, "outbox")
	files, err := os.ReadDir(outboxDir)
	if err != nil {
		return fmt.Errorf("state: read outbox dir: %w", err)
	}

	if len(files) > s.cfg.OutboxMax {
		sort.Slice(files, func(i, j int) bool {
			return files[i].Name() < files[j].Name()
		})

		toDelete := len(files) - s.cfg.OutboxMax
		for i := 0; i < toDelete; i++ {
			os.Remove(filepath.Join(outboxDir, files[i].Name()))
		}
	}
	return nil
}
