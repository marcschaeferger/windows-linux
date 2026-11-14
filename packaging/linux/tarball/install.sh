#!/bin/sh
# Installs the Pangolin client from the release's tar.gz, for distributions
# without a deb, rpm or Arch package (Gentoo, Void, Alpine, ...). Installs the
# same files as the packages, plus an OpenRC service where systemd isn't the
# init system, then starts the background service.
#
# Usage: sudo ./install.sh [--uninstall]
#
# Environment:
#   PREFIX   Where the binary, menu entry, icon and GNOME extension go.
#            Default /usr/local. The service, autostart entry and polkit
#            policy always go under /etc and /usr/share.
#   INIT     systemd, openrc or none. Detected when unset.
#   DESTDIR  Stage the files under this directory instead, for packaging.
#            Doesn't touch the running system.
#
# Installing also leaves a copy of this script as
# $PREFIX/lib/pangolin-client/uninstall.sh, which removes everything again.
# Saved sign-ins and logs stay; delete /var/lib/pangolin-client and
# /var/log/pangolin-client too to remove them.

set -eu

PREFIX=${PREFIX:-/usr/local}
DESTDIR=${DESTDIR:-}
NAME=pangolin-client
EXTENSION_UUID=pangolin@pangolin.net

BIN=$PREFIX/bin/$NAME
APPLICATIONS=$PREFIX/share/applications
ICON=$PREFIX/share/icons/hicolor/256x256/apps/$NAME.png
EXTENSION_DIR=$PREFIX/share/gnome-shell/extensions/$EXTENSION_UUID
LIB_DIR=$PREFIX/lib/$NAME
AUTOSTART=/etc/xdg/autostart/$NAME-autostart.desktop
POLKIT=/usr/share/polkit-1/actions/net.pangolin.client.policy
SYSTEMD_UNIT=/etc/systemd/system/$NAME.service
OPENRC_SCRIPT=/etc/init.d/$NAME

say() { printf '%s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

detect_init() {
    if [ -n "${INIT:-}" ]; then
        case "$INIT" in systemd|openrc|none) echo "$INIT"; return ;; esac
        die "INIT must be systemd, openrc or none, not $INIT"
    fi
    if [ -d /run/systemd/system ]; then
        echo systemd
    elif [ -x /sbin/openrc-run ] || command -v openrc-run >/dev/null 2>&1; then
        echo openrc
    else
        echo none
    fi
}

uninstall() {
    if [ -z "$DESTDIR" ]; then
        if [ -d /run/systemd/system ] && [ -e "$SYSTEMD_UNIT" ]; then
            systemctl disable --now "$NAME.service" >/dev/null 2>&1 || true
        fi
        if [ -e "$OPENRC_SCRIPT" ] && command -v rc-service >/dev/null 2>&1; then
            rc-service "$NAME" stop >/dev/null 2>&1 || true
            rc-update del "$NAME" >/dev/null 2>&1 || true
        fi
    fi
    rm -f "$DESTDIR$BIN" "$DESTDIR$APPLICATIONS/$NAME.desktop" "$DESTDIR$ICON" \
        "$DESTDIR$AUTOSTART" "$DESTDIR$POLKIT" "$DESTDIR$SYSTEMD_UNIT" "$DESTDIR$OPENRC_SCRIPT"
    rm -rf "$DESTDIR$EXTENSION_DIR" "$DESTDIR$LIB_DIR"
    if [ -z "$DESTDIR" ] && [ -d /run/systemd/system ]; then
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    say "Pangolin was removed. Saved sign-ins and logs are still in /var/lib/$NAME and /var/log/$NAME."
}

install_files() {
    src=$(cd "$(dirname "$0")" && pwd)
    [ -f "$src/bin/$NAME" ] || die "run install.sh from the extracted archive (no bin/$NAME next to it)"
    init=$1

    # Exec= and TryExec= are the full path so the menu entries work when PREFIX
    # isn't in PATH.
    exec_sed="s|^Exec=$NAME|Exec=$BIN|;s|^TryExec=$NAME|TryExec=$BIN|"

    install -Dm755 "$src/bin/$NAME" "$DESTDIR$BIN"
    install -d "$DESTDIR$APPLICATIONS" "$DESTDIR$(dirname "$AUTOSTART")"
    sed "$exec_sed" "$src/share/applications/$NAME.desktop" > "$DESTDIR$APPLICATIONS/$NAME.desktop"
    sed "$exec_sed" "$src/etc/xdg/autostart/$NAME-autostart.desktop" > "$DESTDIR$AUTOSTART"
    chmod 644 "$DESTDIR$APPLICATIONS/$NAME.desktop" "$DESTDIR$AUTOSTART"
    install -Dm644 "$src/share/icons/hicolor/256x256/apps/$NAME.png" "$DESTDIR$ICON"
    install -Dm644 "$src/share/polkit-1/actions/net.pangolin.client.policy" "$DESTDIR$POLKIT"
    install -d "$DESTDIR$EXTENSION_DIR"
    install -m644 "$src/share/gnome-shell/extensions/$EXTENSION_UUID"/* "$DESTDIR$EXTENSION_DIR/"

    case "$init" in
        systemd)
            install -d "$DESTDIR$(dirname "$SYSTEMD_UNIT")"
            sed "s|/usr/bin/$NAME|$BIN|" "$src/lib/systemd/system/$NAME.service" > "$DESTDIR$SYSTEMD_UNIT"
            chmod 644 "$DESTDIR$SYSTEMD_UNIT"
            ;;
        openrc)
            install -d "$DESTDIR$(dirname "$OPENRC_SCRIPT")"
            sed "s|/usr/bin/$NAME|$BIN|" "$src/etc/init.d/$NAME" > "$DESTDIR$OPENRC_SCRIPT"
            chmod 755 "$DESTDIR$OPENRC_SCRIPT"
            ;;
    esac

    # This script, with PREFIX baked in, to uninstall without the archive.
    install -d "$DESTDIR$LIB_DIR"
    sed "s|^PREFIX=\${PREFIX:-/usr/local}$|PREFIX=\${PREFIX:-$PREFIX}|" "$src/install.sh" > "$DESTDIR$LIB_DIR/uninstall.sh"
    chmod 755 "$DESTDIR$LIB_DIR/uninstall.sh"
}

start_service() {
    init=$1 upgrade=$2
    case "$init" in
        systemd)
            systemctl daemon-reload
            if [ "$upgrade" = 1 ]; then
                systemctl try-restart "$NAME.service"
            else
                systemctl enable --now "$NAME.service"
            fi
            ;;
        openrc)
            if [ "$upgrade" = 1 ]; then
                if rc-service "$NAME" status >/dev/null 2>&1; then
                    rc-service "$NAME" restart
                fi
            else
                rc-update add "$NAME" default
                rc-service "$NAME" start
            fi
            ;;
        none)
            say "No systemd or OpenRC found. Run \"$BIN --daemon\" as root at boot to start the background service."
            ;;
    esac
}

main() {
    mode=install
    [ "$(basename "$0")" = uninstall.sh ] && mode=uninstall
    case "${1:-}" in
        --uninstall) mode=uninstall ;;
        "") ;;
        -h|--help) sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
        *) die "unknown argument $1 (try --help)" ;;
    esac
    if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
        die "run as root, e.g. sudo $0${1:+ $1}"
    fi

    if [ "$mode" = uninstall ]; then
        uninstall
        return
    fi

    init=$(detect_init)
    upgrade=0
    [ -e "$DESTDIR$BIN" ] && upgrade=1
    install_files "$init"
    say "Installed Pangolin to $PREFIX (service: $init)."
    [ -n "$DESTDIR" ] && return

    # Not every desktop refreshes these caches by itself.
    command -v gtk-update-icon-cache >/dev/null 2>&1 &&
        gtk-update-icon-cache -q -t "$PREFIX/share/icons/hicolor" 2>/dev/null || true
    command -v update-desktop-database >/dev/null 2>&1 &&
        update-desktop-database -q "$APPLICATIONS" 2>/dev/null || true

    start_service "$init" "$upgrade"
    say "Open Pangolin from your applications menu. On GNOME, log out and back in, then run"
    say "\"gnome-extensions enable $EXTENSION_UUID\" for the tray icon."
    say "To uninstall: sudo $LIB_DIR/uninstall.sh"
}

main "$@"
