//go:build !windows

package logging

import "os"

// openAppend opens path for appending, creating it with perm.
func openAppend(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
}

// restrictFile tightens a file that was created with a wider mode by an
// earlier version.
func restrictFile(f *os.File, perm os.FileMode) {
	if info, err := f.Stat(); err == nil && info.Mode().Perm()&^perm != 0 {
		_ = f.Chmod(perm)
	}
}

// RestrictDir makes dir mode perm if it is wider than that, for directories
// created by an earlier version or by the service manager.
func RestrictDir(dir string, perm os.FileMode) {
	if info, err := os.Stat(dir); err == nil && info.IsDir() && info.Mode().Perm()&^perm != 0 {
		_ = os.Chmod(dir, perm)
	}
}

// OpenRead opens path for reading.
func OpenRead(path string) (*os.File, error) { return os.Open(path) }
