package managers

import (
	"errors"
	"testing"
)

func TestAcquireUILock(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	uiLockFile = nil
	t.Cleanup(func() {
		if uiLockFile != nil {
			uiLockFile.Close()
		}
		uiLockFile = nil
	})

	if err := AcquireUILock(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first := uiLockFile
	// The same process asking again keeps its own lock.
	if err := AcquireUILock(); err != nil {
		t.Fatalf("repeat acquire: %v", err)
	}

	// Another UI (a different open file description) is refused while the
	// first is running.
	uiLockFile = nil
	if err := AcquireUILock(); !errors.Is(err, ErrUIAlreadyRunning) {
		t.Fatalf("second acquire = %v, want ErrUIAlreadyRunning", err)
	}

	// When the first goes away the lock is free again.
	first.Close()
	if err := AcquireUILock(); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestAcquireUILockWithoutRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/pangolin-test")
	uiLockFile = nil
	if uiLockPath() != "" {
		t.Skip("/run/user/<uid> exists on this machine")
	}
	if err := AcquireUILock(); err != nil {
		t.Fatalf("acquire without a runtime dir: %v", err)
	}
}
