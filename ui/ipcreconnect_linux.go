package ui

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/managers"
)

// The Linux UI outlives the daemon: the daemon restarts on a package upgrade
// or after a crash, and may start after the UI at login. Instead of quitting
// or staying attached to nothing, the UI keeps trying to attach, with backoff,
// and then restores what the daemon forgot.

const (
	ipcReconnectInitialDelay = time.Second
	ipcReconnectMaxDelay     = 30 * time.Second

	// otherUIConfirmations is how many attaches in a row must be refused as
	// "already running" before the UI believes another one took over.
	otherUIConfirmations = 3
)

var (
	ipcReconnectMu  sync.Mutex
	ipcReconnecting bool
	ipcEverAttached atomic.Bool
)

// handleManagerStopping runs when the daemon announces it is stopping, for a
// restart or an upgrade. The UI stays: it drops the connection now (the pipes
// close right after anyway, but until then RPCs would reach a stopping
// daemon) and reconnects when the daemon is back.
func handleManagerStopping() {
	logger.Info("Manager service is stopping; waiting for it to come back")
	managers.IPCClientDropConnection()
}

// canStartConnectAtLaunch reports whether the start-up connect step has the
// manager connection it needs; if not, it runs after the reconnect.
func canStartConnectAtLaunch() bool { return managers.IPCClientReady() }

// setupIPCReconnect starts reconnecting whenever the daemon connection drops.
func setupIPCReconnect() {
	managers.IPCClientRegisterConnectionLost(startIPCReconnect)
}

// reconnectIPCIfDown starts reconnecting when the UI started without a
// daemon connection.
func reconnectIPCIfDown() {
	if managers.IPCClientReady() {
		ipcEverAttached.Store(true)
		return
	}
	startIPCReconnect()
}

// alwaysOnMarker records that Always-On is set, in the user's runtime
// directory, so that it outlives this UI process and the daemon's memory (a
// restarted daemon and a re-executed or relaunched UI both start without it)
// but not the user's session, as on Windows.
func alwaysOnMarker() string { return managers.UserRuntimeFile("pangolin-client.alwayson") }

// persistAlwaysOn saves the Always-On setting to the marker.
func persistAlwaysOn(enabled bool) {
	path := alwaysOnMarker()
	if path == "" {
		return
	}
	var err error
	if enabled {
		err = os.WriteFile(path, nil, 0o600)
	} else if err = os.Remove(path); os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		logger.Warn("Always-On: could not update %s: %v", path, err)
	}
}

// alwaysOnPersisted reports whether the marker says Always-On was set.
func alwaysOnPersisted() bool {
	path := alwaysOnMarker()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func startIPCReconnect() {
	ipcReconnectMu.Lock()
	if ipcReconnecting {
		ipcReconnectMu.Unlock()
		return
	}
	ipcReconnecting = true
	ipcReconnectMu.Unlock()
	go func() {
		defer func() {
			ipcReconnectMu.Lock()
			ipcReconnecting = false
			ipcReconnectMu.Unlock()
		}()
		reconnectIPC()
	}()
}

// ipcReconnectDelay is the wait before attempt number attempt (from 0).
func ipcReconnectDelay(attempt int) time.Duration {
	delay := ipcReconnectInitialDelay
	for i := 0; i < attempt && delay < ipcReconnectMaxDelay; i++ {
		delay *= 2
	}
	if delay > ipcReconnectMaxDelay {
		delay = ipcReconnectMaxDelay
	}
	return delay
}

type reconnectOutcome int

const (
	// reconnectAttached means the pipes are connected to the daemon.
	reconnectAttached reconnectOutcome = iota
	// reconnectOtherUI means another UI for this user attached first.
	reconnectOtherUI
	// reconnectReplaced means this binary was replaced; it should re-execute.
	reconnectReplaced
	// reconnectGone means this binary was removed, so there is nothing to
	// reconnect to or restart as.
	reconnectGone
)

// attachFunc is managers.AttachUI.
type attachFunc func() (reader, writer, events *os.File, err error)

// reconnectLoop attaches to the daemon, trying again with backoff until it
// succeeds, another UI is running, or this binary stops being the installed
// one. A package upgrade replaces the binary before it restarts the daemon,
// so a replaced binary means the new version should take over.
func reconnectLoop(attach attachFunc, binary func() binaryState, sleep func(time.Duration)) (outcome reconnectOutcome, reader, writer, events *os.File) {
	alreadyRunning := 0
	for attempt := 0; ; attempt++ {
		switch binary() {
		case binaryReplaced:
			return reconnectReplaced, nil, nil, nil
		case binaryRemoved:
			return reconnectGone, nil, nil, nil
		}
		r, w, e, err := attach()
		switch {
		case err == nil:
			return reconnectAttached, r, w, e
		case errors.Is(err, managers.ErrUIAlreadyRunning):
			// The daemon may not have noticed yet that this UI's old
			// connection is gone; a UI that is really another one stays.
			if alreadyRunning++; alreadyRunning >= otherUIConfirmations {
				return reconnectOtherUI, nil, nil, nil
			}
		}
		// An unreachable daemon or one installing an update: keep trying.
		if attempt == 0 || attempt%10 == 0 {
			logger.Info("Waiting for the Pangolin daemon: %v", err)
		}
		sleep(ipcReconnectDelay(attempt))
	}
}

func reconnectIPC() {
	outcome, reader, writer, events := reconnectLoop(managers.AttachUI, currentBinaryState, time.Sleep)
	switch outcome {
	case reconnectOtherUI:
		logger.Info("Another Pangolin UI is running for this user; quitting this one")
		app.Quit()
	case reconnectReplaced:
		logger.Info("Pangolin was updated; restarting the UI")
		reexecUI()
	case reconnectGone:
		logger.Info("Pangolin was uninstalled; quitting the UI")
		app.Quit()
	case reconnectAttached:
		managers.InitializeIPCClient(reader, writer, events)
		logger.Info("Reconnected to the Pangolin daemon")
		onIPCReattached(ipcEverAttached.Swap(true))
	}
}

// onIPCReattached restores state after the connection came back. A daemon
// that restarted knows nothing of this UI, so Always-On is told again. When
// the UI started before the daemon was reachable, the start-up connect steps
// that needed it run now.
func onIPCReattached(wasAttachedBefore bool) {
	if wasAttachedBefore {
		if alwaysOnEnabled() {
			if err := managers.IPCClientSetAlwaysOn(true); err != nil {
				logger.Error("Failed to tell the manager Always-On is on: %v", err)
			}
		}
		publish()
		return
	}
	// Start-up loaded the account without the manager's secret store, so the
	// session token wasn't found: load it again.
	if authManager != nil {
		if err := authManager.Initialize(); err != nil {
			logger.Error("Failed to reload the account after attaching: %v", err)
		}
	}
	refreshCLIInstallState()
	go checkStartupUpdate()
	if !onboardingNeeded() {
		startConnectAtLaunch()
	}
	publish()
}

// waitForIPC blocks until the daemon connection is back or keepWaiting says
// to stop. It reports whether the connection is up.
func waitForIPC(keepWaiting func() bool) bool {
	for !managers.IPCClientReady() {
		if !keepWaiting() {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return true
}

// binaryState says whether the running executable is still the installed one.
type binaryState int

const (
	binaryCurrent binaryState = iota
	binaryReplaced
	binaryRemoved
)

func currentBinaryState() binaryState {
	link, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return binaryCurrent
	}
	return binaryStateOf(link, func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// binaryStateOf interprets the target of /proc/self/exe. The kernel appends
// " (deleted)" when the file was unlinked, which is what a package upgrade
// does to the old binary; if a file exists again at the path, it is the new
// version.
func binaryStateOf(procExeTarget string, exists func(path string) bool) binaryState {
	path, deleted := strings.CutSuffix(procExeTarget, " (deleted)")
	if !deleted {
		return binaryCurrent
	}
	if exists(path) {
		return binaryReplaced
	}
	return binaryRemoved
}

// reexecUI starts the installed binary in place of this process.
func reexecUI() {
	link, err := os.Readlink("/proc/self/exe")
	if err != nil {
		app.Quit()
		return
	}
	exe := strings.TrimSuffix(link, " (deleted)")
	// --autostart is dropped: the user is signed in and the UI was running.
	// Always-On is carried by its marker file.
	if err := syscall.Exec(exe, []string{exe}, os.Environ()); err != nil {
		logger.Error("Failed to restart the UI after the update: %v", err)
		app.Quit()
	}
}

// startupWarningGrace is how long the daemon gets to appear before the UI
// warns that it is not running.
const startupWarningGrace = 20 * time.Second

// startupWarningStillNeeded waits up to the grace period for the connection
// the reconnect loop is making, and reports whether it is still missing.
func startupWarningStillNeeded() bool {
	deadline := time.Now().Add(startupWarningGrace)
	for time.Now().Before(deadline) {
		if managers.IPCClientReady() {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return !managers.IPCClientReady()
}
