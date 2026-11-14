package ui

import (
	"strings"
	"testing"

	"github.com/fosrl/windows-linux/auth"
	"github.com/fosrl/windows-linux/config"
)

func TestApplySettingsDeviceName(t *testing.T) {
	tests := []struct {
		name      string
		saved     *string // device name in the config beforehand
		input     string
		wantField string
		want      *string // device name in the config afterwards
	}{
		{name: "set a name", input: "  work pc  ", want: ptr("work pc")},
		{name: "unchanged", saved: ptr("work pc"), input: "work pc", want: ptr("work pc")},
		{name: "use computer name", saved: ptr("work pc"), input: "", want: nil},
		{name: "blank is computer name", saved: ptr("work pc"), input: "   ", want: nil},
		{name: "255 units fit", input: strings.Repeat("a", 255), want: ptr(strings.Repeat("a", 255))},
		{name: "256 units are too long", saved: ptr("old"), input: strings.Repeat("a", 256), wantField: "deviceName", want: ptr("old")},
		// An emoji is two UTF-16 units, which is how the server counts.
		{name: "128 emoji are too long", saved: ptr("old"), input: strings.Repeat("😀", 128), wantField: "deviceName", want: ptr("old")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			cm := config.NewConfigManager()
			cfg := cm.GetConfigCopy()
			cfg.DeviceName = tt.saved
			if !cm.Save(cfg) {
				t.Fatal("could not save the starting config")
			}
			prevConfig, prevAuth := configManager, authManager
			configManager, authManager = cm, nil
			defer func() { configManager, authManager = prevConfig, prevAuth }()

			form := currentSettings()
			form.DeviceName = tt.input
			res := applySettings(form, "global")

			if res.Field != tt.wantField {
				t.Errorf("field = %q (%s), want %q", res.Field, res.Error, tt.wantField)
			}
			if got := cm.GetConfigCopy().DeviceName; !samePtr(got, tt.want) {
				t.Errorf("saved device name = %s, want %s", show(got), show(tt.want))
			}
			// Not signed in: a changed name isn't synced, and nothing is reported.
			if res.DeviceNameStatus != "" {
				t.Errorf("status %q without an auth manager", res.DeviceNameStatus)
			}
			if tt.wantField == "" && res.Settings.DeviceName != config.NormalizeDeviceName(tt.input) {
				t.Errorf("shown device name = %q, want %q", res.Settings.DeviceName, tt.input)
			}
		})
	}
}

func TestDeviceNameStatusKind(t *testing.T) {
	tests := map[auth.DeviceNameSyncStatus]string{
		auth.DeviceNameRenamed:          "ok",
		auth.DeviceNameSkipped:          "ok",
		auth.DeviceNamePartial:          "warning",
		auth.DeviceNamePermissionDenied: "warning",
		auth.DeviceNameFailed:           "error",
	}
	for status, want := range tests {
		if got := deviceNameStatusKind(status); got != want {
			t.Errorf("deviceNameStatusKind(%s) = %q, want %q", status, got, want)
		}
	}
}

func ptr(s string) *string { return &s }

func samePtr(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func show(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return "\"" + *s + "\""
}
