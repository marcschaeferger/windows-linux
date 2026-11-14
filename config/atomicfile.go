package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fosrl/newt/logger"
)

// writeFileAtomic replaces path with data so that a crash, power loss or full
// disk leaves either the old or the new contents, never a truncated file: it
// writes a temp file in the same directory, syncs it and renames it over path.
// An existing file keeps its mode (perm only applies to a new one), and a
// symlink is followed so the link itself stays in place.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		perm = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	// CreateTemp makes the file 0600; give it the mode the file is meant to have.
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := replaceFile(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename in dir durable where the OS supports it. Errors are
// ignored: Windows can't sync a directory handle, and the rename itself is
// already atomic.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// backupCorruptFile keeps a copy of a file that failed to parse next to it,
// so the next save doesn't destroy what is left of it. It returns the
// backup's path, or "" if it could not be written.
func backupCorruptFile(path string, data []byte, perm os.FileMode) string {
	backup := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405"))
	if err := writeFileAtomic(backup, data, perm); err != nil {
		logger.Error("Failed to back up unreadable %s: %v", filepath.Base(path), err)
		return ""
	}
	logger.Warn("Kept a copy of unreadable %s at %s", filepath.Base(path), backup)
	return backup
}
