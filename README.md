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
