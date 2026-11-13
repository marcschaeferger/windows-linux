package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newTestManager(t *testing.T) (*AccountManager, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "accounts.json")
	return loadAccountManager(path), path
}

func corruptBackups(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Covers #8/#21: readers race with every kind of writer; before the RWMutex
// this was a concurrent map iteration/write (fatal error or -race report).
func TestAccountManagerConcurrentReadWrite(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < 4; i++ {
		if err := m.AddAccount(Account{UserID: fmt.Sprintf("u%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = m.SetActiveUser("u0")

	var wg sync.WaitGroup
	const iters = 40
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				f(i)
			}
		}()
	}
	// writers
	run(func(i int) { _ = m.AddAccount(Account{UserID: fmt.Sprintf("u%d", i%6), Email: "e"}) })
	run(func(i int) { _ = m.UpdateAccountUserInfo("u0", "name", fmt.Sprint(i)) })
	run(func(i int) {
		_ = m.SetServerSettings("u0", fmt.Sprintf("o%d", i%3), &ClientSettings{})
	})
	run(func(i int) { _ = m.SetExitNode("u0", fmt.Sprintf("o%d", i%3), i+1) })
	run(func(i int) { _ = m.RemoveAccount(fmt.Sprintf("u%d", 4+i%2)) })
	run(func(i int) { _ = m.SetActiveUser("u0") })
	// readers
	for r := 0; r < 4; r++ {
		run(func(i int) {
			for _, a := range m.Accounts() {
				for range a.OrgSettings {
				}
			}
			_, _ = m.Account("u0")
			_ = m.ActiveUserID()
			_ = m.HasAccounts()
			if a, err := m.ActiveAccount(); err == nil {
				for range a.OrgSettings {
				}
			}
			_ = m.GetExitNode("u0", "o1")
			_ = m.GetServerSettings("u0", "o1")
		})
	}
	wg.Wait()
}

// Covers #8/#21: returned copies must not alias the manager's OrgSettings map.
func TestAccountCopiesDoNotAlias(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.AddAccount(Account{UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetExitNode("u", "org", 7); err != nil {
		t.Fatal(err)
	}
	if err := m.SetActiveUser("u"); err != nil {
		t.Fatal(err)
	}

	mutate := func(a *Account) {
		a.OrgSettings["org"] = OrgSettings{ExitNodeResourceID: 99}
		a.OrgSettings["injected"] = OrgSettings{ExitNodeResourceID: 1}
	}
	acc, ok := m.Account("u")
	if !ok {
		t.Fatal("missing account")
	}
	mutate(&acc)
	all := m.Accounts()
	if len(all) != 1 {
		t.Fatalf("accounts = %d", len(all))
	}
	mutate(&all[0])
	active, err := m.ActiveAccount()
	if err != nil {
		t.Fatal(err)
	}
	mutate(active)

	if got := m.GetExitNode("u", "org"); got != 7 {
		t.Errorf("exit node changed through a copy: %d", got)
	}
	fresh, _ := m.Account("u")
	if _, ok := fresh.OrgSettings["injected"]; ok || len(fresh.OrgSettings) != 1 {
		t.Errorf("manager map mutated: %+v", fresh.OrgSettings)
	}
}

func TestAccountReadersEmptyAndMissing(t *testing.T) {
	m, _ := newTestManager(t)
	if m.HasAccounts() || m.ActiveUserID() != "" || len(m.Accounts()) != 0 {
		t.Error("expected empty manager")
	}
	if _, ok := m.Account("x"); ok {
		t.Error("unexpected account")
	}
	if _, err := m.ActiveAccount(); err == nil {
		t.Error("expected error without active account")
	}
}

// Covers #51/#56: save/load round trip keeps OrgSettings and activeUserId, with
// the same JSON keys as before, and accounts.json is 0600.
func TestAccountsRoundTripAndFormat(t *testing.T) {
	m, path := newTestManager(t)
	if err := m.AddAccount(Account{UserID: "u1", Email: "a@b", OrgID: "o"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetExitNode("u1", "o", 5); err != nil {
		t.Fatal(err)
	}
	if err := m.SetActiveUser("u1"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["activeUserId"]; !ok {
		t.Errorf("missing activeUserId key: %s", raw)
	}
	if _, ok := top["accounts"]; !ok {
		t.Errorf("missing accounts key: %s", raw)
	}
	if !strings.Contains(string(raw), `"orgSettings"`) || !strings.Contains(string(raw), `"exitNodeResourceId": 5`) {
		t.Errorf("org settings keys changed: %s", raw)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", st.Mode().Perm())
		}
	}
	for _, n := range listDir(t, filepath.Dir(path)) {
		if strings.Contains(n, ".tmp-") {
			t.Errorf("temp file left: %s", n)
		}
	}

	m2 := loadAccountManager(path)
	if m2.ActiveUserID() != "u1" || m2.GetExitNode("u1", "o") != 5 {
		t.Errorf("round trip lost state: active=%q exit=%d", m2.ActiveUserID(), m2.GetExitNode("u1", "o"))
	}
	if a, ok := m2.Account("u1"); !ok || a.Email != "a@b" || a.OrgID != "o" {
		t.Errorf("account = %+v", a)
	}
}

// Covers #51/#56: reading a file in the old format (fixed JSON keys).
func TestAccountsLoadsExistingFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	src := `{"activeUserId":"u","accounts":{"u":{"userId":"u","email":"e","orgId":"o","username":"n","name":"N","hostname":"h","orgSettings":{"o":{"exitNodeResourceId":3}}}}}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	m := loadAccountManager(path)
	if m.ActiveUserID() != "u" || m.GetExitNode("u", "o") != 3 {
		t.Errorf("not loaded: %q %d", m.ActiveUserID(), m.GetExitNode("u", "o"))
	}
	if len(corruptBackups(t, path)) != 0 {
		t.Error("valid file must not be backed up")
	}
}

// Covers #51/#56: corrupt or empty accounts.json gives an empty manager and a
// backup with the original bytes; the original stays until the next save.
func TestCorruptAccountsFile(t *testing.T) {
	cases := map[string]string{
		"truncated": `{"activeUserId":"u","accounts":{"u":{"userId":"u","em`,
		"empty":     ``,
		"garbage":   `not json at all`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "accounts.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			m := loadAccountManager(path)
			if m.HasAccounts() || m.ActiveUserID() != "" {
				t.Fatal("expected empty manager")
			}
			backups := corruptBackups(t, path)
			if len(backups) != 1 {
				t.Fatalf("backups = %v", backups)
			}
			if got, _ := os.ReadFile(backups[0]); string(got) != content {
				t.Errorf("backup = %q, want %q", got, content)
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Errorf("original overwritten on load: %q", got)
			}
			if err := m.AddAccount(Account{UserID: "new"}); err != nil {
				t.Fatal(err)
			}
			m2 := loadAccountManager(path)
			if _, ok := m2.Account("new"); !ok {
				t.Error("saved file not valid/loadable")
			}
			if len(corruptBackups(t, path)) != 1 {
				t.Error("valid file after save must not add backups")
			}
		})
	}
}

// Covers #51/#56: a missing file creates an empty valid file.
func TestMissingAccountsFileCreated(t *testing.T) {
	m, path := newTestManager(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	var f accountsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("invalid json %q: %v", raw, err)
	}
	if m.HasAccounts() || len(corruptBackups(t, path)) != 0 {
		t.Error("expected empty manager and no backups")
	}
}

// Covers #51/#56: a failed save returns the error and keeps the old file.
func TestAccountsSaveFailureKeepsOldFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission enforcement")
	}
	m, path := newTestManager(t)
	if err := m.AddAccount(Account{UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := m.AddAccount(Account{UserID: "v"}); err == nil {
		t.Fatal("expected save error")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("old file changed on failed save")
	}
}

// Covers the no-op writes: unchanged values must not touch accounts.json
// (the tray calls these on every open). The file is replaced by a sentinel;
// a rewrite would overwrite it.
func TestAccountSettersSkipUnchangedWrites(t *testing.T) {
	const sentinel = "SENTINEL-NOT-JSON"
	setup := func(t *testing.T) (*AccountManager, string) {
		m, path := newTestManager(t)
		if err := m.AddAccount(Account{UserID: "u", Username: "un", Name: "nm", OrgID: "o1"}); err != nil {
			t.Fatal(err)
		}
		if err := m.AddAccount(Account{UserID: "v"}); err != nil {
			t.Fatal(err)
		}
		if err := m.SetActiveUser("u"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		return m, path
	}
	untouched := func(t *testing.T, path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != sentinel {
			t.Errorf("accounts.json was rewritten: %q", data)
		}
	}
	rewritten := func(t *testing.T, path string) accountsFile {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f accountsFile
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("accounts.json not rewritten as JSON: %q", data)
		}
		return f
	}

	t.Run("UpdateAccountUserInfo", func(t *testing.T) {
		m, path := setup(t)
		if err := m.UpdateAccountUserInfo("u", "un", "nm"); err != nil {
			t.Fatal(err)
		}
		untouched(t, path)
		if err := m.UpdateAccountUserInfo("u", "un2", "nm"); err != nil {
			t.Fatal(err)
		}
		if got := rewritten(t, path).Accounts["u"].Username; got != "un2" {
			t.Errorf("username = %q", got)
		}
	})
	t.Run("UpdateAccountUserInfo name only", func(t *testing.T) {
		m, path := setup(t)
		if err := m.UpdateAccountUserInfo("u", "un", "nm2"); err != nil {
			t.Fatal(err)
		}
		if got := rewritten(t, path).Accounts["u"].Name; got != "nm2" {
			t.Errorf("name = %q", got)
		}
	})
	t.Run("SetUserOrganization", func(t *testing.T) {
		m, path := setup(t)
		if err := m.SetUserOrganization("u", "o1"); err != nil {
			t.Fatal(err)
		}
		untouched(t, path)
		if err := m.SetUserOrganization("u", "o2"); err != nil {
			t.Fatal(err)
		}
		if got := rewritten(t, path).Accounts["u"].OrgID; got != "o2" {
			t.Errorf("org = %q", got)
		}
	})
	t.Run("SetActiveUser", func(t *testing.T) {
		m, path := setup(t)
		if err := m.SetActiveUser("u"); err != nil {
			t.Fatal(err)
		}
		untouched(t, path)
		if err := m.SetActiveUser("v"); err != nil {
			t.Fatal(err)
		}
		if got := rewritten(t, path).ActiveUserID; got != "v" {
			t.Errorf("active = %q", got)
		}
	})
	t.Run("unknown account still errors", func(t *testing.T) {
		m, path := setup(t)
		if m.UpdateAccountUserInfo("zz", "", "") == nil || m.SetUserOrganization("zz", "") == nil || m.SetActiveUser("zz") == nil {
			t.Error("unknown account did not return an error")
		}
		untouched(t, path)
	})
}

// An accounts file that exists but can't be read is not overwritten by the
// next save, which would drop every account in it.
func TestUnreadableAccountsFileIsNotOverwritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	path := filepath.Join(t.TempDir(), AccountsFileName)
	orig := []byte(`{"activeUserId":"u1","accounts":{"u1":{"userId":"u1"}}}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	m := loadAccountManager(path)
	if err := m.AddAccount(Account{UserID: "u2"}); err == nil {
		t.Fatal("AddAccount saved over an unreadable accounts file")
	}
	_ = os.Chmod(path, 0o600)
	if b, _ := os.ReadFile(path); string(b) != string(orig) {
		t.Fatalf("accounts file changed: %s", b)
	}
}
