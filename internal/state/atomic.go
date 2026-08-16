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

	// Ensure directory exists
	os.MkdirAll(dir, 0700)

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

	// Sync to ensure durability
	err = tmpFile.Sync()
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpPath)
		return err
	}

	// Set permissions before rename (temp files created at 0600)
	os.Chmod(tmpPath, mode)

	// Atomic rename
	return os.Rename(tmpPath, fullPath)
}
