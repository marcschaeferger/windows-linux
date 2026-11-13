package managers

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/fosrl/newt/logger"
	"golang.org/x/sys/windows"
)

const cliSecretsPipePath = `\\.\pipe\pangolin-manager-cli-secrets`

var (
	modKernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	modAdvapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	procGetNamedPipeClientProcessId = modKernel32.NewProc("GetNamedPipeClientProcessId")
	procGetNamedPipeClientSessionId = modKernel32.NewProc("GetNamedPipeClientSessionId")
	procImpersonateNamedPipeClient  = modAdvapi32.NewProc("ImpersonateNamedPipeClient")
)

func runCLISecretsPipeListener(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleCLISecretsConn(conn)
	}
}

// cliSecretsReadTimeout bounds how long a client may take to send its request.
const cliSecretsReadTimeout = 5 * time.Second

func handleCLISecretsConn(conn net.Conn) {
	defer conn.Close()

	handleProvider, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		logger.Error("CLI secrets pipe: connection has no handle")
		return
	}
	pipeHandle := windows.Handle(handleProvider.Fd())

	// Open the client process right away, before reading anything, for the
	// fallback identification below. Holding the handle keeps Windows from
	// reusing its PID while this connection is served.
	client, clientErr := openPipeClientProcess(pipeHandle)
	if clientErr == nil {
		defer windows.CloseHandle(client.process)
	}

	// Read the request first: a named pipe client can only be impersonated
	// once data has been read from the pipe.
	_ = conn.SetReadDeadline(time.Now().Add(cliSecretsReadTimeout))
	userID, err := readCLISecretsUserID(conn)
	if err != nil {
		logger.Error("CLI secrets: read user id failed: %v", err)
		writeCLISecretsResponse(conn, cliSecretsStatusError, []byte("invalid request"))
		return
	}

	windowsSID, err := pipeClientWindowsSID(pipeHandle, client, clientErr)
	if err != nil {
		logger.Error("CLI secrets pipe: resolve caller SID failed: %v", err)
		writeCLISecretsResponse(conn, cliSecretsStatusError, []byte("failed to resolve caller identity"))
		return
	}

	serveCLISecretsRequest(conn, windowsSID, userID)
}

// pipeClient is the process on the other end of the pipe, opened when the
// connection was accepted, and the session the pipe reports for the client.
type pipeClient struct {
	pid     uint32
	session uint32
	process windows.Handle
}

func openPipeClientProcess(pipeHandle windows.Handle) (pipeClient, error) {
	var c pipeClient
	if r0, _, e1 := procGetNamedPipeClientProcessId.Call(uintptr(pipeHandle), uintptr(unsafe.Pointer(&c.pid))); r0 == 0 {
		return c, callErr(e1)
	}
	if r0, _, e1 := procGetNamedPipeClientSessionId.Call(uintptr(pipeHandle), uintptr(unsafe.Pointer(&c.session))); r0 == 0 {
		return c, callErr(e1)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, c.pid)
	if err != nil {
		return c, err
	}
	c.process = process
	return c, nil
}

// pipeClientWindowsSID returns the SID of the user on the other end of the
// pipe. It takes it from the client's own token by impersonating the client,
// which cannot be confused with another process. A client that connected with
// the anonymous impersonation level (go-winio's DialPipe default, used by
// released Pangolin CLIs) has no token to query; for those the SID comes from
// the client process opened when the connection was accepted, which must be
// in the session the pipe reports for the client.
func pipeClientWindowsSID(pipeHandle windows.Handle, client pipeClient, clientErr error) (string, error) {
	sid, err := pipeClientSIDByImpersonation(pipeHandle)
	if err == nil {
		return sid, nil
	}
	if !errors.Is(err, windows.ERROR_CANT_OPEN_ANONYMOUS) && !errors.Is(err, windows.ERROR_BAD_IMPERSONATION_LEVEL) {
		return "", fmt.Errorf("impersonate pipe client: %w", err)
	}
	if clientErr != nil {
		return "", fmt.Errorf("open pipe client process: %w", clientErr)
	}
	logger.Debug("CLI secrets pipe: client connected anonymously, identifying it by process")
	return pipeClientSIDByProcess(client)
}

// pipeClientSIDByImpersonation impersonates the pipe client on a locked OS
// thread and reads the user SID from the impersonation token.
func pipeClientSIDByImpersonation(pipeHandle windows.Handle) (string, error) {
	runtime.LockOSThread()
	reverted := true
	defer func() {
		// A thread that could not stop impersonating is left locked, so the
		// runtime ends it with the goroutine instead of reusing it.
		if reverted {
			runtime.UnlockOSThread()
		}
	}()

	if r0, _, e1 := procImpersonateNamedPipeClient.Call(uintptr(pipeHandle)); r0 == 0 {
		return "", callErr(e1)
	}
	var token windows.Token
	// openAsSelf: check access against the service's own token, not the
	// (possibly identification-level) client token.
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
	if revertErr := windows.RevertToSelf(); revertErr != nil {
		reverted = false
		if err == nil {
			token.Close()
		}
		return "", fmt.Errorf("revert to self: %w", revertErr)
	}
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

// pipeClientSIDByProcess reads the SID from the token of the client process
// opened at accept time, and checks it is in the pipe client's session.
func pipeClientSIDByProcess(client pipeClient) (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(client.process, windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()

	var tokenSession uint32
	var n uint32
	if err := windows.GetTokenInformation(token, windows.TokenSessionId, (*byte)(unsafe.Pointer(&tokenSession)), uint32(unsafe.Sizeof(tokenSession)), &n); err != nil {
		return "", err
	}
	if tokenSession != client.session {
		return "", fmt.Errorf("process %d is in session %d, but the pipe client is in session %d", client.pid, tokenSession, client.session)
	}

	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func callErr(e1 error) error {
	if e1 != nil && e1 != syscall.Errno(0) {
		return e1
	}
	return syscall.EINVAL
}
