# Any MCP client

Merge [mcp-config.json](mcp-config.json) into your client's MCP config:

| Client | File |
|---|---|
| Cursor | `.cursor/mcp.json` (project) or `~/.cursor/mcp.json` (user) |
| Windsurf | `~/.codeium/windsurf/mcp_config.json` |
| Claude Desktop | `claude_desktop_config.json` |
| Claude Code | `.mcp.json` (what `smem init --claude-code` writes) |
| Custom | spawn `smem mcp --init` over stdio |

`smem mcp` serves the store it finds by walking up from the server's working directory. `--init` creates
`.smem/smem.db` there if no store is found.

Some clients don't start servers in the project directory: user-level configs, Claude Desktop, most
custom hosts. For those, pin the store and the identity:

```json
{"mcpServers": {"smem": {"command": "smem", "args": ["mcp"],
  "env": {"SMEM_DB": "/abs/path/to/.smem/smem.db", "SMEM_ACTOR": "cursor-alice"}}}}
```

Tools: `memory_recall`, `memory_context`, `memory_remember`, `memory_update`, `memory_invalidate`,
`memory_signal`, `decision_record`, `decision_outcome`, `why`, `review`, `factcheck`, `failure_record`,
`rule_propose`, `rules_list`. Hide any of them with `mcp.disable`, and replace the usage text the agent sees
with `mcp.instructions`. Both go in the config; see [../../config/README.md](../../config/README.md).

The MCP config gives the agent the tools, not the habit of using them. Paste
[INSTRUCTIONS-template.md](INSTRUCTIONS-template.md) into the agent's rules file or system prompt.

If your client supports lifecycle hooks, `smem hook session-start` prints the context to inject, and
`smem hook post-tool-use` reads a Claude Code-shaped hook JSON on stdin.
