package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/logging"

	"github.com/fosrl/newt/logger"
)

// stringToLogLevel converts a string log level to logger.LogLevel.
// Returns INFO as default if the string doesn't match any known level.
func stringToLogLevel(levelStr string) logger.LogLevel {
	switch strings.ToLower(levelStr) {
	case "debug":
		return logger.DEBUG
	case "info":
		return logger.INFO
	case "warn":
		return logger.WARN
	case "error":
		return logger.ERROR
	case "fatal":
		return logger.FATAL
	default:
		return logger.INFO
	}
}

// isLogOwner reports whether this process is the one that rotates the shared
// log: the Linux daemon or the Windows manager service. Tunnel processes and
// UIs append to the same file but never rename it, so the daemon's open file
// is never rotated out from under it.
func isLogOwner(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return args[1] == "--daemon" || args[1] == "/managerservice"
}

// setupLogging initializes the logger: every line is redacted, then written to
// the log file, which the owner process rotates by size. If the shared log
// cannot be opened, for example by a standard user's UI on Windows where
// ProgramData is read-only, the process logs to a file under its own
// user data directory instead.
func setupLogging() {
	// The logger must be ours before anything logs, so the writer comes first.
	// Until a file is open, lines go to stdout.
	writer := logging.NewWriter(nil)
	installed := logging.Install(writer)

	logInstance := logger.GetLogger()
	logLevelStr := config.GetSystemLogLevel()
	logInstance.SetLevel(stringToLogLevel(logLevelStr))
	if !installed {
		logger.Warn("Logger was initialized before setupLogging; log lines are not redacted")
	}

	owner := isLogOwner(os.Args)
	file, logFile, err := openLogFile(config.GetLogDir(), owner)
	if err != nil {
		logger.Error("Failed to open log file in %s: %v", config.GetLogDir(), err)
		fallbackDir := filepath.Join(config.UserDataDir(), "logs")
		var fbErr error
		file, logFile, fbErr = openLogFile(fallbackDir, owner)
		if fbErr != nil {
			logger.Error("Failed to open log file in %s: %v", fallbackDir, fbErr)
			return
		}
	}

	if installed {
		writer.SetOutput(file)
	} else if plain, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		// Redaction is not available; write the way the logger always did.
		logInstance.SetOutput(plain)
	}

	logger.Info("Pangolin logging initialized - log file: %s, log level: %s", logFile, logLevelStr)
}

// openLogFile creates dir with restrictive permissions and opens pangolin.log in it.
func openLogFile(dir string, owner bool) (*logging.RotatingFile, string, error) {
	dirMode, fileMode := logModes()
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, "", err
	}
	if owner {
		logging.RestrictDir(dir, dirMode)
	}
	path := filepath.Join(dir, "pangolin.log")
	file, err := logging.OpenRotatingFile(path, logging.FileOptions{Owner: owner, FileMode: fileMode})
	return file, path, err
}

// logModes returns the directory and file modes for log files. The root
// daemon's log lists every user's accounts, orgs and tunnel activity, so only
// root can read it; the Logs tab reads it through the daemon. A user's own
// logs are private to that user. Windows ignores the modes and relies on ACLs.
func logModes() (dir, file os.FileMode) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return 0o750, 0o640
	}
	return 0o700, 0o600
}
