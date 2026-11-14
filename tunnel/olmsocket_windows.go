package tunnel

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// OLMSocketPath is the named pipe OLM serves its API on.
const OLMSocketPath = `\\.\pipe\pangolin-olm`

func dialOLM(ctx context.Context) (net.Conn, error) {
	return winio.DialPipe(OLMSocketPath, nil)
}

// prepareOLMSocket has nothing to prepare on Windows: OLM creates the pipe,
// with a DACL that allows Everyone. Restricting it to SYSTEM and the
// tunnel's owner needs an option in OLM's api_windows.go.
func prepareOLMSocket() error { return nil }
