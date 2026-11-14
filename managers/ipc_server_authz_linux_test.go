package managers

import (
	"encoding/gob"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fosrl/windows-linux/tunnel"
	"github.com/fosrl/windows-linux/updater"
)

// fakeTunnels stands in for the tunnel processes the daemon starts.
type fakeTunnels struct {
	mu          sync.Mutex
	running     map[string]bool
	installs    int
	uninstalls  int
	uninstalled []string
	granted     []uint32
}

func useFakeTunnels(t *testing.T) *fakeTunnels {
	t.Helper()
	f := &fakeTunnels{running: make(map[string]bool)}
	oldInstall, oldUninstall, oldRunning := installTunnel, uninstallTunnel, tunnelIsRunning
	installTunnel = func(configJSON string) error {
		cfg, err := tunnel.ConfigFromJSON(configJSON)
		if err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.installs++
		f.running[cfg.Name] = true
		return nil
	}
	uninstallTunnel = func(name string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.uninstalls++
		f.uninstalled = append(f.uninstalled, name)
		delete(f.running, name)
		return nil
	}
	tunnelIsRunning = func(name string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.running[name]
	}
	oldGrant := grantOLMSocketAccess
	grantOLMSocketAccess = func(uid uint32) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.granted = append(f.granted, uid)
		return nil
	}
	oldLoginctl, oldLogind := loginctl, logindRunning
	loginctl = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "show-user" {
			return []byte("7\n"), nil
		}
		return []byte("Remote=no\nSeat=seat0\nClass=user\nState=active\n"), nil
	}
	logindRunning = func() bool { return true }
	activeTunnelsLock.Lock()
	activeTunnels = make(map[string]tunnelOwner)
	activeTunnelsLock.Unlock()
	t.Cleanup(func() {
		grantOLMSocketAccess = oldGrant
		loginctl, logindRunning = oldLoginctl, oldLogind
		installTunnel, uninstallTunnel, tunnelIsRunning = oldInstall, oldUninstall, oldRunning
		activeTunnelsLock.Lock()
		activeTunnels = make(map[string]tunnelOwner)
		activeTunnelsLock.Unlock()
		tunnel.SetState(tunnel.StateStopped)
	})
	return f
}

func (f *fakeTunnels) counts() (installs, uninstalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installs, f.uninstalls
}

// admin returns an administrator's client. useFakeTunnels gives every UID a
// local seat session, so the test is about ownership.
func admin(uid string) *ManagerService {
	return &ManagerService{clientUserKey: uid, session: 0, elevatedToken: 1}
}

func owner(name string) (tunnelOwner, bool) {
	activeTunnelsLock.RLock()
	defer activeTunnelsLock.RUnlock()
	o, ok := activeTunnels[name]
	return o, ok
}

func TestTunnelOwnershipAcrossUsers(t *testing.T) {
	f := useFakeTunnels(t)
	alice, bob := admin("1000"), admin("1001")
	alice.session, bob.session = 1000, 1001

	if err := alice.startTunnelChecked(tunnel.Config{Name: "../evil"}); err != nil {
		t.Fatalf("alice StartTunnel: %v", err)
	}
	if o, ok := owner(tunnel.DefaultTunnelName); !ok || o.userKey != "1000" || o.session != 1000 {
		t.Fatalf("owner after alice's start = %+v, %v; want 1000", o, ok)
	}
	if _, ok := owner("../evil"); ok {
		t.Fatal("the client-chosen tunnel name was used")
	}

	// Bob can't replace, stop or quit alice's tunnel.
	if err := bob.startTunnelChecked(tunnel.Config{}); !errors.Is(err, errTunnelInUse) {
		t.Fatalf("bob StartTunnel = %v, want errTunnelInUse", err)
	}
	if err := bob.StopTunnel(); err != nil {
		t.Fatalf("bob StopTunnel = %v, want nil (nothing of his to stop)", err)
	}
	if err := bob.StopAllTunnels(); err != nil {
		t.Fatalf("bob StopAllTunnels: %v", err)
	}
	if installs, uninstalls := f.counts(); installs != 1 || uninstalls != 0 {
		t.Fatalf("after bob's calls: installs=%d uninstalls=%d, want 1 and 0", installs, uninstalls)
	}
	if o, _ := owner(tunnel.DefaultTunnelName); o.userKey != "1000" {
		t.Fatalf("owner changed to %q", o.userKey)
	}

	// Alice stops her own tunnel, and the record goes away (audit #83).
	if err := alice.StopTunnel(); err != nil {
		t.Fatalf("alice StopTunnel: %v", err)
	}
	if _, uninstalls := f.counts(); uninstalls != 1 {
		t.Fatalf("uninstalls = %d, want 1", uninstalls)
	}
	if _, ok := owner(tunnel.DefaultTunnelName); ok {
		t.Fatal("activeTunnels still records the stopped tunnel")
	}

	// Now bob may connect.
	if err := bob.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatalf("bob StartTunnel after alice stopped: %v", err)
	}
	if o, _ := owner(tunnel.DefaultTunnelName); o.userKey != "1001" {
		t.Fatalf("owner = %q, want 1001", o.userKey)
	}
}

func TestDeadTunnelHasNoOwner(t *testing.T) {
	f := useFakeTunnels(t)
	alice, bob := admin("1000"), admin("1001")
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	// The tunnel process exits on its own.
	f.mu.Lock()
	delete(f.running, tunnel.DefaultTunnelName)
	f.mu.Unlock()
	if err := bob.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatalf("bob StartTunnel after alice's tunnel died: %v", err)
	}
}

func TestStopAllTunnelsStopsOnlyOwn(t *testing.T) {
	f := useFakeTunnels(t)
	alice := admin("1000")
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := alice.StopAllTunnels(); err != nil {
		t.Fatal(err)
	}
	if _, uninstalls := f.counts(); uninstalls != 1 {
		t.Fatalf("uninstalls = %d, want 1", uninstalls)
	}
	if _, ok := owner(tunnel.DefaultTunnelName); ok {
		t.Fatal("tunnel still recorded after StopAllTunnels")
	}
}

func TestStopSessionTunnelsOnSignOut(t *testing.T) {
	f := useFakeTunnels(t)
	alice := admin("1000")
	alice.session = 1000
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	stopSessionTunnels(1001) // someone else signing out
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatalf("another session's sign-out stopped the tunnel")
	}
	stopSessionTunnels(1000)
	if _, uninstalls := f.counts(); uninstalls != 1 {
		t.Fatalf("alice's sign-out did not stop her tunnel")
	}
}

func TestQuitNeedsAdministrator(t *testing.T) {
	s := &ManagerService{clientUserKey: "1000"}
	alreadyQuit, err := s.Quit(true)
	if err == nil || alreadyQuit {
		t.Fatalf("Quit from a non-admin = %v, %v; want an error", alreadyQuit, err)
	}
	if haveQuit != 0 {
		t.Fatal("a refused Quit marked the manager as quit")
	}
	select {
	case <-quitManagersChan:
		t.Fatal("a refused Quit asked the manager to stop")
	default:
	}
}

func TestUpdateNeedsAdministratorWithoutStagedUpdate(t *testing.T) {
	updateLock.Lock()
	oldStaged := stagedUpdate
	stagedUpdate = nil
	updateLock.Unlock()
	t.Cleanup(func() {
		updateLock.Lock()
		stagedUpdate = oldStaged
		updateLock.Unlock()
	})

	s := &ManagerService{clientUserKey: "1000"}
	if err := s.authorizeUpdate(); !errors.Is(err, errUpdateNeedsAdmin) {
		t.Fatalf("Update from a non-admin = %v, want errUpdateNeedsAdmin", err)
	}
	s.elevatedToken = 1
	if err := s.authorizeUpdate(); err != nil {
		t.Fatalf("Update from an admin = %v", err)
	}
	s.elevatedToken = 0
	updateLock.Lock()
	stagedUpdate = &updater.StagedUpdate{}
	updateLock.Unlock()
	if err := s.authorizeUpdate(); err != nil {
		t.Fatalf("Update of a staged update from a non-admin = %v", err)
	}
}

// TestServeConnUpdateReply drives the Update request over the gob protocol:
// a refusal comes back as the reply, and nothing is stopped or recorded.
func TestServeConnUpdateReply(t *testing.T) {
	f := useFakeTunnels(t)
	updateLock.Lock()
	oldStaged := stagedUpdate
	stagedUpdate = nil
	updateLock.Unlock()
	t.Cleanup(func() {
		updateLock.Lock()
		stagedUpdate = oldStaged
		updateLock.Unlock()
	})

	alice := admin("1000")
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}

	clientRead, serverWrite, _ := os.Pipe()
	serverRead, clientWrite, _ := os.Pipe()
	s := &ManagerService{clientUserKey: "1001"} // not an administrator
	go s.ServeConn(serverRead, serverWrite)
	t.Cleanup(func() { clientWrite.Close(); clientRead.Close() })

	oldEnc, oldDec := rpcEncoder, rpcDecoder
	rpcEncoder, rpcDecoder = gob.NewEncoder(clientWrite), gob.NewDecoder(clientRead)
	t.Cleanup(func() { rpcEncoder, rpcDecoder = oldEnc, oldDec })

	if err := IPCClientUpdate(); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("Update over IPC = %v, want the administrator error", err)
	}
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatal("a refused update stopped a tunnel")
	}
	// The stream is still in step: another call works.
	if _, err := IPCClientUpdateState(); err != nil {
		t.Fatalf("connection unusable after the refused update: %v", err)
	}
}

func TestStopTunnelsForUpdateStopsEveryOwner(t *testing.T) {
	f := useFakeTunnels(t)
	alice := admin("1000")
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	stopTunnelsForUpdate()
	if _, uninstalls := f.counts(); uninstalls != 1 {
		t.Fatalf("uninstalls = %d, want 1", uninstalls)
	}
	if _, ok := owner(tunnel.DefaultTunnelName); ok {
		t.Fatal("tunnel still recorded")
	}
}

func TestIPCClientUpdateWithoutManager(t *testing.T) {
	oldEnc, oldDec := rpcEncoder, rpcDecoder
	rpcEncoder, rpcDecoder = nil, nil
	t.Cleanup(func() { rpcEncoder, rpcDecoder = oldEnc, oldDec })
	if err := IPCClientUpdate(); !errors.Is(err, errIPCNotConnected) {
		t.Fatalf("IPCClientUpdate without a manager = %v, want errIPCNotConnected", err)
	}
}

func TestTunnelStateGoesOnlyToOwner(t *testing.T) {
	mk := func(key string) (*ManagerService, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); w.Close() })
		return &ManagerService{clientUserKey: key, events: w}, r
	}
	alice, aliceEvents := mk("1000")
	bob, bobEvents := mk("1001")
	managerServicesLock.Lock()
	managerServices[alice] = true
	managerServices[bob] = true
	managerServicesLock.Unlock()
	t.Cleanup(func() {
		managerServicesLock.Lock()
		delete(managerServices, alice)
		delete(managerServices, bob)
		managerServicesLock.Unlock()
	})

	notifyTunnelState("1000", tunnel.StateRunning)

	readOne := func(f *os.File) (NotificationType, TunnelState, error) {
		_ = f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		dec := gob.NewDecoder(f)
		var nt NotificationType
		var st TunnelState
		if err := dec.Decode(&nt); err != nil {
			return 0, 0, err
		}
		err := dec.Decode(&st)
		return nt, st, err
	}
	nt, st, err := readOne(aliceEvents)
	if err != nil || nt != TunnelStateChangeNotificationType || st != tunnel.StateRunning {
		t.Fatalf("alice got %v %v %v", nt, st, err)
	}
	if _, _, err := readOne(bobEvents); err == nil {
		t.Fatal("bob received alice's tunnel state")
	}
}

// fakeLoginctl answers loginctl calls from a table keyed by the joined args.
func fakeLoginctl(t *testing.T, answers map[string]string, logind bool) {
	t.Helper()
	oldLoginctl, oldRunning := loginctl, logindRunning
	loginctl = func(args ...string) ([]byte, error) {
		out, ok := answers[strings.Join(args, " ")]
		if !ok {
			// Like loginctl for an unknown user or session.
			return nil, exec.Command("false").Run()
		}
		return []byte(out), nil
	}
	logindRunning = func() bool { return logind }
	t.Cleanup(func() { loginctl, logindRunning = oldLoginctl, oldRunning })
}

const sessionProps = "--property=Remote --property=Seat --property=Class --property=State"

func TestAuthorizeTunnelUse(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		uid     string
		logind  bool
		ok      bool
	}{
		{"root", nil, "0", true, true},
		// sudo/wheel membership is not enough without a local sign-in (#101).
		{"administrator over ssh only", map[string]string{
			"show-user 1000 --property=Sessions --value": "5",
			"show-session 5 " + sessionProps:             "Remote=yes\nSeat=\nClass=user\nState=active\n",
		}, "1000", true, false},
		{"no logind, regular account", nil, "1000", false, true},
		{"no logind, system account", nil, "998", false, false},
		{"no logind, nobody", nil, "65534", false, false},
		{"local seat session", map[string]string{
			"show-user 1000 --property=Sessions --value": "2\n",
			"show-session 2 " + sessionProps:             "Remote=no\nSeat=seat0\nClass=user\nState=active\n",
		}, "1000", true, true},
		{"inactive local session (user switched away)", map[string]string{
			"show-user 1000 --property=Sessions --value": "2",
			"show-session 2 " + sessionProps:             "Remote=no\nSeat=seat0\nClass=user\nState=online\n",
		}, "1000", true, true},
		{"ssh only", map[string]string{
			"show-user 1000 --property=Sessions --value": "5",
			"show-session 5 " + sessionProps:             "Remote=yes\nSeat=\nClass=user\nState=active\n",
		}, "1000", true, false},
		{"greeter on a seat", map[string]string{
			"show-user 1000 --property=Sessions --value": "c1",
			"show-session c1 " + sessionProps:            "Remote=no\nSeat=seat0\nClass=greeter\nState=active\n",
		}, "1000", true, false},
		{"closing local session", map[string]string{
			"show-user 1000 --property=Sessions --value": "2",
			"show-session 2 " + sessionProps:             "Remote=no\nSeat=seat0\nClass=user\nState=closing\n",
		}, "1000", true, false},
		{"no session (system account)", map[string]string{}, "1000", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeLoginctl(t, tt.answers, tt.logind)
			loginDefs := filepath.Join(t.TempDir(), "login.defs")
			_ = os.WriteFile(loginDefs, []byte("# comment\nUID_MIN\t\t\t 1000\n"), 0o600)
			old := loginDefsPath
			loginDefsPath = loginDefs
			t.Cleanup(func() { loginDefsPath = old })
			// An administrator's token must not matter.
			s := &ManagerService{clientUserKey: tt.uid, elevatedToken: 1}
			// No UI is attached, so the polkit fallback has no subject and
			// refuses without running pkcheck.
			err := authorizeTunnelUse(s)
			if (err == nil) != tt.ok {
				t.Fatalf("authorizeTunnelUse = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestUnauthorizedUserCannotStartOrReadPosture(t *testing.T) {
	f := useFakeTunnels(t)
	fakeLoginctl(t, map[string]string{}, true)
	s := &ManagerService{clientUserKey: "998"} // a system account
	if err := s.startTunnelChecked(tunnel.Config{}); err == nil {
		t.Fatal("StartTunnel allowed for a user without a session")
	}
	if installs, _ := f.counts(); installs != 0 {
		t.Fatal("a tunnel was installed")
	}
	if _, err := s.GetDevicePosture(); err == nil {
		t.Fatal("GetDevicePosture allowed for a user without a session")
	}
}

func TestSignedOut(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		logind  bool
		want    bool
	}{
		{"active", map[string]string{"show-user 1000 --property=State --value": "active\n"}, true, false},
		{"online", map[string]string{"show-user 1000 --property=State --value": "online\n"}, true, false},
		{"closing", map[string]string{"show-user 1000 --property=State --value": "closing\n"}, true, true},
		{"lingering", map[string]string{"show-user 1000 --property=State --value": "lingering\n"}, true, true},
		{"not known to logind", map[string]string{}, true, true},
		{"no logind", map[string]string{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeLoginctl(t, tt.answers, tt.logind)
			if got := signedOut(1000); got != tt.want {
				t.Fatalf("signedOut = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseProperties(t *testing.T) {
	got := parseProperties("Remote=no\nSeat=seat0\n\nBogus\nClass=user=x\n")
	want := map[string]string{"Remote": "no", "Seat": "seat0", "Class": "user=x"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestOwnerGoneAndWatcher(t *testing.T) {
	f := useFakeTunnels(t)
	alice := admin("1000")
	alice.session = 1000
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	o, _ := owner(tunnel.DefaultTunnelName)
	if !o.local {
		t.Fatal("an owner with a local seat session was not marked local")
	}

	// Alice signs out of the desktop but keeps an SSH session open.
	fakeLoginctl(t, map[string]string{
		"show-user 1000 --property=Sessions --value": "5",
		"show-session 5 " + sessionProps:             "Remote=yes\nSeat=\nClass=user\nState=active\n",
		"show-user 1000 --property=State --value":    "active",
	}, true)
	gone := make(map[tunnelOwner]int)
	checkTunnelOwners(gone)
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatal("stopped after a single check")
	}
	checkTunnelOwners(gone)
	if _, uninstalls := f.counts(); uninstalls != 1 {
		t.Fatal("the tunnel of an owner without a local session was not stopped")
	}
	if _, ok := owner(tunnel.DefaultTunnelName); ok {
		t.Fatal("tunnel still recorded")
	}
}

func TestWatcherKeepsTunnelOfSignedInOwner(t *testing.T) {
	f := useFakeTunnels(t)
	if err := admin("1000").startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	gone := make(map[tunnelOwner]int)
	for range 3 {
		checkTunnelOwners(gone)
	}
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatal("stopped the tunnel of a signed-in owner")
	}
	// A flaky answer once, then signed in again: the count starts over.
	fakeLoginctl(t, map[string]string{}, true)
	checkTunnelOwners(gone)
	fakeLoginctl(t, map[string]string{
		"show-user 1000 --property=Sessions --value": "2",
		"show-session 2 " + sessionProps:             "Remote=no\nSeat=seat0\nClass=user\nState=online\n",
	}, true)
	checkTunnelOwners(gone)
	fakeLoginctl(t, map[string]string{}, true)
	checkTunnelOwners(gone)
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatal("two non-consecutive misses stopped the tunnel")
	}
	// Without logind nothing is ever stopped.
	fakeLoginctl(t, map[string]string{}, false)
	for range 3 {
		checkTunnelOwners(gone)
	}
	if _, uninstalls := f.counts(); uninstalls != 0 {
		t.Fatal("stopped a tunnel without logind")
	}
}

func TestStartTunnelGrantsOwnerAccess(t *testing.T) {
	f := useFakeTunnels(t)
	if err := admin("1000").startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.granted) != 1 || f.granted[0] != 1000 {
		t.Fatalf("granted = %v, want [1000]", f.granted)
	}
}

// TestStagedUpdateDoesNotLetOthersDropTunnels: a staged update lets a
// non-admin install only when nobody else is affected.
func TestStagedUpdateDoesNotLetOthersDropTunnels(t *testing.T) {
	useFakeTunnels(t)
	updateLock.Lock()
	oldStaged := stagedUpdate
	stagedUpdate = &updater.StagedUpdate{}
	updateLock.Unlock()
	t.Cleanup(func() {
		updateLock.Lock()
		stagedUpdate = oldStaged
		updateLock.Unlock()
	})
	bob := &ManagerService{clientUserKey: "1001"}

	// Another user's UI is attached.
	attachedUIsLock.Lock()
	attachedUIs[1000] = &attachedUI{done: make(chan struct{})}
	attachedUIsLock.Unlock()
	t.Cleanup(func() {
		attachedUIsLock.Lock()
		delete(attachedUIs, 1000)
		attachedUIsLock.Unlock()
	})
	if err := bob.authorizeUpdate(); !errors.Is(err, errUpdateNeedsAdmin) {
		t.Fatalf("staged update with another user's UI attached = %v, want refused", err)
	}
	attachedUIsLock.Lock()
	delete(attachedUIs, 1000)
	attachedUIsLock.Unlock()

	// Another user's tunnel is running.
	if err := admin("1000").startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := bob.authorizeUpdate(); !errors.Is(err, errUpdateNeedsAdmin) {
		t.Fatalf("staged update while another user's tunnel runs = %v, want refused", err)
	}
	if err := admin("1000").StopTunnel(); err != nil {
		t.Fatal(err)
	}
	if err := bob.authorizeUpdate(); err != nil {
		t.Fatalf("staged update with nobody else around = %v, want allowed", err)
	}
}

func TestQuitNeedsRootOnLinux(t *testing.T) {
	wheel := &ManagerService{clientUserKey: "1000", elevatedToken: 1}
	if quitAllowed(wheel) {
		t.Fatal("a sudo/wheel member may Quit without authentication")
	}
	if !quitAllowed(&ManagerService{clientUserKey: "0", elevatedToken: 1}) {
		t.Fatal("root may not Quit")
	}
}

func TestMayAdoptTunnel(t *testing.T) {
	useFakeTunnels(t)
	alice, bob := admin("1000"), admin("1001")
	if !bob.mayAdoptTunnel() {
		t.Fatal("no tunnel running, but adoption refused")
	}
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	if bob.mayAdoptTunnel() {
		t.Fatal("bob may adopt alice's tunnel")
	}
	if !alice.mayAdoptTunnel() {
		t.Fatal("alice may not adopt her own tunnel")
	}
}

// TestStopForUpdateSendsStopping checks the owner's UI hears Stopping before
// the stop, so it doesn't count the gap as a lost connection.
func TestStopForUpdateSendsStopping(t *testing.T) {
	useFakeTunnels(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	ui := &ManagerService{clientUserKey: "1000", events: w}
	if err := admin("1000").startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	managerServicesLock.Lock()
	managerServices[ui] = true
	managerServicesLock.Unlock()
	t.Cleanup(func() {
		managerServicesLock.Lock()
		delete(managerServices, ui)
		managerServicesLock.Unlock()
	})
	stopTunnelsForUpdate()

	var states []TunnelState
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	// One decoder for the stream, as the UI reads it (a decoder buffers ahead).
	dec := gob.NewDecoder(r)
	for len(states) < 2 {
		var nt NotificationType
		var st TunnelState
		if dec.Decode(&nt) != nil || dec.Decode(&st) != nil {
			break
		}
		states = append(states, st)
	}
	seen := map[TunnelState]bool{}
	for _, st := range states {
		seen[st] = true
	}
	if !seen[tunnel.StateStopping] || !seen[tunnel.StateStopped] {
		t.Fatalf("owner got %v, want Stopping and Stopped", states)
	}
}

func TestLegacyUpdateMethodGetsNoReply(t *testing.T) {
	useFakeTunnels(t)
	clientRead, serverWrite, _ := os.Pipe()
	serverRead, clientWrite, _ := os.Pipe()
	s := &ManagerService{clientUserKey: "1001"}
	go s.ServeConn(serverRead, serverWrite)
	t.Cleanup(func() { clientWrite.Close(); clientRead.Close() })
	enc, dec := gob.NewEncoder(clientWrite), gob.NewDecoder(clientRead)
	// An older UI sends UpdateMethodType and expects no reply; the next
	// request's reply must be the next thing on the stream.
	if err := enc.Encode(UpdateMethodType); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(MayAdoptTunnelMethodType); err != nil {
		t.Fatal(err)
	}
	var mayAdopt bool
	if err := dec.Decode(&mayAdopt); err != nil || !mayAdopt {
		t.Fatalf("reply after legacy Update = %v %v, want true", mayAdopt, err)
	}
}

// TestAdminUpdateWithOtherUsers: on Linux a sudo/wheel token alone doesn't
// let an update drop other users' tunnels (#101); root's does, and an admin
// alone on the computer updates without a prompt.
func TestAdminUpdateWithOtherUsers(t *testing.T) {
	useFakeTunnels(t)
	wheel := &ManagerService{clientUserKey: "1001", elevatedToken: 1}
	root := &ManagerService{clientUserKey: "0", elevatedToken: 1}
	if err := wheel.authorizeUpdate(); err != nil {
		t.Fatalf("admin alone = %v, want allowed", err)
	}
	if err := admin("1000").startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := wheel.authorizeUpdate(); !errors.Is(err, errUpdateNeedsAdmin) {
		t.Fatalf("wheel member while another user's tunnel runs = %v, want refused", err)
	}
	if err := root.authorizeUpdate(); err != nil {
		t.Fatalf("root = %v, want allowed", err)
	}
	if err := admin("1000").StopTunnel(); err != nil {
		t.Fatal(err)
	}
	attachedUIsLock.Lock()
	attachedUIs[1000] = &attachedUI{done: make(chan struct{})}
	attachedUIsLock.Unlock()
	t.Cleanup(func() {
		attachedUIsLock.Lock()
		delete(attachedUIs, 1000)
		attachedUIsLock.Unlock()
	})
	if err := wheel.authorizeUpdate(); !errors.Is(err, errUpdateNeedsAdmin) {
		t.Fatalf("wheel member with another user's UI attached = %v, want refused", err)
	}
}

func TestOtherSessionsTunnelRunning(t *testing.T) {
	useFakeTunnels(t)
	alice := admin("1000")
	alice.session = 1000
	if otherSessionsTunnelRunning(1001) {
		t.Fatal("no tunnel, but reported running")
	}
	if err := alice.startTunnelChecked(tunnel.Config{}); err != nil {
		t.Fatal(err)
	}
	if !otherSessionsTunnelRunning(1001) {
		t.Fatal("alice's tunnel not seen from bob's session")
	}
	if otherSessionsTunnelRunning(1000) {
		t.Fatal("alice's own tunnel blocks her staged install")
	}
}
