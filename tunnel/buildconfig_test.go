package tunnel

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/auth"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/managers/secretstore"
	"github.com/fosrl/windows-linux/secrets"
)

type memSecrets struct {
	mu   sync.Mutex
	data map[string]secretstore.UserSecrets
}

func (m *memSecrets) Ready() bool { return true }
func (m *memSecrets) GetUserSecrets(id string) (secretstore.UserSecrets, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[id], nil
}
func (m *memSecrets) SaveUserSecrets(string, secretstore.SecretsUpdate) error        { return nil }
func (m *memSecrets) DeleteUserSecrets(string, secretstore.DeleteSecretsFlags) error { return nil }

// Audit #1. buildConfig used to dereference authManager.CurrentUser(), which
// is nil while the client is authenticated but the server was down at start.
// Connect then panicked with a nil pointer. The state is reached the real way:
// Initialize with a failing health check and a cached account org.
func TestBuildConfigWithoutCurrentUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()

	store := &memSecrets{data: map[string]secretstore.UserSecrets{
		"user-1": {SessionToken: "tok", OlmId: "olm-id", OlmSecret: "olm-secret"},
	}}
	secrets.SetIPCAPI(store)
	defer secrets.SetIPCAPI(nil)

	accounts := config.NewAccountManager()
	if err := accounts.AddAccount(config.Account{UserID: "user-1", OrgID: "org-1", Hostname: down.URL}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.SetActiveUser("user-1"); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigManager()
	sm := secrets.NewSecretManager()
	client := api.NewAPIClient(down.URL, "")
	am := auth.NewAuthManager(client, cfg, accounts, sm)
	if err := am.Initialize(); err != nil {
		t.Fatal(err)
	}

	// Preconditions: the state that triggered the panic.
	if !am.IsAuthenticated() || !am.IsServerDown() {
		t.Fatalf("setup: authenticated=%v serverDown=%v", am.IsAuthenticated(), am.IsServerDown())
	}
	if am.CurrentUser() != nil {
		t.Fatal("setup: CurrentUser should be nil")
	}
	if o := am.CurrentOrg(); o == nil || o.Id != "org-1" {
		t.Fatalf("setup: CurrentOrg = %+v", o)
	}

	tm := NewManager(am, cfg, accounts, sm, nil)

	var got Config
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("buildConfig panicked: %v", r)
			}
		}()
		got, err = tm.buildConfig()
	}()
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if got.ID != "olm-id" || got.Secret != "olm-secret" {
		t.Errorf("OLM credentials = %q/%q, want those of the active account user-1", got.ID, got.Secret)
	}
	if got.UserToken != "tok" || got.OrgID != "org-1" || got.Endpoint != down.URL {
		t.Errorf("config = token %q org %q endpoint %q", got.UserToken, got.OrgID, got.Endpoint)
	}
}
