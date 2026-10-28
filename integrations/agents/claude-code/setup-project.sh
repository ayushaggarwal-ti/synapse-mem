#!/bin/sh
# Wire smem into one repo for Claude Code: store at <repo>/.smem/, MCP server in .mcp.json,
# SessionStart + PostToolUse hooks in .claude/settings.json. Existing entries are kept; safe to re-run.
#
#   setup-project.sh [repo-dir] [--project-config FILE] [setup-store.sh options...]
#
# repo-dir defaults to the current directory. Extra options (--remote, --pull-only, --trust, --sync)
# are passed to install/setup-store.sh. Installs smem first if it isn't on PATH.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
install_dir=$here/../../install

repo=.
case ${1:-} in ""|-*) ;; *) repo=$1; shift ;; esac
[ -d "$repo" ] || { echo "setup-project: $repo is not a directory" >&2; exit 1; }
repo=$(cd "$repo" && pwd)

if [ -z "${SMEM_BIN:-}" ] && ! command -v smem >/dev/null 2>&1; then
  "$install_dir/install-smem.sh"
  SMEM_BIN=${SMEM_INSTALL_DIR:-$HOME/.local/bin}/smem
  export SMEM_BIN
fi
smem=${SMEM_BIN:-$(command -v smem)}

"$install_dir/setup-store.sh" "$repo" "$@"
# Hooks and .mcp.json call plain `smem` unless told otherwise; pin the absolute path when it isn't on PATH.
if command -v smem >/dev/null 2>&1; then
  (cd "$repo" && "$smem" init --claude-code)
else
  (cd "$repo" && "$smem" init --claude-code --bin "$smem")
fi
echo "done: commit .mcp.json, .claude/settings.json and .smem/.gitignore (+ .smem/config.json if any); .smem/smem.db stays local"
