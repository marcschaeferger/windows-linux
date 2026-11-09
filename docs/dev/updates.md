# Updates (Windows and Linux)

Both platforms share: the signed manifest format, version comparison, the
update state machine (`managers/updatestate.go`), the silent
auto-download-and-install feature, the UI update window and Preferences
toggle. They differ in how a package is downloaded and installed.

## Signed manifest

`latest.sig` is signify's embedded format (`signify -S -e`):
```
untrusted comment: ...                      ← not signed
<base64 Ed25519 signature (10-byte header + 64 bytes)>
<b2sum-256 hex>  <file name>  <download location>     ← signed text, one line per file
...
```
`updater/signify.go` `readFileList` verifies with `releasePublicKeyBase64`
(`updater/constants.go`, a `var` so test builds can override it via
`-ldflags -X`). **Any edit to a line, including the URL, breaks the
signature**; re-sign with `scripts/sign-manifest.sh signing-keys/release.sec
[dir]`. Keys: `signing-keys/release.{pub,sec}` locally (gitignored), CI secret
`UPDATE_SIGNING_KEY`. Same key for Windows and Linux.

`findCandidate` (`updater/versions.go`) picks the first file matching this
machine's `candidatePattern()` whose version (numeric dot parts only; each
part parsed as uint16) is newer than `version.Number`:
- Windows: `pangolin-<arch>-<ver>.msi` (`pangolin-amd64-…`).
- Linux: `pangolin-client-<goarch>-<ver>` + `.deb` / `.rpm` / `.pkg.tar.zst`
  according to `/usr/share/pangolin-client/package-format` (`deb`, `rpm`,
  `archlinux`, `aur`). AUR and hand-installed copies look at `.deb` entries
  just to learn the latest version.

Note: an `-rc.N` version (from an rc tag) won't parse as numeric in
`versionNewerThanUs`, so rc builds can't be compared. Release manifests should
list plain X.Y.Z files.

Download location: absolute URL, or a path relative to the update server.
Manifests point at GitHub release assets:
- Windows: `https://github.com/fosrl/windows-linux/releases/download/windows-<ver>/<file>`
- Linux: `https://github.com/fosrl/windows-linux/releases/download/linux-<ver>/<file>`
  (`scripts/generate-linux-manifest.sh`, override with
  `DOWNLOAD_LOCATION_TEMPLATE`).

## Update server

S3 bucket `s3://static.pangolin.net/` served at `https://static.pangolin.net`:
- `/windows-client/latest.sig` plus one `<version>.sig` per release (0.5.0 …
  0.16.0 as of 2026-11-05).
- `/linux-client/latest.sig` (it returned 403 on 2026-11-05 because it hadn't
  been uploaded yet; check before assuming it exists).
- `/get-linux.sh` (install script).

Use `AWS_PROFILE=fossorial` (the default profile can't list it). Bucket
versioning is **off**, so overwrites are permanent. **Never upload without the
owner's explicit go-ahead in that conversation.** If there's a CDN in front,
`latest.sig` may need an invalidation after upload.

`updateBaseURL` (Linux, `-ldflags -X`) replaces scheme+host for test builds.

## State machine (`managers/updatestate.go`, shared)

```
Unknown ──check finds newer──▶ FoundUpdate
FoundUpdate ──auto-download wanted, staged──▶ UpdateDownloaded
UpdateDownloaded ──7 days not installed──▶ InstallReminder   (hourly ticker)
UpdatesDisabledUnofficialBuild (Windows unsigned builds)
```
`HasUpdate()` is true for Found/Downloaded/Reminder. `checkForUpdates`
loop: optional boot jitter 2–5 min (`services.StartedAtBoot`, Windows only),
check, stage if `autoDownloadUpdatesWanted()`, notify UIs on change, sleep
`UpdateCheckInterval` (default 24h, min 1h) ±5%. Errors retry after 4–6 min,
then 25–30 min. Disabled entirely by machine config `autoUpdateChecksEnabled:
false`.

UI side (`ui/ipcevents.go`, `ui/update.go`): `userUpdateState` maps
Downloaded/Reminder to FoundUpdate for a user who hasn't turned on
auto-download themselves (another user's toggle can stage it). Found → one
toast per session; Reminder → update window. UpdateDownloaded is silent.

## Silent auto-download and install

Origin: Windows commit `d1d82ba` "add auto download and install update option"
(on main), merged into the Linux branch 2026-11-06 and ported so Linux behaves
the same.

Setting: `autoDownloadUpdatesEnabled` (default false) in machine or user
`pangolin.json`, toggled in Preferences (greyed out with instructions on
AUR/hand-installed Linux builds). `autoDownloadUpdatesWanted()` = not a manual
install && (machine config on || **any signed-in user's** config on).
Turning it on calls IPC `AutoDownloadUpdatesTurnedOn` → download now.

Staging (`updater/staged_*.go`): download to `pending.<ext>.part`, verify hash,
commit `pending.<ext>` + `pending.sig` (the manifest it came from; old manifest
removed first so a half pair is never valid). Directory must be trusted
(Linux: `/var/lib/pangolin-client/updates`, root-owned, not group/other
writable, else replaced; Windows: `%ProgramData%\Pangolin\updates` owned by
SYSTEM/Admins). Re-verified (signature + hash + still newer) right before
install. `LoadStagedUpdate()` at manager start keeps a valid staged update and
deletes everything else (including the just-installed one).

`installStagedUpdate(session, launch, onFailure)` runs only if a staged update
exists and that session/UID has auto-download on (session 0 = machine or any
user). It stops active tunnels, installs, and queues UI relaunches. If an
install is already running, launches are queued. On failure: the staged files
are discarded (so it isn't retried every quit; the next check re-downloads),
state goes back to FoundUpdate, and held-back UIs launch on the old version.
Exception (Linux): if systemd is shutting down (`systemctl is-system-running`
= `stopping`), `installStaged` refuses before touching the files, so they stay
on disk and the next daemon start installs them.

### Install triggers

| Trigger | Windows (`service_windows.go`) | Linux (`daemon_linux.go`) |
| --- | --- | --- |
| User quits Pangolin | UI exits, session still alive, Always-On off, no other session's UI → silent msiexec | UI detaches, Always-On off, after 2s `loginctl` says user active/online, no other UI attached → silent install |
| UI launch | `goStartProcess`: no other UI → install with msiexec progress on that desktop instead of launching; new service launches the UI | attach replies `attachUpdating` (3); UI runs `ui.RunInstallingUpdate` ("Updating Pangolin…" window), polls `Q` every 2s (≤15 min), then `syscall.Exec`s itself so the new binary runs |
| Manager start (boot) | install before launching any UI | install before any attach succeeds |
| Always-On relaunch | relaunch path installs first | relaunch goes through the attach path |
| Sign-out / reboot / shutdown | never | never (logind check; shutdown check) |
| Manual "Install Update" | admin (UAC token) | admin (root/sudo/wheel/admin) if nothing staged; **anyone** if staged (same as quitting) |

## Windows install path

`updater/downloader_windows.go`: WinHTTP to the update server, Authenticode
check of the MSI (`authenticode_windows.go`), `msirunner_windows.go` runs
msiexec: `/qb!-` (basic progress, no cancel) for manual installs, `/qn /norestart` silent for quit/boot,
`runMsiInSession` (SYSTEM, progress on the user's desktop) for launch. The MSI
stops/replaces the service; the new service relaunches UIs from the
restart-ui flag. Unsigned (unofficial) builds get
`UpdateStateUpdatesDisabledUnofficialBuild` (a `PANGOLIN_ALLOW_DEV_UPDATES`
override exists but is commented out).

## Linux install path (`updater/update_linux.go`)

- Download with Go `net/http` (UA `pangolin-linux-<ver>`), 100 MiB cap, BLAKE2b
  check against the manifest.
- Install in a **transient systemd unit** so the package's postinst restarting
  `pangolin-client.service` doesn't kill the install:
  `systemd-run --unit=pangolin-client-update-<ts> --collect --wait --quiet
  --setenv=DEBIAN_FRONTEND=noninteractive --setenv=ZYPP_LOCK_TIMEOUT=300 -- <cmd>`.
  Errors name the unit: `journalctl -u pangolin-client-update-<ts>`.
- Commands (`installCommand`):
  - deb: `apt-get install -y -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=300 <file>` (fallback `dpkg -i --force-confold`)
  - rpm: `dnf install -y --nogpgcheck` / `zypper --non-interactive install --allow-unsigned-rpm` / `yum install -y --nogpgcheck` / `rpm -U --replacepkgs`
  - Arch: wait up to 5 min for `/var/lib/pacman/db.lck` to go away, then `pacman -U --noconfirm`
- Packages aren't signed for the package manager; the signed manifest is what
  vouches for them. A system with pacman `LocalFileSigLevel = Required` will
  refuse the Arch package.
- Before a manual or staged install the daemon records which UIDs have a UI
  open (`prepareUpdateRestart` → `/var/lib/pangolin-client/restart-ui-after-update`);
  the new daemon reopens them with `systemd-run --user --machine=<user>@.host`.
- The UI's update window shows progress; on Linux the "installing" phase is
  the small window shown while waiting.

### Manual-update installs (`updater.ManualUpdate`)

| `package-format` | Behavior |
| --- | --- |
| `deb`, `rpm`, `archlinux` | in-app updates (manual + silent) |
| `aur` (written by the AUR PKGBUILD) | notify only; window shows `yay -Syu pangolin-client-bin` with Copy |
| missing (tar.gz, `make install-linux`, hand builds) | notify only; link to GitHub releases `?q=linux-` |

History: Arch originally was notify-only too (2026-10-30 decision). On
2026-11-05 the owner chose in-app updates for the release `.pkg.tar.zst`
(installed with `pacman -U`), because an AUR account couldn't be created
(registrations restricted), and kept AUR installs on notify-only so the app
and the AUR helper don't fight (`pangolin-client-bin` conflicts with
`pangolin-client`).

## Releasing an update (summary; full steps in BUILD_STEPS.md)

1. Push tag `linux-X.Y.Z` / `windows-X.Y.Z`; CI builds a draft release with
   assets and `<os>-latest.sig` (signed if `UPDATE_SIGNING_KEY` is set, else
   `<os>-filelist.txt` to sign by hand).
2. Publish the release (assets must be downloadable before clients see the manifest).
3. Owner uploads `<os>-latest.sig` → `/<os>-client/latest.sig` (and
   `<version>.sig` for Windows history).

## Testing updates

See [testing.md](testing.md#end-to-end-update-test-linux): test key + local
HTTP server via ldflags, packages for two versions, systemd containers for
Ubuntu (apt), Fedora (dnf), Arch (pacman). Last full run 2026-11-06: 22 checks
× 3 distros passed (quit, setting off, no logind session, launch, boot, corrupt
staged package rejected, non-admin with/without staged update, failure path).
