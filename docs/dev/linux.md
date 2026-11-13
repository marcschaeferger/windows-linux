# Linux

Target: one codebase with Windows, supporting KDE, GNOME and XFCE (owner's
goal, 2026-10-30), shipped as deb, rpm, Arch package and a tar.gz. UI and tray
details are in [linux-tray-popup.md](linux-tray-popup.md); updates in
[updates.md](updates.md); packaging in [linux-packaging.md](linux-packaging.md).

## Build requirements

- cgo, `gtk3` and `webkit2gtk-4.1` dev packages (Ubuntu: `libgtk-3-dev
  libwebkit2gtk-4.1-dev`; Arch: `gtk3 webkit2gtk-4.1`). Owner's Arch KDE laptop
  already has them.
- Build tags `gtk3 production`. `ui/gtk3_required_linux.go` references an
  undefined symbol under `!gtk3` so a tagless build fails loudly.
- Wails v3.0.0 + `@wailsio/runtime` 3.0.0 (must match). Wails 3.1.0 exists and
  still has GTK3; we stayed on 3.0.0 because that's what was validated.
- Renderer is WebKitGTK (not Chromium), so small rendering differences vs
  WebView2 are possible. RHEL 9 and rebuilds lack WebKitGTK 4.1, so they're unsupported.
- `make build-linux` → `build/pangolin-client`. `go build` without the
  Makefile's ldflags reports version `dev`.

## Process model

```
systemd ── pangolin-client --daemon (root)
              ├─ listens /run/pangolin-client/manager.sock (0666)
              ├─ pangolin-client --tunnel /run/pangolin-client/tunnels/olm.json  (child, Pdeathsig)
              │     └─ olm: TUN, routes, DNS; API on /run/pangolin-client/olm.sock
              └─ systemd-run units: pangolin-client-update-<ts> (package installs),
                 pangolin-client-ui-<ts> (UI relaunch in a user's systemd --user)

user session ── pangolin-client           (UI, GDK_BACKEND=x11, attaches to the daemon)
            └── pangolin-client --autostart (from /etc/xdg/autostart; exits unless opted in)
```

`--daemon` refuses to run unless euid 0. It creates `/run/pangolin-client`
(0755), removes a stale socket, listens, then: `loadStagedUpdate()`, tries to
install a staged update before anything else (UIs that connect meanwhile get
`attachUpdating`), relaunches UIs recorded before an update, and starts the
update checker. On SIGTERM/SIGINT: refuse new attaches, notify UIs
(ManagerStopping), stop all tunnel processes (parallel), exit. The systemd unit
uses `KillMode=mixed` + `TimeoutStopSec=45` so only the daemon gets SIGTERM and
can stop tunnels cleanly (restoring DNS/routes).

## Daemon socket protocol (`managers/daemon_linux.go`, `managers/ui_launcher_linux.go`)

Every connection: 10s deadline, `SO_PEERCRED` gives uid + pid, then one request byte.

| Byte | Request | Reply |
| --- | --- | --- |
| `U` | attach UI | 1 byte: `0` OK (+ 3 FDs via SCM_RIGHTS: reader, writer, events), `1` already running for this uid, `2` failed / daemon stopping, `3` update installing |
| `A` + u32 LE action | ask this uid's UI to do a `UIAction` (e.g. `UIActionShow`) | `0` delivered, `1` no UI |
| `S` + u32 LE uid + CLI secrets request | CLI companion mode | see `managers/cli_secrets.go`; uid must equal the caller's unless caller is root (CLI under sudo sends `SUDO_UID`) |
| `Q` | is an update installing? | `0` ready, `1` still installing |

One UI per UID (`attachedUIs`). A second launch gets `attachAlreadyRunning`,
and `main_linux.go` then sends `A UIActionShow` so the running UI shows a
window, then exits. The attach records a polkit subject (`pid,start-time,uid`)
for later authorization prompts.

If the socket can't be reached, the UI still starts but shows a warning with
the start command for this system's init (`daemonUnavailableWarning`: systemd,
OpenRC, or `sudo pangolin-client --daemon`). Sign-in can't persist without the
daemon.

### UI detach handling

When the UI's IPC connection closes (`IPCServerListen` returns `done`):
1. Remove from `attachedUIs`.
2. `installStagedUpdateAfterQuit`: if a staged update exists, Always-On is off,
   the daemon isn't stopping, wait 2s, check `loginctl show-user <uid>
   --property=State` is `active`/`online` (so sign-out/crash-at-logout doesn't
   count as quit), and no other UI is attached → install silently.
3. Otherwise `relaunchUIIfAlwaysOn`: if Always-On is set for the uid, relaunch
   after 1s via `systemd-run --user --machine=<user>@.host --collect
   --unit=pangolin-client-ui-<ts> <exe>`. More than 5 relaunches in a minute →
   give up and clear Always-On. A relaunch failure also clears it.

The UI's Quit clears Always-On first, so a quit doesn't trigger a relaunch.

## Admin rights

`tokenForUID` (`managers/identity_linux.go`): root, or a member of `sudo`,
`wheel` or `admin` → non-zero `elevatedToken`. Used for: installing an update
when nothing is staged, receiving admin-only notifications. Anyone may install
an already-staged update (equivalent to quitting).

CLI install (`managers/cli_installer_linux.go`, `managers/polkit_linux.go`):
admins install immediately; others are asked via polkit (`pkcheck
--allow-user-interaction`, action `net.pangolin.client.install-cli`,
`auth_admin_keep` = 5 min) using the UI process as subject. Policy file
`packaging/linux/net.pangolin.client.policy` must be installed (all packages
and `install-linux` do). pkcheck exit codes: 1 not authorized, 2 no
authentication agent in the session, 3 cancelled, 127 + "is not registered" =
policy missing. The install downloads `pangolin-cli_linux_<arch>` from the
CLI's latest GitHub release (`managers/cli_release.go`, shared with Windows),
checks it against the sha256 digest the GitHub release API publishes for the
asset (a release without one is refused), checks it's an ELF, and renames it into `/usr/local/bin/pangolin` (same place
`get-cli.sh` uses). Test with network: `PANGOLIN_TEST_CLI_DOWNLOAD=1 go test ./managers -run CLI`.

## Secrets (`managers/secretstore/filestore_linux.go`)

Root-only files `/var/lib/pangolin-client/credentials/<uid>/<pangolinUserId>.secrets.json`.
There's no DPAPI equivalent for a system service; protection is file
permissions (same as NetworkManager system connections). `ValidateUID`
rejects non-numeric keys (path traversal test exists). Per-UID separation is
tested. `deb purge` deletes `/var/lib/pangolin-client`.

## Device posture and fingerprint (`fingerprint/fingerprint_linux.go`)

Ported from `../cli/internal/fingerprint` on 2026-11-02, then deliberately
diverged (owner: "They should be separate on purpose", 2026-11-06). Gathered by
the root daemon, cached, refreshed every 3h.

- Reports: root filesystem on dm-crypt (follows LVM/device-mapper; the CLI
  instead says true if *any* crypt device exists), firewall, TPM (`/dev/tpm0` or
  `/dev/tpmrm0`), AppArmor, SELinux, model, serial (junk values like "To be
  filled by O.E.M." filtered, with a cached fallback), OS version (`NAME VERSION`
  from os-release), arch as GOARCH (`amd64`, CLI uses `uname -m`).
- `platformFingerprint` hash: `key=value|…` of os, arch, cpu, DMI sys_vendor,
  product_name, product_sku, board_vendor, board_name, product_serial. The
  only per-unit identifier is `product_serial`, so machines of the same model
  with junk/empty serials (whitebox desktops, cloned VMs) share a hash. A
  session on 2026-11-06 added `uuid=<product_uuid>` (root-only read) with an
  `isUsefulUUID` filter for empty/all-0/all-F/placeholder UUIDs, but **that
  change is not in the code** (never committed; as of `f147784` there's no
  `product_uuid` in `fingerprint/`). Adding it changes every existing Linux
  hash once. Windows has the same gap (fix: `Win32_ComputerSystemProduct` UUID).
- Username: gathered as root, then `postureForClient` swaps in the connecting
  user's name before `StartTunnel` sends it.

## CLI companion mode

The Pangolin CLI (`../cli`) can sign in using the desktop app's session. On
Linux (built 2026-11-02, committed in `../cli` by the owner on 2026-11-05):
- Daemon answers `S` on `manager.sock`; framing after the uid is identical to
  the Windows pipe (shared `managers/cli_secrets.go`: u32 length + Pangolin
  user id → u32 status, u32 length, JSON payload or error).
- CLI side: provider shared between windows/linux, data dir
  `~/.config/pangolin-client` (or the sudo user's home), override setting
  `companion_app_data_dirs.linux`, agent name "Pangolin Linux".
- **Deliberate difference from Windows**: on Linux, companion mode only turns on
  when the desktop client is installed (`pangolin-client` on PATH or the daemon
  socket exists), so standalone CLI users (servers) aren't locked out.
- The CLI checks its own `/var/run/olm.sock` before the app's
  `/run/pangolin-client/olm.sock`. `up` refuses while the app's tunnel runs.
- One-time CLI notice for Linux users without the desktop client, linking
  https://pangolin.net/downloads (`../cli/internal/notice/notices.go`).
- Tested in containers with a static daemon harness; `pangolin up` through
  companion mode against a real server is untested.

## Names and paths

`pangolin-client` everywhere on Linux because the CLI already owns
`~/.config/pangolin`, `/etc/pangolin`, `/var/log/pangolin` (with a conflicting
`accounts.json`). Binary `/usr/bin/pangolin-client` (packages) or
`/usr/local/bin` (tar.gz, `make install-linux`). X11 `WM_CLASS` class is
`Pangolin-client` (desktop file `StartupWMClass=pangolin-client`). Desktop
entry `pangolin-client.desktop`; icon `pangolin-client` (256px PNG). User agent
`pangolin-linux-<version>` (server doesn't parse it). OS display name "Linux".

## Opening URLs

`openURL` (`ui/actions.go`) uses `github.com/pkg/browser`, which only tries
`xdg-open`, `x-www-browser`, `www-browser`. A fresh Arch + XFCE has none
(`xdg-utils` isn't in the `xfce4` group), so "Open browser" silently fails
(logged as "failed to open"). Proposed but **not done** (2026-11-05): add
`xdg-utils` to package deps and fall back to `gio open` / `exo-open`. Check
before assuming it's fixed.

## Things that are stubs or missing on Linux

- `services.StartedAtBoot()` always false → no 2–5 min boot jitter before the
  first update check.
- No tunnel stop on user logout (Windows `stopSessionTunnels`).
- `errNotImplemented` in `managers/install_linux.go` is now unused.
- Native Wayland UI: not supported (issue #84); see the tray doc.
