package managers

import (
	"errors"
	"os"
	"testing"
	"time"
)

// Audit #49: while a long request runs (the CLI install waiting for polkit or
// the MSI wizard, an update check), the UI's other requests are answered, so
// the connect switch, Disconnect and Quit don't freeze.
func TestLongRequestDoesNotBlockOtherRequests(t *testing.T) {
	serverReader, clientWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	clientReader, serverWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventsReader, eventsWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	// Fake the long operations before anything runs concurrently.
	installEntered := make(chan struct{}, 1)
	installResult := make(chan error)
	origInstall, origCheck := asyncOps[InstallCLIAsyncMethodType], asyncOps[CheckForUpdatesAsyncMethodType]
	asyncOps[InstallCLIAsyncMethodType] = func(*ManagerService) (UpdateState, error) {
		installEntered <- struct{}{}
		return UpdateStateUnknown, <-installResult
	}
	asyncOps[CheckForUpdatesAsyncMethodType] = func(*ManagerService) (UpdateState, error) {
		return UpdateStateFoundUpdate, nil
	}

	const userKey = "test-user"
	s := &ManagerService{events: eventsWriter, clientUserKey: userKey}
	managerServicesLock.Lock()
	managerServices[s] = true
	managerServicesLock.Unlock()
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.ServeConn(serverReader, serverWriter)
	}()
	InitializeIPCClient(clientReader, clientWriter, eventsReader)
	t.Cleanup(func() {
		clientWriter.Close()
		serverReader.Close()
		<-served
		managerServicesLock.Lock()
		delete(managerServices, s)
		managerServicesLock.Unlock()
		s.stopEvents()
		eventsWriter.Close()
		eventsReader.Close()
		serverWriter.Close()
		clientReader.Close()
		setUserAlwaysOn(userKey, false)
		asyncOps[InstallCLIAsyncMethodType] = origInstall
		asyncOps[CheckForUpdatesAsyncMethodType] = origCheck
	})

	installDone := make(chan error, 1)
	go func() { installDone <- IPCClientInstallCLI() }()
	select {
	case <-installEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the install never started")
	}

	answered := make(chan bool)
	go func() {
		_ = IPCClientSetAlwaysOn(true)
		answered <- IPCClientAlwaysOn()
	}()
	select {
	case on := <-answered:
		if !on {
			t.Fatal("Always-On was not set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("requests are blocked while the install runs")
	}

	// The same long request can't run twice at once for one UI.
	if err := IPCClientInstallCLI(); err == nil || err.Error() != errAsyncBusy.Error() {
		t.Fatalf("second install = %v, want %v", err, errAsyncBusy)
	}

	installResult <- errors.New("not authorized")
	select {
	case err := <-installDone:
		if err == nil || err.Error() != "not authorized" {
			t.Fatalf("install result = %v, want not authorized", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the install result never arrived")
	}

	state, err := IPCClientCheckForUpdates()
	if err != nil || state != UpdateStateFoundUpdate {
		t.Fatalf("CheckForUpdates = %v, %v; want found update", state, err)
	}
}

// A long request still waiting when the event connection closes fails
// instead of hanging.
func TestAsyncWaitersFailWhenEventsClose(t *testing.T) {
	conn := asyncConnected()
	asyncLock.Lock()
	reply := make(chan asyncReply, 1)
	asyncWaiters[987654] = reply
	asyncLock.Unlock()

	failAsyncWaiters(conn - 1) // an earlier connection: nothing happens
	select {
	case <-reply:
		t.Fatal("an old connection's close released a waiter")
	default:
	}
	failAsyncWaiters(conn)
	select {
	case r := <-reply:
		if !errors.Is(r.err, errIPCNotConnected) {
			t.Fatalf("waiter got %v, want %v", r.err, errIPCNotConnected)
		}
	default:
		t.Fatal("waiter was not released")
	}
}
