package managers

import (
	"encoding/gob"
	"os"
	"testing"
)

// Audit #23: a UI receives the notifications in the order they were sent,
// so a late Stopping (or download progress) can't overwrite a newer state.
func TestNotifyAllDeliversInOrder(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &ManagerService{events: w}
	managerServicesLock.Lock()
	managerServices[s] = true
	managerServicesLock.Unlock()
	t.Cleanup(func() {
		managerServicesLock.Lock()
		delete(managerServices, s)
		managerServicesLock.Unlock()
		s.stopEvents()
		w.Close()
		r.Close()
	})

	const n = 2000
	go func() {
		for i := range n {
			IPCServerNotifyUIAction(uint32(i), UIActionShow)
		}
	}()

	decoder := gob.NewDecoder(r)
	for i := range n {
		var notificationType NotificationType
		var sessionID uint32
		var action UIAction
		if err := decoder.Decode(&notificationType); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if err := decoder.Decode(&sessionID); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if err := decoder.Decode(&action); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if notificationType != UIActionNotificationType || sessionID != uint32(i) {
			t.Fatalf("event %d arrived as type %d with sequence %d: out of order", i, notificationType, sessionID)
		}
	}
}

// Notifications for a UI that has disconnected are dropped, and its writer
// goroutine ends.
func TestQueueEventAfterStopIsDropped(t *testing.T) {
	s := &ManagerService{}
	s.stopEvents()
	s.queueEvent([]byte("x"))
	s.queue.mu.Lock()
	defer s.queue.mu.Unlock()
	if len(s.queue.pending) != 0 || s.queue.running {
		t.Fatal("event queued for a stopped UI")
	}
}
