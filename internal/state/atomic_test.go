package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteAtomic must not leave its temp file behind when the final Rename
// fails, a stray .tmp-* in history/ evicts a real report from analyze's
// window (S.9 case 2), and one anywhere else is a write outside the C4
// whitelist that outlives the failed call. Renaming a regular file onto an
// existing directory reliably fails (EISDIR) on every platform this runs
// on, which is the cheapest reproducible way to force Rename to fail
// without needing a genuinely full or read-only filesystem.
func TestWriteAtomic_CleansUpTempFileOnRenameFailure(t *testing.T) {
	stateDir := t.TempDir()
	// "heartbeat" exists as a directory, not a file -> Rename(tmp, .../heartbeat) fails.
	if err := os.MkdirAll(filepath.Join(stateDir, "heartbeat"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := WriteAtomic(stateDir, "heartbeat", []byte("data\n"), 0o644)
	if err == nil {
		t.Fatal("WriteAtomic onto an existing directory = nil error, want non-nil")
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("WriteAtomic left a temp file behind after a failed Rename: %s", e.Name())
		}
	}
}
