package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// systemReadFails makes the fake system loader report a read error.
var systemReadFails bool

func ptr[T any](v T) *T { return &v }

// newTestManager returns a manager backed by a temp user file and a fake
// machine-wide config that the test can change through the returned pointer.
func newTestManager(t *testing.T, system *SystemConfig, userJSON string) (*ConfigManager, **SystemConfig, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ConfigFileName)
	if userJSON != "" {
		if err := os.WriteFile(path, []byte(userJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sys := system
	cm := &ConfigManager{configPath: path, loadSystem: func() (*SystemConfig, bool) { return sys, !systemReadFails }}
	systemReadFails = false
	cm.config = cm.load()
	return cm, &sys, path
}

func readUserFile(t *testing.T, path string) Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Audit #15: saving must not copy the machine-wide settings into the user file.
func TestSaveDoesNotFreezeSystemSettings(t *testing.T) {
	system := &SystemConfig{Config: Config{
		DefaultServerURL: ptr("https://old.example.com"),
		MTU:              ptr(1400),
		DNSOverride:      ptr(false),
		AuthPath:         ptr("/auth"),
		Orgs:             map[string]OrgConfig{"o1": {MTU: ptr(1300)}},
	}}
	cm, sys, path := newTestManager(t, system, "")

	if !cm.MarkOnboardingStarted() || !cm.SetOnboardingSeenWelcome(true) {
		t.Fatal("save failed")
	}
	if !cm.UpdateOrgConfig("o1", OrgConfig{DNSTunnel: ptr(true)}) {
		t.Fatal("org save failed")
	}

	got := readUserFile(t, path)
	if got.DefaultServerURL != nil || got.MTU != nil || got.DNSOverride != nil || got.AuthPath != nil {
		t.Fatalf("system values frozen into user file: %+v", got)
	}
	if got.OnboardingSeenWelcome == nil || !*got.OnboardingSeenWelcome {
		t.Fatalf("user value lost: %+v", got)
	}
	if o := got.Orgs["o1"]; o.MTU != nil || o.DNSTunnel == nil || !*o.DNSTunnel {
		t.Fatalf("org layer wrong: %+v", got.Orgs)
	}

	// The admin changes the system file later; the user must follow.
	(*sys).DefaultServerURL = ptr("https://new.example.com")
	(*sys).MTU = ptr(1200)
	cm.Load()
	if cm.GetDefaultServerURL() != "https://new.example.com" || cm.GetMTU() != 1200 {
		t.Fatalf("admin change not applied: url=%q mtu=%d", cm.GetDefaultServerURL(), cm.GetMTU())
	}
	if m := cm.GetOrgConfig("o1"); m.MTU == nil || *m.MTU != 1300 || m.DNSTunnel == nil {
		t.Fatalf("org merge wrong: %+v", m)
	}
}

func TestSaveKeepsUserChangesAndOverridesSystem(t *testing.T) {
	system := &SystemConfig{Config: Config{MTU: ptr(1400), PrimaryDNS: ptr("9.9.9.9")}}
	cm, _, path := newTestManager(t, system, "")

	cm.SetMTU(1350)
	cm.SetDNSTunnel(true)

	got := readUserFile(t, path)
	if got.MTU == nil || *got.MTU != 1350 || got.DNSTunnel == nil || !*got.DNSTunnel || got.PrimaryDNS != nil {
		t.Fatalf("unexpected user file: %+v", got)
	}
	if cm.GetMTU() != 1350 || cm.GetPrimaryDNS() != "9.9.9.9" {
		t.Fatalf("effective view wrong: mtu=%d dns=%q", cm.GetMTU(), cm.GetPrimaryDNS())
	}

	// Setting the value back to the system value drops the user override.
	cm.SetMTU(1400)
	if got := readUserFile(t, path); got.MTU != nil {
		t.Fatalf("override equal to system value kept: %+v", got)
	}
}

// Files written by earlier versions hold copies of the system values; the next
// save drops the ones that still equal the current system value.
func TestSavePrunesStaleSystemCopies(t *testing.T) {
	system := &SystemConfig{Config: Config{DefaultServerURL: ptr("https://a.example.com")}}
	cm, _, path := newTestManager(t, system, `{"defaultServerURL":"https://a.example.com","mtu":1111}`)

	cm.SetOnboardingSeenWelcome(true)
	got := readUserFile(t, path)
	if got.DefaultServerURL != nil {
		t.Fatalf("stale copy not pruned: %+v", got)
	}
	if got.MTU == nil || *got.MTU != 1111 {
		t.Fatalf("user value lost: %+v", got)
	}
}

func TestBulkSaveFromSettingsForm(t *testing.T) {
	system := &SystemConfig{Config: Config{DefaultServerURL: ptr("https://a.example.com")}}
	cm, _, path := newTestManager(t, system, "")

	cfg := cm.GetConfigCopy() // as ui/settings.go does
	cfg.AutoConnectAtLogin = ptr(true)
	if !cm.Save(cfg) {
		t.Fatal("save failed")
	}
	got := readUserFile(t, path)
	if got.DefaultServerURL != nil || got.AutoConnectAtLogin == nil || !*got.AutoConnectAtLogin {
		t.Fatalf("unexpected user file: %+v", got)
	}
}

func TestClearEmptiesUserFileOnly(t *testing.T) {
	system := &SystemConfig{Config: Config{MTU: ptr(1400)}}
	cm, _, path := newTestManager(t, system, `{"dnsTunnel":true}`)
	if !cm.Clear() {
		t.Fatal("clear failed")
	}
	got := readUserFile(t, path)
	if got.DNSTunnel != nil || cm.GetMTU() != 1400 {
		t.Fatalf("clear wrong: file=%+v mtu=%d", got, cm.GetMTU())
	}
}

// Audit #16: the admin lock cannot be undone from the user file.
func TestSystemLockWins(t *testing.T) {
	system := &SystemConfig{Config: Config{
		UserSettingsDisabled: ptr(true),
		DefaultServerURL:     ptr("https://corp.example.com"),
		MTU:                  ptr(1280),
		Orgs:                 map[string]OrgConfig{"o1": {MTU: ptr(1300)}},
	}}
	userJSON := `{"userSettingsDisabled":false,"defaultServerURL":"https://evil.example.com",
		"mtu":9000,"dnsTunnel":true,"orgs":{"o1":{"mtu":9000,"dnsOverride":false}}}`
	cm, _, _ := newTestManager(t, system, userJSON)

	if !cm.GetUserSettingsDisabled() {
		t.Fatal("user file unlocked the settings")
	}
	if cm.GetDefaultServerURL() != "https://corp.example.com" || cm.GetMTU() != 1280 {
		t.Fatalf("locked values overridden: url=%q mtu=%d", cm.GetDefaultServerURL(), cm.GetMTU())
	}
	if o := cm.GetOrgConfig("o1"); o.MTU == nil || *o.MTU != 1300 {
		t.Fatalf("locked org value overridden: %+v", o)
	}
	// Values the admin left unmanaged still come from the user.
	if !cm.GetDNSTunnel() {
		t.Fatal("unmanaged user value ignored while locked")
	}
	if o := cm.GetOrgConfig("o1"); o.DNSOverride == nil || *o.DNSOverride {
		t.Fatalf("unmanaged org value ignored while locked: %+v", o)
	}

	// Saving while locked must not unlock it either.
	cm.SetMTU(1500)
	if !cm.GetUserSettingsDisabled() || cm.GetMTU() != 1280 {
		t.Fatalf("lock bypassed through save: locked=%v mtu=%d", cm.GetUserSettingsDisabled(), cm.GetMTU())
	}
}

func TestSystemUnlockedKeepsUserOverrides(t *testing.T) {
	system := &SystemConfig{Config: Config{UserSettingsDisabled: ptr(false), MTU: ptr(1280)}}
	cm, _, _ := newTestManager(t, system, `{"mtu":1350}`)
	if cm.GetUserSettingsDisabled() || cm.GetMTU() != 1350 {
		t.Fatalf("unexpected: locked=%v mtu=%d", cm.GetUserSettingsDisabled(), cm.GetMTU())
	}
}

// The lock is admin policy: a copy in the user file (frozen there by an older
// version, or typed by the user) never counts, so removing it from the system
// config really unlocks.
func TestUserFileCannotSetLock(t *testing.T) {
	cm, _, path := newTestManager(t, &SystemConfig{}, `{"userSettingsDisabled":true}`)
	if cm.GetUserSettingsDisabled() {
		t.Fatal("user file locked the settings")
	}
	cm.SetOnboardingSeenWelcome(true)
	if got := readUserFile(t, path); got.UserSettingsDisabled != nil {
		t.Fatalf("stale lock kept in the user file: %+v", got)
	}
}

func TestSystemConfigReadErrorKeepsLastGood(t *testing.T) {
	system := &SystemConfig{Config: Config{UserSettingsDisabled: ptr(true), MTU: ptr(1280)}}
	cm, sys, _ := newTestManager(t, system, "")
	if !cm.GetUserSettingsDisabled() {
		t.Fatal("lock not applied")
	}

	// The admin tool is rewriting the file: the read fails (or yields nothing).
	systemReadFails = true
	defer func() { systemReadFails = false }()
	*sys = &SystemConfig{}
	cm.SetOnboardingSeenWelcome(true)
	if !cm.GetUserSettingsDisabled() || cm.GetMTU() != 1280 {
		t.Fatalf("admin policy dropped on a failed read: locked=%v mtu=%d", cm.GetUserSettingsDisabled(), cm.GetMTU())
	}
}

func TestMergeWithSystemHelpersUsedByDaemon(t *testing.T) {
	system := &Config{UserSettingsDisabled: ptr(true), AutoDownloadUpdatesEnabled: ptr(false)}
	user := &Config{AutoDownloadUpdatesEnabled: ptr(true)}
	if autoDownloadUpdates(mergeWithSystem(system, user)) {
		t.Fatal("user file overrode the locked auto-download setting")
	}
}
