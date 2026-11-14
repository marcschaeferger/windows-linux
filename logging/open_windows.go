//go:build windows

package logging

import (
	"os"

	"golang.org/x/sys/windows"
)

// openAppend opens path for appending like os.OpenFile does, but also shares
// the file for delete and rename. Go's own open leaves FILE_SHARE_DELETE out,
// so while any Pangolin process held the log open no other could rotate it.
func openAppend(path string, _ os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// restrictFile does nothing on Windows, where access comes from the ACL of the directory.
func restrictFile(*os.File, os.FileMode) {}

// RestrictDir limits dir and everything in it to SYSTEM and Administrators.
// The ACL is protected, so it does not inherit ProgramData's read access for
// Users. Processes of standard users log to their own directory instead.
func RestrictDir(dir string, _ os.FileMode) {
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return
	}
	_ = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

// OpenRead opens path for reading like os.Open, but shares it for delete and
// rename too, so reading the log never blocks the process that rotates it.
func OpenRead(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
