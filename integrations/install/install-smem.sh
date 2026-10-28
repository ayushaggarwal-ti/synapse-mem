#!/bin/sh
# Install the smem binary from a GitHub release, checksum-verified.
# macOS and Linux, arm64 and amd64. Re-running with the same version is a no-op.
#
#   install-smem.sh [--force] [version] [dir]
#
#   version  vX.Y.Z or "latest" (default: $SMEM_VERSION, else latest)
#   dir      install dir (default: $SMEM_INSTALL_DIR, else ~/.local/bin)
#
# GH_TOKEN or GITHUB_TOKEN is optional. It is sent only to github.com, in a header file, never on argv.
set -eu

repo=${SMEM_REPO:-rahulsub/synapse-mem}
force=0
[ "${1:-}" = "--force" ] && { force=1; shift; }
version=${1:-${SMEM_VERSION:-latest}}
dir=${2:-${SMEM_INSTALL_DIR:-$HOME/.local/bin}}
token=${GH_TOKEN:-${GITHUB_TOKEN:-}}

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "install-smem: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "install-smem: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
umask 077
: > "$tmp/auth"
[ -n "$token" ] && printf 'Authorization: Bearer %s\n' "$token" > "$tmp/auth"
get() { curl -fsSL --retry 3 --connect-timeout 15 --max-time 300 -H @"$tmp/auth" "$@"; }

if [ "$version" = latest ]; then
  # The /releases/latest redirect names the tag without spending API rate limit.
  version=$(curl -fsSI --retry 3 --connect-timeout 15 "https://github.com/$repo/releases/latest" |
    tr -d '\r' | sed -n 's|^[Ll]ocation: .*/releases/tag/\(.*\)$|\1|p' | tail -1)
  [ -n "$version" ] || { echo "install-smem: could not resolve the latest release of $repo" >&2; exit 1; }
fi
case $version in v*) ;; *) version=v$version ;; esac
num=${version#v}

if [ "$force" = 0 ] && [ -x "$dir/smem" ] && [ "$("$dir/smem" version 2>/dev/null)" = "smem $num" ]; then
  echo "smem $num already installed at $dir/smem"
  exit 0
fi

archive=smem_${num}_${os}_${arch}.tar.gz
base=https://github.com/$repo/releases/download/$version
get -o "$tmp/$archive" "$base/$archive"
get -o "$tmp/checksums.txt" "$base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || { echo "install-smem: $archive not listed in checksums.txt" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
fi
[ "$want" = "$got" ] || { echo "install-smem: checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" smem
mkdir -p "$dir"
# Install via a temp name + rename so a running smem is never left half-written.
install -m 0755 "$tmp/smem" "$dir/.smem.new"
mv -f "$dir/.smem.new" "$dir/smem"
echo "installed $("$dir/smem" version) to $dir/smem (sha256 $got)"
case :$PATH: in *:"$dir":*) ;; *) echo "note: $dir is not on PATH" >&2 ;; esac
