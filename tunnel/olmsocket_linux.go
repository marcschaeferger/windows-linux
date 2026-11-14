package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/fosrl/newt/logger"
	"golang.org/x/sys/unix"
)

// OLMSocketPath is the Unix socket OLM serves its API on. OLM makes the
// socket itself world-writable (0666), so it lives in OLMSocketDir, which
// root owns and only the user who started the tunnel may enter (a POSIX ACL
// set by the daemon, GrantOLMSocketAccess). That user's UI and Pangolin CLI
// reach the API; other local users can't.
const OLMSocketPath = "/run/pangolin-client/olm/olm.sock"

// OLMSocketDir is the directory OLMSocketPath is in.
const OLMSocketDir = "/run/pangolin-client/olm"

// legacyOLMSocketPath is where the socket used to be, and where the Pangolin
// CLI looks for it. It is a symlink to OLMSocketPath; following it needs the
// same access to OLMSocketDir.
const legacyOLMSocketPath = "/run/pangolin-client/olm.sock"

func dialOLM(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", OLMSocketPath)
}

// prepareOLMSocket runs in the tunnel process before OLM creates its socket.
// The daemon has normally created OLMSocketDir and granted the owner access;
// otherwise it is created root-only. Its mode is not changed when it exists,
// as that would drop the owner's ACL entry.
func prepareOLMSocket() error {
	if err := ensureOLMSocketDir(OLMSocketDir); err != nil {
		return err
	}
	// Replace a stale socket or an old link at the legacy path; best effort.
	if err := os.Remove(legacyOLMSocketPath); err == nil || errors.Is(err, os.ErrNotExist) {
		_ = os.Symlink(OLMSocketPath, legacyOLMSocketPath)
	}
	return nil
}

// ensureOLMSocketDir creates dir as 0700 if it is missing and checks that it
// is a directory owned by this (root) user that nobody else can write to, so
// nobody else can swap the socket OLM creates and chmods in it.
func ensureOLMSocketDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not a directory owned by this user", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others", dir)
	}
	return nil
}

// GrantOLMSocketAccess makes uid, the user starting the tunnel, the only
// user besides root who can reach the OLM API, and takes access away from
// whoever had it before. The daemon calls it before starting a tunnel. If
// the filesystem has no POSIX ACL support, it falls back to letting every
// user through, as before, and logs that.
func GrantOLMSocketAccess(uid uint32) error {
	return grantOLMSocketAccess(OLMSocketDir, uid)
}

func grantOLMSocketAccess(dir string, uid uint32) error {
	if err := ensureOLMSocketDir(dir); err != nil {
		return err
	}
	if err := unix.Removexattr(dir, aclXattr); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("clear access to %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if uid == 0 {
		return nil
	}
	err := unix.Setxattr(dir, aclXattr, ownerTraverseACL(uid), 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		logger.Error("Tunnel: %s has no POSIX ACL support; every local user can reach the OLM API", dir)
		return os.Chmod(dir, 0o711)
	}
	if err != nil {
		return fmt.Errorf("grant uid %d access to %s: %w", uid, dir, err)
	}
	return nil
}

// aclXattr holds a file's access ACL in the kernel's binary format.
const aclXattr = "system.posix_acl_access"

// ownerTraverseACL is "user::rwx, user:<uid>:--x, group::---, mask::--x,
// other::---": root keeps full access, uid may only pass through.
func ownerTraverseACL(uid uint32) []byte {
	const (
		version     = 2
		tagUserObj  = 0x01
		tagUser     = 0x02
		tagGroupObj = 0x04
		tagMask     = 0x10
		tagOther    = 0x20
		undefinedID = 0xFFFFFFFF
	)
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{
		{tagUserObj, 7, undefinedID},
		{tagUser, 1, uid},
		{tagGroupObj, 0, undefinedID},
		{tagMask, 1, undefinedID},
		{tagOther, 0, undefinedID},
	}
	buf := binary.LittleEndian.AppendUint32(nil, version)
	for _, e := range entries {
		buf = binary.LittleEndian.AppendUint16(buf, e.tag)
		buf = binary.LittleEndian.AppendUint16(buf, e.perm)
		buf = binary.LittleEndian.AppendUint32(buf, e.id)
	}
	return buf
}
