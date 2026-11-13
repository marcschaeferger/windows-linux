package fingerprint

import (
	"testing"
	"time"
)

func resetPostureCache() {
	postureMemMu.Lock()
	defer postureMemMu.Unlock()
	postureMemFP, postureMemPostures, postureMemOK, postureMemAt = nil, nil, false, time.Time{}
	postureFailFP, postureFailPostures, postureFailAt = nil, nil, time.Time{}
}

type fakeGather struct {
	ok       bool
	firewall bool
	calls    int
}

func (f *fakeGather) gather() (*Fingerprint, *PostureChecks, bool) {
	f.calls++
	return &Fingerprint{Hostname: "h", PlatformFingerprint: "fp"}, &PostureChecks{FirewallEnabled: f.firewall}, f.ok
}

func setupPostureTest(t *testing.T, f *fakeGather) *time.Time {
	t.Helper()
	resetPostureCache()
	clock := time.Unix(1_700_000_000, 0)
	oldGather, oldNow := gatherPosture, now
	gatherPosture = f.gather
	now = func() time.Time { return clock }
	t.Cleanup(func() { gatherPosture, now = oldGather, oldNow; resetPostureCache() })
	return &clock
}

// Audit #19: a failed gather is not a good snapshot and must not replace one.
func TestFailedGatherDoesNotOverwriteGoodSnapshot(t *testing.T) {
	f := &fakeGather{ok: true, firewall: true}
	clock := setupPostureTest(t, f)

	if !RefreshPostureMemory() {
		t.Fatal("good gather reported as failed")
	}
	*clock = clock.Add(time.Hour)
	f.ok, f.firewall = false, false
	if RefreshPostureMemory() {
		t.Fatal("failed gather reported as success")
	}

	snap, ok := LatestDevicePosture()
	if !ok || snap.Postures["firewallEnabled"] != true {
		t.Fatalf("last good snapshot lost: %+v ok=%v", snap, ok)
	}
}

func TestFailedGatherIsNotCachedAsSuccess(t *testing.T) {
	f := &fakeGather{ok: false}
	clock := setupPostureTest(t, f)

	if RefreshPostureMemory() {
		t.Fatal("failed gather reported as success")
	}
	// Within the retry window callers get the degraded snapshot instead of
	// repeating the slow query...
	if _, ok := CachedDevicePosture(); !ok {
		t.Fatal("expected the degraded snapshot inside the retry window")
	}
	// ...afterwards the cache misses so the caller gathers again.
	*clock = clock.Add(PostureRetryInterval + time.Second)
	if _, ok := CachedDevicePosture(); ok {
		t.Fatal("failed gather stayed cached as a success")
	}
	if _, ok := CachedDevicePostureMaxAge(time.Hour); ok {
		t.Fatal("failed gather satisfied a max-age lookup")
	}
	// A connect with no good snapshot can still use the degraded one.
	if _, ok := LatestDevicePosture(); !ok {
		t.Fatal("no degraded fallback for connect")
	}
}

// Audit #18: a snapshot older than the max age forces a refresh.
func TestSnapshotMaxAge(t *testing.T) {
	f := &fakeGather{ok: true}
	clock := setupPostureTest(t, f)
	RefreshPostureMemory()

	if _, ok := CachedDevicePostureMaxAge(PostureMaxAgeAtConnect); !ok {
		t.Fatal("fresh snapshot missed")
	}
	*clock = clock.Add(PostureMaxAgeAtConnect + time.Second)
	if _, ok := CachedDevicePostureMaxAge(PostureMaxAgeAtConnect); ok {
		t.Fatal("stale snapshot served")
	}
	if _, ok := CachedDevicePosture(); !ok {
		t.Fatal("age-agnostic lookup should still hit")
	}

	// A refresh that fails right after must not make connect retry for 2 min.
	f.ok = false
	RefreshPostureMemory()
	snap, ok := CachedDevicePostureMaxAge(PostureMaxAgeAtConnect)
	if !ok || snap.Fingerprint == nil {
		t.Fatal("recent failure should be remembered")
	}
}
