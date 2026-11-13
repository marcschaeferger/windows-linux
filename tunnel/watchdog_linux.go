package tunnel

import (
	"os"
	"strings"
)

// dnsWatchdogSubcommand is the argument that makes pangolin-client run the
// olm DNS watchdog (see main_linux.go). olm adds --parent-pid, --interface
// and --socket.
const dnsWatchdogSubcommand = "--dns-watchdog"

// watchdogSettings returns what olm needs to start its DNS watchdog: a
// separate process that resets the system DNS if the tunnel process dies
// without restoring it (SIGKILL, OOM). Without them olm runs no watchdog.
func watchdogSettings() (executable string, subcommand []string) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	// After a package upgrade the kernel reports the replaced binary with a
	// " (deleted)" suffix; the path now holds the new version.
	return strings.TrimSuffix(exe, " (deleted)"), []string{dnsWatchdogSubcommand}
}
