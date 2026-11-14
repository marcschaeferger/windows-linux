package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fosrl/windows-linux/auth"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/secrets"

	"github.com/fosrl/newt/logger"
)

// IPCClient provides an interface for IPC operations needed by the tunnel manager
// This avoids circular dependencies between tunnel and managers packages
type IPCClient interface {
	StartTunnel(config Config) error
	StopTunnel() error
	RegisterStateChangeCallback(cb func(State)) func() // Returns unregister function
}

// StopReason records why the tunnel last became Stopped.
type StopReason int

const (
	StopReasonNone StopReason = iota
	StopReasonUser
	StopReasonError
	StopReasonLost
)

// Manager manages tunnel connection state and operations
// It provides a simplified API for the UI layer, abstracting away IPC details
type Manager struct {
	mu             sync.RWMutex
	currentState   State
	stopReason     StopReason
	isConnected    bool
	stateCallback  func(State)
	errorCallback  func(*OLMStatusError)
	unregisterCb   func()
	ipcClient      IPCClient
	authManager    *auth.AuthManager
	configManager  *config.ConfigManager
	accountManager *config.AccountManager
	secretManager  *secrets.SecretManager
	// gatewayResolver looks up the exit node (gateway) to establish when the
	// tunnel connects; nil or a 0 resource ID means connect without one. An
	// error means the tunnel must not connect (the org forces an exit node that
	// can't be looked up).
	gatewayResolver func(orgID string) (siteResourceID int, siteIDs []int, err error)
	// serverSettingsFetcher fetches the settings the org's server manages.
	serverSettingsFetcher func(orgID string) *config.ClientSettings

	// opGen identifies the current connection. Connect, disconnect, adopting a
	// tunnel and a stop reported by the manager each start a new one, so a
	// Connect that was cancelled, or a status poller of an earlier connection,
	// can tell that it must no longer act.
	opGen uint64
	// connecting is set from the start of Connect until OLM of the new tunnel
	// has answered a status poll. Stop notifications that arrive meanwhile
	// belong to an earlier stop: the manager sends them before it answers the
	// stop request, but the UI may still be working through them. A real stop
	// in this window is caught by the status poller instead.
	connecting bool
	// opMu serializes the start and stop requests to the manager, so a
	// Disconnect during Connect's StartTunnel stops the tunnel after it started.
	opMu sync.Mutex

	// prepareConfig makes sure credentials exist and builds the tunnel config.
	// It is a field so tests can replace the auth, config and secret managers.
	prepareConfig func() (Config, error)
	// orgSelected reports whether an organization is selected.
	orgSelected func() bool
	// fetchStatus reads OLM's status; a field so tests can fake OLM.
	fetchStatus func(ctx context.Context) (*OLMStatusResponse, error)

	// Status polling fields
	pollInterval time.Duration
	// startupUnreachablePolls is how many failed polls in a row, before OLM's
	// API has answered once, mean the tunnel process is gone.
	startupUnreachablePolls int
	pollCtx                 context.Context
	pollCancel              context.CancelFunc
}

// NewManager creates a new Manager instance
func NewManager(
	authManager *auth.AuthManager,
	configManager *config.ConfigManager,
	accountManager *config.AccountManager,
	secretManager *secrets.SecretManager,
	ipcClient IPCClient,
) *Manager {
	tm := &Manager{
		currentState:            StateStopped,
		isConnected:             false,
		authManager:             authManager,
		configManager:           configManager,
		accountManager:          accountManager,
		secretManager:           secretManager,
		ipcClient:               ipcClient,
		pollInterval:            time.Second,
		startupUnreachablePolls: startupUnreachableThreshold,
	}
	tm.prepareConfig = tm.prepareConnect
	tm.orgSelected = func() bool { return tm.authManager != nil && tm.authManager.CurrentOrg() != nil }
	tm.fetchStatus = getOLMStatus

	// Register for tunnel state change notifications
	if ipcClient != nil {
		tm.unregisterCb = ipcClient.RegisterStateChangeCallback(tm.handleManagerState)
	}

	return tm
}

// handleManagerState applies a tunnel state reported by the manager service.
// Its notifications arrive asynchronously, so one can describe an operation
// that this Manager has already moved past.
func (tm *Manager) handleManagerState(state State) {
	if isTransitionalConnectState(state) {
		// The status poller tracks the connect progress.
		return
	}

	tm.mu.Lock()
	switch {
	case tm.connecting && (state == StateStopped || state == StateStopping):
		// The stop of a previous connection, reported after Connect began.
		tm.mu.Unlock()
		return
	case state == StateStopping && tm.currentState == StateStopped:
		// A late report of a stop that has already finished.
		tm.mu.Unlock()
		return
	case state == tm.currentState:
		tm.mu.Unlock()
		return
	}
	if state == StateStopped {
		// The tunnel is gone, for example stopped by another session: end this
		// connection so its poller stops and can no longer act on it.
		tm.opGen++
		tm.stopPollingLocked()
	}
	tm.currentState = state
	tm.isConnected = (state == StateRunning)
	callback := tm.stateCallback
	tm.mu.Unlock()

	if callback != nil {
		callback(state)
	}
}

// SetGatewayResolver sets the function used at connect time to resolve the
// saved exit node into the resource and site IDs olm needs.
func (tm *Manager) SetGatewayResolver(fn func(orgID string) (siteResourceID int, siteIDs []int, err error)) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.gatewayResolver = fn
}

// SetServerSettingsFetcher sets the function used at connect time to fetch the
// settings the org's server manages.
func (tm *Manager) SetServerSettingsFetcher(fn func(orgID string) *config.ClientSettings) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.serverSettingsFetcher = fn
}

// Close cleans up resources used by the Manager
func (tm *Manager) Close() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.stopPollingLocked()

	if tm.unregisterCb != nil {
		tm.unregisterCb()
		tm.unregisterCb = nil
	}
}

// State returns the current tunnel state
func (tm *Manager) State() State {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.currentState
}

// IsConnected returns whether the tunnel is currently connected
func (tm *Manager) IsConnected() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.isConnected
}

// RegisterStateChangeCallback registers a callback that will be called when tunnel state changes
func (tm *Manager) RegisterStateChangeCallback(cb func(State)) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.stateCallback = cb
}

// RegisterErrorCallback registers a callback that will be called when an error is detected in OLM status
func (tm *Manager) RegisterErrorCallback(cb func(*OLMStatusError)) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.errorCallback = cb
}

func isTransitionalConnectState(state State) bool {
	return state == StateStarting || state == StateRegistering || state == StateRegistered
}

// setStateIfCurrent moves to state if gen is still the current connection,
// and calls the state callback if the state changed. It reports whether gen
// was current.
func (tm *Manager) setStateIfCurrent(gen uint64, state State, reason *StopReason) bool {
	tm.mu.Lock()
	if tm.opGen != gen {
		tm.mu.Unlock()
		return false
	}
	if reason != nil {
		tm.stopReason = *reason
	}
	if state == StateStopped {
		tm.connecting = false
	}
	changed := tm.currentState != state
	tm.currentState = state
	tm.isConnected = (state == StateRunning)
	callback := tm.stateCallback
	tm.mu.Unlock()

	if changed && callback != nil {
		callback(state)
	}
	return true
}

func (tm *Manager) isCurrent(gen uint64) bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.opGen == gen
}

// TakeStopReason returns the reason for the most recent stop and clears it,
// so a later stop notification is not attributed to the same event.
func (tm *Manager) TakeStopReason() StopReason {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	reason := tm.stopReason
	tm.stopReason = StopReasonNone
	return reason
}

// failConnect ends the connect attempt gen after an error, unless it was
// cancelled meanwhile.
func (tm *Manager) failConnect(gen uint64) {
	reason := StopReasonError
	tm.setStateIfCurrent(gen, StateStopped, &reason)
}

// upstreamDNSAddress turns a DNS server from the settings into the host:port
// olm dials. IPv6 addresses need brackets: "2606:4700::1111" becomes
// "[2606:4700::1111]:53", not the invalid "2606:4700::1111:53".
func upstreamDNSAddress(server string) string {
	server = strings.TrimSpace(server)
	if ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(server, "["), "]")); ip != nil {
		return net.JoinHostPort(ip.String(), "53")
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		// Already has a port.
		return server
	}
	return net.JoinHostPort(server, "53")
}

// buildConfig builds the tunnel configuration from auth manager, config manager, and secret manager
func (tm *Manager) buildConfig() (Config, error) {
	activeAccount, err := tm.accountManager.ActiveAccount()
	if err != nil {
		return Config{}, err
	}

	userToken, found := tm.secretManager.GetSessionToken(activeAccount.UserID)
	if !found || userToken == "" {
		return Config{}, fmt.Errorf("session token not found")
	}

	currentOrg := tm.authManager.CurrentOrg()
	if currentOrg == nil {
		return Config{}, fmt.Errorf("no organization selected")
	}

	// The active account owns the session token above. CurrentUser() can be
	// nil while authenticated (server down at start, or /user failing after
	// an account switch), so don't dereference it here.
	userId := activeAccount.UserID
	olmId, found := tm.secretManager.GetOlmId(userId)
	if !found || olmId == "" {
		return Config{}, fmt.Errorf("OLM ID not found")
	}
	olmSecret, found := tm.secretManager.GetOlmSecret(userId)
	if !found || olmSecret == "" {
		return Config{}, fmt.Errorf("OLM secret not found")
	}

	// The server's settings are fetched on every connect; the gateway resolver
	// below relies on them being recorded by now.
	tm.mu.RLock()
	fetchServerSettings := tm.serverSettingsFetcher
	tm.mu.RUnlock()
	var serverSettings *config.ClientSettings
	if fetchServerSettings != nil {
		serverSettings = fetchServerSettings(currentOrg.Id)
	}

	settings := tm.configManager.EffectiveSettings(currentOrg.Id, serverSettings)
	primaryDNS := settings.PrimaryDNS
	secondaryDNS := settings.SecondaryDNS
	dnsOverride := settings.DNSOverride
	dnsTunnel := settings.DNSTunnel
	preferLocalRoutes := settings.PreferLocalRoutes
	exitNodeTakesPrecedence := settings.ExitNodeTakesPrecedence

	// Build UpstreamDNS array with port 53 added to each. If no DNS servers are
	// configured, this stays empty, telling olm to use the system DNS.
	upstreamDNS := []string{}
	if primaryDNS != "" {
		upstreamDNS = append(upstreamDNS, upstreamDNSAddress(primaryDNS))
	}
	if secondaryDNS != "" {
		upstreamDNS = append(upstreamDNS, upstreamDNSAddress(secondaryDNS))
	}

	config := Config{
		Name:                DefaultTunnelName,
		ID:                  olmId,
		Secret:              olmSecret,
		UserToken:           userToken,
		MTU:                 settings.MTU,
		Holepunch:           true,
		PingIntervalSeconds: 5,
		PingTimeoutSeconds:  5,
		Endpoint:            activeAccount.Hostname,
		//  DNS:                 "1.1.1.1", // this gets pulled dynamically from the host system now
		OrgID:                   currentOrg.Id,
		InterfaceName:           "Pangolin",
		UpstreamDNS:             upstreamDNS, // Each value is a host:port
		MatchDomains:            settings.MatchDomains,
		OverrideDNS:             dnsOverride,
		TunnelDNS:               dnsTunnel,
		PreferLocalRoutes:       preferLocalRoutes,
		ExitNodeTakesPrecedence: exitNodeTakesPrecedence,
	}

	tm.mu.RLock()
	resolveGateway := tm.gatewayResolver
	tm.mu.RUnlock()
	if resolveGateway != nil {
		var gwErr error
		config.GatewaySiteResourceId, config.GatewaySiteIds, gwErr = resolveGateway(currentOrg.Id)
		if gwErr != nil {
			return Config{}, formatConnectionError("Exit Node Unavailable", gwErr.Error(), gwErr)
		}
	}

	return config, nil
}

// ConnectionError represents a connection error with a user-friendly message
type ConnectionError struct {
	Title   string
	Message string
	Err     error
}

func (e *ConnectionError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Message
}

// formatConnectionError creates a user-friendly error message
func formatConnectionError(title, message string, err error) *ConnectionError {
	return &ConnectionError{
		Title:   title,
		Message: message,
		Err:     err,
	}
}

// connectStateError returns the error for a Connect in state, or nil if the
// tunnel can be connected from it.
func connectStateError(state State) error {
	if state == StateRunning {
		logger.Info("Tunnel is already running")
		return formatConnectionError(
			"Tunnel Already Running",
			"The tunnel is already running. Please disconnect it before connecting again.",
			nil,
		)
	}
	if isTransitionalConnectState(state) {
		logger.Info("Tunnel is already starting/connecting")
		return formatConnectionError(
			"Tunnel Already Starting",
			"The tunnel is already starting. Please wait for it to complete.",
			nil,
		)
	}
	return nil
}

// beginConnect checks the state and moves to Starting in one step, so two
// concurrent Connect calls can't both start the tunnel. It returns the new
// connection's generation.
func (tm *Manager) beginConnect() (uint64, error) {
	tm.mu.Lock()
	if err := connectStateError(tm.currentState); err != nil {
		tm.mu.Unlock()
		return 0, err
	}
	tm.opGen++
	gen := tm.opGen
	tm.connecting = true
	tm.stopReason = StopReasonNone
	tm.currentState = StateStarting
	tm.isConnected = false
	callback := tm.stateCallback
	tm.mu.Unlock()

	if callback != nil {
		callback(StateStarting)
	}
	return gen, nil
}

// prepareConnect makes sure the device has OLM credentials and builds the
// tunnel configuration. Its errors are *ConnectionError.
func (tm *Manager) prepareConnect() (Config, error) {
	// Ensure OLM credentials exist before connecting
	currentUser := tm.authManager.CurrentUser()
	if currentUser != nil && currentUser.UserId != "" {
		if err := tm.authManager.EnsureOlmCredentials(currentUser.UserId); err != nil {
			logger.Error("Failed to ensure OLM credentials: %v", err)
			return Config{}, formatConnectionError(
				"OLM Credentials Error",
				fmt.Sprintf("Failed to set up device credentials: %v", err),
				err,
			)
		}
	} else {
		activeAccount, err := tm.accountManager.ActiveAccount()
		if err != nil {
			logger.Error("Failed to get active account: %v", err)
			return Config{}, formatConnectionError(
				"Authentication Error",
				"No user ID available. Please log in again.",
				err,
			)
		}

		if err := tm.authManager.EnsureOlmCredentials(activeAccount.UserID); err != nil {
			logger.Error("Failed to ensure OLM credentials: %v", err)
			return Config{}, formatConnectionError(
				"OLM Credentials Error",
				fmt.Sprintf("Failed to set up device credentials: %v", err),
				err,
			)
		}
	}

	// Build config from dependencies
	config, err := tm.buildConfig()
	if err != nil {
		logger.Error("Failed to build tunnel config: %v", err)
		var connErr *ConnectionError
		if errors.As(err, &connErr) {
			return Config{}, connErr
		}
		// Format config build errors
		if err.Error() == "session token not found" {
			return Config{}, formatConnectionError(
				"Authentication Error",
				"Session token not found. Please log in again.",
				err,
			)
		}
		return Config{}, formatConnectionError(
			"Configuration Error",
			fmt.Sprintf("Failed to build tunnel configuration: %v", err),
			err,
		)
	}
	return config, nil
}

// Connect starts the tunnel, building the configuration internally. A
// Disconnect while it runs cancels it: Connect then returns nil without
// leaving a tunnel up.
func (tm *Manager) Connect() error {
	if err := connectStateError(tm.State()); err != nil {
		return err
	}

	// Require an organization to be selected before connecting
	if !tm.orgSelected() {
		logger.Error("No organization selected, aborting connection")
		return formatConnectionError(
			"No Organization Selected",
			"Please select an organization before connecting.",
			nil,
		)
	}

	gen, err := tm.beginConnect()
	if err != nil {
		return err
	}

	config, err := tm.prepareConfig()
	if !tm.isCurrent(gen) {
		logger.Info("Connect was cancelled while preparing the tunnel")
		return nil
	}
	if err != nil {
		tm.failConnect(gen)
		return err
	}

	logger.Info("Connecting tunnel with config: Name=%s, Endpoint=%s", config.Name, config.Endpoint)
	if tm.ipcClient == nil {
		tm.failConnect(gen)
		return formatConnectionError(
			"Connection Error",
			"IPC client not initialized. Please restart the application.",
			nil,
		)
	}

	tm.opMu.Lock()
	if !tm.isCurrent(gen) {
		tm.opMu.Unlock()
		logger.Info("Connect was cancelled before the tunnel started")
		return nil
	}
	err = tm.ipcClient.StartTunnel(config)
	current := tm.isCurrent(gen)
	tm.opMu.Unlock()

	if !current {
		// The Disconnect that cancelled this was waiting for StartTunnel to
		// return and now stops the tunnel.
		logger.Info("Connect was cancelled while the tunnel was starting")
		return nil
	}
	if err != nil {
		logger.Error("Failed to start tunnel: %v", err)
		tm.failConnect(gen)
		return formatConnectionError(
			"Connection Failed",
			fmt.Sprintf("Failed to start the tunnel: %v", err),
			err,
		)
	}

	logger.Info("Starting status polling")
	tm.startStatusPolling(gen)

	return nil
}

// AdoptRunningTunnel picks up a tunnel that is already running when the UI
// starts, such as one left up when a previous UI exited, so the UI shows it
// instead of replacing it. A tunnel for a different organization than orgID
// is left alone. It reports whether the tunnel was adopted.
func (tm *Manager) AdoptRunningTunnel(orgID string) bool {
	if tm.State() != StateStopped {
		return false
	}
	if !mayAdopt() {
		logger.Info("The running tunnel belongs to another user; not adopting it")
		return false
	}
	status, err := tm.fetchStatus(context.Background())
	if err != nil {
		// No tunnel is running, so its API pipe does not exist.
		return false
	}
	if status.Terminated || status.Error != nil {
		return false
	}
	if orgID != "" && status.OrgID != "" && status.OrgID != orgID {
		logger.Info("Running tunnel is for organization %s, not %s; not adopting it", status.OrgID, orgID)
		return false
	}

	if !status.Registered {
		// OLM is up but not registering, e.g. after its /disconnect: nothing
		// would move such a tunnel on, so leave it to the next Connect.
		logger.Info("Running tunnel is not registered; not adopting it")
		return false
	}
	state := StateRegistered
	if status.Connected {
		state = StateRunning
	}

	tm.mu.Lock()
	if tm.currentState != StateStopped || tm.connecting {
		// A Connect started meanwhile.
		tm.mu.Unlock()
		return false
	}
	tm.opGen++
	gen := tm.opGen
	tm.mu.Unlock()

	logger.Info("Adopting tunnel that is already running (state %s)", state)
	SetState(state)
	if !tm.setStateIfCurrent(gen, state, nil) {
		return false
	}
	tm.startStatusPolling(gen)
	return true
}

// Disconnect stops the tunnel because the user asked to. It also cancels a
// Connect that is still in progress.
func (tm *Manager) Disconnect() error {
	_, err := tm.disconnect(0, StopReasonUser)
	return err
}

// StopForAccountChange stops the tunnel before the active account changes,
// like Disconnect. It also asks the manager to stop a tunnel this Manager
// doesn't know about, such as one started before the UI.
func (tm *Manager) StopForAccountChange() error {
	switch tm.State() {
	case StateStopped, StateStopping:
	default:
		_, err := tm.disconnect(0, StopReasonUser)
		return err
	}
	if tm.ipcClient == nil {
		return fmt.Errorf("IPC client not initialized")
	}
	// After any disconnect that is in progress.
	tm.opMu.Lock()
	defer tm.opMu.Unlock()
	return tm.ipcClient.StopTunnel()
}

// disconnect stops the tunnel. With a non-zero onlyGen it acts only if that
// is still the current connection, which the status poller uses so it never
// stops a newer connection than the one it watches. It reports whether it
// stopped the tunnel.
func (tm *Manager) disconnect(onlyGen uint64, reason StopReason) (bool, error) {
	tm.mu.Lock()
	if onlyGen != 0 && tm.opGen != onlyGen {
		tm.mu.Unlock()
		return false, nil
	}
	// Check if already disconnected or disconnecting
	switch tm.currentState {
	case StateStopped:
		tm.mu.Unlock()
		logger.Info("Tunnel is already stopped")
		return false, nil
	case StateStopping:
		tm.mu.Unlock()
		logger.Info("Tunnel is already stopping")
		return false, nil
	}
	tm.opGen++
	gen := tm.opGen
	tm.connecting = false
	tm.stopReason = reason
	tm.currentState = StateStopping
	tm.isConnected = false
	callback := tm.stateCallback
	tm.mu.Unlock()

	if callback != nil {
		callback(StateStopping)
	}

	logger.Info("Disconnecting tunnel")
	var err error
	tm.opMu.Lock()
	tm.StopStatusPolling()
	if tm.ipcClient == nil {
		err = fmt.Errorf("IPC client not initialized")
	} else {
		err = tm.ipcClient.StopTunnel()
	}
	tm.opMu.Unlock()

	// Always end in Stopped, even when the tunnel service is already gone
	// (uninstall/stop can fail after a crash). The manager's own Stopped
	// notification may arrive later or not at all.
	tm.setStateIfCurrent(gen, StateStopped, nil)
	if err != nil {
		logger.Error("Failed to stop tunnel: %v", err)
		return true, err
	}

	logger.Info("Disconnected tunnel")

	return true, nil
}

// OLMStatusError represents an error in the OLM status response
type OLMStatusError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// sessionExpiredErrorCodes are OLM/connection error codes that mean session expired or unauthorized.
// When the connection reports one of these before fully established, we set session-expired state.
var sessionExpiredErrorCodes = map[string]struct{}{
	"UNAUTHORIZED":                      {},
	"SESSION_EXPIRED":                   {},
	"ORG_ACCESS_POLICY_SESSION_EXPIRED": {},
	"INVALID_USER_SESSION":              {},
	"USER_ID_NOT_FOUND":                 {},
}

// OLMStatusResponse represents the status response from OLM API
type OLMStatusResponse struct {
	Connected       bool                   `json:"connected"`
	Registered      bool                   `json:"registered"`
	Terminated      bool                   `json:"terminated"`
	Version         string                 `json:"version,omitempty"`
	Agent           string                 `json:"agent,omitempty"`
	OrgID           string                 `json:"orgId,omitempty"`
	PeerStatuses    map[int]*OLMPeerStatus `json:"peers,omitempty"`
	NetworkSettings map[string]interface{} `json:"networkSettings,omitempty"`
	Error           *OLMStatusError        `json:"error,omitempty"`
	ExitNode        *OLMExitNodeStatus     `json:"exitNode,omitempty"`

	// Gateway (exit node) selection: whether all traffic is routed through
	// one, the site resource it was selected from, and the sites in use.
	GatewayActive         bool  `json:"gatewayActive,omitempty"`
	GatewaySiteResourceID int   `json:"gatewaySiteResourceId,omitempty"`
	GatewaySiteIDs        []int `json:"gatewaySiteIds,omitempty"`
}

// OLMPeerStatus represents the status of a peer connection
type OLMPeerStatus struct {
	SiteID    int           `json:"siteId"`
	SiteName  string        `json:"name"`
	Connected bool          `json:"connected"`
	RTT       time.Duration `json:"rtt"`
	LastSeen  time.Time     `json:"lastSeen"`
	Endpoint  string        `json:"endpoint,omitempty"`
	IsRelay   bool          `json:"isRelay"`
	IsLocal   bool          `json:"isLocal"` // true when connected via a local network endpoint, bypassing both the public endpoint and relay
	PeerIP    string        `json:"peerAddress,omitempty"`
}

// OLMExitNodeStatus represents the connectivity status of the client's own exit
// node connection (used for site resources hosted on the exit node)
type OLMExitNodeStatus struct {
	Connected bool          `json:"connected"`
	RTT       time.Duration `json:"rtt"`
	LastSeen  time.Time     `json:"lastSeen"`
	Endpoint  string        `json:"endpoint,omitempty"`
}

// SwitchOrgRequest represents the request body for switching organizations
type SwitchOrgRequest struct {
	OrgID string `json:"org_id"`
}

// SelectGatewayRequest represents the request body for selecting an exit node
type SelectGatewayRequest struct {
	SiteResourceID int   `json:"siteResourceId"`
	SiteIDs        []int `json:"siteIds"`
}

var (
	olmClientOnce sync.Once
	olmClient     *http.Client
)

// olmHTTPClient returns the HTTP client that talks to the OLM API over its
// local socket (a named pipe on Windows, a Unix socket on Linux). One client
// is shared by every call, including the 1 s status poll. Keep-alives are off:
// connecting to the local socket is cheap, and an idle connection would
// outlive the tunnel process it belongs to.
func olmHTTPClient() *http.Client {
	olmClientOnce.Do(func() {
		olmClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					// Ignore network and addr, we're connecting to the local socket
					return dialOLM(ctx)
				},
				DisableKeepAlives: true,
			},
			Timeout: 10 * time.Second,
		}
	})
	return olmClient
}

// GetOLMStatus retrieves the status from OLM via the named pipe API
func (tm *Manager) GetOLMStatus() (*OLMStatusResponse, error) {
	return getOLMStatus(context.Background())
}

func getOLMStatus(ctx context.Context) (*OLMStatusResponse, error) {
	// Make GET request to /status endpoint
	// Use a dummy URL since we're connecting via named pipe
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/status", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := olmHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to OLM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OLM API returned status %d: %s", resp.StatusCode, string(body))
	}

	var statusResp OLMStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("failed to decode OLM status response: %w", err)
	}

	return &statusResp, nil
}

// SwitchOLMOrg switches the organization in OLM via the named pipe API
func (tm *Manager) SwitchOLMOrg(orgID string) error {
	tm.mu.RLock()
	currentState := tm.currentState
	tm.mu.RUnlock()

	// Only allow switching org if tunnel is running
	if currentState != StateRunning {
		logger.Info("Tunnel is not running, cannot switch organization")
		return fmt.Errorf("tunnel is not running")
	}

	if orgID == "" {
		return fmt.Errorf("orgID cannot be empty")
	}

	logger.Info("Switching tunnel organization to: %s", orgID)

	// Create request body
	reqBody := SwitchOrgRequest{
		OrgID: orgID,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	// Make POST request to /switch-org endpoint
	req, err := http.NewRequest("POST", "http://localhost/switch-org", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := olmHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to OLM: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OLM API returned status %d: %s", resp.StatusCode, string(body))
	}

	logger.Info("Successfully switched OLM organization to: %s", orgID)
	return nil
}

// postOLM sends a POST to an OLM API endpoint over the named pipe and returns
// an error unless the response status is one of the expected ones. The error
// carries OLM's message body, which explains why a request was rejected.
func postOLM(path string, body any, expected ...int) error {
	var reader io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}
		reader = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequest("POST", "http://localhost"+path, reader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := olmHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to OLM: %w", err)
	}
	defer resp.Body.Close()

	for _, code := range expected {
		if resp.StatusCode == code {
			return nil
		}
	}
	msg, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%s", strings.TrimSpace(string(msg)))
}

// SelectGateway routes all tunnel traffic through the given sites (the exit
// node). siteResourceID is the gateway site resource they belong to. Every
// site must already be a connected peer, so the tunnel has to be running.
func (tm *Manager) SelectGateway(siteResourceID int, siteIDs []int) error {
	if tm.State() != StateRunning {
		return fmt.Errorf("tunnel is not running")
	}
	logger.Info("Selecting exit node: site resource %d, sites %v", siteResourceID, siteIDs)
	return postOLM("/gateway/select", SelectGatewayRequest{SiteResourceID: siteResourceID, SiteIDs: siteIDs}, http.StatusAccepted)
}

// DisableGateway stops routing all tunnel traffic through an exit node.
func (tm *Manager) DisableGateway() error {
	if tm.State() != StateRunning {
		return fmt.Errorf("tunnel is not running")
	}
	logger.Info("Disabling exit node")
	return postOLM("/gateway/disable", nil, http.StatusOK)
}

// How many consecutive 1s poll failures (or lost-connection reports) while
// StateRunning before we treat the tunnel as dead and disconnect. The same
// limit applies in the other connected states once OLM's API has answered.
const statusUnreachableThreshold = 3

// startupUnreachableThreshold is how many consecutive poll failures, before
// OLM's API has answered for the first time, mean the tunnel process exited
// while starting (bad config, missing posture data, a crash). It is longer
// because the API socket only exists once the process is up.
const startupUnreachableThreshold = 30

// tunnelExitedCode is the error code passed to the error callback when the
// tunnel process went away before it connected.
const tunnelExitedCode = "TUNNEL_EXITED"

// StartStatusPolling starts polling the OLM status endpoint every second for
// the current connection.
func (tm *Manager) StartStatusPolling() {
	tm.mu.RLock()
	gen := tm.opGen
	tm.mu.RUnlock()
	tm.startStatusPolling(gen)
}

// startStatusPolling starts the poller for connection gen, replacing any
// running poller. It does nothing if gen is no longer the current connection.
func (tm *Manager) startStatusPolling(gen uint64) {
	tm.mu.Lock()
	if tm.opGen != gen {
		tm.mu.Unlock()
		return
	}
	tm.stopPollingLocked()
	ctx, cancel := context.WithCancel(context.Background())
	tm.pollCtx, tm.pollCancel = ctx, cancel
	interval := tm.pollInterval
	tm.mu.Unlock()

	go tm.pollStatus(ctx, gen, interval)
	logger.Info("Started OLM status polling (every %v)", interval)
}

// pollerDone forgets the poller with ctx, unless a newer one replaced it.
func (tm *Manager) pollerDone(ctx context.Context) {
	tm.mu.Lock()
	if tm.pollCtx == ctx {
		tm.pollCtx = nil
		tm.pollCancel = nil
	}
	tm.mu.Unlock()
	logger.Info("Status polling stopped")
}

// pollStatus polls OLM until ctx is cancelled and keeps the state of
// connection gen in step with it. Every action is checked against gen, so a
// poller that outlives its connection can't affect a newer one.
func (tm *Manager) pollStatus(ctx context.Context, gen uint64, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	consecutiveLost := 0
	// apiAnswered is set once OLM's API has answered: from then on a failure
	// means the process is gone, not that it is still starting.
	apiAnswered := false
	// wasRunning is set once this connection was fully up.
	wasRunning := false

	tm.mu.RLock()
	startupLimit := tm.startupUnreachablePolls
	fetch := tm.fetchStatus
	tm.mu.RUnlock()

	for {
		select {
		case <-ctx.Done():
			tm.pollerDone(ctx)
			return
		case <-ticker.C:
		}

		// Poll the status
		status, err := fetch(ctx)
		if ctx.Err() != nil {
			// Stopped while the request was in flight; the result belongs to
			// a connection that is being torn down.
			tm.pollerDone(ctx)
			return
		}
		currentState := tm.State()
		if currentState == StateRunning {
			wasRunning = true
		}
		if err != nil {
			logger.Error("Failed to poll OLM status: %v", err)
			if currentState == StateStopped || currentState == StateStopping {
				continue
			}
			consecutiveFailures++
			limit := statusUnreachableThreshold
			if !apiAnswered && currentState != StateRunning {
				// During startup the named pipe may not be ready yet.
				limit = startupLimit
			}
			if consecutiveFailures < limit {
				continue
			}
			consecutiveFailures = 0
			consecutiveLost = 0
			if !apiAnswered && !wasRunning {
				logger.Error("OLM never answered after %d polls; the tunnel stopped while starting", limit)
				stopped, discErr := tm.disconnect(gen, StopReasonError)
				if discErr != nil {
					logger.Error("Failed to disconnect tunnel after it failed to start: %v", discErr)
				}
				if stopped {
					tm.reportError(&OLMStatusError{
						Code:    tunnelExitedCode,
						Message: "The tunnel stopped before it connected. See the logs for details.",
					})
				}
				continue
			}
			reason := StopReasonLost
			if !wasRunning {
				reason = StopReasonError
			}
			logger.Info("OLM unreachable after %d consecutive poll failures, disconnecting", limit)
			if _, discErr := tm.disconnect(gen, reason); discErr != nil {
				logger.Error("Failed to disconnect tunnel after poll failures: %v", discErr)
			}
			continue
		}
		consecutiveFailures = 0
		if !apiAnswered {
			apiAnswered = true
			// The new tunnel is up, so the stops of earlier ones have all
			// been seen; stop notifications count again.
			tm.mu.Lock()
			if tm.opGen == gen {
				tm.connecting = false
			}
			tm.mu.Unlock()
		}

		// This should be checked before checking termination or state updates
		if status.Error != nil {
			// Only handle errors during registration phase (not yet fully connected)
			if currentState != StateRunning {
				logger.Error("OLM status indicates error during registration: code=%s, message=%s", status.Error.Code, status.Error.Message)
				if !tm.isCurrent(gen) {
					continue
				}
				if _, isSessionExpired := sessionExpiredErrorCodes[status.Error.Code]; isSessionExpired && tm.authManager != nil {
					tm.authManager.MarkSessionExpired()
				}
				// Stop the tunnel immediately
				stopped, err := tm.disconnect(gen, StopReasonError)
				if err != nil {
					logger.Error("Failed to disconnect tunnel after error: %v", err)
				}
				// Notify UI of the error
				if stopped {
					tm.reportError(status.Error)
				}
				continue
			}
		}

		// If terminated, disconnect the tunnel
		if status.Terminated {
			logger.Info("OLM status indicates terminated, disconnecting tunnel")
			if _, err := tm.disconnect(gen, StopReasonError); err != nil {
				logger.Error("Failed to disconnect tunnel after termination: %v", err)
			}
			continue
		}

		// Update tunnel state based on OLM status
		// Only consider connected if both registered and connected
		var newState State
		if status.Connected && status.Registered {
			newState = StateRunning
			consecutiveLost = 0
		} else if status.Registered {
			newState = StateRegistered
			consecutiveLost = 0
		} else {
			// Neither connected nor registered. If we were running, the
			// tunnel dropped; after a short grace period, disconnect so
			// the tray/UI do not stay stuck on connected.
			if currentState == StateRunning {
				consecutiveLost++
				if consecutiveLost >= statusUnreachableThreshold {
					logger.Info("OLM reports not connected/registered after %d polls, disconnecting", consecutiveLost)
					if _, discErr := tm.disconnect(gen, StopReasonLost); discErr != nil {
						logger.Error("Failed to disconnect tunnel after lost connection: %v", discErr)
					}
					consecutiveLost = 0
				}
			}
			continue
		}

		// Update Manager's internal state and trigger callback (this notifies the UI)
		tm.mu.Lock()
		if tm.opGen != gen {
			tm.mu.Unlock()
			continue
		}
		oldState := tm.currentState
		if oldState == StateStopped || oldState == StateStopping ||
			(isTransitionalConnectState(oldState) && newState != StateRunning) {
			tm.mu.Unlock()
			continue
		}
		// Update the global tunnel state (for consistency with GetState())
		SetState(newState)
		tm.currentState = newState
		tm.isConnected = (newState == StateRunning)
		callback := tm.stateCallback
		tm.mu.Unlock()

		// Only trigger callback if state actually changed
		if oldState != newState && callback != nil {
			callback(newState)
		}
	}
}

// reportError passes err to the error callback.
func (tm *Manager) reportError(err *OLMStatusError) {
	tm.mu.RLock()
	errorCb := tm.errorCallback
	tm.mu.RUnlock()
	if errorCb != nil {
		errorCb(err)
	}
}

// StopStatusPolling stops the status polling
func (tm *Manager) StopStatusPolling() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.stopPollingLocked()
}

func (tm *Manager) stopPollingLocked() {
	if tm.pollCancel == nil {
		return
	}
	tm.pollCancel()
	tm.pollCancel = nil
	tm.pollCtx = nil
	logger.Info("Stopped OLM status polling")
}
