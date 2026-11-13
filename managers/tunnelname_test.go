package managers

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateTunnelName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"olm", true},
		{"Olm_2-x", true},
		{"a", true},
		{"abcdefghijklmnopqrstuvwxyz012345", true}, // 32 chars
		{"abcdefghijklmnopqrstuvwxyz0123456", false},
		{"", false},
		{"..", false},
		{"../pangolin", false},
		{`..\..\..\Users\Public\x`, false},
		{`..\pangolin`, false},
		{`C:\path\target`, false},
		{"olm:stream", false},
		{"olm.json", false},
		{"olm ", false},
		{"ölm", false},
		{"olm\x00", false},
		{"/etc/passwd", false},
	}
	for _, tt := range tests {
		err := validateTunnelName(tt.name)
		if (err == nil) != tt.ok {
			t.Errorf("validateTunnelName(%q) error = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestTunnelConfigPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Tunnels")
	got, err := tunnelConfigPath(dir, "olm")
	if err != nil {
		t.Fatalf("tunnelConfigPath(olm): %v", err)
	}
	if want := filepath.Join(dir, "olm.json"); got != want {
		t.Errorf("tunnelConfigPath(olm) = %q, want %q", got, want)
	}
	for _, name := range []string{"../pangolin", `..\pangolin`, "a/b", "", ".."} {
		if p, err := tunnelConfigPath(dir, name); err == nil {
			t.Errorf("tunnelConfigPath(%q) = %q, want error", name, p)
		}
	}
}

func TestPathWithinFold(t *testing.T) {
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	base := filepath.Join(root, "Program Files", "Pangolin")
	tests := []struct {
		target string
		want   bool
	}{
		{filepath.Join(base, "Pangolin.exe"), true},
		{filepath.Join(root, "PROGRAM FILES", "pangolin", "Pangolin.exe"), true},
		{base, true},
		{filepath.Join(root, "Program Files", "PangolinEvil", "Pangolin.exe"), false},
		{filepath.Join(base, "..", "Other", "Pangolin.exe"), false},
		{filepath.Join(root, "Users", "x", "Downloads", "Pangolin.exe"), false},
		{"Pangolin.exe", false},
	}
	for _, tt := range tests {
		if got := pathWithinFold(base, tt.target); got != tt.want {
			t.Errorf("pathWithinFold(%q, %q) = %v, want %v", base, tt.target, got, tt.want)
		}
	}
}
