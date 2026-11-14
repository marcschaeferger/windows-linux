package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fosrl/newt/logger"
)

func TestIsLogOwner(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"pangolin-client", "--daemon"}, true},
		{[]string{`C:\Program Files\Pangolin\Pangolin.exe`, "/managerservice"}, true},
		// Tunnel processes and UIs share the file but never rotate it.
		{[]string{"pangolin-client", "--tunnel", "/run/pangolin-client/tunnels/x.json"}, false},
		{[]string{`Pangolin.exe`, "/tunnelservice", `C:\cfg.json`}, false},
		{[]string{"pangolin-client"}, false},
		{[]string{"pangolin-client", "--autostart"}, false},
		{[]string{`Pangolin.exe`, "/ui", "3", "4", "5"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := isLogOwner(tt.args); got != tt.want {
			t.Errorf("isLogOwner(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

// TestSetupLoggingWritesRedactedPrivateFile runs the real setup for a UI
// process (not root, so it logs under XDG_STATE_HOME). It is the only test in
// this package that sets up the process-wide logger.
func TestSetupLoggingWritesRedactedPrivateFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the log goes to /var/log")
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	setupLogging()

	logger.Info("config: {Secret:olm-secret-VALUE-12345 UserToken:tok-VALUE-98765}")

	dir := filepath.Join(state, "pangolin-client", "logs")
	data, err := os.ReadFile(filepath.Join(dir, "pangolin.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "VALUE") || !strings.Contains(string(data), "Secret:[REDACTED]") {
		t.Errorf("log not redacted:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "pangolin.log"))
		di, _ := os.Stat(dir)
		if fi.Mode().Perm()&0o077 != 0 || di.Mode().Perm()&0o077 != 0 {
			t.Errorf("log files must be private, got file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
}
