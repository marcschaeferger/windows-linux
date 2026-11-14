package managers

import (
	"errors"
	"net"
	"syscall"
	"unsafe"
)

// procGetNamedPipeClientSessionId is declared in cli_secrets_pipe_windows.go.

// pipeClientSessionID returns the session of the process that opened the
// client end of conn. The pipe file system records it when the pipe is
// opened, so unlike a PID it can't be swapped for another process's later.
func pipeClientSessionID(conn net.Conn) (uint32, error) {
	handleProvider, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return 0, syscall.EINVAL
	}
	var session uint32
	r0, _, err := procGetNamedPipeClientSessionId.Call(handleProvider.Fd(), uintptr(unsafe.Pointer(&session)))
	if r0 == 0 {
		if err != nil && !errors.Is(err, syscall.Errno(0)) {
			return 0, err
		}
		return 0, syscall.EINVAL
	}
	return session, nil
}

// pipeClientInSession reports whether the client of conn runs in session.
// The UI-launch and UI-action pipes allow Everyone, so a request may only
// name the caller's own session.
func pipeClientInSession(conn net.Conn, session uint32) bool {
	clientSession, err := pipeClientSessionID(conn)
	return err == nil && clientSession == session
}
