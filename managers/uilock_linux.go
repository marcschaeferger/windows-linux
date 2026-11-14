package managers

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// uiLockFile keeps the single-instance lock open (and so held) for as long as
// this process runs. The descriptor is close-on-exec, so a UI that re-executes
// itself, as after an update, releases it for the new image.
var (
	uiLockFile *os.File
	uiLockMu   sync.Mutex
)

// UserRuntimeFile is the path of a file called name in this user's runtime
// directory (cleared at the end of their last session), or "" when the session
// has none.
func UserRuntimeFile(name string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(dir, name)
}

// uiLockPath is the per-user lock file, or "" when there is no runtime dir.
func uiLockPath() string { return UserRuntimeFile("pangolin-client.lock") }

// AcquireUILock takes this user's single-instance lock, which does not depend
// on the daemon: a UI that started before the daemon was up is not in the
// daemon's list of attached UIs, so only this lock stops a second copy. It
// returns ErrUIAlreadyRunning when another UI holds it. Without a runtime
// directory it succeeds without locking, leaving the daemon's check.
func AcquireUILock() error {
	uiLockMu.Lock()
	defer uiLockMu.Unlock()
	if uiLockFile != nil {
		return nil
	}
	path := uiLockPath()
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return ErrUIAlreadyRunning
		}
		return nil
	}
	uiLockFile = f
	return nil
}
