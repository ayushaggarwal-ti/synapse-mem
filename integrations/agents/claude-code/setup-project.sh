#!/bin/sh
# Wire smem into one repo for Claude Code: store at <repo>/.smem/, MCP server in .mcp.json,
# SessionStart + PostToolUse hooks in .claude/settings.json. Existing entries are kept; safe to re-run.
#
#   setup-project.sh [repo-dir] [--project-config FILE] [setup-store.sh options...]
#
# repo-dir defaults to the current directory. Extra options (--remote, --pull-only, --trust, --sync)
# are passed to install/setup-store.sh. Installs smem first if it isn't on PATH.
#
# The generated config runs plain `smem`, so smem must be on PATH for everyone who uses the repo.
# $SMEM_BIN (absolute or relative) picks the binary used here; it is not written into the config.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
install_dir=$here/../../install

repo=.
case ${1:-} in ""|-*) ;; *) repo=$1; shift ;; esac
[ -d "$repo" ] || { echo "setup-project: $repo is not a directory" >&2; exit 1; }
repo=$(cd "$repo" && pwd)

# Resolve now: later steps cd into the repo, which would break a relative SMEM_BIN.
if [ -n "${SMEM_BIN:-}" ]; then
  case $SMEM_BIN in
    */*) SMEM_BIN=$(cd "$(dirname "$SMEM_BIN")" && pwd)/$(basename "$SMEM_BIN") ;;
    *) SMEM_BIN=$(command -v "$SMEM_BIN" || true) ;;
  esac
  [ -n "$SMEM_BIN" ] && [ -x "$SMEM_BIN" ] || { echo "setup-project: SMEM_BIN is not an executable" >&2; exit 1; }
  export SMEM_BIN
elif ! command -v smem >/dev/null 2>&1; then
  "$install_dir/install-smem.sh"
  SMEM_BIN=${SMEM_INSTALL_DIR:-$HOME/.local/bin}/smem
  export SMEM_BIN
fi

# Generated .mcp.json and hooks are committed and shared, so they run plain `smem` (no machine-specific path).
if ! command -v smem >/dev/null 2>&1; then
  bindir=$(dirname "$SMEM_BIN")
  cat >&2 <<EOF2
setup-project: smem is not on PATH, and the generated config runs plain \`smem\`.
Add its directory to PATH, then re-run:
  export PATH="$bindir:\$PATH"                      # this shell
  echo 'export PATH="$bindir:\$PATH"' >> ~/.profile  # persistent (or your shell's rc file)
EOF2
  exit 1
fi
smem=${SMEM_BIN:-$(command -v smem)}
if [ "$smem" != "$(command -v smem)" ]; then
  echo "setup-project: warning: SMEM_BIN ($smem) differs from smem on PATH ($(command -v smem)); the generated config runs the PATH one" >&2
fi

"$install_dir/setup-store.sh" "$repo" "$@"
(cd "$repo" && "$smem" init --claude-code)
echo "done: commit .mcp.json, .claude/settings.json and .smem/.gitignore (+ .smem/config.json if any); .smem/smem.db stays local"
