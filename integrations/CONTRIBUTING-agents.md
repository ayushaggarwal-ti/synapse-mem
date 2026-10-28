# Wiring a new agent to smem

There are two levels of integration.

1. **Tools only.** Point the agent at `smem mcp` ([agents/mcp/](agents/mcp/)) and paste
   [agents/mcp/INSTRUCTIONS-template.md](agents/mcp/INSTRUCTIONS-template.md) into its rules. This takes
   minutes, and the agent records what it remembers to record.
2. **A full adapter.** The agent's own lifecycle feeds smem. Context arrives at session start, and every
   mutating action is logged whether or not the model remembers to call a tool. smem's Claude Code plugin
   (`plugin/` at the repo root) and the Hermes provider ([agents/hermes/](agents/hermes/)) are full
   adapters.

Use the checklist below for a full adapter. Each item names the smem surface to call and where the
Hermes adapter implements it (`agents/hermes/plugin/__init__.py`). Shell out to the `smem` CLI, or speak
MCP to `smem mcp`. Never write to the store's SQLite file directly: every write is a signed, redacted
event.

## Checklist

### 1. Find the binary and the store
- Binary: a configured path, then `$SMEM_BIN`, then `smem` on `PATH`. If none is found, the adapter is
  inactive. It is not an error.
- Store: decide whether it is per project (`.smem/smem.db`, found by walking up from the working
  directory) or per agent profile (a fixed path). Pass it to every call as `SMEM_DB` or `--db`, so it
  doesn't depend on the process's working directory. Create it on first use with
  `smem --db <path> init`.
- Hermes: `_resolve_bin`, `_store_path`, `SynapseMemProvider.is_available`, `SynapseMemProvider.initialize`.

### 2. Inject context at session start, within a budget
- Pipe `{"session_id": ..., "source": "startup"|"resume"|"clear"|"compact"}` to `smem hook session-start`
  and put its stdout in front of the model once.
- smem applies `context.budget`, `context.kinds`, `scopes.read` and `context.on`. By default it injects on
  startup and clear, not on resume or compact. It logs the injection as shown, not used. It also binds the
  session id and records spooled tool calls.
- If the agent has no session-start hook, inject on the first turn. With tools only, call `memory_context`.
- Hermes: `_arm_context` (armed by `initialize` and `on_session_switch`), `_take_context`, consumed in
  `prefetch` on the first turn.

### 3. Recall in the background on each turn
- Run `smem recall --json --no-record --limit N -- <user message>` while the agent prepares the turn, and
  inject the results under a header that says they are stored records, not instructions. `--no-record`
  keeps automatic injection from counting as use.
- Skip trivial prompts. Bound the wait, and drop the results rather than delay the turn. Don't repeat
  memories the session-start context already showed.
- Hermes: `_start_prefetch` / `queue_prefetch` (background), `prefetch` (bounded join and merge),
  `recall_status`.

### 4. Capture actions on mutating tool calls
- After each tool call, pipe a Claude Code-shaped JSON to `smem hook post-tool-use`: `session_id`,
  `tool_name`, `tool_input`, `tool_response`. smem redacts the summary, hashes input and output, writes a
  spool file in about 10 ms without touching the database, and links the call to the session's decision
  when that link is unambiguous.
- Skip read-only tools and the adapter's own smem tools. smem extracts commit shas only from a tool named
  `Bash`, so send the agent's shell tool under that name. Put a `file_path` key in `tool_input`, because
  smem summarizes by it.
- Hermes: `register` (`ctx.register_hook("post_tool_call", ...)`), `_on_post_tool_call` (routes by session
  id), `SynapseMemProvider.capture_tool_call`, `_SKIP_TOOLS`, `_HOOK_TOOL_NAMES`.

### 5. Capture the session at its end (optional)
- If the agent has a session-end event, store at most one `episode` memory with `volatility: volatile`.
  It should be the question and the conclusion, never the transcript. Keep it opt-in.
- Write it synchronously. Session end is usually followed by process exit, which kills background
  writers.
- Hermes: `on_session_end` (behind `enable_turn_capture`).

### 6. Expose the memory and decision tools
- Give the model at least remember, recall, update or invalidate, `decision_record`, `decision_outcome`
  and why. Over MCP these come from `smem mcp`. A native adapter wraps the CLI: `remember`,
  `recall --json`, `supersede`, `invalidate`, `retract`, `reconfirm`, `decide`, `outcome`, `why --json`,
  `review --json`.
- Return `Decision: dec-...` as a commit trailer from `decision_record`, and tell the model to put it in
  commit messages. Run `why` in the session's working directory, so a commit sha resolves through git.
- Add short usage rules to the system prompt. See
  [INSTRUCTIONS-template.md](agents/mcp/INSTRUCTIONS-template.md).
- For the full MCP surface, also wrap `signal`, `context`, `failure`, `rules propose`, `rules list --json`
  and `factcheck --json`. Some CLI details are easy to miss:
  - smem resumes flag parsing after each positional argument, so put `--` before every one
    (`failure -- <label> -- <summary>`). Otherwise text that starts with `-` is read as a flag.
  - `rules propose` and `failure` print text, not JSON. Parse the `<rule> admitted: …` and
    `<rule> held (proposed, not active): …` lines.
  - `factcheck --due` exits non-zero when nothing is due. Without judges, results come back with
    `"not_judged": true` and exit 0.
- Hermes: `TOOL_SCHEMAS`, `_PROMPT_BLOCK`, `handle_tool_call`, `_tool_decide`, `_tool_why`, `_positional`,
  `_tool_rule_propose`, `_tool_factcheck`.

### 7. Fail open on every path
- A missing binary, a broken store, a timeout, or a non-zero exit is logged and becomes empty context or
  a JSON tool error. Never raise into the agent's turn. `smem hook` commands exit 0 even on failure and
  report on stderr, so log stderr.
- Give every call a timeout. Hermes uses 5 s for reads and hooks, 10 s for writes, 2 s for the
  first-turn context, and 120 s for `factcheck`, whose judges are model calls.
- Hermes: `SynapseMemProvider._run`, the `try` blocks in `prefetch`, `capture_tool_call`,
  `handle_tool_call` and `on_session_end`, and the fail-open checks in `tests/test_provider_e2e.py`.

### 8. Identity: actor and session per machine
- Set `SMEM_ACTOR=<agent>-<variant>@<short hostname>`, for example `claude-code@host` or
  `hermes-telegram@host`. smem signs every event with that actor's key. The actor should name the agent,
  not inherit a person's name from the shell.
- Set `SMEM_SESSION` to the agent's session id, and update it when the agent rotates sessions. Decisions,
  recalls and actions are grouped and linked by session.
- Hermes: `SynapseMemProvider.initialize` (env), `on_session_switch`.

## Verify

- `smem doctor` checks the store, keys, config, the spool and remotes.
- After a session: `smem log --limit 20` should show an `injected` recall, `action.recorded` events for
  mutating calls signed by your actor, and the model's decisions. Then `smem why <commit sha>` should
  reach the decision.
- Write the tests against the real CLI with no mocks, like `agents/hermes/tests/test_provider_e2e.py`.
