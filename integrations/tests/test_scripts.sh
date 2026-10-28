#!/bin/bash
# Behavior tests for the integration shell scripts against a stub smem and stub hermes (no network, no real store).
# Run: integrations/tests/test_scripts.sh
set -u

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
pass=0; fail=0
check() { # name, condition exit status
  if [ "$2" = 0 ]; then pass=$((pass + 1)); echo "PASS  $1"; else fail=$((fail + 1)); echo "FAIL  $1"; fi
}

# Stub smem. Records argv in $LOG. State in $STATE: known keys. Failures via STUB_FAIL (space-separated subcommands).
mkdir -p "$work/stub" "$work/bin-elsewhere"
cat > "$work/stub/smem" <<'STUB'
#!/bin/sh
[ "$1" = --db ] && shift 2
echo "$(basename "$0") $*" >> "$LOG"
case " ${STUB_FAIL:-} " in *" $1 "*) echo "stub: $1 failing" >&2; exit 1 ;; esac
case $1 in
  init)
    if [ "${2:-}" = --claude-code ]; then
      echo "init-claude-code cwd=$(pwd) bin=${3:-smem}" >> "$LOG"
    fi ;;
  remote)
    [ "$2" = list ] && cat "$STATE/remotes" 2>/dev/null
    if [ "$2" = add ]; then
      if [ "${4:-}" = --pull-only ]; then echo "$3 $4 (pull-only)" >> "$STATE/remotes"; else echo "$3 $4" >> "$STATE/remotes"; fi
    fi ;;
  pull) echo "key-curator" >> "$STATE/keys" ;;
  keys)
    grep -qx "$3" "$STATE/keys" 2>/dev/null || { echo "unknown key $3" >&2; exit 1; } ;;
  version) echo "smem 0.0.0" ;;
esac
exit 0
STUB
chmod +x "$work/stub/smem"
cp "$work/stub/smem" "$work/bin-elsewhere/smem"

fresh() { rm -rf "$work/state" "$work/log"; mkdir -p "$work/state"; : > "$work/log"; export LOG="$work/log" STATE="$work/state"; unset STUB_FAIL; }
logged() { grep -q -- "$1" "$work/log"; }
sysbin=/usr/bin:/bin

# --- setup-store.sh ---
run_store() { SMEM_BIN="$work/stub/smem" sh "$root/install/setup-store.sh" "$@" >"$work/out" 2>&1; }

fresh
run_store "$work/s1" --pull-only team="$work/feed" --trust key-curator --sync
check "setup-store: pull-only + trust + sync succeeds on first run" $?
pull_line=$(grep -n '^smem pull' "$work/log" | head -1 | cut -d: -f1)
trust_line=$(grep -n 'keys trust' "$work/log" | head -1 | cut -d: -f1)
[ -n "$pull_line" ] && [ -n "$trust_line" ] && [ "$pull_line" -lt "$trust_line" ]
check "setup-store: pull runs before keys trust" $?

fresh
run_store "$work/s2" --pull-only team="$work/feed" --trust key-curator
rc=$?; [ "$rc" != 0 ]
check "setup-store: trusting an unseen key without --sync fails" $?
grep -q "pulled first" "$work/out"
check "setup-store: unseen-key failure prints pull guidance" $?

fresh
run_store "$work/s3" --remote origin="$work/o1" --remote second="$work/o2" --sync; ok=$?
STUB_FAIL="sync" run_store "$work/s3" --remote origin="$work/o1" --remote second="$work/o2" --sync
rc=$?
[ "$ok" = 0 ] && [ "$rc" != 0 ]
check "setup-store: failing sync exits nonzero" $?
[ "$(grep -c '^smem sync' "$work/log")" -ge 4 ]
check "setup-store: other remotes still processed after a failure" $?

fresh
STUB_FAIL="pull" run_store "$work/s4" --pull-only team="$work/feed" --sync
[ $? != 0 ]
check "setup-store: failing pull exits nonzero" $?

fresh
STUB_FAIL="doctor" run_store "$work/s5"
[ $? != 0 ]
check "setup-store: doctor failure is preserved" $?

fresh
run_store "$work/s6"
check "setup-store: plain setup succeeds" $?

# --- setup-project.sh ---
repo="$work/repo"; mkdir -p "$repo"
run_proj() { (cd "$work" && env "$@" sh "$root/agents/claude-code/setup-project.sh" repo >"$work/out" 2>&1); }

fresh
mkdir -p "$work/rel"; cp "$work/stub/smem" "$work/rel/smem"
(cd "$work" && PATH="$work/stub:$sysbin" SMEM_BIN=rel/smem sh "$root/agents/claude-code/setup-project.sh" repo >"$work/out" 2>&1)
check "setup-project: relative SMEM_BIN resolved before cd" $?
logged "init-claude-code cwd=.*/repo bin=smem"
check "setup-project: init --claude-code ran in the repo" $?
! logged "--bin"
check "setup-project: generated config never pins an absolute binary (no --bin)" $?

fresh
run_proj PATH="$sysbin" SMEM_BIN="$work/bin-elsewhere/smem"
rc=$?; [ "$rc" != 0 ] && ! logged "init-claude-code"
check "setup-project: smem off PATH refuses and writes no config" $?
grep -q 'export PATH=' "$work/out" && grep -q "$work/bin-elsewhere" "$work/out"
check "setup-project: off-PATH message shows how to add it to PATH" $?

fresh
run_proj PATH="$work/stub:$sysbin" SMEM_BIN="$work/bin-elsewhere/smem"
rc=$?; [ "$rc" = 0 ] && grep -q "differs from smem on PATH" "$work/out" && ! logged "--bin"
check "setup-project: SMEM_BIN differing from PATH smem warns, config stays portable" $?

fresh
run_proj PATH="$work/stub:$sysbin" SMEM_BIN="$work/missing/smem"
[ $? != 0 ]
check "setup-project: nonexistent SMEM_BIN rejected" $?

# Re-running with a changed PATH installs no differently-named hook: command text is always plain `smem`.
fresh
run_proj PATH="$work/stub:$sysbin"; a=$(grep -c 'init --claude-code' "$work/log")
run_proj PATH="$work/stub:$sysbin" SMEM_BIN="$work/bin-elsewhere/smem"; b=$(grep -c 'init --claude-code' "$work/log")
[ "$a" = 1 ] && [ "$b" = 2 ] && ! logged "--bin"
check "setup-project: re-run after SMEM_BIN/PATH change uses the same bare command" $?

# --- activate.sh ---
home="$work/hh"; mkdir -p "$home"
cat > "$work/stub/hermes" <<'STUB'
#!/bin/sh
case "$1 $2" in
  "config set") echo "$4" > "$HERMES_HOME/.provider" ;;
  "config get") cat "$HERMES_HOME/.provider" ;;
  "memory status") printf 'Provider: synapse-mem\nPlugin: x\nStatus: available\n' ;;
esac
STUB
chmod +x "$work/stub/hermes"

fresh
run_act() { env HERMES_HOME="$home" SMEM_HERMES_CMD="$work/stub/hermes" "$@" sh "$root/agents/hermes/activate.sh" >"$work/out" 2>&1; }
run_act PATH="$sysbin" SMEM_BIN="$work/bin-elsewhere/smem"
check "activate: succeeds with SMEM_BIN off PATH" $?
python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["smem_bin"] == sys.argv[2] else 1)' "$home/synapse-mem.json" "$work/bin-elsewhere/smem"
check "activate: smem_bin persisted to synapse-mem.json" $?
[ "$(stat -f %Lp "$home/synapse-mem.json" 2>/dev/null || stat -c %a "$home/synapse-mem.json")" = 600 ]
check "activate: synapse-mem.json is 0600" $?
echo '{"max_prefetch": 3}' > "$home/synapse-mem.json"
run_act PATH="$sysbin" SMEM_BIN="$work/bin-elsewhere/smem"
python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); sys.exit(0 if c["max_prefetch"]==3 and "smem_bin" in c else 1)' "$home/synapse-mem.json"
check "activate: existing config keys preserved when persisting smem_bin" $?

# --- judge-gateway.example.sh under POSIX sh ---
n=$(sh -c '. "$1"; printf "%s\n" "$ANTHROPIC_CUSTOM_HEADERS" | wc -l' _ "$root/config/judge-gateway.example.sh" | tr -d ' ')
[ "$n" = 2 ]
check "judge-gateway.example.sh: two header lines under POSIX sh" $?
(command -v dash >/dev/null && dash -c '. "$1"; printf "%s\n" "$ANTHROPIC_CUSTOM_HEADERS" | wc -l' _ "$root/config/judge-gateway.example.sh" | tr -d ' ' | grep -qx 2) || ! command -v dash >/dev/null
check "judge-gateway.example.sh: two header lines under dash (if installed)" $?

echo; echo "$pass/$((pass + fail)) passed"
[ "$fail" = 0 ]
