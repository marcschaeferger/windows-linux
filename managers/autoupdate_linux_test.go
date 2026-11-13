package managers

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigHomeFromEnviron(t *testing.T) {
	tests := []struct {
		name    string
		environ string
		want    string
	}{
		{"unset", "HOME=/home/u\x00PATH=/usr/bin\x00", ""},
		{"set", "HOME=/home/u\x00XDG_CONFIG_HOME=/data/cfg\x00PATH=/usr/bin\x00", "/data/cfg"},
		{"relative is ignored like os.UserConfigDir", "XDG_CONFIG_HOME=cfg\x00", ""},
		{"empty", "XDG_CONFIG_HOME=\x00", ""},
		{"not a prefix match", "MY_XDG_CONFIG_HOME=/x\x00", ""},
		{"empty environ", "", ""},
	}
	for _, tt := range tests {
		if got := configHomeFromEnviron([]byte(tt.environ)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestUserConfigPaths(t *testing.T) {
	const uid = 4711
	userConfigDirsLock.Lock()
	delete(userConfigDirs, uid)
	userConfigDirsLock.Unlock()

	got := userConfigPaths(uid, "/home/u")
	want := []string{"/home/u/.config/pangolin-client/pangolin.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without a reported dir: %v, want %v", got, want)
	}

	userConfigDirsLock.Lock()
	userConfigDirs[uid] = "/data/cfg/pangolin-client"
	userConfigDirsLock.Unlock()
	defer func() {
		userConfigDirsLock.Lock()
		delete(userConfigDirs, uid)
		userConfigDirsLock.Unlock()
	}()
	got = userConfigPaths(uid, "/home/u")
	want = []string{"/data/cfg/pangolin-client/pangolin.json", "/home/u/.config/pangolin-client/pangolin.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with a reported dir: %v, want %v", got, want)
	}
}

func TestReadOwnedFile(t *testing.T) {
	dir := t.TempDir()
	uid := uint32(os.Getuid())

	if data, err := readOwnedFile(filepath.Join(dir, "missing.json"), uid); data != nil || err != nil {
		t.Errorf("missing file: %q, %v", data, err)
	}

	path := filepath.Join(dir, "pangolin.json")
	if err := os.WriteFile(path, []byte(`{"autoDownloadUpdates":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readOwnedFile(path, uid)
	if err != nil || string(data) != `{"autoDownloadUpdates":true}` {
		t.Errorf("own file: %q, %v", data, err)
	}

	// Another uid's file (here: any uid but ours) is refused.
	if _, err := readOwnedFile(path, uid+1); err == nil {
		t.Error("read a file owned by another uid")
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnedFile(link, uid); err == nil {
		t.Error("followed a symlink")
	}
}
