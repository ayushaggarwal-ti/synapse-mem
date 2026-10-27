package smem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// CredentialEnv lists the environment variables smem reads credentials from.
// Their values are always redacted, and they are never passed to checkers.
var CredentialEnv = []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// secretName matches environment variable names that hold secrets.
var secretName = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|_KEY$|^KEY$|PRIVATE_?KEY|CREDENTIAL|AUTH|_PAT$|COOKIE|SESSION_?ID|DSN|WEBHOOK)`)

// notSecret are values too common to be secrets; redacting them everywhere
// would mangle ordinary text.
var notSecret = map[string]bool{"true": true, "false": true, "yes": true, "no": true, "none": true, "null": true,
	"bearer": true, "basic": true, "enabled": true, "disabled": true, "default": true}

type secretPattern struct {
	re   *regexp.Regexp
	repl string
}

// The value class used after a secret-looking name: no whitespace, quotes,
// backslashes (so a JSON escape is never split), commas or braces.
const val = `[^\s"'\\,;}{]`

var builtinPatterns = []secretPattern{
	// Whole PEM blocks, and fragments: a header with no footer, or a footer
	// with the base64 body before it.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|$)`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`(?s)(?:[A-Za-z0-9+/=]{16,}\s*){2,}-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`), "[REDACTED:anthropic-key]"},
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{10,}`), "[REDACTED:stripe-key]"},
	{regexp.MustCompile(`\bwhsec_[A-Za-z0-9]{16,}`), "[REDACTED:webhook-secret]"},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`), "[REDACTED:google-key]"},
	{regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]{20,}`), "[REDACTED:google-token]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}`), "[REDACTED:aws-key]"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`), "[REDACTED:gitlab-token]"},
	{regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`), "[REDACTED:npm-token]"},
	{regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`), "[REDACTED:hf-token]"},
	{regexp.MustCompile(`\bxai-[A-Za-z0-9]{40,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`xox[abeprs]-[A-Za-z0-9\-]{10,}`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic|Token)\s+[A-Za-z0-9._~+/\-]{12,}=*`), "$1 [REDACTED]"},
	{regexp.MustCompile(`(?i)\b(x-api-key|api-key|authorization|cookie|set-cookie)(["']?\s*[:=]\s*["']?)` + val + `{6,}`), "$1$2[REDACTED]"},
	{regexp.MustCompile(`([a-z][a-z0-9+.\-]*://[^/\s:@]+:)[^@\s/\\]+@`), "$1[REDACTED]@"},
	// NAME=value, NAME: value, "name": "value" and --name value for names
	// that look secret, including any *_KEY / *-key.
	{regexp.MustCompile(`(?i)\b([A-Z0-9_\-]*(?:PASSWORD|PASSWD|PASSPHRASE|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIALS?|_KEY|-KEY))(["']?\s*[:=]\s*["']?)(` + val + `{3,})`), "${1}${2}[REDACTED]"},
	{regexp.MustCompile(`(?i)(--[a-z\-]*(?:password|passphrase|secret|token|api-?key|access-?key)[=\s]+)([^\s"']{3,})`), "${1}[REDACTED]"},
}

// Redactor removes secrets from text. It removes the exact values of
// credential environment variables, whatever their format, then common
// secret shapes, then any configured patterns.
type Redactor struct {
	patterns  []secretPattern
	secretEnv []string
	minLen    int
	envVals   []string // credential values, read from the environment once
}

// NewRedactor builds a redactor from policy. Policy only adds to the
// built-in rules.
func NewRedactor(p RedactionPolicy) (*Redactor, error) {
	r := &Redactor{patterns: builtinPatterns, secretEnv: p.SecretEnv, minLen: p.MinEnvValue}
	if r.minLen <= 0 {
		r.minLen = 6
	}
	for _, s := range p.Patterns {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("redaction pattern %q: %w", s, err)
		}
		r.patterns = append(r.patterns, secretPattern{re, "[REDACTED]"})
	}
	r.envVals = r.envSecrets()
	return r, nil
}

// DefaultRedactor uses only the built-in rules.
func DefaultRedactor() *Redactor {
	r, _ := NewRedactor(RedactionPolicy{})
	return r
}

// Redact applies the default redactor.
func Redact(s string) string { return DefaultRedactor().Redact(s) }

// Redact replaces secrets with placeholders. It runs on every event payload
// before the payload is hashed and signed, on evidence before it is sent to
// a judge, and on checker errors. Pattern matching is best effort: a secret
// in an unknown format that is not in the environment can get through.
func (r *Redactor) Redact(s string) string {
	for _, v := range r.envVals {
		s = strings.ReplaceAll(s, v, "[REDACTED:env]")
	}
	for _, p := range r.patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// envSecrets returns the values of credential-like environment variables
// (read when the redactor is built: a long-running process keeps the
// environment it started with),
// longest first so that a value containing another is replaced whole.
func (r *Redactor) envSecrets() []string {
	var vals []string
	for _, kv := range os.Environ() {
		name, v, _ := strings.Cut(kv, "=")
		named := contains(CredentialEnv, name) || contains(r.secretEnv, name)
		if !named && !secretName.MatchString(name) {
			continue
		}
		if len(v) < r.minLen || notSecret[strings.ToLower(v)] {
			continue
		}
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return vals
}

// payload marshals payload with every string value redacted. It works on
// decoded values rather than the JSON text, so a replacement can never split
// an escape sequence and corrupt the payload.
func (r *Redactor) payload(payload any) ([]byte, error) {
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
	return json.Marshal(r.value(v))
}

func (r *Redactor) value(v any) any {
	switch x := v.(type) {
	case string:
		return r.Redact(x)
	case []any:
		for i := range x {
			x[i] = r.value(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = r.value(x[k])
		}
	}
	return v
}

// checkerEnvBase is what a command checker sees of the environment, plus
// the names its config lists. Credentials smem holds are never passed.
var checkerEnvBase = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "TMPDIR"}

func checkerEnv(extra []string) []string {
	var env []string
	for _, name := range append(append([]string(nil), checkerEnvBase...), extra...) {
		if contains(CredentialEnv, name) {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}
