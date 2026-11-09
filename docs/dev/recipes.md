# Recipes: how to make common changes

Each recipe lists every place that has to change. After any of them: the
baseline checks in [testing.md](testing.md#baseline-checks-always) (both OSes).

## Add a user/machine setting (`pangolin.json`)

1. `config/config.go`: add a pointer field to `Config` with a `json:"…,omitempty"`
   tag; add a default constant if needed; add it to **`mergeConfig`** and
   **`copyConfig`** (both list fields by hand; forgetting either drops it
   silently). Add getter/setter on `ConfigManager` like the others.
2. If it's per-org-overridable, also `config/clientsettings.go` (`OrgConfig`,
   `mergeOrgConfig`, `TunnelSettings`, `EffectiveSettings`), and if the server
   can manage it, `ClientSettings` + `managedFields` in `ui/settings.go`.
3. If the privileged process must read it for a user, read it through the
   merged-config helpers that take the user's dir (`…In(userDataDir)`,
   `AutoDownloadUpdatesEnabledWithUserConfig`); never `UserDataDir()` from
   the service/daemon (that's SYSTEM's / root's).
4. Machine-only settings (read via `LoadSystemConfig()` only) are fine too;
   document them in public docs.
5. Preferences UI: `ui/settings.go` `Settings` struct (+ json tag),
   `currentSettings()`, `applySettings()`; then `make bindings` (regenerates
   `ui/frontend/src/bindings/.../ui/models.ts`); then
   `ui/frontend/src/prefs/PreferencesTab.tsx`; and `ui/frontend/src/mocks/bindings.ts`
   so `npm run mock` still works.
6. If the manager must react immediately (like `AutoDownloadUpdatesTurnedOn`),
   add an IPC method (next recipe).

## Add an IPC method (UI → manager)

1. `managers/ipc_client.go`: append a `MethodType` at the **end** of the iota
   list. Add `IPCClientFoo(...)`: lock `rpcMutex`, check `rpcEncoder != nil`
   (return `errIPCNotConnected`), encode method type + args, decode results +
   `rpcDecodeError()`.
2. `managers/ipc_server.go`: add `func (s *ManagerService) Foo(...)` and a
   `case FooMethodType:` in `ServeConn` that decodes args in the same order
   and encodes results (errors via `errToString`). On any encode/decode error
   `return` (closes the connection).
3. Authorization: use `s.elevatedToken` (admin), `s.clientUserKey` (SID/UID
   string), `s.session`. Don't trust IDs sent by the client for identity.
4. Types sent over gob must be gob-encodable; new struct fields are tolerated
   by gob, renamed/retyped ones aren't.
5. If it's per-OS, implement the body in `_windows.go`/`_linux.go` helpers.

Manager → UI event: append a `NotificationType`, add `IPCServerNotifyFoo`
using `notifyAll`, decode it in `InitializeIPCClient`'s switch, add a callback
registry like `IPCClientRegisterTunnelStateChange`, register in
`ui/ipcevents.go`.

## Add a method the frontend can call

1. Add a method on a bound service in `ui/services.go` (or a new service type
   registered in `ui.Run`'s `application.NewService` list; `RunInstallingUpdate`
   has its own short list).
2. `make bindings` (needs `wails3` v3.0.0: `go install
   github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0`, add `-tags gtk3` on
   Linux). If `wails3` isn't installed, hand-editing generated models in the
   generator's exact format has been done, but regenerate when possible.
   Binding call IDs are hashes of the fully-qualified Go name: renaming the
   module, package, type or method changes them.
3. Call it from the frontend via the generated module; add a mock in
   `ui/frontend/src/mocks/bindings.ts`.
4. Push state to windows with `app.Event.Emit("<area>:<name>", payload)` and
   listen with `useEvent` in the frontend (see `ui/state.go` event names).

## Add a tray menu item

1. `ui/menu.go`: add an ID constant, emit it in `buildMenuState` (or a submenu
   builder) using `item(...)`, `checkItem(...)`, etc. Inputs come from
   `collectMenuInputs()` in `ui/state.go`; add fields to `menuInputs` if needed.
2. `ui/actions.go` `invokeMenuItem`: handle the ID.
3. Call `publish()` when the inputs change so the popup updates.
4. No frontend change unless you need a new row kind (`MenuKind*` +
   `TrayMenu.tsx`). Same menu shows on Windows, KDE, XFCE, GNOME.
5. There are no menu unit tests yet (`ui/` only tests icon conversion and the
   Plasma click math); `buildMenuState` is pure, so a table test is easy to
   add. At least check the popup in `npm run mock` (`#/tray`).

## Add a window

1. Frontend: a component + a `case` in `ui/frontend/src/main.tsx` for a new
   hash route.
2. Go: create it with `newDialogWindow(name, title, route, height, onClose)`
   (`ui/windows.go`) or similar; apply `scaleWindowOptions` (Linux scaling);
   show with `focusWindow` (fresh X timestamp on Linux).
3. Make sure it works at 1.25/1.5 scale on Linux (`PANGOLIN_UI_SCALE=1.5`).

## Add something platform-specific

- Shared code calls a function; define it in both `foo_windows.go` and
  `foo_linux.go` with the same signature. If one side has nothing to do, make
  it a documented no-op or return a clear error (pattern: `checkWebView`,
  `migrateFromCredentialManager`, `prepareUpdateRestart`).
- Linux GTK/X11 calls go through cgo in `ui/gtk_linux.go` and must run on the
  main thread: wrap in `application.InvokeSync`/`InvokeAsync`.
- D-Bus: use `github.com/godbus/dbus/v5` (already a dependency; see `ui/sni`,
  `ui/shell_linux.go`, `ui/darkmode_linux.go`).
- Avoid new heavy deps; the Wayland client was written by hand on purpose.

## Add a daemon socket request (Linux)

`managers/daemon_linux.go`: new request byte constant, a `case` in
`handleDaemonConn` (you have `uid`, `pid` from SO_PEERCRED), client function in
`managers/ui_launcher_linux.go` using `dialDaemon()`. Keep replies a single
byte where possible. Check permissions against `uid` (root = 0 may act for
others only where that's intended, as in CLI secrets).

## Support a new GNOME Shell version

Add it to `shell-version` in
`packaging/linux/gnome-shell-extension/pangolin@pangolin.net/metadata.json`,
test in a container of that version (button appears, left click opens popup,
right click Quit, desktop click closes). Check `PanelMenu.Button` click
handling didn't change again (it did in GNOME 49: `Clutter.ClickGesture`).

## Change the update or package-name scheme

Touch all of: `updater/constants_*.go` (`candidatePattern`, paths),
`scripts/generate-*manifest.sh`, Makefile `package-linux` names, nfpm
`package-format` entries, `get-linux.sh` asset names, AUR PKGBUILD sources,
`.github/workflows/*.yml` globs, landing site release lookup, public docs.
Old clients in the field still look for the old names, so keep the old names
in the manifest until they've updated.

## Make a release

See BUILD_STEPS.md. Short version: tag `linux-X.Y.Z` / `windows-X.Y.Z`, wait
for the draft release, publish, owner uploads `<os>-latest.sig`. Don't tag,
publish or upload unless asked.
