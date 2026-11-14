package config

import (
	"os"
	"strings"
	"unicode/utf8"
)

// MaxDeviceNameLength is the longest device name the server accepts. The
// server counts JavaScript string length, i.e. UTF-16 code units.
const MaxDeviceNameLength = 255

// hostnameFunc returns the computer's hostname; tests replace it.
var hostnameFunc = os.Hostname

// friendlyDeviceNameFunc returns the generic "<OS> Laptop/Desktop" name; tests
// replace it so they don't depend on the machine's battery.
var friendlyDeviceNameFunc = GetFriendlyDeviceName

// DefaultDeviceName is the name this device registers under when the user has
// not chosen one: the computer's hostname, like the Pangolin CLI. Placeholder
// hostnames ("localhost") and failures fall back to the generic
// "<OS> Laptop/Desktop" name the client used before.
func DefaultDeviceName() string {
	return defaultDeviceName(hostnameFunc, friendlyDeviceNameFunc)
}

func defaultDeviceName(hostname func() (string, error), fallback func() string) string {
	name, err := hostname()
	name = strings.TrimSpace(name)
	if err != nil || name == "" || isPlaceholderHostname(name) {
		return fallback()
	}
	return TruncateDeviceName(name)
}

func isPlaceholderHostname(name string) bool {
	switch strings.ToLower(name) {
	case "localhost", "localhost.localdomain":
		return true
	}
	return false
}

// NormalizeDeviceName trims name and caps it to MaxDeviceNameLength.
func NormalizeDeviceName(name string) string {
	return TruncateDeviceName(strings.TrimSpace(name))
}

// DeviceNameLength returns the length of name as the server counts it
// (UTF-16 code units).
func DeviceNameLength(name string) int {
	n := 0
	for _, r := range name {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// TruncateDeviceName caps name to MaxDeviceNameLength without splitting a
// character.
func TruncateDeviceName(name string) string {
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "")
	}
	n := 0
	for i, r := range name {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > MaxDeviceNameLength {
			return name[:i]
		}
		n += w
	}
	return name
}

// LegacyDeviceNames are the generic names older versions of this client gave
// every device, whatever it was called.
func LegacyDeviceNames() []string {
	return []string{
		"Linux Laptop", "Linux Desktop",
		"Windows Laptop", "Windows Desktop",
	}
}

// IsLegacyDeviceName reports whether name is one of the generic names older
// versions of this client registered devices under. Only those are renamed
// automatically: any other name may have been chosen by a person.
func IsLegacyDeviceName(name string) bool {
	name = strings.TrimSpace(name)
	for _, legacy := range LegacyDeviceNames() {
		if name == legacy {
			return true
		}
	}
	return false
}

// GetDeviceNameOverride returns the device name the user or administrator set,
// trimmed, or "" when none is set.
func (cm *ConfigManager) GetDeviceNameOverride() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return deviceNameOverride(cm.config)
}

func deviceNameOverride(cfg *Config) string {
	if cfg != nil && cfg.DeviceName != nil {
		return NormalizeDeviceName(*cfg.DeviceName)
	}
	return ""
}

// EffectiveDeviceName is the name to register this device under: the
// configured override, or else DefaultDeviceName.
func (cm *ConfigManager) EffectiveDeviceName() string {
	return effectiveDeviceName(cm.GetDeviceNameOverride(), DefaultDeviceName)
}

func effectiveDeviceName(override string, def func() string) string {
	if override = NormalizeDeviceName(override); override != "" {
		return override
	}
	return def()
}

// DeviceNameSetting is the value to store in Config.DeviceName for the name
// the user entered: the normalized name, or for an empty name (use the
// computer's name) nil. When the machine-wide config sets a device name, an
// empty name is stored as "" instead, since an unset user value would let the
// machine-wide one apply again on the next start.
func DeviceNameSetting(name string, systemSetsName bool) *string {
	name = NormalizeDeviceName(name)
	if name == "" && !systemSetsName {
		return nil
	}
	return &name
}

// SystemSetsDeviceName reports whether the machine-wide config sets a device
// name, which applies unless the user's config sets one (possibly empty).
func SystemSetsDeviceName() bool {
	return systemSetsDeviceName(LoadSystemConfig())
}

func systemSetsDeviceName(sys *SystemConfig) bool {
	return sys != nil && sys.DeviceName != nil && NormalizeDeviceName(*sys.DeviceName) != ""
}
