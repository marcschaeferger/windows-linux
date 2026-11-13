package managers

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/fosrl/windows-linux/config"
	"github.com/fosrl/windows-linux/logging"
)

// maxLogChunk caps what one ReadLog call returns, so a UI that asks for the
// whole file of a large log doesn't make the daemon allocate it at once.
const maxLogChunk = 1 << 20

// LogChunk is a piece of the daemon log: see logging.Chunk.
type LogChunk = logging.Chunk

// daemonLogPath is the log of the daemon (manager service) and its tunnels.
// A variable so tests can point it elsewhere.
var daemonLogPath = func() string {
	return filepath.Join(config.DaemonLogDir(), "pangolin.log")
}

// readLogFile reads up to max bytes of the file at path from offset, and
// redacts them. Unless the read reaches the end of the file, it stops after
// the last complete line, so a secret is never cut in two where redaction
// can't see it; Next says where to continue. A missing file is an empty log.
func readLogFile(path string, offset int64, max int) (LogChunk, error) {
	if offset < 0 {
		return LogChunk{}, errors.New("negative log offset")
	}
	if max > maxLogChunk || max < 0 {
		max = maxLogChunk
	}
	f, err := logging.OpenRead(path)
	if err != nil {
		if isNotExist(err) {
			return LogChunk{}, nil
		}
		return LogChunk{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return LogChunk{}, err
	}
	chunk := LogChunk{Size: info.Size(), Next: offset}
	if max == 0 || offset >= chunk.Size {
		return chunk, nil
	}
	want := chunk.Size - offset
	if want > int64(max) {
		want = int64(max)
	}
	buf := make([]byte, want)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return LogChunk{}, err
	}
	buf = buf[:n]
	if offset+int64(n) < chunk.Size {
		// More follows: end at a line boundary if this chunk has one.
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		}
	}
	chunk.Next = offset + int64(len(buf))
	chunk.Data = []byte(logging.Redact(string(buf)))
	return chunk, nil
}

// ReadLog returns the part of the daemon log from offset on. The log is not
// readable by users, so the Logs tab reads it through the daemon, and the
// daemon only ever reads its own log file, never a path the UI names.
func (s *ManagerService) ReadLog(offset int64, max int) (LogChunk, error) {
	return readLogFile(daemonLogPath(), offset, max)
}

// ReadLogFile is the fallback for a UI that isn't connected to the daemon: it
// reads the log file directly, which only works where the user may read it.
func ReadLogFile(offset int64, max int) (LogChunk, error) {
	return readLogFile(daemonLogPath(), offset, max)
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
