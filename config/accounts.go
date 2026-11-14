package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/fosrl/newt/logger"
)

const (
	AccountsFileName = "accounts.json"
)

// AccountManager holds the saved accounts. Its state is only reached through
// its methods, which take mu: the UI reads it from many goroutines while
// others write it, and an unlocked map iteration during a write is a fatal
// runtime error.
type AccountManager struct {
	mu sync.RWMutex

	path string
	// readOnly is set when the file exists but could not be read; saving
	// would then replace accounts this run never saw.
	readOnly bool

	activeUserID string
	accounts     map[string]Account
}

// accountsFile is the on-disk form of accounts.json.
type accountsFile struct {
	ActiveUserID string             `json:"activeUserId"`
	Accounts     map[string]Account `json:"accounts"`
}

type Account struct {
	UserID   string `json:"userId"`
	Email    string `json:"email"`
	OrgID    string `json:"orgId"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`

	OrgSettings map[string]OrgSettings `json:"orgSettings,omitempty"`

	// DeviceNameMigrations records, per olm ID, the name this device was last
	// automatically renamed to from a generic "<OS> Laptop/Desktop" name, so
	// the rename isn't retried on every start (e.g. when the user's role may not
	// rename devices).
	DeviceNameMigrations map[string]string `json:"deviceNameMigrations,omitempty"`
}

type OrgSettings struct {
	ExitNodeResourceID int `json:"exitNodeResourceId,omitempty"`

	// The settings the org's server manages for this user, as last fetched
	// from it. Refreshed on every connect and whenever Preferences opens; only
	// kept here so they still apply if the server can't be reached.
	ServerSettings *ClientSettings `json:"serverSettings,omitempty"`
}

func (s OrgSettings) IsEmpty() bool {
	return s.ExitNodeResourceID == 0 && s.ServerSettings == nil
}

func NewAccountManager() *AccountManager {
	pangolinDir := UserDataDir()
	// Create directory if it doesn't exist
	if err := os.MkdirAll(pangolinDir, 0o755); err != nil {
		logger.Error("Failed to create config directory: %v", err)
	}
	return loadAccountManager(filepath.Join(pangolinDir, AccountsFileName))
}

// loadAccountManager reads the accounts file at path. A missing file starts
// empty. A file that can't be parsed is copied aside first, so the next save
// does not silently drop every account in it.
func loadAccountManager(path string) *AccountManager {
	mgr := &AccountManager{
		path:     path,
		accounts: make(map[string]Account),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := mgr.Save(); err != nil {
				logger.Error("failed to create accounts file: %v", err)
			}
			return mgr
		}
		logger.Error("failed to read accounts file, not saving changes to it: %v", err)
		mgr.readOnly = true
		return mgr
	}

	var file accountsFile
	if err := json.Unmarshal(data, &file); err != nil {
		logger.Error("failed to parse accounts file: %v", err)
		backupCorruptFile(path, data, accountsFileMode)
		return mgr
	}

	if file.Accounts != nil {
		mgr.accounts = file.Accounts
	}
	mgr.activeUserID = file.ActiveUserID
	return mgr
}

// accountsFileMode keeps accounts.json private to the user.
const accountsFileMode = 0o600

func (m *AccountManager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saveLocked()
}

func (m *AccountManager) saveLocked() error {
	if m.readOnly {
		return errors.New("accounts file could not be read at startup; not overwriting it")
	}
	data, err := json.MarshalIndent(accountsFile{ActiveUserID: m.activeUserID, Accounts: m.accounts}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, data, accountsFileMode)
}

// ActiveUserID returns the active account's user ID, or "" if none is active.
func (m *AccountManager) ActiveUserID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeUserID
}

// Account returns a copy of the account for userID.
func (m *AccountManager) Account(userID string) (Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	account, ok := m.accounts[userID]
	if !ok {
		return Account{}, false
	}
	return copyAccount(account), true
}

// Accounts returns a copy of every account, in no particular order.
func (m *AccountManager) Accounts() []Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Account, 0, len(m.accounts))
	for _, account := range m.accounts {
		out = append(out, copyAccount(account))
	}
	return out
}

// HasAccounts reports whether any account is saved.
func (m *AccountManager) HasAccounts() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.accounts) > 0
}

// copyAccount copies an account so callers can't share its OrgSettings or
// DeviceNameMigrations map with the manager.
func copyAccount(a Account) Account {
	if a.OrgSettings != nil {
		settings := make(map[string]OrgSettings, len(a.OrgSettings))
		for id, s := range a.OrgSettings {
			settings[id] = s
		}
		a.OrgSettings = settings
	}
	if a.DeviceNameMigrations != nil {
		migrations := make(map[string]string, len(a.DeviceNameMigrations))
		for id, n := range a.DeviceNameMigrations {
			migrations[id] = n
		}
		a.DeviceNameMigrations = migrations
	}
	return a
}

func (m *AccountManager) AddAccount(account Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.accounts[account.UserID]; ok {
		if len(existing.OrgSettings) > 0 {
			account.OrgSettings = existing.OrgSettings
		}
		if len(existing.DeviceNameMigrations) > 0 && account.DeviceNameMigrations == nil {
			account.DeviceNameMigrations = existing.DeviceNameMigrations
		}
	}
	m.accounts[account.UserID] = account
	return m.saveLocked()
}

func (m *AccountManager) RemoveAccount(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.accounts, userID)

	if m.activeUserID == userID {
		m.activeUserID = ""
	}

	return m.saveLocked()
}

func (m *AccountManager) ActiveAccount() (*Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.activeUserID == "" {
		return nil, errors.New("no active account")
	}

	account, ok := m.accounts[m.activeUserID]
	if !ok {
		return nil, errors.New("active account not present in list")
	}

	account = copyAccount(account)
	return &account, nil
}

func (m *AccountManager) SetActiveUser(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.accounts[userID]; !ok {
		return errors.New("account does not exist")
	}
	if m.activeUserID == userID {
		return nil
	}

	m.activeUserID = userID
	return m.saveLocked()
}

func (m *AccountManager) SetUserOrganization(userID string, orgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if account, ok := m.accounts[userID]; ok {
		if account.OrgID == orgID {
			return nil
		}
		account.OrgID = orgID
		m.accounts[userID] = account // Put the modified account back in the map
	} else {
		return errors.New("account does not exist")
	}

	return m.saveLocked()
}

// GetOrgSettings returns the settings for orgID under userID, or empty defaults
// if none are stored or the account doesn't exist.
func (m *AccountManager) GetOrgSettings(userID, orgID string) OrgSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.accounts[userID].OrgSettings[orgID]
}

// UpdateOrgSettings updates the settings for orgID under userID. Empty settings
// remove that org's entry.
func (m *AccountManager) UpdateOrgSettings(userID, orgID string, update func(*OrgSettings)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, ok := m.accounts[userID]
	if !ok {
		return errors.New("account does not exist")
	}

	settings := make(map[string]OrgSettings, len(account.OrgSettings)+1)
	for id, s := range account.OrgSettings {
		settings[id] = s
	}
	org := settings[orgID]
	update(&org)
	if org.IsEmpty() {
		delete(settings, orgID)
	} else {
		settings[orgID] = org
	}
	if len(settings) == 0 {
		settings = nil
	}
	account.OrgSettings = settings
	m.accounts[userID] = account
	return m.saveLocked()
}

// GetServerSettings returns the settings last fetched from the server for orgID
// under userID, or nil if it manages none.
func (m *AccountManager) GetServerSettings(userID, orgID string) *ClientSettings {
	return m.GetOrgSettings(userID, orgID).ServerSettings
}

// SetServerSettings records the settings fetched from the server for orgID
// under userID.
func (m *AccountManager) SetServerSettings(userID, orgID string, settings *ClientSettings) error {
	return m.UpdateOrgSettings(userID, orgID, func(s *OrgSettings) {
		s.ServerSettings = settings
	})
}

// GetExitNode returns the resource ID of the exit node selected for orgID under
// userID, or 0 if none is selected or the account doesn't exist.
func (m *AccountManager) GetExitNode(userID, orgID string) int {
	return m.GetOrgSettings(userID, orgID).ExitNodeResourceID
}

// SetExitNode records the selected exit node (a gateway resource) for orgID
// under userID; resourceID 0 clears it.
func (m *AccountManager) SetExitNode(userID, orgID string, resourceID int) error {
	return m.UpdateOrgSettings(userID, orgID, func(s *OrgSettings) {
		s.ExitNodeResourceID = resourceID
	})
}

func (m *AccountManager) UpdateAccountUserInfo(userID, username, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if account, ok := m.accounts[userID]; ok {
		// Called on every tray open; don't rewrite the file for nothing.
		if account.Username == username && account.Name == name {
			return nil
		}
		account.Username = username
		account.Name = name
		m.accounts[userID] = account // Put the modified account back in the map
	} else {
		return errors.New("account does not exist")
	}

	return m.saveLocked()
}

// GetDeviceNameMigration returns the name the device olmID of userID was last
// automatically renamed to, or "" if that was never attempted.
func (m *AccountManager) GetDeviceNameMigration(userID, olmID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.accounts[userID].DeviceNameMigrations[olmID]
}

// SetDeviceNameMigration records that the device olmID of userID was
// automatically renamed to name (successfully or not).
func (m *AccountManager) SetDeviceNameMigration(userID, olmID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	account, ok := m.accounts[userID]
	if !ok {
		return errors.New("account does not exist")
	}
	migrations := make(map[string]string, len(account.DeviceNameMigrations)+1)
	for id, n := range account.DeviceNameMigrations {
		migrations[id] = n
	}
	migrations[olmID] = name
	account.DeviceNameMigrations = migrations
	m.accounts[userID] = account
	return m.saveLocked()
}
