package updater

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/version"
)

// parsedVersion is a version like 1.2.3 or 1.2.3-rc.1+build, ordered by the
// semver rules: dotted numeric core first, then a version with a pre-release
// tag sorts below the same version without one. Build metadata is ignored.
type parsedVersion struct {
	core []uint64
	pre  []string
}

func parseVersion(v string) (parsedVersion, error) {
	var pv parsedVersion
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core = v[:i]
		pre := v[i+1:]
		if pre == "" {
			return pv, errors.New("Empty pre-release part")
		}
		pv.pre = strings.Split(pre, ".")
		for _, id := range pv.pre {
			if id == "" {
				return pv, errors.New("Empty pre-release identifier")
			}
		}
	}
	if core == "" {
		return pv, errors.New("Empty version")
	}
	for _, part := range strings.Split(core, ".") {
		if part == "" {
			return pv, errors.New("Empty version part")
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return pv, errors.New("Invalid version integer part")
		}
		pv.core = append(pv.core, n)
	}
	return pv, nil
}

// compareVersions returns -1, 0 or 1 as a is older than, equal to or newer than b.
func compareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", a, err)
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", b, err)
	}
	for i := 0; i < len(pa.core) || i < len(pb.core); i++ {
		var x, y uint64
		if i < len(pa.core) {
			x = pa.core[i]
		}
		if i < len(pb.core) {
			y = pb.core[i]
		}
		if x != y {
			return cmpUint(x, y), nil
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, nil
	case len(pa.pre) == 0:
		return 1, nil
	case len(pb.pre) == 0:
		return -1, nil
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePreIdentifier(pa.pre[i], pb.pre[i]); c != 0 {
			return c, nil
		}
	}
	return cmpUint(uint64(len(pa.pre)), uint64(len(pb.pre))), nil
}

func cmpUint(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// comparePreIdentifier orders pre-release identifiers: numbers numerically and
// below text, text in ASCII order.
func comparePreIdentifier(a, b string) int {
	an, aErr := strconv.ParseUint(a, 10, 64)
	bn, bErr := strconv.ParseUint(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		return cmpUint(an, bn)
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func versionNewerThanUs(candidate string) (bool, error) {
	logger.Debug("Updater: Comparing versions - candidate: %s, current: %s", candidate, version.Number)
	c, err := compareVersions(candidate, version.Number)
	if err != nil {
		return false, err
	}
	logger.Debug("Updater: Candidate is newer: %v", c > 0)
	return c > 0, nil
}

func findCandidate(candidates fileList) (*UpdateFound, error) {
	prefix, suffix := candidatePattern()
	currentVersion := version.Number
	logger.Debug("Updater: findCandidate() - Current version: %s, Architecture: %s", currentVersion, runtime.GOARCH)
	logger.Debug("Updater: Looking for files matching prefix: %s, suffix: %s", prefix, suffix)
	logger.Debug("Updater: Total files in manifest: %d", len(candidates))

	if _, err := parseVersion(currentVersion); err != nil {
		return nil, fmt.Errorf("this build's version %q can't be compared with update versions: %w", currentVersion, err)
	}

	var best *UpdateFound
	for name, entry := range candidates {
		logger.Debug("Updater: Checking file: %s", name)
		hasPrefix := strings.HasPrefix(name, prefix)
		hasSuffix := strings.HasSuffix(name, suffix)
		logger.Debug("Updater: File %s - hasPrefix(%s): %v, hasSuffix(%s): %v", name, prefix, hasPrefix, suffix, hasSuffix)

		if hasPrefix && hasSuffix {
			candidateVersion := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
			logger.Debug("Updater: File matches pattern! Extracted version: %s", candidateVersion)

			if len(candidateVersion) > 128 {
				logger.Debug("Updater: Version string too long: %d characters", len(candidateVersion))
				return nil, errors.New("Version length is too long")
			}

			logger.Debug("Updater: Comparing candidate version %s with current version %s", candidateVersion, currentVersion)
			newer, err := versionNewerThanUs(candidateVersion)
			if err != nil {
				// One unparseable entry must not hide the other candidates.
				logger.Warn("Updater: Skipping %s: error comparing version %s: %v", name, candidateVersion, err)
				continue
			}
			logger.Debug("Updater: Version comparison result - %s is newer than %s: %v", candidateVersion, currentVersion, newer)

			if newer {
				logger.Debug("Updater: ✓ Update candidate found: %s (hash: %x, location: %s)", name, entry.hash, entry.downloadLocation)
				// Several newer entries can share a manifest; take the newest, not the first in map order.
				if best != nil {
					if c, err := compareVersions(candidateVersion, best.version); err != nil || c <= 0 {
						continue
					}
				}
				best = &UpdateFound{
					name:             name,
					version:          candidateVersion,
					hash:             entry.hash,
					downloadLocation: entry.downloadLocation,
				}
			} else {
				logger.Debug("Updater: Candidate version %s is not newer, skipping", candidateVersion)
			}
		} else {
			logger.Debug("Updater: File %s does not match pattern (needs prefix '%s' and suffix '%s')", name, prefix, suffix)
		}
	}
	if best == nil {
		logger.Debug("Updater: No update candidate found after checking all %d files", len(candidates))
	}
	return best, nil
}
