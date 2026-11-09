# Linux packaging, service auto-start, install script, CI

## Formats

| Format | Built by | Distros | Updates |
| --- | --- | --- | --- |
| `.deb` | nfpm | Ubuntu 22.04+, Debian 12+ | in-app (apt-get) |
| `.rpm` | nfpm | Fedora, openSUSE Tumbleweed | in-app (dnf / zypper / yum / rpm) |
| `.pkg.tar.zst` | nfpm (`archlinux` packager) | Arch (`sudo pacman -U <file>`) | in-app (pacman -U) |
| AUR `pangolin-client-bin` | `.github/workflows/aur.yml` from `packaging/linux/aur/PKGBUILD.in` | Arch via yay/paru | notify only (`aur` marker) |
| `.tar.gz` + `install.sh` | `make package-linux-tarball` | Gentoo, Void, Alpine (glibc compat), Slackware, … | notify only (no marker) |

Names: `pangolin-client-<goarch>-<version>.<ext>` (goarch `amd64`/`arm64`).
The updater relies on this pattern.

Builds come from **Ubuntu 22.04 (glibc 2.35)** in CI so the binary runs on
22.04+, Debian 12+, Fedora, openSUSE, Arch. RHEL 9 / Rocky can't install it
(no WebKitGTK 4.1). Package installs were smoke-tested (2026-11-02) on Ubuntu
22.04, 24.04, Debian 12, Fedora, openSUSE Tumbleweed, Arch.

## nfpm (`packaging/linux/nfpm.yaml`)

`make package-linux VERSION=X.Y.Z [LINUX_GOARCH=arm64] [NFPM=path]` runs
`check-release-version` (refuses `dev`), `build-linux`, the tarball, then nfpm
three times. Env: `VERSION`, `GOARCH`, `BINARY`. The binary entry needs
`expand: true` for `${BINARY}` to expand.

Dependencies:
- deb: `libgtk-3-0t64 | libgtk-3-0`, `libwebkit2gtk-4.1-0`
- rpm: `(gtk3 or libgtk-3-0)`, `(webkit2gtk4.1 or libwebkit2gtk-4_1-0)` (Fedora or openSUSE names)
- arch: `gtk3`, `webkit2gtk-4.1`
- Not declared but should be (see linux.md): `xdg-utils` for opening URLs.

Contents (same layout in every format and in the tar.gz):

| Source | Installed to |
| --- | --- |
| binary | `/usr/bin/pangolin-client` |
| `pangolin-client.service` | `/usr/lib/systemd/system/` |
| `pangolin-client.desktop` | `/usr/share/applications/` |
| `pangolin-client-autostart.desktop` | `/etc/xdg/autostart/` (config, noreplace) |
| `net.pangolin.client.policy` | `/usr/share/polkit-1/actions/` |
| `pangolin-client.png` | `/usr/share/icons/hicolor/256x256/apps/` |
| GNOME extension `metadata.json`, `extension.js` | `/usr/share/gnome-shell/extensions/pangolin@pangolin.net/` |
| `package-format/{deb,rpm,archlinux}` | `/usr/share/pangolin-client/package-format` (per packager) |

Maintainer field `Fossorial, Inc. <support@pangolin.net>` was a guess in
2026-11-02; nobody has confirmed the address.

## systemd unit (`pangolin-client.service`)

`Type=simple`, `ExecStart=/usr/bin/pangolin-client --daemon`,
`Restart=on-failure`, `RestartSec=2`, `KillMode=mixed`, `TimeoutStopSec=45`,
`RuntimeDirectory` (0755), `StateDirectory` (0700), `LogsDirectory` (0755) all
`pangolin-client`, after/wants `network-online.target`, `WantedBy=multi-user.target`.

## Service auto-start and maintainer scripts (`packaging/linux/scripts/`)

All scripts exit early without systemd (`/run/systemd/system` missing).

| Script | deb args | rpm args | Arch | Action |
| --- | --- | --- | --- | --- |
| `postinstall.sh` | `configure <old ver or empty>` | `1` install / `≥2` upgrade | `post_install <ver>` | new install → `systemctl enable --now`; upgrade → `try-restart` |
| `preremove.sh` | `remove` / `upgrade` / `failed-upgrade` | `0` remove / `1` upgrade | `pre_remove` | real removal only → `disable --now` + touch `/var/lib/pangolin-client/.removed` |
| `postremove.sh` | `purge` etc. | | | daemon-reload; deb purge removes `/var/lib`, `/var/log`, `/etc` dirs |
| `postupgrade-arch.sh` | | | `post_upgrade` | daemon-reload + try-restart |

**The `.removed` marker** (added 2026-11-05): `apt remove` keeps config files,
so dpkg passes the old version to `configure` on a later reinstall, which made
it look like an upgrade (try-restart of a stopped, disabled service = nothing).
Confirmed with the old scripts, fixed with the marker. `postinstall.sh` treats
"marker present" as a fresh install, then deletes it.

Behavior (tested in systemd containers, 2026-11-05):

| Case | deb (Ubuntu 24.04) | rpm (Fedora 44) | Arch |
| --- | --- | --- | --- |
| Fresh install | enabled + running | enabled + running | enabled + running |
| Upgrade | restarted | restarted | (unchanged script) |
| User disabled it, then upgrade | stays off | stays off | not tested |
| Remove | stopped + disabled | stopped + disabled | stopped + disabled |
| Reinstall after remove | running | running | not tested |

Arch used to only print the enable command (Arch convention). The owner chose
auto-start on every format (2026-11-05).

The UI does **not** start on install; it starts at next login only if the user
turned on open-at-login / connect-at-start (the autostart entry runs
`pangolin-client --autostart`, which exits otherwise), or from the app menu.
The in-app updater relies on `try-restart` during an upgrade (it runs the
install in its own systemd unit, see updates.md).

## AUR (`packaging/linux/aur/`)

`PKGBUILD.in` repackages the release `.pkg.tar.zst` (bsdtar extract into
`$pkgdir`), writes `aur` into `package-format`, `provides`/`conflicts`
`pangolin-client`, `backup` the autostart file. `pangolin-client-bin.install`
enables/starts the service on install and prints the GNOME extension hint.
`aur.yml` is `workflow_dispatch` only (release trigger commented out), skips
prereleases, and does nothing without `AUR_SSH_PRIVATE_KEY`. **No AUR account
exists** (registrations were restricted on 2026-11-05), so it has never pushed.
The PKGBUILD was tested with makepkg + pacman in an Arch container
(2026-11-02); its install hooks were re-run by hand on 2026-11-05.

## tar.gz for other distributions (added 2026-11-09)

Layout: `pangolin-client-<arch>-<ver>/{bin/, share/…, etc/xdg/autostart/,
etc/init.d/pangolin-client, lib/systemd/system/, install.sh, README.md,
LICENSE}`. Built with `--sort=name --owner=0 --group=0`.

`install.sh` (`packaging/linux/tarball/install.sh`, POSIX sh):
- `PREFIX` (default `/usr/local`) for binary, desktop file, icon, GNOME
  extension, `lib/pangolin-client/uninstall.sh`. Autostart, polkit policy,
  service files always go under `/etc` and `/usr/share`.
- `INIT=systemd|openrc|none`, detected otherwise. systemd unit goes to
  `/etc/systemd/system/` with the binary path rewritten; OpenRC script to
  `/etc/init.d/` likewise. `Exec=` in desktop files rewritten to the full path.
- Re-running over an existing install = upgrade (restart service only).
- `DESTDIR` stages files for distro packagers without touching the system.
- `--uninstall` or running the installed `uninstall.sh` removes everything but
  `/var/lib/pangolin-client` and `/var/log/pangolin-client`.
- Refreshes icon cache and desktop database when the tools exist.

OpenRC service (`packaging/linux/openrc/pangolin-client`): `command_background`,
pidfile, `retry="TERM/45/KILL/5"` (like `TimeoutStopSec`), `need net`,
`start_pre` creates the run/state/log dirs systemd would create.

Tested only with `DESTDIR` staging; **not run on a real systemd or OpenRC
system** yet. musl distros need a glibc compatibility layer (warned by
`get-linux.sh`).

## `make install-linux` (dev installs)

`sudo make install-linux [PREFIX=/usr]` installs the build like a package but
under `$PREFIX` (default `/usr/local`) and the unit in `/etc/systemd/system/`.
It doesn't enable the service: `sudo systemctl enable --now pangolin-client`.
No `package-format` file → updates are notify-only. `uninstall-linux` reverses it.

## get-linux.sh (`curl -fsSL https://static.pangolin.net/get-linux.sh | sh`)

- Detects arch (`amd64`/`arm64`) and format (deb/rpm/arch from os-release and
  package managers), `--format deb|rpm|arch|tar`, `--version X.Y.Z`.
- Unknown distro → falls back to `tar` with a warning; extracts and runs
  `install.sh` as root; warns if WebKitGTK 4.1 is missing or on musl.
- Latest version + sha256 checksums come from the Fossorial versions API
  `https://api.fossorial.io/api/v1/versions/linux` (`latestVersion`, asset →
  sha256), parsed with sed (no jq). Checksums exist only for the latest
  release; pinned older versions skip verification with a warning. There's no
  GitHub "latest" fallback because Windows and Linux share the repo.
- Downloads from `github.com/fosrl/windows-linux/releases/download/linux-<ver>/…`.
- Uses `sudo`, or `doas` if sudo is missing.
- The copy served from static.pangolin.net is uploaded by hand; the repo copy
  can be ahead of it. Don't upload without the owner's go-ahead.

## CI

- `test.yml`: on PRs and pushes to `main`/`linux`: Linux vet + test with
  `-tags gtk3`; Windows vet (`-unsafeptr=false`, the WTS EventData cast) and build.
  (The `linux` branch no longer exists; daily work is on `dev`.)
- `linux.yml`: tag `linux-X.Y.Z[-rc.N]` or manual dispatch. Matrix amd64
  (`ubuntu-24.04`) / arm64 (`ubuntu-24.04-arm`), both in `container:
  ubuntu:22.04`. Installs nfpm v2.48.0 with `GOTOOLCHAIN=auto` (nfpm needs a
  newer Go than go.mod). Uploads deb/rpm/zst/tar.gz; release job builds and
  signs the manifest → `linux-latest.sig`, creates a draft release
  (prerelease for `-rc.`).
- `windows.yml`: see [windows.md](windows.md#release-ci-githubworkflowswindowsyml).
- `aur.yml`: above.
- Actions are pinned by commit SHA.

## Public docs to keep in sync

When packaging or install steps change, also update:
- `../docs/content/docs/manage/clients/platforms/linux.mdx` (install tabs per
  format, systemd/OpenRC tabs, "Other Distributions", update, uninstall)
- `../landing-v4/` downloads page. A session on 2026-11-09 added `.tar.gz`
  support there (release lookup, an "Other distributions (Gentoo, Void)"
  entry), but as of 2026-11-09 evening **none of it is in landing-v4**
  (clean tree, no `tar.gz` anywhere), so it was discarded or never kept.
  The docs repo's `linux.mdx` does have the tarball sections.
- `README.md` compatibility line
- `BUILD_STEPS.md`
