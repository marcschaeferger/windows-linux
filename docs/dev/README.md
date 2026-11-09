# Developer notes (for future Claude sessions and maintainers)

These notes record how the client works on both platforms, why it was built
that way, and how to change it safely. They were written on 2026-11-09 from the
code at `f147784` plus the history of every Claude session that built the
Linux port (2026-10-30 to 2026-11-09). When code and notes disagree, the code
wins. Fix the note.

| File | What's in it |
| --- | --- |
| [architecture.md](architecture.md) | Process model on both OSes, package map, IPC protocol, data flow, config layers |
| [windows.md](windows.md) | Manager service, UI launch, named pipes, elevation, MSI, WebView2, signing, Windows release |
| [linux.md](linux.md) | Root daemon, socket protocol, tunnels as child processes, secrets, paths, polkit, posture, CLI companion mode |
| [linux-tray-popup.md](linux-tray-popup.md) | XWayland, scaling units, our StatusNotifierItem, the GNOME Shell extension, KDE mixed-scale fix, monitors |
| [updates.md](updates.md) | Signed manifests, Windows MSI updates, Linux package updates, silent auto-download/install on both, update server |
| [linux-packaging.md](linux-packaging.md) | nfpm packages, maintainer scripts and service auto-start, AUR, tar.gz + OpenRC, get-linux.sh, CI release |
| [testing.md](testing.md) | How to test each area: containers, E2E update harness, simulated tray clicks, GNOME/XFCE containers, pitfalls |
| [recipes.md](recipes.md) | Step-by-step: add a setting, an IPC method, a bound service method, a menu item, a window, a per-OS feature |
| [history.md](history.md) | Timeline, owner decisions with reasons, open issues, known gaps and things deliberately left alone |

Other docs in the repo:
- `BUILD_STEPS.md`: release procedure (signing keys, manifests, tags). Still accurate.
- `packaging/linux/tarball/README.md`: shipped inside the tar.gz for end users.
- `README.md`: public readme.
- Public user docs live in a sibling repo: `../docs/content/docs/manage/clients/platforms/linux.mdx`
  (and `windows.mdx`). The website downloads page is in `../landing-v4/`.

Related repos on this machine: `../cli` (Pangolin CLI, companion mode client
side), `../olm` (tunnel library), `../newt` (logger and shared code).
