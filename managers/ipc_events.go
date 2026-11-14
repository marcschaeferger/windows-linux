package managers

import (
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
)

// eventQueue delivers one UI's notifications in the order they were sent.
// Writing each one from its own goroutine, as notifyAll used to, let them
// reach the UI out of order: a late Stopping after Stopped left the UI stuck
// on "Disconnecting...", and a late download progress event after an error
// left the update window stuck on "Downloading".
type eventQueue struct {
	mu      sync.Mutex
	pending [][]byte
	// wake is signalled when pending gets an event or the queue stops.
	wake    chan struct{}
	running bool
	stopped bool
}

// eventQueueSize is how many notifications may wait for a UI that reads
// slowly, so a UI that stops reading can't make the manager hold events forever.
const eventQueueSize = 1024

// queueEvent queues an encoded notification for m's UI. It never blocks on
// the UI: a single writer goroutine per UI does the writing.
func (m *ManagerService) queueEvent(event []byte) {
	q := &m.queue
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	if len(q.pending) >= eventQueueSize {
		q.mu.Unlock()
		logger.Warn("IPC: dropping a notification for %s: its event queue is full", m.clientUserKey)
		return
	}
	q.pending = append(q.pending, event)
	if q.wake == nil {
		q.wake = make(chan struct{}, 1)
	}
	if !q.running {
		q.running = true
		go m.writeEvents(q.wake)
	}
	wake := q.wake
	q.mu.Unlock()

	select {
	case wake <- struct{}{}:
	default:
	}
}

// stopEvents ends m's writer once its UI has disconnected.
func (m *ManagerService) stopEvents() {
	q := &m.queue
	q.mu.Lock()
	q.stopped = true
	q.pending = nil
	wake := q.wake
	q.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// writeEvents writes queued notifications to the UI one after another.
func (m *ManagerService) writeEvents(wake <-chan struct{}) {
	q := &m.queue
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.stopped {
			q.mu.Unlock()
			<-wake
			q.mu.Lock()
		}
		if q.stopped {
			q.running = false
			q.mu.Unlock()
			return
		}
		batch := q.pending
		q.pending = nil
		q.mu.Unlock()

		for _, event := range batch {
			m.writeEvent(event)
		}
	}
}

func (m *ManagerService) writeEvent(event []byte) {
	m.eventLock.Lock()
	defer m.eventLock.Unlock()
	if m.events != nil {
		m.events.SetWriteDeadline(time.Now().Add(time.Second))
		m.events.Write(event)
	}
}
