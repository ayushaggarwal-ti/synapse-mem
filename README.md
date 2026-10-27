# synapse-mem (`smem`)

Embedded memory for agents. Memories have two timelines and decay with time
and disuse. Decisions record why they were made and get flagged when what
they relied on changes. Everything is an Ed25519-signed, hash-chained,
append-only event log that works locally or syncs between machines.

Design: [docs/DESIGN.md](docs/DESIGN.md) · Configuration: [docs/CONFIG.md](docs/CONFIG.md)

## Install

**Binary**, from a GitHub release, with any token that can read the repo
(no SSH setup needed):

```sh
GH_TOKEN=… sh scripts/install.sh            # latest; or: sh scripts/install.sh v0.3.0 /persist/bin
```

It verifies the checksum. On agents whose home directory doesn't survive a
restart, install to persistent storage.

**From source.** The repo is private, so Go must skip the public proxy and
fetch with a key that has access (here the `github.com-rahulsub` SSH alias):

```sh
export GOPRIVATE=github.com/rahulsub/*
git config --global url."git@github.com-rahulsub:rahulsub/".insteadOf "https://github.com/rahulsub/"
go install github.com/rahulsub/synapse-mem/cmd/smem@latest
```

Or build from a checkout with `make build` (`make dist` for every platform).

Then run `smem doctor`. It checks the store, the config files, your identity
and key, judges (`--ping` sends each a tiny question), trust, the spool and
remotes.

### Claude Code

**Per project, kept with the repo** (best for agents whose `~/.claude` is
lost on restart):

```sh
smem init --claude-code     # writes .mcp.json and the hooks in .claude/settings.json, keeping what's there
```

**As a plugin, per user:**

```
/plugin marketplace add git@github.com-rahulsub:rahulsub/synapse-mem.git
/plugin install smem@synapse-mem
```

(`rahulsub/synapse-mem` works too where the default GitHub credentials can
read the repo.)

The plugin adds the `smem` MCP server (memory and decision tools), the
session-start and post-tool-use hooks, and a skill on when to use them. It
needs the `smem` binary on `PATH` (or `SMEM_BIN`). It creates `.smem/` in each
project on first use, already git-ignored except `config.json`.

### Any MCP agent

```json
{"mcpServers": {"smem": {"command": "smem", "args": ["mcp", "--init"]}}}
```

Tools: `memory_recall`, `memory_context`, `memory_remember`, `memory_update`,
`memory_invalidate`, `memory_signal`, `decision_record`, `decision_outcome`,
`why`, `review`, `factcheck`.

### Go

```go
e, err := smem.Open(".smem/smem.db")
cfg, _ := smem.LoadConfig(smem.ConfigPath(e.Path()))
e.Configure(cfg) // judges from TYPESAFE_API_KEY / ANTHROPIC_API_KEY
id, err := e.Remember(smem.MemoryInput{Kind: "fact", Content: "..."})
```

### Release

`git tag v0.2.0 && git push origin v0.2.0` runs the tests and publishes
binaries with checksums via goreleaser.

## Quick tour

```sh
smem init                                   # .smem/smem.db in this directory

F=$(smem remember "orders table is append-only" --kind fact)
smem remember "prefer boring tech" --kind preference
smem recall orders --explain                # relevance × salience × confidence

D=$(smem decide --question "fix bad totals?" --choice "insert correction rows" \
      --why "append-only keeps history" \
      --reject "UPDATE in place|breaks the invariant" \
      --assume "no consumer sums raw rows|grep -r 'SUM(total)' services/" \
      --used $F --bead bd-77 | head -1)
git commit -m "Correction rows

$(smem trailer $D)"

smem invalidate $F --reason "migration 0042 added UPDATE triggers"
#   ⚠ dec-… is now suspect (because mem-… changed)
smem review
smem why $(git rev-parse --short HEAD)       # or dec-…, mem-…, asm-…, bd-77
smem recall orders --as-of 2026-03-01 --known-at 2026-03-02
smem verify                                 # every hash, link and signature
```

## Sync

```sh
smem remote add origin /shared/smem         # any shared dir: NAS, Dropbox, a git repo
smem sync                                   # pull (verify + merge), then push
smem keys                                   # keys from other replicas start untrusted
smem keys trust key-…
smem heads                                  # publish these hashes to anchor the log
```

Events signed by keys you haven't trusted are verified and stored but
**quarantined**: never recalled or injected into a prompt until you trust
the key (`smem keys trust`, or `trust.keys` in the local config). Imports
refuse events dated in the future and clamp backdated ones. A restored or
copied database continues as a new replica instead of forking the old one.

**Team memory, read-only:** a curator pushes to a shared directory. Agents
add it with `smem remote add team <dir> --pull-only`, trust only the
curator's key, and choose what they see with `scopes.read`.

## Fact-checking

```sh
export TYPESAFE_API_KEY=…      # Jev: fast, calibrated
export ANTHROPIC_API_KEY=…     # Claude Haiku 5.5: when Jev is unsure (optional)
smem judges --ping             # what's configured, and whether each judge answers
smem factcheck mem-…           # or --due to check decayed facts
```

Write your own checks in any language. Each one gets the claim as JSON on
stdin and prints a verdict, evidence, or nothing. Checkers run code, so they
go in the **local** config (`$SMEM_CONFIG`), never in the committed
`.smem/config.json`:

```json
{"checkers": [{"name": "port", "command": ["/persist/checks/check_port.py"], "match": "port \\d+"}]}
```

**Agreement.** Changing a memory takes two agreeing checks by default
(`factcheck.min_agreeing`), where a checker or a judge each counts once. A
single judge never reconfirms a memory or marks it suspect on its own;
anything short of agreement goes to review.

A contradiction marks a memory suspect, never invalid. Unchanged claims
aren't re-judged within `cache_ttl`, and `daily_budget` caps judge calls.
Evidence files must be inside the project and not on the deny list (`.env`,
keys, `.git`, …). Every check is a signed judgment you can trace with
`smem why`.

## Claude Code hooks

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "smem hook session-start"}]}],
    "PostToolUse":  [{"hooks": [{"type": "command", "command": "smem hook post-tool-use"}]}]
  }
}
```

**Session start.** The hook injects the most salient memories in the
session's scopes, within a budget. It does this on startup and `/clear`, not
on resume or compact (`context.on`). Being shown doesn't count as being used.

**Tool calls.** The post-tool-use hook spools each mutating tool call as a
small redacted file, so it never waits on the database. The next writer
records it, including commit shas. A call is linked to a decision only when
that's unambiguous (`hooks.link_decisions`), so parallel subagents don't get
each other's work.

**General.** Hooks find the store from the session's working directory.
They never fail the agent, unless `hooks.require_store` is set.

## Configuration

Everything that differs between teams is configuration. That includes bead
prefixes (`dojo-…`), scopes, which MCP tools are on and the instructions the
agent gets, decay, sync bounds, hook behavior, extra redaction patterns, and
fact-check thresholds, cache and budget. Settings that are safe to commit go
in `.smem/config.json`. Identity, keys, trust, judge endpoints and checkers
go in the local config (`$SMEM_CONFIG`), which a cloned repo can't
override. See [docs/CONFIG.md](docs/CONFIG.md), which includes a worked
example for a beads-based fleet.

## Credentials

API keys are read from the environment only and never stored. Every payload
is redacted before it is signed, as is evidence before it goes to a judge.
Redaction removes the exact values of credential-like environment variables,
whatever their format, and common secret shapes in text: provider keys,
Stripe/Google/AWS/GitHub/GitLab/Slack tokens, JWTs, PEM blocks and fragments,
`*_KEY=` and similar assignments, auth headers, and URL passwords. Patterns
can be added in config. Command checkers get a minimal environment and never
the API keys. Details and limits: DESIGN.md §7.

## Environment

| Variable | Meaning |
|---|---|
| `SMEM_DB` | Database path (default: nearest `.smem/smem.db`) |
| `SMEM_ACTOR` | Who is writing (default `user@host`; hooks `claude-code@host`, MCP `<client>@host`). Set it for agents |
| `SMEM_CONFIG` | Local config file (default: user config dir `smem/config.json`) |
| `SMEM_BIN` | Binary the Claude Code plugin runs (default: `smem` on `PATH`) |
| `SMEM_SESSION` | Session id |
| `SMEM_KEY_DIR` | Private keys (default: user config dir `smem/keys`) |
| `TYPESAFE_API_KEY` / `OPENROUTER_API_KEY` | Enables the Jev judge |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | Enables the Claude judge |
| `ANTHROPIC_BASE_URL` + `ANTHROPIC_CUSTOM_HEADERS` | Claude judge through a gateway (headers are `Name: value` lines) |
| `SMEM_JUDGE_MODEL` | Claude judge model for this agent, e.g. a gateway-prefixed id |
