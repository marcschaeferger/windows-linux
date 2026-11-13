package managers

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/fosrl/windows-linux/config"
	"golang.org/x/sys/unix"
)

// maxUserConfigSize bounds how much of a user's config file the daemon reads.
const maxUserConfigSize = 1024 * 1024

// sessionAutoDownloadUpdates reports whether the user uid has automatic
// update downloads on, in the machine config or their own. If their config
// can't be read, only the machine config counts.
func sessionAutoDownloadUpdates(uid uint32) (bool, error) {
	data, err := readUserConfig(uid)
	return config.AutoDownloadUpdatesEnabledWithUserConfig(data), err
}

// userConfigDirs remembers where each attached UI keeps its config: the UI
// writes to $XDG_CONFIG_HOME/pangolin-client, which the root daemon can't see
// in its own environment.
var (
	userConfigDirs     = make(map[uint32]string)
	userConfigDirsLock sync.Mutex
)

// configHomeFromEnviron returns XDG_CONFIG_HOME from a /proc/<pid>/environ
// image (NUL-separated KEY=VALUE), or "" when it is unset or not absolute, as
// os.UserConfigDir treats it.
func configHomeFromEnviron(environ []byte) string {
	for _, kv := range strings.Split(string(environ), "\x00") {
		if v, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
			if filepath.IsAbs(v) {
				return v
			}
			return ""
		}
	}
	return ""
}

// rememberUserConfigDir records the config directory of the UI process pid
// for uid, so settings the daemon acts on (automatic update downloads) are
// read from where the UI saved them.
func rememberUserConfigDir(uid uint32, pid int32) {
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return
	}
	dir := ""
	if home := configHomeFromEnviron(environ); home != "" {
		dir = config.UserDataDirUnder(home)
	}
	userConfigDirsLock.Lock()
	defer userConfigDirsLock.Unlock()
	if dir == "" {
		delete(userConfigDirs, uid)
	} else {
		userConfigDirs[uid] = dir
	}
}

// userConfigPaths lists where uid's config file may be: the directory their
// UI reported, then ~/.config/pangolin-client.
func userConfigPaths(uid uint32, home string) []string {
	var paths []string
	userConfigDirsLock.Lock()
	dir := userConfigDirs[uid]
	userConfigDirsLock.Unlock()
	if dir != "" {
		paths = append(paths, filepath.Join(dir, config.ConfigFileName))
	}
	return append(paths, filepath.Join(config.UserDataDirFor(home), config.ConfigFileName))
}

// readUserConfig reads uid's config file, normally
// ~/.config/pangolin-client/pangolin.json (or under their $XDG_CONFIG_HOME).
// The daemon runs as root and the user controls the file, so it must be a
// regular file the user owns: a symlink, FIFO or device is refused.
func readUserConfig(uid uint32) ([]byte, error) {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, err
	}
	for _, path := range userConfigPaths(uid, u.HomeDir) {
		data, err := readOwnedFile(path, uid)
		if err != nil {
			return nil, err
		}
		if data != nil {
			return data, nil
		}
	}
	return nil, nil
}

// readOwnedFile reads path if it is a regular file owned by uid. A missing
// file gives nil, nil.
func readOwnedFile(path string, uid uint32) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uid {
		return nil, fmt.Errorf("%s is not a regular file owned by uid %d", path, uid)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUserConfigSize))
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// signedInSessions returns the UIDs of users with a login session. logind
// keeps /run/user/<uid> while a user is signed in.
func signedInSessions() []uint32 {
	entries, _ := os.ReadDir("/run/user")
	var uids []uint32
	for _, e := range entries {
		if uid, err := strconv.ParseUint(e.Name(), 10, 32); err == nil && uid != 0 && e.IsDir() {
			uids = append(uids, uint32(uid))
		}
	}
	return uids
}

// userSignedIn reports whether logind still has an open session for uid, so
// a UI that went away was quit (or crashed) rather than closed by sign-out.
func userSignedIn(uid uint32) bool {
	out, err := exec.Command("loginctl", "show-user", strconv.FormatUint(uint64(uid), 10),
		"--property=State", "--value").Output()
	if err != nil {
		return false
	}
	switch strings.TrimSpace(string(out)) {
	case "active", "online":
		return true
	}
	return false
}
