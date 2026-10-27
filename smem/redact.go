package smem

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
)

// CredentialEnv lists the environment variables smem reads credentials from.
// Their values are redacted wherever they appear, and they are removed from
// the environment of command checkers.
var CredentialEnv = []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// secretName matches environment variable and field names that hold secrets.
var secretName = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL|AUTH)`)

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`), "[REDACTED:anthropic-key]"},
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "[REDACTED:aws-key]"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`xox[abprs]-[A-Za-z0-9\-]{10,}`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic|Token)\s+[A-Za-z0-9._~+/\-]{16,}=*`), "$1 [REDACTED]"},
	{regexp.MustCompile(`(?i)\b(x-api-key|api-key|authorization)(["']?\s*[:=]\s*["']?)[^\s"',}]{8,}`), "$1$2[REDACTED]"},
	{regexp.MustCompile(`([a-z][a-z0-9+.\-]*://[^/\s:@]+:)[^@\s/]+@`), "$1[REDACTED]@"},
	// NAME=value, "name": "value", name: value and --name value
	{regexp.MustCompile(`(?i)\b([A-Z0-9_\-]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY))(["']?\s*[:=]\s*["']?)([^\s"',}]{6,})`), "${1}${2}[REDACTED]"},
	{regexp.MustCompile(`(?i)(--[a-z\-]*(?:password|secret|token|api-?key)[=\s]+)([^\s"']{6,})`), "${1}[REDACTED]"},
}

// Redact replaces secrets with placeholders. It runs on every event payload
// before the payload is hashed and signed, on evidence before it is sent to
// a judge, and on checker errors. It removes the exact values of credential
// environment variables (whatever their format) and common secret shapes.
// Pattern matching is best effort: a secret in an unknown format that is
// not in the environment can get through.
func Redact(s string) string {
	for _, v := range envSecrets() {
		s = strings.ReplaceAll(s, v, "[REDACTED:env]")
	}
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// envSecrets returns the values of credential-like environment variables,
// longest first so that a value containing another is replaced whole.
func envSecrets() []string {
	var vals []string
	for _, kv := range os.Environ() {
		name, v, _ := strings.Cut(kv, "=")
		if len(v) < 12 || !(secretName.MatchString(name) || contains(CredentialEnv, name)) {
			continue
		}
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return vals
}

// checkerEnv is the environment for command checkers: smem's own judge
// credentials are removed, since a checker has no business with them.
// Other variables pass through so checkers can reach their own systems.
func checkerEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !contains(CredentialEnv, name) {
			env = append(env, kv)
		}
	}
	return env
}

// redactPayload marshals payload with every string value redacted. It works
// on decoded values rather than the JSON text, so a replacement can never
// split an escape sequence and corrupt the payload.
func redactPayload(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(redactValue(v))
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return Redact(x)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = redactValue(x[k])
		}
	}
	return v
}
