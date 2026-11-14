package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func legacyHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func neverCalled(t *testing.T) func() string {
	return func() string {
		t.Helper()
		t.Error("machine id must not be read when the serial is usable")
		return "x"
	}
}

// A machine with a real serial keeps the fingerprint earlier versions computed,
// so already-registered devices are not re-created.
func TestFingerprintUnchangedForUsableSerial(t *testing.T) {
	got := platformFingerprintHash("linux", "amd64", "vendor=genuineintel|model=85", "sys_vendor=dell|product_name=xps",
		"ABC123", neverCalled(t))
	want := legacyHash("linux|amd64|vendor=genuineintel|model=85|sys_vendor=dell|product_name=xps|serial=abc123")
	if got != want {
		t.Fatalf("fingerprint of a machine with a good serial changed:\n got %s\nwant %s", got, want)
	}
}

func TestFingerprintDiffersPerMachineWithoutSerial(t *testing.T) {
	hash := func(serial, id string) string {
		return platformFingerprintHash("linux", "amd64", "cpu", "dmi", serial, func() string { return id })
	}
	for _, serial := range []string{"", "To Be Filled By O.E.M.", "Default string", "0000000000", "N/A", "Not Applicable"} {
		a, b := hash(serial, "machine-a"), hash(serial, "machine-b")
		if a == b {
			t.Errorf("identical machines share a fingerprint with serial %q", serial)
		}
		if a != hash(serial, "machine-a") {
			t.Errorf("fingerprint is not stable for serial %q", serial)
		}
	}
	// Different placeholders on the same machine are the same machine.
	if hash("", "m") != hash("Default string", "m") {
		t.Error("placeholder text leaks into the fingerprint")
	}
}

func TestFingerprintWithoutAnyIdentifierIsLegacy(t *testing.T) {
	got := platformFingerprintHash("linux", "amd64", "cpu", "dmi", "", func() string { return "" })
	if want := legacyHash("linux|amd64|cpu|dmi"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestIsUsefulSerial(t *testing.T) {
	useless := []string{"", "  ", "To be filled by O.E.M.", "DEFAULT STRING", "None", "Not Specified", "System Serial Number",
		"0123456789", "00000000", "0000000000000000", "FFFFFFFF", "O.E.M.", "Not Applicable", "N/A", "Unknown"}
	for _, s := range useless {
		if isUsefulSerial(s) {
			t.Errorf("%q treated as a real serial", s)
		}
	}
	for _, s := range []string{"PF2ABCDE", "5CG1234XYZ", "VMware-56 4d 12", "0001"} {
		if !isUsefulSerial(s) {
			t.Errorf("%q rejected", s)
		}
	}
}

func TestAppSpecificIDDoesNotExposeRawID(t *testing.T) {
	raw := "0123456789abcdef0123456789abcdef"
	id := appSpecificID(raw)
	if id == raw || len(id) != 64 || appSpecificID(" "+raw+"\n") != id || appSpecificID("other") == id {
		t.Fatalf("unexpected app-specific id %q", id)
	}
}

func withCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cachePathOverride = filepath.Join(dir, serialCacheFileName)
	t.Cleanup(func() { cachePathOverride = "" })
	return dir
}

func TestCacheFieldsSurviveEachOther(t *testing.T) {
	withCacheDir(t)
	saveCachedSerial("SER123")
	id := loadOrCreateInstallID(func() string { return "install-1" })
	saveCPUTopology(&cpuTopologyCache{Key: "k", Siblings: "8", Cores: "4"})
	saveCachedSerial("SER456")

	if got := loadCachedSerial(); got != "SER456" {
		t.Errorf("serial = %q", got)
	}
	if id != "install-1" || loadOrCreateInstallID(func() string { return "install-2" }) != "install-1" {
		t.Errorf("install id not stable: %q", id)
	}
	if topo := loadCPUTopology(); topo == nil || topo.Siblings != "8" {
		t.Errorf("topology lost: %+v", topo)
	}
}

func TestPlaceholderSerialNotCached(t *testing.T) {
	dir := withCacheDir(t)
	saveCachedSerial("To Be Filled By O.E.M.")
	if got := loadCachedSerial(); got != "" {
		t.Errorf("placeholder cached: %q", got)
	}
	// A placeholder an older version cached is ignored on read.
	path := cachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"serialNumber":"Default string"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadCachedSerial(); got != "" {
		t.Errorf("stale placeholder used: %q (dir %s)", got, dir)
	}
}

func TestInstallIDWithoutGeneratorIsEmpty(t *testing.T) {
	withCacheDir(t)
	if id := loadOrCreateInstallID(func() string { return "" }); id != "" {
		t.Fatalf("got %q", id)
	}
}
