package smem

import "regexp"

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`), "[REDACTED:anthropic-key]"},
	{regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "[REDACTED:aws-key]"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`xox[abprs]-[A-Za-z0-9\-]{10,}`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY))=([^\s"\\]{6,})`), "${1}=[REDACTED]"},
}

// Redact replaces common secret shapes with placeholders. It runs on every
// event payload before the payload is hashed and signed.
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
