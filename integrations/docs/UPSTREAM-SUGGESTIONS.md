# Upstream suggestions for smem

These are changes to smem itself (`rahulsub/synapse-mem`) that would remove workarounds in our adapters.
None of them are sent yet. Each one is meant to become an issue or PR upstream.

Line numbers are from smem 0.4.0 as of d39a63a, the last commit that touched `smem/`, `cmd/` or `mcp/`.

## 1. `recall --injected`: log a recall as shown without reinforcing it

**Problem.** An adapter that injects memories every turn can't make them show up in decision snapshots.
A decision's "memories in view" is whatever the session's latest recall event returned. The CLI gives
two choices for a recall: record it and reinforce every hit, or record nothing. Neither fits an
automatic injection.

**Current behavior.**
- `cmd/smem/main.go:641` defines `--no-record`. There is no flag for an injected recall.
- `smem/recall.go:64-68`: `Recall` writes `recordRecall(q.Text, hits, false)` unless `NoRecord` is set.
  `injected` is always `false` on this path.
- `smem/recall.go:72-85`: `recordRecall` already takes `injected` and stores it in the payload
  (`smem/apply.go:48-52`). `smem/recall.go:107` keeps injected and looked-up recalls apart when it
  dedupes.
- `smem/apply.go:256-264`: replay reinforces each hit (`a.access(..., "retrieved")`) only when
  `!p.Injected`.
- `smem/recall.go:486`: `Context` (the session-start hook) is the only caller that passes
  `injected=true`, through `!pol.Reinforce`.
- `smem/decision.go:149-154` and `:167-189`: when `decide` is not given `--context`, the snapshot is
  `lastRecall()`, the session's latest recall event, whether it was injected or not.

So the mechanism exists. Only `Context` can reach it.

**Proposed change.** Add `Injected bool` to `RecallQuery`. Pass it through as
`e.recordRecall(q.Text, hits, q.Injected)` in `Recall`, and expose it as `smem recall --injected`. It
records the recall and skips reinforcement, the same as session-start context. `--injected` together
with `--no-record` is an error.

**How we'd use it.** Hermes' per-turn prefetch (`integrations/agents/hermes/plugin/__init__.py`,
`_start_prefetch`) would switch from `--no-record` to `--injected`. A decision made in that turn would
then list the memories the model was shown. A recall the model makes itself later in the turn still
comes after the injection, so it remains the snapshot. Any adapter that injects context without a
session-start hook needs the same thing.

## 2. `commitFrom`: get commit shas from any shell tool, not only `Bash`

**Problem.** smem links a tool call to a commit only when the tool is named `Bash`. Other agents' shell
tools, such as Hermes' `terminal` and Codex's `shell`, get no commit linkage, so `why <sha>` can't find
the action.

**Current behavior.**
- `smem/hook.go:250-266`: `commitFrom` returns `""` unless `tool == "Bash"` and the input contains
  `git commit`. After that it reads only a `stdout` field (`:256-261`), then falls back to the raw
  response, and matches `commitLine` (`:43`).
- `smem/hook.go:77-81`: `capture` stores `in.ToolName` as the action's tool, alongside the commit it
  found.
- `smem/hook.go:38-41`: `readOnlyTools` is Claude Code names only. `hooks.skip_tools`
  (`smem/config.go:142-143`) already makes skipping configurable. Shell-tool names aren't
  configurable.

**Proposed change.** Pick one:
- (a) Drop the tool-name check. Look for a sha whenever the input contains `git commit` and the output
  matches `commitLine`. That match is already strict (`[branch sha]`).
- (b) Add `hooks.shell_tools`, a list of globs with default `["Bash"]`, next to `skip_tools`.

Either way, also read an `output` field next to `stdout`, since Hermes' `terminal` returns
`{"output": ..., "exit_code": ...}`.

**How we'd use it.** Today the adapter renames `terminal` to `Bash` before calling the hook
(`_HOOK_TOOL_NAMES` in the Hermes plugin). That works, but every Hermes shell action is then signed in
the audit log as `Bash`, a tool Hermes doesn't have. With (a) or (b) we would drop the rename and
record the real tool name.

## 3. Read content from stdin for `remember`, `supersede`, `decide`, `failure` and `rules propose`

**Problem.** A native adapter has to put memory text, decision rationale, failure summaries and rule
text on the command line. Any local user can read it with `ps` while the call runs. smem redacts the text
before storing it, but argv is never redacted. Over MCP the same content travels on stdin.

**Current behavior.** We tested this live with smem 0.4.0: `echo x | smem remember --kind fact -` stores
a memory whose content is the literal `"-"`, and `smem remember` with no argument fails with a usage
error.
- `cmd/smem/main.go:509-529` (`remember`) and `:531-560` (`supersede`): content is
  `strings.Join(pos, " ")` from positional args only.
- `cmd/smem/main.go:726-763` (`decide`): the rationale comes only from `--why`
  (`:733`), and the question, choice, rejects and assumptions only from flags.
- `cmd/smem/main.go:876-893` (`action`): the summary comes only from `--summary` (`:880`).
- `cmd/smem/main.go:1428-1446` (`failure`) and `:1515-1536` (`rules propose`): the text comes only from
  positional args.
- Precedents already exist: `import -` reads `c.in` (`cmd/smem/main.go:1214-1225`), and `hook` reads
  its JSON from stdin (`:205`).

**Proposed change.** Treat a lone `-` content argument as "read from stdin" (`remember -`,
`supersede <mem> -`, `failure <label> -`, `rules propose -`). For `decide` and `action`, accept
`--input -`: a JSON object on stdin with the same fields as the MCP `decision_record` arguments, or
`smem.ActionInput`. That would also let adapters skip the `"option|reason"` pipe encoding of
`--reject` and `--assume`. Today `-` is stored literally, which is never what anyone wants, so the
change is effectively backward compatible.

**How we'd use it.** The Hermes provider already pipes JSON to `smem hook` through `_run(..., input=)`.
`remember`, `supersede`, `decide`, `failure` and `rules propose` would move to stdin, and only ids and
enum flags would stay on argv. Until this lands, the other way to keep content off argv is to talk to a
long-lived `smem mcp --session <id>` over stdio. That would mean running an MCP client inside the
provider, which is why we haven't done it.
