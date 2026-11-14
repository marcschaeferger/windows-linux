//go:build windows

package secretstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/fosrl/newt/logger"
	"golang.org/x/sys/windows"
)

var (
	errUntrustedDir = errors.New("directory is not trusted")
	errLooseDACL    = errors.New("directory DACL is wider than expected")
)

// maxProtectedDirAttempts bounds how often an untrusted directory is moved
// aside and re-created before EnsureProtectedDir gives up (fails closed).
const maxProtectedDirAttempts = 3

// EnsureProtectedDir makes path a real directory with the owner and protected
// DACL in sddl (which must name an owner, a group and a DACL). The DACL in
// sddl must only grant access to SYSTEM and Administrators.
//
// A missing directory is created with sddl. An existing one is used as it is
// when it is a real directory (not a junction or symlink), owned by SYSTEM or
// Administrators, with a protected DACL whose allow entries are all for SYSTEM
// or Administrators. One owned by SYSTEM or Administrators whose DACL is wider
// (an administrator edited it, or an older version left it inheriting) gets
// the DACL from sddl in place, so its contents are kept; if that fails it is
// replaced like an untrusted one. Anything else, such as a
// directory a standard user created first under ProgramData, is renamed aside
// and a new directory created in its place. That is not repaired in place:
// Windows checks access when a handle is opened, so a handle its creator
// already holds would keep its rights through any change of owner or DACL.
// Renaming leaves those handles pointing at the old directory. The renamed
// directory is left for an administrator to inspect; it is not deleted,
// because walking a tree a user controls as SYSTEM could follow a junction
// they swap in.
func EnsureProtectedDir(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < maxProtectedDirAttempts; attempt++ {
		err = createProtectedDir(path16, sd)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("create %s: %w", path, err)
		}
		err = checkExistingDir(path16)
		if err == nil {
			return nil
		}
		if errors.Is(err, errLooseDACL) {
			err = resetDirSecurity(path16, sd)
			if err == nil {
				logger.Warn("Reset the access control list of %s to SYSTEM and Administrators only", path)
				continue
			}
			logger.Error("Could not reset the access control list of %s: %v", path, err)
			// Fall through and replace it like any other untrusted directory.
		} else if !errors.Is(err, errUntrustedDir) {
			return fmt.Errorf("check %s: %w", path, err)
		}
		if err := moveAside(path); err != nil {
			return fmt.Errorf("untrusted directory %s could not be moved aside: %w", path, err)
		}
	}
	return fmt.Errorf("untrusted directory %s keeps reappearing", path)
}

func createProtectedDir(path16 *uint16, sd *windows.SECURITY_DESCRIPTOR) error {
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	err := windows.CreateDirectory(path16, sa)
	runtime.KeepAlive(sd)
	return err
}

// checkExistingDir returns nil for a trusted directory at path16 and
// errUntrustedDir for anything that has to be replaced. It reads the
// directory through a handle opened without following reparse points.
func checkExistingDir(path16 *uint16) error {
	h, err := windows.CreateFile(path16, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// SYSTEM can't even read it: someone else owns it and locked it.
			return errUntrustedDir
		}
		return err
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errUntrustedDir
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !isSystemOrAdmins(owner) {
		return errUntrustedDir
	}
	if !dirSecurityTrusted(sd) {
		return errLooseDACL
	}
	return nil
}

// resetDirSecurity replaces the DACL of the directory at path16 with want's
// protected DACL, through a handle that does not follow reparse points. It is
// only used for a directory already owned by SYSTEM or Administrators, so the
// owner is kept; WRITE_DAC is all it needs, which the owner always has.
// SetKernelObjectSecurity changes this directory only and does not walk its
// children.
func resetDirSecurity(path16 *uint16, want *windows.SECURITY_DESCRIPTOR) error {
	h, err := windows.CreateFile(path16, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errUntrustedDir
	}
	err = windows.SetKernelObjectSecurity(h,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, want)
	runtime.KeepAlive(want)
	return err
}

// moveAside renames path (a directory, junction or file) to a random sibling
// name. Renaming a junction or symlink renames the link, not its target.
func moveAside(path string) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	aside := path + ".untrusted-" + hex.EncodeToString(suffix[:])
	if err := os.Rename(path, aside); err != nil {
		return err
	}
	logger.Error("Moved untrusted %s aside to %s; an administrator should inspect and delete it", path, aside)
	return nil
}

// dirSecurityTrusted reports whether sd has a SYSTEM or Administrators owner
// and a protected DACL in which every allow entry is for SYSTEM or
// Administrators.
func dirSecurityTrusted(sd *windows.SECURITY_DESCRIPTOR) bool {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !isSystemOrAdmins(owner) {
		return false
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// A NULL DACL grants everyone full access.
		return false
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !isSystemOrAdmins((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
				return false
			}
		default:
			// Object or callback ACEs never appear in what we create.
			return false
		}
	}
	return true
}

func isSystemOrAdmins(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// CreateProtectedFile creates a new file at path with the owner and protected
// DACL in sddl and writes data to it. Anything already at path, including a
// planted hard link or symlink, is removed first (the link itself), and the
// file is opened with CREATE_NEW so nothing put there in between is
// overwritten or followed. The directory should itself be protected.
func CreateProtectedFile(path, sddl string, data []byte) error {
	if err := removePathForReplace(path); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateFile(path16, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), path)
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
