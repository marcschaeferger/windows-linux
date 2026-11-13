#!/bin/sh

# Get Pangolin - Linux desktop client installation script
# Usage: curl -fsSL https://static.pangolin.net/get-linux.sh | sh
#
# Detects the distribution and CPU architecture, downloads the matching
# package (deb, rpm or Arch) from the latest Linux release on GitHub and
# installs it with the system package manager (rpm-ostree on image-based
# Fedora). On other distributions (Gentoo, Void, ...) and wherever systemd is
# not the init (Devuan, Artix, ...) it installs the tar.gz with its install.sh
# instead.
#
# Options:
#   --format deb|rpm|arch|tar   Skip distribution detection.
#   --version X.Y.Z             Install this version instead of the latest.

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# GitHub repository info. Windows and Linux are released from the same repo,
# with tags prefixed by platform (linux-0.1.0, windows-0.16.0).
REPO="fosrl/windows-linux"
TAG_PREFIX="linux-"
RELEASE_API_URL="https://api.fossorial.io/api/v1/versions/linux"

# Output helpers
print_status() {
    printf '%b[INFO]%b %s\n' "${GREEN}" "${NC}" "$1"
}

print_warning() {
    printf '%b[WARN]%b %s\n' "${YELLOW}" "${NC}" "$1" >&2
}

print_error() {
    printf '%b[ERROR]%b %s\n' "${RED}" "${NC}" "$1" >&2
}

# Fetch a URL to stdout
fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$1"
    else
        print_error "Neither curl nor wget is available."
        exit 1
    fi
}

# Download a URL to a file
download() {
    if command -v curl >/dev/null 2>&1; then
        curl -fSL --progress-bar "$1" -o "$2"
    else
        wget -q --show-progress "$1" -O "$2"
    fi
}

# Fetch the latest release info (version and asset checksums) from the
# Fossorial API into RELEASE_INFO. Returns 1 if it isn't available.
RELEASE_INFO=""
fetch_release_info() {
    if command -v curl >/dev/null 2>&1; then
        RELEASE_INFO=$(curl -fsSL "$RELEASE_API_URL" 2>/dev/null || true)
    else
        RELEASE_INFO=$(wget -qO- "$RELEASE_API_URL" 2>/dev/null || true)
    fi
    RELEASE_INFO=$(printf '%s' "$RELEASE_INFO" | tr -d '\n\r')
    [ -n "$RELEASE_INFO" ]
}

# latestVersion from RELEASE_INFO, without a leading "v". Parsed without jq.
release_info_version() {
    printf '%s' "$RELEASE_INFO" |
        sed -n 's/.*"latestVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
        sed 's/^v//'
}

# Published sha256 of a release asset from RELEASE_INFO, or nothing.
release_info_digest() {
    asset_re=$(printf '%s' "$1" | sed 's/[][\.*^$/]/\\&/g')
    printf '%s' "$RELEASE_INFO" |
        sed -n "s/.*\"${asset_re}\"[[:space:]]*:[[:space:]]*\"\([0-9a-f]\{64\}\)\".*/\1/p"
}

# sha256 of a file, with whichever tool this system has.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256 >/dev/null 2>&1; then
        sha256 -q "$1"
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    else
        return 1
    fi
}

# Check a downloaded file against the checksum published for its release
# asset. Exits on a mismatch; warns and carries on when there's no published
# checksum (pinned older version, or the API was unreachable).
verify_checksum() {
    checksum_file="$1"
    checksum_asset="$2"

    checksum_expected=$(release_info_digest "$checksum_asset")
    if [ -z "$checksum_expected" ]; then
        print_warning "No published checksum for ${checksum_asset}; skipping verification."
        return 0
    fi

    checksum_actual=$(sha256_of "$checksum_file" || true)
    if [ -z "$checksum_actual" ]; then
        print_warning "No sha256 tool found (sha256sum, shasum, sha256 or openssl); skipping verification."
        return 0
    fi

    if [ "$checksum_actual" != "$checksum_expected" ]; then
        print_error "Checksum mismatch for ${checksum_asset}!"
        print_error "  expected: ${checksum_expected}"
        print_error "  got:      ${checksum_actual}"
        print_error "The download may be corrupted or tampered with. Nothing was installed."
        exit 1
    fi

    print_status "Checksum verified (sha256 ${checksum_expected})"
}

# CPU architecture, as used in the package names
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *)
            print_error "Unsupported architecture: $(uname -m). Packages are built for x86_64 and ARM64."
            exit 1
            ;;
    esac
}

# True when systemd is the running init. The deb, rpm and Arch packages only
# ship a systemd unit; the tar.gz's install.sh also handles OpenRC.
has_systemd() {
    [ -d /run/systemd/system ]
}

# True inside a container image (Docker, Podman), where there is no init at
# all and the packages are what a Dockerfile wants.
in_container() {
    [ -e /.dockerenv ] || [ -e /run/.containerenv ]
}

# True on image-based Fedora (Silverblue, Kinoite, ...), where the root
# filesystem is a read-only ostree and packages are layered with rpm-ostree.
is_ostree() {
    [ -e /run/ostree-booted ]
}

# Package format for this distribution: deb, rpm or arch. Reads
# /etc/os-release, then falls back to whichever package manager is present,
# then to the tar.gz. Without systemd (Devuan, MX, Artix, Alpine, ...) the
# packages would install a service nothing starts, so it is the tar.gz too.
detect_format() {
    if ! has_systemd && ! in_container; then
        print_warning "systemd is not running; the deb, rpm and Arch packages need it, so installing the tar.gz."
        echo "tar"
        return
    fi

    ids=""
    if [ -r /etc/os-release ]; then
        # shellcheck disable=SC1091
        ids=$(. /etc/os-release && printf '%s %s' "${ID:-}" "${ID_LIKE:-}")
    fi

    for id in $ids; do
        case "$id" in
            debian|ubuntu) echo "deb"; return ;;
            fedora|rhel|centos|rocky|almalinux|suse|opensuse*|sles) echo "rpm"; return ;;
            arch|archarm|manjaro|endeavouros|cachyos) echo "arch"; return ;;
        esac
    done

    if command -v apt-get >/dev/null 2>&1; then echo "deb"; return; fi
    if command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1 || command -v zypper >/dev/null 2>&1; then
        echo "rpm"; return
    fi
    if command -v pacman >/dev/null 2>&1; then echo "arch"; return; fi

    print_warning "No deb, rpm or Arch package for this distribution; installing the tar.gz instead."
    print_warning "To use a package, re-run with --format deb, --format rpm or --format arch."
    echo "tar"
}

# File extension for a package format
format_extension() {
    case "$1" in
        deb) echo "deb" ;;
        rpm) echo "rpm" ;;
        arch) echo "pkg.tar.zst" ;;
        tar) echo "tar.gz" ;;
        *) print_error "Unknown package format: $1 (expected deb, rpm, arch or tar)"; exit 1 ;;
    esac
}

# Command prefix to run as root: empty when already root
get_sudo_cmd() {
    if [ "$(id -u)" -eq 0 ]; then
        echo ""
    elif command -v sudo >/dev/null 2>&1; then
        echo "sudo"
    elif command -v doas >/dev/null 2>&1; then
        echo "doas"
    else
        print_error "Installing a package needs root, and neither sudo nor doas is available."
        print_error "Run this script as root or install sudo."
        exit 1
    fi
}

# Install a downloaded package file with the system package manager
install_package() {
    format="$1"
    file="$2"
    sudo_cmd="$3"

    case "$format" in
        deb)
            # apt-get treats the argument as a file because it is an
            # absolute path. It installs the dependencies too.
            $sudo_cmd apt-get install -y "$file"
            ;;
        rpm)
            if is_ostree; then
                # dnf can't write to the read-only root; layer the package.
                if ! command -v rpm-ostree >/dev/null 2>&1; then
                    print_error "This is an image-based system but rpm-ostree was not found."
                    exit 1
                fi
                $sudo_cmd rpm-ostree install --idempotent "$file"
            elif command -v dnf >/dev/null 2>&1; then
                $sudo_cmd dnf install -y "$file"
            elif command -v zypper >/dev/null 2>&1; then
                # The package isn't signed with a key zypper knows.
                $sudo_cmd zypper --non-interactive install --allow-unsigned-rpm "$file"
            elif command -v yum >/dev/null 2>&1; then
                $sudo_cmd yum install -y "$file"
            else
                print_error "No dnf, zypper or yum found to install the rpm."
                exit 1
            fi
            ;;
        arch)
            $sudo_cmd pacman -U --noconfirm --needed "$file"
            ;;
        tar)
            install_tarball "$file" "$sudo_cmd"
            ;;
    esac
}

# Extract the tar.gz next to it and run its install.sh, which puts the files
# under /usr/local and starts the service with systemd or OpenRC.
install_tarball() {
    file="$1"
    sudo_cmd="$2"

    if ldd --version 2>&1 | grep -qi musl; then
        print_warning "This system uses musl; Pangolin needs glibc 2.35 or newer and may not start."
    fi

    dir=$(dirname "$file")
    tar -xzf "$file" -C "$dir"
    installer="${dir}/$(basename "$file" .tar.gz)/install.sh"
    if [ ! -x "$installer" ]; then
        print_error "The archive has no install.sh: $file"
        exit 1
    fi
    $sudo_cmd "$installer"

    # The app needs GTK 3 and WebKitGTK 4.1, which only the packages pull in.
    libs=$( (ldconfig -p || /sbin/ldconfig -p) 2>/dev/null || true)
    if [ -n "$libs" ] && ! printf '%s' "$libs" | grep -q 'libwebkit2gtk-4\.1\.so'; then
        print_warning "WebKitGTK 4.1 was not found. Install it with your package manager"
        print_warning "(Gentoo: net-libs/webkit-gtk:4.1, Void: libwebkit2gtk41) before opening Pangolin."
    fi
}

# Parse arguments
parse_args() {
    FORMAT=""
    VERSION=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --format) FORMAT="${2:-}"; shift ;;
            --format=*) FORMAT="${1#--format=}" ;;
            --version) VERSION="${2:-}"; shift ;;
            --version=*) VERSION="${1#--version=}" ;;
            -h|--help)
                echo "Usage: get-linux.sh [--format deb|rpm|arch|tar] [--version X.Y.Z]"
                exit 0
                ;;
            *) print_warning "Ignoring unknown argument: $1" ;;
        esac
        shift
    done
}

main() {
    parse_args "$@"

    if [ "$(uname -s)" != "Linux" ]; then
        print_error "This script installs the Linux client. For other platforms see https://pangolin.net/downloads"
        exit 1
    fi

    print_status "Installing the Pangolin client for Linux..."

    ARCH=$(detect_arch)
    if [ -z "$FORMAT" ]; then
        FORMAT=$(detect_format)
    fi
    EXTENSION=$(format_extension "$FORMAT")
    if [ "$FORMAT" = tar ]; then
        print_status "Detected platform: tar.gz for ${ARCH}"
    else
        print_status "Detected platform: ${FORMAT} package for ${ARCH}"
    fi

    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        print_error "Neither curl nor wget is available."
        exit 1
    fi

    # The latest release info also carries the package checksums. Windows
    # and Linux share a repo, so there's no GitHub "latest" link to fall
    # back to when the API is unreachable.
    print_status "Fetching latest version..."
    LATEST=""
    if fetch_release_info; then
        LATEST=$(release_info_version | sed "s/^${TAG_PREFIX}//")
    fi
    if [ -z "$VERSION" ]; then
        if [ -z "$LATEST" ]; then
            print_error "Could not get the latest version from ${RELEASE_API_URL}"
            print_error "Re-run with --version X.Y.Z, or download a package from https://github.com/${REPO}/releases"
            exit 1
        fi
        VERSION="$LATEST"
    fi
    VERSION=$(printf '%s' "$VERSION" | sed "s/^${TAG_PREFIX}//; s/^v//")
    case "$VERSION" in
        ""|*[!0-9A-Za-z.+-]*)
            print_error "Invalid version: ${VERSION}"
            exit 1
            ;;
    esac
    # Checksums are only published for the latest release.
    if [ "$VERSION" != "$LATEST" ]; then
        RELEASE_INFO=""
    fi
    print_status "Version: ${VERSION}"

    SUDO_CMD=$(get_sudo_cmd)

    asset_name="pangolin-client-${ARCH}-${VERSION}.${EXTENSION}"
    download_url="https://github.com/${REPO}/releases/download/${TAG_PREFIX}${VERSION}/${asset_name}"

    # Readable by everyone so apt's unprivileged _apt user can open the file
    # instead of warning that it fell back to root.
    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "$TMP_DIR"' EXIT
    chmod 755 "$TMP_DIR"
    package_file="${TMP_DIR}/${asset_name}"

    print_status "Downloading ${download_url}"
    if ! download "$download_url" "$package_file"; then
        print_error "Download failed. Check that version ${VERSION} has a ${FORMAT} package for ${ARCH}:"
        print_error "https://github.com/${REPO}/releases/tag/${TAG_PREFIX}${VERSION}"
        exit 1
    fi
    verify_checksum "$package_file" "$asset_name"
    chmod 644 "$package_file"

    if [ -n "$SUDO_CMD" ]; then
        print_status "Root privileges are needed to install the package"
    fi
    install_package "$FORMAT" "$package_file" "$SUDO_CMD"

    if [ "$FORMAT" = rpm ] && is_ostree; then
        # The layered package only appears in the next deployment.
        print_status "Pangolin ${VERSION} is layered with rpm-ostree."
        print_status "Reboot to use it, then start the background service once with:"
        print_status "  sudo systemctl enable --now pangolin-client"
        exit 0
    fi

    if command -v pangolin-client >/dev/null 2>&1 || [ -x /usr/bin/pangolin-client ] ||
        [ -x /usr/local/bin/pangolin-client ]; then
        print_status "Pangolin ${VERSION} installed successfully!"
        print_status "Open Pangolin from your applications menu, then log in from the tray icon."
    else
        print_error "Installation failed: pangolin-client was not found after installing."
        exit 1
    fi
}

main "$@"
