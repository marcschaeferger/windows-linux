package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/fosrl/windows-linux/version"
)

func TestDefaultDeviceName(t *testing.T) {
	fallback := func() string { return "Linux Laptop" }
	long := strings.Repeat("a", 300)

	tests := []struct {
		name     string
		hostname string
		err      error
		want     string
	}{
		{"hostname", "marcs-thinkpad", nil, "marcs-thinkpad"},
		{"trimmed", "  build-box\n", nil, "build-box"},
		{"empty falls back", "", nil, "Linux Laptop"},
		{"blank falls back", "   ", nil, "Linux Laptop"},
		{"error falls back", "whatever", errors.New("boom"), "Linux Laptop"},
		{"localhost falls back", "localhost", nil, "Linux Laptop"},
		{"localhost.localdomain falls back", "LOCALHOST.localdomain", nil, "Linux Laptop"},
		{"capped", long, nil, strings.Repeat("a", MaxDeviceNameLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultDeviceName(func() (string, error) { return tt.hostname, tt.err }, fallback)
			if got != tt.want {
				t.Errorf("defaultDeviceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultDeviceNameUsesHostnameFunc(t *testing.T) {
	oldHost, oldFallback := hostnameFunc, friendlyDeviceNameFunc
	t.Cleanup(func() { hostnameFunc, friendlyDeviceNameFunc = oldHost, oldFallback })
	hostnameFunc = func() (string, error) { return "desk-42", nil }
	friendlyDeviceNameFunc = func() string { return "Windows Desktop" }

	if got := DefaultDeviceName(); got != "desk-42" {
		t.Errorf("DefaultDeviceName() = %q, want desk-42", got)
	}
	hostnameFunc = func() (string, error) { return "localhost", nil }
	if got := DefaultDeviceName(); got != "Windows Desktop" {
		t.Errorf("DefaultDeviceName() = %q, want the fallback", got)
	}
}

func TestEffectiveDeviceName(t *testing.T) {
	def := func() string { return "host-1" }
	tests := []struct {
		name, override, want string
	}{
		{"no override", "", "host-1"},
		{"blank override", "   ", "host-1"},
		{"override", "Marc's laptop", "Marc's laptop"},
		{"override trimmed", "  work pc  ", "work pc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveDeviceName(tt.override, def); got != tt.want {
				t.Errorf("effectiveDeviceName(%q) = %q, want %q", tt.override, got, tt.want)
			}
		})
	}
}

func TestDeviceNameOverrideFromConfig(t *testing.T) {
	blank := "  "
	set := " kiosk-3 "
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, ""},
		{"unset", &Config{}, ""},
		{"blank", &Config{DeviceName: &blank}, ""},
		{"set", &Config{DeviceName: &set}, "kiosk-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deviceNameOverride(tt.cfg); got != tt.want {
				t.Errorf("deviceNameOverride() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeviceNameMergeAndCopy(t *testing.T) {
	system := "fleet-default"
	user := "my-name"
	merged := mergeConfig(&Config{DeviceName: &system}, &Config{DeviceName: &user})
	if merged.DeviceName == nil || *merged.DeviceName != user {
		t.Fatalf("user device name should win: %+v", merged.DeviceName)
	}
	merged = mergeConfig(&Config{DeviceName: &system}, &Config{})
	if merged.DeviceName == nil || *merged.DeviceName != system {
		t.Fatalf("system device name lost: %+v", merged.DeviceName)
	}
	cp := copyConfig(merged)
	*cp.DeviceName = "changed"
	if *merged.DeviceName != system {
		t.Fatal("copyConfig must deep-copy DeviceName")
	}
}

func TestTruncateDeviceName(t *testing.T) {
	emoji := "\U0001F600" // two UTF-16 code units
	tests := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"short", "abc", 3},
		{"exact", strings.Repeat("x", 255), 255},
		{"ascii over", strings.Repeat("x", 256), 255},
		{"umlauts", strings.Repeat("ä", 300), 255},
		{"astral does not split", strings.Repeat(emoji, 200), 254},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateDeviceName(tt.in)
			if DeviceNameLength(got) != tt.wantLen {
				t.Errorf("length = %d, want %d", DeviceNameLength(got), tt.wantLen)
			}
			if !strings.HasPrefix(tt.in, got) {
				t.Errorf("result is not a prefix of the input")
			}
		})
	}
}

func TestIsLegacyDeviceName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Linux Laptop", true},
		{"Linux Desktop", true},
		{"Windows Laptop", true},
		{"Windows Desktop", true},
		{" Linux Laptop ", true},
		{"linux laptop", false},
		{"Linux Laptop 2", false},
		{"marcs-thinkpad", false},
		{"MacBook Pro", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLegacyDeviceName(tt.name); got != tt.want {
				t.Errorf("IsLegacyDeviceName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// The names this OS's GetFriendlyDeviceName produces must count as legacy.
func TestLegacyNamesCoverFriendlyName(t *testing.T) {
	for _, suffix := range []string{" Laptop", " Desktop"} {
		if name := version.OSDisplayName + suffix; !IsLegacyDeviceName(name) {
			t.Errorf("%q is not recognized as a legacy name", name)
		}
	}
	if name := GetFriendlyDeviceName(); !IsLegacyDeviceName(name) {
		t.Errorf("GetFriendlyDeviceName() = %q is not recognized as a legacy name", name)
	}
}

func TestDeviceNameSetting(t *testing.T) {
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return "\"" + *p + "\""
	}
	tests := []struct {
		name   string
		input  string
		system bool
		want   string
	}{
		{"name", " work pc ", false, `"work pc"`},
		{"name, system sets one", "work pc", true, `"work pc"`},
		{"computer name", "  ", false, "<nil>"},
		// An unset user value would let the machine-wide name apply again.
		{"computer name, system sets one", "", true, `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := str(DeviceNameSetting(tt.input, tt.system)); got != tt.want {
				t.Errorf("DeviceNameSetting(%q, %v) = %s, want %s", tt.input, tt.system, got, tt.want)
			}
		})
	}

	// The explicit "" wins over the machine-wide name and means the hostname.
	system, empty := "fleet-default", ""
	merged := mergeConfig(&Config{DeviceName: &system}, &Config{DeviceName: &empty})
	if got := deviceNameOverride(merged); got != "" {
		t.Errorf("user \"\" over system %q: override = %q, want none", system, got)
	}
	if !systemSetsDeviceName(&SystemConfig{Config: Config{DeviceName: &system}}) ||
		systemSetsDeviceName(&SystemConfig{Config: Config{DeviceName: &empty}}) || systemSetsDeviceName(nil) {
		t.Error("systemSetsDeviceName misreports")
	}
}
