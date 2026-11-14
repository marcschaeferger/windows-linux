package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIPC stands in for the manager service.
type fakeIPC struct {
	mu        sync.Mutex
	calls     []string
	startHook func(Config) error
	stopHook  func() error
	cb        func(State)
}

func (f *fakeIPC) StartTunnel(c Config) error {
	f.mu.Lock()
	f.calls = append(f.calls, "start")
	hook := f.startHook
	f.mu.Unlock()
	if hook != nil {
		return hook(c)
	}
	return nil
}

func (f *fakeIPC) StopTunnel() error {
	f.mu.Lock()
	f.calls = append(f.calls, "stop")
	hook := f.stopHook
	f.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

func (f *fakeIPC) RegisterStateChangeCallback(cb func(State)) func() {
	f.mu.Lock()
	f.cb = cb
	f.mu.Unlock()
	return func() {}
}

func (f *fakeIPC) notify(state State) {
	f.mu.Lock()
	cb := f.cb
	f.mu.Unlock()
	cb(state)
}

func (f *fakeIPC) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeIPC) count(call string) int {
	n := 0
	for _, c := range f.callLog() {
		if c == call {
			n++
		}
	}
	return n
}

var errNoOLM = errors.New("olm is not running")

// newTestManager returns a Manager with fake dependencies: an org is
// selected, preparing succeeds at once, and OLM never answers.
func newTestManager(t *testing.T, ipc *fakeIPC) *Manager {
	t.Helper()
	tm := NewManager(nil, nil, nil, nil, ipc)
	tm.orgSelected = func() bool { return true }
	tm.prepareConfig = func() (Config, error) { return Config{Name: DefaultTunnelName}, nil }
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) { return nil, errNoOLM }
	tm.pollInterval = 2 * time.Millisecond
	tm.startupUnreachablePolls = 1000
	t.Cleanup(tm.Close)
	return tm
}

// stateRecorder records the states passed to the state callback.
type stateRecorder struct {
	mu     sync.Mutex
	states []State
}

func (r *stateRecorder) record(s State) {
	r.mu.Lock()
	r.states = append(r.states, s)
	r.mu.Unlock()
}

func (r *stateRecorder) all() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]State(nil), r.states...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (tm *Manager) pollerRunning() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.pollCancel != nil
}

// Audit #44: turning the switch off while Connect is still preparing (server
// settings, exit node lookup) must cancel it instead of starting the tunnel.
func TestDisconnectWhilePreparingCancelsConnect(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	entered := make(chan struct{})
	release := make(chan struct{})
	tm.prepareConfig = func() (Config, error) {
		close(entered)
		<-release
		return Config{Name: DefaultTunnelName}, nil
	}

	result := make(chan error, 1)
	go func() { result <- tm.Connect() }()
	<-entered
	if got := tm.State(); got != StateStarting {
		t.Fatalf("state while preparing = %v, want starting", got)
	}
	if err := tm.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("cancelled Connect returned %v, want nil", err)
	}

	if n := ipc.count("start"); n != 0 {
		t.Fatalf("StartTunnel called %d times after the connect was cancelled", n)
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	if got := tm.TakeStopReason(); got != StopReasonUser {
		t.Fatalf("stop reason = %v, want user", got)
	}
	if tm.pollerRunning() {
		t.Fatal("status poller running after a cancelled connect")
	}
}

// Audit #44: a Disconnect that arrives while the manager is starting the
// tunnel must stop it after it started, and Connect must not start polling.
func TestDisconnectWhileStartingStopsTunnelAfterStart(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	entered := make(chan struct{})
	release := make(chan struct{})
	ipc.startHook = func(Config) error {
		close(entered)
		<-release
		return nil
	}

	connectDone := make(chan error, 1)
	go func() { connectDone <- tm.Connect() }()
	<-entered

	disconnectDone := make(chan error, 1)
	go func() { disconnectDone <- tm.Disconnect() }()
	eventually(t, "the disconnect to begin", func() bool { return tm.State() == StateStopping })
	close(release)

	if err := <-connectDone; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := <-disconnectDone; err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got, want := ipc.callLog(), []string{"start", "stop"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("manager calls = %v, want %v", got, want)
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	if tm.pollerRunning() {
		t.Fatal("status poller running for a cancelled connect")
	}
}

// Audit #70: concurrent Connect calls (autoConnect, Always-On resume, a click)
// must start the tunnel only once.
func TestConcurrentConnectStartsTunnelOnce(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	tm.prepareConfig = func() (Config, error) {
		time.Sleep(10 * time.Millisecond)
		return Config{Name: DefaultTunnelName}, nil
	}

	const callers = 8
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	start := make(chan struct{})
	for range callers {
		wg.Go(func() {
			<-start
			if tm.Connect() == nil {
				succeeded.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if n := ipc.count("start"); n != 1 {
		t.Fatalf("StartTunnel called %d times, want 1", n)
	}
	if n := succeeded.Load(); n != 1 {
		t.Fatalf("%d Connect calls succeeded, want 1", n)
	}
}

// Audit #23: Disconnect ends in Stopped by itself; the manager's notifications
// may be late or out of order, and a late Stopping must not undo it.
func TestDisconnectEndsStoppedAndIgnoresLateStopping(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	rec := &stateRecorder{}
	tm.RegisterStateChangeCallback(rec.record)

	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tm.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state after Disconnect = %v, want stopped", got)
	}

	// The manager's own notifications for that stop, arriving late and
	// reordered, plus a duplicate.
	ipc.notify(StateStopped)
	ipc.notify(StateStopping)
	ipc.notify(StateStopped)
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state after late notifications = %v, want stopped", got)
	}
	want := []State{StateStarting, StateStopping, StateStopped}
	got := rec.all()
	if len(got) != len(want) {
		t.Fatalf("callback states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("callback states = %v, want %v", got, want)
		}
	}

	// The switch must work again.
	if err := tm.Connect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
}

// Audit #23: a stop notification of an earlier connection that arrives
// while a new Connect is in progress must not mark the new one stopped.
func TestStopNotificationDuringConnectIsIgnored(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	entered := make(chan struct{})
	release := make(chan struct{})
	tm.prepareConfig = func() (Config, error) {
		close(entered)
		<-release
		return Config{Name: DefaultTunnelName}, nil
	}

	result := make(chan error, 1)
	go func() { result <- tm.Connect() }()
	<-entered
	ipc.notify(StateStopping)
	ipc.notify(StateStopped)
	if got := tm.State(); got != StateStarting {
		t.Fatalf("state = %v after a stale stop notification, want starting", got)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if n := ipc.count("start"); n != 1 {
		t.Fatalf("StartTunnel called %d times, want 1", n)
	}
}

// A stop reported by the manager outside of this Manager's own operations
// (another session, the daemon) is applied and ends the poller.
func TestExternalStopNotificationApplies(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		return &OLMStatusResponse{Registered: true}, nil
	}
	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	eventually(t, "OLM to answer", func() bool {
		tm.mu.RLock()
		defer tm.mu.RUnlock()
		return !tm.connecting
	})
	ipc.notify(StateStopping)
	ipc.notify(StateStopped)
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	if tm.pollerRunning() {
		t.Fatal("status poller still running after the tunnel was stopped")
	}
}

// Audit #23: the previous stop's notifications can still be on their way
// after the new tunnel's StartTunnel returned (a quick Always-On reconnect);
// they are ignored until the new OLM has answered.
func TestStaleStopAfterStartTunnelIsIgnored(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// StartTunnel has returned; OLM has not answered yet (fetch fails).
	ipc.notify(StateStopping)
	ipc.notify(StateStopped)
	if got := tm.State(); got != StateStarting {
		t.Fatalf("state = %v after a stale stop, want starting", got)
	}
	if !tm.pollerRunning() {
		t.Fatal("a stale stop ended the new connection's poller")
	}
}

// Audit #24: an OLM that answers but is not registered (stopped through its
// own /disconnect) is not adopted, since nothing would move it on.
func TestUnregisteredTunnelIsNotAdopted(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		return &OLMStatusResponse{}, nil
	}
	if tm.AdoptRunningTunnel("") {
		t.Fatal("adopted a tunnel that is not registered")
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		return &OLMStatusResponse{Registered: true, Connected: true}, nil
	}
	if !tm.AdoptRunningTunnel("") || tm.State() != StateRunning {
		t.Fatalf("a connected tunnel was not adopted (state %v)", tm.State())
	}
}

// Audit #24: a tunnel that never answers while starting (the process exited
// before OLM came up) is detected; the UI must not stay on "Connecting".
func TestPollerDetectsTunnelThatDiesWhileStarting(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	tm.startupUnreachablePolls = 5
	var reported atomic.Pointer[OLMStatusError]
	tm.RegisterErrorCallback(func(e *OLMStatusError) { reported.Store(e) })

	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	eventually(t, "the dead tunnel to be detected", func() bool { return tm.State() == StateStopped })
	if got := tm.TakeStopReason(); got != StopReasonError {
		t.Fatalf("stop reason = %v, want error", got)
	}
	if n := ipc.count("stop"); n != 1 {
		t.Fatalf("StopTunnel called %d times, want 1", n)
	}
	eventually(t, "the error to be reported", func() bool { return reported.Load() != nil })
	if got := reported.Load().Code; got != tunnelExitedCode {
		t.Fatalf("reported error code %q, want %q", got, tunnelExitedCode)
	}
}

// Audit #24: once OLM has answered, failures in a connecting state (for
// example Registered after a drop) count with the short limit.
func TestPollerDetectsDeathAfterAPIAnsweredWithShortLimit(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	var polls atomic.Int32
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		if polls.Add(1) <= 3 {
			return &OLMStatusResponse{Registered: true}, nil
		}
		return nil, errNoOLM
	}

	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// startupUnreachablePolls is 1000, so this only ends with the short limit.
	eventually(t, "the dead tunnel to be detected", func() bool { return tm.State() == StateStopped })
	if got := tm.TakeStopReason(); got != StopReasonError {
		t.Fatalf("stop reason = %v, want error (it never connected)", got)
	}
}

// A tunnel that was running and then dies is reported as lost, so Always-On
// reconnects it.
func TestPollerReportsLostAfterRunning(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	var polls atomic.Int32
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		if polls.Add(1) <= 3 {
			return &OLMStatusResponse{Registered: true, Connected: true}, nil
		}
		return nil, errNoOLM
	}
	rec := &stateRecorder{}
	tm.RegisterStateChangeCallback(rec.record)

	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	eventually(t, "the lost tunnel to be detected", func() bool { return tm.State() == StateStopped })
	if got := tm.TakeStopReason(); got != StopReasonLost {
		t.Fatalf("stop reason = %v, want lost", got)
	}
	sawRunning := false
	for _, s := range rec.all() {
		sawRunning = sawRunning || s == StateRunning
	}
	if !sawRunning {
		t.Fatalf("states %v never reached running", rec.all())
	}
}

// Audit #66: a poller whose request was in flight when its connection ended
// must neither act on the result nor stop the next connection's poller.
func TestStalePollerDoesNotAffectNewConnection(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	tm.fetchStatus = func(context.Context) (*OLMStatusResponse, error) {
		if first.CompareAndSwap(false, true) {
			close(inFlight)
			<-release // ignores ctx, like a request stuck on a hung pipe
			return &OLMStatusResponse{Terminated: true}, nil
		}
		return &OLMStatusResponse{Registered: true}, nil
	}

	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	<-inFlight
	if err := tm.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if err := tm.Connect(); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	close(release)

	// Give the old poller time to (wrongly) act.
	time.Sleep(50 * time.Millisecond)
	if got := tm.State(); got == StateStopped || got == StateStopping {
		t.Fatalf("state = %v: the old poller stopped the new connection", got)
	}
	if n := ipc.count("stop"); n != 1 {
		t.Fatalf("StopTunnel called %d times, want 1 (only the user's Disconnect)", n)
	}
	if !tm.pollerRunning() {
		t.Fatal("the new connection's poller was forgotten")
	}
	if err := tm.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if tm.pollerRunning() {
		t.Fatal("poller still running after Disconnect")
	}
}

// Audit #70: the manager's state notifications read the callback while the
// UI registers it (go test -race).
func TestStateCallbackRegistrationRace(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			tm.RegisterStateChangeCallback(func(State) {})
		}
	})
	wg.Go(func() {
		for i := range 200 {
			if i%2 == 0 {
				ipc.notify(StateRunning)
			} else {
				ipc.notify(StateStopped)
			}
		}
	})
	wg.Wait()
}

// Audit #46/#68: an account change stops the tunnel through the Manager, so
// polling ends and the stop counts as the user's; a tunnel the Manager
// doesn't know about is stopped too.
func TestStopForAccountChange(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	if err := tm.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tm.StopForAccountChange(); err != nil {
		t.Fatalf("StopForAccountChange: %v", err)
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	if got := tm.TakeStopReason(); got != StopReasonUser {
		t.Fatalf("stop reason = %v, want user", got)
	}
	if tm.pollerRunning() {
		t.Fatal("status poller still running")
	}

	// Already stopped: the manager is still asked to stop whatever runs.
	before := ipc.count("stop")
	if err := tm.StopForAccountChange(); err != nil {
		t.Fatalf("StopForAccountChange while stopped: %v", err)
	}
	if got := ipc.count("stop"); got != before+1 {
		t.Fatalf("StopTunnel calls = %d, want %d", got, before+1)
	}
}

// A failed prepare (credentials, config, a forced exit node that can't be
// looked up) ends the attempt as an error.
func TestConnectPrepareErrorStops(t *testing.T) {
	ipc := &fakeIPC{}
	tm := newTestManager(t, ipc)
	want := formatConnectionError("Exit Node Unavailable", "required exit node unknown", nil)
	tm.prepareConfig = func() (Config, error) { return Config{}, want }

	err := tm.Connect()
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Title != want.Title {
		t.Fatalf("Connect error = %v, want %v", err, want)
	}
	if got := tm.State(); got != StateStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	if got := tm.TakeStopReason(); got != StopReasonError {
		t.Fatalf("stop reason = %v, want error", got)
	}
	if n := ipc.count("start"); n != 0 {
		t.Fatalf("StartTunnel called %d times", n)
	}
}

// Audit #47: IPv6 upstream DNS servers need brackets in host:port form.
func TestUpstreamDNSAddress(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1.1.1.1", "1.1.1.1:53"},
		{" 9.9.9.9 ", "9.9.9.9:53"},
		{"2606:4700::1111", "[2606:4700::1111]:53"},
		{"[2606:4700::1111]", "[2606:4700::1111]:53"},
		{"::1", "[::1]:53"},
		{"1.1.1.1:5353", "1.1.1.1:5353"},
		{"[2001:db8::1]:5353", "[2001:db8::1]:5353"},
		{"dns.example.com", "dns.example.com:53"},
	}
	for _, tt := range tests {
		got := upstreamDNSAddress(tt.in)
		if got != tt.want {
			t.Errorf("upstreamDNSAddress(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Errorf("upstreamDNSAddress(%q) = %q is not a valid host:port: %v", tt.in, got, err)
		}
	}
}

// Audit #72: one OLM HTTP client for all calls, without idle connections
// that outlive the tunnel.
func TestOLMHTTPClientIsShared(t *testing.T) {
	a, b := olmHTTPClient(), olmHTTPClient()
	if a != b {
		t.Fatal("olmHTTPClient returned a new client")
	}
	transport, ok := a.Transport.(*http.Transport)
	if !ok || !transport.DisableKeepAlives {
		t.Fatal("OLM transport keeps idle connections")
	}
}
