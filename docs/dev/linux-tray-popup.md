# Linux tray icon and popup

The hardest part of the Linux port. The popup is a frameless, transparent,
always-on-top WebKitGTK window that must open beside the tray icon, scaled
correctly, take focus, and close on click-away, on KDE, GNOME and XFCE, on
X11 and Wayland sessions, at any scale, across monitors.

Fallback if this ever becomes unworkable: a native tray menu generated from
the same `MenuState` tree (never built).

## Why XWayland (`GDK_BACKEND=x11`)

Spike on KDE Plasma Wayland at 1.5x, Wails v3.0.0, 2026-10-30:
- Native Wayland: KWin ignores client window positioning and centres the
  popup; keep-above doesn't stick; KWin also adds a title bar to the frameless window.
- XWayland: exact placement, keep-above works, skip-taskbar works, focus-loss
  event fires.

So `beforeAppStart()` (`ui/app_linux.go`) sets `GDK_BACKEND=x11` unless the
user set it. Pure X11 (XFCE) works directly. GTK4 can't position windows at
all, hence GTK3 (`-tags gtk3`).

Native Wayland is tracked as **issue #84** (filed 2026-11-09, `enhancement`).
Options listed there: wlr-layer-shell on KDE so the app can place the popup;
let the GNOME extension draw/place the popup on GNOME; fall back to the native
tray menu elsewhere.

## Tray icon: our own StatusNotifierItem (`ui/sni`)

Wails v3.0.0's Linux tray was unusable for us: click coordinates private,
`setTooltip` stub, `openMenu` stub, `bounds()` empty, PNG-only icons (repo ships
`.ico`), no signal for a missing tray host. So `ui/sni` implements
`org.kde.StatusNotifierItem` + `com.canonical.dbusmenu` with godbus:
- `Activate(x, y)` reports the click → popup opens there.
- Tooltip, icon (ICO frames ≤64px converted to ARGB pixmaps), right-click menu
  with one item "Quit Pangolin".
- `SetPassive(true)` while our GNOME extension is active, so there's one icon.
- `HostAvailable()`: if after 5s there's no SNI host and no GNOME extension,
  Preferences opens so the app is reachable (`startLinuxTray` in `ui/tray_linux.go`).

Wails' tray isn't used on Linux. Wails also has no `WindowHide` event on Linux;
`afterTrayHide()` is called from the focus-lost handler instead.

## Coordinate units (read this before touching placement)

Three unit systems (comment at top of `ui/tray_linux.go`):
- **X pixels**: what the X server (XWayland) uses. SNI hosts report clicks in
  X pixels (except Plasma with mixed scales, below).
- **GTK units** = X pixels / `gtkScale()` (GTK's integer `GDK_SCALE`, usually 1).
  Windows are positioned and sized in GTK units.
- **CSS pixels**: the popup's layout (`placeTrayWindow` in
  `ui/trayplacement.go`, shared with Windows where the unit is DIPs). One CSS
  px = `uiScale()` GTK units. Web content is zoomed by `uiScale()` (Wails `Zoom`).

`uiScale()` (`ui/gtk_linux.go`) = GTK font DPI / 96, rounded to 0.25, clamped
1..4. Under XWayland on KDE with "apply scaling themselves" (kwinrc
`[Xwayland] Scale=1.5`, `Xft.dpi=144`), X apps are unscaled physical pixels and
only the DPI says how big things should be. When GTK scales by itself
(`GDK_SCALE=2`), it reports unscaled DPI, so uiScale stays 1.
`PANGOLIN_UI_SCALE=<0.5..4>` overrides it (useful for testing).

All windows (Preferences, setup, update, CLI) get `scaleWindowOptions`: sizes
multiplied by uiScale and `Zoom` set.

## Placement pipeline

1. Click arrives: SNI `Activate(x,y)` → `sniClickToX` (Plasma fix) → divide by
   gtkScale → `setTrayClick` (GTK units). GNOME extension `Activate(x, y,
   stageW, stageH)` → scaled by root window size / stage size → `setTrayClick`.
2. `toggleTrayPopup` → `trayClickPoint()` converts to CSS px (falls back to the
   pointer position, then bottom-right of the primary work area).
3. `trayWorkArea(cursor)` picks the monitor containing the click from
   `monitors()` (**live GDK query each time**) and returns its work area in CSS px.
4. `placeTrayWindow` (shared): left edge at the cursor unless it would overflow,
   submenus open right or left, open downward if the whole menu fits else upward,
   moved (never shrunk) into the work area. 8px shadow padding; panel 310px;
   window wide enough for 2 cascading submenus.
5. `trayNativeBounds` converts back to GTK units; `beforeTrayShow` sets popup
   hints (`skip_taskbar`, `skip_pager`, `keep_above`, type UTILITY before map)
   and zoom; `focusTrayWindow` → `presentNow`.

`DisableResize` breaks window height under GTK, so it stays off
(`setTrayWindowOptions`). On Linux the popup opens in place, transparent, and
fades in once drawn (`trayOpensOffscreen = false`; X WMs may clamp off-screen
windows). With compositing off (plain XFCE), the transparent margin renders as
a solid/black box; not handled.

## Focus

`presentNow` (`ui/gtk_linux.go`) calls `gtk_window_present_with_time` with a
fresh **X server timestamp** (`gdk_x11_get_server_time`). Without it Mutter's
focus-stealing prevention refuses focus, and KWin keeps windows opened without
a startup id behind the active window. Used for the popup and every window
(`focusWindow`).

Click-away closes the popup via the WindowLostFocus event. Stock GNOME has no
desktop window, so a click on the empty wallpaper produces no focus change; our
GNOME extension reports those clicks (`ClickAway`). On XFCE, click-away on the
desktop needs xfdesktop running (it is in real sessions). Escape and clicking
the icon again also close it (`trayReopenGuard` 300ms stops the same click from
reopening).

## Monitors: Wails' cache is stale (issue #83 part 1, fixed `d1d1367`)

Wails v3.0.0 on Linux reads `app.Screen` once at startup and never refreshes.
After switching laptop panel → 4K monitor, the popup was clamped to the old
3200x2000 screen. `monitors()` queries GDK live every time; nothing on Linux
uses Wails screens. Verified on KDE at 4K and after a live switch to 2560x1440.

## Plasma mixed scales (issue #83 part 2, fixed `e80514c`)

Plasma's tray (`SystemTray::popupPosition` in plasma-workspace
`applets/systemtray/systemtray.cpp`) with XwaylandClientsScale and more than
one screen sends `nativeTopLeft + (pos - logicalTopLeft) * <panel screen's own
DPR>`. KWin's X space is logical × **one** Xwayland scale (the largest screen
scale). They only agree when the panel's screen has the max scale.

Reproduced on the owner's laptop (eDP-1 at 1.5 + DP-1 at 1.25, panel on DP-1):
Plasma sent `5511,2111`, correct X was `7233,2533`. Fix:
- `ui/wayland_linux.go`: a tiny pure-Go Wayland client (no new deps) that reads
  `wl_output`/xdg-output logical position, size and exact scale of each output.
- `ui/plasmaclick_linux.go` `plasmaClickToX`: undo Plasma's per-screen
  conversion, map to X using the single XWayland scale (root width / logical
  width). Only on KDE (`XDG_CURRENT_DESKTOP` contains KDE), Wayland session,
  X11 GDK backend. If XWayland scale is 1, the click is left alone. Ambiguous
  clicks pick the screen where the decoded point is closest to an edge (tray
  icons sit on panels at edges). That heuristic can be wrong when a
  higher-scale screen is left of/above the panel's screen near the shared edge.
- Tests: `ui/plasmaclick_linux_test.go` with the recorded real clicks plus a
  layout like the reporter's (4 × 1440p, one portrait at 125%).
- Owner confirmed the popup opens at the icon. The reporter's real hardware
  was not tested.

Single-monitor Plasma 6.8: Activate coords were correct X px (3448,2124 on
3840x2160 at 1.25).

**Separate KWin bug (not worked around):** `_NET_WORKAREA` isn't recomputed
when the Xwayland scale changes, only when struts change. After a live layout
change the GDK work area can be wrong (popup pushed left/up) until a panel
change (e.g. toggling auto-hide) or relogin.

## GNOME: our Shell extension

GNOME has no tray. With the AppIndicator extension (default on Ubuntu, not
stock Fedora/Debian/Arch), a **single left-click never calls Activate**; it
opens the native menu, and only double-click activates (indicatorStatusIcon.js;
no app setting changes this). Owner decided (2026-10-30) to ship our own
extension.

`packaging/linux/gnome-shell-extension/pangolin@pangolin.net/` (GNOME 45–51,
ES modules):
- Owns `net.pangolin.Client.ShellExtension` on the session bus; the app sees
  that name and makes its SNI passive.
- Watches `net.pangolin.Client`; the button only exists while the app runs.
- App side (`ui/shell_linux.go`): object `/net/pangolin/Client`, interface
  `net.pangolin.Client1`: `Activate(x,y,stageW,stageH)`, `ClickAway()`,
  `Quit()`, properties `Icon` (ay, .ico bytes), `Tooltip`, `PopupVisible`.
- Left click → Activate with the button position; right click → native menu
  with Quit. While the popup is visible, a stage `captured-event` handler
  reports clicks outside the app's windows (matched by WM_CLASS
  `pangolin-client`) as ClickAway.
- GNOME 49+ PanelMenu.Button uses `Clutter.ClickGesture` (replaced by ours);
  older versions use `vfunc_event`.
- Needs a `shell-version` bump in `metadata.json` for each new GNOME release.
  (`metadata.json` `url` still says `github.com/fosrl/windows`.)
- The extension draws none of the menu; the popup is the same webview as
  everywhere else.

Install notes for users/testers: GNOME on Wayland only scans for new
extensions at shell start, so **log out and back in**, then
`gnome-extensions enable pangolin@pangolin.net`. Installed under
`/usr/share` (packages) or `$PREFIX/share` (tar.gz, `make install-linux` →
`/usr/local/share`; check `XDG_DATA_DIRS` includes it). The make target is
`install-linux`, not `linux-install`. Check `gsettings get org.gnome.shell
disable-user-extensions` is false. Logs: `journalctl --user -b -o cat
/usr/bin/gnome-shell | grep -i pangolin`.

Tested: GNOME 51 container at 1x and 2x (2x needs `gsd-xsettings` running, as
in real sessions), owner's GNOME 51 VM. Not tested: GNOME fractional scaling,
mixed-scale monitors on GNOME.

## Per-desktop status

| | Tray | Popup placement | Tested how |
| --- | --- | --- | --- |
| KDE Plasma Wayland | SNI (built in) | XWayland; Plasma mixed-scale fix | Owner's laptop, real clicks, single + mixed scale |
| XFCE (X11) | SNI via panel Status Tray plugin | direct X11 | Container with real X11 clicks; owner's Arch XFCE install |
| GNOME Wayland | our extension (AppIndicator fallback = double-click) | XWayland; stage→root scaling | Containers 1x/2x, owner's VM |

Dark mode: follows the xdg-desktop-portal `org.freedesktop.appearance color-scheme` and switches
live (`ui/darkmode_linux.go` `watchColorScheme`, `setPreferDark`), including on
KDE where the GTK theme itself may not switch.
