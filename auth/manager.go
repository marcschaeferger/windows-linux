package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/secrets"
	"github.com/fosrl/windows-linux/version"

	"github.com/fosrl/newt/logger"
)

// AuthError represents authentication-specific errors
type AuthError struct {
	Type AuthErrorType
}

type AuthErrorType int

const (
	AuthErrorTwoFactorRequired AuthErrorType = iota
	AuthErrorEmailVerificationRequired
	AuthErrorDeviceCodeExpired
	AuthErrorInvalidToken
)

func (e *AuthError) Error() string {
	switch e.Type {
	case AuthErrorTwoFactorRequired:
		return "Two-factor authentication code required"
	case AuthErrorEmailVerificationRequired:
		return "Email verification required"
	case AuthErrorDeviceCodeExpired:
		return "Device code expired. Please try again."
	case AuthErrorInvalidToken:
		return "Invalid session token"
	default:
		return "Authentication error"
	}
}

// AuthManager manages authentication state and operations
type AuthManager struct {
	apiClient      *api.APIClient
	configManager  *config.ConfigManager
	accountManager *config.AccountManager
	secretManager  *secrets.SecretManager

	// State
	mu                         sync.RWMutex
	isAuthenticated            bool
	currentUser                *api.User
	currentOrg                 *api.Org
	organizations              []api.Org
	isInitializing             bool
	errorMessage               *string
	deviceAuthCode             *string
	deviceAuthLoginURL         *string
	serverInfo                 *api.ServerInfo
	isServerDown               bool
	sessionExpired             bool
	isDeviceAuthInProgress     bool
	startDeviceAuthImmediately bool

	// sessionGen changes whenever the active session changes (login,
	// account switch, logout). Requests that run without the lock note it
	// first and drop their result if it changed, so a slow refresh for one
	// account can't overwrite the state of the next.
	sessionGen uint64

	// deviceAuthOwner is the device-login attempt allowed to set the device
	// code (0 when none); deviceAuthSeq numbers the attempts.
	deviceAuthOwner uint64
	deviceAuthSeq   uint64
}

// NewAuthManager creates a new AuthManager instance
func NewAuthManager(
	apiClient *api.APIClient,
	configManager *config.ConfigManager,
	accountManager *config.AccountManager,
	secretManager *secrets.SecretManager,
) *AuthManager {
	return &AuthManager{
		apiClient:      apiClient,
		configManager:  configManager,
		accountManager: accountManager,
		secretManager:  secretManager,
		isInitializing: true,
	}
}

// Initialize loads session token from secrets and verifies authentication
func (am *AuthManager) Initialize() error {
	am.mu.Lock()
	am.isInitializing = true
	am.mu.Unlock()

	defer func() {
		am.mu.Lock()
		am.isInitializing = false
		am.mu.Unlock()
	}()

	// Initialize also runs again while the UI is up (waitForServer). A login,
	// account switch or logout that happens meanwhile owns the session; this
	// run must not reset the client to the account it read before that.
	startGen := am.sessionGeneration()
	activeAccount, _ := am.accountManager.ActiveAccount()
	if activeAccount != nil {
		logger.Debug("Auth: active account found (userId=%s), loading session token", activeAccount.UserID)
		// Load session token from secrets store via manager IPC
		token, found := am.secretManager.GetSessionToken(activeAccount.UserID)
		if found && token != "" {
			am.mu.Lock()
			if am.sessionGen != startGen {
				am.mu.Unlock()
				logger.Debug("Auth: session changed while initializing; leaving it alone")
				return nil
			}
			am.sessionGen++
			gen := am.sessionGen
			am.apiClient.SetSession(activeAccount.Hostname, token)
			am.mu.Unlock()

			// Health check before fetching user/orgs
			_ = am.CheckHealthAndSetState()

			am.mu.RLock()
			serverDown := am.isServerDown
			am.mu.RUnlock()

			if serverDown {
				// Server is down, but keep authenticated state to show UI
				am.mu.Lock()
				if am.sessionGen == gen {
					am.isAuthenticated = true
				}
				am.mu.Unlock()
				return nil
			}

			// Always fetch the latest user info to verify the user exists and update stored info
			user, err := am.apiClient.GetUser()
			if err != nil {
				switch {
				case api.IsSessionRejected(err):
					// Session expired; keep user in logged-in UI and show re-auth
					am.mu.Lock()
					if am.sessionGen == gen {
						am.isAuthenticated = true
						am.markSessionExpiredLocked()
					}
					am.mu.Unlock()
				case api.IsTransient(err):
					// A timeout, rate limit or server error says nothing about the
					// stored session. Keep it and treat the server as down, so the
					// startup wait and the next tray open check again.
					logger.Warn("Auth: could not load the user, keeping the stored session: %v", err)
					am.mu.Lock()
					if am.sessionGen == gen {
						am.isAuthenticated = true
						am.isServerDown = true
						msg := fmt.Sprintf("Could not load your account from the server: %v", err)
						am.errorMessage = &msg
						am.restoreCachedOrganizationLocked(activeAccount)
					}
					am.mu.Unlock()
				default:
					// The server answered that the user is gone (e.g. 404).
					am.mu.Lock()
					if am.sessionGen == gen {
						am.isAuthenticated = false
					}
					am.mu.Unlock()
				}
				return nil // Not an error, just not (yet) verified
			}

			// Update account info with user data
			var username string
			if user.Username != nil {
				username = *user.Username
			}
			var name string
			if user.Name != nil {
				name = *user.Name
			}
			_ = am.accountManager.UpdateAccountUserInfo(activeAccount.UserID, username, name)

			// Update stored config with latest user info
			err = am.activateSession(gen, nil, am.apiClient, user, token, false)
			if errors.Is(err, ErrSessionChanged) {
				return nil
			}
			return err
		}
		logger.Debug("Auth: no session token for active account")
	} else {
		logger.Debug("Auth: no active account")
	}

	am.mu.Lock()
	if am.sessionGen == startGen {
		am.isAuthenticated = false
	}
	am.mu.Unlock()
	return nil
}

// ErrSessionChanged reports that the active session changed (login, account
// switch or logout) while a request ran, so its result was dropped.
var ErrSessionChanged = errors.New("the active account changed")

// beginDeviceAuth makes a new device-login attempt the one whose code the UI
// shows, replacing any earlier attempt, and returns its ID. An attempt whose
// ctx is already cancelled gets none: the UI cancels an attempt before it
// starts the next, so a superseded attempt that is scheduled late can't take
// ownership from its replacement.
func (am *AuthManager) beginDeviceAuth(ctx context.Context) (uint64, error) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	am.deviceAuthSeq++
	am.deviceAuthOwner = am.deviceAuthSeq
	am.deviceAuthCode = nil
	am.deviceAuthLoginURL = nil
	am.isDeviceAuthInProgress = true
	return am.deviceAuthSeq, nil
}

// setDeviceAuth sets the device code and login URL shown to the user, if the
// attempt is still the current one. A superseded attempt must not replace or
// clear the newer attempt's code.
func (am *AuthManager) setDeviceAuth(attempt uint64, code, loginURL *string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.deviceAuthOwner != attempt {
		return
	}
	am.deviceAuthCode = code
	am.deviceAuthLoginURL = loginURL
}

// endDeviceAuth clears the in-progress flag, if the attempt is still the current one.
func (am *AuthManager) endDeviceAuth(attempt uint64) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.deviceAuthOwner != attempt {
		return
	}
	am.deviceAuthOwner = 0
	am.isDeviceAuthInProgress = false
}

// LoginWithDeviceAuth authenticates using device authentication flow.
// Cancelling ctx aborts the attempt, including a request in flight.
//
// Everything runs on a separate client until the new session is verified and
// stored; only then does it replace the active account's client and state, so
// a failed login leaves the active account as it was.
func (am *AuthManager) LoginWithDeviceAuth(ctx context.Context, hostnameOverride *string) error {
	attempt, err := am.beginDeviceAuth(ctx)
	if err != nil {
		return err
	}
	defer am.endDeviceAuth(attempt)

	baseURL := am.apiClient.CurrentBaseURL()
	if hostnameOverride != nil && *hostnameOverride != "" {
		baseURL = *hostnameOverride
	}
	loginClient := api.NewAPIClient(baseURL, "")
	loginClient.SetSessionCookieName(am.configManager.GetSessionCookieName())

	// Get friendly device name (e.g., "Windows Laptop" or "Windows Desktop")
	deviceName := config.GetFriendlyDeviceName()

	// Start device auth
	startResponse, err := loginClient.StartDeviceAuthContext(ctx, "Pangolin "+version.OSDisplayName+" Client", &deviceName)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// Store code and URL for UI display
	code := startResponse.Code
	loginURL := fmt.Sprintf("%s/auth/login/device", loginClient.CurrentBaseURL())
	am.setDeviceAuth(attempt, &code, &loginURL)

	// Poll for verification
	expiresAt := time.Now().Add(time.Duration(startResponse.ExpiresInSeconds) * time.Second)
	verified := false
	var sessionToken *string

	// Poll every second, like the macOS client, so approval shows up promptly.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for !verified && time.Now().Before(expiresAt) {
		select {
		case <-ctx.Done():
			// Context canceled, clear state and return
			am.setDeviceAuth(attempt, nil, nil)
			return ctx.Err()
		case <-ticker.C:
			pollResponse, token, err := loginClient.PollDeviceAuthContext(ctx, code)
			if err != nil {
				// Continue polling on error
				continue
			}

			if pollResponse.Verified {
				verified = true
				if token != nil {
					sessionToken = token
				}
			} else if pollResponse.Message != nil {
				message := *pollResponse.Message
				if contains(message, "expired") || contains(message, "not found") {
					am.setDeviceAuth(attempt, nil, nil)
					return &AuthError{Type: AuthErrorDeviceCodeExpired}
				}
			}
		}
	}

	if !verified {
		am.setDeviceAuth(attempt, nil, nil)
		return &AuthError{Type: AuthErrorDeviceCodeExpired}
	}

	if sessionToken == nil {
		return &AuthError{Type: AuthErrorInvalidToken}
	}

	// Verify the new session on the login client, not the active account's.
	loginClient.UpdateSessionToken(*sessionToken)
	user, err := loginClient.GetUserContext(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		am.setDeviceAuth(attempt, nil, nil)
		return ctxErr
	}
	if err != nil {
		return err
	}

	// Clear device auth UI state after successful auth
	am.setDeviceAuth(attempt, nil, nil)

	// Commit only while this attempt is still the current one and not
	// cancelled; both are checked under am.mu together with the commit.
	stillCurrent := func() bool { return ctx.Err() == nil && am.deviceAuthOwner == attempt }
	if err := am.activateSession(0, stillCurrent, loginClient, user, *sessionToken, true); err != nil {
		if errors.Is(err, ErrSessionChanged) && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// loadOrganizations lists userID's organizations on client and picks the one
// to select: the account's remembered organization if it still exists,
// otherwise the first one.
func loadOrganizations(client *api.APIClient, userID string, account *config.Account) ([]api.Org, *api.Org, error) {
	orgsResponse, err := client.ListUserOrgs(userID)
	if err != nil {
		return []api.Org{}, nil, err
	}
	orgs := orgsResponse.Orgs
	if orgs == nil {
		orgs = []api.Org{}
	}
	if account != nil && account.OrgID != "" {
		for i := range orgs {
			if orgs[i].Id == account.OrgID {
				org := orgs[i]
				return orgs, &org, nil
			}
		}
	}
	if len(orgs) > 0 {
		org := orgs[0]
		return orgs, &org, nil
	}
	return orgs, nil, nil
}

// activateSession makes a verified session the active one: it loads the
// user's organizations through client, stores the token (when saveToken is
// set), and only then switches the shared API client, the in-memory state and
// the account store over to it in one step.
//
// gen is the session generation the caller started from, or 0 for a new
// login. When the session changed since then (another login, an account
// switch or a logout), nothing is applied and ErrSessionChanged is returned.
// stillCurrent, if set, is checked with am.mu held right before the commit;
// returning false also drops the session with ErrSessionChanged.
func (am *AuthManager) activateSession(gen uint64, stillCurrent func() bool, client *api.APIClient, user *api.User, token string, saveToken bool) error {
	// Ensure userId is set (map from Id if needed)
	if user.UserId == "" {
		user.UserId = user.Id
	}
	hostname := client.CurrentBaseURL()

	// Check if account already exists to use its stored org ID
	var existingAccount *config.Account
	if existing, ok := am.accountManager.Account(user.UserId); ok {
		existingAccount = &existing
	}

	orgs, selectedOrg, orgErr := loadOrganizations(client, user.UserId, existingAccount)
	if orgErr != nil {
		// Non-fatal error, continue without the org list
		logger.Error("Failed to load organizations: %v", orgErr)
	}

	if saveToken && !am.secretManager.SaveSessionToken(user.UserId, token) {
		logger.Error("Auth: SaveSessionToken() failed (userId=%s)", user.UserId)
		return fmt.Errorf("failed to save session token")
	}

	var username string
	if user.Username != nil {
		username = *user.Username
	}

	var name string
	if user.Name != nil {
		name = *user.Name
	}

	newAccount := config.Account{
		UserID:   user.UserId,
		Email:    user.Email,
		Username: username,
		Name:     name,
		Hostname: hostname,
	}
	switch {
	case selectedOrg != nil:
		newAccount.OrgID = selectedOrg.Id
	case orgErr != nil && existingAccount != nil:
		// Keep the remembered organization when the list could not be loaded.
		newAccount.OrgID = existingAccount.OrgID
	}

	am.mu.Lock()
	if (gen != 0 && am.sessionGen != gen) || (stillCurrent != nil && !stillCurrent()) {
		am.mu.Unlock()
		if saveToken && existingAccount == nil {
			// Don't leave a token behind for an account that was never added,
			// unless another login added it meanwhile with its own token.
			if _, added := am.accountManager.Account(user.UserId); !added {
				if stored, ok := am.secretManager.GetSessionToken(user.UserId); ok && stored == token {
					_ = am.secretManager.DeleteSessionToken(user.UserId)
				}
			}
		}
		return ErrSessionChanged
	}
	am.sessionGen++
	gen = am.sessionGen
	am.apiClient.SetSession(hostname, token)
	am.currentUser = user
	am.organizations = orgs
	am.currentOrg = selectedOrg
	am.serverInfo = nil
	am.isServerDown = false
	am.errorMessage = nil
	_ = am.accountManager.AddAccount(newAccount)
	_ = am.accountManager.SetActiveUser(user.UserId)
	if am.currentOrg == nil {
		am.restoreCachedOrganizationLocked(&newAccount)
	}
	am.isAuthenticated = true
	am.sessionExpired = false
	am.startDeviceAuthImmediately = false
	am.mu.Unlock()

	// Fetch server info after successful authentication
	_ = am.fetchServerInfo(gen)

	return nil
}

// MarkSessionExpired sets the session-expired state so the UI shows re-auth and disables connect.
// Called from the API layer (on 401/403) and tunnel layer (on session-expired error codes).
// The last selected org stays visible from the locally cached account org id.
// It does nothing when no session is active, e.g. after logging out.
func (am *AuthManager) MarkSessionExpired() {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.markSessionExpiredLocked()
}

// MarkSessionExpiredFor marks the session expired only if the API client
// still uses token, the session token a rejected request was sent with. The
// API client's 401 callback runs it asynchronously; by then the user may have
// switched accounts, and the new account's session is not expired.
func (am *AuthManager) MarkSessionExpiredFor(token string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if token == "" || am.apiClient.SessionToken() != token {
		return
	}
	am.markSessionExpiredLocked()
}

// markSessionExpiredIf marks the session expired if it is still generation gen.
func (am *AuthManager) markSessionExpiredIf(gen uint64) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.sessionGen != gen {
		return
	}
	am.markSessionExpiredLocked()
}

func (am *AuthManager) markSessionExpiredLocked() {
	if !am.isAuthenticated {
		return
	}
	am.sessionExpired = true
	am.errorMessage = nil
	if account, err := am.accountManager.ActiveAccount(); err == nil {
		am.restoreCachedOrganizationLocked(account)
	}
}

// restoreCachedOrganizationLocked sets currentOrg from the locally cached account org ID
// when the server is unreachable or the session is expired. Keeps a known display name
// when the id already matches. Caller must hold am.mu.
func (am *AuthManager) restoreCachedOrganizationLocked(account *config.Account) {
	if account == nil || account.OrgID == "" {
		return
	}
	if am.currentOrg == nil || am.currentOrg.Id != account.OrgID {
		am.currentOrg = &api.Org{Id: account.OrgID, Name: account.OrgID}
	}
	if am.currentOrg == nil || am.currentOrg.Id != account.OrgID {
		return
	}
	for _, org := range am.organizations {
		if org.Id == account.OrgID {
			return
		}
	}
	am.organizations = append(am.organizations, *am.currentOrg)
}

// RefreshOrganizations refreshes the list of organizations
func (am *AuthManager) RefreshOrganizations() error {
	am.mu.RLock()
	authenticated := am.isAuthenticated
	gen := am.sessionGen
	userId := ""
	if am.currentUser != nil {
		userId = am.currentUser.UserId
	}
	am.mu.RUnlock()

	// Only refresh if authenticated and user ID is available
	if !authenticated || userId == "" {
		return nil
	}

	orgsResponse, err := am.apiClient.ListUserOrgs(userId)
	if err != nil {
		logger.Error("Failed to refresh organizations in background: %v", err)
		return err
	}

	am.mu.Lock()
	if am.sessionGen != gen {
		// The account changed while the request ran; this list belongs to
		// the previous one.
		am.mu.Unlock()
		logger.Debug("Dropping organizations fetched for a previous session")
		return nil
	}
	newOrgs := orgsResponse.Orgs
	currentOrgId := ""
	if am.currentOrg != nil {
		currentOrgId = am.currentOrg.Id
	}

	// Preserve current org selection if it still exists in the new list
	if currentOrgId != "" {
		found := false
		for _, org := range newOrgs {
			if org.Id == currentOrgId {
				am.currentOrg = &org
				found = true
				break
			}
		}

		if !found {
			// Current org no longer exists, clear selection
			am.currentOrg = nil
			_ = am.accountManager.SetUserOrganization(userId, "")
		}
	} else if activeAccount, _ := am.accountManager.ActiveAccount(); activeAccount != nil && activeAccount.OrgID != "" {
		// Restore selection from the cached account org after an offline startup
		for _, org := range newOrgs {
			if org.Id == activeAccount.OrgID {
				am.currentOrg = &org
				break
			}
		}
	}

	// Update organizations list
	am.organizations = newOrgs
	am.mu.Unlock()

	logger.Debug("Organizations refreshed successfully: %d orgs", len(newOrgs))
	return nil
}

// RefreshCurrentUser fetches the signed-in user from the server and stores
// it, with the account's display info. When the active session changed while
// the request ran, the result is dropped and ErrSessionChanged is returned,
// whatever the request's own outcome.
func (am *AuthManager) RefreshCurrentUser() (*api.User, error) {
	gen := am.sessionGeneration()
	user, err := am.apiClient.GetUser()
	if am.sessionGeneration() != gen {
		return nil, ErrSessionChanged
	}
	if err != nil {
		// A 403 means the session is no longer accepted. (A 401 is reported
		// through the API client's callback.)
		if api.HTTPStatus(err) == 403 {
			am.markSessionExpiredIf(gen)
		}
		return nil, err
	}
	if user.UserId == "" {
		user.UserId = user.Id
	}

	active, _ := am.accountManager.ActiveAccount()
	if active == nil || active.UserID != user.UserId {
		return nil, ErrSessionChanged
	}

	am.mu.Lock()
	if am.sessionGen != gen {
		am.mu.Unlock()
		return nil, ErrSessionChanged
	}
	am.currentUser = user
	am.mu.Unlock()

	var username, name string
	if user.Username != nil {
		username = *user.Username
	}
	if user.Name != nil {
		name = *user.Name
	}
	_ = am.accountManager.UpdateAccountUserInfo(user.UserId, username, name)
	return user, nil
}

// SessionGeneration identifies the active session; it changes on every
// login, account switch and logout. Code that applies the result of a request
// made without the lock compares it before and after.
func (am *AuthManager) SessionGeneration() uint64 {
	return am.sessionGeneration()
}

// sessionGeneration identifies the active session; it changes on every login,
// account switch and logout.
func (am *AuthManager) sessionGeneration() uint64 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.sessionGen
}

// RefreshFromMyDevice refreshes user info, organizations, and authentication status from MyDevice API
func (am *AuthManager) RefreshFromMyDevice(olmId string) error {
	am.mu.RLock()
	authenticated := am.isAuthenticated
	userId := ""
	if am.currentUser != nil {
		userId = am.currentUser.UserId
	}
	am.mu.RUnlock()

	// Only refresh if authenticated and user ID is available
	if !authenticated || userId == "" {
		return nil
	}

	// Get MyDevice data
	myDevice, err := am.apiClient.GetMyDevice(olmId)
	if err != nil {
		logger.Error("Failed to refresh from MyDevice: %v", err)
		// If we get an unauthorized error, user might be logged out
		if apiErr, ok := err.(*api.APIError); ok && apiErr.Status == 401 {
			logger.Info("Session expired, clearing authentication")
			am.mu.Lock()
			am.isAuthenticated = false
			am.mu.Unlock()
		}
		return err
	}

	am.mu.Lock()
	defer am.mu.Unlock()

	// Update user info
	if myDevice.User.UserId != "" {
		// Update current user if it matches
		if am.currentUser != nil && am.currentUser.UserId == myDevice.User.UserId {
			am.currentUser.Email = myDevice.User.Email
			am.currentUser.Username = myDevice.User.Username
			am.currentUser.Name = myDevice.User.Name
		}
	}

	// Convert ResponseOrg to Org and update organizations
	newOrgs := make([]api.Org, 0, len(myDevice.Orgs))
	currentOrgId := ""
	if am.currentOrg != nil {
		currentOrgId = am.currentOrg.Id
	}

	for _, responseOrg := range myDevice.Orgs {
		org := api.Org{
			Id:   responseOrg.OrgId,
			Name: responseOrg.OrgName,
		}
		newOrgs = append(newOrgs, org)

		// Preserve current org selection if it still exists
		if currentOrgId != "" && org.Id == currentOrgId {
			am.currentOrg = &org
		}
	}

	// If current org no longer exists, clear selection
	if currentOrgId != "" && am.currentOrg != nil && am.currentOrg.Id != currentOrgId {
		am.currentOrg = nil
		_ = am.accountManager.SetUserOrganization(am.currentUser.UserId, "")
	}

	// Update organizations list
	am.organizations = newOrgs

	// Ensure authentication is still set (should be true if we got here)
	am.isAuthenticated = true

	logger.Info("Refreshed from MyDevice")
	return nil
}

// GetOlmId gets the OLM ID for the current user
func (am *AuthManager) GetOlmId() (string, bool) {
	am.mu.RLock()
	userId := ""
	if am.currentUser != nil {
		userId = am.currentUser.UserId
	}
	am.mu.RUnlock()

	if userId == "" {
		return "", false
	}

	return am.secretManager.GetOlmId(userId)
}

// CheckOrgAccess checks if the user has access to an organization
func (am *AuthManager) CheckOrgAccess(orgId string) (bool, error) {
	// First, try to fetch the org to check access
	_, err := am.apiClient.GetOrg(orgId)
	if err == nil {
		return true, nil
	}

	// Check if it's an unauthorized error
	apiErr, ok := err.(*api.APIError)
	if !ok {
		return false, err
	}

	if apiErr.Type == api.ErrorTypeHTTPError && (apiErr.Status == 401 || apiErr.Status == 403) {
		// Try to get org policy to understand why access was denied
		am.mu.RLock()
		userId := ""
		if am.currentUser != nil {
			userId = am.currentUser.UserId
		}
		am.mu.RUnlock()

		if userId != "" {
			policyResponse, err := am.apiClient.CheckOrgUserAccess(orgId, userId)
			if err == nil {
				// Check if access is denied and show error message
				if !policyResponse.Allowed {
					// Get hostname for the resolution URL
					var hostname string
					if activeAccount, _ := am.accountManager.ActiveAccount(); activeAccount != nil {
						hostname = activeAccount.Hostname
					} else {
						// Ideally this should never happen, but use a safe fallback
						// just in case.
						hostname = config.DefaultHostname
					}

					resolutionURL := fmt.Sprintf("%s/%s", hostname, orgId)

					// Always use fallback message format
					fallbackMsg := "Access denied due to organization policy violations."
					if policyResponse.Error != nil && *policyResponse.Error != "" {
						fallbackMsg = fmt.Sprintf("Access denied: %s", *policyResponse.Error)
					}
					fallbackMsg += fmt.Sprintf("\n\nSee more and resolve the issues by visiting: %s", resolutionURL)
					return false, errors.New(fallbackMsg)
				}

				// Return false with a descriptive error (shouldn't reach here if Allowed is true)
				return false, fmt.Errorf("org policy preventing access to this org")
			}
		}

		// Return false with generic unauthorized message
		return false, errors.New("unauthorized access to this org. Contact your admin")
	}

	// Some other error occurred
	return false, err
}

// SelectOrganization selects an organization
func (am *AuthManager) SelectOrganization(org *api.Org) error {
	am.mu.RLock()
	user := am.currentUser
	gen := am.sessionGen
	am.mu.RUnlock()
	if user == nil {
		return errors.New("not signed in")
	}

	// First check org access
	hasAccess, err := am.CheckOrgAccess(org.Id)
	if err != nil || !hasAccess {
		return err
	}

	// If access is granted, proceed with selecting the org, unless the
	// account changed while access was checked.
	am.mu.Lock()
	if am.sessionGen != gen {
		am.mu.Unlock()
		return ErrSessionChanged
	}
	am.currentOrg = org
	am.mu.Unlock()

	// Save selected org to accounts store
	userID := user.UserId

	if err := am.accountManager.SetUserOrganization(userID, org.Id); err != nil {
		logger.Warn("failed to persist selected account to store: %v", err)
	}

	return nil
}

// EnsureOlmCredentials ensures OLM credentials exist for the user
func (am *AuthManager) EnsureOlmCredentials(userId string) error {
	if am.secretManager.HasOlmCredentials(userId) {
		olmIdString, found := am.secretManager.GetOlmId(userId)
		if found {
			gen := am.sessionGeneration()
			olm, err := am.apiClient.GetUserOlm(userId, olmIdString, nil)
			if am.sessionGeneration() != gen {
				// The answer came from another account's session; it says
				// nothing about these credentials.
				return ErrSessionChanged
			}
			switch {
			case err == nil && olm != nil && olm.OlmId == olmIdString:
				logger.Info("Auth: OLM credentials verified (userId=%s)", userId)
				return nil
			case err == nil && olm != nil:
				logger.Error("Auth: OLM ID mismatch (userId=%s, server=%s, stored=%s)", userId, olm.OlmId, olmIdString)
				am.secretManager.DeleteOlmCredentials(userId)
			case isNotFoundError(err):
				logger.Warn("Auth: stored OLM no longer exists on server, discarding (userId=%s, olmId=%s)", userId, olmIdString)
				am.secretManager.DeleteOlmCredentials(userId)
			default:
				logger.Warn("Auth: could not verify OLM credentials, keeping stored credentials (userId=%s): %v", userId, err)
				return nil
			}
		}
	}

	logger.Info("Auth: no local OLM credentials, using manager fingerprint (userId=%s)", userId)
	if devicePostureIPC == nil {
		logger.Error("Auth: device posture IPC not registered (userId=%s)", userId)
		return errors.New("device posture IPC is not connected")
	}
	platformFingerprint, err := devicePostureIPC.PlatformFingerprint()
	if err != nil {
		logger.Error("Auth: failed to get device fingerprint from manager (userId=%s): %v", userId, err)
		return fmt.Errorf("failed to get device fingerprint from manager: %w", err)
	}

	recoveredCreds, err := am.apiClient.RecoverOlmFromFingerprint(userId, platformFingerprint)
	if err == nil {
		saved := am.secretManager.SaveOlmCredentials(userId, recoveredCreds.OlmID, recoveredCreds.Secret)
		if !saved {
			return errors.New("failed to save OLM credentials")
		}
		logger.Info("Auth: recovered OLM credentials (userId=%s)", userId)
		return nil
	}
	if !isNotFoundError(err) {
		logger.Error("Auth: failed to recover OLM from fingerprint (userId=%s): %v", userId, err)
		return fmt.Errorf("failed to recover OLM: %w", err)
	}
	logger.Info("Auth: no OLM matches this device fingerprint, creating a new one (userId=%s)", userId)

	deviceName := config.GetFriendlyDeviceName()
	olmResponse, err := am.apiClient.CreateOlm(userId, deviceName)
	if err != nil {
		logger.Error("Auth: failed to create OLM (userId=%s): %v", userId, err)
		return fmt.Errorf("failed to create OLM: %w", err)
	}

	saved := am.secretManager.SaveOlmCredentials(userId, olmResponse.OlmId, olmResponse.Secret)
	if !saved {
		return errors.New("failed to save OLM credentials")
	}

	logger.Info("Auth: created OLM credentials (userId=%s, olmId=%s)", userId, olmResponse.OlmId)
	return nil
}

func isNotFoundError(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.Type == api.ErrorTypeHTTPError && apiErr.Status == 404
}

func (am *AuthManager) SwitchAccount(userID string) error {
	accountToSwitchTo, exists := am.accountManager.Account(userID)
	if !exists {
		return errors.New("account does not exist")
	}

	token, found := am.secretManager.GetSessionToken(accountToSwitchTo.UserID)
	if !found || token == "" {
		return errors.New("session token does not exist for this user")
	}

	// Step 1: Switch locally first (optimistic switch). The stored active
	// account, the server and the token change together, and the new
	// generation makes requests still running for the previous account drop
	// their results.
	// Step 2: Clear user data and per-session state immediately
	am.mu.Lock()
	am.sessionGen++
	gen := am.sessionGen
	_ = am.accountManager.SetActiveUser(userID)
	am.apiClient.SetSession(accountToSwitchTo.Hostname, token)
	am.currentUser = nil
	am.currentOrg = nil
	am.organizations = []api.Org{}
	am.serverInfo = nil // Clear server info to avoid showing stale data
	am.isAuthenticated = true
	am.isServerDown = false
	am.errorMessage = nil
	am.sessionExpired = false
	am.mu.Unlock()

	// Step 3: Validate with server (health check, fetch user, select org, fetch server info)
	_ = am.CheckHealthAndSetState()

	am.mu.RLock()
	serverDown := am.isServerDown
	am.mu.RUnlock()

	if serverDown {
		// Server is down, but account is switched - show error but don't revert.
		// CheckHealthAndSetState already restored the cached org for display.
		logger.Warn("Server appears to be down after account switch")
		return nil
	}

	// Fetch user data
	user, err := am.apiClient.GetUser()
	if am.sessionGeneration() != gen {
		return nil
	}
	if err != nil {
		if api.IsSessionRejected(err) {
			am.markSessionExpiredIf(gen)
		} else {
			am.mu.Lock()
			if am.sessionGen == gen {
				msg := err.Error()
				am.errorMessage = &msg
				am.restoreCachedOrganizationLocked(&accountToSwitchTo)
			}
			am.mu.Unlock()
		}
		logger.Error("Failed to fetch user after account switch: %v", err)
		return nil
	}
	if user.UserId == "" {
		user.UserId = user.Id
	}

	// Update account info with user data
	var username string
	if user.Username != nil {
		username = *user.Username
	}
	var name string
	if user.Name != nil {
		name = *user.Name
	}
	_ = am.accountManager.UpdateAccountUserInfo(userID, username, name)

	// Select organization
	orgs, selectedOrg, orgErr := loadOrganizations(am.apiClient, user.UserId, &accountToSwitchTo)
	if orgErr != nil {
		logger.Error("Failed to load organizations: %v", orgErr)
	}

	am.mu.Lock()
	if am.sessionGen != gen {
		am.mu.Unlock()
		return nil
	}
	am.currentUser = user
	am.organizations = orgs
	am.currentOrg = selectedOrg
	if selectedOrg != nil {
		_ = am.accountManager.SetUserOrganization(userID, selectedOrg.Id)
	} else if orgErr != nil {
		// Keep the remembered organization when the list could not be loaded.
		am.restoreCachedOrganizationLocked(&accountToSwitchTo)
	} else {
		_ = am.accountManager.SetUserOrganization(userID, "")
	}
	am.mu.Unlock()

	// Fetch server info
	_ = am.fetchServerInfo(gen)

	return nil
}

// Logout logs out the current user
func (am *AuthManager) Logout() error {
	am.mu.RLock()
	gen := am.sessionGen
	baseURL, token := am.apiClient.CurrentBaseURL(), am.apiClient.SessionToken()
	am.mu.RUnlock()
	userID := am.accountManager.ActiveUserID()

	// Try to call logout endpoint (ignore errors), with the session read
	// above so a switch meanwhile can't end the next account's session. It
	// doesn't report a 401 as an expired session either.
	if token != "" {
		client := api.NewAPIClient(baseURL, token)
		client.SetSessionCookieName(am.apiClient.SessionCookieName())
		_ = client.Logout()
	}

	// Find the account to switch to afterwards: the first remaining one by
	// name, as the macOS client does.
	nextAccountID := am.nextAccountAfter(userID)

	// Clear local data, including everything that belongs to this session.
	am.mu.Lock()
	if am.sessionGen != gen {
		// A login or account switch replaced the session meanwhile; don't
		// tear down that one or remove its account.
		am.mu.Unlock()
		return ErrSessionChanged
	}
	am.sessionGen++
	am.apiClient.UpdateSessionToken("")
	am.isAuthenticated = false
	am.currentUser = nil
	am.currentOrg = nil
	am.organizations = []api.Org{}
	am.serverInfo = nil
	am.isServerDown = false
	am.sessionExpired = false
	am.errorMessage = nil
	// The device code belongs to a login that may be running for another
	// account; it is left to that attempt.
	am.mu.Unlock()

	_ = am.secretManager.DeleteSessionToken(userID)
	// for backward compatibility with old servers so we dont recreate the olm
	// we are keeping this commented out for now so we dont remove the olm
	// _ = am.secretManager.DeleteOlmCredentials(userID)

	_ = am.accountManager.RemoveAccount(userID)

	// Auto-select next available account if one exists
	if nextAccountID != "" {
		// Check if the account still exists (it should, since we got it before removal)
		if _, exists := am.accountManager.Account(nextAccountID); exists {
			// Switch to the next available account
			if err := am.SwitchAccount(nextAccountID); err != nil {
				logger.Warn("Failed to auto-switch to next account after logout: %v", err)
				// Don't return error - logout was successful, auto-switch is just a convenience
			}
		}
	}

	return nil
}

// nextAccountAfter returns the first account other than userID, ordered by
// display name, or "" when there is none.
func (am *AuthManager) nextAccountAfter(userID string) string {
	var next *config.Account
	for _, account := range am.accountManager.Accounts() {
		if account.UserID == userID {
			continue
		}
		account := account
		if next == nil || strings.ToLower(AccountDisplayName(&account)) < strings.ToLower(AccountDisplayName(next)) {
			next = &account
		}
	}
	if next == nil {
		return ""
	}
	return next.UserID
}

// HasSession reports whether a session token is stored for the account.
func (am *AuthManager) HasSession(userID string) bool {
	_, ok := am.secretManager.GetSessionToken(userID)
	return ok
}

// DeleteAccount signs out of an account and removes it. The active account
// goes through Logout, which switches to the next account. Any other account
// is removed locally and signed out on its server in the background.
func (am *AuthManager) DeleteAccount(userID string) error {
	if userID == am.accountManager.ActiveUserID() {
		return am.Logout()
	}
	account, ok := am.accountManager.Account(userID)
	if !ok {
		return nil
	}
	if token, ok := am.secretManager.GetSessionToken(userID); ok && account.Hostname != "" {
		client := api.NewAPIClient(account.Hostname, token)
		client.SetSessionCookieName(am.configManager.GetSessionCookieName())
		go func() { _ = client.Logout() }()
	}
	_ = am.secretManager.DeleteSessionToken(userID)
	return am.accountManager.RemoveAccount(userID)
}

// CheckHealthAndSetState performs a health check and updates the server down state
func (am *AuthManager) CheckHealthAndSetState() error {
	gen := am.sessionGeneration()
	healthy, err := am.apiClient.CheckHealth()

	am.mu.Lock()
	defer am.mu.Unlock()
	if am.sessionGen != gen {
		// The answer is about the previous account's server.
		return err
	}
	if err != nil || !healthy {
		// Network error means server is down
		am.isServerDown = true
		msg := "The server appears to be down."
		am.errorMessage = &msg
		if account, accErr := am.accountManager.ActiveAccount(); accErr == nil {
			am.restoreCachedOrganizationLocked(account)
		}
		return err
	}
	am.isServerDown = false
	am.errorMessage = nil
	return nil
}

// fetchServerInfo fetches and stores server information, unless the session
// changed from gen in the meantime.
func (am *AuthManager) fetchServerInfo(gen uint64) error {
	serverInfo, err := am.apiClient.GetServerInfo()
	if err != nil {
		// Non-fatal error, just log it
		logger.Error("Failed to fetch server info: %v", err)
		return err
	}

	am.mu.Lock()
	if am.sessionGen == gen {
		am.serverInfo = serverInfo
	}
	am.mu.Unlock()

	return nil
}

// Getters for state (thread-safe)

func (am *AuthManager) IsAuthenticated() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.isAuthenticated
}

func (am *AuthManager) CurrentUser() *api.User {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.currentUser
}

func (am *AuthManager) CurrentOrg() *api.Org {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.currentOrg
}

func (am *AuthManager) Organizations() []api.Org {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.organizations
}

func (am *AuthManager) IsInitializing() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.isInitializing
}

func (am *AuthManager) ErrorMessage() *string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.errorMessage
}

func (am *AuthManager) DeviceAuthCode() *string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.deviceAuthCode
}

func (am *AuthManager) DeviceAuthLoginURL() *string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.deviceAuthLoginURL
}

func (am *AuthManager) ServerInfo() *api.ServerInfo {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.serverInfo
}

func (am *AuthManager) IsServerDown() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.isServerDown
}

func (am *AuthManager) SessionExpired() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.sessionExpired
}

func (am *AuthManager) IsDeviceAuthInProgress() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.isDeviceAuthInProgress
}

func (am *AuthManager) StartDeviceAuthImmediately() bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.startDeviceAuthImmediately
}

func (am *AuthManager) SetStartDeviceAuthImmediately(v bool) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.startDeviceAuthImmediately = v
}

func (am *AuthManager) ClearStartDeviceAuthImmediately() {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.startDeviceAuthImmediately = false
}

// ClearDeviceAuth clears the device authentication code and URL. A login
// attempt still running can no longer set them; it has been cancelled or is
// about to be replaced.
func (am *AuthManager) ClearDeviceAuth() {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.deviceAuthCode = nil
	am.deviceAuthLoginURL = nil
	am.deviceAuthOwner = 0
	am.isDeviceAuthInProgress = false
}

// UpdateCurrentUser updates the current user (used for session verification)
func (am *AuthManager) UpdateCurrentUser(user *api.User) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.currentUser = user
}

// APIClient returns a reference to the API client
func (am *AuthManager) APIClient() *api.APIClient {
	return am.apiClient
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			strings.Contains(s, substr))))
}
