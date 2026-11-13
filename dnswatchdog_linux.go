package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	dnsOverride "github.com/fosrl/olm/dns/override"
)

// watchdogConfig parses the arguments olm gives the DNS watchdog process
// (dns/override SpawnWatchdog): --parent-pid, --interface, --socket.
func watchdogConfig(args []string) (dnsOverride.WatchdogConfig, error) {
	fs := flag.NewFlagSet("dns-watchdog", flag.ContinueOnError)
	parentPID := fs.Int("parent-pid", 0, "PID of the tunnel process to watch")
	socket := fs.String("socket", "", "olm API socket of the tunnel process")
	iface := fs.String("interface", "", "WireGuard interface name")
	interval := fs.Duration("interval", 5*time.Second, "liveness check interval")
	threshold := fs.Int("threshold", 3, "failed checks before DNS is reset")
	if err := fs.Parse(args); err != nil {
		return dnsOverride.WatchdogConfig{}, err
	}
	if *parentPID <= 0 {
		return dnsOverride.WatchdogConfig{}, errors.New("--parent-pid is required")
	}
	return dnsOverride.WatchdogConfig{
		ParentPID:        *parentPID,
		SocketPath:       *socket,
		InterfaceName:    *iface,
		CheckInterval:    *interval,
		FailureThreshold: *threshold,
	}, nil
}

// runDNSWatchdog runs `pangolin-client --dns-watchdog`: it watches the tunnel
// process that overrode the system DNS and resets the DNS if that process
// dies before restoring it.
func runDNSWatchdog(args []string) error {
	cfg, err := watchdogConfig(args)
	if err != nil {
		return fmt.Errorf("dns watchdog: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := dnsOverride.RunWatchdog(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
