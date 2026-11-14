# Architecture

## One binary, several roles

The same executable runs in different roles depending on its arguments.

| Role | Windows (`Pangolin.exe`) | Linux (`pangolin-client`) |
| --- | --- | --- |
| Privileged manager | `/managerservice`, Windows service `PangolinManager`, LocalSystem | `--daemon`, systemd `pangolin-client.service` (or OpenRC), root |
| Tunnel | `/tunnelservice <cfg>`, its own Windows service `PangolinTunnel$olm` | `--tunnel <cfg>`, child process of the daemon |
| UI | `/ui <r> <w> <ev>`, started *by the service* in the user's session with inherited pipe handles | no args, started *by the user* (menu, autostart); attaches to the daemon's socket |
| UI at login | service launches it at WTS logon if the setting is on | `--autostart` from `/etc/xdg/autostart`; exits unless open-at-login / connect-at-start |
| Installer helper | `/installmanagerservice` (after UAC) | none (packages install the service) |
| Toast click | COM activation (`ui.IsToastActivation`) | n/a |

Entry points: `main.go` (shared `runUI`), `main_windows.go`, `main_linux.go`.

The key inversion: **on Windows the service launches the UI** (it has
SeTcbPrivilege and uses `WTSQueryUserToken` + `CreateProcessAsUser`). **On Linux
a root daemon can't launch a GUI into a user's session**, so the UI starts as
the user and connects to the daemon. Both end up with the same three pipes and
the same gob IPC protocol, so everything above the transport is shared.

```
            Windows                                   Linux
  ┌──────────────────────────┐            ┌──────────────────────────────┐
  │ PangolinManager service  │            │ pangolin-client --daemon     │
  │ (LocalSystem)            │            │ (root, systemd/OpenRC)       │
  │  ├ IPC server (gob)      │            │  ├ IPC server (gob, same)    │
  │  ├ secret store (DPAPI)  │            │  ├ secret store (root files) │
  │  ├ updater (MSI)         │            │  ├ updater (apt/dnf/pacman)  │
  │  └ named pipes:          │            │  └ /run/pangolin-client/     │
  │     ui-launch, ui-action,│            │       manager.sock           │
  │     cli-secrets          │            │     ('U','A','S','Q' bytes)  │
  └──────┬───────────────────┘            └──────┬───────────────────────┘
         │ CreateProcessAsUser +                 │ UI dials socket,
         │ 3 inherited pipe handles              │ gets 3 pipe FDs via SCM_RIGHTS
  ┌──────▼───────────────────┐            ┌──────▼───────────────────────┐
  │ Pangolin.exe /ui (user)  │            │ pangolin-client (user)       │
  │  Wails + WebView2        │            │  Wails + WebKitGTK (XWayland)│
  │  tray: Wails systray     │            │  tray: own SNI / GNOME ext.  │
  └──────────────────────────┘            └──────────────────────────────┘
  Tunnel: Windows service per tunnel      Tunnel: child process per tunnel
  OLM API: \\.\pipe\pangolin-olm          OLM API: /run/pangolin-client/olm.sock
```

## Package map

| Package | Role | Per-OS files |
| --- | --- | --- |
| `main` | role dispatch, logging (`logger.go`; the daemon / manager service rotates the shared log by size, tunnels and UIs only follow) | `main_windows.go`, `main_linux.go` |
| `api` | Pangolin server HTTP client (session cookie, 401 → session expired) | – |
| `auth` | login flow, accounts, OLM credential lifecycle, device posture IPC | – |
| `config` | `pangolin.json` user + machine config, accounts, paths, server client-settings | `paths_*.go` |
| `secrets` | UI-side `SecretManager`, talks to the manager over IPC | `migrate_*.go` (Windows Credential Manager migration; no-op on Linux) |
| `managers` | the privileged side: IPC server/client, service/daemon, tunnel install, updates state, CLI install, CLI secrets, polkit | many `_windows`/`_linux` |
| `managers/secretstore` | on-disk secrets per OS user + Pangolin user | `filestore_*.go`, `dpapi_windows.go` |
| `tunnel` | tunnel config, `Manager` (UI side: connect, status polling over OLM API), `RunTunnelService` (tunnel side, wraps olm) | `run_*.go`, `olmsocket_*.go` |
| `fingerprint` | device posture + platform fingerprint (cached, refreshed every 3h by the manager) | `fingerprint_*.go` |
| `updater` | manifest check, signify verify, download, stage, install | `*_windows.go`, `*_linux.go` |
| `ui` | Wails app, tray popup, windows, bound services, menu model | many; see below |
| `ui/sni` | Linux StatusNotifierItem + dbusmenu over godbus | Linux only |
| `version` | `Number` (ldflags), user agent `pangolin-<goos>-<ver>`, OS name | `os_*.go`, `official_windows.go` |
| `elevate` | Windows UAC ShellExecute / "doas" as SYSTEM | Windows only |
| `services` | `StartedAtBoot()` (boot jitter for update checks) | `boot_*.go` (Linux always false) |
| `icons` | embedded `.ico` files (orange/gray etc.) | – |

`ui/` per-OS split: `app_*.go` (Wails options, `beforeAppStart`), `tray_*.go`
(icon + popup hooks), `dialogs_*.go` (message boxes: Win32 vs GTK cgo),
`darkmode_*.go`, `toastactivation_windows.go`, `webview2_windows.go`, and
Linux-only `gtk_linux.go`, `shell_linux.go`, `wayland_linux.go`,
`plasmaclick_linux.go`, `gtk3_required_linux.go`.

## IPC protocol (UI ↔ manager)

Defined in `managers/ipc_client.go` (client + type enums) and
`managers/ipc_server.go` (server). Three unidirectional pipes:

1. UI → manager: method calls, gob-encoded `MethodType` followed by args.
2. manager → UI: return values (gob), errors as strings (`errToString`, "" = nil).
3. manager → UI events: `NotificationType` + payload, written to every
   attached UI (`notifyAll`). One-second write deadline.

`MethodType` and `NotificationType` are `iota` enums. **Append only**: UI and
manager of different versions can briefly talk during an update. Calls are
serialized by `rpcMutex` on the client.

Methods today: Quit, UpdateState, Update, StartTunnel, StopTunnel,
StopAllTunnels, IsCLIInstalled, InstallCLI, Get/Save/DeleteUserSecrets,
GetDevicePosture, CheckForUpdates, SetAlwaysOn, AlwaysOnEnabled, UpdateVersion,
AutoDownloadUpdatesTurnedOn.

Notifications: ManagerStopping, UpdateFound, UpdateProgress, TunnelStateChange,
UIAction (carries a session ID; only the UI whose `currentSessionID()` matches
acts; on Linux the "session" is the UID).

`ManagerService` (one per attached UI) carries `clientUserKey` (Windows SID or
Linux UID string), `session` (Windows session ID or UID), and `elevatedToken`
(Windows: elevated token or 0; Linux: `uintptr` 1 if root or in
sudo/wheel/admin, else 0). `notifyAll(..., adminOnly)` skips non-admins.

Side channels outside the gob IPC:
- Windows named pipes: `\\.\pipe\pangolin-manager-ui-launch` (u32 session →
  0 launching / 1 already running / 2 no session), `...-ui-action` (session,
  action → 0/1), `...-cli-secrets` (CLI companion mode). All allow Everyone;
  launch and action requests must name the caller's own session.
- Linux socket `/run/pangolin-client/manager.sock` (0666, SO_PEERCRED checks):
  first byte `U` attach, `A` UI action, `S` CLI secrets, `Q` update status.
  See [linux.md](linux.md).

## Tunnel flow

1. UI `tunnel.Manager.Connect()` (`tunnel/manager.go`) builds a `tunnel.Config`
   from the active account, OLM credentials (via secrets IPC), the merged
   settings for the org, and exit node choice.
2. It calls IPC `StartTunnel(config)`. The manager attaches the cached device
   posture (`fingerprint.CachedDevicePosture`, with username swapped for the
   connecting user via `postureForClient`) and calls
   `tunnel.StartTunnel` → `InstallTunnel(configJSON)`.
3. Windows: refuses tunnel names outside `[A-Za-z0-9_-]{1,32}`, writes
   `Pangolin\Tunnels\<name>.json` under LocalSystem's local app data
   (`C:\Windows\System32\config\systemprofile\AppData\Local`, not ProgramData,
   where any user can create folders) with a SYSTEM/Administrators-only DACL (it
   holds the olm secret and session token), then creates/starts service
   `PangolinTunnel$olm` running `/tunnelservice <cfg>`. Linux: writes `/run/pangolin-client/tunnels/<name>.json`
   (0600) and starts `pangolin-client --tunnel <path>` with `Pdeathsig=SIGTERM`.
4. The tunnel process runs olm (`tunnel/build.go`), which serves its HTTP API
   on the OLM socket. The UI polls status over that socket
   (`StartStatusPolling`, `GetOLMStatus`), switches org, selects gateway.
   On Linux the socket is in `/run/pangolin-client/olm/`, which root owns and
   only the tunnel's owner may enter (POSIX ACL set by the daemon before each
   start, `tunnel.GrantOLMSocketAccess`), so the owner's UI and CLI reach it
   and other users don't. The Windows pipe still allows Everyone: olm sets
   its DACL, and restricting it needs an olm option.
5. Stop: IPC `StopTunnel` → `UninstallTunnel` (Linux: SIGTERM, 30s, then
   kill; olm restores DNS and routes on SIGTERM).

Only one tunnel name is used in practice: `tunnel.DefaultTunnelName = "olm"`.

Quitting from the tray (`quit()` in `ui/actions.go`) clears Always-On, calls
IPC `StopAllTunnels`, then exits; it does **not** use the IPC `Quit` method
(legacy, see windows.md). If the UI goes away any other way (crash, killed,
update restart), the tunnel keeps running in the manager and the next UI
adopts it (`adoptRunningTunnel`, commit `5ea5fa9`). Signing out of the
Pangolin account stops it.

There is one tunnel per computer (the manager pins the name to
`tunnel.DefaultTunnelName`), owned by the user who started it
(`activeTunnels`: user key, session, `managers/tunnel_owner.go`). While it
runs, other users' `StartTunnel` is refused with "connected for another
user", their `StopTunnel`/`StopAllTunnels` leave it alone, and state
notifications go only to the owner's UIs. A UI adopts a running tunnel only
if the manager says it isn't another user's (`MayAdoptTunnelMethodType`,
checked in `AdoptRunningTunnel`); on Linux other users can't reach its OLM
socket either. A tunnel
whose process is gone has no owner. On Windows the owner is also stored in
the tunnel service's registry key, so a tunnel that outlives a manager
restart keeps it (on Linux tunnels stop with the daemon). Signing out stops
the tunnels that session started (`stopSessionTunnels`): Windows on
`WTS_SESSION_LOGOFF`, Linux from `watchTunnelOwners` (logind, every 5s).

Authorization (`authorizeTunnelUse`, `authz_*.go`): Windows allows every UI
the service started (interactive session users only). Linux allows root and
users with a session on a local seat (active or switched away); anyone else
(SSH-only users, system accounts, also sudo/wheel members) needs the polkit
action `net.pangolin.client.connect` (`auth_admin_keep`). Without logind,
regular accounts (UID_MIN and up) are allowed. It gates `StartTunnel` and
`GetDevicePosture` (which carries root-only data such as the DMI serial).
`Update` (`UpdateCheckedMethodType`, which replies with a refusal; the old
`UpdateMethodType` is checked the same way and gets no reply) is checked
before anything happens. When nobody else is affected (no other user's
tunnel runs, nobody else signed in; Linux: has a UI attached), admins may,
and so may anyone else (Linux: a staged update only). When others are
affected, only an elevated admin token (Windows) or root (Linux) may. The
manager then stops every tunnel itself, sending Stopping first, before the
install starts. The staged installs at UI quit and launch also wait while
another session's tunnel runs. `Quit` needs an elevated token on Windows and
root on Linux.

## UI process

`ui.Run` (`ui/app.go`) creates the Wails app with bound services
(`ui/services.go`: Menu, Preferences, Status, Logs, Login, Accounts,
Onboarding, App, Update, CLI, notifier), sets up the tray, registers IPC
callbacks (`ui/ipcevents.go`), then on start: shows startup warning, refreshes
tray, checks for updates, adopts a running tunnel, then onboarding or
Always-On resume or auto-connect.

Windows are routes of one page: `#/tray`, `#/preferences`, `#/onboarding`,
`#/cli`, `#/update` (`ui/frontend/src/main.tsx`). The tray popup is a frameless
transparent window that renders `MenuState` (built in Go: `ui/menu.go`
`buildMenuState` from `ui/state.go` `collectMenuInputs`) and posts clicks back
as item IDs (`invokeMenuItem` in `ui/actions.go`). Go pushes state with Wails
events (`menu:state`, `status:update`, `logs:append`, `update:state`, ...;
names in `ui/state.go`).

The same menu model and frontend serve Windows, KDE, XFCE and GNOME. The only
per-platform menu duplication is the one-item right-click "Quit Pangolin" menu
(`ui/tray_windows.go`, `ui/tray_linux.go`, GNOME `extension.js`).

## Configuration layers

`pangolin.json` exists at two levels with the same schema (`config.Config`):

| | Windows | Linux |
| --- | --- | --- |
| Machine (`SystemConfig`, adds `logLevel`) | `%ProgramData%\Pangolin\pangolin.json` | `/etc/pangolin-client/pangolin.json` |
| User | `%LOCALAPPDATA%\Pangolin\pangolin.json` | `~/.config/pangolin-client/pangolin.json` |

Merge: machine first, user overrides (`mergeConfig`). Some keys are machine-only
in effect because they're read via `LoadSystemConfig()` directly:
`autoUpdateChecksEnabled`, `checkForUpdatesButtonEnabled`,
`updateCheckIntervalSeconds` (min 3600), `logLevel`. Per-org overrides live in
`orgs.<orgId>`; precedence when connecting is server client-settings (`GET
/org/:orgId/my-client-settings`) > org section > global
(`config/clientsettings.go` `EffectiveSettings`). The server's settings are
re-fetched on every connect (`tunnel.Manager.buildConfig`); by the Preferences
window when it opens or its Preferences tab is selected
(`PreferencesService.Refresh`, whose return value is used instead of an event
so it can't be missed while the window loads); and every 10s while that tab is
showing (`ui/settings.go` `pollServerSettings`, told about the selected tab by
`PreferencesService.SetSettingsActive`, pushing changes with `prefs:settings`).
`userSettingsDisabled` locks the
Preferences form.

The privileged process must read a user's config **for that user** (it can't use
its own env): Windows reads the session token's `LOCALAPPDATA`
(`localAppDataForSession`); Linux reads `~user/.config/pangolin-client/pangolin.json`
safely with `O_NOFOLLOW`, regular file owned by that UID, 1 MiB cap
(`managers/autoupdate_linux.go` `readUserConfig`).

## Other paths

| | Windows | Linux |
| --- | --- | --- |
| Logs (manager, tunnels) | `%ProgramData%\Pangolin\logs\pangolin.log` (SYSTEM+Admins ACL) | `/var/log/pangolin-client/pangolin.log` (root only, 0640 in a 0750 dir) |
| Logs (UI) | `%LOCALAPPDATA%\Pangolin\logs\pangolin.log` | `~/.local/state/pangolin-client/logs/pangolin.log` |
| Secrets | `%ProgramData%\Pangolin\Credentials\<SID>\<userId>.secrets.dpapi` (SYSTEM+Admins ACL, DPAPI) | `/var/lib/pangolin-client/credentials/<uid>/<userId>.secrets.json` (root-only) |
| Staged update | `%ProgramData%\Pangolin\updates\pending.msi` + `pending.sig` | `/var/lib/pangolin-client/updates/pending.<ext>` + `pending.sig` |
| Relaunch-after-update list | restart-ui flag file (updater) | `/var/lib/pangolin-client/restart-ui-after-update` (UIDs) |
| Runtime | named pipes | `/run/pangolin-client/` (manager.sock, olm.sock, tunnels/) |
| WebView data | `%LOCALAPPDATA%\Pangolin\WebView2` | WebKitGTK default |

The Logs tab shows the manager/daemon log (where tunnel problems show up), not
the UI log. Users can't read that file, so the tab reads it through the daemon
(`ReadLog` IPC). Every log line is redacted (`logging.Redact`) before it is
written, and again when the tab shows, copies or exports it.
