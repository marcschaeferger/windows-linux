package main

import (
	"testing"
	"time"
)

func TestWatchdogConfig(t *testing.T) {
	cfg, err := watchdogConfig([]string{"--parent-pid=4242", "--interface=Pangolin", "--socket=/run/pangolin-client/olm.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ParentPID != 4242 || cfg.InterfaceName != "Pangolin" || cfg.SocketPath != "/run/pangolin-client/olm.sock" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.CheckInterval != 5*time.Second || cfg.FailureThreshold != 3 {
		t.Errorf("defaults: %+v", cfg)
	}
	if _, err := watchdogConfig([]string{"--interface=Pangolin"}); err == nil {
		t.Error("accepted a missing --parent-pid")
	}
}
