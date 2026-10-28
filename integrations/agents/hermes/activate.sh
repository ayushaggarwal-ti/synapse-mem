#!/bin/sh
# Install the synapse-mem MemoryProvider into a Hermes profile and make it the active memory provider.
# Safe to re-run; this repo's plugin/ is the source of truth and the installed copy is replaced from it.
#
#   activate.sh [--upgrade] [setup-store.sh options...]
#   activate.sh --uninstall
#
#   --upgrade    run install-smem.sh even if smem is already on PATH ($SMEM_VERSION picks the version)
#   --uninstall  unset memory.provider and remove the installed plugin; the store is kept
#
# Profile: $HERMES_HOME (default ~/.hermes). SMEM_HERMES_CMD overrides the hermes CLI.
# Store: store_path in <profile>/synapse-mem.json, else <profile>/synapse/smem.db.
# Remaining options (--remote, --pull-only, --trust, --sync) go to setup-store.sh.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
install_dir=$here/../../install
home=${HERMES_HOME:-$HOME/.hermes}
dest=$home/plugins/synapse-mem
hermes=${SMEM_HERMES_CMD:-hermes}
export HERMES_HOME="$home"

command -v "$hermes" >/dev/null 2>&1 || { echo "activate: hermes CLI not found (set SMEM_HERMES_CMD)" >&2; exit 1; }

if [ "${1:-}" = "--uninstall" ]; then
  if [ "$("$hermes" config get memory.provider 2>/dev/null)" = synapse-mem ]; then
    "$hermes" config unset memory.provider
  fi
  rm -rf "$dest"
  echo "synapse-mem deactivated; store left in place"
  exit 0
fi

upgrade=0
[ "${1:-}" = "--upgrade" ] && { upgrade=1; shift; }

if [ "$upgrade" = 1 ] || { [ -z "${SMEM_BIN:-}" ] && ! command -v smem >/dev/null 2>&1; }; then
  "$install_dir/install-smem.sh"
  command -v smem >/dev/null 2>&1 || { SMEM_BIN=${SMEM_INSTALL_DIR:-$HOME/.local/bin}/smem; export SMEM_BIN; }
fi

mkdir -p "$home/plugins"
if [ -d "$dest" ] && ! diff -rq -x __pycache__ "$here/plugin" "$dest" >/dev/null 2>&1; then
  backup=$dest.bak-$(date +%Y%m%d%H%M%S)
  mv "$dest" "$backup"
  echo "previous install differed from the repo; moved to $backup"
fi
rm -rf "$dest.new"
cp -R "$here/plugin" "$dest.new"
rm -rf "$dest.new/__pycache__" "$dest"
mv "$dest.new" "$dest"
echo "plugin: $here/plugin -> $dest"

store=$(python3 -c 'import json, os, sys
p = os.path.join(sys.argv[1], "synapse-mem.json")
cfg = json.load(open(p)) if os.path.exists(p) else {}
print(os.path.expanduser(cfg.get("store_path") or os.path.join(sys.argv[1], "synapse", "smem.db")))' "$home")
"$install_dir/setup-store.sh" "$store" "$@"

"$hermes" config set memory.provider synapse-mem >/dev/null
provider=$("$hermes" config get memory.provider)
[ "$provider" = synapse-mem ] || { echo "activate: memory.provider is '$provider', expected synapse-mem" >&2; exit 1; }

# `memory status` loads the provider through Hermes' own discovery and runs is_available().
status=$("$hermes" memory status 2>&1 || true)
if ! printf '%s\n' "$status" | grep -q 'Status: *available'; then
  printf '%s\n' "$status" >&2
  echo "activate: hermes does not report synapse-mem as available" >&2
  exit 1
fi
printf '%s\n' "$status" | grep -E 'Provider:|Plugin:|Status:'
echo "memory.provider: $provider"
