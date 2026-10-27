# Configuring smem

Everything that varies between teams, projects and agents is set in
configuration. Code changes shouldn't be needed. smem reads two files and
merges them over the defaults:

| File | Where | May be committed | Holds |
|---|---|---|---|
| Project config | `.smem/config.json` next to the store | yes | behavior: beads, scopes, context, decay, sync bounds, hooks, redaction, MCP, judges, fact-check policy |
| Local config | `$SMEM_CONFIG`, else `~/.config/smem/config.json` (macOS: `~/Library/Application Support/smem/config.json`) | no | everything above, plus the local-only settings |

The local config wins over the project config. Environment variables win
over both: `SMEM_ACTOR`, `SMEM_KEY_DIR`, `SMEM_DB`, `SMEM_SESSION` and the API
keys. API keys are only ever read from the environment.

**Local-only settings** are `actor`, `checkers`, `endpoints`, `keys` and
`trust`. A project config that sets one is refused with an error. A cloned
repository must not be able to run commands on your machine, send your API
key to another server, or decide whose synced memories you see.

Unknown keys are an error, so a typo doesn't silently do nothing. Durations
are strings such as `"90s"`, `"30m"`, `"24h"` or `"7d"`. Run `smem doctor`
to see which files were loaded and what they did.

## Reference

Defaults are shown.

### `beads`: which references are bead ids

```json
"beads": {"prefixes": ["bd"], "detect": true}
```

`prefixes` are bead id prefixes. `dojo` matches `dojo-j6lv` and child ids
such as `dojo-j6lv.1.2`. With `detect`, smem also reads `issue-prefix` from
`.beads/config.yaml`. Bead ids are recognized in `decide --evidence`,
`derived_from`, `source` and `why`.

### `scopes`: what an agent sees and writes

```json
"scopes": {"read": [], "write": ""}
```

`read` limits recall and session context to memories in these scopes. Each
entry is a glob such as `"project:dojo*"`, `"team"` or `"agent:reviewer"`.
Memories with no scope are always visible, and an empty list means
everything. `write` is the scope given to new memories that don't name one.

### `context`: what the session-start hook injects

```json
"context": {"budget": 4000, "kinds": [], "reinforce": false, "on": ["startup", "clear"]}
```

`budget` is in characters. `kinds` limits the injected kinds. `reinforce`
counts an injection as a use. It's off by default, because otherwise
memories keep themselves alive just by being shown. `on` lists the
SessionStart sources that inject: `startup`, `resume`, `clear` and
`compact`.

### `decay`: the clock memories fade by

```json
"decay": {"clock": "active", "idle_after": "24h"}
```

On the `active` clock, a stretch with no activity from this agent counts as
at most `idle_after`, so an agent that is off for a week comes back with its
memory. `wall` counts real time. Confidence ("is it still true?") always
uses real time, because the world changes whether or not the agent is
running.

### `recall`

```json
"recall": {"dedupe_window": "10m"}
```

A recall identical to one logged in the same session within the window
(same query, same results) isn't logged again.

### `sync`: what imports may claim about time

```json
"sync": {"max_future_skew": "5m", "max_backdate": "720h"}
```

Imported events dated more than `max_future_skew` ahead are refused until
the clocks agree. Events older than `max_backdate` are kept as signed but
projected at the clamp, so they can't rewrite what as-of queries say was
known.

### `hooks`

```json
"hooks": {"spool": true, "lock_timeout": "2s", "link_decisions": "unambiguous",
          "link_window": "30m", "skip_tools": [], "require_store": false}
```

- `spool`: post-tool-use writes a small redacted file and returns without
  touching the database. The next writer records it: any CLI command, the
  MCP server, or the next session start.
- `lock_timeout`: how long a hook waits for the database lock. Keep it well
  under the hook's own timeout.
- `link_decisions`: how a tool call is attributed to a decision.
  - `unambiguous`: the session's latest decision, but only if it is the only
    one made within `link_window`. Parallel subagents don't get guessed.
  - `latest`: always the session's latest decision.
  - `never`: no linking.

  A `Decision:` commit trailer is always exact.
- `skip_tools`: tool-name globs that are never recorded, e.g.
  `["mcp__slack__*"]`.
- `require_store`: makes hooks fail visibly when no store is found. Set this
  in the local config, since without a store there is no project config.

### `redaction`: adds to the built-in rules

```json
"redaction": {"min_env_value": 6, "secret_env": [], "patterns": []}
```

The exact values of credential-like environment variables are always
redacted, whatever their format. `min_env_value` is the shortest such value
redacted. `secret_env` names more variables to treat as secrets, and
`patterns` adds regular expressions. Configuration can only add to the
built-in rules, never remove them.

### `mcp`

```json
"mcp": {"disable": [], "instructions": ""}
```

`disable` hides tools, for example `["memory_remember"]` where `bd remember`
owns plain memories. `instructions` replaces the usage text the server gives
the agent.

### `judges`

```json
"judges": {"jev": {"disabled": false, "model": "jev-latest"},
           "claude": {"when": "escalate", "model": "claude-haiku-5-5", "effort": "low", "max_tokens": 4096}}
```

- **Jev** is enabled by `TYPESAFE_API_KEY`, or `OPENROUTER_API_KEY` to reach
  it through OpenRouter.
- **Claude** is enabled by `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`.
  The SDK also honors `ANTHROPIC_BASE_URL`.
- `when` controls when Claude is asked:
  - `escalate`: when Jev is unsure or unavailable, or to make up
    `min_agreeing`.
  - `always`: on every check.
  - `never`: not at all.

Run `smem judges --ping` to send each judge a tiny question and report what
happened.

### `factcheck`

```json
"factcheck": {"auto_accept": 0.8, "auto_reconfirm": 0.9, "subject_at": 0.5, "related_memories": 3,
              "max_evidence_chars": 4000, "min_agreeing": 2, "cache_ttl": "24h", "daily_budget": 500,
              "evidence": {"allow": ["**"], "deny": [], "max_bytes": 262144}}
```

- `min_agreeing`: how many independent checks must agree before a memory is
  reconfirmed or marked suspect. Each checker verdict and each judge counts
  once. With the default of 2, no single judge can change a memory, and the
  result goes to review instead.
- `cache_ttl`: an unchanged claim with unchanged evidence isn't re-judged
  within this time.
- `daily_budget`: caps judge calls per replica per 24 hours. 0 means no cap.
- `evidence`: limits which files can be read and sent to a judge. Paths must
  resolve, symlinks included, to a file inside the project.
  - `allow` and `deny` are globs relative to the project, where `**` matches
    any depth.
  - `deny` adds to a built-in list that can't be removed: `.git`, `.smem`,
    `.beads`, `.env*`, keys and certificates, `.ssh`, `.aws`, `.netrc`,
    `.npmrc`, credential files, `*secret*` and Terraform state.

### Local only

```json
{
  "actor": "dojo-agent-7",
  "keys": {"dir": "/persist/smem/keys", "allow_new": false},
  "trust": {"keys": ["key-3f9c0e1a2b4d5c6e"]},
  "endpoints": {"jev": "", "claude": ""},
  "checkers": [{"name": "port", "command": ["/persist/checks/port.py"], "match": "port \\d+",
                "kinds": ["fact"], "timeout": "10s", "env": ["CONSUL_HTTP_ADDR"]}]
}
```

- `actor`: the identity this agent signs as. The default is `user@host`
  (`claude-code@host` for hooks and MCP), which changes whenever the host
  name does. Set it explicitly for agents.
- `keys.dir`: where signing keys live. It must be persistent. If an actor
  that already signs the store has lost its key, writes are refused rather
  than silently minting a new identity. `allow_new` permits starting a new
  key deliberately.
- `trust.keys`: keys whose synced events are shown. Events from other keys
  are stored and verified but quarantined: never recalled or injected.
  `smem keys trust <key-id>` does the same interactively.
- `endpoints`: send judges to a gateway or proxy.
- `checkers`: commands that check claims, in any language. A checker gets
  only `PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `TZ`, `TMPDIR` and the
  variables named in its `env`. smem's own API keys are never passed.

## Example: Dojo

Committed `.smem/config.json`:

```json
{
  "beads": {"prefixes": ["dojo"]},
  "scopes": {"read": ["team", "project:dojo*"], "write": "project:dojo"},
  "mcp": {"disable": ["memory_remember"],
          "instructions": "Plain notes go in `bd remember`. Use smem for decisions (decision_record, decision_outcome), for checking what you know (memory_recall), and for `why`."},
  "hooks": {"skip_tools": ["mcp__plugin_beads_*"]},
  "factcheck": {"daily_budget": 200}
}
```

Each agent's local config, at `$SMEM_CONFIG` on persistent storage:

```json
{
  "actor": "dojo-agent-7",
  "keys": {"dir": "/persist/smem/keys"},
  "trust": {"keys": ["key-<curator>"]}
}
```

Team memory is a pull-only remote that a curator publishes. Agents trust
only the curator's key:

```sh
smem remote add team /shared/dojo-team-memory --pull-only
smem sync team
```
