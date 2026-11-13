package ui

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fosrl/windows-linux/updater"
)

func resetUpdateInfo(t *testing.T, phase string) {
	t.Helper()
	updateMu.Lock()
	old := updateInfo
	updateInfo = UpdateInfo{Phase: phase}
	updateMu.Unlock()
	t.Cleanup(func() {
		updateMu.Lock()
		updateInfo = old
		updateMu.Unlock()
	})
}

// A stale progress event that arrives after the error or completion it
// preceded must not put the window back on "Downloading".
func TestApplyProgressIgnoresStaleEvents(t *testing.T) {
	for _, phase := range []string{updatePhaseError, updatePhaseComplete} {
		info := UpdateInfo{Phase: phase, Error: "boom"}
		if applyProgress(&info, updater.DownloadProgress{Activity: "Downloading update", BytesDownloaded: 5}) {
			t.Errorf("phase %s: stale progress was applied", phase)
		}
		if info.Phase != phase || info.BytesDownloaded != 0 {
			t.Errorf("phase %s: info changed to %+v", phase, info)
		}
	}
	info := UpdateInfo{Phase: updatePhaseDownloading}
	if !applyProgress(&info, updater.DownloadProgress{Activity: "Downloading update", BytesDownloaded: 5, BytesTotal: 10}) {
		t.Fatal("progress while downloading was ignored")
	}
	if info.Activity != "Downloading update" || info.BytesDownloaded != 5 || info.BytesTotal != 10 {
		t.Fatalf("info = %+v", info)
	}
}

// Two quick clicks on Install Update start one update.
func TestBeginUpdateOnlyOnce(t *testing.T) {
	resetUpdateInfo(t, updatePhaseAvailable)
	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if beginUpdate() {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	if started.Load() != 1 {
		t.Fatalf("%d concurrent clicks started %d updates, want 1", 8, started.Load())
	}
}

// Try Again after a failure starts a new attempt.
func TestBeginUpdateAfterError(t *testing.T) {
	resetUpdateInfo(t, updatePhaseError)
	updateInfo.Error = "old"
	if !beginUpdate() {
		t.Fatal("Try Again was ignored")
	}
	if updateInfo.Phase != updatePhaseDownloading || updateInfo.Error != "" {
		t.Fatalf("info = %+v", updateInfo)
	}
	// Progress of the new attempt is accepted again.
	if !applyProgress(&updateInfo, updater.DownloadProgress{Activity: "Downloading update"}) {
		t.Fatal("progress after Try Again was ignored")
	}
}
