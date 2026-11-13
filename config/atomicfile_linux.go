package config

import "os"

// replaceFile renames from over to.
func replaceFile(from, to string) error {
	return os.Rename(from, to)
}
