package logging

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeLog is an in-memory log file.
type fakeLog struct {
	mu   sync.Mutex
	data []byte
}

func (f *fakeLog) append(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, s...)
}

func (f *fakeLog) replace(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = []byte(s)
}

func (f *fakeLog) read(offset int64, max int) (Chunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	size := int64(len(f.data))
	if max == 0 || offset >= size {
		return Chunk{Next: offset, Size: size}, nil
	}
	end := offset + int64(max)
	if end > size {
		end = size
	}
	return Chunk{Data: append([]byte(nil), f.data[offset:end]...), Next: end, Size: size}, nil
}

func TestTailerHoldsBackPartialLines(t *testing.T) {
	log := &fakeLog{}
	tl := NewTailer(0)
	log.append("one\ntw")
	lines, err := tl.Start(log.read)
	if err != nil || !reflect.DeepEqual(lines, []string{"one"}) {
		t.Fatalf("Start = %q, %v", lines, err)
	}
	// The writer finishes the line later: it shows up once, whole.
	log.append("o\nthree\r\nfo")
	lines, reset, err := tl.Poll(log.read)
	if err != nil || reset || !reflect.DeepEqual(lines, []string{"two", "three"}) {
		t.Fatalf("Poll = %q, reset %v, %v", lines, reset, err)
	}
	log.append("ur\n")
	lines, _, _ = tl.Poll(log.read)
	if !reflect.DeepEqual(lines, []string{"four"}) {
		t.Fatalf("Poll = %q", lines)
	}
	lines, _, _ = tl.Poll(log.read)
	if len(lines) != 0 {
		t.Fatalf("idle Poll = %q", lines)
	}
}

func TestTailerReadsLinesOver64KiB(t *testing.T) {
	long := strings.Repeat("x", 200<<10) // over bufio.Scanner's 64 KiB limit and one read chunk
	log := &fakeLog{}
	log.append("before\n" + long + "\nafter\n")
	tl := NewTailer(0)
	lines, err := tl.Start(log.read)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0] != "before" || lines[1] != long || lines[2] != "after" {
		t.Fatalf("got %d lines, lengths %v", len(lines), lineLengths(lines))
	}

	log.append(long + "\nlater\n")
	lines, _, err = tl.Poll(log.read)
	if err != nil || len(lines) != 2 || lines[0] != long || lines[1] != "later" {
		t.Fatalf("poll after long line: %v %v", lineLengths(lines), err)
	}
}

func TestTailerTruncatesAbsurdLines(t *testing.T) {
	log := &fakeLog{}
	log.append("a\n" + strings.Repeat("y", MaxTailLine+5000) + "\nb\n")
	lines, err := NewTailer(0).Start(log.read)
	if err != nil || len(lines) != 3 {
		t.Fatalf("got %v, %v", lineLengths(lines), err)
	}
	if !strings.HasSuffix(lines[1], TruncatedMarker) || len(lines[1]) != MaxTailLine+len(TruncatedMarker) {
		t.Errorf("long line not truncated: %d bytes", len(lines[1]))
	}
	if lines[2] != "b" {
		t.Errorf("line after the long one = %q", lines[2])
	}
}

func TestTailerStartsAtTailAndDropsCutLine(t *testing.T) {
	log := &fakeLog{}
	log.append("aaaaaaaa\nbbbbbbbb\ncccccccc\n")
	lines, err := NewTailer(14).Start(log.read) // starts inside "bbbbbbbb"
	if err != nil || !reflect.DeepEqual(lines, []string{"cccccccc"}) {
		t.Fatalf("got %q, %v", lines, err)
	}
}

func TestTailerFollowsRotation(t *testing.T) {
	log := &fakeLog{}
	log.append("old1\nold2\n")
	tl := NewTailer(0)
	if _, err := tl.Start(log.read); err != nil {
		t.Fatal(err)
	}
	log.replace("new1\n")
	lines, reset, err := tl.Poll(log.read)
	if err != nil || !reset || !reflect.DeepEqual(lines, []string{"new1"}) {
		t.Fatalf("Poll = %q, reset %v, %v", lines, reset, err)
	}
	log.append("new2\n")
	lines, reset, _ = tl.Poll(log.read)
	if reset || !reflect.DeepEqual(lines, []string{"new2"}) {
		t.Fatalf("Poll after rotation = %q, reset %v", lines, reset)
	}
}

func lineLengths(lines []string) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		out[i] = len(l)
	}
	return out
}

// A reader may return fewer or different bytes than it consumed, e.g. redacted ones.
func TestTailerFollowsNextNotLength(t *testing.T) {
	raw := "secret=aaaaaaaaaaaa\nsecond\n"
	read := func(offset int64, max int) (Chunk, error) {
		size := int64(len(raw))
		if max == 0 || offset >= size {
			return Chunk{Next: offset, Size: size}, nil
		}
		return Chunk{Data: []byte(Redact(raw[offset:])), Next: size, Size: size}, nil
	}
	lines, err := NewTailer(0).Start(read)
	if err != nil || !reflect.DeepEqual(lines, []string{"secret=[REDACTED]", "second"}) {
		t.Fatalf("got %q, %v", lines, err)
	}
}
