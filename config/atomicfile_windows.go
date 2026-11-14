package config

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile renames from over to. On Windows the rename fails while any
// process (an antivirus or indexer scan, the manager service reading the
// user's config) has either file open without FILE_SHARE_DELETE, which the
// in-place write this replaced did not care about. Such opens are brief, so
// retry for a short while, as cmd/go's robustio does.
func replaceFile(from, to string) error {
	const timeout = 500 * time.Millisecond
	delay := time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		err := os.Rename(from, to)
		if err == nil || !isTransientRenameError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < 64*time.Millisecond {
			delay *= 2
		}
	}
}

func isTransientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
