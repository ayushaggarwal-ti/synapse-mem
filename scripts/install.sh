#!/bin/sh
# Installs the smem binary from a GitHub release of rahulsub/synapse-mem
# and verifies its checksum. GH_TOKEN or GITHUB_TOKEN is optional; it
# raises GitHub's API rate limit.
#
#   sh install.sh [version] [dir]
#
# version defaults to the latest release; dir to $SMEM_INSTALL_DIR or
# ~/.local/bin. On agents with an ephemeral home, pick a persistent dir.
set -eu
repo=rahulsub/synapse-mem
version=${1:-latest}
dir=${2:-${SMEM_INSTALL_DIR:-$HOME/.local/bin}}
token=${GH_TOKEN:-${GITHUB_TOKEN:-}}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
# The token goes in a private header file, never on a command line where
# other users could read it with ps.
umask 077
: > "$tmp/auth"
[ -n "$token" ] && printf 'Authorization: Bearer %s\n' "$token" > "$tmp/auth"
get() { curl -fsSL --retry 3 --connect-timeout 15 --max-time 300 -H @"$tmp/auth" "$@"; }

api=https://api.github.com/repos/$repo/releases
if [ "$version" = latest ]; then url=$api/latest; else url=$api/tags/$version; fi
release=$(get -H "Accept: application/vnd.github+json" "$url")

asset_url() { # name -> API url of the asset (each asset lists its url before its name)
  printf '%s' "$release" | grep -oE '"url": *"[^"]*/releases/assets/[0-9]+"|"name": *"[^"]*"' |
    awk -v want="$1" -F'"' '$2 == "url" { u = $4 } $2 == "name" && $4 == want { print u; exit }'
}
tag=$(printf '%s' "$release" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/')
archive=smem_${tag}_${os}_${arch}.tar.gz
a=$(asset_url "$archive"); c=$(asset_url checksums.txt)
[ -n "$a" ] || { echo "no $archive in release $tag" >&2; exit 1; }

get -H "Accept: application/octet-stream" -o "$tmp/$archive" "$a"
get -H "Accept: application/octet-stream" -o "$tmp/checksums.txt" "$c"
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" smem
mkdir -p "$dir"
install -m 0755 "$tmp/smem" "$dir/smem"
echo "installed $("$dir/smem" version) to $dir/smem"
