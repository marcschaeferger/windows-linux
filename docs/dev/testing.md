# Testing

Claude sessions on this machine have Docker but **no sudo** (`sudo -n` asks
for a password). Root-only things (daemon, packages, `product_uuid`) are tested
in containers, or the owner runs them (suggest `! sudo …` so output lands in
the conversation). Nobody can run Windows here: cross-compile and vet, then ask
the owner to smoke-test.

Scratch work belongs in the session scratchpad. It's wiped on reboot, so
harnesses from earlier sessions are gone; rebuild them from the recipes below.

## Baseline checks (always)

```sh
go vet -tags gtk3 ./... && go test -tags gtk3 ./...
GOOS=windows go vet -unsafeptr=false ./... && GOOS=windows go build -tags production -o /dev/null .
make build-linux
cd ui/frontend && npm run typecheck
```
Pre-existing `gofmt` complaints (import order) exist in files nobody touched;
don't mix reformatting into feature changes.

To check that a refactor didn't drop Windows code: build the Windows binary
before and after (`git worktree` for the old commit) and diff
`go tool nm` symbol lists filtered to `github.com/fosrl/windows-linux`.

## Owner's machine (KDE Plasma Wayland on Arch)

Laptop eDP-1 (3200x2000, 1.5) + external DP-1 4K (3840x2160, 1.25), panel
usually on DP-1, auto-hide + floating. The installed package's daemon runs at
`/usr/bin/pangolin-client --daemon`. To test a build: quit the running UI
(tunnel stays up; the daemon owns it), run `./build/pangolin-client`. Debug
logging: machine config `{"logLevel": "debug"}` in
`/etc/pangolin-client/pangolin.json` (needs root) — UI log at
`~/.local/state/pangolin-client/logs/pangolin.log`.

Run a background UI from a tool call without killing your shell:
```sh
setsid nohup ./build/pangolin-client > $S/ui.log 2>&1 < /dev/null &
```
Don't `pkill -f pangolin-client` with a pattern that also matches the shell
running the command (it killed the shell once).

### Simulated tray clicks (KDE, SNI)

```sh
P=$(pgrep -f 'build/pangolin-client' | head -1)
ID=org.kde.StatusNotifierItem-$P-1
busctl --user call $ID /StatusNotifierItem org.kde.StatusNotifierItem Activate ii 3448 2124
busctl --user get-property $ID /StatusNotifierItem org.kde.StatusNotifierItem ToolTip
busctl --user call $ID /MenuBar com.canonical.dbusmenu GetLayout iias 0 -- -1 0
busctl --user call $ID /MenuBar com.canonical.dbusmenu Event isvu 1 clicked s "" 0   # Quit
```
Log real clicks to see what Plasma sends:
```sh
dbus-monitor --session "type='method_call',interface='org.kde.StatusNotifierItem',member='Activate'"
```
Read popup geometry from X: `DISPLAY=:1` on this machine (XWayland), use
`xdotool search --classname pangolin-client`, `xwininfo`, `xprop -root
_NET_ACTIVE_WINDOW`, `xdpyinfo | grep dimensions`, `xrdb -query | grep dpi`.
Layout: `kscreen-doctor -o`, `grep -A3 Xwayland ~/.config/kwinrc`.

Change the layout (ask first; the screen flickers):
`kscreen-doctor output.DP-1.mode.2560x1440@144`, `output.eDP-1.enable`, etc.
Put it back afterwards and tell the owner what state it's in.

**Don't run `spectacle` while KDE changes display modes**: it grew to 18 GB
and was OOM-killed (taking the test command with it). Screenshot only in a
stable layout, or read X geometry instead. Real clicks by the owner beat
simulated ones; simulated clicks once misled a scaling conclusion.

## Desktop containers

Images on this machine (rebuildable): `pangolin-spike-xfce`,
`pangolin-spike-gnome` (GNOME 51), `pangolin-upd-ubuntu`,
`pangolin-upd-fedora`, `pangolin-upd-arch` (systemd + GUI deps). They're
several GB each.

XFCE: run Xvfb, `xfwm4 --compositor=on`, `xfce4-panel` (tray) and
`xfdesktop` (needed for click-away on the desktop) under a user via `runuser -u
alice -- env DISPLAY=:1 HOME=/home/alice dbus-run-session -- bash -c '…'`; real
X11 clicks with `xdotool`; screenshots with `import -window root`. Run the
daemon as root in the same container (`pangolin-client --daemon &`).

GNOME 51 in a container needs: a system D-Bus (gnome-shell crashes without
one), a logind stub, ES-module paths (`resource:///org/gnome/shell/ui/...`; old
`imports.ui` don't work), the public X display `:0` (Mutter's private `:1` is
wrong), `Shell.Eval` results are JSON-encoded, the Overview open at start (close
it right before clicking), `gsd-xsettings` for 2x scale. GNOME 51 has no
`gresource` tool; read `panelMenu.js` from the running shell's resources.
Single left-click via the AppIndicator extension opens the menu, not Activate
(that's expected; our extension fixes it).

## Daemon/IPC tests without GTK

`managers` has no GTK dependency, so a static harness binary (a small `main`
that calls `managers.Run()` or acts as a client using `AttachUI`, secrets IPC,
`RequestUIAction`, CLI secrets) can run the real daemon code in any container.
Used for: attach/second-instance, secrets per uid, path traversal, posture,
tunnel start/stop, companion mode with the real CLI from `../cli`.

## End-to-end update test (Linux)

Recipe (used 2026-11-02, -05, -06):

1. Throwaway signify key: `signify -G -n -p pub -s sec` (Debian:
   `signify-openbsd`); public key base64 = second line of `pub`.
2. Build two versions with test overrides:
   ```sh
   LD="-s -w -X github.com/fosrl/windows-linux/version.Number=0.16.0 \
     -X github.com/fosrl/windows-linux/updater.releasePublicKeyBase64=$PUB \
     -X github.com/fosrl/windows-linux/updater.updateBaseURL=http://172.17.0.1:8765"
   CGO_ENABLED=1 go build -tags "gtk3 production" -ldflags="$LD" -o old/pangolin-client .
   # same with version.Number=0.16.1 → new/
   ```
   Package each with nfpm (`VERSION=… GOARCH=amd64 BINARY=… nfpm pkg -f
   packaging/linux/nfpm.yaml -p deb|rpm|archlinux -t …`). Release packages
   should be built in `ubuntu:22.04` for glibc; for local tests the host build is fine.
3. Manifest: `DOWNLOAD_LOCATION_TEMPLATE='http://172.17.0.1:8765/pkgs/%s'`
   (one `%s` = file name) `scripts/generate-linux-manifest.sh <new pkgs>`,
   sign with the test key, serve with `python3 -m http.server 8765` so that
   `/linux-client/latest.sig` and `/pkgs/<file>` exist. 172.17.0.1 is the
   docker bridge.
4. Systemd container:
   ```sh
   docker run -d --name pg-deb --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock \
     -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v $S/pkgs:/pkgs:ro -v $S/e2e:/e2e:ro \
     pangolin-upd-ubuntu /sbin/init
   docker exec pg-deb /e2e/test.sh deb
   ```
5. In the scenario script: install the old package, create a user (**uid 1001
   on Ubuntu**: the image already has uid 1000 `ubuntu`), give them a real
   logind session with `systemd-run --uid=1001 -p PAMName=login …` (needed for
   the quit-install `loginctl` check; Fedora's image has no `runuser` and its
   PAM stack doesn't open a logind session, so adapt), write their
   `~/.config/pangolin-client/pangolin.json` with
   `{"autoDownloadUpdatesEnabled": true, "onboardingSeenWelcome": true,
   "onboardingAcknowledgedPrivacy": true}`, drive a fake UI client to attach /
   turn on downloads / quit, then assert the installed version
   (`dpkg-query`/`rpm -q`/`pacman -Q`), service enabled + new PID, staged dir
   emptied, relaunch file handled.
6. UI "Updating Pangolin…" window: run the real UI in the XFCE container with
   a fake `systemd-run` in PATH that sleeps then fails, screenshot it, check it
   re-execs and starts normally with the update offered again.

Scenarios that passed on Ubuntu/Fedora/Arch: install on quit; no install with
the setting off; no install without a login session; install at launch; install
at daemon start; corrupted staged package rejected (old version keeps running);
non-admin Install with and without a staged update; failure path.

## Packaging tests

Maintainer scripts must be tested under real systemd (containers above), since
plain containers skip `systemctl`. Cases: fresh install, upgrade, user-disabled
then upgrade, remove, reinstall after remove, purge then install. For the
tarball: `DESTDIR=$S/stage ./install.sh` and inspect, plus a real run in a
systemd container and ideally an OpenRC one (not done yet).

## What has never been tested

- A real tunnel connection through the Linux daemon by Claude (no Pangolin
  account in sessions); the owner tested step 3 themselves on 2026-11-02.
- `pangolin up` via companion mode against a real server.
- Polkit password dialog on a real desktop (no agent in containers).
- GNOME fractional scaling; mixed-scale monitors on GNOME.
- The tar.gz on a real OpenRC/systemd system; the AUR push.
- The #83 reporter's actual 4-monitor setup.
