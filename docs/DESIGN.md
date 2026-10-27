# smem: design

smem is an embedded memory for agents. It treats memory as something that
**changes over time and loses value**, records **why** every decision was made,
and keeps a **signed, append-only audit trail** of work and decisions that can
stay local or sync between machines.

It is meant to sit next to [beads](https://github.com/steveyegge/beads), not
replace it. Beads records work: what's ready, what blocks what. smem records
knowledge and reasoning: what we believe, how sure we are, what we decided on
the strength of it, and what happened as a result. The two link by id (a
decision can name a bead; `smem why bd-77` finds it).

## 1. Requirements

| Need | What it means here |
|---|---|
| Time | Separate *when something was true* from *when we learned it*. Never overwrite. |
| Decay | Memories fade from attention unless used, and become less certain unless re-checked. Fading never deletes. |
| Provenance | Every memory and decision says what it came from. When a basis changes, dependents are flagged. |
| Audit | An immutable record of what was done, decided, approved and seen, and by whom. |
| Integrity | Tampering is detectable; events cannot be forged without the author's key. |
| Local and sync | Works offline as a single file; replicas can exchange logs and converge. |
| Embedded | A Go library plus a single-binary CLI; SQLite; no server. |

Non-goals for the core: semantic (embedding) search, automatic extraction of
memories from transcripts, consolidation of episodes into facts. Each has a
place in the design (§9) but none is needed to get the guarantees above.

## 2. Architecture: the log is the database

```
            writes                                   reads
 agent ──▶ Engine.emit ──▶ events (signed, chained) ──▶ apply ──▶ projections
                               ▲      │                            memories, decisions,
                     import ───┘      └──▶ push (feed files)       assumptions, edges,
                     (verify)                                      accesses, actions,
                                                                   approvals, status_log
```

* **Every write appends one or more events.** Nothing else is ever written
  directly; the other tables are *projections* built by `apply(event)`.
* `apply` is a pure function of the event and of earlier state. `smem rebuild`
  drops every projection and replays the log; a test asserts the result is
  byte-for-byte identical.
* Decay, propagation and conflict detection operate on projections. **The log
  itself never decays.** Memories can fade from recall; the record of what
  happened is permanent.

### Event

```
id        evt-<first 16 hex of hash>
replica   which database wrote it
rseq      1, 2, 3 … within that replica
at        recorded time (ms), see §6
actor     who wrote it; key_id names the actor's signing key
session   agent session
type      e.g. memory.asserted, decision.made
subject   the record it is about
payload   JSON, redacted before hashing
prev      hash of this replica's previous event (genesis = 64 zeros)
hash      sha256 of every field above except id
sig       Ed25519 signature over hash by the actor's key
```

Event types: `key.registered`, `memory.{asserted, invalidated, retracted,
suspected, reconfirmed, signal, archived}`, `recall`, `source.captured`,
`decision.{made, outcome, approved, suspected, reaffirmed, reverted}`,
`assumption.{broken, held}`, `action.recorded`. Unknown types are kept and
skipped by `apply`, so an older binary can replay a newer log.

## 3. Time: three clocks

Most memory systems use one "age". That mixes up three things:

| Clock | Question | Changed by |
|---|---|---|
| **Validity** | Is it still true? | Evidence only: `invalidate`, `retract`, `supersede` |
| **Confidence** | How sure are we, given we haven't checked lately? | Time since last verification, by volatility class; contradictions |
| **Salience** | Is it worth bringing up now? | How recently and how often it has been used |

Old is not wrong, and unused is not untrue. Time only lowers salience and
confidence. Validity changes only on evidence.

### Two timelines

Each memory version carries:

* `valid_from` / `valid_to`: when it was true in the world (valid time)
* `recorded_at` / `closed_at` / `retracted_at`: when we learned it, learned it ended, or learned it was never true (recorded time)

A memory is visible to a query *as of* time V, *as known at* time K, when:

```
recorded_at ≤ K
and (retracted_at is null or retracted_at > K)
and valid_from ≤ V
and not (closed_at ≤ K and valid_to ≤ V)      -- closing only counts once we knew
```

So `recall --as-of 2026-03-01 --known-at 2026-03-02` answers "what did we
believe on March 2nd about how things were on March 1st", which is the
question an audit asks about a past decision.

Memory versions are immutable: a changed fact is a **new memory that
supersedes** the old one. The old one gets `valid_to` = the new one's
`valid_from`. A memory id therefore *is* a version, which is why a decision's
snapshot can just list ids.

* **Invalidate**: it was true, the world changed. (`valid_to` set.)
* **Retract**: it was never true; the belief was wrong. (`retracted_at` set; queries known after that moment don't see it at all.)
* **Supersede**: there is a newer version.

## 4. Decay

Scores are computed **when a memory is recalled**, from an access log, not
stored and updated by a background job.

```
score      = relevance × salience × confidence
relevance  = bm25(query) normalized to (0,1]; 1 when there is no query
salience   = σ(B + 2),  B = ln Σⱼ wⱼ · tⱼ^(−d_kind)        -- ACT-R base-level activation
confidence = c₀ · ½^(Δt_verified / half_life[volatility]) · ½^(contradictions) · (½ if suspect)
```

* `tⱼ` is hours since access j (min 1 minute). ACT-R gives recency,
  frequency and the spacing effect for free.
* Access weights `wⱼ`: asserted 1, retrieved 0.5, used 1, cited by a decision
  1.5, confirmed by a successful outcome 2, reconfirmed 1. A contradiction
  doesn't add to activation; it halves confidence.
* Decay exponent by kind: episode 0.8, fact 0.5, insight 0.4, procedure 0.3,
  preference 0.3.
* Confidence half-life by volatility: volatile 1 day, normal 30 days, stable
  1 year, permanent never. Defaults: facts and insights normal; episodes,
  procedures and preferences stable.

Reference points: one access an hour ago gives salience ≈ 0.88; one access a
year ago ≈ 0.07 for a fact, ≈ 0.3 for a preference.

**Tiers.** Hot memories are injected at session start (`smem context`, within
a character budget). Warm ones are found by `recall`. Cold ones are archived
by `sweep` (salience below 0.15 and untouched for 30 days): they leave default
recall but stay in the log and are returned with `--archived`. Nothing is
deleted. Anything a decision cites stays reachable through `why`.

Historical queries (`--known-at`) compute salience and confidence from
accesses up to that moment, so you can replay what an agent would have
recalled then.

## 5. Provenance and propagation

### The graph

Edges point from the dependent to what it depends on:

```
decision ──used──────────▶ memory (version)
decision ──evidenced_by──▶ source (url, commit, bead, file, transcript)
decision ──supersedes────▶ decision
memory   ──derived_from──▶ memory | decision | source
memory   ──supersedes────▶ memory
assumption ∈ decision     (statement + check hint)
action   ──▶ decision     (via decision_id)
```

The vocabulary follows W3C PROV loosely: memories and sources are entities;
decisions and actions are activities; actors are agents.

### Truth maintenance

When a basis changes (a memory is invalidated, retracted or superseded; an
assumption is broken; a decision is reverted or superseded), smem walks the
graph to the records that depend on it and emits `*.suspected` events, each
naming its **cause** and the **root** of the chain. This is a
justification-based truth maintenance system (Doyle, 1979), made practical
because agents can record the justifications as they work.

* **Depth.** Only direct dependents are flagged automatically
  (`PropagationDepth = 1`). Further downstream records appear in
  `smem review` as **at risk**. One bad fact can't wipe out confidence across
  the whole store, and a person or agent decides how far the doubt should go.
* Only *active* records are flagged, so propagating the same cause twice is a
  no-op. That makes it safe to re-run after a sync (§7).
* Suspect decisions are cleared with `reaffirm` (it still stands) or `revert`.
  Suspect memories with `reconfirm` or `invalidate`.
* **Decisions decay differently.** Their salience fades, but their authority
  doesn't. A decision stays binding until superseded, reverted, or its basis
  breaks.
* **Outcomes feed back.** A `success` outcome confirms every memory the
  decision used (weight 2). A `failure` does not weaken them automatically,
  because the reasoning may have been at fault rather than the facts.

## 6. Audit trail

### What a decision records

| Field | Purpose |
|---|---|
| question, choice, rationale | The decision and the agent's stated reasons |
| rejected options + reasons | Usually the most valuable part later |
| assumptions + check hints | Lets a decision be flagged when an assumption breaks |
| used | The memory versions relied on (these drive propagation) |
| evidence | Sources; free references are captured as sources automatically |
| snapshot | The memories in view (by default the session's last recall), model, prompt hash, skills |
| bead | Link to the work item |
| approvals | Who signed off, and the hash of the decision event they saw |
| actions | Work done under it (tool calls, commits) |
| outcome | Recorded later |
| history | Every status change with its cause |
| signers | Actor and key of every event about the decision |

**Stated vs observed.** An LLM's rationale is an explanation written after the
fact, not proof of what caused the action. smem stores both the stated
rationale and the observed facts: what was recalled (recall events) and what
was done (hook-captured actions). When they disagree, that is worth seeing.

### Capture without relying on discipline

* **Actions**: the Claude Code `PostToolUse` hook (`smem hook post-tool-use`)
  records every mutating tool call: summary, hashes of input and output, the
  transcript path, and the commit sha if the call was a `git commit`. It links
  the action to the session's latest active decision. That is a heuristic;
  `smem action --decision` is exact. Read-only tools are skipped unless `--all`.
* **Context**: the `SessionStart` hook (`smem hook session-start`) injects
  memories and records that injection as a recall, so the log shows what the
  agent was given.
* **Decisions**: the agent calls `smem decide`. (Extracting decisions it
  didn't record is future work, §9.)
* **Commits**: `smem trailer <dec>` prints `Decision: dec-…` for the commit
  message. `smem why <sha>` follows both recorded actions and trailers, so
  `git blame` → commit → decision → rationale, rejected options, and what the
  agent knew.

### Questions it answers

* `smem why <sha|dec|mem|asm|act|src|bead>`: why is this the way it is?
* `smem recall --as-of T --known-at T`: what did we believe then?
* `smem review`: which decisions rest on something that has changed?
* `smem log --subject <id>`: every event about a record, with signer.
* `smem verify`: has anything been altered?

## 7. Integrity: signing

* Each actor has an **Ed25519 key**, generated on first write and stored
  outside the database (`$SMEM_KEY_DIR`, default the user config dir, mode
  0600). The first event an actor writes in a replica is a self-signed
  `key.registered` with the public key; `key_id` is a fingerprint of it.
* Every event is hashed (covering its replica, position, previous hash, time,
  actor, key, type, subject and payload) and signed.
* SQLite triggers refuse `UPDATE` and `DELETE` on `events`.
* **Redaction happens before hashing.** Common secret shapes (API keys,
  tokens, private keys, JWTs, `*_TOKEN=` assignments) are replaced in every
  payload. An append-only log can't be cleaned up later.

`smem verify` checks for every replica: contiguous `rseq`, each `prev` equal
to the previous hash, each hash equal to its recomputed content, each
signature valid for the registered key, and that the key belongs to the
claimed actor.

**What this does and doesn't guarantee.** Nothing can make a local file
impossible to modify. The guarantees are:

1. **Modification is detected.** Changing, inserting, removing or reordering
   an event breaks the hash or the chain.
2. **Forgery needs the key.** Re-signing a modified event with another key
   fails verification (the key is bound to the actor by its registration).
3. **Rewriting your own history is visible to others.** Once events have been
   synced, every peer holds them; a rewritten feed is refused as a fork.
   `smem heads` prints the latest hash per replica. Publishing heads somewhere
   append-only (a commit message, a chat channel) anchors the log even against
   someone who controls every key on the machine.

**Trust** is local policy, like `known_hosts`: keys created locally are
trusted; keys arriving by sync are listed as untrusted in `smem review` until
`smem keys trust <key-id>`. Trust is not an event and is never synced.

## 8. Local and sync

### Local mode

One SQLite file (`.smem/smem.db`, found by walking up from the current
directory, or `$SMEM_DB`). Several agents on one machine share it; SQLite
serializes writes. Every event is still signed and chained.

### Sync mode: one feed per replica

A single global hash chain can't survive two machines appending at once.
Instead each replica (database) owns an append-only **feed**. Replicas
exchange feeds; nobody ever writes into someone else's feed. This is the same
shape as Secure Scuttlebutt and Hypercore.

**Order.** The merged log is ordered by `(at, replica, rseq)`. When writing,
`at` is set to `max(clock, newest event seen + 1 ms)`, a hybrid logical
clock. Anything causally after an event therefore sorts after it, whatever the
machines' clocks say. Queries treat "now" as the later of the clock and the
newest event.

**Import** (`Engine.Import`), for each incoming feed:

1. Skip events we already have, checking the hash matches; a different event
   at a known position is a **fork** and the feed is refused from there.
2. Require the next `rseq`, the right `prev`, a known key bound to the
   claimed actor, a correct hash and a valid signature. Stop at the first
   failure: nothing after a bad event is trusted.
3. If any accepted event sorts before what is already projected, **replay**
   the projections in order; otherwise apply incrementally.
4. **Propagate locally.** The remote writer couldn't see our dependents (a
   decision made here using a memory invalidated there), so invalidations
   arriving by sync are propagated again. Records already flagged are left
   alone.

**Conflicts.** If two replicas supersede the same memory or decision
independently, the first in log order becomes `superseded_by`. Both successors
stay live and `smem review` lists the fork. Resolve it by superseding one.
Status changes are idempotent (only active records become suspect), so
duplicate flags from two replicas do no harm.

**Convergence.** Replicas holding the same events produce identical
projections after a rebuild; a test checks this across two replicas.

**Transport.** The first remote is a **directory**: `feeds/<replica>.jsonl`,
one file per replica, appended only by its owner. That works over a shared
disk, Dropbox or a git repo (commit the directory). The directory is
untrusted: every event is verified on import. `push` refuses if the remote
copy of our own feed doesn't match the local log. Other transports (S3, HTTP,
beads/Dolt) only need "read all feeds" and "append to my feed".

## 9. Where to go next

1. **Semantic recall.** Add embeddings (sqlite-vec) as another relevance
   signal next to bm25; `auth` should find "authentication".
2. **Extraction and consolidation.** A pass at session end proposes memories
   and decisions from the transcript, as proposals the agent or user confirms.
   Repeated episodes become facts or procedures, linked by `derived_from`.
3. **Assumption checking.** Run check hints in a sandbox, only from trusted
   keys, and record `assumption.held` / `assumption.broken`. smem currently
   stores hints but never executes them, since a hint from a peer is untrusted
   input.
4. **Key rotation and revocation** as signed events; a trusted-keys file that
   can be committed to a repo.
5. **Evaluation.** Replay past sessions with `--known-at` and measure whether
   recall surfaced what was later cited.
6. **Beads integration.** Write `bd provenance` entries for decisions that
   name a bead, and file suspect decisions as beads so they show up in
   `bd ready`.
7. **Feed offsets.** Pull currently re-reads whole feed files; track offsets
   per feed for large logs.

## 10. Code map

| File | Contents |
|---|---|
| `smem/event.go` | Event, hashing, signing, ordering |
| `smem/keys.go` | Key storage, registration, trust |
| `smem/engine.go` | Open, write transactions, emit |
| `smem/apply.go` | Event payloads and the projection function |
| `smem/memory.go` | Memories, invalidation, propagation |
| `smem/decision.go` | Decisions, assumptions, outcomes, approvals, actions |
| `smem/recall.go` | Scoring, two-timeline queries, context, sweep |
| `smem/review.go` | Suspect, at-risk, conflicts, untrusted keys |
| `smem/why.go` | Traces and their text rendering |
| `smem/verify.go` | Verification, heads, rebuild |
| `smem/sync.go` | Import, export, directory remote |
| `smem/hook.go` | Claude Code hook capture |
| `cmd/smem` | CLI |
