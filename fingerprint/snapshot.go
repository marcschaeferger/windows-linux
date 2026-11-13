package fingerprint

import "time"

// DevicePostureSnapshot is fingerprint and posture data from the manager cache.
type DevicePostureSnapshot struct {
	Fingerprint map[string]any
	Postures    map[string]any
}

// PlatformFingerprint returns the hashed platform fingerprint from the snapshot.
func (s DevicePostureSnapshot) PlatformFingerprint() (string, bool) {
	if s.Fingerprint == nil {
		return "", false
	}
	v, ok := s.Fingerprint["platformFingerprint"].(string)
	return v, ok && v != ""
}

// CachedDevicePosture returns the current cached snapshot, if available.
func CachedDevicePosture() (DevicePostureSnapshot, bool) {
	return snapshotOf(SnapshotPostureMemory())
}

// CachedDevicePostureMaxAge is CachedDevicePosture, but a snapshot older than
// maxAge counts as missing.
func CachedDevicePostureMaxAge(maxAge time.Duration) (DevicePostureSnapshot, bool) {
	return snapshotOf(SnapshotPostureMemoryMaxAge(maxAge))
}

// LatestDevicePosture returns the best snapshot after a refresh attempt: the
// last good one, however old, else the one from a failed gather.
func LatestDevicePosture() (DevicePostureSnapshot, bool) {
	return snapshotOf(SnapshotPostureMemoryLatest())
}

func snapshotOf(fp, postures map[string]any, ok bool) (DevicePostureSnapshot, bool) {
	if !ok {
		return DevicePostureSnapshot{}, false
	}
	return DevicePostureSnapshot{Fingerprint: fp, Postures: postures}, true
}
