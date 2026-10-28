# Sync patterns

A remote is just a directory: a NAS mount, a Dropbox or iCloud folder, a git checkout you commit and push,
or an rsync target. Each replica writes its own signed feed there and reads everyone else's. Nothing is
ever overwritten, so there is no merge conflict to resolve.

```sh
smem remote add origin /shared/smem      # or: ../install/setup-store.sh <store> --remote origin=/shared/smem
smem sync                                # pull (verify + merge), then push; bare `sync` means origin
smem keys                                # signing keys seen, trusted or not
smem heads                               # latest hash per replica; publish these to anchor the log
```

## 1. One person, several machines

Every machine adds the same remote, and each one trusts the other machines' keys once.

```sh
# on each machine
../install/setup-store.sh ~/code/my-app --remote origin="$HOME/Dropbox/smem/my-app" --sync
smem keys                          # note the other machines' key-… ids
../install/setup-store.sh ~/code/my-app --trust key-<laptop> --trust key-<desktop>
```

- Set a stable `actor` per machine in its local config, such as `alice-laptop`. Keep `keys.dir` on
  persistent storage. If an actor that already signs the store loses its key, writes are refused rather
  than silently minting a new identity.
- smem never syncs on its own: hooks, the MCP server and agent adapters only write locally. Run `smem sync` on a
  schedule, or before and after work.
- A copied or restored database that already holds events continues as a new replica instead of forking
  the old one. An empty copy keeps the original replica ID, so two empty clones can collide on their first
  writes. Create each machine's store fresh (`setup-store.sh`) and pull from the remote rather than copying
  an empty database.

## 2. Team memory, read-only, from a curator

A curator, which can be a person or a reviewing agent, owns a store and pushes it to a shared
directory. Agents pull from that directory and never write to it.

```sh
# curator
../install/setup-store.sh /srv/curator --remote team=/shared/team-memory
smem --db /srv/curator/.smem/smem.db sync team

# each agent
../install/setup-store.sh ~/code/my-app --pull-only team=/shared/team-memory --trust key-<curator> --sync
```

- Agents trust only the curator's key. Events from any other key the feed publishes stay quarantined; events
  signed by a key the store doesn't know are rejected.
- A new key must be pulled before it can be trusted, so keep `--sync` on the first run: the script pulls,
  then applies `--trust`.
- `scopes.read` in the agent's project config chooses which of the curator's scopes the agent sees,
  e.g. `["team", "project:my-app*"]`.
- `--sync` on a pull-only remote only pulls.

## 3. Trust and quarantine

Events signed by a known but untrusted key (one the store has seen, such as published with a pulled feed) are
verified and stored, but **quarantined**. They are never recalled and never injected into a prompt. Events signed by a key the
store has never seen are rejected on import.

```sh
smem sync                           # "N event(s) signed by untrusted keys are quarantined"
smem keys                           # key-…  <actor>  untrusted (quarantined)  N events
smem why <mem-…>                    # inspect before trusting
smem keys trust key-…               # or "trust": {"keys": [...]} in the local config
smem keys distrust key-…            # back to quarantine; nothing is deleted
smem verify                         # every hash, chain link and signature
smem review                         # suspect, at-risk and conflicting records; untrusted keys
```

- Trust is per store, and only the local config may set it. A committed project config can't make
  you trust anyone.
- Imports refuse events dated more than `sync.max_future_skew` ahead, and clamp events backdated past
  `sync.max_backdate`. A remote therefore can't rewrite what as-of queries say was known.
- Recommended routine for a new key: `sync` → `keys` → spot-check with `why` → `trust` → `recall --known-at`
  → `verify`. Trusting replays the log, so quarantined events only become recallable afterwards.

## Moving events without a shared directory

```sh
smem export > events.jsonl          # optionally --replica rep-…
smem import events.jsonl            # verified like a pull; unknown keys are rejected, known untrusted keys quarantined
```
