# Hermes adapter

This is the only Hermes-specific part of integrations. It is a Hermes `MemoryProvider` plugin that
puts smem behind Hermes' memory slot. It reuses the same `install/` scripts as every other agent. It
is also the reference adapter in [../../CONTRIBUTING-agents.md](../../CONTRIBUTING-agents.md).

```
plugin/__init__.py     SynapseMemProvider: shells out to the smem CLI and imports only Hermes' plugin API
plugin/plugin.yaml     manifest; kind: exclusive keeps the general PluginManager from eager-loading it
activate.sh            installer: smem binary → plugin copy → store → memory.provider → verify
tests/test_provider_e2e.py   real-CLI tests against this directory's plugin (no mocks)
```

## Install

```sh
./activate.sh                 # uses $HERMES_HOME, default ~/.hermes
```

1. Runs `install/install-smem.sh` if `smem` isn't on `PATH`. Pass `--upgrade` to always run it; set
   `SMEM_VERSION` to pin a version.
2. Copies `plugin/` to `<profile>/plugins/synapse-mem/`. This repo is the source of truth. If the installed
   copy has drifted, it is moved to `synapse-mem.bak-<timestamp>` first.
3. Runs `install/setup-store.sh` on the profile store: `store_path` from `<profile>/synapse-mem.json`, or
   `<profile>/synapse/smem.db`. Any extra arguments go to `setup-store.sh`:
   ```sh
   ./activate.sh --remote origin=/Volumes/shared/smem --sync
   ```
4. Runs `hermes config set memory.provider synapse-mem`.
5. Verifies with `hermes memory status`. This loads the plugin through Hermes' own discovery and calls
   `is_available()`, and the script exits non-zero unless the plugin is reported available.

Re-running is safe. To undo it, run `./activate.sh --uninstall`. That unsets `memory.provider` and removes
the installed plugin, but keeps the store.

## How the provider works

- **One store per profile**, at `<profile>/synapse/smem.db`. It is passed to every smem call as `SMEM_DB`,
  so it doesn't depend on the session's cwd.
- **Identity.** `SMEM_ACTOR=hermes-<platform>@<host>` and `SMEM_SESSION=<hermes session id>`. The actor
  is always set, so agent writes are never signed with a person's `SMEM_ACTOR` inherited from the shell.
- **Tools.**
  - `memory_remember`, `memory_recall`, `memory_update` (supersede, invalidate, retract, reconfirm)
  - `decision_record`, with rejected options, assumptions, used memories and evidence. It returns a
    `commit_trailer` (`Decision: dec-…`), and the system prompt tells the model to put it in commit
    messages.
  - `decision_outcome`
  - `decision_why`, for provenance of a `dec-`/`mem-`/`asm-` id, commit sha or bead id. It runs in the
    session's working directory, so a sha resolves through git, including `Decision:` trailers.
  - `memory_review`, for suspect and at-risk records
- **Session-start context.** The first prefetch of a new session runs `smem hook session-start`, and so
  does the first prefetch after `/new` or `/reset`. That is the same context Claude Code's SessionStart
  hook injects: salient memories within `context.budget`, filtered by `scopes.read` and `context.kinds`,
  plus suspect, rule and quarantine notes. Compression passes `compact`, and resume and branch pass
  `resume`. smem's `context.on` decides whether those inject (by default they don't). The context is
  logged as shown, not used. Hermes skips prefetch for trivial prompts like "hi", so the context arrives
  with the first real message.
- **Recall.** Each turn prefetches up to `max_prefetch` memories (default 10, cap 15) in the background
  with `--no-record`, so automatic injection doesn't count as use. Memories the session context already
  showed aren't repeated.
- **Tool calls.** The plugin registers a `post_tool_call` hook. Each mutating tool call is piped to
  `smem hook post-tool-use`, which redacts it and writes it to the store's spool in about 10 ms. The next
  smem call records it as a signed action, linked to the session's decision when that is unambiguous.
  Read-only tools, the built-in `memory` tool (already mirrored) and this provider's own tools are
  skipped. smem extracts commit shas only from Claude Code's `Bash` tool, so `terminal` calls are sent
  under that name. A `git commit` then lets `decision_why <sha>` reach the decision.
- **Writes are deliberate.** `sync_turn` is a no-op. The model writes through the tools.
- **Native mirror.** Adds and replaces from Hermes' built-in memory tool are mirrored into smem under
  scope `hermes-native-<target>`.
- **Session episode.** With `enable_turn_capture` on, a session of 3 or more messages ends with one
  `episode` memory: the last question and the final answer.
- **Fail-open.** A missing binary or a broken store logs a warning. Prefetch returns nothing, tools
  return error JSON, and tool-call capture is skipped. A Hermes turn never breaks.

Settings live in `<profile>/synapse-mem.json` (written by `hermes memory setup` or `save_config`):

| Key | Default | Meaning |
|---|---|---|
| `smem_bin` | `smem` on `PATH` (or `$SMEM_BIN`) | binary override |
| `store_path` | `<profile>/synapse/smem.db` | store override |
| `max_prefetch` | 10 | memories injected per turn, 0–15; 0 also turns off session-start context |
| `session_context` | true | inject smem's session-start context on the first turn |
| `capture_tool_calls` | true | record mutating tool calls as signed actions |
| `enable_turn_capture` | false | store a session-end episode |

smem's own settings (scopes, decay, judges) come from `<profile>/synapse/config.json`, which is the
project config next to the store, and from the local config. See [../../config/README.md](../../config/README.md).

## Parity with smem's Claude Code wiring

The reference is the plugin in `plugin/` at the repo root, README "Claude Code hooks", and DESIGN.md §6
"Capture without relying on discipline".

| Behavior | Hermes |
|---|---|
| Session-start context (budget, `scopes.read`, `context.kinds`, `context.on`, shown ≠ used) | parity, through the same `smem hook session-start`, injected on the first prefetch |
| Mutating tool calls spooled, redacted, linked to decisions, commit shas extracted | parity, through the same `smem hook post-tool-use`, from a `post_tool_call` hook |
| `Decision:` commit trailer, and `why <sha>` | parity: `commit_trailer` from `decision_record`; `decision_why` runs in the session cwd |
| Session binding | `SMEM_SESSION` set directly. Ancestor-pid lookup isn't needed because smem runs as a child process, not a separately spawned MCP server |
| Actor | `hermes-<platform>@<host>`, the same `<client>@<short host>` shape as `claude-code@host` |
| Redaction | smem redacts every payload before signing or spooling; the provider writes no memory or tool content to disk itself |

Known limitations:
- **Per-turn recall isn't in decision snapshots.** smem has no CLI flag to log a recall as injected
  without reinforcing it, so per-turn recall uses `--no-record`. A decision's snapshot ("memories in
  view") is the session's latest logged recall. That is the session-start context or the model's own
  `memory_recall`, never a per-turn injection.
- **Tool surface.** The provider exposes 7 tools. `smem mcp` has 14. Missing are `memory_signal`,
  `memory_context`, `failure_record`, `rule_propose`, `rules_list` and `factcheck`, plus `derived_from`
  on remember and `bead`/`supersedes` on decisions. Use the CLI or an `smem mcp` entry for those.
- **Content on argv.** The CLI takes memory and decision text as arguments, so it is visible in the
  process list while each call runs. It is redacted before it is stored. Over MCP it would travel on
  stdin.
- **Gateway processes.** The hook is process-wide and routes each call by its session id. Calls from
  sessions without a provider, such as subagents, go to the most recently initialized provider, so they
  carry that provider's platform in the actor.

## Test

```sh
~/.hermes/hermes-agent/venv/bin/python tests/test_provider_e2e.py
```

The test runs against a throwaway `HERMES_HOME` whose `plugins/synapse-mem` is a symlink to this
directory's `plugin/`. `HERMES_AGENT_DIR` overrides the hermes-agent checkout, which defaults to
`~/.hermes/hermes-agent`.

## Overrides

| Variable | Meaning |
|---|---|
| `HERMES_HOME` | target profile |
| `SMEM_HERMES_CMD` | `hermes` CLI to call |
| `SMEM_BIN`, `SMEM_INSTALL_DIR`, `SMEM_VERSION` | passed through to the generic install scripts |
