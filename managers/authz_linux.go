package managers

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/fosrl/newt/logger"

	"github.com/fosrl/windows-linux/tunnel"
)

// connectAction is the polkit action asked for when a user without a local
// sign-in (an SSH-only user, a service account) wants to use the tunnel.
const connectAction = "net.pangolin.client.connect"

// errUpdateNeedsAdmin is returned when a non-administrator asks to download
// and install an update.
var errUpdateNeedsAdmin = errors.New("installing updates needs an administrator: a member of the sudo or wheel group")

// loginctl runs loginctl; tests replace it.
var loginctl = func(args ...string) ([]byte, error) {
	return exec.Command("loginctl", args...).Output()
}

// logindRunning reports whether systemd-logind (or elogind) tracks sessions.
var logindRunning = func() bool {
	_, err := os.Stat("/run/systemd/seats")
	return err == nil
}

// loginDefsPath is read for UID_MIN; tests replace it.
var loginDefsPath = "/etc/login.defs"

// authorizeTunnelUse decides whether the connected user may bring up a
// tunnel, which changes routes and DNS for the whole machine, or read the
// device data the daemon gathered as root. Like NetworkManager, it lets the
// people at the computer do it without asking: users signed in on a local
// seat (active or switched away) and root. Anyone else (signed in over SSH
// only, a system account) is asked for an administrator's password through
// polkit; sudo/wheel membership alone is not consent (audit #101). Without
// logind, sessions can't be told apart, so regular login accounts (UID_MIN
// and up) are allowed as before.
func authorizeTunnelUse(s *ManagerService) error {
	uid, err := strconv.ParseUint(s.clientUserKey, 10, 32)
	if err != nil {
		return errors.New("unknown user")
	}
	if uid == 0 {
		return nil
	}
	if !logindRunning() {
		if uid >= uidMin() && uid != 65534 {
			return nil
		}
	} else if hasLocalSession(uint32(uid)) {
		return nil
	}
	attachedUIsLock.Lock()
	ui := attachedUIs[uint32(uid)]
	attachedUIsLock.Unlock()
	if ui == nil || ui.subject == "" {
		return errors.New("connecting needs a local sign-in or an administrator")
	}
	if err := polkitAuthorize(connectAction, ui.subject); err != nil {
		return err
	}
	logger.Info("Tunnel use authorized by polkit for uid %d", uid)
	return nil
}

// uidMin is the first UID of regular login accounts (UID_MIN in login.defs).
func uidMin() uint64 {
	f, err := os.Open(loginDefsPath)
	if err != nil {
		return 1000
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "UID_MIN" {
			if v, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
				return v
			}
		}
	}
	return 1000
}

// hasLocalSession reports whether uid has a session on a local seat: not
// remote and attached to a seat. Greeter and lock-screen sessions don't
// count, nor do sessions in the middle of closing.
func hasLocalSession(uid uint32) bool {
	out, err := loginctl("show-user", strconv.FormatUint(uint64(uid), 10), "--property=Sessions", "--value")
	if err != nil {
		return false
	}
	for _, id := range strings.Fields(string(out)) {
		props, err := loginctl("show-session", id, "--property=Remote", "--property=Seat",
			"--property=Class", "--property=State")
		if err != nil {
			continue
		}
		if localUserSession(parseProperties(string(props))) {
			return true
		}
	}
	return false
}

func localUserSession(p map[string]string) bool {
	return p["Remote"] == "no" && p["Seat"] != "" && p["Class"] == "user" && p["State"] != "closing"
}

// parseProperties parses loginctl's "Name=value" lines.
func parseProperties(out string) map[string]string {
	props := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[name] = value
		}
	}
	return props
}

// signedOut reports whether logind says uid has no session left. It is
// false when that can't be told (no logind), so nothing is stopped on a
// guess.
func signedOut(uid uint32) bool {
	if !logindRunning() {
		return false
	}
	out, err := loginctl("show-user", strconv.FormatUint(uint64(uid), 10), "--property=State", "--value")
	if err != nil {
		var exitErr *exec.ExitError
		// loginctl exits non-zero for a user that is neither signed in nor lingering.
		return errors.As(err, &exitErr)
	}
	switch strings.TrimSpace(string(out)) {
	case "active", "online":
		return false
	}
	// offline, lingering (no session, only the user manager) or closing.
	return true
}

// ownerIsLocal reports whether the client's user is signed in on a local
// seat, which decides how their sign-out is detected (ownerGone).
func ownerIsLocal(s *ManagerService) bool {
	uid, err := strconv.ParseUint(s.clientUserKey, 10, 32)
	return err == nil && uid != 0 && logindRunning() && hasLocalSession(uint32(uid))
}

// ownerGone reports whether a tunnel's owner has signed out: for an owner
// who connected from a local seat, once no local seat session is left (an
// SSH session that stays open doesn't keep the tunnel for the next person
// at the computer); for others, once logind lists no session at all.
// Without logind it is never true.
func ownerGone(owner tunnelOwner) bool {
	uid, err := strconv.ParseUint(owner.userKey, 10, 32)
	if err != nil || uid == 0 || !logindRunning() {
		return false
	}
	if owner.local {
		return !hasLocalSession(uint32(uid))
	}
	return signedOut(uint32(uid))
}

// ownerCheckInterval is how often watchTunnelOwners looks for owners who
// signed out. A tunnel stops after two checks in a row agree.
var ownerCheckInterval = 5 * time.Second

// watchTunnelOwners stops a user's tunnels once they sign out, as the
// Windows service does on logoff, so a root tunnel doesn't stay up with
// their credentials, routes and DNS for the login screen and the next
// user. It runs for the life of the daemon and doesn't depend on the UI.
func watchTunnelOwners() {
	gone := make(map[tunnelOwner]int)
	for range time.Tick(ownerCheckInterval) {
		checkTunnelOwners(gone)
	}
}

// checkTunnelOwners is one round of watchTunnelOwners. gone counts how many
// rounds in a row each owner has been seen signed out.
func checkTunnelOwners(gone map[tunnelOwner]int) {
	activeTunnelsLock.RLock()
	owners := make(map[tunnelOwner]bool)
	for _, owner := range activeTunnels {
		owners[owner] = true
	}
	activeTunnelsLock.RUnlock()
	for owner := range gone {
		if !owners[owner] {
			delete(gone, owner)
		}
	}
	for owner := range owners {
		if !ownerGone(owner) {
			delete(gone, owner)
			continue
		}
		gone[owner]++
		if gone[owner] < 2 {
			continue
		}
		delete(gone, owner)
		logger.Info("uid %s signed out; stopping their tunnels", owner.userKey)
		stopSessionTunnels(owner.session)
	}
}

// prepareTunnelAccess lets the owner, and no other user, reach the OLM API
// of the tunnel about to start (their UI and CLI talk to it directly).
func prepareTunnelAccess(owner tunnelOwner) error {
	uid, err := strconv.ParseUint(owner.userKey, 10, 32)
	if err != nil {
		return errors.New("unknown user")
	}
	return grantOLMSocketAccess(uint32(uid))
}

// grantOLMSocketAccess is tunnel.GrantOLMSocketAccess; tests replace it.
var grantOLMSocketAccess = tunnel.GrantOLMSocketAccess

// updateWithoutAdmin lets a non-administrator install a staged update; the
// package install needs an administrator otherwise.
func updateWithoutAdmin(s *ManagerService, staged bool) bool { return staged }

// otherUsersPresent reports whether another user has a UI attached.
func otherUsersPresent(s *ManagerService) bool {
	attachedUIsLock.Lock()
	defer attachedUIsLock.Unlock()
	for uid := range attachedUIs {
		if strconv.FormatUint(uint64(uid), 10) != s.clientUserKey {
			return true
		}
	}
	return false
}

// adminOverridesOthers: a sudo/wheel member's token is only group membership,
// not consent (audit #101), so only root may update over other users.
func adminOverridesOthers(s *ManagerService) bool { return s.clientUserKey == "0" }

// quitAllowed: Quit stops every user's tunnels; on Linux only root may, not
// any sudo/wheel member without a password (audit #101).
func quitAllowed(s *ManagerService) bool { return s.clientUserKey == "0" }

// tunnelRunning reports whether the tunnel process for name is running.
func tunnelRunning(name string) bool {
	tunnelProcsLock.Lock()
	defer tunnelProcsLock.Unlock()
	return tunnelProcs[name] != nil
}

// recordTunnelOwner has nothing to persist on Linux: tunnels are child
// processes and stop with the daemon.
func recordTunnelOwner(name string, owner tunnelOwner) {}

func recoverTunnelOwner(name string) (tunnelOwner, bool) { return tunnelOwner{}, false }
