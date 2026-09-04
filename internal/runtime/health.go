// health.go: Health(), the compose healthcheck (R2).
package runtime

import (
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// Health reports exit 0 iff $STATE_DIR/heartbeat exists and its mtime is
// younger than state.HealthWindow (the interval plus every configured term
// of the longest legal tick), else 1. It reads nothing else.
func Health(cfg *config.Config) (int, error) {
	store, err := state.New(cfg)
	if err != nil {
		return 69, err
	}
	if err := store.Health(); err != nil {
		return 1, err
	}
	return 0, nil
}
