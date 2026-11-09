# Pangolin client for Windows and Linux

One Go codebase (module `github.com/fosrl/windows-linux`) builds the Pangolin
desktop client for Windows (`Pangolin.exe` + MSI) and Linux (`pangolin-client`
deb/rpm/Arch/tar.gz). The UI is a React app in `ui/frontend` rendered by Wails
v3.0.0 (WebView2 on Windows, WebKitGTK 4.1 on Linux). A privileged background
process (Windows service / Linux root daemon) owns tunnels, secrets and updates;
the UI runs as the user and talks to it over gob IPC.

**Read `docs/dev/` before changing anything non-trivial.** Start at
`docs/dev/README.md`. It covers architecture, Windows, Linux, the tray popup,
updates, packaging, testing, how-to recipes, and the decision history.

## Build and check

```sh
make build-linux                     # build/pangolin-client (needs gtk3, webkit2gtk-4.1, cgo)
make build VERSION=1.2.3             # build/Pangolin.exe (cross-compiles from Linux fine)
make test                            # go test -tags gtk3 ./...
go vet -tags gtk3 ./...              # Linux vet
GOOS=windows go vet -unsafeptr=false ./... && GOOS=windows go build -tags production -o /dev/null .
make package-linux VERSION=1.2.3     # deb, rpm, pkg.tar.zst, tar.gz (needs nfpm)
make bindings                        # after changing any Go method bound to the frontend
cd ui/frontend && npm run mock       # browser preview of the UI with fake data
```

Always check **both** platforms compile after touching shared code. CI
(`.github/workflows/test.yml`) runs exactly the vet/test/cross-build above.

## Rules that are easy to break

- Linux builds need `-tags gtk3` (a guard file fails the build without it).
  GTK4 can't position windows, so the tray popup needs GTK3.
- Per-OS code goes in `*_windows.go` / `*_linux.go` with the **same function
  names** on both sides; shared code calls them. Don't add `//go:build windows`
  to whole shared files.
- Changing a bound Go method's signature or the module path means regenerating
  bindings (`make bindings`); binding IDs are hashes of the fully-qualified
  method name, so moved/stale bindings compile but fail at runtime.
- Adding a `config.Config` field: add it to the struct, `mergeConfig` and
  `copyConfig` in `config/config.go` or it silently won't merge/copy.
- Adding an IPC method: append to the `MethodType` iota (never reorder), add
  client + server cases. See `docs/dev/recipes.md`.
- Linux paths use `pangolin-client` (not `pangolin`, which the CLI owns).
- Never upload to `s3://static.pangolin.net/` without the owner's explicit
  go-ahead in that conversation. Use `AWS_PROFILE=fossorial` for reads.
- Don't commit, push, tag, or post on GitHub issues unless asked. The owner
  usually commits work themselves after testing.
- GitHub comments/issues the owner asks for: keep them short and avoid the
  writing tropes listed in `../../tropes.md` (i.e. `/home/owen/tropes.md`).
- Release tags: `windows-X.Y.Z` and `linux-X.Y.Z` (optional `-rc.N`). The
  version is stamped from the tag via `-ldflags`; a plain build reports `dev`.
