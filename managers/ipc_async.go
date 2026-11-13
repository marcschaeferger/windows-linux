package managers

import (
	"bytes"
	"encoding/gob"
	"errors"
	"sync"
)

// Long requests (installing the CLI, which can wait minutes for a polkit
// prompt or the MSI wizard, and checking for updates) are asynchronous. Each
// UI has one request stream, served one request at a time, and the client
// holds rpcMutex until the reply arrives, so a long request used to block
// every other call: the connect switch, Disconnect and Quit froze until it
// finished. Now the manager acknowledges such a request at once, runs it in
// the background and sends the result to the requesting UI as an
// AsyncResultNotificationType event. The client waits for that event without
// holding rpcMutex.

// asyncOps are the requests that run in the background.
var asyncOps = map[MethodType]func(s *ManagerService) (UpdateState, error){
	InstallCLIAsyncMethodType: func(s *ManagerService) (UpdateState, error) {
		return UpdateStateUnknown, s.InstallCLI()
	},
	CheckForUpdatesAsyncMethodType: func(s *ManagerService) (UpdateState, error) {
		return s.CheckForUpdates()
	},
}

// singleFlightOps may run only once at a time per UI connection.
var singleFlightOps = map[MethodType]bool{
	InstallCLIAsyncMethodType: true,
}

type asyncKey struct {
	s      *ManagerService
	method MethodType
}

var (
	asyncRunningLock sync.Mutex
	asyncRunning     = make(map[asyncKey]bool)
)

// errAsyncBusy is returned when the same long request is already running.
var errAsyncBusy = errors.New("this is already in progress")

// startAsync starts the long request method for s in the background. Its
// result is sent to s's UI with id once it finishes.
func (s *ManagerService) startAsync(method MethodType, id uint64) error {
	op := asyncOps[method]
	if op == nil {
		return errors.New("unknown request")
	}
	key := asyncKey{s, method}
	if singleFlightOps[method] {
		asyncRunningLock.Lock()
		if asyncRunning[key] {
			asyncRunningLock.Unlock()
			return errAsyncBusy
		}
		asyncRunning[key] = true
		asyncRunningLock.Unlock()
	}
	go func() {
		state, err := op(s)
		if singleFlightOps[method] {
			asyncRunningLock.Lock()
			delete(asyncRunning, key)
			asyncRunningLock.Unlock()
		}
		s.notify(AsyncResultNotificationType, id, state, errToString(err))
	}()
	return nil
}

// encodeNotification gob-encodes a notification and its values.
func encodeNotification(notificationType NotificationType, ifaces ...any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(notificationType); err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if err := encoder.Encode(iface); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// notify sends a notification to s's UI only.
func (s *ManagerService) notify(notificationType NotificationType, ifaces ...any) {
	event, err := encodeNotification(notificationType, ifaces...)
	if err != nil {
		return
	}
	s.queueEvent(event)
}

// Client side.

type asyncReply struct {
	state UpdateState
	err   error
}

var (
	asyncLock    sync.Mutex
	asyncNextID  uint64
	asyncWaiters = make(map[uint64]chan asyncReply)
	// asyncConn counts InitializeIPCClient calls; asyncClosed is set once the
	// current event connection is gone, so no result can arrive any more.
	asyncConn   uint64
	asyncClosed bool
)

// asyncConnected records a new event connection and returns its number.
func asyncConnected() uint64 {
	asyncLock.Lock()
	defer asyncLock.Unlock()
	asyncConn++
	asyncClosed = false
	return asyncConn
}

// ipcClientCallAsync sends a long request and waits for its result without
// blocking other requests.
func ipcClientCallAsync(method MethodType) (UpdateState, error) {
	asyncLock.Lock()
	if asyncClosed {
		asyncLock.Unlock()
		return UpdateStateUnknown, errIPCNotConnected
	}
	asyncNextID++
	id := asyncNextID
	// Registered before sending: the result can arrive before the reply.
	reply := make(chan asyncReply, 1)
	asyncWaiters[id] = reply
	asyncLock.Unlock()

	rpcMutex.Lock()
	err := func() error {
		if rpcEncoder == nil {
			return errIPCNotConnected
		}
		if err := rpcEncoder.Encode(method); err != nil {
			return err
		}
		if err := rpcEncoder.Encode(id); err != nil {
			return err
		}
		return rpcDecodeError()
	}()
	rpcMutex.Unlock()
	if err != nil {
		asyncLock.Lock()
		delete(asyncWaiters, id)
		asyncLock.Unlock()
		return UpdateStateUnknown, err
	}

	r := <-reply
	return r.state, r.err
}

// deliverAsyncResult hands a long request's result to its waiting caller.
func deliverAsyncResult(id uint64, state UpdateState, errStr string) {
	asyncLock.Lock()
	reply := asyncWaiters[id]
	delete(asyncWaiters, id)
	asyncLock.Unlock()
	if reply == nil {
		return
	}
	var err error
	if errStr != "" {
		err = errors.New(errStr)
	}
	reply <- asyncReply{state: state, err: err}
}

// failAsyncWaiters ends every waiting long request once event connection
// conn is gone.
func failAsyncWaiters(conn uint64) {
	asyncLock.Lock()
	defer asyncLock.Unlock()
	if conn != asyncConn {
		return
	}
	asyncClosed = true
	for id, reply := range asyncWaiters {
		reply <- asyncReply{state: UpdateStateUnknown, err: errIPCNotConnected}
		delete(asyncWaiters, id)
	}
}
