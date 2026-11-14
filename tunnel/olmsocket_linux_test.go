package tunnel

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOLMSocketPathIsInPrivateDir(t *testing.T) {
	if filepath.Dir(OLMSocketPath) != OLMSocketDir {
		t.Fatalf("OLMSocketPath %s is not in OLMSocketDir %s", OLMSocketPath, OLMSocketDir)
	}
	if filepath.Dir(OLMSocketDir) != filepath.Dir(legacyOLMSocketPath) {
		t.Fatal("OLMSocketDir is not next to the legacy socket path")
	}
}

func readACL(t *testing.T, dir string) []byte {
	t.Helper()
	buf := make([]byte, 256)
	n, err := unix.Getxattr(dir, aclXattr, buf)
	if errors.Is(err, unix.ENODATA) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestGrantOLMSocketAccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "olm")
	if err := grantOLMSocketAccess(dir, 4242); err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no POSIX ACL support here")
		}
		t.Fatal(err)
	}
	if !bytes.Equal(readACL(t, dir), ownerTraverseACL(4242)) {
		if info, _ := os.Stat(dir); info.Mode().Perm() == 0o711 {
			t.Skip("no POSIX ACL support here (fell back to 0711)")
		}
		t.Fatalf("ACL = %x, want %x", readACL(t, dir), ownerTraverseACL(4242))
	}
	// The group bits show the ACL mask: traverse only.
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o710 {
		t.Fatalf("mode = %v, want 0710 (owner rwx, mask x, other none)", info.Mode().Perm())
	}

	// The next owner replaces the previous one.
	if err := grantOLMSocketAccess(dir, 4343); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readACL(t, dir), ownerTraverseACL(4343)) {
		t.Fatal("the previous owner's entry was not replaced")
	}

	// Root's tunnel: nobody else gets in.
	if err := grantOLMSocketAccess(dir, 0); err != nil {
		t.Fatal(err)
	}
	if acl := readACL(t, dir); acl != nil {
		t.Fatalf("ACL left for a root tunnel: %x", acl)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
	}

	// The tunnel process's check accepts the granted directory as it is.
	if err := grantOLMSocketAccess(dir, 4242); err != nil {
		t.Fatal(err)
	}
	if err := ensureOLMSocketDir(dir); err != nil {
		t.Fatalf("ensureOLMSocketDir on a granted dir: %v", err)
	}
	if !bytes.Equal(readACL(t, dir), ownerTraverseACL(4242)) {
		t.Fatal("ensureOLMSocketDir changed the ACL")
	}
}

func TestEnsureOLMSocketDirRefusesUnsafe(t *testing.T) {
	base := t.TempDir()

	created := filepath.Join(base, "new")
	if err := ensureOLMSocketDir(created); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(created); info.Mode().Perm() != 0o700 {
		t.Fatalf("created with %v, want 0700", info.Mode().Perm())
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureOLMSocketDir(file); err == nil {
		t.Fatal("a regular file was accepted")
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(created, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureOLMSocketDir(link); err == nil {
		t.Fatal("a symlink was accepted")
	}

	writable := filepath.Join(base, "writable")
	if err := os.Mkdir(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensureOLMSocketDir(writable); err == nil {
		t.Fatal("a world-writable directory was accepted")
	}
}

func TestOwnerTraverseACLFormat(t *testing.T) {
	acl := ownerTraverseACL(1000)
	// Header plus five entries of tag(2) perm(2) id(4).
	if len(acl) != 4+5*8 {
		t.Fatalf("len = %d", len(acl))
	}
	want := []byte{
		2, 0, 0, 0,
		0x01, 0, 7, 0, 0xff, 0xff, 0xff, 0xff, // user::rwx
		0x02, 0, 1, 0, 0xe8, 0x03, 0, 0, // user:1000:--x
		0x04, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, // group::---
		0x10, 0, 1, 0, 0xff, 0xff, 0xff, 0xff, // mask::--x
		0x20, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, // other::---
	}
	if !bytes.Equal(acl, want) {
		t.Fatalf("ACL = %x\nwant  %x", acl, want)
	}
}

// TestOLMSocketReachableOnlyByOwner checks the real permission model: a
// socket made 0666 inside the granted directory, as OLM makes it, can be
// dialled by the owner and not by another user. It needs root to switch
// users, so it runs in the build container.
func TestOLMSocketReachableOnlyByOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	base, err := os.MkdirTemp("/run", "olmtest")
	if err != nil {
		base = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(base) })
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "olm")
	if err := grantOLMSocketAccess(dir, 4242); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() == 0o711 {
		t.Skip("no POSIX ACL support here")
	}
	sock := filepath.Join(dir, "olm.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}

	// The test binary sits in a private build directory; other users need
	// a copy they can run.
	bin := filepath.Join(base, "olmtest.bin")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	dialAs := func(uid uint32) error {
		cmd := exec.Command(bin, "-test.run=^TestHelperDialOLM$")
		cmd.Dir = "/"
		cmd.Env = append(os.Environ(), "OLM_TEST_DIAL="+sock)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := dialAs(4242); err != nil {
		t.Fatalf("the owner could not reach the socket: %v", err)
	}
	if err := dialAs(4343); err == nil {
		t.Fatal("another user reached the socket")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("another user failed for an unexpected reason: %v", err)
	}
}

// TestHelperDialOLM is run by TestOLMSocketReachableOnlyByOwner as another user.
func TestHelperDialOLM(t *testing.T) {
	path := os.Getenv("OLM_TEST_DIAL")
	if path == "" {
		t.Skip("helper")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
