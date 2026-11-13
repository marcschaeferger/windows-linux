package managers

import (
	"sync"
	"time"
	_ "unsafe"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/services"
	"github.com/fosrl/windows-linux/updater"
)

//go:linkname fastrandn runtime.fastrandn
func fastrandn(n uint32) uint32

type UpdateState uint32

const (
	UpdateStateUnknown UpdateState = iota
	UpdateStateFoundUpdate
	UpdateStateUpdatesDisabledUnofficialBuild
	// UpdateStateUpdateDownloaded means the update was downloaded silently and will
	// be installed the next time Pangolin quits or launches, for users with
	// autoDownloadUpdatesEnabled.
	UpdateStateUpdateDownloaded
	// UpdateStateInstallReminder means a downloaded update has waited long enough
	// that the user should be prompted to install it.
	UpdateStateInstallReminder
)

// HasUpdate reports whether an update is available to install.
func (s UpdateState) HasUpdate() bool {
	return s == UpdateStateFoundUpdate || s == UpdateStateUpdateDownloaded || s == UpdateStateInstallReminder
}

// stagedUpdateReminderAge is how long a downloaded update waits for the user to
// quit before they are prompted to install it, like Sparkle.
const stagedUpdateReminderAge = 7 * 24 * time.Hour

var (
	updateLock   sync.Mutex
	updateState  = UpdateStateUnknown
	foundVersion string                // version of the found update, when updateState.HasUpdate()
	stagedUpdate *updater.StagedUpdate // silently downloaded update waiting to be installed

	installingStagedUpdate bool     // a background install started by installStagedUpdate is running
	installedVersion       string   // an update that installed without this process being replaced, until it restarts
	pendingLaunchSessions  []uint32 // UI launches held back by that install
)

func jitterSleep(min, max time.Duration) {
	time.Sleep(min + time.Millisecond*time.Duration(fastrandn(uint32((max-min+1)/time.Millisecond))))
}

func currentUpdateState() (UpdateState, string) {
	updateLock.Lock()
	defer updateLock.Unlock()
	return updateState, foundVersion
}

// checkForUpdate is updater.CheckForUpdate, except that an update which is
// already installed but isn't running yet (the package didn't restart this
// process) is not offered or downloaded again.
func checkForUpdate() (*updater.UpdateFound, error) {
	update, err := updater.CheckForUpdate()
	if err != nil || update == nil {
		return update, err
	}
	updateLock.Lock()
	installed := installedVersion
	updateLock.Unlock()
	if installed != "" && update.Version() == installed {
		return nil, nil
	}
	return update, nil
}

// setFoundUpdate records that version is available and reports whether the state changed.
func setFoundUpdate(version string) (state UpdateState, changed bool) {
	updateLock.Lock()
	defer updateLock.Unlock()
	state = UpdateStateFoundUpdate
	if stagedUpdate != nil && stagedUpdate.Version() == version {
		state = UpdateStateUpdateDownloaded
		if updateState == UpdateStateInstallReminder && foundVersion == version {
			state = UpdateStateInstallReminder
		}
	}
	changed = state != updateState || version != foundVersion
	updateState, foundVersion = state, version
	return state, changed
}

// loadStagedUpdate picks up an update downloaded before the manager last stopped.
func loadStagedUpdate() {
	staged := updater.LoadStagedUpdate()
	if staged == nil {
		return
	}
	updateLock.Lock()
	stagedUpdate = staged
	updateState, foundVersion = UpdateStateUpdateDownloaded, staged.Version()
	updateLock.Unlock()
}

// stageUpdate downloads version in the background unless it is already staged.
func stageUpdate(version string) error {
	updateLock.Lock()
	alreadyStaged := installingStagedUpdate || stagedUpdate != nil && stagedUpdate.Version() == version
	updateLock.Unlock()
	if alreadyStaged {
		return nil
	}

	// A new download replaces the staged files on disk.
	staged, err := updater.DownloadInBackground()
	if err != nil {
		return err
	}
	updateLock.Lock()
	stagedUpdate = staged
	updateLock.Unlock()
	logger.Info("Update %s downloaded; it will be installed the next time Pangolin quits or launches", staged.Version())
	return nil
}

// autoDownloadUpdatesForSession reports whether the user logged into session has
// automatic update downloads on in their merged config. Session 0 means no
// particular user: the machine config or any signed-in user turning it on counts.
// On Linux a session is a UID.
func autoDownloadUpdatesForSession(session uint32) bool {
	if session == 0 {
		return autoDownloadUpdatesWanted()
	}
	enabled, err := sessionAutoDownloadUpdates(session)
	if err != nil {
		logger.Error("Automatic update downloads: %v", err)
	}
	return enabled
}

// autoDownloadUpdatesWanted reports whether updates should be downloaded in the
// background: the machine config or a signed-in user's config turns it on.
func autoDownloadUpdatesWanted() bool {
	// Linux AUR and hand-installed builds can't install updates themselves.
	if manual, _ := updater.ManualUpdate(); manual != "" {
		return false
	}
	if config.AutoDownloadUpdatesEnabled("") {
		return true
	}
	for _, session := range signedInSessions() {
		if enabled, err := sessionAutoDownloadUpdates(session); err == nil && enabled {
			return true
		}
	}
	return false
}

// autoDownloadUpdatesEnabled applies a user turning on automatic downloads in
// Preferences right away, downloading an available update now instead of at the
// next scheduled check.
func autoDownloadUpdatesEnabled() {
	if manual, _ := updater.ManualUpdate(); manual != "" || !config.AutoUpdateChecksEnabled() {
		return
	}
	go func() {
		update, err := checkForUpdate()
		if err != nil || update == nil {
			return
		}
		if err := stageUpdate(update.Version()); err != nil {
			logger.Error("Unable to download update: %v", err)
			return
		}
		if state, changed := setFoundUpdate(update.Version()); changed {
			IPCServerNotifyUpdateFound(state)
		}
	}()
}

// takeStagedUpdate returns the staged update, if there is one, handing it to the
// caller to install. restoreStagedUpdate puts it back if the install never started.
func takeStagedUpdate() *updater.StagedUpdate {
	updateLock.Lock()
	defer updateLock.Unlock()
	staged := stagedUpdate
	stagedUpdate = nil
	return staged
}

func restoreStagedUpdate(staged *updater.StagedUpdate) {
	if staged == nil {
		return
	}
	updateLock.Lock()
	defer updateLock.Unlock()
	if stagedUpdate == nil {
		stagedUpdate = staged
	}
}

// progressInterval is the least time between download progress notifications to
// the UI; a download otherwise sends one per 32 KiB.
const progressInterval = 250 * time.Millisecond

// progressThrottle thins out download progress. Errors, completion and a new
// activity always pass.
type progressThrottle struct {
	interval time.Duration
	last     time.Time
	activity string
}

func newProgressThrottle(interval time.Duration) *progressThrottle {
	return &progressThrottle{interval: interval}
}

func (t *progressThrottle) allow(dp updater.DownloadProgress, now time.Time) bool {
	if dp.Error != nil || dp.Complete || dp.Activity != t.activity || now.Sub(t.last) >= t.interval {
		t.last, t.activity = now, dp.Activity
		return true
	}
	return false
}

// installStagedUpdate installs the staged update, if one is waiting and the user in
// session (0 at service start) has automatic downloads on. With launch set, it runs
// instead of launching the UI in session, and that user sees msiexec's progress bar;
// otherwise (at quit or service start) the install is silent. It reports whether an
// install is running, in which case the caller must not launch the UI: the manager
// that starts after the upgrade launches it. Launches requested while installing are
// queued the same way. If the install fails, onFailure gets the queued sessions so
// the caller can launch them on the current version.
func installStagedUpdate(session uint32, launch bool, onFailure func(sessions []uint32)) bool {
	updateLock.Lock()
	installing, hasStaged := installingStagedUpdate, stagedUpdate != nil
	updateLock.Unlock()
	if !installing && (!hasStaged || !autoDownloadUpdatesForSession(session)) {
		return false
	}

	launchSession := session
	if !launch {
		launchSession = 0
	}
	updateLock.Lock()
	if installingStagedUpdate {
		if launchSession != 0 {
			pendingLaunchSessions = append(pendingLaunchSessions, launchSession)
		}
		updateLock.Unlock()
		queueUIRelaunchAfterUpdate(launchSession)
		return true
	}
	staged := stagedUpdate
	if staged == nil {
		updateLock.Unlock()
		return false
	}
	stagedUpdate = nil
	installingStagedUpdate = true
	pendingLaunchSessions = nil
	if launchSession != 0 {
		pendingLaunchSessions = append(pendingLaunchSessions, launchSession)
	}
	updateLock.Unlock()
	queueUIRelaunchAfterUpdate(launchSession)

	logger.Info("Installing update %s in the background", staged.Version())
	go func() {
		stopActiveTunnels()
		err := installStagedFn(launchSession)
		installed := ""
		if err == nil {
			// The package restarts this process before getting here. If it didn't
			// (a daemon started by hand, a failed restart), carry on as after a failure.
			time.Sleep(installRestartGrace)
			logger.Error("Update %s is installed but this process is still running; resuming", staged.Version())
			installed = staged.Version()
		} else {
			logger.Error("Unable to install update %s: %v", staged.Version(), err)
		}
		finishStagedInstall(installed, onFailure)
	}()
	return true
}

// These are variables for tests.
var (
	installStagedFn     = updater.InstallStagedInBackground
	installRestartGrace = time.Minute
	cancelUIRelaunch    = updater.CancelUIRelaunchAfterUpdate
	reloadStagedUpdate  = loadStagedUpdate
)

// finishStagedInstall ends a background install that didn't replace this
// process: UI launches held back by it go ahead, and the update can be
// installed again. A package that is still staged, such as one that found the
// package manager locked, is picked up again.
//
// installed is the version that did install, when this process outlived it: the
// update is then neither offered nor downloaded again until this process restarts.
func finishStagedInstall(installed string, onFailure func(sessions []uint32)) {
	cancelUIRelaunch()
	updateLock.Lock()
	installingStagedUpdate = false
	installedVersion = installed
	sessions := pendingLaunchSessions
	pendingLaunchSessions = nil
	if installed != "" {
		updateState, foundVersion = UpdateStateUnknown, ""
	} else if updateState.HasUpdate() {
		updateState = UpdateStateFoundUpdate
	}
	updateLock.Unlock()
	reloadStagedUpdate()
	if onFailure != nil {
		onFailure(sessions)
	}
}

func queueUIRelaunchAfterUpdate(session uint32) {
	if session == 0 {
		return
	}
	if err := updater.RelaunchUIAfterUpdate(session); err != nil {
		logger.Error("Unable to queue UI relaunch for session %d after update: %v", session, err)
	}
}

// remindAboutStagedUpdate prompts the user to install a downloaded update once it
// has waited stagedUpdateReminderAge without the user quitting.
func remindAboutStagedUpdate() {
	for range time.Tick(time.Hour) {
		updateLock.Lock()
		remind := stagedUpdate != nil && updateState == UpdateStateUpdateDownloaded &&
			time.Since(stagedUpdate.StagedAt) >= stagedUpdateReminderAge
		if remind {
			updateState = UpdateStateInstallReminder
		}
		updateLock.Unlock()
		if remind {
			logger.Info("Downloaded update has not been installed for a week, prompting to install")
			IPCServerNotifyUpdateFound(UpdateStateInstallReminder)
		}
	}
}

// stageRetryDelay is how long to wait before trying a failed download again:
// 30 minutes, doubling with each failure. Zero, once that would be no sooner
// than the regular check interval, which then takes over.
func stageRetryDelay(failures int, interval time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := 30 * time.Minute
	for i := 1; i < failures && delay < interval; i++ {
		delay *= 2
	}
	if delay >= interval {
		return 0
	}
	return delay
}

func checkForUpdates() {
	if !config.AutoUpdateChecksEnabled() {
		logger.Info("Automatic update checks are disabled by config")
		return
	}

	go remindAboutStagedUpdate()

	// Initial jitter if started at boot - prevents all machines from checking at once after boot
	if services.StartedAtBoot() {
		jitterSleep(time.Minute*2, time.Minute*5)
	}

	noError, didNotify := true, false
	stageFailures := 0
	for {
		update, err := checkForUpdate()
		var stageErr error
		if err == nil && update != nil && autoDownloadUpdatesWanted() {
			stageErr = stageUpdate(update.Version())
		}
		if err == nil && update != nil {
			// Tell the user about the update even if downloading it failed.
			if state, changed := setFoundUpdate(update.Version()); changed {
				logger.Info("An update is available")
				IPCServerNotifyUpdateFound(state)
			}
			didNotify = true
			if stageErr != nil {
				stageFailures++
				logger.Error("Update checker: unable to download update %s: %v", update.Version(), stageErr)
				if delay := stageRetryDelay(stageFailures, config.UpdateCheckInterval()); delay > 0 {
					jitterSleep(delay, delay+delay/10)
					continue
				}
			} else {
				stageFailures = 0
			}
		} else if err != nil {
			logger.Error("Update checker: %v", err)
			if !didNotify {
				if noError {
					jitterSleep(time.Minute*4, time.Minute*6)
					noError = false
				} else {
					jitterSleep(time.Minute*25, time.Minute*30)
				}
				continue
			}
		}

		interval := config.UpdateCheckInterval()
		jitter := interval / 20 // ±5%
		if jitter < time.Minute {
			jitter = time.Minute
		}
		jitterSleep(interval-jitter, interval+jitter)
	}
}

// stagedUpdateInstalling reports whether a background install started by
// installStagedUpdate is running.
func stagedUpdateInstalling() bool {
	updateLock.Lock()
	defer updateLock.Unlock()
	return installingStagedUpdate
}
