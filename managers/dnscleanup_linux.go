package managers

import (
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
	dnsOverride "github.com/fosrl/olm/dns/override"
)

// tunnelInterfaceName is the WireGuard interface the tunnel creates, and so
// the name olm's DNS override state is filed under (see tunnel.buildConfig).
const tunnelInterfaceName = "Pangolin"

// staleDNSCleanupWait bounds how long a tunnel start waits for the cleanup.
const staleDNSCleanupWait = 15 * time.Second

var (
	staleDNSCleanupDone     = make(chan struct{})
	staleDNSCleanupDoneOnce sync.Once
)

// cleanupStaleDNS removes DNS settings an earlier tunnel left behind when it
// was killed (SIGKILL after a stop timeout, OOM, a power loss): olm's
// NetworkManager drop-in, resolvconf entry or /etc/resolv.conf rewrite, which
// all outlive a reboot. At daemon start no tunnel of ours is running (they die
// with the daemon), so nothing live is touched. It is a no-op when there is
// nothing stale.
func cleanupStaleDNS(cleanup func(interfaceName string) error) {
	defer staleDNSCleanupDoneOnce.Do(func() { close(staleDNSCleanupDone) })
	if err := cleanup(tunnelInterfaceName); err != nil {
		logger.Warn("Daemon: stale DNS cleanup: %v", err)
	}
}

// startStaleDNSCleanup runs the cleanup without delaying the daemon's socket.
func startStaleDNSCleanup() {
	go cleanupStaleDNS(dnsOverride.CleanupStaleState)
}

// waitForStaleDNSCleanup makes a tunnel start wait until the cleanup is done,
// so it can't undo a new tunnel's DNS override.
func waitForStaleDNSCleanup() {
	select {
	case <-staleDNSCleanupDone:
	case <-time.After(staleDNSCleanupWait):
		logger.Warn("Daemon: stale DNS cleanup still running; starting the tunnel anyway")
	}
}
