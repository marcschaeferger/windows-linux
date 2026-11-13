package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fosrl/newt/logger"
	"github.com/fosrl/windows-linux/logging"
	"github.com/fosrl/windows-linux/managers"
)

const (
	maxLogLinesDisplayed = 10000
	maxInitialLogLines   = 1000
	// initialLogTailBytes is how much of the end of the log the first load reads.
	initialLogTailBytes = 1 << 20
)

// LogEntry is a log line as sent to the Logs tab. Seq increases with every
// line read, so the frontend can drop appends it already has.
type LogEntry struct {
	Seq   uint64 `json:"seq"`
	Stamp string `json:"stamp"`
	Level string `json:"level"`
	Line  string `json:"line"`
}

// LogsSnapshot replaces everything shown in the Logs tab.
type LogsSnapshot struct {
	Entries []LogEntry `json:"entries"`
}

type logEntryItem struct {
	seq  uint64
	line LogLine
}

var (
	logsMu    sync.Mutex
	logsQuit  chan struct{}
	logsItems []logEntryItem
	logsSeq   uint64
)

// readDaemonLog reads the log the Logs tab shows: the one the manager service
// and tunnels write, where connection problems show up. That log is not
// readable by users (on Linux only root can read it), so the daemon reads it
// for us. Without a connection to the daemon the file is read directly, which
// works where the log is readable.
func readDaemonLog(offset int64, max int) (logging.Chunk, error) {
	chunk, err := managers.IPCClientReadLog(offset, max)
	if managers.IsIPCNotConnected(err) {
		chunk, err = managers.ReadLogFile(offset, max)
	}
	return chunk, err
}

// parseLogLines parses and redacts raw log lines. Redaction here covers logs
// written before secrets were redacted at the source, and lines from other
// writers, before anything reaches the screen, the clipboard or an export.
func parseLogLines(raw []string) []LogLine {
	parsed := make([]LogLine, 0, len(raw))
	for _, line := range raw {
		if p := parseLogLine(line); p != nil {
			parsed = append(parsed, *p)
		}
	}
	return parsed
}

func toLogEntry(it logEntryItem) LogEntry {
	return LogEntry{Seq: it.seq, Stamp: it.line.Stamp.Format(logStampFormat), Level: it.line.Level, Line: it.line.Line}
}

// logsSnapshotLocked must be called with logsMu held.
func logsSnapshotLocked() LogsSnapshot {
	entries := make([]LogEntry, len(logsItems))
	for i, it := range logsItems {
		entries[i] = toLogEntry(it)
	}
	return LogsSnapshot{Entries: entries}
}

func currentLogsSnapshot() LogsSnapshot {
	logsMu.Lock()
	defer logsMu.Unlock()
	return logsSnapshotLocked()
}

// appendLogLinesLocked must be called with logsMu held.
func appendLogLinesLocked(lines []LogLine) []LogEntry {
	added := make([]LogEntry, 0, len(lines))
	for _, l := range lines {
		logsSeq++
		it := logEntryItem{seq: logsSeq, line: l}
		logsItems = append(logsItems, it)
		added = append(added, toLogEntry(it))
	}
	if len(logsItems) > maxLogLinesDisplayed {
		logsItems = logsItems[len(logsItems)-maxLogLinesDisplayed:]
	}
	return added
}

// startLogTail loads the last lines of the log file and tails it once a second.
func startLogTail() {
	logsMu.Lock()
	if logsQuit != nil {
		logsMu.Unlock()
		return
	}
	logsQuit = make(chan struct{})
	quit := logsQuit
	logsItems = nil
	logsMu.Unlock()

	// The log comes through the daemon, which can be busy with another call,
	// so nothing here may block the window that is opening: the first load
	// happens in the background and replaces the (empty) list when it is in.
	go func() {
		tailer := logging.NewTailer(initialLogTailBytes)
		raw, err := tailer.Start(readDaemonLog)
		if err != nil {
			logger.Debug("Logs: failed to read the log: %v", err)
		}
		if len(raw) > maxInitialLogLines {
			raw = raw[len(raw)-maxInitialLogLines:]
		}

		logsMu.Lock()
		if logsQuit != quit {
			// Stopped, and maybe started again, while the log was read.
			logsMu.Unlock()
			return
		}
		logsItems = nil
		appendLogLinesLocked(parseLogLines(raw))
		snapshot := logsSnapshotLocked()
		logsMu.Unlock()
		app.Event.Emit(eventLogsReset, snapshot)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				readNewLogLines(quit, tailer)
			}
		}
	}()
}

func stopLogTail() {
	logsMu.Lock()
	defer logsMu.Unlock()
	if logsQuit != nil {
		close(logsQuit)
		logsQuit = nil
	}
}

func readNewLogLines(quit chan struct{}, tailer *logging.Tailer) {
	raw, rotated, err := tailer.Poll(readDaemonLog)
	if err != nil {
		// Keep what is shown; the daemon may be restarting. Not logged, as
		// this runs every second.
		return
	}
	newLines := parseLogLines(raw)

	logsMu.Lock()
	defer logsMu.Unlock()
	if logsQuit != quit {
		return
	}
	if rotated {
		// The log was rotated; what is shown belongs to the old file.
		logsItems = nil
		if len(newLines) > maxInitialLogLines {
			newLines = newLines[len(newLines)-maxInitialLogLines:]
		}
	}
	added := appendLogLinesLocked(newLines)
	if rotated {
		app.Event.Emit(eventLogsReset, logsSnapshotLocked())
	} else if len(added) > 0 {
		app.Event.Emit(eventLogsAppend, LogsSnapshot{Entries: added})
	}
}

func clearLogs() {
	logsMu.Lock()
	logsItems = nil
	snapshot := logsSnapshotLocked()
	logsMu.Unlock()
	app.Event.Emit(eventLogsReset, snapshot)
}

// formatLogLines formats the lines with the given sequence numbers for the clipboard or a file.
func formatLogLines(seqs []uint64) string {
	want := make(map[uint64]bool, len(seqs))
	for _, s := range seqs {
		want[s] = true
	}
	var b strings.Builder
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, it := range logsItems {
		if seqs == nil || want[it.seq] {
			b.WriteString(it.line.String())
		}
	}
	return b.String()
}

func copyLogLines(seqs []uint64) {
	if len(seqs) == 0 {
		return
	}
	app.Clipboard.SetText(formatLogLines(seqs))
}

// exportLogs asks for a file and writes all displayed log lines to it.
func exportLogs() {
	owner := preferencesWindowOrNil()
	d := app.Dialog.SaveFile().
		SetFilename(fmt.Sprintf("pangolin-log-%s.txt", time.Now().Format("2006-01-02T150405"))).
		SetMessage("Export log to file").
		AddFilter("Text Files (*.txt)", "*.txt").
		AddFilter("All Files (*.*)", "*.*")
	if owner != nil {
		d.AttachToWindow(owner)
	}
	path, err := d.PromptForSingleSelection()
	if err != nil || path == "" {
		return
	}
	if filepath.Ext(path) == "" {
		path += ".txt"
	}

	if _, err := os.Stat(path); err == nil {
		if !confirm(owner, "File Exists",
			fmt.Sprintf("The file %s already exists. Do you want to overwrite it?", filepath.Base(path)), false) {
			return
		}
	}

	if err := os.WriteFile(path, []byte(formatLogLines(nil)), 0o600); err != nil {
		logger.Error("Failed to write log file: %v", err)
		showError(owner, "Save Failed", fmt.Sprintf("Failed to write file: %v", err))
		return
	}
	showInfo(owner, "Save Successful", fmt.Sprintf("Log saved to %s", path))
}
