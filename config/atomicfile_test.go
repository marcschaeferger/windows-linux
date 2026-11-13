package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// listDir returns the names in dir.
func listDir(t *testing.T, dir string) []string {
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

// Covers #51/#56: the data lands with the requested mode and no temp file is left.
func TestWriteFileAtomicModeAndNoTempFiles(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.json")
			data := []byte(`{"a":1}`)
			if err := writeFileAtomic(path, data, mode); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("content = %q, err %v", got, err)
			}
			if runtime.GOOS != "windows" {
				st, _ := os.Stat(path)
				// Chmod on the open file is not subject to umask.
				if st.Mode().Perm() != mode {
					t.Errorf("mode = %v, want %v", st.Mode().Perm(), mode)
				}
			}
			if names := listDir(t, dir); len(names) != 1 || names[0] != "f.json" {
				t.Errorf("leftover files: %v", names)
			}
		})
	}
}

// Covers #51/#56: overwriting replaces the whole file, even with shorter data.
func TestWriteFileAtomicOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := writeFileAtomic(path, []byte("long old content here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("got %q", got)
	}
}

// Covers #51/#56: a failing write (read-only directory) leaves the old file intact.
func TestWriteFileAtomicReadOnlyDirKeepsOldFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission enforcement")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := writeFileAtomic(path, []byte("new"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Errorf("old file changed: %q", got)
	}
}

// Covers #51/#56: when the rename fails (target is a directory) the temp file is removed.
func TestWriteFileAtomicTargetIsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	if names := listDir(t, dir); len(names) != 1 || names[0] != "target" {
		t.Errorf("leftover files: %v", names)
	}
}

// Covers #51/#56: a missing directory is an error, not a panic.
func TestWriteFileAtomicMissingDir(t *testing.T) {
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "nope", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
}

func TestBackupCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	orig := []byte("{broken")
	backup := backupCorruptFile(path, orig, 0o600)
	if backup == "" || !strings.HasPrefix(filepath.Base(backup), "x.json.corrupt-") {
		t.Fatalf("backup = %q", backup)
	}
	if got, _ := os.ReadFile(backup); !bytes.Equal(got, orig) {
		t.Errorf("backup content = %q", got)
	}
	if backupCorruptFile(filepath.Join(dir, "no", "x.json"), orig, 0o600) != "" {
		t.Error("expected empty path when backup cannot be written")
	}
}

// An existing file keeps its mode, so a user who tightened it keeps that.
func TestWriteFileAtomicKeepsExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pangolin.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 kept", info.Mode().Perm())
	}
}

// A symlinked file is updated through the link; the link stays a link.
func TestWriteFileAtomicFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced by a regular file (err=%v)", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("target = %q, want %q", b, "new")
	}
}
