package managers

import (
	"bytes"
	"encoding/gob"
	"errors"
	"sync"
	"time"

	"github.com/fosrl/newt/logger"

	"github.com/fosrl/windows-linux/tunnel"
)

// There is one tunnel per computer. It belongs to the user who started it:
// other users can't replace, stop or watch it, and it is stopped when its
// owner signs out. The manager records the owner in activeTunnels.

// tunnelOwner is the user who started a tunnel: their user key (a SID on
// Windows, a UID on Linux), the session they started it from, and whether
// they were signed in on a local seat then (Linux; see watchTunnelOwners).
type tunnelOwner struct {
	userKey string
	session uint32
	local   bool
}

// errTunnelInUse is returned when a user tries to start a tunnel while
// another user's is running.
var errTunnelInUse = errors.New("Pangolin is connected for another user on this computer; they need to disconnect first")

var (
	// tunnelOpLock serializes starting and stopping tunnels, so two users'
	// requests can't interleave their ownership checks and installs.
	tunnelOpLock sync.Mutex

	// Tunnel process control; tests replace them.
	installTunnel   = InstallTunnel
	uninstallTunnel = UninstallTunnel
	tunnelIsRunning = tunnelRunning
)

// runningTunnelOwner returns who started the tunnel called name, if that is
// known and the tunnel is still running. On Windows the owner of a tunnel
// that outlived a manager restart is read back from its service.
func runningTunnelOwner(name string) (tunnelOwner, bool) {
	activeTunnelsLock.RLock()
	owner, ok := activeTunnels[name]
	activeTunnelsLock.RUnlock()
	if !tunnelIsRunning(name) {
		if ok {
			forgetTunnelOwner(name, owner)
		}
		return tunnelOwner{}, false
	}
	if ok {
		return owner, true
	}
	if owner, ok = recoverTunnelOwner(name); ok {
		activeTunnelsLock.Lock()
		if _, exists := activeTunnels[name]; !exists {
			activeTunnels[name] = owner
		}
		activeTunnelsLock.Unlock()
	}
	return owner, ok
}

// forgetTunnelOwner removes the record for name if it still names owner.
func forgetTunnelOwner(name string, owner tunnelOwner) {
	activeTunnelsLock.Lock()
	if activeTunnels[name] == owner {
		delete(activeTunnels, name)
	}
	activeTunnelsLock.Unlock()
}

// setTunnelCallbacks points the tunnel package at this manager. State
// changes go to the UIs of ownerKey, the user whose tunnel it is.
func setTunnelCallbacks(ownerKey string) {
	tunnel.SetStateChangeCallback(func(state TunnelState) {
		notifyTunnelState(ownerKey, state)
	})
	tunnel.SetInstallTunnelCallback(func(configJSON string) error { return installTunnel(configJSON) })
	tunnel.SetUninstallTunnelCallback(func(name string) error { return uninstallTunnel(name) })
}

// startTunnelChecked is StartTunnel as the IPC client may call it: the user
// must be allowed to use the tunnel (authorizeTunnelUse), the manager picks
// the tunnel name, and another user's running tunnel is not replaced.
func (s *ManagerService) startTunnelChecked(config tunnel.Config) error {
	if err := authorizeTunnelUse(s); err != nil {
		return err
	}
	// The name selects the service or process and its files.
	config.Name = tunnel.DefaultTunnelName

	tunnelOpLock.Lock()
	defer tunnelOpLock.Unlock()
	if owner, ok := runningTunnelOwner(config.Name); ok && owner.userKey != s.clientUserKey {
		logger.Error("IPC server: StartTunnel refused for %s: the tunnel of %s is running", s.clientUserKey, owner.userKey)
		return errTunnelInUse
	}
	owner := tunnelOwner{userKey: s.clientUserKey, session: s.session, local: ownerIsLocal(s)}
	if err := prepareTunnelAccess(owner); err != nil {
		return err
	}
	return s.StartTunnel(config)
}

// recordStartedTunnel notes who started the tunnel called name.
func (s *ManagerService) recordStartedTunnel(name string) {
	owner := tunnelOwner{userKey: s.clientUserKey, session: s.session, local: ownerIsLocal(s)}
	activeTunnelsLock.Lock()
	activeTunnels[name] = owner
	activeTunnelsLock.Unlock()
	recordTunnelOwner(name, owner)
}

// stopOwnTunnel stops this client's user's tunnel. Another user's running
// tunnel is left alone: this user has nothing to stop. The caller holds
// tunnelOpLock.
func (s *ManagerService) stopOwnTunnel() error {
	// Read the name before stopping: tunnel.StopTunnel clears it.
	name := tunnel.GetTunnelName()
	if name == "" {
		name = tunnel.DefaultTunnelName
	}
	owner, owned := runningTunnelOwner(name)
	if owned && owner.userKey != s.clientUserKey {
		logger.Info("IPC server: StopTunnel from %s leaves the tunnel of %s running", s.clientUserKey, owner.userKey)
		// The caller has no tunnel; make sure its UI doesn't show one.
		notifyTunnelState(s.clientUserKey, tunnel.StateStopped)
		return nil
	}
	setTunnelCallbacks(s.clientUserKey)
	err := tunnel.StopTunnel()
	if owned && (err == nil || !tunnelIsRunning(name)) {
		forgetTunnelOwner(name, owner)
	}
	if err != nil {
		return err
	}
	notifyTunnelState(s.clientUserKey, tunnel.GetState())
	return nil
}

// stopOwnedTunnels stops the recorded tunnels whose owner matches and tells
// that owner's UIs. The caller holds tunnelOpLock.
func stopOwnedTunnels(match func(tunnelOwner) bool, reason string) {
	// Pick up the owner of a tunnel that outlived a manager restart.
	runningTunnelOwner(tunnel.DefaultTunnelName)
	activeTunnelsLock.RLock()
	owned := make(map[string]tunnelOwner)
	for name, owner := range activeTunnels {
		if match(owner) {
			owned[name] = owner
		}
	}
	activeTunnelsLock.RUnlock()

	for name, owner := range owned {
		logger.Info("%s, stopping tunnel: %s", reason, name)
		// As tunnel.StopTunnel does, so the owner's UI doesn't take the
		// stop for a lost connection while it waits.
		if tunnel.GetTunnelName() == name {
			tunnel.SetState(tunnel.StateStopping)
		}
		notifyTunnelState(owner.userKey, tunnel.StateStopping)
		if err := uninstallTunnel(name); err != nil {
			logger.Error("Failed to stop tunnel %s: %v", name, err)
			continue
		}
		forgetTunnelOwner(name, owner)
		if tunnel.GetTunnelName() == name {
			tunnel.SetState(tunnel.StateStopped)
		}
		notifyTunnelState(owner.userKey, tunnel.StateStopped)
	}
}

// authorizeUpdate decides whether this client may start an update install,
// which restarts the manager and drops every user's tunnel. When nobody else
// is affected (no other user's tunnel runs, no other user is signed in;
// Linux: has a UI attached), administrators may, and so may anyone else
// where updateWithoutAdmin allows it (Linux: a staged update only, as the
// package install needs an administrator otherwise). When others are
// affected, only an administrator whose consent is more than group
// membership may (adminOverridesOthers: Windows elevated token, Linux root).
func (s *ManagerService) authorizeUpdate() error {
	updateLock.Lock()
	staged := stagedUpdate != nil
	updateLock.Unlock()
	owner, running := runningTunnelOwner(tunnel.DefaultTunnelName)
	othersAffected := (running && owner.userKey != s.clientUserKey) || otherUsersPresent(s)
	switch {
	case s.elevatedToken != 0 && (!othersAffected || adminOverridesOthers(s)):
		return nil
	case s.elevatedToken == 0 && !othersAffected && updateWithoutAdmin(s, staged):
		return nil
	}
	logger.Error("IPC server: Update refused for %s (admin=%v, others affected=%v, staged=%v)",
		s.clientUserKey, s.elevatedToken != 0, othersAffected, staged)
	return errUpdateNeedsAdmin
}

// otherSessionsTunnelRunning reports whether a tunnel started from another
// session than session is running, for the staged installs at UI quit and
// launch, which would stop it.
func otherSessionsTunnelRunning(session uint32) bool {
	owner, ok := runningTunnelOwner(tunnel.DefaultTunnelName)
	if ok && owner.session != session {
		logger.Info("Not installing the staged update now: session %d's tunnel is running", owner.session)
		return true
	}
	return false
}

// mayAdoptTunnel reports whether this client's UI may adopt the running
// tunnel: not when another user started it.
func (s *ManagerService) mayAdoptTunnel() bool {
	tunnelOpLock.Lock()
	defer tunnelOpLock.Unlock()
	owner, ok := runningTunnelOwner(tunnel.DefaultTunnelName)
	return !ok || owner.userKey == s.clientUserKey
}

// loadTunnelOwners reads back the owner of a tunnel that kept running while
// the manager restarted (Windows), so sign-out and updates still find it.
func loadTunnelOwners() {
	tunnelOpLock.Lock()
	defer tunnelOpLock.Unlock()
	runningTunnelOwner(tunnel.DefaultTunnelName)
}

// stopTunnelsForUpdate stops every tunnel before an update install starts,
// so its download doesn't run through a tunnel about to go away, and tells
// each owner.
func stopTunnelsForUpdate() {
	tunnelOpLock.Lock()
	defer tunnelOpLock.Unlock()
	stopOwnedTunnels(func(tunnelOwner) bool { return true }, "Update requested")
	// A tunnel with no known owner (started by an older version before the
	// manager restarted) is stopped too, as the UI used to stop it.
	if tunnelIsRunning(tunnel.DefaultTunnelName) {
		if _, owned := runningTunnelOwner(tunnel.DefaultTunnelName); !owned {
			logger.Info("Update requested, stopping tunnel with no recorded owner: %s", tunnel.DefaultTunnelName)
			notifyTunnelState("", tunnel.StateStopping)
			if err := uninstallTunnel(tunnel.DefaultTunnelName); err != nil {
				logger.Error("Failed to stop tunnel %s: %v", tunnel.DefaultTunnelName, err)
				return
			}
			tunnel.SetState(tunnel.StateStopped)
			notifyTunnelState("", tunnel.StateStopped)
		}
	}
}

// notifyTunnelState tells the UIs of ownerKey's user about their tunnel.
// Other users' UIs don't see it: it isn't theirs to show or control.
func notifyTunnelState(ownerKey string, state TunnelState) {
	if ownerKey == "" {
		IPCServerNotifyTunnelStateChange(state)
		return
	}
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if encoder.Encode(TunnelStateChangeNotificationType) != nil || encoder.Encode(state) != nil {
		return
	}
	managerServicesLock.RLock()
	defer managerServicesLock.RUnlock()
	for m := range managerServices {
		if m.clientUserKey != ownerKey {
			continue
		}
		go func(m *ManagerService) {
			m.eventLock.Lock()
			defer m.eventLock.Unlock()
			if m.events != nil {
				m.events.SetWriteDeadline(time.Now().Add(time.Second))
				m.events.Write(buf.Bytes())
			}
		}(m)
	}
}
