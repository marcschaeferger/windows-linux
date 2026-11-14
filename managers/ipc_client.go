package managers

import (
	"encoding/gob"
	"errors"
	"os"
	"sync"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/fingerprint"
	"github.com/fosrl/windows-linux/logging"
	"github.com/fosrl/windows-linux/managers/secretstore"
	"github.com/fosrl/windows-linux/tunnel"
	"github.com/fosrl/windows-linux/updater"
)

// TunnelConfig is exported for use in UI
type TunnelConfig = tunnel.Config

type NotificationType int

const (
	ManagerStoppingNotificationType NotificationType = iota
	UpdateFoundNotificationType
	UpdateProgressNotificationType
	TunnelStateChangeNotificationType
	UIActionNotificationType
)

type MethodType int

const (
	QuitMethodType MethodType = iota
	UpdateStateMethodType
	UpdateMethodType
	StartTunnelMethodType
	StopTunnelMethodType
	StopAllTunnelsMethodType
	IsCLIInstalledMethodType
	InstallCLIMethodType
	GetUserSecretsMethodType
	SaveUserSecretsMethodType
	DeleteUserSecretsMethodType
	GetDevicePostureMethodType
	CheckForUpdatesMethodType
	SetAlwaysOnMethodType
	AlwaysOnEnabledMethodType
	UpdateVersionMethodType
	AutoDownloadUpdatesTurnedOnMethodType
	ReadLogMethodType // appended: the numbers above are part of the wire format
	// UpdateCheckedMethodType is Update with an error reply (a refusal).
	// UpdateMethodType stays for older UIs and gets no reply.
	UpdateCheckedMethodType
	// MayAdoptTunnelMethodType asks whether the running tunnel is not
	// another user's. Reply: bool.
	MayAdoptTunnelMethodType
)

var (
	rpcEncoder *gob.Encoder
	rpcDecoder *gob.Decoder
	rpcMutex   sync.Mutex
)

type ManagerStoppingCallback struct {
	cb func()
}

var managerStoppingCallbacks = make(map[*ManagerStoppingCallback]bool)

type UpdateFoundCallback struct {
	cb func(updateState UpdateState)
}

var updateFoundCallbacks = make(map[*UpdateFoundCallback]bool)

type UpdateProgressCallback struct {
	cb func(dp updater.DownloadProgress)
}

var updateProgressCallbacks = make(map[*UpdateProgressCallback]bool)

type TunnelStateChangeCallback struct {
	cb func(state TunnelState)
}

var tunnelStateChangeCallbacks = make(map[*TunnelStateChangeCallback]bool)

type UIActionCallback struct {
	cb func(action UIAction)
}

var uiActionCallbacks = make(map[*UIActionCallback]bool)

// IPCConnectionLostCallback runs when the manager's event pipe closes
// without an orderly shutdown, for example when the daemon crashed or was
// restarted by a package upgrade.
type IPCConnectionLostCallback struct {
	cb func()
}

var (
	ipcConnectionLostCallbacks = make(map[*IPCConnectionLostCallback]bool)
	ipcConnectionLostLock      sync.Mutex
)

// ipcConn is the pipes of the current connection, so a lost one can be closed
// and a new one told apart from it. Guarded by rpcMutex.
var (
	ipcConnFiles      []*os.File
	ipcConnGeneration uint64
)

func InitializeIPCClient(reader, writer, events *os.File) {
	rpcMutex.Lock()
	closeIPCFiles(ipcConnFiles)
	ipcConnGeneration++
	generation := ipcConnGeneration
	ipcConnFiles = []*os.File{reader, writer, events}
	rpcDecoder = gob.NewDecoder(reader)
	rpcEncoder = gob.NewEncoder(writer)
	rpcMutex.Unlock()
	registerSecretsIPC()
	registerDevicePostureIPC()
	tunnel.SetAdoptCheck(IPCClientMayAdoptTunnel)
	go func() {
		defer ipcConnectionLost(generation)
		decoder := gob.NewDecoder(events)
		for {
			var notificationType NotificationType
			err := decoder.Decode(&notificationType)
			if err != nil {
				return
			}
			switch notificationType {
			case ManagerStoppingNotificationType:
				for cb := range managerStoppingCallbacks {
					cb.cb()
				}
			case UpdateFoundNotificationType:
				var state UpdateState
				err = decoder.Decode(&state)
				if err != nil {
					continue
				}
				for cb := range updateFoundCallbacks {
					cb.cb(state)
				}
			case UpdateProgressNotificationType:
				var dp updater.DownloadProgress
				err = decoder.Decode(&dp.Activity)
				if err != nil {
					continue
				}
				err = decoder.Decode(&dp.BytesDownloaded)
				if err != nil {
					continue
				}
				err = decoder.Decode(&dp.BytesTotal)
				if err != nil {
					continue
				}
				var errStr string
				err = decoder.Decode(&errStr)
				if err != nil {
					continue
				}
				if len(errStr) > 0 {
					dp.Error = errors.New(errStr)
				}
				err = decoder.Decode(&dp.Complete)
				if err != nil {
					continue
				}
				for cb := range updateProgressCallbacks {
					cb.cb(dp)
				}
			case TunnelStateChangeNotificationType:
				var state TunnelState
				err = decoder.Decode(&state)
				if err != nil {
					continue
				}
				for cb := range tunnelStateChangeCallbacks {
					cb.cb(state)
				}
			case UIActionNotificationType:
				var sessionID uint32
				var action UIAction
				if decoder.Decode(&sessionID) != nil || decoder.Decode(&action) != nil {
					continue
				}
				if sessionID != currentSessionID() {
					continue
				}
				for cb := range uiActionCallbacks {
					cb.cb(action)
				}
			}
		}
	}()
}

func closeIPCFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}

// ipcConnectionLost drops the connection of the given generation, so later
// RPCs fail with errIPCNotConnected instead of a broken pipe, and tells the
// registered callbacks. A newer connection is left alone.
func ipcConnectionLost(generation uint64) {
	rpcMutex.Lock()
	if generation != ipcConnGeneration || rpcEncoder == nil {
		rpcMutex.Unlock()
		return
	}
	rpcEncoder, rpcDecoder = nil, nil
	closeIPCFiles(ipcConnFiles)
	ipcConnFiles = nil
	rpcMutex.Unlock()
	logger.Warn("IPC: lost the connection to the manager")
	ipcConnectionLostLock.Lock()
	callbacks := make([]*IPCConnectionLostCallback, 0, len(ipcConnectionLostCallbacks))
	for cb := range ipcConnectionLostCallbacks {
		callbacks = append(callbacks, cb)
	}
	ipcConnectionLostLock.Unlock()
	for _, cb := range callbacks {
		cb.cb()
	}
}

// IPCClientDropConnection treats the current connection as lost now, for when
// the manager announced it is stopping and its pipes are about to close:
// later RPCs fail as not connected instead of reaching a stopping manager.
func IPCClientDropConnection() {
	rpcMutex.Lock()
	generation := ipcConnGeneration
	rpcMutex.Unlock()
	ipcConnectionLost(generation)
}

// IPCClientRegisterConnectionLost registers cb to run when the connection to
// the manager drops.
func IPCClientRegisterConnectionLost(cb func()) *IPCConnectionLostCallback {
	s := &IPCConnectionLostCallback{cb}
	ipcConnectionLostLock.Lock()
	ipcConnectionLostCallbacks[s] = true
	ipcConnectionLostLock.Unlock()
	return s
}

func (cb *IPCConnectionLostCallback) Unregister() {
	ipcConnectionLostLock.Lock()
	delete(ipcConnectionLostCallbacks, cb)
	ipcConnectionLostLock.Unlock()
}

// errIPCNotConnected is returned when the UI has no connection to the manager,
// for example when it was started on its own for development.
var errIPCNotConnected = errors.New("not connected to the Pangolin manager service")

// IsIPCNotConnected reports whether err means the UI has no manager
// connection at all, so the manager can't be running anything for it.
func IsIPCNotConnected(err error) bool { return errors.Is(err, errIPCNotConnected) }

func rpcDecodeError() error {
	var str string
	err := rpcDecoder.Decode(&str)
	if err != nil {
		return err
	}
	if len(str) == 0 {
		return nil
	}
	return errors.New(str)
}

func IPCClientQuit(stopTunnelsOnQuit bool) (alreadyQuit bool, err error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return false, errIPCNotConnected
	}

	err = rpcEncoder.Encode(QuitMethodType)
	if err != nil {
		return
	}
	err = rpcEncoder.Encode(stopTunnelsOnQuit)
	if err != nil {
		return
	}
	err = rpcDecoder.Decode(&alreadyQuit)
	if err != nil {
		return
	}
	err = rpcDecodeError()
	return
}

func IPCClientUpdateState() (updateState UpdateState, err error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return UpdateStateUnknown, errIPCNotConnected
	}

	err = rpcEncoder.Encode(UpdateStateMethodType)
	if err != nil {
		return
	}
	err = rpcDecoder.Decode(&updateState)
	if err != nil {
		return
	}
	return
}

// IPCClientUpdateVersion returns the version of the update the manager found, or "" if none.
func IPCClientUpdateVersion() (version string, err error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return "", errIPCNotConnected
	}

	err = rpcEncoder.Encode(UpdateVersionMethodType)
	if err != nil {
		return
	}
	err = rpcDecoder.Decode(&version)
	return
}

// IPCClientAutoDownloadUpdatesTurnedOn tells the manager that this user turned on
// automatic update downloads, so it downloads an available update now.
func IPCClientAutoDownloadUpdatesTurnedOn() error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()

	if rpcEncoder == nil {
		return errors.New("manager IPC is not connected")
	}
	return rpcEncoder.Encode(AutoDownloadUpdatesTurnedOnMethodType)
}

func IPCClientCheckForUpdates() (updateState UpdateState, err error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return UpdateStateUnknown, errIPCNotConnected
	}

	err = rpcEncoder.Encode(CheckForUpdatesMethodType)
	if err != nil {
		return
	}
	err = rpcDecoder.Decode(&updateState)
	if err != nil {
		return
	}
	err = rpcDecodeError()
	return
}

// IPCClientUpdate asks the manager to install the update. The manager
// refuses users who may not install updates (the error is returned) and
// otherwise stops the tunnels itself before the install starts.
func IPCClientUpdate() error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}
	if err := rpcEncoder.Encode(UpdateCheckedMethodType); err != nil {
		return err
	}
	return rpcDecodeError()
}

// IPCClientMayAdoptTunnel reports whether the UI may adopt the running
// tunnel: false when another user on the computer started it. Without an
// answer from the manager it says yes, as before.
func IPCClientMayAdoptTunnel() bool {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return true
	}
	if err := rpcEncoder.Encode(MayAdoptTunnelMethodType); err != nil {
		return true
	}
	var mayAdopt bool
	if err := rpcDecoder.Decode(&mayAdopt); err != nil {
		return true
	}
	return mayAdopt
}

func IPCClientRegisterManagerStopping(cb func()) *ManagerStoppingCallback {
	s := &ManagerStoppingCallback{cb}
	managerStoppingCallbacks[s] = true
	return s
}

func (cb *ManagerStoppingCallback) Unregister() {
	delete(managerStoppingCallbacks, cb)
}

func IPCClientRegisterUpdateFound(cb func(updateState UpdateState)) *UpdateFoundCallback {
	s := &UpdateFoundCallback{cb}
	updateFoundCallbacks[s] = true
	return s
}

func (cb *UpdateFoundCallback) Unregister() {
	delete(updateFoundCallbacks, cb)
}

func IPCClientRegisterUpdateProgress(cb func(dp updater.DownloadProgress)) *UpdateProgressCallback {
	s := &UpdateProgressCallback{cb}
	updateProgressCallbacks[s] = true
	return s
}

func (cb *UpdateProgressCallback) Unregister() {
	delete(updateProgressCallbacks, cb)
}

func IPCClientStartTunnel(config TunnelConfig) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	err := rpcEncoder.Encode(StartTunnelMethodType)
	if err != nil {
		return err
	}
	err = rpcEncoder.Encode(config)
	if err != nil {
		return err
	}
	return rpcDecodeError()
}

func IPCClientStopTunnel() error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	err := rpcEncoder.Encode(StopTunnelMethodType)
	if err != nil {
		return err
	}
	err = rpcDecodeError()
	return err
}

func IPCClientStopAllTunnels() error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	err := rpcEncoder.Encode(StopAllTunnelsMethodType)
	if err != nil {
		return err
	}
	err = rpcDecodeError()
	return err
}

func IPCClientIsCLIInstalled() (installed bool, err error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return false, errIPCNotConnected
	}

	err = rpcEncoder.Encode(IsCLIInstalledMethodType)
	if err != nil {
		return false, err
	}
	err = rpcDecoder.Decode(&installed)
	if err != nil {
		return false, err
	}
	return installed, nil
}

func IPCClientInstallCLI() error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	err := rpcEncoder.Encode(InstallCLIMethodType)
	if err != nil {
		return err
	}
	return rpcDecodeError()
}

func IPCClientRegisterTunnelStateChange(cb func(state TunnelState)) *TunnelStateChangeCallback {
	s := &TunnelStateChangeCallback{cb}
	tunnelStateChangeCallbacks[s] = true
	return s
}

func (cb *TunnelStateChangeCallback) Unregister() {
	delete(tunnelStateChangeCallbacks, cb)
}

func IPCClientRegisterUIAction(cb func(action UIAction)) *UIActionCallback {
	s := &UIActionCallback{cb}
	uiActionCallbacks[s] = true
	return s
}

func (cb *UIActionCallback) Unregister() {
	delete(uiActionCallbacks, cb)
}

// IPCClientReady reports whether the UI has an active RPC connection to the manager service.
func IPCClientReady() bool {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	return rpcEncoder != nil
}

func IPCClientGetUserSecrets(userID string) (secretstore.UserSecrets, error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return secretstore.UserSecrets{}, errIPCNotConnected
	}

	if rpcEncoder == nil {
		return secretstore.UserSecrets{}, errors.New("manager IPC is not connected")
	}
	err := rpcEncoder.Encode(GetUserSecretsMethodType)
	if err != nil {
		return secretstore.UserSecrets{}, err
	}
	err = rpcEncoder.Encode(userID)
	if err != nil {
		return secretstore.UserSecrets{}, err
	}
	var secrets secretstore.UserSecrets
	err = rpcDecoder.Decode(&secrets)
	if err != nil {
		return secretstore.UserSecrets{}, err
	}
	err = rpcDecodeError()
	if err == nil {
		// Whatever format a later log line prints these in, they are masked.
		logging.RegisterSecrets(secrets.SessionToken, secrets.OlmSecret)
	}
	return secrets, err
}

func IPCClientSaveUserSecrets(userID string, update secretstore.SecretsUpdate) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	if rpcEncoder == nil {
		return errors.New("manager IPC is not connected")
	}
	err := rpcEncoder.Encode(SaveUserSecretsMethodType)
	if err != nil {
		return err
	}
	err = rpcEncoder.Encode(userID)
	if err != nil {
		return err
	}
	err = rpcEncoder.Encode(update)
	if err != nil {
		return err
	}
	err = rpcDecodeError()
	if err != nil {
		logger.Debug("IPC client: SaveUserSecrets() failed (userId=%s): %v", userID, err)
	}
	return err
}

func IPCClientDeleteUserSecrets(userID string, flags secretstore.DeleteSecretsFlags) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	if rpcEncoder == nil {
		return errors.New("manager IPC is not connected")
	}
	err := rpcEncoder.Encode(DeleteUserSecretsMethodType)
	if err != nil {
		return err
	}
	err = rpcEncoder.Encode(userID)
	if err != nil {
		return err
	}
	err = rpcEncoder.Encode(flags)
	if err != nil {
		return err
	}
	return rpcDecodeError()
}

func IPCClientGetDevicePosture() (fingerprint.DevicePostureSnapshot, error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return fingerprint.DevicePostureSnapshot{}, errIPCNotConnected
	}

	if rpcEncoder == nil {
		return fingerprint.DevicePostureSnapshot{}, errors.New("manager IPC is not connected")
	}
	err := rpcEncoder.Encode(GetDevicePostureMethodType)
	if err != nil {
		return fingerprint.DevicePostureSnapshot{}, err
	}
	var snapshot fingerprint.DevicePostureSnapshot
	err = rpcDecoder.Decode(&snapshot)
	if err != nil {
		return fingerprint.DevicePostureSnapshot{}, err
	}
	err = rpcDecodeError()
	if err != nil {
		logger.Debug("IPC client: GetDevicePosture() failed: %v", err)
	}
	return snapshot, err
}

func IPCClientSetAlwaysOn(enabled bool) error {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return errIPCNotConnected
	}

	if rpcEncoder == nil {
		return errors.New("manager IPC is not connected")
	}
	if err := rpcEncoder.Encode(SetAlwaysOnMethodType); err != nil {
		return err
	}
	return rpcEncoder.Encode(enabled)
}

func IPCClientAlwaysOn() bool {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()

	if rpcEncoder == nil {
		return false
	}
	if err := rpcEncoder.Encode(AlwaysOnEnabledMethodType); err != nil {
		logger.Error("Always-On: failed to query manager: %v", err)
		return false
	}
	var enabled bool
	if err := rpcDecoder.Decode(&enabled); err != nil {
		logger.Error("Always-On: failed to read manager response: %v", err)
		return false
	}
	return enabled
}

// IPCClientReadLog reads up to max bytes of the daemon log from offset on. It
// returns errIPCNotConnected when the UI isn't attached to the daemon.
func IPCClientReadLog(offset int64, max int) (LogChunk, error) {
	rpcMutex.Lock()
	defer rpcMutex.Unlock()
	if rpcEncoder == nil {
		return LogChunk{}, errIPCNotConnected
	}

	if err := rpcEncoder.Encode(ReadLogMethodType); err != nil {
		return LogChunk{}, err
	}
	if err := rpcEncoder.Encode(offset); err != nil {
		return LogChunk{}, err
	}
	if err := rpcEncoder.Encode(max); err != nil {
		return LogChunk{}, err
	}
	var chunk LogChunk
	if err := rpcDecoder.Decode(&chunk); err != nil {
		return LogChunk{}, err
	}
	err := rpcDecodeError()
	return chunk, err
}

// IsIPCNotConnected reports whether err means the UI has no connection to the daemon.
func IsIPCNotConnected(err error) bool {
	return errors.Is(err, errIPCNotConnected)
}
