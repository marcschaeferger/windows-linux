package managers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/managers/secretstore"
	"github.com/fosrl/windows-linux/tunnel"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var cachedServiceManager *mgr.Mgr

func serviceManager() (*mgr.Mgr, error) {
	if cachedServiceManager != nil {
		return cachedServiceManager, nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	cachedServiceManager = m
	return cachedServiceManager, nil
}

var ErrManagerAlreadyRunning = errors.New("Manager already installed and running")

func InstallManager() error {
	m, err := serviceManager()
	if err != nil {
		return err
	}

	serviceName := config.AppName + "Manager"
	service, err := m.OpenService(serviceName)
	if err == nil {
		defer service.Close()
		status, err := service.Query()
		if err != nil {
			return err
		}
		switch status.State {
		case svc.StartPending:
			// We were *just* started by something else, so return success here, assuming the other program
			// starting this does the right thing. This can happen when, e.g., the updater relaunches the
			// manager service and then invokes the executable to raise the UI.
			return nil
		case svc.Stopped:
			// Start the registered service as it is. It is never deleted and
			// re-created here: that would replace the MSI's service with a
			// LocalSystem service running whatever copy of the exe was launched,
			// possibly one in a user-writable folder.
			if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return err
			}
			return nil
		default:
			return ErrManagerAlreadyRunning
		}
	}

	path, err := os.Executable()
	if err != nil {
		return err
	}
	// Register the resolved path the check approved, so a link can't make the
	// service run something other than what was checked.
	path, err = checkManagerExecutableLocation(path)
	if err != nil {
		return err
	}

	svcConfig := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  config.AppName + " Manager",
	}

	service, err = m.CreateService(serviceName, path, svcConfig, "/managerservice")
	if err != nil {
		return err
	}
	service.Start()
	return service.Close()
}

// checkManagerExecutableLocation refuses to register a LocalSystem service for
// an executable outside Program Files, where the MSI installs it. A copy in a
// user-writable folder (Downloads, a USB stick) could be replaced by any
// process of that user and would then run as SYSTEM.
func checkManagerExecutableLocation(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve %s: %w", path, err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", err
	}
	for _, folder := range []*windows.KNOWNFOLDERID{windows.FOLDERID_ProgramFiles, windows.FOLDERID_ProgramFilesX86} {
		base, err := windows.KnownFolderPath(folder, 0)
		if err == nil && base != "" && pathWithinFold(base, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("refusing to install the %s Manager service for %s: run the installed copy under Program Files or reinstall %s", config.AppName, path, config.AppName)
}

func UninstallManager() error {
	m, err := serviceManager()
	if err != nil {
		return err
	}
	serviceName := config.AppName + "Manager"
	service, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	service.Control(svc.Stop)
	err = service.Delete()
	err2 := service.Close()
	if err != nil {
		return err
	}
	return err2
}

// InstallTunnel creates a Windows service for a tunnel
func InstallTunnel(configJSON string) error {
	m, err := serviceManager()
	if err != nil {
		return err
	}
	path, err := os.Executable()
	if err != nil {
		return err
	}

	// Parse config to get tunnel name
	var tunnelConfig map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &tunnelConfig); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	name, ok := tunnelConfig["name"].(string)
	if !ok || name == "" {
		name = tunnel.DefaultTunnelName
	}
	// The name comes from the UI over IPC; refuse anything that could leave
	// the Tunnels directory before it is used for a service or a file.
	configDir, err := tunnelConfigDir()
	if err != nil {
		return err
	}
	configPath, err := tunnelConfigPath(configDir, name)
	if err != nil {
		return err
	}

	// Create service name (Windows service names have restrictions)
	serviceName := config.AppName + "Tunnel$" + sanitizeServiceName(name)
	if len(serviceName) > 80 {
		serviceName = serviceName[:80]
	}

	// Check if service already exists
	service, err := m.OpenService(serviceName)
	if err == nil {
		status, err := service.Query()
		if err != nil && err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			service.Close()
			return err
		}
		if status.State != svc.Stopped && err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			if stopErr := stopTunnelService(service, serviceName); stopErr != nil {
				service.Close()
				return stopErr
			}
		}
		err = service.Delete()
		service.Close()
		if err != nil && err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			return err
		}
		// Wait for the service to be fully deleted. Something else holding a
		// handle to it (the Services console, a leaked handle) keeps it marked
		// for deletion indefinitely, so give up instead of hanging StartTunnel
		// and with it the UI's whole IPC channel.
		if !waitForServiceDeleted(m, serviceName, serviceDeleteTimeout) {
			return fmt.Errorf("previous tunnel service %s is still pending deletion; close the Services console or any other tool that has it open, or restart the computer", serviceName)
		}
	}

	// Older versions wrote the config, secrets included, into a
	// ProgramData folder every user can read; remove any copy left there.
	removeLegacyTunnelConfig(name)

	// Pass the config to the service in a file only SYSTEM and Administrators
	// can read: it holds the olm secret and the user's session token.
	if err := writeTunnelConfig(configPath, []byte(configJSON)); err != nil {
		return err
	}

	svcConfig := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartManual,
		ErrorControl: mgr.ErrorNormal,
		Dependencies: []string{"Nsi", "TcpIp"},
		DisplayName:  config.AppName + " Tunnel: " + name,
		SidType:      windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}

	// Create service with /tunnelservice argument and config path
	service, err = m.CreateService(serviceName, path, svcConfig, "/tunnelservice", configPath)
	if err != nil {
		return err
	}

	err = service.Start()
	service.Close()
	return err
}

// UninstallTunnel removes a Windows service for a tunnel
func UninstallTunnel(name string) error {
	configDir, err := tunnelConfigDir()
	if err != nil {
		return err
	}
	configPath, err := tunnelConfigPath(configDir, name)
	if err != nil {
		return err
	}
	m, err := serviceManager()
	if err != nil {
		return err
	}

	serviceName := config.AppName + "Tunnel$" + sanitizeServiceName(name)
	if len(serviceName) > 80 {
		serviceName = serviceName[:80]
	}

	service, err := m.OpenService(serviceName)
	if err != nil {
		if err == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			return nil // Already uninstalled
		}
		return err
	}
	// Wait for the service to actually stop, so the tunnel only reads as
	// stopped once another one can be started.
	if stopErr := stopTunnelService(service, serviceName); stopErr != nil {
		logger.Error("Failed to stop tunnel service %s: %v", serviceName, stopErr)
	}
	err = service.Delete()
	service.Close()
	if err != nil && err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
		return err
	}
	waitForServiceDeleted(m, serviceName, serviceDeleteTimeout)

	// Clean up config file
	os.Remove(configPath) // Best effort cleanup
	removeLegacyTunnelConfig(name)

	return nil
}

const (
	serviceStopTimeout      = 30 * time.Second
	serviceTerminateTimeout = 10 * time.Second
	serviceDeleteTimeout    = 10 * time.Second
)

// stopTunnelService stops a tunnel service and waits until it is stopped. If
// the service does not stop in time, its process is terminated.
func stopTunnelService(service *mgr.Service, serviceName string) error {
	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("failed to query tunnel service: %w", err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	pid := status.ProcessId
	logger.Info("Stopping tunnel service %s (state %d, pid %d)", serviceName, status.State, pid)
	started := time.Now()

	// A service that is already stopping can't take another Stop; just wait
	// for it to finish.
	// A service that doesn't answer the Stop (ERROR_SERVICE_REQUEST_TIMEOUT)
	// is hung, so it is terminated below like one that never finishes stopping.
	stopped := false
	if status.State != svc.StopPending {
		_, ctrlErr := service.Control(svc.Stop)
		switch {
		case ctrlErr == nil || stopInProgress(ctrlErr):
			stopped = waitForServiceStopped(service, serviceStopTimeout)
		case errors.Is(ctrlErr, windows.ERROR_SERVICE_REQUEST_TIMEOUT):
			logger.Error("Tunnel service %s did not respond to Stop", serviceName)
		default:
			return fmt.Errorf("failed to stop running tunnel service: %w", ctrlErr)
		}
	} else {
		stopped = waitForServiceStopped(service, serviceStopTimeout)
	}
	if stopped {
		logger.Info("Tunnel service %s stopped after %v", serviceName, time.Since(started).Round(time.Millisecond))
		return nil
	}

	if status, err := service.Query(); err == nil {
		if status.ProcessId != 0 {
			pid = status.ProcessId
		}
		logger.Error("Tunnel service %s did not stop after %v (state %d, pid %d); terminating its process",
			serviceName, time.Since(started).Round(time.Millisecond), status.State, pid)
	} else {
		logger.Error("Tunnel service %s did not stop after %v (query failed: %v); terminating its process",
			serviceName, time.Since(started).Round(time.Millisecond), err)
	}
	if err := terminateServiceProcess(pid); err != nil {
		logger.Error("Failed to terminate tunnel service process %d: %v", pid, err)
	}
	if !waitForServiceStopped(service, serviceTerminateTimeout) {
		return errors.New("timed out waiting for tunnel service to stop")
	}
	logger.Info("Tunnel service %s stopped after terminating its process", serviceName)
	return nil
}

// terminateServiceProcess ends a service process that did not stop on request
// and waits for it to exit.
func terminateServiceProcess(pid uint32) error {
	if pid == 0 || pid == windows.GetCurrentProcessId() {
		return fmt.Errorf("refusing to terminate process %d", pid)
	}
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.TerminateProcess(process, 1); err != nil {
		return err
	}
	if _, err := windows.WaitForSingleObject(process, uint32(serviceTerminateTimeout/time.Millisecond)); err != nil {
		return err
	}
	return nil
}

// stopInProgress reports whether a Stop control failed only because the
// service is already stopping or stopped.
func stopInProgress(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) || errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE)
}

// waitForServiceDeleted waits until a service marked for deletion is gone and
// reports whether it went before the timeout.
func waitForServiceDeleted(m *mgr.Mgr, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		service, err := m.OpenService(name)
		if err != nil && err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			return true
		}
		if service != nil {
			service.Close()
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Second / 3)
	}
}

const (
	tunnelConfigDirSDDL  = "O:SYG:SYD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	tunnelConfigFileSDDL = "O:SYG:SYD:PAI(A;;FA;;;SY)(A;;FA;;;BA)"
)

// tunnelConfigDir is where tunnel configs are handed to the tunnel service:
// Pangolin\Tunnels in the local app data of LocalSystem, which both the
// manager and the tunnel service run as. Unlike %ProgramData%, whose default
// ACL lets every user create folders, only SYSTEM and Administrators can
// create anything under the system profile, so no other user can plant the
// directory, swap it for a junction or hold a handle to it.
func tunnelConfigDir() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("failed to locate the service's local app data: %w", err)
	}
	return filepath.Join(base, config.AppName, "Tunnels"), nil
}

// removeLegacyTunnelConfig deletes %ProgramData%\Pangolin\Tunnels\<name>.json,
// where older versions passed the tunnel config (with the olm secret and
// session token) under the ProgramData ACL every user can read. Users can
// create folders and junctions under ProgramData, so the file is opened
// without following a reparse point at its own path, its final path is checked
// to still be the expected one (a junction anywhere above it changes that),
// and it is deleted through that handle. name must already be validated.
func removeLegacyTunnelConfig(name string) {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		return
	}
	legacyPath, err := tunnelConfigPath(filepath.Join(programData, config.AppName, "Tunnels"), name)
	if err != nil {
		return
	}
	// The expected final path is built on the resolved ProgramData folder, so
	// a machine where ProgramData itself is redirected (junction, short name)
	// still matches; only links below it, which users can create, make the
	// paths differ.
	resolvedProgramData, err := finalPathOf(programData, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return
	}
	expected := filepath.Join(resolvedProgramData, config.AppName, "Tunnels", name+".json")
	path16, err := windows.UTF16PtrFromString(legacyPath)
	if err != nil {
		return
	}
	h, err := windows.CreateFile(path16, windows.DELETE|windows.FILE_READ_ATTRIBUTES, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			logger.Debug("Legacy tunnel config %s not removed: %v", legacyPath, err)
		}
		return
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		logger.Error("Legacy tunnel config %s is not a regular file; leaving it", legacyPath)
		return
	}
	final, err := finalPathOfHandle(h)
	if err != nil {
		return
	}
	if !strings.EqualFold(filepath.Clean(final), filepath.Clean(expected)) {
		logger.Error("Legacy tunnel config %s resolves to %s; leaving it", legacyPath, final)
		return
	}
	deleteFile := byte(1) // FILE_DISPOSITION_INFO{DeleteFile: TRUE}
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&deleteFile)), uint32(unsafe.Sizeof(deleteFile))); err != nil {
		logger.Error("Failed to remove legacy tunnel config %s: %v", legacyPath, err)
		return
	}
	logger.Info("Removed legacy tunnel config %s", legacyPath)
}

// finalPathOf opens path (following links) and returns its final DOS path.
func finalPathOf(path string, flags uint32) (string, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(path16, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return finalPathOfHandle(h)
}

// finalPathOfHandle returns the normalized DOS path of an open handle without
// the \\?\ prefix.
func finalPathOfHandle(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if int(n) >= len(buf) {
		return "", errors.New("path too long")
	}
	return strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`), nil
}

// writeTunnelConfig writes a tunnel config into the Tunnels directory with a
// SYSTEM/Administrators-only DACL, creating the directory first.
func writeTunnelConfig(configPath string, data []byte) error {
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(filepath.Dir(configDir), 0o700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	if err := secretstore.EnsureProtectedDir(configDir, tunnelConfigDirSDDL); err != nil {
		return fmt.Errorf("failed to secure config directory: %w", err)
	}
	if err := secretstore.CreateProtectedFile(configPath, tunnelConfigFileSDDL, data); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

func waitForServiceStopped(service *mgr.Service, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return false
		}
		if status.State == svc.Stopped {
			return true
		}
		time.Sleep(time.Second / 3)
	}
	return false
}

// sanitizeServiceName removes invalid characters from service name
func sanitizeServiceName(name string) string {
	// Windows service names can only contain: letters, numbers, and: -_()[]{}
	var result strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' {
			result.WriteRune(r)
		} else {
			result.WriteRune('_')
		}
	}
	return result.String()
}
