package logging

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
)

// Writer is a logger.LogWriter that redacts every message before it writes the
// line in the newt format, "LEVEL: 2006/01/02 15:04:05 message". It also
// redacts what the olm library logs through the shared newt logger.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
	loc *time.Location
}

// NewWriter returns a Writer that writes to out. A nil out writes to stdout.
func NewWriter(out io.Writer) *Writer {
	if out == nil {
		out = os.Stdout
	}
	loc := time.Local
	if tz := os.Getenv("LOGGER_TIMEZONE"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return &Writer{out: out, loc: loc}
}

// Write implements logger.LogWriter.
func (w *Writer) Write(level logger.LogLevel, ts time.Time, message string) {
	line := level.String() + ": " + ts.In(w.loc).Format("2006/01/02 15:04:05") + " " + Redact(message) + "\n"
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = io.WriteString(w.out, line)
}

// SetOutput points the writer at out, for example a RotatingFile.
func (w *Writer) SetOutput(out io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.out = out
}

// Install makes w the writer of the shared newt logger. It only works before
// anything else has initialized that logger, so call it first thing in main.
// It reports whether w is now in use.
func Install(w *Writer) bool {
	l := logger.NewLoggerWithWriter(w)
	return logger.Init(l) == l
}
