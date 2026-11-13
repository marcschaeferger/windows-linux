package updater

import (
	"errors"
	"testing"
	"time"
)

func TestIsLockFailure(t *testing.T) {
	for out, want := range map[string]bool{
		"Failed to obtain rpm transaction lock. Another transaction is in progress.":                        true,
		"error: can't create transaction lock on /var/lib/rpm/.rpm.lock (Resource temporarily unavailable)": true,
		"Error: Transaction test error: file conflicts":                                                     false,
		"": false,
	} {
		if got := isLockFailure(out); got != want {
			t.Errorf("isLockFailure(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestRunWithLockRetry(t *testing.T) {
	locked := "Failed to obtain rpm transaction lock"

	t.Run("succeeds once the lock is free", func(t *testing.T) {
		calls := 0
		err := runWithLockRetry(func(n int) (string, error) {
			calls++
			if n < 2 {
				return locked, errors.New("exit status 1")
			}
			return "", nil
		}, time.Second, time.Millisecond)
		if err != nil || calls != 3 {
			t.Fatalf("err = %v, calls = %d; want nil, 3", err, calls)
		}
	})

	t.Run("other failures are not retried", func(t *testing.T) {
		calls := 0
		err := runWithLockRetry(func(int) (string, error) {
			calls++
			return "Error: GPG check FAILED", errors.New("exit status 1")
		}, time.Second, time.Millisecond)
		if err == nil || errors.Is(err, errPackageLocked) || calls != 1 {
			t.Fatalf("err = %v, calls = %d; want a plain error after one attempt", err, calls)
		}
	})

	t.Run("a lock held for the whole wait is reported as transient", func(t *testing.T) {
		err := runWithLockRetry(func(int) (string, error) {
			return locked, errors.New("exit status 1")
		}, 20*time.Millisecond, 5*time.Millisecond)
		if !errors.Is(err, errPackageLocked) {
			t.Fatalf("err = %v, want errPackageLocked", err)
		}
	})
}
