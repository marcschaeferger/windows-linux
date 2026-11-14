package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/managers/secretstore"
	"github.com/fosrl/windows-linux/secrets"
)

func TestShouldMigrateDeviceName(t *testing.T) {
	tests := []struct {
		name                                    string
		override, serverName, target, attempted string
		want                                    bool
	}{
		{"generic linux name", "", "Linux Laptop", "thinkpad", "", true},
		{"generic windows name", "", "Windows Desktop", "DESKTOP-1", "", true},
		{"user chose a name", "My PC", "Linux Laptop", "thinkpad", "", false},
		{"admin chose a name", "", "Finance laptop 3", "thinkpad", "", false},
		{"already the hostname", "", "thinkpad", "thinkpad", "", false},
		{"hostname unusable, fallback equals server", "", "Linux Laptop", "Linux Laptop", "", false},
		{"already attempted", "", "Linux Laptop", "thinkpad", "thinkpad", false},
		{"hostname changed since attempt", "", "Linux Laptop", "new-host", "thinkpad", true},
		{"no target", "", "Linux Laptop", "", "", false},
		{"lowercase is not generic", "", "linux laptop", "thinkpad", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldMigrateDeviceName(tt.override, tt.serverName, tt.target, tt.attempted); got != tt.want {
				t.Errorf("shouldMigrateDeviceName() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeOlm is another device of the user: its name and the client it last
// connected through (0 for none).
type fakeOlm struct {
	name     string
	clientID int
}

// fakeServer imitates the parts of the Pangolin API that renaming uses.
type fakeServer struct {
	mu sync.Mutex

	// knowsUserIDFilter: the server is 1.26.0 or newer, whose user-devices
	// listing accepts ?userId=. Older ones validate the query strictly and
	// answer 400 to it.
	knowsUserIDFilter bool
	olmName           string
	olmClientID       int
	orgs              []string
	otherOlms         []fakeOlm // the user's other devices
	// unlistedOlms: devices the olm listing leaves out (beyond its page).
	unlistedOlms int
	devices      map[string][]api.UserDevice
	listDenied   map[string]bool
	// updStatus is the status POST /client/:id answers with, if not 200.
	updStatus map[int]int

	// before runs ahead of each request, outside the lock; it may block.
	before func(r *http.Request)

	renamedClients []int
	calls          []string
}

var (
	reOlms   = regexp.MustCompile(`^/api/v1/user/([^/]+)/olms$`)
	reDevs   = regexp.MustCompile(`^/api/v1/org/([^/]+)/user-devices$`)
	reClient = regexp.MustCompile(`^/api/v1/client/(\d+)$`)
)

// userDevicesQueryKeys are the query keys GET /org/:orgId/user-devices
// accepts before 1.26.0 (a z.strictObject).
var userDevicesQueryKeys = []string{"pageSize", "page", "query", "sort_by", "order", "online", "agent", "status"}

func reply(w http.ResponseWriter, status int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	ok := status < 300
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "success": ok, "error": !ok, "message": message, "status": status,
	})
}

func (f *fakeServer) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// clientName returns the current name of client id.
func (f *fakeServer) clientName(id int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, devs := range f.devices {
		for _, d := range devs {
			if d.ClientId == id {
				return d.Name
			}
		}
	}
	return ""
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.before != nil {
		f.before(r)
		if r.Context().Err() != nil {
			return // the client gave up
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	notFound := func() { reply(w, 404, "The requests url is not found - "+r.URL.Path, nil) }
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	switch p := r.URL.Path; {
	case r.Method == "GET" && p == "/api/v1/my-device":
		orgs := []map[string]any{}
		for _, o := range f.orgs {
			orgs = append(orgs, map[string]any{"orgId": o, "orgName": o, "roleId": 1})
		}
		olm := map[string]any{"olmId": r.URL.Query().Get("olmId"), "userId": "u1", "name": f.olmName}
		if f.olmClientID != 0 {
			olm["clientId"] = f.olmClientID
		}
		reply(w, 200, "ok", map[string]any{"user": map[string]any{"userId": "u1"}, "orgs": orgs, "olm": olm})
	case r.Method == "GET" && reOlms.MatchString(p):
		olms := []map[string]any{{"olmId": "olm1", "name": f.olmName}}
		for i, o := range f.otherOlms {
			olm := map[string]any{"olmId": "other" + strconv.Itoa(i), "name": o.name}
			if o.clientID != 0 {
				olm["clientId"] = o.clientID
			}
			olms = append(olms, olm)
		}
		total := len(olms) + f.unlistedOlms
		reply(w, 200, "ok", map[string]any{"olms": olms, "pagination": map[string]any{"total": total, "limit": 1000, "offset": 0}})
	case r.Method == "GET" && reDevs.MatchString(p):
		org := reDevs.FindStringSubmatch(p)[1]
		if f.listDenied[org] {
			reply(w, 403, "User does not have permission to perform this action", nil)
			return
		}
		q := r.URL.Query()
		for k := range q {
			if !slices.Contains(userDevicesQueryKeys, k) && (k != "userId" || !f.knowsUserIDFilter) {
				reply(w, 400, fmt.Sprintf("Validation error: Unrecognized key: %q", k), nil)
				return
			}
		}
		devices := []api.UserDevice{}
		for _, d := range f.devices[org] {
			if uid := q.Get("userId"); uid == "" || (d.UserId != nil && *d.UserId == uid) {
				devices = append(devices, d)
			}
		}
		reply(w, 200, "ok", map[string]any{"devices": devices})
	case r.Method == "POST" && reClient.MatchString(p):
		id, _ := strconv.Atoi(reClient.FindStringSubmatch(p)[1])
		switch f.updStatus[id] {
		case 0:
		case 403:
			reply(w, 403, "User does not have permission to perform this action", nil)
			return
		default:
			reply(w, f.updStatus[id], "Failed to update client", nil)
			return
		}
		f.renamedClients = append(f.renamedClients, id)
		for org, devs := range f.devices {
			for i := range devs {
				if devs[i].ClientId == id {
					f.devices[org][i].Name = body.Name
				}
			}
		}
		reply(w, 200, "Client updated", map[string]any{"clientId": id, "name": body.Name})
	default:
		notFound()
	}
}

func userDevice(clientID int, org, name string) api.UserDevice {
	return userDeviceOf("u1", clientID, org, name)
}

func userDeviceOf(user string, clientID int, org, name string) api.UserDevice {
	return api.UserDevice{ClientId: clientID, OrgId: org, Name: name, UserId: &user}
}

// oneOrg: org a holds the olm's client 10, named "Linux Laptop".
func oneOrg(f *fakeServer) {
	f.knowsUserIDFilter = true
	f.olmName = "Linux Laptop"
	f.olmClientID = 10
	f.orgs = []string{"a"}
	f.devices = map[string][]api.UserDevice{"a": {userDevice(10, "a", "Linux Laptop")}}
}

// twoOrgs: org a holds the olm's current client (10), org b this device's
// client 20; 11 and 21 are the user's other device, "phone", which last
// connected through 21.
func twoOrgs(f *fakeServer) {
	f.knowsUserIDFilter = true
	f.olmName = "Linux Laptop"
	f.olmClientID = 10
	f.orgs = []string{"a", "b"}
	f.otherOlms = []fakeOlm{{"phone", 21}}
	f.devices = map[string][]api.UserDevice{
		"a": {userDevice(10, "a", "Linux Laptop"), userDevice(11, "a", "phone")},
		"b": {userDevice(20, "b", "Linux Laptop"), userDevice(21, "b", "phone")},
	}
}

func TestSyncDeviceName(t *testing.T) {
	legacy := config.LegacyDeviceNames()
	tests := []struct {
		name        string
		setup       func(*fakeServer)
		onlyFrom    []string
		wantStatus  DeviceNameSyncStatus
		wantClients []int
		wantRenamed int
		wantDenied  int
		wantFailed  int
		wantUnsure  int
		transient   bool
	}{
		{
			name: "server error is transient",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.updStatus = map[int]int{10: 500, 20: 500}
			},
			wantStatus: DeviceNameFailed, wantFailed: 2, transient: true,
		},
		{
			name: "expired session is transient",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.updStatus = map[int]int{10: 401, 20: 401}
			},
			wantStatus: DeviceNameFailed, wantFailed: 2, transient: true,
		},
		{
			name:        "admin: renames each org's client",
			setup:       twoOrgs,
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{10, 20}, wantRenamed: 2,
		},
		{
			name: "server before 1.26 (no userId filter), admin: renames each org's client",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.knowsUserIDFilter = false
				// Without the filter an admin sees everyone's devices.
				f.devices["b"] = append(f.devices["b"], userDeviceOf("u2", 22, "b", "Linux Laptop"))
			},
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{10, 20}, wantRenamed: 2,
		},
		{
			name: "member role: permission denied",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.listDenied = map[string]bool{"a": true, "b": true}
				f.updStatus = map[int]int{10: 403, 20: 403}
			},
			wantStatus: DeviceNamePermissionDenied,
			wantDenied: 2, // two orgs, however many calls were refused
		},
		{
			name: "allowed in one org only: partial",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.listDenied = map[string]bool{"b": true}
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantDenied: 1,
		},
		{
			name: "an update fails: partial, not blamed on the role",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.updStatus = map[int]int{20: 500}
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantFailed: 1,
		},
		{
			name: "known client's org can't be listed: counted once",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.listDenied = map[string]bool{"a": true}
			},
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{10, 20}, wantRenamed: 2,
		},
		{
			name: "single device: renames each org's client by name",
			setup: func(f *fakeServer) {
				oneOrg(f)
				f.orgs = []string{"a", "b"}
				f.devices["b"] = []api.UserDevice{userDevice(20, "b", "Linux Laptop")}
			},
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{10, 20}, wantRenamed: 2,
		},
		{
			name: "another device with the same name, accounted for in org b: renamed",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.devices["b"][1].Name = "Linux Laptop"
				f.otherOlms = []fakeOlm{{"Linux Laptop", 21}}
			},
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{10, 20}, wantRenamed: 2,
		},
		{
			// The review case: the other device was renamed (its olm took
			// the name of the client it connects through, in org a), but its
			// org-b client kept this device's name, and this device has no
			// client in org b.
			name: "another device's client in org b has this device's name: left alone",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.otherOlms = []fakeOlm{{"phone", 11}}
				f.devices["b"] = []api.UserDevice{userDevice(21, "b", "Linux Laptop")}
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantUnsure: 1,
		},
		{
			name: "the other device last connected elsewhere: org b left alone",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.otherOlms = []fakeOlm{{"phone", 11}}
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantUnsure: 1,
		},
		{
			name: "the other device never connected: org b left alone",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.otherOlms = []fakeOlm{{"phone", 0}}
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantUnsure: 1,
		},
		{
			name: "the device listing is cut short: org b left alone",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.unlistedOlms = 1
			},
			wantStatus:  DeviceNamePartial,
			wantClients: []int{10}, wantRenamed: 1, wantUnsure: 1,
		},
		{
			name: "no known client and org b unsure: failed, nothing renamed",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.olmClientID = 0
				f.orgs = []string{"b"}
				f.otherOlms = []fakeOlm{{"phone", 11}}
			},
			wantStatus: DeviceNameFailed, wantUnsure: 1,
		},
		{
			name: "ambiguous names in an org: skipped",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.olmClientID = 0
				f.orgs = []string{"b"}
				f.devices["b"] = append(f.devices["b"], userDevice(22, "b", "Linux Laptop"))
			},
			wantStatus: DeviceNameFailed, wantUnsure: 1,
		},
		{
			name: "automatic: a client an admin renamed is kept",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.devices["a"][0].Name = "Finance laptop 3"
			},
			onlyFrom:    legacy,
			wantStatus:  DeviceNameRenamed,
			wantClients: []int{20}, wantRenamed: 1,
		},
		{
			name: "automatic: the known client's name can't be checked",
			setup: func(f *fakeServer) {
				twoOrgs(f)
				f.listDenied = map[string]bool{"a": true}
			},
			onlyFrom:    legacy,
			wantStatus:  DeviceNamePartial,
			wantClients: []int{20}, wantRenamed: 1, wantDenied: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			tt.setup(f)
			srv := httptest.NewServer(f)
			defer srv.Close()

			client := api.NewAPIClient(srv.URL, "tok")
			var got DeviceNameSyncResult
			if tt.onlyFrom == nil {
				got = syncDeviceName(context.Background(), client, "u1", "olm1", "new-host")
			} else {
				myDevice, err := client.GetMyDeviceContext(context.Background(), "olm1")
				if err != nil {
					t.Fatal(err)
				}
				got = renameDeviceClients(context.Background(), client, "u1", "olm1", myDevice, "new-host", tt.onlyFrom)
			}

			if got.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s (err %v, calls %v)", got.Status, tt.wantStatus, got.Err, f.calls)
			}
			slices.Sort(f.renamedClients)
			if !slices.Equal(f.renamedClients, tt.wantClients) {
				t.Errorf("renamed clients %v, want %v", f.renamedClients, tt.wantClients)
			}
			if got.Renamed != tt.wantRenamed || got.Denied != tt.wantDenied || got.Failed != tt.wantFailed || got.Unsure != tt.wantUnsure {
				t.Errorf("counts renamed=%d denied=%d failed=%d unsure=%d, want %d/%d/%d/%d (err %v)",
					got.Renamed, got.Denied, got.Failed, got.Unsure, tt.wantRenamed, tt.wantDenied, tt.wantFailed, tt.wantUnsure, got.Err)
			}
			if got.transient() != tt.transient {
				t.Errorf("transient = %v, want %v (err %v)", got.transient(), tt.transient, got.Err)
			}
			if got.Message() == "" {
				t.Error("empty message")
			}
			if got.Denied == 0 && strings.Contains(got.Message(), "administrator") {
				t.Errorf("message blames permissions without a refusal: %q", got.Message())
			}
		})
	}
}

func TestSyncDeviceNameNetworkErrorIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more

	got := syncDeviceName(context.Background(), api.NewAPIClient(url, "tok"), "u1", "olm1", "x")
	if got.Status != DeviceNameFailed || !got.transient() {
		t.Errorf("status %s transient %v, want a transient failure (err %v)", got.Status, got.transient(), got.Err)
	}
}

func TestSyncDeviceNameMessages(t *testing.T) {
	err := fmt.Errorf("HTTP error 500")
	tests := []struct {
		r    DeviceNameSyncResult
		want string
	}{
		{DeviceNameSyncResult{Status: DeviceNameRenamed}, "Renamed on the server."},
		{DeviceNameSyncResult{Status: DeviceNamePermissionDenied}, "Saved. Renaming devices on the server needs an administrator (the updateClient permission); ask one to rename this device."},
		{DeviceNameSyncResult{Status: DeviceNamePartial, Renamed: 1, Denied: 2}, "Renamed in 1 of 3 organizations. In the others, renaming devices needs an administrator (the updateClient permission)."},
		{DeviceNameSyncResult{Status: DeviceNamePartial, Renamed: 2, Failed: 1, Err: err}, "Renamed in 2 of 3 organizations. Renaming failed in the others: HTTP error 500"},
		{DeviceNameSyncResult{Status: DeviceNamePartial, Renamed: 1, Denied: 1, Failed: 1, Err: err}, "Renamed in 1 of 3 organizations. Renaming devices needs an administrator (the updateClient permission) in 1; renaming failed in 1: HTTP error 500"},
		{DeviceNameSyncResult{Status: DeviceNamePartial, Renamed: 1, Unsure: 1}, "Renamed in 1 of 2 organizations. In 1, this device couldn't be told apart from your other devices; rename it there in the dashboard."},
		{DeviceNameSyncResult{Status: DeviceNamePartial, Renamed: 1, Denied: 1, Failed: 1, Unsure: 2, Err: err}, "Renamed in 1 of 5 organizations. Renaming devices needs an administrator (the updateClient permission) in 1. Renaming failed in 1: HTTP error 500. In 2, this device couldn't be told apart from your other devices; rename it there in the dashboard."},
	}
	for _, tt := range tests {
		if got := tt.r.Message(); got != tt.want {
			t.Errorf("%s: Message() = %q, want %q", tt.r.Status, got, tt.want)
		}
	}
}

// fakeIPC stands in for the manager service's secret store.
type fakeIPC struct{ olmID string }

func (f fakeIPC) Ready() bool { return true }
func (f fakeIPC) GetUserSecrets(string) (secretstore.UserSecrets, error) {
	return secretstore.UserSecrets{OlmId: f.olmID, OlmSecret: "s"}, nil
}
func (f fakeIPC) SaveUserSecrets(string, secretstore.SecretsUpdate) error        { return nil }
func (f fakeIPC) DeleteUserSecrets(string, secretstore.DeleteSecretsFlags) error { return nil }

// newTestAuthManager builds an AuthManager for user u1 with device olm1
// against url, with config and accounts in a temporary directory, and the
// computer named "new-host".
func newTestAuthManager(t *testing.T, url string) *AuthManager {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prevName := defaultDeviceName
	defaultDeviceName = func() string { return "new-host" }
	secrets.SetIPCAPI(fakeIPC{olmID: "olm1"})
	t.Cleanup(func() {
		defaultDeviceName = prevName
		secrets.SetIPCAPI(nil)
	})

	accounts := config.NewAccountManager()
	if err := accounts.AddAccount(config.Account{UserID: "u1", Hostname: url}); err != nil {
		t.Fatal(err)
	}
	am := NewAuthManager(api.NewAPIClient(url, "tok"), config.NewConfigManager(), accounts, secrets.NewSecretManager())
	am.currentUser = &api.User{Id: "u1", UserId: "u1"}
	return am
}

func setOverride(t *testing.T, cm *config.ConfigManager, name string) {
	t.Helper()
	cfg := cm.GetConfigCopy()
	cfg.DeviceName = &name
	if !cm.Save(cfg) {
		t.Fatal("could not save the device name")
	}
}

func TestMigrateDeviceName(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*fakeServer)
		override   string
		wantName   string // client 10's name afterwards
		wantRecord string
		// wantSecondCalls is how many requests a second start makes.
		wantSecondCalls int
	}{
		{
			name:       "generic name: renamed once and recorded",
			setup:      oneOrg,
			wantName:   "new-host",
			wantRecord: "new-host",
		},
		{
			name:     "user chose a name: nothing is asked or renamed",
			setup:    oneOrg,
			override: "my pc",
			wantName: "Linux Laptop",
		},
		{
			name:       "already the hostname: recorded, not asked again",
			setup:      func(f *fakeServer) { oneOrg(f); f.olmName = "new-host"; f.devices["a"][0].Name = "new-host" },
			wantName:   "new-host",
			wantRecord: "new-host",
		},
		{
			name: "a name someone chose: kept, recorded, not asked again",
			setup: func(f *fakeServer) {
				oneOrg(f)
				f.olmName = "Finance laptop 3"
				f.devices["a"][0].Name = "Finance laptop 3"
			},
			wantName:   "Finance laptop 3",
			wantRecord: "new-host",
		},
		{
			name:            "server error: not recorded, retried on the next start",
			setup:           func(f *fakeServer) { oneOrg(f); f.updStatus = map[int]int{10: 500} },
			wantName:        "Linux Laptop",
			wantSecondCalls: 4, // my-device, olms, user-devices and the rename again
		},
		{
			name:            "session expired: not recorded, retried on the next start",
			setup:           func(f *fakeServer) { oneOrg(f); f.updStatus = map[int]int{10: 401} },
			wantName:        "Linux Laptop",
			wantSecondCalls: 4,
		},
		{
			name: "member role: final, recorded, not asked again",
			setup: func(f *fakeServer) {
				oneOrg(f)
				f.listDenied = map[string]bool{"a": true}
				f.updStatus = map[int]int{10: 403}
			},
			wantName:   "Linux Laptop",
			wantRecord: "new-host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{}
			tt.setup(f)
			srv := httptest.NewServer(f)
			defer srv.Close()
			am := newTestAuthManager(t, srv.URL)
			if tt.override != "" {
				setOverride(t, am.configManager, tt.override)
			}

			am.migrateDeviceName(context.Background(), "u1")
			if got := f.clientName(10); got != tt.wantName {
				t.Errorf("client 10 is named %q, want %q (calls %v)", got, tt.wantName, f.calls)
			}
			if got := am.accountManager.GetDeviceNameMigration("u1", "olm1"); got != tt.wantRecord {
				t.Errorf("recorded %q, want %q", got, tt.wantRecord)
			}
			if tt.override != "" && len(f.calls) != 0 {
				t.Errorf("asked the server despite the user's name: %v", f.calls)
			}

			before := len(f.calls)
			am.migrateDeviceName(context.Background(), "u1")
			if got := len(f.calls) - before; got != tt.wantSecondCalls {
				t.Errorf("second start made %d requests %v, want %d", got, f.calls[before:], tt.wantSecondCalls)
			}
		})
	}
}

// A name the user saves while the automatic rename asks the server wins.
func TestMigrateDeviceNameYieldsToUserName(t *testing.T) {
	f := &fakeServer{}
	oneOrg(f)
	srv := httptest.NewServer(f)
	defer srv.Close()
	am := newTestAuthManager(t, srv.URL)
	f.before = func(r *http.Request) {
		if r.URL.Path == "/api/v1/my-device" {
			setOverride(t, am.configManager, "my pc")
		}
	}

	am.migrateDeviceName(context.Background(), "u1")
	if len(f.renamedClients) != 0 {
		t.Errorf("automatic rename of clients %v overwrote the user's name", f.renamedClients)
	}
}

// Saving a name stops a running automatic rename, and only one runs at once.
func TestStartDeviceNameMigrationDedupAndCancel(t *testing.T) {
	f := &fakeServer{}
	oneOrg(f)
	srv := httptest.NewServer(f)
	defer srv.Close()
	am := newTestAuthManager(t, srv.URL)

	blocked := make(chan struct{})
	var once sync.Once
	f.before = func(r *http.Request) {
		// The automatic rename hangs on the server until it is cancelled.
		if r.Method != "POST" {
			return
		}
		data, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		if strings.Contains(string(data), `"new-host"`) {
			once.Do(func() { close(blocked) })
			<-r.Context().Done()
		}
	}

	am.startDeviceNameMigration("u1")
	am.startDeviceNameMigration("u1") // already running: ignored
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the automatic rename never reached the server")
	}

	setOverride(t, am.configManager, "my pc")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := am.SyncDeviceName(ctx, "my pc")
	if res.Status != DeviceNameRenamed {
		t.Fatalf("manual rename: %s (%v)", res.Status, res.Err)
	}

	am.deviceNameMigrating.Lock() // wait for the background rename to end
	am.deviceNameMigrating.Unlock()

	// Once by the single background rename, once by the user's.
	if got := f.callCount("GET /api/v1/my-device"); got != 2 {
		t.Errorf("my-device asked %d times, want 2 (dedup)", got)
	}
	if got := f.clientName(10); got != "my pc" {
		t.Errorf("final name %q, want the user's", got)
	}
	if got := am.accountManager.GetDeviceNameMigration("u1", "olm1"); got != "" {
		t.Errorf("cancelled rename recorded as %q", got)
	}
}
