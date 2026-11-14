package fingerprint

import (
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
)

const (
	// PostureMaxAgeAtConnect is how old the cached posture may be when a
	// tunnel starts. Posture is only sent to the server at connect, so a user
	// who fixes a failing check (firewall, disk encryption) and reconnects
	// must not be judged on a snapshot from hours ago.
	PostureMaxAgeAtConnect = 5 * time.Minute

	// PostureRetryInterval is how soon a failed gather is retried. Until
	// then the failure is remembered instead of repeating a slow query on
	// every call. Repeated failures back off up to postureRetryMax, so a
	// machine where the query can never work (PowerShell blocked by policy)
	// doesn't run it every two minutes forever.
	PostureRetryInterval = 2 * time.Minute
	postureRetryMax      = time.Hour
)

var (
	postureMemMu sync.RWMutex

	// The last snapshot that was gathered successfully.
	postureMemFP       map[string]any
	postureMemPostures map[string]any
	postureMemOK       bool
	postureMemAt       time.Time

	// The last snapshot of a failed gather (every check false on Windows),
	// kept only so a connect with no good snapshot can still proceed. It is
	// never reported as a good snapshot and never replaces one.
	postureFailFP       map[string]any
	postureFailPostures map[string]any
	postureFailAt       time.Time
	postureFailStreak   int

	postureLastRefreshAt time.Time
	postureLastRefreshOK bool

	// refreshMu makes concurrent refreshes share one gather.
	refreshMu sync.Mutex

	// Replaced in tests.
	gatherPosture = gatherDevicePosture
	now           = time.Now
)

// RefreshPostureMemory gathers fingerprint and posture again. It returns
// false when the gather failed; the previous good snapshot is then kept, so a
// transient WMI or PowerShell failure cannot overwrite it with defaults.
func RefreshPostureMemory() bool {
	started := now()
	refreshMu.Lock()
	defer refreshMu.Unlock()

	// Another caller refreshed while this one waited for the lock; use its
	// result instead of running the slow query again.
	postureMemMu.RLock()
	shared, done := postureLastRefreshOK, postureLastRefreshAt
	postureMemMu.RUnlock()
	if !done.IsZero() && !done.Before(started) {
		return shared
	}

	logger.Debug("Fingerprint: RefreshPostureMemory() starting")
	fingerprint, posturesStruct, gathered := gatherPosture()
	fp := fingerprint.ToMap()
	postures := posturesStruct.ToMap()
	logger.Debug("Fingerprint: RefreshPostureMemory() fingerprint keys=%d, posture keys=%d, gathered=%v",
		len(fp), len(postures), gathered)

	postureMemMu.Lock()
	defer postureMemMu.Unlock()
	postureLastRefreshAt = now()
	if !gathered || len(fp) == 0 || len(postures) == 0 {
		postureLastRefreshOK = false
		postureFailStreak++
		postureFailFP, postureFailPostures, postureFailAt = fp, postures, now()
		logger.Warn("Fingerprint: device posture gathering failed; keeping the last good snapshot (have one: %v)", postureMemOK)
		return false
	}
	postureMemFP, postureMemPostures, postureMemOK, postureMemAt = fp, postures, true, now()
	postureFailFP, postureFailPostures, postureFailAt, postureFailStreak = nil, nil, time.Time{}, 0
	postureLastRefreshOK = true
	logger.Debug("Fingerprint: RefreshPostureMemory() finished")
	return true
}

// recentlyFailedLocked reports whether a gather failed within
// PostureRetryInterval. Caller holds postureMemMu.
func recentlyFailedLocked() bool {
	return !postureFailAt.IsZero() && now().Sub(postureFailAt) < retryDelayLocked()
}

// retryDelayLocked is the wait after the current streak of failed gathers:
// PostureRetryInterval, doubling per further failure up to postureRetryMax.
func retryDelayLocked() time.Duration {
	d := PostureRetryInterval
	for i := 1; i < postureFailStreak && d < postureRetryMax; i++ {
		d *= 2
	}
	return min(d, postureRetryMax)
}

// PostureRetryDelay is how long to wait before refreshing again after a
// failed RefreshPostureMemory.
func PostureRetryDelay() time.Duration {
	postureMemMu.RLock()
	defer postureMemMu.RUnlock()
	return retryDelayLocked()
}

// latestLocked returns the good snapshot, else the one from a failed gather.
func latestLocked() (fp, postures map[string]any, ok bool) {
	if postureMemOK {
		return postureMemFP, postureMemPostures, true
	}
	if postureFailFP != nil {
		return postureFailFP, postureFailPostures, true
	}
	return nil, nil, false
}

// SnapshotPostureMemory returns the cached snapshot of any age. It is a miss
// when nothing good was gathered, unless a gather failed so recently that
// retrying now would only repeat the failure.
func SnapshotPostureMemory() (fp map[string]any, postures map[string]any, ok bool) {
	postureMemMu.RLock()
	defer postureMemMu.RUnlock()
	if !postureMemOK && !recentlyFailedLocked() {
		logger.Debug("Fingerprint: SnapshotPostureMemory() miss (cache not ready)")
		return nil, nil, false
	}
	return latestLocked()
}

// SnapshotPostureMemoryMaxAge is SnapshotPostureMemory, but a good snapshot
// older than maxAge is a miss too, so the caller refreshes it.
func SnapshotPostureMemoryMaxAge(maxAge time.Duration) (fp map[string]any, postures map[string]any, ok bool) {
	postureMemMu.RLock()
	defer postureMemMu.RUnlock()
	if postureMemOK && now().Sub(postureMemAt) <= maxAge {
		return postureMemFP, postureMemPostures, true
	}
	if recentlyFailedLocked() {
		return latestLocked()
	}
	logger.Debug("Fingerprint: SnapshotPostureMemoryMaxAge() miss (stale or not ready)")
	return nil, nil, false
}

// SnapshotPostureMemoryLatest returns the best snapshot there is after a
// refresh: the last good one, however old, else the one from a failed gather.
func SnapshotPostureMemoryLatest() (fp map[string]any, postures map[string]any, ok bool) {
	postureMemMu.RLock()
	defer postureMemMu.RUnlock()
	return latestLocked()
}
