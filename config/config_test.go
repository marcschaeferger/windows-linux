package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func setupConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	return filepath.Join(UserDataDir(), ConfigFileName)
}

// Covers #51/#56: a corrupt pangolin.json is backed up before a save replaces it.
func TestConfigManagerBacksUpCorruptConfig(t *testing.T) {
	path := setupConfigDir(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"dnsOverride": tru`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cm := NewConfigManager()
	if cm.GetConfig() == nil {
		t.Fatal("nil config")
	}
	backups := corruptBackups(t, path)
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if got, _ := os.ReadFile(backups[0]); string(got) != content {
		t.Errorf("backup = %q", got)
	}
	if got, _ := os.ReadFile(path); string(got) != content {
		t.Errorf("original overwritten on load: %q", got)
	}

	// a later save writes a valid, 0644 file
	if !cm.Save(&Config{}) {
		t.Fatal("save failed")
	}
	if _, ok := loadConfigFile(path); !ok {
		t.Error("saved config not valid")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v", st.Mode().Perm())
		}
	}
	for _, n := range listDir(t, filepath.Dir(path)) {
		if len(n) > 5 && n[:1] == "." {
			t.Errorf("temp file left: %s", n)
		}
	}
}

func TestConfigManagerValidConfigNotBackedUp(t *testing.T) {
	path := setupConfigDir(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"dnsOverride": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cm := NewConfigManager()
	if !cm.GetDNSOverride() {
		t.Error("valid config not loaded")
	}
	if b := corruptBackups(t, path); len(b) != 0 {
		t.Errorf("unexpected backups %v", b)
	}
}

func TestConfigManagerMissingFileNoBackup(t *testing.T) {
	path := setupConfigDir(t)
	_ = NewConfigManager()
	if b := corruptBackups(t, path); len(b) != 0 {
		t.Errorf("unexpected backups %v", b)
	}
}
