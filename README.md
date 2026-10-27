# synapse-mem (`smem`)

Embedded memory for agents. Memories have two timelines and decay with time
and disuse. Decisions record why they were made and get flagged when what
they relied on changes. Everything is an Ed25519-signed, hash-chained,
append-only event log that works locally or syncs between machines.

Design: [docs/DESIGN.md](docs/DESIGN.md)

## Build

```sh
go build -o bin/smem ./cmd/smem
go test ./...
```

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

## Fact-checking

```sh
export TYPESAFE_API_KEY=…      # Jev: fast, calibrated
export ANTHROPIC_API_KEY=…     # Claude Haiku 5.5: when Jev is unsure (optional)
smem judges                    # what's configured
smem factcheck mem-…           # or --due to check decayed facts
```

Write your own checks in any language. Each one gets the claim as JSON on
stdin and prints a verdict, evidence, or nothing. Configure them in
`.smem/config.json`:

```json
{
  "checkers": [{"name": "port", "command": ["./check_port.py"], "match": "port \\d+"}],
  "judges": {"claude": {"when": "escalate"}},
  "factcheck": {"auto_accept": 0.8, "auto_reconfirm": 0.9}
}
```

Checkers run first and a decisive one skips the models entirely. A confident
contradiction marks a memory suspect (never invalid). Every check is a signed
judgment you can trace with `smem why`.

## Claude Code hooks

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "smem hook session-start"}]}],
    "PostToolUse":  [{"hooks": [{"type": "command", "command": "smem hook post-tool-use"}]}]
  }
}
```

The session-start hook injects the most salient memories within a budget and
records that it did. The post-tool-use hook records each mutating tool call,
including commit shas, linked to the session's current decision. Hooks never
fail the agent.

## Environment

| Variable | Meaning |
|---|---|
| `SMEM_DB` | Database path (default: nearest `.smem/smem.db`) |
| `SMEM_ACTOR` | Who is writing (default `$USER`; hooks default to `claude-code`) |
| `SMEM_SESSION` | Session id |
| `SMEM_KEY_DIR` | Private keys (default: user config dir `smem/keys`) |
| `TYPESAFE_API_KEY` / `OPENROUTER_API_KEY` | Enables the Jev judge |
| `ANTHROPIC_API_KEY` | Enables the Claude judge |
