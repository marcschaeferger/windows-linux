package main

import (
	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/auth"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/secrets"
	"github.com/fosrl/windows-linux/ui"
)

// runUI creates the managers the UI works with and runs the tray UI until it
// quits. startupWarning, if set, is shown once the UI is up.
func runUI(startupWarning string) error {
	// Initialize managers
	accountManager := config.NewAccountManager()
	configManager := config.NewConfigManager()
	secretManager := secrets.NewSecretManager()

	var hostname string
	if activeAccount, _ := accountManager.ActiveAccount(); activeAccount != nil {
		hostname = activeAccount.Hostname
	} else {
		hostname = config.DefaultHostname
	}

	apiClient := api.NewAPIClient(hostname, "")
	apiClient.SetSessionCookieName(configManager.GetSessionCookieName())
	authManager := auth.NewAuthManager(apiClient, configManager, accountManager, secretManager)

	// When any authenticated request gets 401, mark the session expired.
	// This runs asynchronously, as walk's Synchronize did, because the request
	// may have been made while the auth manager holds its lock.
	// It only acts while the client still uses the rejected token, so a switch
	// in the meantime can't expire the next account.
	apiClient.SetOnUnauthorized(func(token string) {
		go authManager.MarkSessionExpiredFor(token)
	})

	// Initialize auth manager (loads saved session token if available)
	if err := authManager.Initialize(); err != nil {
		logger.Error("Failed to initialize auth manager: %v", err)
	}

	return ui.Run(ui.Deps{
		Auth:     authManager,
		Config:   configManager,
		Accounts: accountManager,
		API:      apiClient,
		Secrets:  secretManager,

		StartupWarning: startupWarning,
	})
}
