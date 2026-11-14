package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/config"
)

// DeviceNameSyncStatus is the outcome of renaming this device on the server.
type DeviceNameSyncStatus string

const (
	// DeviceNameRenamed: the device has the new name everywhere it was found.
	DeviceNameRenamed DeviceNameSyncStatus = "renamed"
	// DeviceNamePartial: renamed in some orgs; the user's role may not rename
	// devices in (or the rename failed for) the others.
	DeviceNamePartial DeviceNameSyncStatus = "partial"
	// DeviceNamePermissionDenied: the server refused, because the user's role
	// lacks the updateClient permission. Only administrators rename devices.
	DeviceNamePermissionDenied DeviceNameSyncStatus = "permission_denied"
	// DeviceNameFailed: the rename failed (network, server error, ...).
	DeviceNameFailed DeviceNameSyncStatus = "failed"
	// DeviceNameSkipped: nothing to rename yet (not signed in, or this device
	// isn't registered); the name is used when it is.
	DeviceNameSkipped DeviceNameSyncStatus = "skipped"
)

// errNoClientFound: none of the device's clients could be found (or, for an
// automatic rename, none still had a generic name).
var errNoClientFound = errors.New("no client of this device was found on the server")

// errClientNotIdentified: a client with the device's name was found, but it
// may be another of the user's devices, so it was left alone.
var errClientNotIdentified = errors.New("this device's client couldn't be told apart from the user's other devices")

// DeviceNameSyncResult summarizes a rename for the UI and the logs.
type DeviceNameSyncResult struct {
	Status DeviceNameSyncStatus
	Name   string
	// Renamed, Denied, Failed and Unsure count organizations: the device was
	// renamed there, the user's role may not rename (or list) devices there,
	// renaming failed for another reason, or a client with the device's name
	// was found that may be another of the user's devices (so it was left
	// alone). Organizations in which no client of this device was found are
	// not counted.
	Renamed int
	Denied  int
	Failed  int
	Unsure  int
	// Err is the last error, if any.
	Err error
}

// Message describes the result for the Preferences window.
func (r DeviceNameSyncResult) Message() string {
	switch r.Status {
	case DeviceNameRenamed:
		return "Renamed on the server."
	case DeviceNamePartial:
		if r.Unsure > 0 {
			return r.partialWithUnsure()
		}
		total := r.Renamed + r.Denied + r.Failed
		switch {
		case r.Failed == 0:
			return fmt.Sprintf("Renamed in %d of %d organizations. In the others, renaming devices needs an administrator (the updateClient permission).",
				r.Renamed, total)
		case r.Denied == 0:
			return fmt.Sprintf("Renamed in %d of %d organizations. Renaming failed in the others: %v",
				r.Renamed, total, r.Err)
		default:
			return fmt.Sprintf("Renamed in %d of %d organizations. Renaming devices needs an administrator (the updateClient permission) in %d; renaming failed in %d: %v",
				r.Renamed, total, r.Denied, r.Failed, r.Err)
		}
	case DeviceNamePermissionDenied:
		return "Saved. Renaming devices on the server needs an administrator (the updateClient permission); ask one to rename this device."
	case DeviceNameSkipped:
		return "Saved. The name is used when this device is next registered."
	default:
		if r.Err != nil {
			return fmt.Sprintf("Saved, but the device couldn't be renamed on the server: %v", r.Err)
		}
		return "Saved, but the device couldn't be renamed on the server."
	}
}

// partialWithUnsure is Message for a partial rename that left some orgs alone
// because this device's client there couldn't be identified.
func (r DeviceNameSyncResult) partialWithUnsure() string {
	msg := fmt.Sprintf("Renamed in %d of %d organizations.", r.Renamed, r.Renamed+r.Denied+r.Failed+r.Unsure)
	if r.Denied > 0 {
		msg += fmt.Sprintf(" Renaming devices needs an administrator (the updateClient permission) in %d.", r.Denied)
	}
	if r.Failed > 0 {
		msg += fmt.Sprintf(" Renaming failed in %d: %v.", r.Failed, r.Err)
	}
	return msg + fmt.Sprintf(" In %d, this device couldn't be told apart from your other devices; rename it there in the dashboard.", r.Unsure)
}

// transient reports whether the rename failed for a reason that may go away
// on its own (no answer from the server, a server error, or the session
// expiring), so an automatic rename should be retried later rather than
// recorded as done.
func (r DeviceNameSyncResult) transient() bool {
	if r.Status != DeviceNameFailed || r.Err == nil {
		return false
	}
	if errors.Is(r.Err, context.DeadlineExceeded) || errors.Is(r.Err, context.Canceled) {
		return true
	}
	var apiErr *api.APIError
	if !errors.As(r.Err, &apiErr) {
		return false
	}
	switch apiErr.Type {
	case api.ErrorTypeNetworkError:
		return true
	case api.ErrorTypeHTTPError:
		// Status 0 is a timeout before any answer.
		switch s := apiErr.Status; {
		case s == 0, s >= 500, s == 429, s == 401, s == 408:
			return true
		}
	}
	return false
}

// SyncDeviceName renames this device on the server for the signed-in user:
// its client in each org it can find (see renameDeviceClients). It is best
// effort, and only works for roles with the updateClient permission. It stops
// a background rename that is still running, so that can't overwrite name.
func (am *AuthManager) SyncDeviceName(ctx context.Context, name string) DeviceNameSyncResult {
	name = config.NormalizeDeviceName(name)
	if name == "" {
		name = defaultDeviceName()
	}

	am.cancelDeviceNameMigration()
	am.deviceNameRenaming.Lock()
	defer am.deviceNameRenaming.Unlock()

	am.mu.RLock()
	userID := ""
	if am.currentUser != nil {
		userID = am.currentUser.UserId
	}
	am.mu.RUnlock()
	if userID == "" {
		return DeviceNameSyncResult{Status: DeviceNameSkipped, Name: name}
	}
	olmID, found := am.secretManager.GetOlmId(userID)
	if !found || olmID == "" {
		return DeviceNameSyncResult{Status: DeviceNameSkipped, Name: name}
	}

	result := syncDeviceName(ctx, am.apiClient, userID, olmID, name)
	logDeviceNameSync(olmID, result)
	return result
}

func logDeviceNameSync(olmID string, r DeviceNameSyncResult) {
	switch r.Status {
	case DeviceNameRenamed:
		logger.Info("Device name: renamed olm %s to %q (orgs=%d)", olmID, r.Name, r.Renamed)
	case DeviceNameSkipped:
		logger.Debug("Device name: nothing to rename yet")
	default:
		logger.Warn("Device name: renaming olm %s to %q: %s (renamed=%d, denied=%d, failed=%d): %v",
			olmID, r.Name, r.Status, r.Renamed, r.Denied, r.Failed, r.Err)
	}
}

// syncDeviceName renames olmID of userID to name on the server.
func syncDeviceName(ctx context.Context, client *api.APIClient, userID, olmID, name string) DeviceNameSyncResult {
	myDevice, err := client.GetMyDeviceContext(ctx, olmID)
	if err != nil {
		return DeviceNameSyncResult{Status: DeviceNameFailed, Name: name, Err: err}
	}
	return renameDeviceClients(ctx, client, userID, olmID, myDevice, name, nil)
}

// orgOutcome is what renaming the device did in one org.
type orgOutcome int

const (
	orgNothingFound orgOutcome = iota
	orgRenamed
	orgDenied
	orgFailed
	orgUnsure
)

func outcomeOf(err error) orgOutcome {
	switch {
	case err == nil:
		return orgRenamed
	case api.IsForbidden(err):
		return orgDenied
	default:
		return orgFailed
	}
}

// renameDeviceClients renames the device olmID, which myDevice describes (as
// GET /my-device returned it), to name: its client in each of the user's
// orgs, with POST /client/:clientId. That needs the updateClient permission
// (and listing the org's devices needs listClients), which only administrators
// have. If onlyFrom is set, only clients whose current name is in it are
// renamed.
func renameDeviceClients(ctx context.Context, client *api.APIClient, userID, olmID string, myDevice *api.MyDeviceResponse, name string, onlyFrom []string) DeviceNameSyncResult {
	result := DeviceNameSyncResult{Name: name}
	fail := func(err error) DeviceNameSyncResult {
		result.Status, result.Err = DeviceNameFailed, err
		return result
	}
	allowed := func(current string) bool {
		return onlyFrom == nil || slices.Contains(onlyFrom, current)
	}

	if myDevice == nil || myDevice.Olm == nil {
		return fail(errors.New("this device isn't registered on the server"))
	}
	oldName := ""
	if myDevice.Olm.Name != nil {
		oldName = *myDevice.Olm.Name
	}

	// The client the olm last connected through is certainly this device's.
	knownID := 0
	if id := myDevice.Olm.ClientId; id != nil && *id > 0 {
		knownID = *id
	}
	knownOrg, knownSeen, knownKept := "", false, true

	// outcomes is per org; targets maps each client to rename to its org.
	outcomes := map[string]orgOutcome{}
	targets := map[int]string{}
	var order []int

	// The server links the olm only to the client it last connected through;
	// nothing it returns links the olm to its clients in the other orgs. So
	// there a client is taken as this device's only if it has the device's old
	// name and no other device of the user can own it: every other device is
	// accounted for in that org by the client it last connected through (an
	// org holds one client per device). Otherwise the org is left alone.
	others, othersKnown := otherDeviceClients(ctx, client, userID, olmID)
	var listFailed []string
	for _, org := range myDevice.Orgs {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		devices, err := client.ListUserDevices(ctx, org.OrgId, userID)
		if err != nil {
			outcomes[org.OrgId] = outcomeOf(err)
			listFailed = append(listFailed, org.OrgId)
			result.Err = err
			continue
		}
		if i := slices.IndexFunc(devices, func(d api.UserDevice) bool { return knownID != 0 && d.ClientId == knownID }); i >= 0 {
			// An org has one client per device, so there is nothing more to
			// find in it.
			knownOrg, knownSeen = org.OrgId, true
			if d := devices[i]; !allowed(d.Name) {
				logger.Info("Device name: client %d in org %s is named %q, not renaming it", d.ClientId, org.OrgId, d.Name)
				knownKept = false
			}
			continue
		}
		var candidates []int
		listed := map[int]bool{}
		for _, d := range devices {
			listed[d.ClientId] = true
			if oldName != "" && d.Name == oldName && allowed(d.Name) && !slices.Contains(others, d.ClientId) {
				candidates = append(candidates, d.ClientId)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		accounted := othersKnown
		for _, id := range others {
			accounted = accounted && listed[id]
		}
		if len(candidates) == 1 && accounted {
			targets[candidates[0]] = org.OrgId
			order = append(order, candidates[0])
			continue
		}
		logger.Warn("Device name: %d client(s) named %q in org %s may belong to another device of this user, not renaming any",
			len(candidates), oldName, org.OrgId)
		outcomes[org.OrgId] = orgUnsure
	}

	switch {
	case knownID == 0 || !knownKept:
		// none, or its current name isn't one to replace
	case knownSeen:
		targets[knownID] = knownOrg
		order = append([]int{knownID}, order...)
	case onlyFrom != nil:
		// Its name couldn't be checked, so leave it alone.
		logger.Info("Device name: could not check the name of client %d, not renaming it", knownID)
	default:
		// It is in one of the orgs that couldn't be listed (an org that was
		// listed would have shown it); any of them stands in for the count.
		org := "client:" + strconv.Itoa(knownID)
		if len(listFailed) > 0 {
			org = listFailed[0]
		}
		targets[knownID] = org
		order = append([]int{knownID}, order...)
	}

	for _, clientID := range order {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		err := client.UpdateClientName(ctx, clientID, name)
		// One client per device and org, so this decides the org's outcome.
		outcomes[targets[clientID]] = outcomeOf(err)
		if err != nil {
			result.Err = err
		}
	}

	for _, o := range outcomes {
		switch o {
		case orgRenamed:
			result.Renamed++
		case orgDenied:
			result.Denied++
		case orgFailed:
			result.Failed++
		case orgUnsure:
			result.Unsure++
		}
	}
	switch {
	case result.Renamed > 0 && result.Denied == 0 && result.Failed == 0 && result.Unsure == 0:
		result.Status = DeviceNameRenamed
		result.Err = nil
	case result.Renamed > 0:
		result.Status = DeviceNamePartial
	case result.Denied > 0:
		result.Status = DeviceNamePermissionDenied
	default:
		result.Status = DeviceNameFailed
		if result.Err == nil && result.Unsure > 0 {
			result.Err = errClientNotIdentified
		} else if result.Err == nil {
			result.Err = errNoClientFound
		}
	}
	return result
}

// otherDeviceClients returns, for each of the user's devices other than
// olmID, the client it last connected through. ok is false if that isn't
// known for all of them (the listing failed, was cut short, or a device has
// no such client).
func otherDeviceClients(ctx context.Context, client *api.APIClient, userID, olmID string) (clientIDs []int, ok bool) {
	olms, err := client.ListUserOlms(ctx, userID)
	if err != nil {
		logger.Warn("Device name: could not list this user's devices: %v", err)
		return nil, false
	}
	ok = true
	for _, o := range olms {
		if o.OlmId == olmID {
			continue
		}
		if o.ClientId == nil || *o.ClientId <= 0 {
			ok = false
			continue
		}
		clientIDs = append(clientIDs, *o.ClientId)
	}
	return clientIDs, ok
}

// shouldMigrateDeviceName decides whether to rename a device automatically
// from a generic name older clients gave it to target (the hostname). It only
// does so when the user hasn't chosen a name, the server still has a generic
// name (any other name may have been chosen by an administrator), the target
// differs, and the same rename wasn't already attempted.
func shouldMigrateDeviceName(override, serverName, target, attempted string) bool {
	switch {
	case override != "":
		return false
	case target == "" || serverName == target:
		return false
	case !config.IsLegacyDeviceName(serverName):
		return false
	case attempted == target:
		return false
	}
	return true
}

// migrateDeviceNameTimeout bounds an automatic rename.
const migrateDeviceNameTimeout = 2 * time.Minute

// defaultDeviceName is config.DefaultDeviceName; tests replace it.
var defaultDeviceName = config.DefaultDeviceName

// startDeviceNameMigration renames this device in the background if it still
// has a generic name from an older client (see shouldMigrateDeviceName). It
// never blocks connecting.
func (am *AuthManager) startDeviceNameMigration(userID string) {
	if !am.deviceNameMigrating.TryLock() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), migrateDeviceNameTimeout)
	am.cancelMigrationMu.Lock()
	am.cancelMigration = cancel
	am.cancelMigrationMu.Unlock()
	go func() {
		defer am.deviceNameMigrating.Unlock()
		defer func() {
			am.cancelMigrationMu.Lock()
			am.cancelMigration = nil
			am.cancelMigrationMu.Unlock()
			cancel()
		}()
		am.migrateDeviceName(ctx, userID)
	}()
}

// cancelDeviceNameMigration stops a running background rename, if any.
func (am *AuthManager) cancelDeviceNameMigration() {
	am.cancelMigrationMu.Lock()
	defer am.cancelMigrationMu.Unlock()
	if am.cancelMigration != nil {
		am.cancelMigration()
	}
}

func (am *AuthManager) migrateDeviceName(ctx context.Context, userID string) {
	if am.configManager.GetDeviceNameOverride() != "" {
		return
	}
	olmID, found := am.secretManager.GetOlmId(userID)
	if !found || olmID == "" {
		return
	}
	target := defaultDeviceName()
	attempted := am.accountManager.GetDeviceNameMigration(userID, olmID)
	// Don't ask the server when the outcome is already known.
	if target == attempted || config.IsLegacyDeviceName(target) {
		return
	}
	record := func() {
		if err := am.accountManager.SetDeviceNameMigration(userID, olmID, target); err != nil {
			logger.Warn("Device name: could not record the rename: %v", err)
		}
	}

	myDevice, err := am.apiClient.GetMyDeviceContext(ctx, olmID)
	if err != nil {
		logger.Debug("Device name: could not check the server's name for this device: %v", err)
		return
	}
	if myDevice.Olm == nil || myDevice.Olm.Name == nil {
		return
	}
	serverName := *myDevice.Olm.Name
	if !shouldMigrateDeviceName("", serverName, target, attempted) {
		// Already the hostname, or a name someone chose: nothing to do for
		// this hostname, so later starts needn't ask the server again.
		record()
		return
	}

	am.deviceNameRenaming.Lock()
	defer am.deviceNameRenaming.Unlock()
	// The user may have chosen a name while the server was being asked.
	if am.configManager.GetDeviceNameOverride() != "" || ctx.Err() != nil {
		return
	}

	logger.Info("Device name: renaming this device from the generic %q to %q", serverName, target)
	result := renameDeviceClients(ctx, am.apiClient, userID, olmID, myDevice, target, config.LegacyDeviceNames())
	logDeviceNameSync(olmID, result)
	if result.transient() {
		return // try again on a later start
	}
	record()
}
