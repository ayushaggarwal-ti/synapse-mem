# synapse-mem agent integrations

Install, config and sync recipes for [synapse-mem](https://github.com/rahulsub/synapse-mem) (`smem`),
for any agent stack. Everything here is agent-agnostic except `agents/hermes/`, the one adapter for the
Hermes agent.

## What smem is

`smem` is an embedded memory store for agents: one Go binary, SQLite underneath, no server, no API keys
needed for the core.

- **Memories are beliefs, not transcripts.** Each memory is a fact, preference, procedure, insight or
  episode. It has a confidence that decays with time and disuse, and two timelines: when it was true, and
  when we learned it. Stale beliefs get superseded, invalidated or retracted. They are never silently
  overwritten.
- **Decisions keep their reasons.** A decision records the options it rejected, the assumptions it depends
  on and the memories it used. If one of those memories changes later, the decision is flagged as suspect.
  `smem why <commit|dec-…|mem-…>` answers "why did we do this?".
- **Everything is a signed event.** The store is an Ed25519-signed, hash-chained, append-only log. Any
  directory can be a sync remote. Events from keys you haven't trusted are quarantined.

The reasoning behind adopting it is in [docs/DECISIONS.md](docs/DECISIONS.md).

## Per-agent setup

| Agent | Scope | How | Files |
|---|---|---|---|
| Any | binary | `install/install-smem.sh` | checksum-verified release download; macOS/Linux, arm64/amd64 |
| Any | store | `install/setup-store.sh <dir\|file.db>` | creates the store, attaches sync remotes, trusts keys |
| Claude Code | one repo | `agents/claude-code/setup-project.sh [repo]` | `.mcp.json` + hooks in `.claude/settings.json`, kept with the repo |
| Claude Code | per user, all repos | plugin marketplace | [agents/claude-code/setup-user.md](agents/claude-code/setup-user.md) |
| Claude Code | manual | merge `agents/claude-code/settings-snippet.json` | hooks only; pair it with an MCP entry |
| Cursor, Windsurf, any MCP client | per client | merge `agents/mcp/mcp-config.json` | [agents/mcp/README.md](agents/mcp/README.md) |
| Hermes | per profile | `agents/hermes/activate.sh` | MemoryProvider plugin; [agents/hermes/README.md](agents/hermes/README.md) |

## Quick start (any agent)

```sh
./install/install-smem.sh                    # → ~/.local/bin/smem; no-op if already at that version
./install/setup-store.sh ~/code/my-app       # → ~/code/my-app/.smem/smem.db
smem doctor
```

Then wire your agent from the table above. For Claude Code in one repo, the single command is:

```sh
./agents/claude-code/setup-project.sh ~/code/my-app
```

## Layout

```
install/          install-smem.sh, setup-store.sh: shared by every adapter
agents/
  claude-code/    per-project script, per-user plugin notes, raw hooks snippet
  mcp/            MCP server entry for any MCP-capable client
  hermes/         Hermes-only adapter: plugin/ (MemoryProvider), activate.sh, tests/
config/           project vs local config, judge-through-gateway example
sync/             multi-machine, team curator feeds, trust and quarantine
docs/             DECISIONS.md
```

Only `agents/hermes/` depends on Hermes. If this folder moves to its own repo, it keeps working as-is. The
scripts only find each other by relative path.

## Requirements

`curl`, `tar`, and `sha256sum` or `shasum` for the installer. POSIX `sh` for everything else.
`activate.sh` also needs `python3` and the `hermes` CLI.
