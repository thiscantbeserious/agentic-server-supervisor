package state

import (
	"os"
	"path/filepath"
)

// writeAtomic writes data to a file atomically by using a temp file + rename.
// relPath is relative to stateDir.
func writeAtomic(stateDir, relPath string, data []byte, mode os.FileMode) error {
	fullPath := filepath.Join(stateDir, relPath)
	dir := filepath.Dir(fullPath)

	// Ensure directory exists. Unchecked, this masked a real cause: a
	// permission or ENOTDIR failure here would surface as a confusing
	// CreateTemp error two lines later instead of its own.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// Create temp file in the same directory for atomic rename
	tmpFile, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	// Write data
	_, err = tmpFile.Write(data)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}

	// Sync to ensure durability, then Close, both checked: a buffered
	// write can still fail on Close (e.g. ENOSPC surfacing late), and a
	// silently-failed Close is a truncated file this func would otherwise
	// report as written successfully.
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Set permissions before rename (temp files created at 0600)
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Atomic rename, on failure the temp file must not be left behind: a
	// stray .tmp-* in history/ evicts a real report from analyze's window
	// (S.9 case 2), and one anywhere else is a write outside the C4
	// whitelist that outlives this call.
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
