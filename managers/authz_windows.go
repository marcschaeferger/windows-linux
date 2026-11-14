package managers

import (
	"errors"
	"unsafe"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/config"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
)

// errUpdateNeedsAdmin is returned when a standard user asks to install an
// update while other users are signed in or connected.
var errUpdateNeedsAdmin = errors.New("installing updates needs an administrator while other users are signed in")

// authorizeTunnelUse lets every connected user use the tunnel: the service
// starts a UI only for users signed in to an interactive session, and hands
// the IPC pipes to that process alone.
func authorizeTunnelUse(s *ManagerService) error {
	return nil
}

// ownerIsLocal is not used on Windows, where logoff stops a session's tunnels.
func ownerIsLocal(s *ManagerService) bool { return false }

// prepareTunnelAccess has nothing to prepare: OLM creates its pipe with a
// DACL that allows Everyone, which needs a change in OLM to restrict.
func prepareTunnelAccess(owner tunnelOwner) error { return nil }

// updateWithoutAdmin lets a standard user install an update, staged or
// not, when nobody else is affected (checked by the caller), as before.
func updateWithoutAdmin(s *ManagerService, staged bool) bool { return true }

// otherUsersPresent reports whether another user is signed in.
func otherUsersPresent(s *ManagerService) bool { return otherUserSignedIn(s.clientUserKey) }

// adminOverridesOthers: an administrator's elevated token may update while
// others are signed in, as with any MSI install.
func adminOverridesOthers(s *ManagerService) bool { return true }

// quitAllowed: Quit stops and uninstalls the manager for every user.
func quitAllowed(s *ManagerService) bool { return s.elevatedToken != 0 }

// otherUserSignedIn reports whether a user other than sid has a session,
// connected or not (fast user switching leaves the other user's session
// disconnected but signed in). It errs towards true.
func otherUserSignedIn(sid string) bool {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		logger.Error("WTSEnumerateSessions failed: %v", err)
		return true
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	for _, session := range unsafe.Slice(sessions, count) {
		if session.SessionID == 0 {
			continue
		}
		var token windows.Token
		if err := windows.WTSQueryUserToken(session.SessionID, &token); err != nil {
			continue // no user signed in to this session
		}
		user, err := token.GetTokenUser()
		other := err != nil || user.User.Sid.String() != sid
		token.Close()
		if other {
			return true
		}
	}
	return false
}

func tunnelServiceName(name string) string {
	serviceName := config.AppName + "Tunnel$" + sanitizeServiceName(name)
	if len(serviceName) > 80 {
		serviceName = serviceName[:80]
	}
	return serviceName
}

// tunnelRunning reports whether the tunnel service for name exists and is
// not stopped. When that can't be told, it says running, so an owner is
// not forgotten on a guess.
func tunnelRunning(name string) bool {
	m, err := serviceManager()
	if err != nil {
		return true
	}
	service, err := m.OpenService(tunnelServiceName(name))
	if err != nil {
		return !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return true
	}
	return status.State != svc.Stopped
}

// The owner is also stored in the tunnel service's registry key, which only
// SYSTEM and administrators can write and which goes away with the service,
// so a tunnel that outlives a manager restart keeps its owner.
const (
	ownerValue        = "PangolinOwner"
	ownerSessionValue = "PangolinOwnerSession"
)

func tunnelServiceKey(name string) string {
	return `SYSTEM\CurrentControlSet\Services\` + tunnelServiceName(name)
}

func recordTunnelOwner(name string, owner tunnelOwner) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, tunnelServiceKey(name), registry.SET_VALUE)
	if err != nil {
		logger.Error("Failed to record the owner of tunnel %s: %v", name, err)
		return
	}
	defer key.Close()
	if err := key.SetStringValue(ownerValue, owner.userKey); err != nil {
		logger.Error("Failed to record the owner of tunnel %s: %v", name, err)
	}
	if err := key.SetDWordValue(ownerSessionValue, owner.session); err != nil {
		logger.Error("Failed to record the owner session of tunnel %s: %v", name, err)
	}
}

func recoverTunnelOwner(name string) (tunnelOwner, bool) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, tunnelServiceKey(name), registry.QUERY_VALUE)
	if err != nil {
		return tunnelOwner{}, false
	}
	defer key.Close()
	userKey, _, err := key.GetStringValue(ownerValue)
	if err != nil || userKey == "" {
		return tunnelOwner{}, false
	}
	session, _, err := key.GetIntegerValue(ownerSessionValue)
	if err != nil {
		return tunnelOwner{}, false
	}
	logger.Info("Tunnel %s belongs to %s (session %d), from its service", name, userKey, session)
	return tunnelOwner{userKey: userKey, session: uint32(session)}, true
}
