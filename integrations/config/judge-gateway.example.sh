# Claude judge through an Anthropic-compatible gateway. Source before running smem:
#   . ./judge-gateway.example.sh
export ANTHROPIC_BASE_URL=https://gateway.example.com
# The gateway authenticates by header; smem still needs a token set to enable the Claude judge.
export ANTHROPIC_AUTH_TOKEN=gateway
# "Name: value" lines, newline-separated, the same format Claude Code takes.
export ANTHROPIC_CUSTOM_HEADERS="x-gateway-key: <key>
x-team-token: <token>"
export SMEM_JUDGE_MODEL=my-gateway/claude-haiku-4-5-20251001
unset ANTHROPIC_API_KEY
