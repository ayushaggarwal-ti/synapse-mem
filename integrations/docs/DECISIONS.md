# Decisions

## 1. Memory is a set of event-sourced beliefs, not a transcript store

Most agent memory stores text and retrieves it by similarity. Nothing in such a store ever becomes false.
Last month's port number and this month's both get recalled with equal confidence. When the agent
acts on the wrong one, nothing records which memory it relied on.

smem models memory as beliefs derived from an append-only event log:

- **A memory has a status and a confidence.** It can be superseded (a new version), invalidated (it
  stopped being true) or retracted (it was never true). Confidence decays with real time, and salience
  decays with disuse. Fading memories are archived by `sweep`, not deleted.
- **Two timelines.** "When was this true?" and "when did we know it?" are separate, so
  `recall --as-of … --known-at …` can reconstruct what an agent believed when it made a call.
- **Decisions carry their reasons.** A decision records what was chosen, what was rejected and why, the
  assumptions it depends on and the memories it used. When a used memory changes, the decision turns
  *suspect* and shows up in `review`.
- **The log is the truth.** Every write is a signed, hash-chained event. All tables are projections
  that `rebuild` can replay. Sync merges logs, so replicas never conflict. Audit is `verify` plus
  `why`.

**Consequence for integrations: writes should be deliberate.** The Hermes adapter doesn't write every
turn (`sync_turn` is a no-op). The model writes through tools, and capturing tool calls is left to hooks
such as Claude Code's PostToolUse.

## 2. Why we adopted smem

- **Proven in production.** smem is fleet-proven in the CEO's agent fleet at $1M/mo scale. We get its
  failure modes already found and fixed.
- **One binary.** It's embedded SQLite, needs no server and makes no network calls in the core. It
  works on ephemeral agent hosts as long as the store and keys sit on persistent storage.
- **Provenance covers more than memory.** `why` resolves commit shas (through `Decision:` trailers)
  and bead ids too. That fits how our fleet already tracks work.
- **Safe by construction for shared repos.**
  - The project/local config split means a cloned repo can't run commands, redirect credentials or
    pick whose memories you trust.
  - Secrets are redacted before anything is signed.
  - Synced events from unknown keys are quarantined.
- **Rules need a reason.** Agent rules are admitted only when a person asked, an incident recurred or
  a decision requires them. This addresses the CLAUDE.md rule sprawl we otherwise accumulate.

## 3. Shape of this folder

- **Generic first, one adapter.** Install, store setup, config, sync and the Claude Code and MCP wiring
  work for anyone. Everything that knows about Hermes is in `agents/hermes/`, and that adapter calls
  the same `install/` scripts rather than re-implementing them.
- **Repo-shaped.** The scripts find each other by relative path and reference nothing outside this
  folder, so it can be split into its own repository unchanged.
- **The plugin source lives here.** It is not in `~/.hermes/plugins/`. That directory is an install
  target that `activate.sh` overwrites from `agents/hermes/plugin/`, keeping a backup if the installed
  copy drifted. There is one source of truth.
- **Judge through a gateway is the worked example.** Many of our agents reach Claude only through a
  header-authenticated gateway with prefixed model ids. The example is written for any gateway, not
  ours specifically.

## 4. Migrating from the Hermes native store

Hermes' built-in memory is two files per profile, `memories/MEMORY.md` and `memories/USER.md`. Entries are
separated by a line containing only `§` (`"\n§\n"`).

- **No cut-over.** Activating synapse-mem leaves the native store, its injection and its `memory` tool
  in place. Nothing is deleted or rewritten.
- **New native writes are mirrored.** The provider's `on_memory_write` copies every add or replace from
  the native tool into smem, as `kind=fact` with `scope=hermes-native-memory` or `hermes-native-user`.
  Mirrored memories are easy to find, and easy to retract in bulk, by scope.
- **Existing entries are not imported automatically.** To back-fill them once:
  ```sh
  export SMEM_DB=~/.hermes/synapse/smem.db SMEM_ACTOR=hermes-migration
  for target in memory user; do
    f=~/.hermes/memories/$(echo $target | tr a-z A-Z).md
    python3 -c 'import sys; [print(e.strip().replace("\n", " ")) for e in open(sys.argv[1]).read().split("\n§\n") if e.strip()]' "$f" |
      while IFS= read -r entry; do smem remember --kind fact --scope "hermes-native-$target" -- "$entry" >/dev/null; done
  done
  smem recall --scope hermes-native-memory --limit 100 --no-record   # and hermes-native-user
  ```
  Then review the result. Native entries often hold preferences or procedures, so re-kind them with
  `smem supersede <mem> "…" --kind preference`, and retract stale ones instead of keeping them.
- **Retiring the native store later** is a separate decision. When smem recall is trusted, disable native
  injection in the Hermes memory config. Keep the files as an archive.
- **Rollback.** `agents/hermes/activate.sh --uninstall` returns Hermes to the native store alone. The smem
  store stays on disk.
