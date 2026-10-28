# Claude Code: per-user plugin install

The upstream plugin gives every repo you open in Claude Code:

- the `smem` MCP server, with memory, decision, `why`, review, fact-check and rules tools;
- a SessionStart hook that injects the most salient memories;
- a PostToolUse hook that records mutating tool calls in the signed log;
- a skill on when to use all of the above.

## Install

1. Put the binary on `PATH`. The plugin's wrapper also checks `$SMEM_BIN`, `~/go/bin`,
   `/opt/homebrew/bin` and `/usr/local/bin`.
   ```sh
   ../../install/install-smem.sh
   ```
2. Inside Claude Code:
   ```
   /plugin marketplace add rahulsub/synapse-mem
   /plugin install smem@synapse-mem
   ```
3. Restart the session. `/mcp` should list `smem`, and `smem doctor` in any repo should show a store
   after the first session there.

Each project gets `.smem/` on first use. Everything in it is already git-ignored except `config.json`.

## Plugin or per-project?

| | Plugin (this page) | `setup-project.sh` |
|---|---|---|
| Lives in | `~/.claude` | the repo (`.mcp.json`, `.claude/settings.json`) |
| Applies to | every repo you open | that repo, for everyone who clones it |
| Survives a wiped home dir | no | yes |
| Good for | a personal laptop | ephemeral agent hosts, shared team repos |

Pick one per repo. If you use both, Claude Code registers the hooks and the MCP server twice.

## Settings worth setting

- `SMEM_ACTOR`: a stable identity such as `alice-laptop` or `ci-agent-3`. The default is `claude-code@<host>`,
  which changes whenever the host name does.
- `SMEM_CONFIG`: points at your local config on persistent storage. See [../../config/README.md](../../config/README.md).

## Uninstall

```
/plugin uninstall smem@synapse-mem
```

Stores in `.smem/` are left alone.
