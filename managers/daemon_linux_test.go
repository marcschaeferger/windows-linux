package managers

import (
	"errors"
	"testing"
	"time"
)

func TestHandleUIGone(t *testing.T) {
	type result struct {
		slept         time.Duration
		installCalled bool
		relaunched    bool
	}
	run := func(stopping, signedIn, installs bool, alwaysOn bool) result {
		var r result
		const key = "1000"
		setUserAlwaysOn(key, alwaysOn)
		defer setUserAlwaysOn(key, false)
		handleUIGone(1000, key, uiGoneActions{
			stopping: func() bool { return stopping },
			sleep:    func(d time.Duration) { r.slept = d },
			signedIn: func(uint32) bool { return signedIn },
			installUpdate: func(uint32, string) bool {
				r.installCalled = true
				return installs
			},
			relaunch: func(uint32, string) { r.relaunched = true },
		})
		if !stopping && !signedIn && userAlwaysOn(key) {
			t.Error("Always-On still set after sign-out")
		}
		return r
	}

	t.Run("daemon stopping does nothing", func(t *testing.T) {
		r := run(true, false, false, true)
		if r.slept != 0 || r.installCalled || r.relaunched {
			t.Errorf("unexpected actions: %+v", r)
		}
	})
	t.Run("sign-out clears Always-On and does not relaunch", func(t *testing.T) {
		r := run(false, false, false, true)
		if r.slept != quitSignOutGrace {
			t.Errorf("slept %v, want %v", r.slept, quitSignOutGrace)
		}
		if r.installCalled || r.relaunched {
			t.Errorf("sign-out must not install or relaunch: %+v", r)
		}
	})
	t.Run("quit while signed in keeps Always-On", func(t *testing.T) {
		r := run(false, true, false, true)
		if !r.installCalled || !r.relaunched {
			t.Errorf("want install attempt then relaunch: %+v", r)
		}
	})
	t.Run("an install replaces the relaunch", func(t *testing.T) {
		r := run(false, true, true, false)
		if !r.installCalled || r.relaunched {
			t.Errorf("unexpected: %+v", r)
		}
	})
}

func TestSignedInFromLogind(t *testing.T) {
	failed := errors.New("exit status 1")
	tests := []struct {
		name       string
		state      string
		err        error
		runtimeDir bool
		want       bool
	}{
		{"active", "active\n", nil, true, true},
		{"online", "online\n", nil, true, true},
		{"lingering only", "lingering\n", nil, true, false},
		{"closing", "closing\n", nil, true, false},
		{"loginctl fails, runtime dir still there", "", failed, true, true},
		{"loginctl fails, user gone", "", failed, false, false},
	}
	for _, tt := range tests {
		if got := signedInFromLogind(tt.state, tt.err, tt.runtimeDir); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
