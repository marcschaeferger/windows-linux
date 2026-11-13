package ui

import "github.com/fosrl/newt/logger"

// The Windows manager service starts and stops the UI itself, so the UI does
// not reconnect: it quits when the service stops.

func handleManagerStopping() {
	logger.Info("Manager service is stopping, exiting UI")
	app.Quit()
}

func setupIPCReconnect() {}

func reconnectIPCIfDown() {}

func persistAlwaysOn(bool) {}

func alwaysOnPersisted() bool { return false }

func canStartConnectAtLaunch() bool { return true }

func waitForIPC(keepWaiting func() bool) bool { return true }

func startupWarningStillNeeded() bool { return true }
