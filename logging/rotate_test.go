package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func listLogs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestOwnerRotatesBySizeWhileRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pangolin.log")
	f, err := OpenRotatingFile(path, FileOptions{Owner: true, MaxSize: 100, KeepFiles: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	line := strings.Repeat("a", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	names := listLogs(t, dir)
	if len(names) < 2 {
		t.Fatalf("expected rotated files next to the live log, got %v", names)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 100 {
		t.Errorf("live log is %d bytes, over the 100 byte limit", info.Size())
	}
	var total int64
	for _, n := range names {
		i, _ := os.Stat(filepath.Join(dir, n))
		total += i.Size()
	}
	if total != 10*40 {
		t.Errorf("lines were lost by rotation: %d of %d bytes kept in %v", total, 400, names)
	}
}

func TestNonOwnerNeverRotatesAndFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pangolin.log")
	owner, err := OpenRotatingFile(path, FileOptions{Owner: true, MaxSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	now := time.Now()
	child, err := OpenRotatingFile(path, FileOptions{MaxSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	child.now = func() time.Time { return now }

	// A non-owner past MaxSize does not rotate.
	if _, err := child.Write([]byte("0123456789ABCDEF\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Write([]byte("second line past the limit\n")); err != nil {
		t.Fatal(err)
	}
	if names := listLogs(t, dir); len(names) != 1 {
		t.Fatalf("non-owner rotated the log: %v", names)
	}

	// The owner rotates; the child must write to the new live file, not the renamed one.
	if err := owner.rotateLocked(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := child.Write([]byte("after rotation\n")); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != "after rotation\n" {
		t.Errorf("live log = %q, want only the line written after rotation", live)
	}
}

func TestOwnerRotatesStaleFileOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pangolin.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := os.Chtimes(path, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}

	f, err := OpenRotatingFile(path, FileOptions{Owner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names := listLogs(t, dir)
	if len(names) != 2 {
		t.Fatalf("want live + rotated file, got %v", names)
	}
	// A tunnel process (non-owner) leaves a stale file alone.
	dir2 := t.TempDir()
	path2 := filepath.Join(dir2, "pangolin.log")
	if err := os.WriteFile(path2, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path2, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	g, err := OpenRotatingFile(path2, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if names := listLogs(t, dir2); len(names) != 1 {
		t.Errorf("non-owner rotated: %v", names)
	}
}

func TestPruneRotated(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	mk("pangolin.log", 0)
	mk("pangolin-old.log", 5*24*time.Hour)
	for i := 1; i <= 4; i++ {
		mk("pangolin-r"+string(rune('0'+i))+".log", time.Duration(i)*time.Hour)
	}
	mk("other.log", 10*24*time.Hour)

	pruneRotated(dir, "pangolin", 3, 2, now)
	got := strings.Join(listLogs(t, dir), ",")
	want := "other.log,pangolin-r1.log,pangolin-r2.log,pangolin.log"
	if got != want {
		t.Errorf("after prune: %s, want %s", got, want)
	}
}

func TestNewFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pangolin.log")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRotatingFile(path, FileOptions{Owner: true, FileMode: 0o640})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0o007 != 0 {
		t.Errorf("owner left the log world-accessible: %v", info.Mode())
	}
}
