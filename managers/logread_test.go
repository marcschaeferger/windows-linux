package managers

import (
	"encoding/gob"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pangolin.log")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		offset   int64
		max      int
		wantData string
		wantNext int64
		wantErr  bool
	}{
		{"whole file", 0, 100, "line one\nline two\n", 18, false},
		{"from offset", 9, 100, "line two\n", 18, false},
		{"limit cuts at a line end", 0, 12, "line one\n", 9, false},
		{"limit inside the only line returns it as is", 9, 4, "line", 13, false},
		{"size only", 0, 0, "", 0, false},
		{"offset at end", 18, 10, "", 18, false},
		{"offset past a rotated file", 500, 10, "", 500, false},
		{"negative offset", -1, 10, "", 0, true},
		{"oversized max is capped, not rejected", 0, maxLogChunk * 4, "line one\nline two\n", 18, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLogFile(path, tt.offset, tt.max)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if string(got.Data) != tt.wantData || got.Next != tt.wantNext || got.Size != 18 {
				t.Errorf("got (%q, next %d, size %d), want (%q, next %d, size 18)",
					got.Data, got.Next, got.Size, tt.wantData, tt.wantNext)
			}
		})
	}
}

func TestReadLogFileRedactsAndKeepsOffsetsInFileBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pangolin.log")
	first := "DEBUG: 2026/11/14 08:30:05 cfg {Secret:olm-secret-VALUE-12345 UserToken:tok-VALUE-98765}\n"
	second := "INFO: 2026/11/14 08:30:06 done\n"
	if err := os.WriteFile(path, []byte(first+second), 0o600); err != nil {
		t.Fatal(err)
	}

	// A chunk that ends inside the second line stops after the first, so a
	// secret is never cut where redaction can't see it.
	got, err := readLogFile(path, 0, len(first)+5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Data), "VALUE") {
		t.Errorf("chunk leaks secrets: %q", got.Data)
	}
	if got.Next != int64(len(first)) {
		t.Errorf("Next = %d, want %d (file bytes, not redacted length %d)", got.Next, len(first), len(got.Data))
	}
	rest, err := readLogFile(path, got.Next, 1000)
	if err != nil || string(rest.Data) != second {
		t.Errorf("second read = %q, %v", rest.Data, err)
	}
}

func TestReadLogFileMissingIsEmpty(t *testing.T) {
	got, err := readLogFile(filepath.Join(t.TempDir(), "nope.log"), 0, 10)
	if err != nil || got.Size != 0 || len(got.Data) != 0 {
		t.Errorf("got (%+v, %v), want an empty log", got, err)
	}
}

// The method numbers are the wire format between a UI and the daemon of the
// same release; a new method must not move an old one.
func TestMethodTypeNumbersAreStable(t *testing.T) {
	want := map[MethodType]int{
		QuitMethodType:                        0,
		GetDevicePostureMethodType:            11,
		AutoDownloadUpdatesTurnedOnMethodType: 16,
		ReadLogMethodType:                     17,
	}
	for m, n := range want {
		if int(m) != n {
			t.Errorf("method type = %d, want %d", m, n)
		}
	}
}

func TestServeConnReadLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pangolin.log")
	if err := os.WriteFile(path, []byte("hello token=abc123def456\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := daemonLogPath
	daemonLogPath = func() string { return path }
	t.Cleanup(func() { daemonLogPath = old })

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&ManagerService{}).ServeConn(clientToServerR, serverToClientW)
	}()

	enc := gob.NewEncoder(clientToServerW)
	dec := gob.NewDecoder(serverToClientR)
	for _, v := range []any{ReadLogMethodType, int64(0), 4096} {
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	var chunk LogChunk
	var errStr string
	if err := dec.Decode(&chunk); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&errStr); err != nil {
		t.Fatal(err)
	}
	if errStr != "" || string(chunk.Data) != "hello token=[REDACTED]\nworld\n" || chunk.Next != 31 {
		t.Errorf("got %q (next %d), err %q", chunk.Data, chunk.Next, errStr)
	}

	clientToServerW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return after the client closed")
	}
}
