package ui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/config"
)

// withConfig points the UI at a ConfigManager whose user config file holds
// fileJSON, in a temporary config directory.
func withConfig(t *testing.T, fileJSON string) {
	t.Helper()
	dir := t.TempDir()
	// Linux and Windows config locations, so a real config is never touched.
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("APPDATA", dir)
	if fileJSON != "" {
		path := filepath.Join(config.UserDataDir(), config.ConfigFileName)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fileJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := configManager
	configManager = config.NewConfigManager()
	t.Cleanup(func() { configManager = old })
}

// Audit #97: a switch that doesn't touch MTU or DNS must save even when the
// MTU in effect is outside the range the MTU sheet accepts (hand-edited
// config, or set by the org's server).
func TestApplySettingsIgnoresUntouchedInvalidMTU(t *testing.T) {
	withConfig(t, `{"mtu": 100}`)
	form := currentSettings()
	if form.MTU != "100" {
		t.Fatalf("effective MTU = %q, want 100 from the config file", form.MTU)
	}
	form.AutoConnect = true

	result := applySettings(form, "global")
	if result.Error != "" {
		t.Fatalf("toggle refused: %s (field %q)", result.Error, result.Field)
	}
	if !configManager.GetAutoConnectAtLogin() {
		t.Fatal("connect at start was not saved")
	}
}

// A changed field is still checked.
func TestApplySettingsValidatesChangedFields(t *testing.T) {
	withConfig(t, "")
	for _, tt := range []struct {
		field string
		edit  func(*Settings)
	}{
		{"mtu", func(s *Settings) { s.MTU = "100000" }},
		{"primaryDns", func(s *Settings) { s.PrimaryDNS = "not-an-ip" }},
		{"secondaryDns", func(s *Settings) { s.SecondaryDNS = "1.2.3" }},
	} {
		form := currentSettings()
		tt.edit(&form)
		if result := applySettings(form, "global"); result.Field != tt.field || result.Error == "" {
			t.Errorf("%s: got field %q error %q, want a %s error", tt.field, result.Field, result.Error, tt.field)
		}
	}

	// An IPv6 resolver is valid (the tunnel writes it as [addr]:53).
	form := currentSettings()
	form.PrimaryDNS = "2606:4700::1111"
	if result := applySettings(form, "global"); result.Error != "" {
		t.Fatalf("IPv6 DNS refused: %s", result.Error)
	}
	if got := currentSettings().PrimaryDNS; got != "2606:4700::1111" {
		t.Fatalf("primary DNS = %q after save", got)
	}
}

// Audit #45: a form shown for another org than the current one (the org or
// account was switched while Preferences was open) is not saved, and Reset
// doesn't reset the org that is current now.
func TestApplySettingsRefusesFormOfAnotherOrg(t *testing.T) {
	withConfig(t, "")
	form := currentSettings()
	form.OrgID = "org-a" // shown for org A; no org is current now
	form.DNSOverride = !form.DNSOverride
	form.AutoConnect = true

	result := applySettings(form, settingsScopeOrg)
	if result.Error == "" || result.Field != "" {
		t.Fatalf("stale form accepted: field %q error %q", result.Field, result.Error)
	}
	if result.Settings.OrgID != "" {
		t.Fatalf("result shows org %q, want the current org", result.Settings.OrgID)
	}
	if configManager.GetAutoConnectAtLogin() || currentSettings().DNSOverride == form.DNSOverride {
		t.Fatal("a stale form was saved")
	}
}

// Audit #45: "Use Global Settings for A" shown for org A must not reset org
// B, which became current after the window was filled in.
func TestResetOrgSettingsRefusesAnotherOrg(t *testing.T) {
	withConfig(t, "")
	old := currentOrgInfo
	currentOrgInfo = func() (string, string) { return "org-b", "B" }
	t.Cleanup(func() { currentOrgInfo = old })
	mtu := 1400
	if !configManager.UpdateOrgConfig("org-b", config.OrgConfig{MTU: &mtu}) {
		t.Fatal("could not seed org B's settings")
	}

	if got := resetOrgSettings("org-a"); got.OrgID != "org-b" || !got.OrgMode {
		t.Fatalf("reset for org A changed org B: %+v", got)
	}
	if configManager.GetOrgConfig("org-b").IsEmpty() {
		t.Fatal("org B's settings were reset by a form for org A")
	}
	if got := resetOrgSettings("org-b"); got.OrgMode {
		t.Fatal("org B's own reset did not reset it")
	}
}

// Audit #45: with org B current, a form filled in for org A is refused even
// though both orgs exist.
func TestApplySettingsRefusesFormOfPreviousOrg(t *testing.T) {
	withConfig(t, "")
	old := currentOrgInfo
	currentOrgInfo = func() (string, string) { return "org-a", "A" }
	t.Cleanup(func() { currentOrgInfo = old })
	form := currentSettings()
	currentOrgInfo = func() (string, string) { return "org-b", "B" }

	form.MTU = "1400"
	if result := applySettings(form, settingsScopeOrg); result.Error == "" || result.Settings.OrgID != "org-b" {
		t.Fatalf("form for org A accepted while org B is current: %+v", result)
	}
	if !configManager.GetOrgConfig("org-b").IsEmpty() {
		t.Fatal("org A's form was saved into org B")
	}
}

// Audit #71: while the tunnel is really reconnecting, the org items stay
// disabled even though the debounced menu still shows it as connected.
func TestOrgItemsDisabledDuringHeldBackDrop(t *testing.T) {
	in := menuInputs{
		Authenticated:  true,
		Orgs:           []menuOrg{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}},
		CurrentOrgID:   "a",
		CurrentOrgName: "A",
		TunnelPhase:    phaseRunning,
		RawTunnelPhase: phaseStarting,
	}
	orgs := 0
	for _, it := range orgSubmenu(in).Items {
		if strings.HasPrefix(it.ID, menuPrefixOrg) {
			orgs++
			if it.Enabled {
				t.Fatalf("org item %q enabled while the tunnel reconnects", it.ID)
			}
		}
	}
	if orgs != 2 {
		t.Fatalf("found %d org items, want 2", orgs)
	}
	in.RawTunnelPhase = phaseRunning
	for _, it := range orgSubmenu(in).Items {
		if strings.HasPrefix(it.ID, menuPrefixOrg) && !it.Enabled {
			t.Fatalf("org item %q disabled while connected", it.ID)
		}
	}
}

// Audit #100: a snapshot built before a newer one was sent is dropped.
func TestEmitIfNewestDropsOlderSnapshots(t *testing.T) {
	publishMu.Lock()
	saved := lastPublished
	lastPublished = 0
	publishMu.Unlock()
	t.Cleanup(func() {
		publishMu.Lock()
		lastPublished = saved
		publishMu.Unlock()
	})

	var sent []uint64
	for _, seq := range []uint64{1, 3, 2, 4} {
		emitIfNewest(seq, func() { sent = append(sent, seq) })
	}
	if want := []uint64{1, 3, 4}; len(sent) != len(want) || sent[0] != 1 || sent[1] != 3 || sent[2] != 4 {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// Audit #100: two switches started at once (tray and Accounts tab) can't both
// begin.
func TestBeginSwitchIsExclusive(t *testing.T) {
	t.Cleanup(func() {
		stateMu.Lock()
		switchingAccountID, switchingOrgID = "", ""
		stateMu.Unlock()
	})
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			flag := &switchingAccountID
			if i%2 == 1 {
				flag = &switchingOrgID
			}
			if beginSwitch(flag, "x") {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	if n := won.Load(); n != 1 {
		t.Fatalf("%d switches began, want 1", n)
	}
}

// Audit #73: a click-timeout that already fired must not clear the pending
// state of a newer click.
func TestStalePendingTimeoutKeepsNewerClick(t *testing.T) {
	t.Cleanup(func() { setPendingTunnel(nil) })
	on := true
	setPendingTunnel(&on)

	// The old timeout fires and waits for stateMu while the newer click
	// (what setPendingTunnel does) holds it.
	stateMu.Lock()
	timer := pendingTunnelTimer
	timer.Reset(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	off := false
	pendingTunnelOn = &off
	pendingTunnelGen++
	pendingTunnelTimer = nil
	stateMu.Unlock()

	time.Sleep(50 * time.Millisecond)
	stateMu.RLock()
	p := pendingTunnelOn
	stateMu.RUnlock()
	if p == nil || *p != false {
		t.Fatal("the stale timeout cleared the newer click")
	}
}
