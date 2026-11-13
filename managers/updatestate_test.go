package managers

import (
	"errors"
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

// A background install that returns without replacing this process must not
// leave the UI locked out for good.
func TestFinishStagedInstallClearsFlag(t *testing.T) {
	updateLock.Lock()
	installingStagedUpdate = true
	pendingLaunchSessions = []uint32{1000, 1001}
	updateState = UpdateStateUpdateDownloaded
	updateLock.Unlock()

	var got []uint32
	finishStagedInstall(func(sessions []uint32) { got = sessions })

	if stagedUpdateInstalling() {
		t.Fatal("installingStagedUpdate is still set")
	}
	if len(got) != 2 || got[0] != 1000 || got[1] != 1001 {
		t.Fatalf("onFailure sessions = %v, want the held-back launches", got)
	}
	updateLock.Lock()
	pending := pendingLaunchSessions
	updateLock.Unlock()
	if len(pending) != 0 {
		t.Fatalf("pendingLaunchSessions = %v, want empty", pending)
	}
}

// Notifications must reach a UI in the order they were sent: a stale progress
// event arriving after the final one used to leave the update window stuck.
func TestNotificationsKeepOrder(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	svc := &ManagerService{events: w, eventQueue: make(chan []byte, eventQueueSize)}
	go svc.writeEvents()

	const n = 200
	for i := 0; i < n; i++ {
		svc.enqueueEvent([]byte{byte(i)})
	}
	managerServicesLock.Lock()
	svc.closeEventQueue()
	managerServicesLock.Unlock()

	got := make([]byte, 0, n)
	buf := make([]byte, n)
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		type result struct {
			n   int
			err error
		}
		ch := make(chan result, 1)
		go func() { k, err := r.Read(buf); ch <- result{k, err} }()
		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("read: %v", res.err)
			}
			got = append(got, buf[:res.n]...)
		case <-deadline:
			t.Fatalf("timed out after %d of %d bytes", len(got), n)
		}
	}
	for i := 0; i < n; i++ {
		if got[i] != byte(i) {
			t.Fatalf("event %d arrived as %d: out of order", i, got[i])
		}
	}
	w.Close()
}

func TestEnqueueAfterCloseOrWithoutQueueDoesNotPanic(t *testing.T) {
	svc := &ManagerService{} // no queue, as in a service that was never served
	svc.enqueueEvent([]byte("x"))
	svc.closeEventQueue()
	svc.closeEventQueue()
}
