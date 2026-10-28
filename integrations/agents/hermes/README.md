# Hermes adapter

This is the only Hermes-specific part of integrations. It is a Hermes `MemoryProvider` plugin that
puts smem behind Hermes' memory slot. It reuses the same `install/` scripts as every other agent.

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
- **Identity.** `SMEM_ACTOR=hermes-<platform>@<host>` and `SMEM_SESSION=<hermes session id>`.
- **Tools.**
  - `memory_remember`, `memory_recall`, `memory_update` (supersede, invalidate, retract, reconfirm)
  - `decision_record`, with rejected options, assumptions, used memories and evidence
  - `decision_outcome`
  - `decision_why`, for provenance of a `dec-`/`mem-`/`asm-` id, commit sha or bead id
  - `memory_review`, for suspect and at-risk records
- **Recall.** Each turn prefetches up to `max_prefetch` memories (default 10, cap 15) in the background
  with `--no-record`, so automatic injection doesn't count as use.
- **Writes are deliberate.** `sync_turn` is a no-op. The model writes through the tools.
- **Native mirror.** Adds and replaces from Hermes' built-in memory tool are mirrored into smem under
  scope `hermes-native-<target>`.
- **Session episode.** With `enable_turn_capture` on, a session of 3 or more messages ends with one
  `episode` memory: the last question and the final answer.
- **Fail-open.** A missing binary or a broken store logs a warning. Prefetch returns nothing and tools
  return error JSON. A Hermes turn never breaks.

Settings live in `<profile>/synapse-mem.json` (written by `hermes memory setup` or `save_config`):

| Key | Default | Meaning |
|---|---|---|
| `smem_bin` | `smem` on `PATH` (or `$SMEM_BIN`) | binary override |
| `store_path` | `<profile>/synapse/smem.db` | store override |
| `max_prefetch` | 10 | memories injected per turn, 0–15 |
| `enable_turn_capture` | false | store a session-end episode |

smem's own settings (scopes, decay, judges) come from `<profile>/synapse/config.json`, which is the
project config next to the store, and from the local config. See [../../config/README.md](../../config/README.md).

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
