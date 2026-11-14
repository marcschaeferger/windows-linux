package ui

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/managers"
)

func TestIPCReconnectDelay(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for attempt, w := range want {
		if got := ipcReconnectDelay(attempt); got != w {
			t.Errorf("attempt %d: delay %v, want %v", attempt, got, w)
		}
	}
	if got := ipcReconnectDelay(10000); got != ipcReconnectMaxDelay {
		t.Errorf("huge attempt: delay %v, want %v", got, ipcReconnectMaxDelay)
	}
}

func TestReconnectLoop(t *testing.T) {
	current := func() binaryState { return binaryCurrent }
	f, _ := os.CreateTemp(t.TempDir(), "pipe")
	defer f.Close()

	t.Run("retries until the daemon is up", func(t *testing.T) {
		calls := 0
		var slept []time.Duration
		attach := func() (*os.File, *os.File, *os.File, error) {
			calls++
			if calls < 4 {
				return nil, nil, nil, errors.New("connection refused")
			}
			return f, f, f, nil
		}
		outcome, r, _, _ := reconnectLoop(attach, current, func(d time.Duration) { slept = append(slept, d) })
		if outcome != reconnectAttached || r != f {
			t.Fatalf("outcome %v, reader %v", outcome, r)
		}
		if len(slept) != 3 || slept[0] != time.Second || slept[2] != 4*time.Second {
			t.Errorf("sleeps = %v, want 1s 2s 4s", slept)
		}
	})

	t.Run("keeps waiting while an update installs", func(t *testing.T) {
		calls := 0
		attach := func() (*os.File, *os.File, *os.File, error) {
			calls++
			if calls == 1 {
				return nil, nil, nil, managers.ErrUpdateInstalling
			}
			return f, f, f, nil
		}
		outcome, _, _, _ := reconnectLoop(attach, current, func(time.Duration) {})
		if outcome != reconnectAttached || calls != 2 {
			t.Fatalf("outcome %v after %d calls", outcome, calls)
		}
	})

	t.Run("stops when another UI is attached", func(t *testing.T) {
		calls := 0
		attach := func() (*os.File, *os.File, *os.File, error) {
			calls++
			return nil, nil, nil, managers.ErrUIAlreadyRunning
		}
		outcome, _, _, _ := reconnectLoop(attach, current, func(time.Duration) {})
		if outcome != reconnectOtherUI || calls != otherUIConfirmations {
			t.Fatalf("outcome %v after %d attaches, want reconnectOtherUI after %d", outcome, calls, otherUIConfirmations)
		}
	})

	t.Run("a stale 'already running' from the daemon is retried", func(t *testing.T) {
		calls := 0
		attach := func() (*os.File, *os.File, *os.File, error) {
			calls++
			if calls == 1 {
				return nil, nil, nil, managers.ErrUIAlreadyRunning
			}
			return f, f, f, nil
		}
		outcome, _, _, _ := reconnectLoop(attach, current, func(time.Duration) {})
		if outcome != reconnectAttached {
			t.Fatalf("outcome %v, want reconnectAttached", outcome)
		}
	})

	t.Run("a replaced binary re-executes before attaching", func(t *testing.T) {
		attach := func() (*os.File, *os.File, *os.File, error) {
			t.Fatal("attached with an outdated binary")
			return nil, nil, nil, nil
		}
		outcome, _, _, _ := reconnectLoop(attach, func() binaryState { return binaryReplaced }, func(time.Duration) {})
		if outcome != reconnectReplaced {
			t.Fatalf("outcome %v, want reconnectReplaced", outcome)
		}
	})

	t.Run("a package upgrade during the wait is noticed", func(t *testing.T) {
		state := binaryCurrent
		attach := func() (*os.File, *os.File, *os.File, error) {
			return nil, nil, nil, errors.New("connection refused")
		}
		outcome, _, _, _ := reconnectLoop(attach, func() binaryState { return state }, func(time.Duration) { state = binaryReplaced })
		if outcome != reconnectReplaced {
			t.Fatalf("outcome %v, want reconnectReplaced", outcome)
		}
	})
}

func TestBinaryStateOf(t *testing.T) {
	exists := func(path string) bool { return path == "/usr/bin/pangolin-client" }
	tests := []struct {
		target string
		want   binaryState
	}{
		{"/usr/bin/pangolin-client", binaryCurrent},
		{"/usr/bin/pangolin-client (deleted)", binaryReplaced},
		{"/opt/gone/pangolin-client (deleted)", binaryRemoved},
	}
	for _, tt := range tests {
		if got := binaryStateOf(tt.target, exists); got != tt.want {
			t.Errorf("binaryStateOf(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

func TestAlwaysOnMarker(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if alwaysOnPersisted() {
		t.Fatal("marker set in a fresh runtime dir")
	}
	persistAlwaysOn(true)
	if !alwaysOnPersisted() {
		t.Fatal("marker not written")
	}
	persistAlwaysOn(true) // idempotent
	persistAlwaysOn(false)
	if alwaysOnPersisted() {
		t.Fatal("marker not removed")
	}
	persistAlwaysOn(false) // removing a missing marker is fine
}
