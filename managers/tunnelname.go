package managers

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxTunnelNameLength keeps the Windows service name (PangolinTunnel$<name>)
// well inside the SCM's 256-character limit.
const maxTunnelNameLength = 32

// validateTunnelName accepts only names made of ASCII letters, digits, '-' and
// '_'. The name comes from the UI over IPC and the privileged manager uses it
// in a service name and a file path, so anything else (path separators, "..",
// drive letters, ':' for alternate data streams) is refused outright instead
// of being rewritten.
func validateTunnelName(name string) error {
	if name == "" || len(name) > maxTunnelNameLength {
		return fmt.Errorf("invalid tunnel name: length must be 1-%d", maxTunnelNameLength)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("invalid tunnel name %q", name)
	}
	return nil
}

// tunnelConfigPath returns dir/<name>.json for a valid tunnel name and checks
// that the result stays directly inside dir.
func tunnelConfigPath(dir, name string) (string, error) {
	if err := validateTunnelName(name); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".json")
	if filepath.Dir(path) != filepath.Clean(dir) {
		return "", fmt.Errorf("tunnel config path escapes %s", dir)
	}
	return path, nil
}

// pathWithinFold reports whether target is base or lies below it, comparing
// case-insensitively as Windows paths are. Both paths must be absolute.
func pathWithinFold(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if !filepath.IsAbs(base) || !filepath.IsAbs(target) {
		return false
	}
	if strings.EqualFold(base, target) {
		return true
	}
	prefix := base
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return len(target) > len(prefix) && strings.EqualFold(target[:len(prefix)], prefix)
}
