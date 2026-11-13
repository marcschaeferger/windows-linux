package fingerprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fosrl/newt/logger"
)

// serialCacheFileName holds values that must stay put between runs so the
// platform fingerprint doesn't drift: the last good hardware serial, a
// per-install ID for machines without any usable identifier, and the CPU
// topology (see cpuTopologyCache). It lives in the daemon's state directory
// (user data directory for non-root/user processes); see serialCachePath.
const serialCacheFileName = "fingerprint_cache.json"

type fingerprintCacheFile struct {
	SerialNumber string            `json:"serialNumber,omitempty"`
	InstallID    string            `json:"installId,omitempty"`
	CPUTopology  *cpuTopologyCache `json:"cpuTopology,omitempty"`
}

var serialCacheMu sync.Mutex

// cachePathOverride replaces serialCachePath in tests, which must not touch
// the real state directory (running as root, that is /var/lib/pangolin-client).
var cachePathOverride string

func cachePath() string {
	if cachePathOverride != "" {
		return cachePathOverride
	}
	return serialCachePath()
}

// readCacheLocked returns the cache file's content, or an empty value.
func readCacheLocked() fingerprintCacheFile {
	var cached fingerprintCacheFile
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return cached
	}
	if err := json.Unmarshal(data, &cached); err != nil {
		return fingerprintCacheFile{}
	}
	return cached
}

// updateCache applies change to the cache file's content and writes it back if
// change returns true. Fields change doesn't touch are preserved.
func updateCache(change func(*fingerprintCacheFile) bool) {
	serialCacheMu.Lock()
	defer serialCacheMu.Unlock()

	cached := readCacheLocked()
	if !change(&cached) {
		return
	}

	path := cachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logger.Debug("Fingerprint: failed to create cache directory: %v", err)
		return
	}

	data, err := json.Marshal(cached)
	if err != nil {
		return
	}

	// Write-and-rename so a crash can't leave a truncated file that would
	// silently reset every cached value.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		logger.Debug("Fingerprint: failed to write cache: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		logger.Debug("Fingerprint: failed to replace cache: %v", err)
	}
}

// loadCachedSerial returns the last good serial. A placeholder that an earlier
// version cached is ignored.
func loadCachedSerial() string {
	serialCacheMu.Lock()
	defer serialCacheMu.Unlock()

	serial := strings.TrimSpace(readCacheLocked().SerialNumber)
	if !isUsefulSerial(serial) {
		return ""
	}
	return serial
}

// saveCachedSerial remembers a good serial; placeholders are not worth keeping.
func saveCachedSerial(serial string) {
	serial = strings.TrimSpace(serial)
	if !isUsefulSerial(serial) {
		return
	}
	updateCache(func(c *fingerprintCacheFile) bool {
		if c.SerialNumber == serial {
			return false
		}
		c.SerialNumber = serial
		return true
	})
}

// loadOrCreateInstallID returns the per-install ID, creating it with generate
// on first use. It is "" if the ID can't be created or stored: an ID that
// would change on every run is worse than none.
func loadOrCreateInstallID(generate func() string) string {
	var id string
	updateCache(func(c *fingerprintCacheFile) bool {
		if c.InstallID != "" {
			id = c.InstallID
			return false
		}
		id = generate()
		c.InstallID = id
		return id != ""
	})
	if id == "" {
		return ""
	}
	// Confirm that it was persisted.
	serialCacheMu.Lock()
	defer serialCacheMu.Unlock()
	if readCacheLocked().InstallID != id {
		return ""
	}
	return id
}

// cpuTopologyCache pins the topology values of /proc/cpuinfo that depend on
// which CPUs are online. Key identifies the CPU model they were read for.
type cpuTopologyCache struct {
	Key      string `json:"key"`
	Siblings string `json:"siblings,omitempty"`
	Cores    string `json:"cores,omitempty"`
}

func loadCPUTopology() *cpuTopologyCache {
	serialCacheMu.Lock()
	defer serialCacheMu.Unlock()
	return readCacheLocked().CPUTopology
}

func saveCPUTopology(t *cpuTopologyCache) {
	updateCache(func(c *fingerprintCacheFile) bool {
		c.CPUTopology = t
		return true
	})
}
