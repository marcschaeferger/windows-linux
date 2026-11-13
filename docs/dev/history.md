# History, decisions, open issues

## Timeline of the Linux port

Branches: work happened on `linux` (later merged), then `dev` → PRs into
`main`. The owner commits and pushes themselves after testing; Claude sessions
leave changes staged or uncommitted unless asked.

| Date | What | Commit(s) |
| --- | --- | --- |
| 2026-10-30 | Plan + step 1 spike: popup proven on KDE (real click), XFCE and GNOME 51 containers; XWayland chosen; GNOME extension decided | none (throwaway app) |
| 2026-11-02 | Step 2: Wails beta.25 → v3.0.0, per-OS file split, `make build-linux` | `ae81859` Basic linux |
| 2026-11-02 | Step 3: root daemon, socket + SCM_RIGHTS, tunnels as children, root-only secrets, posture port, `--autostart` | `3ffa945` Add linux service |
| 2026-11-02 | Step 4: own SNI tray, scaling, focus, dark mode, GNOME extension, OS-aware wording | `9868111` Cross platform ui |
| 2026-11-02 | Step 5: nfpm deb/rpm/Arch, Linux updater, manifest, CI, AUR template | `f9e09c7` Packaging done |
| 2026-11-02 | Windows silent updates landed on main | `d1d82ba` |
| 2026-11-02 | CLI companion mode on Linux (both repos) | `70d197c` |
| 2026-11-04 | Owner set up a GNOME 51 VM; extension needs logout + enable | – |
| 2026-11-05 | Service auto-start on all formats + `.removed` reinstall fix | `a3bc3e9` Start after install |
| 2026-11-05 | Linux CLI install (download + polkit for non-admins) | `762f206` Fix cli install |
| 2026-11-05 | Arch release package updates in app; AUR → notify-only `aur` marker | `f9e0d8f` |
| 2026-11-05 | Repo renamed `fosrl/windows` → `fosrl/windows-linux`; import paths and bindings regenerated | `245c782` |
| 2026-11-05 | Manifest URL rewrite script (dry-run only, never uploaded, not committed) | – |
| 2026-11-06 | Merged main into linux; silent auto-download/install ported to Linux; E2E on 3 distros | `5829e82` |
| 2026-11-06 | Version from tag via ldflags (`set-version.sh` removed); separate `windows-`/`linux-` tags and workflows | `f7f42b2`, `4af7684` |
| 2026-11-06 | Fingerprint compared with CLI; `product_uuid` added to Linux hash in the working tree | **not committed; absent from code** |
| 2026-11-06 | `get-linux.sh` install script | `92c1604` |
| 2026-11-09 | tar.gz + install.sh + OpenRC; get-linux.sh tar fallback + doas; init-aware daemon warning; docs `linux.mdx` updated (landing-page tarball changes not kept) | `bdf2e3d`, `b050af7`, `07bed46` |
| 2026-11-09 | Issue #83: stale Wails monitor cache fixed (live GDK monitors) | `d1d1367` |
| 2026-11-09 | Issue #83: Plasma mixed-scale click conversion (Wayland client) | `e80514c` |
| 2026-11-09 | Issue #84 filed: native Wayland support | – |
| 2026-11-09 | get-linux.sh reads version + sha256 from `api.fossorial.io/api/v1/versions/linux` | `f147784` |

## Owner decisions (with reasons)

- **One codebase** for Windows and Linux, KDE + GNOME + XFCE (2026-10-30).
- **Ship deb, rpm and AUR** (2026-10-30); later tar.gz for others (2026-11-09).
- **XWayland for the UI**: only way to position the popup on Wayland.
- **Our own GNOME Shell extension** instead of requiring AppIndicator, because
  AppIndicator only activates on double-click (2026-10-30).
- **In-app installs run in a transient systemd unit** so the package's service
  restart can't kill them; manual install limited to sudo/wheel/admin members
  (2026-10-30).
- **Linux paths named `pangolin-client`** to avoid the CLI's `pangolin` dirs
  (Claude's choice on 2026-11-02, not formally confirmed by the owner).
- **CLI companion mode deferred** until after step 4, then done 2026-11-02;
  on Linux only active when the desktop client is installed (owner accepted).
- **Service starts automatically after install on every format**, including
  Arch (2026-11-05).
- **Arch release package updates in-app**; AUR copies stay notify-only. AUR
  account couldn't be created (2026-11-05).
- **Silent updates on Linux behave like Windows** (2026-11-06): merge main (not
  cherry-pick); any user's toggle may make the root daemon install silently;
  manual Install without a staged update needs admin (polkit/sudo/wheel), with
  a staged update anyone may; at UI launch with a staged update show
  "Updating Pangolin…", install, start the new version; AUR/hand builds:
  toggle disabled with the manual-update text.
- **Fingerprint stays separate from the CLI's on purpose** (2026-11-06), but
  should collect at least the same information.
- **Don't upload to the S3 update bucket without explicit go-ahead.**
- **GitHub issue comments**: short, no tropes from `/home/owen/tropes.md`.

## Open issues and threads

- **#83** (Debian 13, Plasma 6.3, 4 monitors, one portrait at 125%): popup
  misplaced. Two causes found and fixed (stale monitors; Plasma mixed-scale
  coordinates). Reply posted saying a fix is coming and a build will follow.
  Not yet confirmed by the reporter on a build with `e80514c`. Plasma 6.3 code
  was assumed to match master (not checked).
- **#84** native Wayland support (enhancement, future).
- `linux-client/latest.sig` must be uploaded for Linux update checks to work
  (403 as of 2026-11-05; check current state).
- `get-linux.sh` on static.pangolin.net: upload only after a Linux release
  with a `.tar.gz` exists (tar fallback otherwise fails at download).
- Old Windows manifests on S3 still reference `github.com/fosrl/windows/…`
  (works via GitHub redirect). Only needs rewriting if the old repo name is
  ever reused.

## Known gaps

- Linux doesn't stop a user's tunnel when they log out (Windows does).
- `services.StartedAtBoot()` is always false on Linux (no boot jitter).
- `xdg-utils` not declared as a dependency; `openURL` has no `gio`/`exo-open`
  fallback, so "open in browser" can silently fail on minimal installs.
- Non-composited X (XFCE with compositing off): popup's transparent margin
  shows as a solid box.
- KWin `_NET_WORKAREA` stale after live Xwayland scale change (KWin bug).
- Plasma click heuristic can choose the wrong screen near a shared edge with a
  higher-scale screen left of/above the panel's screen.
- `rc` versions can't be compared by the updater (non-numeric part).
- GNOME extension `metadata.json` url points at the old repo name.
- `managers/install_linux.go` `errNotImplemented` unused.
- Fingerprint hash: since the audit fix, machines with a junk/empty serial
  get a per-install component (`/etc/machine-id`, Windows `MachineGuid`);
  machines with a real serial keep their old hash. No `board_serial` or
  `product_uuid`: adding either would change every existing hash.
- Maintainer email `support@pangolin.net` unconfirmed.

## Lessons from past sessions

- Simulated D-Bus clicks can lie about coordinate spaces; confirm with the
  owner's real click before concluding.
- Session scratchpads are wiped on reboot; the 2026-10-30 spike app was lost.
  Anything worth keeping goes in the repo or memory.
- After a repo/module rename, regenerate Wails bindings; moving the old files
  builds fine but every frontend call fails at runtime.
- A find/replace of the module path once produced
  `github.com/fosrl/windows-linux/linux/...` imports (37 of them).
- Before writing under the user's config dir, check the CLI doesn't own it.
- Old harnesses (E2E update, ipctest, logind stub) only live in old session
  transcripts under `~/.claude/projects/-home-owen-fossorial-windows-linux/*.jsonl`;
  grep tool_use inputs there to recover exact scripts.
