package smem

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactPatterns(t *testing.T) {
	cases := map[string]string{
		"key sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123":                      "sk-ant-api03",
		"Authorization: Bearer abcdefghijklmnop1234567890":                     "abcdefghijklmnop1234567890",
		`{"password": "hunter2hunter2"}`:                                       "hunter2hunter2",
		"DB_PASSWORD=correcthorsebattery staple":                               "correcthorsebattery",
		"psql postgres://admin:s3cretpass@db.internal:5432/app":                "s3cretpass",
		"curl -H 'x-api-key: tsk_live_9f8e7d6c5b4a' https://api":               "tsk_live_9f8e7d6c5b4a",
		"deploy --api-key abcdef123456 --region us":                            "abcdef123456",
		"token ghp_" + strings.Repeat("a", 36):                                 "ghp_aaaa",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----": "MIIE",
	}
	for in, secret := range cases {
		if out := Redact(in); strings.Contains(out, secret) {
			t.Errorf("%q leaked through: %q", secret, out)
		}
	}
	for _, keep := range []string{"the api listens on port 8080", "token budget is 4000", "use the --password-file flag"} {
		if Redact(keep) != keep {
			t.Errorf("over-redacted %q to %q", keep, Redact(keep))
		}
	}
}

// TestCredentialsNeverStored plants credentials in the environment, drives
// every path that writes or sends data, then scans the database files, the
// export and what checkers and judges received.
func TestCredentialsNeverStored(t *testing.T) {
	// Formats the patterns don't know: only exact-value redaction catches them.
	jevKey := "tsjev_Q7m2Lx9Vb4Np8Rk3Wz6Hc1"
	anthKey := "zz-unknownformat-5t6y7u8i9o0p"
	other := "corp-vault-Zx81Lq0Mm2Nn3Bb4"
	t.Setenv("TYPESAFE_API_KEY", jevKey)
	t.Setenv("ANTHROPIC_API_KEY", anthKey)
	t.Setenv("BILLING_SERVICE_TOKEN", other)
	secrets := []string{jevKey, anthKey, other}

	c := start()
	e := newEngine(t, "alice", c)
	root := filepath.Dir(filepath.Dir(e.Path()))
	os.WriteFile(filepath.Join(root, "app.env"), []byte("PORT=9090\nKEY="+jevKey+"\n"), 0o644)
	src := must[string](t)(e.CaptureSource("file", "app.env", "", "note "+anthKey))
	m := must[string](t)(e.Remember(MemoryInput{Content: "the api uses key " + jevKey + " on port 8080", DerivedFrom: []string{src}}))
	must[string](t)(e.Remember(MemoryInput{Content: `config {"token":"` + other + `"} and a quote \" and ` + anthKey}))
	_, _, err := e.Decide(DecisionInput{Question: "which key?", Choice: "use " + anthKey, Rationale: "env had " + other, Used: []string{m}})
	ok(t, err)
	tool, _ := json.Marshal(map[string]any{"command": "curl -H 'Authorization: Bearer " + jevKey + "' https://api"})
	must[string](t)(e.RecordToolUse(HookInput{SessionID: "s1", ToolName: "Bash", ToolInput: tool, ToolResponse: json.RawMessage(`"` + anthKey + `"`)}, false))

	// A command checker that tries to read and echo the judge keys.
	script := filepath.Join(root, "env.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"{\\\"evidence\\\":[{\\\"id\\\":\\\"env\\\",\\\"text\\\":\\\"jev=$TYPESAFE_API_KEY billing=$BILLING_SERVICE_TOKEN\\\"}]}\"\necho \"stderr $ANTHROPIC_API_KEY\" >&2\n"), 0o755)
	e.Checkers = []Checker{&CommandChecker{CheckName: "env", Command: []string{script}}}
	jev := &scripted{name: "jev", choice: "supports", conf: 0.95, subject: 0.9}
	e.Judges = stages(jev)
	res, err := e.FactCheck(context.Background(), m, true)
	ok(t, err)

	sent, _ := json.Marshal(jev.seen)
	if strings.Contains(string(sent), "jev=tsjev") {
		t.Fatal("command checkers must not receive smem's judge keys")
	}
	var export bytes.Buffer
	_, err = e.Export(&export, "")
	ok(t, err)
	out, _ := json.Marshal(res)
	ok(t, e.Close())
	blobs := map[string][]byte{"judge request": sent, "export": export.Bytes(), "result": out}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if b, err := os.ReadFile(e.Path() + suffix); err == nil {
			blobs["db"+suffix] = b
		}
	}
	for name, b := range blobs {
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains a credential (%s...)", name, s[:6])
			}
		}
	}
}
