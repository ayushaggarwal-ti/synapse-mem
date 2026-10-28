# Configuring smem

smem merges two JSON files over its defaults. Environment variables win over both.

| | Project config | Local config |
|---|---|---|
| Path | `.smem/config.json`, next to the store | `$SMEM_CONFIG`, else `~/.config/smem/config.json` (macOS: `~/Library/Application Support/smem/config.json`) |
| Commit it? | yes | never |
| Holds | behavior: scopes, context budget, decay, hooks, redaction, MCP tools, judge models, fact-check policy | all of that, plus `actor`, `keys`, `trust`, `endpoints`, `checkers` |

The split is a security boundary. Anything that can run a command (`checkers`), redirect credentials
(`endpoints`) or decide whose synced memories you see (`trust`, `keys`) is refused in a project config.
That way a cloned repo can't take over your machine through its `.smem/config.json`. Unknown keys are an
error, so a typo fails loudly instead of doing nothing. `smem doctor` shows which files were loaded.

## Files here

| File | What |
|---|---|
| [project.config.example.json](project.config.example.json) | committable: scopes, context budget, decay, a hook skip-list, a judge budget |
| [config.example.json](config.example.json) | local: identity, the same behavior settings, and the Claude judge through a gateway |
| [judge-gateway.example.sh](judge-gateway.example.sh) | the environment half of the gateway setup: headers and token never go in a file smem reads |

Both JSON files pass `smem doctor` as-is. Copy them, then change the placeholders: `my-agent-1`,
`project:my-app`, `gateway.example.com`, `my-gateway/`.

## Scopes and decay

- `scopes.read` limits what recall and session context can see. Each entry is a glob, such as
  `project:my-app*` or `team`. Memories with no scope are always visible.
- `scopes.write` is the scope given to new memories that don't name one.
- `decay.clock: "active"` counts an idle stretch as at most `idle_after`. An agent that's off for a week
  comes back with its memory. Use `"wall"` for real time.
- Confidence ("is it still true?") always decays in real time, whatever the clock setting.

## Fact-check judges through a gateway

smem can fact-check memories with LLM judges: Jev, Claude, or both. If you route Claude through a gateway
or proxy, an Anthropic-compatible endpoint that authenticates with custom headers, smem needs:

| Setting | Where | Example |
|---|---|---|
| `ANTHROPIC_BASE_URL` | env, or `endpoints.claude` in the local config; if both are set, `endpoints.claude` wins | `https://gateway.example.com` |
| `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` | env only | any non-empty value if the gateway authenticates by header |
| `ANTHROPIC_CUSTOM_HEADERS` | env only | `"x-gateway-key: …\nx-team-token: …"`, newline-separated `Name: value` lines |
| `SMEM_JUDGE_MODEL` | env (per agent), or `judges.claude.model` in config | the gateway's id for the model, e.g. `my-gateway/claude-haiku-4-5-20251001` |

These are the same variables Claude Code uses, so an agent already running Claude Code through a gateway
usually has all but `SMEM_JUDGE_MODEL` set. smem never reads Claude Code's `ANTHROPIC_DEFAULT_*_MODEL`
variables, so name the judge model explicitly whenever your gateway prefixes model ids. Header values
are treated as secrets: they are redacted from the log and never passed to checkers.

The example config leaves `endpoints.claude` out so `ANTHROPIC_BASE_URL` (set by `judge-gateway.example.sh`) selects
the gateway. To pin the gateway in the local config instead, add `"endpoints": {"claude": "https://gateway.example.com"}`;
it then overrides the environment variable.

The example config also disables Jev (`judges.jev.disabled`), runs Claude on every check
(`judges.claude.when: "always"`) and lowers `factcheck.min_agreeing` to 1. With only one judge, the
default of 2 means no check could ever change a memory, and everything would go to `smem review`. Keep
the default of 2 if you have a second judge or a checker.

Verify it:

```sh
. ./judge-gateway.example.sh            # with real values
SMEM_CONFIG=$PWD/config.example.json smem judges --ping
```

**Check that your gateway accepts the request shape.** The Claude judge asks for structured output
(`output_config.format`, a JSON schema). Some gateways or backing models reject that, and the ping
reports `HTTP 400`. On our fleet's gateway, the `claude-haiku-4-5` route returned 400 while the
`claude-sonnet-5-5` route was accepted, though it was rate-limited (`429`) at the time of testing. Pick a
model your gateway serves with structured outputs. A judge that fails is recorded as *not judged*. It
never silently counts as a pass.

## Checkers

These are commands in any language that receive the claim as JSON on stdin and print a verdict. Because
they run code, they are local-config only, and they get a minimal environment with no API keys. See
upstream [docs/CONFIG.md](https://github.com/rahulsub/synapse-mem/blob/main/docs/CONFIG.md).
