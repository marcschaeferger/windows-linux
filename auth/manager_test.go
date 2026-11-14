package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/api"
	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/managers/secretstore"
	"github.com/fosrl/windows-linux/secrets"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeStore is an in-memory secrets.IPCAPI.
type fakeStore struct {
	mu       sync.Mutex
	data     map[string]secretstore.UserSecrets
	failSave atomic.Bool
	// getHook, when set, runs at the start of every GetUserSecrets call
	// (before the store lock), so a test can stall a reader.
	getHook atomic.Pointer[func(userID string)]
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: map[string]secretstore.UserSecrets{}}
}

func (f *fakeStore) Ready() bool { return true }

func (f *fakeStore) GetUserSecrets(userID string) (secretstore.UserSecrets, error) {
	if h := f.getHook.Load(); h != nil {
		(*h)(userID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[userID], nil
}

func (f *fakeStore) SaveUserSecrets(userID string, u secretstore.SecretsUpdate) error {
	if f.failSave.Load() {
		return errors.New("save failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.data[userID]
	if u.SetSessionToken {
		cur.SessionToken = u.Secrets.SessionToken
	}
	if u.SetOlmId {
		cur.OlmId = u.Secrets.OlmId
	}
	if u.SetOlmSecret {
		cur.OlmSecret = u.Secrets.OlmSecret
	}
	f.data[userID] = cur
	return nil
}

func (f *fakeStore) DeleteUserSecrets(userID string, fl secretstore.DeleteSecretsFlags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.data[userID]
	if fl.SessionToken {
		cur.SessionToken = ""
	}
	if fl.OlmCredentials {
		cur.OlmId, cur.OlmSecret = "", ""
	}
	f.data[userID] = cur
	return nil
}

func (f *fakeStore) token(userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[userID].SessionToken
}

// fakeAPI is a Pangolin API server. Every endpoint has a sensible default
// derived from the session cookie; a test overrides single endpoints through
// the handler fields (set them before the first request, or under mu).
type fakeAPI struct {
	srv *httptest.Server

	mu     sync.Mutex
	users  map[string]api.User  // session token -> user
	orgs   map[string][]api.Org // user id -> orgs
	health http.HandlerFunc
	user   http.HandlerFunc
	orgsH  http.HandlerFunc
	info   http.HandlerFunc
	orgH   http.HandlerFunc // GET /org/{id}
	logout http.HandlerFunc
	start  http.HandlerFunc
	poll   http.HandlerFunc
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "success": status < 300, "message": http.StatusText(status)})
}

func strp(s string) *string { return &s }

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{users: map[string]api.User{}, orgs: map[string][]api.Org{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) addUser(token, id string, orgs ...api.Org) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[token] = api.User{Id: id, UserId: id, Email: id + "@example.com", Username: strp("u-" + id)}
	f.orgs[id] = orgs
}

func (f *fakeAPI) set(field *http.HandlerFunc, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*field = h
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1"), "/")
	f.mu.Lock()
	var override http.HandlerFunc
	switch {
	case p == "":
		override = f.health
	case p == "/user":
		override = f.user
	case strings.HasPrefix(p, "/user/") && strings.HasSuffix(p, "/orgs"):
		override = f.orgsH
	case p == "/server-info":
		override = f.info
	case strings.HasPrefix(p, "/org/") && strings.Count(p, "/") == 2:
		override = f.orgH
	case p == "/auth/logout":
		override = f.logout
	case p == "/auth/device-web-auth/start":
		override = f.start
	case strings.HasPrefix(p, "/auth/device-web-auth/poll/"):
		override = f.poll
	}
	f.mu.Unlock()
	if override != nil {
		override(w, r)
		return
	}

	switch {
	case p == "":
		writeJSON(w, 200, map[string]any{})
	case p == "/user":
		f.defaultUser(w, r)
	case strings.HasPrefix(p, "/user/") && strings.HasSuffix(p, "/orgs"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/user/"), "/orgs")
		f.mu.Lock()
		o := f.orgs[id]
		f.mu.Unlock()
		writeJSON(w, 200, api.ListUserOrgsResponse{Orgs: o})
	case p == "/server-info":
		writeJSON(w, 200, api.ServerInfo{Version: "1.0.0", Build: "oss"})
	case strings.HasPrefix(p, "/org/") && strings.Count(p, "/") == 2:
		id := strings.TrimPrefix(p, "/org/")
		writeJSON(w, 200, api.GetOrgResponse{Id: id, Name: id})
	case p == "/auth/logout":
		writeJSON(w, 200, map[string]any{})
	case p == "/auth/device-web-auth/start":
		writeJSON(w, 200, api.DeviceAuthStartResponse{Code: "CODE", ExpiresInSeconds: 60})
	case strings.HasPrefix(p, "/auth/device-web-auth/poll/"):
		writeJSON(w, 200, api.DeviceAuthPollResponse{Verified: false})
	default:
		writeJSON(w, 404, nil)
	}
}

func (f *fakeAPI) defaultUser(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("p_session_token")
	if err != nil {
		writeJSON(w, 401, nil)
		return
	}
	f.mu.Lock()
	u, ok := f.users[c.Value]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 401, nil)
		return
	}
	writeJSON(w, 200, u)
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { writeJSON(w, code, nil) }
}

// env wires an AuthManager like main.go does, on temp config dirs, a fake
// secret store and the given server as the active account's host.
type env struct {
	t       *testing.T
	am      *AuthManager
	client  *api.APIClient
	acc     *config.AccountManager
	store   *fakeStore
	unauthN atomic.Int32 // times the API layer reported a 401 on the session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("HOME", cfg)
	t.Setenv("XDG_CONFIG_HOME", cfg)

	store := newFakeStore()
	secrets.SetIPCAPI(store)
	t.Cleanup(func() { secrets.SetIPCAPI(nil) })

	e := &env{t: t, store: store}
	e.acc = config.NewAccountManager()
	cm := config.NewConfigManager()
	e.client = api.NewAPIClient("https://placeholder.invalid", "")
	e.am = NewAuthManager(e.client, cm, e.acc, secrets.NewSecretManager())
	e.client.SetOnUnauthorized(func(token string) {
		e.unauthN.Add(1)
		go e.am.MarkSessionExpiredFor(token)
	})
	return e
}

// seed stores an account with a session token. active makes it the active
// account and points the shared API client at it, as main.go does at startup.
func (e *env) seed(f *fakeAPI, userID, token, orgID string, active bool) {
	e.t.Helper()
	if err := e.acc.AddAccount(config.Account{UserID: userID, Email: userID + "@example.com", OrgID: orgID, Hostname: f.srv.URL}); err != nil {
		e.t.Fatal(err)
	}
	e.store.mu.Lock()
	e.store.data[userID] = secretstore.UserSecrets{SessionToken: token}
	e.store.mu.Unlock()
	if active {
		if err := e.acc.SetActiveUser(userID); err != nil {
			e.t.Fatal(err)
		}
		e.client.SetSession(f.srv.URL, "")
	}
}

type accountsJSON struct {
	ActiveUserID string                    `json:"activeUserId"`
	Accounts     map[string]config.Account `json:"accounts"`
}

// diskAccounts reads accounts.json as written to disk.
func diskAccounts(t *testing.T) accountsJSON {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(config.UserDataDir(), config.AccountsFileName))
	if err != nil {
		t.Fatal(err)
	}
	var a accountsJSON
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func orgIDs(orgs []api.Org) []string {
	ids := []string{}
	for _, o := range orgs {
		ids = append(ids, o.Id)
	}
	return ids
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var (
	orgA1 = api.Org{Id: "orgA1", Name: "A one"}
	orgA2 = api.Org{Id: "orgA2", Name: "A two"}
	orgB1 = api.Org{Id: "orgB1", Name: "B one"}
	orgB2 = api.Org{Id: "orgB2", Name: "B two"}
)

// signedInA returns an env where account A is active and fully initialized
// on server a (orgs A1, A2; remembered org A2).
func signedInA(t *testing.T) (*env, *fakeAPI) {
	t.Helper()
	a := newFakeAPI(t)
	a.addUser("tokA", "A", orgA1, orgA2)
	e := newEnv(t)
	e.seed(a, "A", "tokA", "orgA2", true)
	if err := e.am.Initialize(); err != nil {
		t.Fatal(err)
	}
	if !e.am.IsAuthenticated() || e.am.CurrentUser() == nil || e.am.CurrentUser().UserId != "A" {
		t.Fatalf("setup: account A not signed in")
	}
	if o := e.am.CurrentOrg(); o == nil || o.Id != "orgA2" {
		t.Fatalf("setup: org = %+v", o)
	}
	return e, a
}

// assertStillA checks that nothing of the active account A changed.
func assertStillA(t *testing.T, e *env, a *fakeAPI) {
	t.Helper()
	if got := e.client.CurrentBaseURL(); got != a.srv.URL {
		t.Errorf("API client base URL = %q, want %q", got, a.srv.URL)
	}
	if got := e.client.SessionToken(); got != "tokA" {
		t.Errorf("API client session token = %q, want tokA", got)
	}
	if u := e.am.CurrentUser(); u == nil || u.UserId != "A" {
		t.Errorf("CurrentUser = %+v, want A", u)
	}
	if o := e.am.CurrentOrg(); o == nil || o.Id != "orgA2" {
		t.Errorf("CurrentOrg = %+v, want orgA2", o)
	}
	if got := orgIDs(e.am.Organizations()); !eq(got, []string{"orgA1", "orgA2"}) {
		t.Errorf("Organizations = %v", got)
	}
	d := diskAccounts(t)
	if d.ActiveUserID != "A" {
		t.Errorf("accounts.json active user = %q, want A", d.ActiveUserID)
	}
	if len(d.Accounts) != 1 {
		t.Errorf("accounts.json has %d accounts, want 1", len(d.Accounts))
	}
	if d.Accounts["A"].OrgID != "orgA2" || d.Accounts["A"].Hostname != a.srv.URL {
		t.Errorf("account A changed on disk: %+v", d.Accounts["A"])
	}
}

// deviceServer returns a second server B whose device flow verifies at once
// and hands out token tokB for user B.
func deviceServer(t *testing.T) *fakeAPI {
	b := newFakeAPI(t)
	b.addUser("tokB", "B", orgB1, orgB2)
	b.set(&b.poll, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.DeviceAuthPollResponse{Verified: true, Token: strp("tokB")})
	})
	return b
}

// ---------------------------------------------------------------------------
// #11: a failed login step must leave the active account untouched
// ---------------------------------------------------------------------------

// Audit #11. Before the fix the login switched the shared API client to the
// new server/token before verifying it, so every failure left it pointing at
// the new server while the old account stayed active.
func TestLoginFailureLeavesActiveAccountUntouched(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(b *fakeAPI, e *env)
	}{
		{"GetUser after poll answers 500", func(b *fakeAPI, e *env) { b.set(&b.user, status(500)) }},
		{"SaveSessionToken fails", func(b *fakeAPI, e *env) { e.store.failSave.Store(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, a := signedInA(t)
			b := deviceServer(t)
			tc.break_(b, e)

			err := e.am.LoginWithDeviceAuth(context.Background(), strp(b.srv.URL))
			if err == nil {
				t.Fatal("login unexpectedly succeeded")
			}
			assertStillA(t, e, a)
			if got := e.store.token("B"); got != "" {
				t.Errorf("token for B stored despite failed login: %q", got)
			}
			if e.am.IsDeviceAuthInProgress() {
				t.Error("device auth still in progress after the attempt ended")
			}
		})
	}
}

// Audit #11, positive side: a successful login switches client, state and
// accounts.json to the new account together.
func TestLoginSuccessSwitchesEverything(t *testing.T) {
	e, a := signedInA(t)
	b := deviceServer(t)

	if err := e.am.LoginWithDeviceAuth(context.Background(), strp(b.srv.URL)); err != nil {
		t.Fatal(err)
	}
	if got := e.client.CurrentBaseURL(); got != b.srv.URL {
		t.Errorf("base URL = %q, want %q", got, b.srv.URL)
	}
	if got := e.client.SessionToken(); got != "tokB" {
		t.Errorf("session token = %q", got)
	}
	if u := e.am.CurrentUser(); u == nil || u.UserId != "B" {
		t.Errorf("CurrentUser = %+v", u)
	}
	if o := e.am.CurrentOrg(); o == nil || o.Id != "orgB1" {
		t.Errorf("CurrentOrg = %+v", o)
	}
	if got := orgIDs(e.am.Organizations()); !eq(got, []string{"orgB1", "orgB2"}) {
		t.Errorf("Organizations = %v", got)
	}
	if got := e.store.token("B"); got != "tokB" {
		t.Errorf("stored token = %q", got)
	}
	d := diskAccounts(t)
	if d.ActiveUserID != "B" || d.Accounts["B"].Hostname != b.srv.URL || d.Accounts["B"].OrgID != "orgB1" {
		t.Errorf("accounts.json = %+v", d)
	}
	if _, ok := d.Accounts["A"]; !ok {
		t.Error("account A vanished")
	}
	if !e.am.IsAuthenticated() || e.am.IsServerDown() || e.am.SessionExpired() {
		t.Errorf("state: auth=%v down=%v expired=%v", e.am.IsAuthenticated(), e.am.IsServerDown(), e.am.SessionExpired())
	}
	_ = a
}

// ---------------------------------------------------------------------------
// #12 / #62: superseded login attempts
// ---------------------------------------------------------------------------

// slowFirstStart makes the first /start request block until release is
// closed (or the request is cancelled) and answer code "CODE1"; later ones
// answer "CODE2" at once. entered is closed when the first request arrives.
type slowStart struct {
	entered chan struct{}
	release chan struct{}
	n       atomic.Int32
}

func newSlowStart(f *fakeAPI, t *testing.T) *slowStart {
	s := &slowStart{entered: make(chan struct{}), release: make(chan struct{})}
	// Registered after the server's own cleanup, so it runs first and
	// unblocks the handler before the server waits for it.
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
	})
	f.set(&f.start, func(w http.ResponseWriter, r *http.Request) {
		if s.n.Add(1) == 1 {
			close(s.entered)
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
			writeJSON(w, 200, api.DeviceAuthStartResponse{Code: "CODE1", ExpiresInSeconds: 60})
			return
		}
		writeJSON(w, 200, api.DeviceAuthStartResponse{Code: "CODE2", ExpiresInSeconds: 60})
	})
	return s
}

// Audit #12/#62. Attempt 1 is stuck in /start, the user cancels the sheet
// (ClearDeviceAuth) and starts attempt 2. When attempt 1 finally answers, and
// when it is then cancelled, it must neither replace nor clear attempt 2's
// code, and must not reset the in-progress flag. Before the fix attempt 1
// wrote CODE1 over CODE2 and cleared it again on cancellation.
func TestSupersededLoginAttemptDoesNotTouchNewerAttempt(t *testing.T) {
	e, _ := signedInA(t)
	b := newFakeAPI(t)
	slow := newSlowStart(b, t)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	res1 := make(chan error, 1)
	go func() { res1 <- e.am.LoginWithDeviceAuth(ctx1, strp(b.srv.URL)) }()
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt 1 never reached the server")
	}

	e.am.ClearDeviceAuth()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	res2 := make(chan error, 1)
	go func() { res2 <- e.am.LoginWithDeviceAuth(ctx2, strp(b.srv.URL)) }()
	waitFor(t, "attempt 2's device code", func() bool {
		c := e.am.DeviceAuthCode()
		return c != nil && *c == "CODE2"
	})

	// Attempt 1 answers late.
	close(slow.release)
	time.Sleep(100 * time.Millisecond) // let it process the response
	if c := e.am.DeviceAuthCode(); c == nil || *c != "CODE2" {
		t.Errorf("late attempt 1 changed the code: %v", c)
	}

	// Cancelling attempt 1 must not clear attempt 2's state either.
	cancel1()
	select {
	case err := <-res1:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("attempt 1 returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attempt 1 did not return after cancel")
	}
	if c := e.am.DeviceAuthCode(); c == nil || *c != "CODE2" {
		t.Errorf("cancelled attempt 1 cleared or changed the code: %v", c)
	}
	if u := e.am.DeviceAuthLoginURL(); u == nil || !strings.HasPrefix(*u, b.srv.URL) {
		t.Errorf("login URL = %v", u)
	}
	if !e.am.IsDeviceAuthInProgress() {
		t.Error("attempt 1 cleared IsDeviceAuthInProgress of attempt 2")
	}

	cancel2()
	select {
	case <-res2:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt 2 did not return after cancel")
	}
	if e.am.IsDeviceAuthInProgress() {
		t.Error("still in progress after the last attempt ended")
	}
}

// Audit #12/#62. Cancelling the context aborts the request in flight at once
// instead of waiting for the server (or the 30 s client timeout).
func TestLoginCancelAbortsInFlightRequest(t *testing.T) {
	e, a := signedInA(t)
	b := newFakeAPI(t)
	slow := newSlowStart(b, t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- e.am.LoginWithDeviceAuth(ctx, strp(b.srv.URL)) }()
	<-slow.entered
	cancel()

	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("login did not abort its in-flight request after cancel")
	}
	if e.am.IsDeviceAuthInProgress() || e.am.DeviceAuthCode() != nil {
		t.Error("device auth state left behind after cancel")
	}
	assertStillA(t, e, a)
}

// ---------------------------------------------------------------------------
// #14: Initialize with a failing /user
// ---------------------------------------------------------------------------

// shortTimeouts makes real client timeouts fast: the API client uses
// http.DefaultTransport, so a response-header timeout there applies to it.
func shortTimeouts(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	tr := old.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 150 * time.Millisecond
	http.DefaultTransport = tr
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		http.DefaultTransport = old
	})
}

// Audit #14. A /user that fails for a transient reason says nothing about the
// session: the user stays signed in, the server counts as down and the cached
// org is shown. Only a "not found" answer means signed out; 401/403 mean
// expired. Before the fix every non-401/403 failure signed the user out.
func TestInitializeUserFailureModes(t *testing.T) {
	cases := []struct {
		name        string
		handler     func(t *testing.T) http.HandlerFunc
		wantAuth    bool
		wantDown    bool
		wantExpired bool
		wantOrg     bool
	}{
		{"500", func(*testing.T) http.HandlerFunc { return status(500) }, true, true, false, true},
		{"429", func(*testing.T) http.HandlerFunc { return status(429) }, true, true, false, true},
		{"timeout", func(t *testing.T) http.HandlerFunc {
			shortTimeouts(t)
			return func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}
		}, true, true, false, true},
		{"connection reset", func(*testing.T) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
			}
		}, true, true, false, true},
		{"404", func(*testing.T) http.HandlerFunc { return status(404) }, false, false, false, false},
		{"401", func(*testing.T) http.HandlerFunc { return status(401) }, true, false, true, true},
		{"403", func(*testing.T) http.HandlerFunc { return status(403) }, true, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newFakeAPI(t)
			a.addUser("tokA", "A", orgA1, orgA2)
			e := newEnv(t)
			e.seed(a, "A", "tokA", "orgA2", true)
			a.set(&a.user, tc.handler(t))

			if err := e.am.Initialize(); err != nil {
				t.Fatal(err)
			}
			if got := e.am.IsAuthenticated(); got != tc.wantAuth {
				t.Errorf("IsAuthenticated = %v, want %v", got, tc.wantAuth)
			}
			if got := e.am.IsServerDown(); got != tc.wantDown {
				t.Errorf("IsServerDown = %v, want %v", got, tc.wantDown)
			}
			if got := e.am.SessionExpired(); got != tc.wantExpired {
				t.Errorf("SessionExpired = %v, want %v", got, tc.wantExpired)
			}
			org := e.am.CurrentOrg()
			if tc.wantOrg && (org == nil || org.Id != "orgA2") {
				t.Errorf("cached org not restored: %+v", org)
			}
			if !tc.wantOrg && org != nil {
				t.Errorf("unexpected org %+v", org)
			}
			if e.am.CurrentUser() != nil {
				t.Errorf("CurrentUser should be nil, got %+v", e.am.CurrentUser())
			}
		})
	}
}

// Audit #1 precondition. Starting while the server is down leaves the client
// authenticated with no CurrentUser but with the cached org restored.
func TestInitializeServerDownKeepsAuthenticatedWithoutUser(t *testing.T) {
	a := newFakeAPI(t)
	a.addUser("tokA", "A", orgA1, orgA2)
	a.set(&a.health, status(503))
	e := newEnv(t)
	e.seed(a, "A", "tokA", "orgA2", true)

	if err := e.am.Initialize(); err != nil {
		t.Fatal(err)
	}
	if !e.am.IsAuthenticated() {
		t.Error("IsAuthenticated = false, want true")
	}
	if !e.am.IsServerDown() {
		t.Error("IsServerDown = false, want true")
	}
	if e.am.CurrentUser() != nil {
		t.Errorf("CurrentUser = %+v, want nil", e.am.CurrentUser())
	}
	if o := e.am.CurrentOrg(); o == nil || o.Id != "orgA2" {
		t.Errorf("CurrentOrg = %+v, want cached orgA2", o)
	}
}

// ---------------------------------------------------------------------------
// #10: refreshes in flight across an account switch
// ---------------------------------------------------------------------------

// gate blocks requests for a given token until released.
type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

func (g *gate) wait(r *http.Request) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-r.Context().Done():
	}
}

// twoAccounts returns A (active, signed in) and B (remembered org orgB2) on
// the same server, with a distinguishable user/org set per account.
func twoAccounts(t *testing.T) (*env, *fakeAPI) {
	e, a := signedInA(t)
	a.addUser("tokB", "B", orgB1, orgB2)
	e.seed(a, "B", "tokB", "orgB2", false)
	return e, a
}

func assertSwitchedToB(t *testing.T, e *env) {
	t.Helper()
	if u := e.am.CurrentUser(); u == nil || u.UserId != "B" {
		t.Errorf("CurrentUser = %+v, want B", u)
	}
	if o := e.am.CurrentOrg(); o == nil || o.Id != "orgB2" {
		t.Errorf("CurrentOrg = %+v, want orgB2", o)
	}
	if got := orgIDs(e.am.Organizations()); !eq(got, []string{"orgB1", "orgB2"}) {
		t.Errorf("Organizations = %v, want B's", got)
	}
	d := diskAccounts(t)
	if d.ActiveUserID != "B" {
		t.Errorf("active user on disk = %q", d.ActiveUserID)
	}
	if d.Accounts["B"].OrgID != "orgB2" {
		t.Errorf("B's remembered org on disk = %q, want orgB2", d.Accounts["B"].OrgID)
	}
}

// Audit #10. RefreshOrganizations for A is still in flight when SwitchAccount
// moves to B. A's late answer must not replace B's orgs/org nor touch B's
// remembered org in accounts.json.
func TestRefreshOrganizationsInFlightAcrossSwitch(t *testing.T) {
	e, a := twoAccounts(t)
	g := newGate(t)
	a.set(&a.orgsH, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/user/A/") {
			g.wait(r)
			writeJSON(w, 200, api.ListUserOrgsResponse{Orgs: []api.Org{{Id: "orgA9", Name: "late"}}})
			return
		}
		o := []api.Org{orgB1, orgB2}
		writeJSON(w, 200, api.ListUserOrgsResponse{Orgs: o})
	})

	done := make(chan struct{})
	go func() { defer close(done); _ = e.am.RefreshOrganizations() }()
	<-g.entered

	if err := e.am.SwitchAccount("B"); err != nil {
		t.Fatal(err)
	}
	assertSwitchedToB(t, e)

	g.open()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not return")
	}
	assertSwitchedToB(t, e)
}

// Audit #10. Same for RefreshCurrentUser: A's late /user answer must not
// become CurrentUser of B nor rewrite B's account.
func TestRefreshCurrentUserInFlightAcrossSwitch(t *testing.T) {
	e, a := twoAccounts(t)
	g := newGate(t)
	a.set(&a.user, func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("p_session_token")
		if c != nil && c.Value == "tokA" {
			g.wait(r)
			u := api.User{Id: "A", UserId: "A", Email: "A@example.com", Username: strp("late-name")}
			writeJSON(w, 200, u)
			return
		}
		a.defaultUser(w, r)
	})

	type result struct {
		u   *api.User
		err error
	}
	res := make(chan result, 1)
	go func() { u, err := e.am.RefreshCurrentUser(); res <- result{u, err} }()
	<-g.entered

	if err := e.am.SwitchAccount("B"); err != nil {
		t.Fatal(err)
	}
	assertSwitchedToB(t, e)

	g.open()
	select {
	case r := <-res:
		if r.u != nil {
			t.Errorf("stale refresh returned user %+v", r.u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not return")
	}
	assertSwitchedToB(t, e)
	if got := diskAccounts(t).Accounts["A"].Username; got == "late-name" {
		t.Errorf("A's late answer was written to accounts.json")
	}
}

// ---------------------------------------------------------------------------
// #50: logout
// ---------------------------------------------------------------------------

// Audit #50. Logging out of the last account clears all per-session state.
// Before the fix serverInfo, serverDown, sessionExpired and currentUser
// lingered.
func TestLogoutLastAccountClearsSessionState(t *testing.T) {
	e, a := signedInA(t)
	if e.am.ServerInfo() == nil {
		t.Fatal("setup: server info not loaded")
	}
	// Put the manager in the states that must not survive: expired + down.
	e.am.MarkSessionExpired()
	a.set(&a.health, status(503))
	_ = e.am.CheckHealthAndSetState()
	if !e.am.SessionExpired() || !e.am.IsServerDown() {
		t.Fatalf("setup: expired=%v down=%v", e.am.SessionExpired(), e.am.IsServerDown())
	}

	if err := e.am.Logout(); err != nil {
		t.Fatal(err)
	}
	if e.am.IsAuthenticated() {
		t.Error("still authenticated")
	}
	if e.am.CurrentUser() != nil {
		t.Errorf("CurrentUser = %+v", e.am.CurrentUser())
	}
	if e.am.CurrentOrg() != nil || len(e.am.Organizations()) != 0 {
		t.Errorf("org state left: %+v %v", e.am.CurrentOrg(), e.am.Organizations())
	}
	if e.am.ServerInfo() != nil {
		t.Error("ServerInfo left behind")
	}
	if e.am.IsServerDown() {
		t.Error("IsServerDown left behind")
	}
	if e.am.SessionExpired() {
		t.Error("SessionExpired left behind")
	}
	if e.client.SessionToken() != "" {
		t.Error("API client still holds the token")
	}
	if got := diskAccounts(t); got.ActiveUserID != "" || len(got.Accounts) != 0 {
		t.Errorf("accounts.json = %+v", got)
	}
	if e.store.token("A") != "" {
		t.Error("session token not deleted")
	}

	// A late MarkSessionExpired (e.g. from the tunnel layer) is a no-op.
	e.am.MarkSessionExpired()
	if e.am.SessionExpired() {
		t.Error("MarkSessionExpired after logout set the flag")
	}
}

// Audit #50. The logout POST answering 401 (the session is already gone on the
// server) must not report the session as expired, neither through the
// synchronous result nor through the async onUnauthorized callback.
func TestLogout401DoesNotMarkSessionExpired(t *testing.T) {
	e, a := signedInA(t)
	a.set(&a.logout, status(401))

	if err := e.am.Logout(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // an async MarkSessionExpired would have run by now
	if n := e.unauthN.Load(); n != 0 {
		t.Errorf("onUnauthorized fired %d times for the logout request", n)
	}
	if e.am.SessionExpired() {
		t.Error("SessionExpired set by the logout's own 401")
	}
	if e.am.IsAuthenticated() {
		t.Error("still authenticated")
	}
}

// ---------------------------------------------------------------------------
// Session-generation guards: login commit, device-auth ownership, expiry,
// logout, organization selection, Initialize
// ---------------------------------------------------------------------------

// blockOrgs makes B's ListUserOrgs block on g and then answer B's orgs.
func blockOrgs(b *fakeAPI, g *gate) {
	b.set(&b.orgsH, func(w http.ResponseWriter, r *http.Request) {
		g.wait(r)
		writeJSON(w, 200, api.ListUserOrgsResponse{Orgs: []api.Org{orgB1, orgB2}})
	})
}

// A login whose attempt is cancelled (ctx) or cleared (ClearDeviceAuth)
// after the browser approval, while the new account's organizations are
// still loading, must not become the active account.
func TestLoginCancelledBeforeCommitDoesNotActivate(t *testing.T) {
	cases := []struct {
		name   string
		cancel func(cancel context.CancelFunc, e *env)
		check  func(t *testing.T, err error)
	}{
		{"ctx cancelled", func(cancel context.CancelFunc, e *env) { cancel() }, func(t *testing.T, err error) {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
		}},
		{"ClearDeviceAuth", func(cancel context.CancelFunc, e *env) { e.am.ClearDeviceAuth() }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrSessionChanged) {
				t.Errorf("err = %v, want ErrSessionChanged", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, a := signedInA(t)
			b := deviceServer(t)
			g := newGate(t)
			blockOrgs(b, g)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := make(chan error, 1)
			go func() { res <- e.am.LoginWithDeviceAuth(ctx, strp(b.srv.URL)) }()
			select {
			case <-g.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("login never reached the organization request")
			}
			tc.cancel(cancel, e)
			g.open()

			select {
			case err := <-res:
				tc.check(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("login did not return")
			}
			assertStillA(t, e, a)
		})
	}
}

// beginDeviceAuth refuses an already-cancelled ctx: a superseded attempt that
// is scheduled late must not take over from the live attempt B.
func TestBeginDeviceAuthRefusesCancelledContext(t *testing.T) {
	e, _ := signedInA(t)
	b := newFakeAPI(t)
	slow := newSlowStart(b, t) // first /start (attempt B) blocks, answers CODE1

	ctxB, cancelB := context.WithCancel(context.Background())
	resB := make(chan error, 1)
	go func() { resB <- e.am.LoginWithDeviceAuth(ctxB, strp(b.srv.URL)) }()
	t.Cleanup(func() {
		cancelB()
		select {
		case <-resB:
		case <-time.After(5 * time.Second):
		}
	})
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt B never reached the server")
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	cancelA()
	err := e.am.LoginWithDeviceAuth(ctxA, strp(b.srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled attempt returned %v, want context.Canceled", err)
	}
	if n := slow.n.Load(); n != 1 {
		t.Errorf("server saw %d /start requests, want 1 (the cancelled attempt must not start)", n)
	}
	if !e.am.IsDeviceAuthInProgress() {
		t.Error("cancelled attempt cleared IsDeviceAuthInProgress of attempt B")
	}

	close(slow.release)
	waitFor(t, "attempt B's device code", func() bool {
		c := e.am.DeviceAuthCode()
		return c != nil && *c == "CODE1"
	})
	time.Sleep(100 * time.Millisecond)
	if c := e.am.DeviceAuthCode(); c == nil || *c != "CODE1" {
		t.Errorf("device code = %v, want CODE1", c)
	}
	if !e.am.IsDeviceAuthInProgress() {
		t.Error("attempt B no longer in progress while polling")
	}
}

// MarkSessionExpiredFor only acts when the client still uses that token.
func TestMarkSessionExpiredFor(t *testing.T) {
	t.Run("token differs", func(t *testing.T) {
		e, a := signedInA(t)
		e.client.SetSession(a.srv.URL, "tokB")
		e.am.MarkSessionExpiredFor("tokA")
		if e.am.SessionExpired() {
			t.Error("a 401 for tokA expired the session that now uses tokB")
		}
		e.am.MarkSessionExpiredFor("")
		if e.am.SessionExpired() {
			t.Error("empty token expired the session")
		}
	})
	t.Run("token matches", func(t *testing.T) {
		e, _ := signedInA(t)
		e.am.MarkSessionExpiredFor("tokA")
		if !e.am.SessionExpired() {
			t.Error("matching token did not mark the session expired")
		}
	})
	t.Run("after logout", func(t *testing.T) {
		e, _ := signedInA(t)
		if err := e.am.Logout(); err != nil {
			t.Fatal(err)
		}
		e.am.MarkSessionExpiredFor("tokA")
		e.am.MarkSessionExpiredFor("")
		if e.am.SessionExpired() {
			t.Error("MarkSessionExpiredFor after logout set the flag")
		}
	})
}

// A switch during the logout request: Logout reports ErrSessionChanged and
// leaves the new session and A's account alone; the POST carried A's token.
func TestLogoutSessionChangedDuringRequest(t *testing.T) {
	e, a := twoAccounts(t)
	g := newGate(t)
	var seen atomic.Value
	a.set(&a.logout, func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("p_session_token"); err == nil {
			seen.Store(c.Value)
		}
		g.wait(r)
		writeJSON(w, 200, map[string]any{})
	})

	res := make(chan error, 1)
	go func() { res <- e.am.Logout() }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("logout request never arrived")
	}
	if err := e.am.SwitchAccount("B"); err != nil {
		t.Fatal(err)
	}
	g.open()

	select {
	case err := <-res:
		if !errors.Is(err, ErrSessionChanged) {
			t.Errorf("Logout = %v, want ErrSessionChanged", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logout did not return")
	}
	if got, _ := seen.Load().(string); got != "tokA" {
		t.Errorf("logout POST cookie = %q, want tokA (captured before the switch)", got)
	}
	assertSwitchedToB(t, e)
	if !e.am.IsAuthenticated() {
		t.Error("switched session lost its authenticated state")
	}
	if got := e.client.SessionToken(); got != "tokB" {
		t.Errorf("client token = %q, want tokB", got)
	}
	if _, ok := e.acc.Account("A"); !ok {
		t.Error("account A was removed")
	}
	if got := e.store.token("A"); got != "tokA" {
		t.Errorf("A's stored token = %q, want it kept", got)
	}
}

// Logout must not clear the device code of a login that is running.
func TestLogoutKeepsRunningLoginDeviceCode(t *testing.T) {
	e, _ := signedInA(t)
	b := newFakeAPI(t) // default /start answers CODE, polls stay unverified

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- e.am.LoginWithDeviceAuth(ctx, strp(b.srv.URL)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-res:
		case <-time.After(5 * time.Second):
		}
	})
	waitFor(t, "device code", func() bool { return e.am.DeviceAuthCode() != nil })

	if err := e.am.Logout(); err != nil {
		t.Fatal(err)
	}
	if c := e.am.DeviceAuthCode(); c == nil || *c != "CODE" {
		t.Errorf("Logout changed the running login's code: %v", c)
	}
	if u := e.am.DeviceAuthLoginURL(); u == nil {
		t.Error("Logout cleared the running login's URL")
	}
	if !e.am.IsDeviceAuthInProgress() {
		t.Error("Logout ended the running login")
	}
}

// An account switch while the org access check runs: no CurrentOrg change.
func TestSelectOrganizationSessionChangedDuringAccessCheck(t *testing.T) {
	e, a := twoAccounts(t)
	g := newGate(t)
	a.set(&a.orgH, func(w http.ResponseWriter, r *http.Request) {
		g.wait(r)
		writeJSON(w, 200, api.GetOrgResponse{Id: "orgA1", Name: "A one"})
	})

	org := orgA1
	res := make(chan error, 1)
	go func() { res <- e.am.SelectOrganization(&org) }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("access check never arrived")
	}
	if err := e.am.SwitchAccount("B"); err != nil {
		t.Fatal(err)
	}
	g.open()

	select {
	case err := <-res:
		if !errors.Is(err, ErrSessionChanged) {
			t.Errorf("SelectOrganization = %v, want ErrSessionChanged", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SelectOrganization did not return")
	}
	assertSwitchedToB(t, e) // CurrentOrg is B's orgB2, not A's orgA1
	if got := diskAccounts(t).Accounts["A"].OrgID; got != "orgA2" {
		t.Errorf("A's remembered org on disk = %q, want orgA2", got)
	}
}

// Initialize reads the active account (A), then its token; an account switch
// to B in between must win. The stall is injected in the fake secret store's
// token read, which Initialize performs after reading the active account and
// the session generation and before committing.
func TestInitializeDoesNotRevertConcurrentSwitch(t *testing.T) {
	e, _ := twoAccounts(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func(userID string) {
		if userID != "A" {
			return
		}
		blocked := false
		once.Do(func() { blocked = true })
		if !blocked {
			return
		}
		close(entered)
		<-release
	}
	e.store.getHook.Store(&hook)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	res := make(chan error, 1)
	go func() { res <- e.am.Initialize() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize never read A's token")
	}
	if err := e.am.SwitchAccount("B"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-res:
		if err != nil {
			t.Errorf("Initialize = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize did not return")
	}

	assertSwitchedToB(t, e)
	if got := e.client.SessionToken(); got != "tokB" {
		t.Errorf("client token = %q, want tokB", got)
	}
	if !e.am.IsAuthenticated() {
		t.Error("not authenticated after switch")
	}
}
