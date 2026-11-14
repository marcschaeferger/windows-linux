package managers

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/updater"
)

func TestProgressThrottle(t *testing.T) {
	th := newProgressThrottle(250 * time.Millisecond)
	t0 := time.Unix(1000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	dl := func(n uint64) updater.DownloadProgress {
		return updater.DownloadProgress{Activity: "Downloading update", BytesDownloaded: n}
	}
	steps := []struct {
		dp   updater.DownloadProgress
		at   int
		want bool
	}{
		{dl(1), 0, true},    // first event of an activity
		{dl(2), 10, false},  // too soon
		{dl(3), 200, false}, // still too soon
		{dl(4), 260, true},  // interval passed
		{dl(5), 270, false}, //
		{updater.DownloadProgress{Activity: "Installing update"}, 271, true},                            // new activity
		{updater.DownloadProgress{Activity: "Installing update", Error: errors.New("boom")}, 272, true}, // errors always pass
		{updater.DownloadProgress{Activity: "Installing update", Complete: true}, 273, true},            // so does completion
	}
	for i, s := range steps {
		if got := th.allow(s.dp, at(s.at)); got != s.want {
			t.Errorf("step %d: allow = %v, want %v", i, got, s.want)
		}
	}
}

func TestTakeAndRestoreStagedUpdate(t *testing.T) {
	staged := &updater.StagedUpdate{}
	updateLock.Lock()
	stagedUpdate = staged
	updateLock.Unlock()
	t.Cleanup(func() { takeStagedUpdate() })

	if got := takeStagedUpdate(); got != staged {
		t.Fatalf("takeStagedUpdate = %v, want the staged update", got)
	}
	if got := takeStagedUpdate(); got != nil {
		t.Fatalf("second takeStagedUpdate = %v, want nil", got)
	}
	// An install that never started puts the update back for the next try.
	restoreStagedUpdate(staged)
	if got := takeStagedUpdate(); got != staged {
		t.Fatalf("after restoreStagedUpdate: take = %v, want the staged update", got)
	}
	// Restoring nothing, or over a newer download, changes nothing.
	restoreStagedUpdate(nil)
	newer := &updater.StagedUpdate{}
	updateLock.Lock()
	stagedUpdate = newer
	updateLock.Unlock()
	restoreStagedUpdate(staged)
	if got := takeStagedUpdate(); got != newer {
		t.Fatalf("restore replaced a newer staged update")
	}
}

// stubUpdateGlobals keeps tests away from the real staged-update directory and
// restores the update state they change.
func stubUpdateGlobals(t *testing.T) {
	t.Helper()
	oldCancel, oldReload := cancelUIRelaunch, reloadStagedUpdate
	cancelUIRelaunch, reloadStagedUpdate = func() {}, func() {}
	updateLock.Lock()
	oldState, oldVersion, oldStaged, oldInstalling, oldInstalled, oldPending :=
		updateState, foundVersion, stagedUpdate, installingStagedUpdate, installedVersion, pendingLaunchSessions
	updateLock.Unlock()
	t.Cleanup(func() {
		cancelUIRelaunch, reloadStagedUpdate = oldCancel, oldReload
		updateLock.Lock()
		updateState, foundVersion, stagedUpdate, installingStagedUpdate, installedVersion, pendingLaunchSessions =
			oldState, oldVersion, oldStaged, oldInstalling, oldInstalled, oldPending
		updateLock.Unlock()
	})
}

// A background install that fails, or returns without replacing this process,
// must not leave the UI locked out for good.
func TestFinishStagedInstallClearsFlag(t *testing.T) {
	stubUpdateGlobals(t)
	updateLock.Lock()
	installingStagedUpdate = true
	pendingLaunchSessions = []uint32{1000, 1001}
	updateState, foundVersion = UpdateStateUpdateDownloaded, "1.2.0"
	updateLock.Unlock()

	var got []uint32
	finishStagedInstall("", func(sessions []uint32) { got = sessions })

	if stagedUpdateInstalling() {
		t.Fatal("installingStagedUpdate is still set")
	}
	if len(got) != 2 || got[0] != 1000 || got[1] != 1001 {
		t.Fatalf("onFailure sessions = %v, want the held-back launches", got)
	}
	state, v := currentUpdateState()
	if state != UpdateStateFoundUpdate || v != "1.2.0" {
		t.Fatalf("after a failed install: state = %v, version = %q; want the update offered again", state, v)
	}
}

// An install that worked but didn't restart this process must not be offered or
// downloaded again, or every quit would reinstall it.
func TestFinishStagedInstallRemembersInstalledVersion(t *testing.T) {
	stubUpdateGlobals(t)
	updateLock.Lock()
	installingStagedUpdate = true
	updateState, foundVersion = UpdateStateUpdateDownloaded, "1.2.0"
	updateLock.Unlock()

	finishStagedInstall("1.2.0", nil)

	state, v := currentUpdateState()
	if state != UpdateStateUnknown || v != "" {
		t.Fatalf("state = %v, version = %q; want nothing offered", state, v)
	}
	updateLock.Lock()
	got := installedVersion
	updateLock.Unlock()
	if got != "1.2.0" {
		t.Fatalf("installedVersion = %q", got)
	}
}

func TestStageRetryDelay(t *testing.T) {
	day := 24 * time.Hour
	for _, tt := range []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{1, day, 30 * time.Minute},
		{2, day, time.Hour},
		{3, day, 2 * time.Hour},
		{6, day, 16 * time.Hour},
		{7, day, 0}, // would be 32 h: the regular interval takes over
		{1, time.Hour, 30 * time.Minute},
		{2, time.Hour, 0},
		{0, day, 30 * time.Minute},
	} {
		if got := stageRetryDelay(tt.failures, tt.interval); got != tt.want {
			t.Errorf("stageRetryDelay(%d, %v) = %v, want %v", tt.failures, tt.interval, got, tt.want)
		}
	}
}

// Notifications must reach a UI in the order they were sent, and only the UI
// they were addressed to: a stale progress event arriving after the final one
// used to leave the update window stuck, and every user's UI got every event.
func TestNotificationsKeepOrderAndRouting(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	other, otherW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	svc := &ManagerService{events: w}
	bystander := &ManagerService{events: otherW}
	managerServicesLock.Lock()
	managerServices[svc], managerServices[bystander] = true, true
	managerServicesLock.Unlock()
	t.Cleanup(func() {
		managerServicesLock.Lock()
		delete(managerServices, svc)
		delete(managerServices, bystander)
		managerServicesLock.Unlock()
		svc.stopEvents()
		bystander.stopEvents()
		w.Close()
		otherW.Close()
	})

	const n = 300
	for i := 0; i < n; i++ {
		notifyOne(svc, UpdateProgressNotificationType, "Downloading update", uint64(i), uint64(n), "", false)
	}

	dec := gob.NewDecoder(r)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			var typ NotificationType
			var activity, errText string
			var got, total uint64
			var complete bool
			for _, v := range []any{&typ, &activity, &got, &total, &errText, &complete} {
				if err := dec.Decode(v); err != nil {
					done <- fmt.Errorf("event %d: %w", i, err)
					return
				}
			}
			if got != uint64(i) {
				done <- fmt.Errorf("event %d arrived as %d: out of order", i, got)
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the notifications")
	}

	// Nothing went to the other UI.
	otherW.Close()
	if data, _ := io.ReadAll(other); len(data) != 0 {
		t.Fatalf("a UI that didn't ask for the update got %d bytes", len(data))
	}
}

func TestQueueEventIsBounded(t *testing.T) {
	svc := &ManagerService{} // no events pipe: the writer drops what it takes
	svc.queue.mu.Lock()
	svc.queue.running = true // keep the writer from draining the queue
	svc.queue.mu.Unlock()
	for i := 0; i < eventQueueSize+10; i++ {
		svc.queueEvent([]byte("x"))
	}
	svc.queue.mu.Lock()
	n := len(svc.queue.pending)
	svc.queue.mu.Unlock()
	svc.stopEvents()
	if n != eventQueueSize {
		t.Fatalf("queue holds %d events, want at most %d", n, eventQueueSize)
	}
}
