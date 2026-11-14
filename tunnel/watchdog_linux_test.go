package tunnel

import "testing"

func TestWatchdogSettings(t *testing.T) {
	exe, sub := watchdogSettings()
	if exe == "" {
		t.Fatal("no executable for the DNS watchdog")
	}
	if len(sub) != 1 || sub[0] != "--dns-watchdog" {
		t.Fatalf("subcommand = %v, want [--dns-watchdog]", sub)
	}
}
