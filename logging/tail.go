package logging

import "bytes"

const (
	// tailChunk is how much one read asks for.
	tailChunk = 256 << 10
	// MaxTailLine is the longest line a Tailer returns whole; the rest of a
	// longer line is dropped and the line ends in TruncatedMarker.
	MaxTailLine = 1 << 20
	// TruncatedMarker ends a line that was cut at MaxTailLine.
	TruncatedMarker = " …[line truncated]"
)

// Chunk is a piece of a log. Next is the offset to continue from, which is
// not always Offset+len(Data): a reader may redact the bytes, or cut them
// short at a line end. Size is the size of the whole log; a size below the
// offset asked for means the log was rotated.
type Chunk struct {
	Data []byte
	Next int64
	Size int64
}

// ReadFunc reads up to max bytes of a log from offset. A read with max 0
// only reports the size.
type ReadFunc func(offset int64, max int) (Chunk, error)

// Tailer follows a growing log file and returns it line by line. Unlike a
// bufio.Scanner it has no line length limit short of MaxTailLine, and it
// returns only complete lines: a line the writer is still in the middle of
// waits for the next poll instead of showing up in two pieces.
type Tailer struct {
	tailBytes int64

	pos, size int64
	partial   []byte
	// skipping is set while the rest of an overlong line, or the cut-off
	// start of the first line read from the middle of the file, is dropped.
	skipping bool
}

// NewTailer returns a Tailer whose first read starts at most tailBytes
// before the end of the log.
func NewTailer(tailBytes int64) *Tailer {
	return &Tailer{tailBytes: tailBytes}
}

// Start reads the end of the log and returns its complete lines.
func (t *Tailer) Start(read ReadFunc) ([]string, error) {
	t.pos, t.size, t.partial, t.skipping = 0, 0, nil, false
	probe, err := read(0, 0)
	if err != nil {
		return nil, err
	}
	if t.tailBytes > 0 && probe.Size > t.tailBytes {
		t.pos = probe.Size - t.tailBytes
		// Beginning in the middle of a line, so drop it.
		t.skipping = true
	}
	t.size = probe.Size
	return t.drain(read)
}

// Poll returns the lines added since the last call. reset reports that the
// log was rotated or truncated; the lines are then the start of the new log
// and replace what was shown before.
func (t *Tailer) Poll(read ReadFunc) (lines []string, reset bool, err error) {
	probe, err := read(t.pos, 0)
	if err != nil {
		return nil, false, err
	}
	size := probe.Size
	if size < t.size || size < t.pos {
		lines, err = t.Start(read)
		return lines, true, err
	}
	t.size = size
	lines, err = t.drain(read)
	return lines, false, err
}

// drain reads everything between pos and the end of the log.
func (t *Tailer) drain(read ReadFunc) ([]string, error) {
	var lines []string
	for {
		chunk, err := read(t.pos, tailChunk)
		if err != nil {
			return lines, err
		}
		t.size = chunk.Size
		if chunk.Next <= t.pos {
			return lines, nil
		}
		t.pos = chunk.Next
		lines = t.feed(lines, chunk.Data)
	}
}

func (t *Tailer) feed(lines []string, data []byte) []string {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if t.skipping {
			if i < 0 {
				return lines
			}
			t.skipping = false
			data = data[i+1:]
			continue
		}
		if i < 0 {
			t.partial = append(t.partial, data...)
			if len(t.partial) > MaxTailLine {
				lines = append(lines, string(t.partial[:MaxTailLine])+TruncatedMarker)
				t.partial = nil
				t.skipping = true
			}
			return lines
		}
		t.partial = append(t.partial, data[:i]...)
		data = data[i+1:]
		line := t.partial
		t.partial = nil
		if len(line) > MaxTailLine {
			lines = append(lines, string(line[:MaxTailLine])+TruncatedMarker)
			continue
		}
		lines = append(lines, string(bytes.TrimSuffix(line, []byte{'\r'})))
	}
	return lines
}
