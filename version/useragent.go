package version

import (
	"fmt"
	"runtime"
)

func Arch() string {
	switch runtime.GOARCH {
	case "arm", "arm64", "amd64":
		return runtime.GOARCH
	case "386":
		return "x86"
	default:
		// Source builds for riscv64, ppc64le and so on must not crash the daemon.
		return runtime.GOARCH
	}
}

func UserAgent() string {
	// pangolin-windows-<version> on Windows, pangolin-linux-<version> on Linux.
	return fmt.Sprintf("pangolin-%s-%s", runtime.GOOS, Number)
}
