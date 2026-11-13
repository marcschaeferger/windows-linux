package updater

import (
	"strings"
	"testing"

	"github.com/fosrl/windows-linux/version"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.0", 1},
		{"1.2", "1.2.0", 0},
		{"1.2.0.1", "1.2.0", 1},
		{"v1.2.3", "1.2.3", 0},
		// Pre-releases sort below their release.
		{"1.2.0", "1.2.0-rc.1", 1},
		{"1.2.0-rc.1", "1.2.0", -1},
		{"1.2.0-rc.2", "1.2.0-rc.1", 1},
		{"1.2.0-rc.10", "1.2.0-rc.9", 1},
		{"1.2.0-rc.1", "1.2.0-rc.1", 0},
		{"1.2.0-rc", "1.2.0-rc.1", -1},
		{"1.2.0-beta.1", "1.2.0-rc.1", -1},
		{"1.2.0-1", "1.2.0-rc", -1}, // numeric identifiers sort below text
		{"1.2.1-rc.1", "1.2.0", 1},
		{"1.2.0-rc.1", "1.1.9", 1},
		// Build metadata is ignored.
		{"1.2.0+abc", "1.2.0+def", 0},
		{"1.2.0-rc.1+abc", "1.2.0-rc.1", 0},
	}
	for _, tt := range tests {
		got, err := compareVersions(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestCompareVersionsInvalid(t *testing.T) {
	for _, v := range []string{"", "dev", "1..2", "1.2.x", "1.2.3-", "1.2.3-rc..1", "-rc.1", "1.2.99999999999"} {
		if _, err := compareVersions(v, "1.0.0"); err == nil {
			t.Errorf("compareVersions(%q, 1.0.0) accepted an invalid version", v)
		}
	}
}

func withVersion(t *testing.T, v string) {
	old := version.Number
	version.Number = v
	t.Cleanup(func() { version.Number = old })
}

func TestVersionNewerThanUs(t *testing.T) {
	tests := []struct {
		ours, candidate string
		want            bool
	}{
		{"1.2.0", "1.2.1", true},
		{"1.2.0", "1.2.0", false},
		{"1.2.0", "1.1.9", false},
		// An RC build is offered the final release and later RCs...
		{"1.2.0-rc.1", "1.2.0", true},
		{"1.2.0-rc.1", "1.2.0-rc.2", true},
		{"1.2.0-rc.1", "1.2.0-rc.1", false},
		// ...and a stable build is not offered an RC of the same or an older version.
		{"1.2.0", "1.2.0-rc.3", false},
		{"1.1.0", "1.2.0-rc.1", true}, // plain version order; findCandidate decides who is offered it
	}
	for _, tt := range tests {
		withVersion(t, tt.ours)
		got, err := versionNewerThanUs(tt.candidate)
		if err != nil || got != tt.want {
			t.Errorf("ours %q, candidate %q: got %v, %v; want %v", tt.ours, tt.candidate, got, err, tt.want)
		}
	}
}

func manifestEntry(version string) string {
	prefix, suffix := candidatePattern()
	return prefix + version + suffix
}

func TestFindCandidatePreRelease(t *testing.T) {
	withVersion(t, "1.2.0")
	// A broken or pre-release entry must not abort the check for every client,
	// and a stable client is never offered the pre-release.
	files := fileList{
		manifestEntry("garbage"):    {},
		manifestEntry("1.1.0"):      {},
		manifestEntry("1.3.0-rc.1"): {},
		manifestEntry("1.3.0"):      {},
		manifestEntry("1.2.5"):      {},
	}
	got, err := findCandidate(files)
	if err != nil {
		t.Fatalf("findCandidate: %v", err)
	}
	if got == nil || got.version != "1.3.0" {
		t.Fatalf("findCandidate = %+v, want 1.3.0 (the newest, not the first in map order)", got)
	}
}

func TestFindCandidateStableSkipsPreRelease(t *testing.T) {
	withVersion(t, "1.1.0")
	got, err := findCandidate(fileList{manifestEntry("1.2.0-rc.1"): {}})
	if err != nil || got != nil {
		t.Fatalf("findCandidate = %+v, %v; a stable client must not be offered an rc", got, err)
	}
}

func TestFindCandidateRCBuildGetsNewerRC(t *testing.T) {
	withVersion(t, "1.2.0-rc.1")
	got, err := findCandidate(fileList{manifestEntry("1.2.0-rc.2"): {}, manifestEntry("1.1.0"): {}})
	if err != nil || got == nil || got.version != "1.2.0-rc.2" {
		t.Fatalf("findCandidate = %+v, %v; want 1.2.0-rc.2", got, err)
	}
}

func TestFindCandidateOnRCBuild(t *testing.T) {
	withVersion(t, "1.2.0-rc.1")
	got, err := findCandidate(fileList{manifestEntry("1.2.0"): {}})
	if err != nil || got == nil || got.version != "1.2.0" {
		t.Fatalf("findCandidate on an RC build = %+v, %v; want 1.2.0", got, err)
	}
	got, err = findCandidate(fileList{manifestEntry("1.2.0-rc.1"): {}})
	if err != nil || got != nil {
		t.Fatalf("findCandidate = %+v, %v; want no update", got, err)
	}
}

func TestFindCandidateUnparseableOwnVersion(t *testing.T) {
	withVersion(t, "dev")
	_, err := findCandidate(fileList{manifestEntry("1.2.0"): {}})
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("findCandidate on a dev build: err = %v, want an explanatory error", err)
	}
}
