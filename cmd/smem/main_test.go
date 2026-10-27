package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every command in the usage text must be registered, and vice versa.
func TestUsageMatchesCommands(t *testing.T) {
	listed := map[string]bool{}
	// "  name ..." and "  push [remote] | pull [remote]"
	for _, m := range regexp.MustCompile(`(?m)(?:^  |\| )([a-z][a-z-]+)`).FindAllStringSubmatch(usage, -1) {
		listed[m[1]] = true
	}
	for name := range listed {
		if _, ok := commands[name]; !ok && name != "version" {
			t.Errorf("%q is in the usage but not registered", name)
		}
	}
	for name := range commands {
		if !listed[name] {
			t.Errorf("%q is registered but not in the usage", name)
		}
	}
}

func TestMCPInitCreatesIgnoredStore(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("SMEM_DB", "")
	t.Setenv("SMEM_KEY_DIR", filepath.Join(dir, "keys"))
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_remember","arguments":{"content":"x","kind":"fact"}}}` + "\n")
	var out bytes.Buffer
	if err := run([]string{"mcp", "--init"}, in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mem-") {
		t.Fatalf("mcp: %s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, ".smem", ".gitignore"))
	if err != nil || !strings.Contains(string(b), "!config.json") {
		t.Fatalf(".smem/.gitignore: %q %v", b, err)
	}
	// Without --init and without a store, mcp refuses rather than scattering stores.
	t.Chdir(t.TempDir())
	if err := run([]string{"mcp"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("mcp without a store or --init should fail")
	}
}
