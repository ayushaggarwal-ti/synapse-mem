---
name: memory
description: How to use smem, the project's long-term memory and decision log. Use when starting a task, when you learn something worth keeping across sessions, when a remembered fact turns out wrong, when you make a non-obvious choice, or when someone asks why something was done.
---

# Using smem

**Project instructions come first.** If CLAUDE.md, AGENTS.md or similar name a different tool for something (for example `bd remember` for plain memories, or beads for tasks), use that tool for it. Use smem for what they leave to it, usually decisions, provenance and "why". A project can turn off smem tools that overlap with its own (`mcp.disable` in `.smem/config.json`). Tools that are turned off don't appear.

smem's tools (`memory_*`, `decision_*`, `why`, `review`, `factcheck`) are your memory across sessions. Memories fade unless they are used, and lose confidence as they age, so what you store and how you report on it decides what future sessions see.

## Recall before you act

At the start of a task, `memory_recall` the topic. If you rely on a result, call `memory_signal` with `used`. If you checked it and it still holds, use `confirmed`. If you saw evidence against it, use `contradicted`.

## Remember sparingly

Store one self-contained statement per call, with the right kind:
- `fact`: true about the system ("the api listens on 9090").
- `preference`: what a person wants ("Rahul wants PRs under 400 lines").
- `procedure`: how to do something that took effort to work out.
- `insight`: a lesson ("flaky test X fails only under -race").
- `episode`: something that happened and will matter later.

Pass `derived_from` with the files, URLs or commits it came from. Fact-checks use those sources.

Don't store what the code, git history or docs already say, what only matters to this task, or secrets. Secrets are redacted anyway, so storing them just leaves a useless memory.

## Correct, don't duplicate

If a remembered fact changed, use `memory_update`; the old version stays in history. If it is no longer true, use `memory_invalidate`; add `never_true` if it was a mistake. Decisions that relied on it are flagged for review automatically.

## Record decisions worth explaining later

Call `decision_record` for choices someone could reasonably question: an architecture or library choice, a workaround, a deliberate deviation from a convention. Include the rejected options with reasons, the assumptions that must keep holding, and the memory ids you relied on in `used`. Put the returned `Decision: dec-...` trailer in the commit message. When you know how it went, call `decision_outcome`.

## Answer "why?"

For "why is it like this?" or "why did we do X?", call `why` with a decision or memory id, a commit sha or a bead id (in the project's prefix, e.g. `dojo-j6lv.1`). Use `review` to see what has become doubtful since.
