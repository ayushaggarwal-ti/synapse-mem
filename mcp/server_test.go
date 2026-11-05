package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rahulsub/synapse-mem/judge"
	"github.com/rahulsub/synapse-mem/smem"
)

func open(t *testing.T) *smem.Engine {
	t.Helper()
	dir := t.TempDir()
	e, err := smem.Open(filepath.Join(dir, ".smem", "smem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	e.KeyDir, e.Actor = filepath.Join(dir, "keys"), "tester"
	return e
}

type reply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// session runs the server over the given messages and returns the replies.
func session(t *testing.T, s *Server, msgs ...any) []reply {
	t.Helper()
	var in, out bytes.Buffer
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		in.Write(append(b, '\n'))
	}
	if err := s.Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	var rs []reply
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r reply
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return rs
}

func call(id int, name string, args any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
}

// text returns a tool result's text and whether it was an error.
func text(t *testing.T, r reply) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("rpc error: %+v", r.Error)
	}
	var res struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		t.Fatalf("result %s: %v", r.Result, err)
	}
	return res.Content[0].Text, res.IsError
}

func TestHandshakeAndToolSchemas(t *testing.T) {
	e := open(t)
	rs := session(t, &Server{Engine: e, Version: "1.2.3", ActorFromClient: true},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "Claude Code", "version": "2"}}},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "nope"},
		map[string]any{"jsonrpc": "2.0", "id": 4, "method": "ping"},
	)
	if len(rs) != 4 {
		t.Fatalf("notifications get no reply: %d replies", len(rs))
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct{ Version string }
	}
	json.Unmarshal(rs[0].Result, &init)
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Version != "1.2.3" {
		t.Fatalf("initialize: %s", rs[0].Result)
	}
	if !strings.HasPrefix(e.Actor, "claude-code@") && e.Actor != "claude-code" {
		t.Fatalf("actor should come from the client: %q", e.Actor)
	}
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		}
	}
	json.Unmarshal(rs[1].Result, &list)
	if len(list.Tools) != len(tools) {
		t.Fatalf("tools/list: %s", rs[1].Result)
	}
	for _, tl := range list.Tools {
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("%s: incomplete schema", tl.Name)
		}
		// Every required field must be a declared property.
		props, _ := tl.InputSchema["properties"].(map[string]any)
		req, _ := tl.InputSchema["required"].([]any)
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s requires undeclared %v", tl.Name, r)
			}
		}
	}
	if rs[2].Error == nil || rs[2].Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", rs[2])
	}
}

func TestRememberRecallDecideWhy(t *testing.T) {
	e := open(t)
	s := &Server{Engine: e, Version: "test"}
	rs := session(t, s,
		call(1, "memory_remember", map[string]any{"content": "the api listens on port 8080", "kind": "fact", "derived_from": []string{"app.env"}}),
		call(2, "memory_remember", map[string]any{"content": "missing kind"}),
	)
	out, isErr := text(t, rs[0])
	var mem struct{ ID string }
	json.Unmarshal([]byte(out), &mem)
	if isErr || !strings.HasPrefix(mem.ID, "mem-") {
		t.Fatalf("remember: %s", out)
	}
	if msg, isErr := text(t, rs[1]); !isErr || !strings.Contains(msg, "kind is required") {
		t.Fatalf("a bad call is a tool error the model can read: %s", msg)
	}

	rs = session(t, s,
		call(3, "memory_recall", map[string]any{"query": "api port"}),
		call(4, "decision_record", map[string]any{
			"question": "which port in the firewall rule?", "choice": "8080", "why": "that is where the api listens",
			"rejected":    []any{map[string]any{"option": "80", "reason": "nothing listens there"}},
			"assumptions": []any{map[string]any{"statement": "the port is not changing this quarter"}},
			"used":        []string{mem.ID},
		}),
	)
	out, _ = text(t, rs[0])
	var hits []hit
	json.Unmarshal([]byte(out), &hits)
	if len(hits) != 1 || hits[0].ID != mem.ID || hits[0].Status != "" {
		t.Fatalf("recall: %s", out)
	}
	out, _ = text(t, rs[1])
	var dec struct {
		ID            string
		CommitTrailer string `json:"commit_trailer"`
	}
	json.Unmarshal([]byte(out), &dec)
	if !strings.HasPrefix(dec.ID, "dec-") || dec.CommitTrailer != "Decision: "+dec.ID {
		t.Fatalf("decide: %s", out)
	}

	// The fact changes: the decision that used it is flagged, and why explains it.
	rs = session(t, s,
		call(5, "memory_update", map[string]any{"id": mem.ID, "content": "the api listens on port 9090"}),
		call(6, "review", map[string]any{}),
		call(7, "why", map[string]any{"ref": dec.ID}),
		call(8, "decision_outcome", map[string]any{"id": dec.ID, "outcome": "failure", "note": "port moved"}),
	)
	out, _ = text(t, rs[0])
	if !strings.Contains(out, dec.ID) {
		t.Fatalf("update should report the flagged decision: %s", out)
	}
	if out, _ = text(t, rs[1]); !strings.Contains(out, dec.ID) {
		t.Fatalf("review: %s", out)
	}
	out, _ = text(t, rs[2])
	for _, want := range []string{"which port in the firewall rule?", "nothing listens there", "the port is not changing"} {
		if !strings.Contains(out, want) {
			t.Fatalf("why should be readable text containing %q:\n%s", want, out)
		}
	}
	if _, isErr := text(t, rs[3]); isErr {
		t.Fatal("outcome")
	}
}

func TestSessionFollowsHook(t *testing.T) {
	e := open(t)
	if err := e.SetCurrentSession("cc-123"); err != nil {
		t.Fatal(err)
	}
	e.Session = ""
	t.Setenv("SMEM_SESSION", "")
	session(t, &Server{Engine: e}, call(1, "memory_recall", map[string]any{"query": "x"}))
	if e.Session != "cc-123" {
		t.Fatalf("the server should file records under the hook's session, got %q", e.Session)
	}
	session(t, &Server{Engine: e, Session: "explicit"}, call(1, "memory_recall", map[string]any{"query": "x"}))
	if e.Session != "explicit" {
		t.Fatal("an explicit session wins")
	}
}

type slowJudge struct{ release chan struct{} }

func (s slowJudge) Name() string { return "slow" }
func (s slowJudge) Ask(ctx context.Context, r judge.Request) (*judge.Result, error) {
	<-s.release
	return nil, errors.New("gave up")
}

func TestFactCheckDoesNotBlockOtherCalls(t *testing.T) {
	e := open(t)
	release := make(chan struct{})
	e.Judges = judge.Cascade{Stages: []judge.Stage{{Judge: slowJudge{release}}}}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Engine: e}
	done := make(chan error)
	go func() { done <- s.Serve(context.Background(), inR, outW); outW.Close() }()
	dec := json.NewDecoder(outR)
	send := func(m any) { b, _ := json.Marshal(m); inW.Write(append(b, '\n')) }
	recv := func() reply { var r reply; dec.Decode(&r); return r }

	send(call(1, "memory_remember", map[string]any{"content": "the api listens on port 8080", "kind": "fact"}))
	out, _ := text(t, recv())
	var mem struct{ ID string }
	json.Unmarshal([]byte(out), &mem)
	send(call(2, "memory_remember", map[string]any{"content": "the api moved to port 9090", "kind": "fact"}))
	recv()
	send(call(3, "factcheck", map[string]any{"ids": []string{mem.ID}}))
	send(call(4, "memory_recall", map[string]any{"query": "api port"}))
	if r := recv(); r.ID != 4 {
		t.Fatalf("recall must answer while the fact-check waits on its judge; got id %d first", r.ID)
	}
	close(release)
	if r := recv(); r.ID != 3 {
		t.Fatalf("then the fact-check: %d", r.ID)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDisabledToolsAreHidden(t *testing.T) {
	e := open(t)
	e.Policy.MCP.Disable = []string{"memory_remember"}
	e.Policy.MCP.Instructions = "Use bd remember for plain memories; smem for decisions."
	rs := session(t, &Server{Engine: e},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		call(3, "memory_remember", map[string]any{"content": "x", "kind": "fact"}),
	)
	if !strings.Contains(string(rs[0].Result), "bd remember") || strings.Contains(string(rs[1].Result), "memory_remember") {
		t.Fatalf("instructions and tool list follow mcp config: %s %s", rs[0].Result, rs[1].Result)
	}
	if rs[2].Error == nil {
		t.Fatal("a disabled tool can't be called")
	}
}
