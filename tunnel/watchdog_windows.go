package tunnel

// watchdogSettings: the Windows DNS override is tied to the interface and is
// reclaimed when the interface goes away, so there is no watchdog.
func watchdogSettings() (executable string, subcommand []string) { return "", nil }
