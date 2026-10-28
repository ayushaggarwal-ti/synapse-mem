#!/bin/sh
# Create (or reuse) an smem store and optionally attach sync remotes. Safe to re-run.
#
#   setup-store.sh <path> [options]
#
#   <path>                 a *.db file to use as the store, or a directory (store at <dir>/.smem/smem.db,
#                          the layout smem finds by walking up from cwd)
#   --remote NAME=DIR      add a sync remote (repeatable); DIR is any shared directory
#   --pull-only NAME=DIR   add a read-only remote, e.g. a team curator feed (repeatable)
#   --trust KEY_ID         trust a signing key so its synced events are recalled (repeatable); the key must
#                          already be known to the store, so combine with --sync to pull it first
#   --sync                 pull, apply --trust, then push every remote once after setup
#   --project-config FILE  copy FILE to <store dir>/config.json if none exists yet
#
# Uses smem from $SMEM_BIN, else PATH. Exits nonzero if any pull, sync, trust or the final doctor check fails.
set -eu

usage() { awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "$0"; exit "${1:-0}"; }
remote_spec() {
  case $1 in ?*=?*) ;; *) echo "setup-store: remote must be NAME=DIR, got $1" >&2; exit 1 ;; esac
}

smem=${SMEM_BIN:-$(command -v smem || true)}
[ -n "$smem" ] && [ -x "$smem" ] || { echo "setup-store: smem not found (run install-smem.sh or set SMEM_BIN)" >&2; exit 1; }

[ $# -ge 1 ] || usage 1
case $1 in -h|--help) usage ;; esac
target=$1; shift

remotes=""; trusts=""; do_sync=0; project_config=""
nl='
'
while [ $# -gt 0 ]; do
  case $1 in
    --remote) remote_spec "${2:-}"; remotes="$remotes${nl}rw $2"; shift 2 ;;
    --pull-only) remote_spec "${2:-}"; remotes="$remotes${nl}ro $2"; shift 2 ;;
    --trust) trusts="$trusts $2"; shift 2 ;;
    --sync) do_sync=1; shift ;;
    --project-config) project_config=$2; shift 2 ;;
    -h|--help) usage ;;
    *) echo "setup-store: unknown option $1" >&2; usage 1 ;;
  esac
done

case $target in
  *.db) db=$target ;;
  *) db=${target%/}/.smem/smem.db ;;
esac
mkdir -p "$(dirname "$db")"
db=$(cd "$(dirname "$db")" && pwd)/$(basename "$db")
store_dir=$(dirname "$db")

# --db keeps init from creating .smem/ in the caller's cwd.
"$smem" --db "$db" init >/dev/null
echo "store: $db"

if [ -n "$project_config" ] && [ ! -e "$store_dir/config.json" ]; then
  cp "$project_config" "$store_dir/config.json"
  echo "project config: $store_dir/config.json"
fi

failed=0

while read -r mode spec; do
  [ -n "${spec:-}" ] || continue
  name=${spec%%=*}; dir=${spec#*=}
  if [ "$mode" = ro ]; then
    # Not created here: a curator feed that doesn't exist yet should show up in doctor.
    "$smem" --db "$db" remote add "$name" "$dir" --pull-only
  else
    mkdir -p "$dir"
    "$smem" --db "$db" remote add "$name" "$dir"
  fi
  echo "remote: $name -> $dir$([ "$mode" = ro ] && echo ' (pull-only)')"
done <<EOF2
$remotes
EOF2

# Bare `smem sync` only covers origin, so walk every remote. A failure is recorded and the walk continues.
sync_remotes() { # $1 = pull | sync
  remote_list=$("$smem" --db "$db" remote list | grep -v '^no remotes' || true)
  while read -r name _ flag; do
    [ -n "${name:-}" ] || continue
    if [ "${flag:-}" = "(pull-only)" ]; then
      "$smem" --db "$db" pull "$name" || { echo "setup-store: pull $name failed" >&2; failed=1; }
    elif [ "$1" = sync ]; then
      "$smem" --db "$db" sync "$name" || { echo "setup-store: sync $name failed" >&2; failed=1; }
    else
      "$smem" --db "$db" pull "$name" || { echo "setup-store: pull $name failed" >&2; failed=1; }
    fi
  done <<EOF2
$remote_list
EOF2
}

# keys trust rejects a key the store hasn't seen, so pull first.
[ "$do_sync" = 1 ] && sync_remotes pull

for key in $trusts; do
  "$smem" --db "$db" keys trust "$key" || {
    echo "setup-store: could not trust $key; a new key must be pulled first (add --sync, or run \`smem pull <remote>\` and re-run with --trust)" >&2
    failed=1
  }
done

[ "$do_sync" = 1 ] && sync_remotes sync

"$smem" --db "$db" doctor || failed=1
exit "$failed"
