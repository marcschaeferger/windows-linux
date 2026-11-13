package managers

import (
	"maps"

	"github.com/fosrl/windows-linux/fingerprint"
	"golang.org/x/sys/windows"
)

// userToken is the connected user's elevated token, or 0 if they are not an admin.
type userToken = windows.Token

// postureForClient reports the user who started the tunnel instead of
// LocalSystem, which gathered the cached posture. clientUserKey is the
// client's SID, resolved with LookupAccountSid to DOMAIN\user, the form
// user.Current reports. (os/user.LookupId would also query the domain
// controller for group and profile data, which can stall StartTunnel when it
// is unreachable, and fails for Microsoft Entra ID accounts.)
func postureForClient(snapshot fingerprint.DevicePostureSnapshot, clientUserKey string) fingerprint.DevicePostureSnapshot {
	if snapshot.Fingerprint == nil {
		return snapshot
	}
	sid, err := windows.StringToSid(clientUserKey)
	if err != nil {
		return snapshot
	}
	name, domain, _, err := sid.LookupAccount("")
	if err != nil || name == "" {
		return snapshot
	}
	username := name
	if domain != "" {
		username = domain + `\` + name
	}
	fp := maps.Clone(snapshot.Fingerprint)
	fp["username"] = username
	snapshot.Fingerprint = fp
	return snapshot
}

// prepareUpdateRestart is a no-op on Windows: the updater writes the
// restart-ui flag itself before running the MSI.
func prepareUpdateRestart() {}
