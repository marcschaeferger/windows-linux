#!/bin/sh
# Tests get-linux.sh's decisions with the system checks replaced by stubs:
#   sh scripts/test-get-linux.sh
# It sources the script without running main(); nothing is downloaded or
# installed (package managers are stubs that print their command line).
# shellcheck disable=SC2329
set -u

script="$(dirname "$0")/../get-linux.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# get-linux.sh minus its final "main" call.
sed '$d' "$script" > "$tmp/lib.sh"
# shellcheck disable=SC1091
. "$tmp/lib.sh"
set +e

fail=0
check() { # name expected actual
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %s: expected "%s", got "%s"\n' "$1" "$2" "$3"
        fail=1
    fi
}

# Stub package managers that record how they were called.
mkdir "$tmp/bin"
for tool in dnf rpm-ostree apt-get; do
    printf '#!/bin/sh\necho "%s $*"\n' "$tool" > "$tmp/bin/$tool"
    chmod +x "$tmp/bin/$tool"
done
PATH="$tmp/bin:$PATH"

# Non-systemd distributions get the tar.gz, even with a Debian-like os-release.
has_systemd() { return 1; }
in_container() { return 1; }
check "no systemd -> tar" "tar" "$(detect_format 2>/dev/null)"

# A container image has no init; the package is what the image build wants.
in_container() { return 0; }
case "$(detect_format 2>/dev/null)" in
    tar) check "container without systemd keeps the package" package tar ;;
    *) check "container without systemd keeps the package" package package ;;
esac
in_container() { return 1; }

# With systemd the os-release decides; the host's own is read here, so only
# check that a package format or the tar fallback comes out.
has_systemd() { return 0; }
case "$(detect_format 2>/dev/null)" in
    deb|rpm|arch|tar) check "systemd -> a known format" ok ok ;;
    *) check "systemd -> a known format" ok bad ;;
esac

# Image-based Fedora layers the rpm instead of running dnf.
is_ostree() { return 0; }
check "ostree -> rpm-ostree" "rpm-ostree install --idempotent /tmp/x.rpm" \
    "$(install_package rpm /tmp/x.rpm "")"

# Ordinary Fedora still uses dnf.
is_ostree() { return 1; }
check "regular rpm -> dnf" "dnf install -y /tmp/x.rpm" "$(install_package rpm /tmp/x.rpm "")"

# Debian still uses apt-get.
check "deb -> apt-get" "apt-get install -y /tmp/x.deb" "$(install_package deb /tmp/x.deb "")"

exit "$fail"
