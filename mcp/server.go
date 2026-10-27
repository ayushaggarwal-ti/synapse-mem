// Package mcp serves an smem store to agents over the Model Context Protocol
// (JSON-RPC 2.0, one message per line on stdio).
//
// Requests are handled one at a time: the store has a single writer, and a
// local agent sends one tool call at a time anyway.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rahulsub/synapse-mem/smem"
)

// protocolVersions are the MCP revisions this server speaks, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = `smem is your long-term memory and decision log for this project.
- Start of a task: memory_recall what you know about it.
- Learned something durable (a fact about the system, a user preference, a procedure that worked): memory_remember it. Not transient chatter.
- A remembered fact turned out wrong: memory_update it if it changed, memory_invalidate it if it no longer holds.
- Made a non-obvious choice: decision_record it with the options you rejected, your assumptions, and the memory ids you relied on. Report how it went with decision_outcome.
- Asked why something is the way it is: why.`

// Server serves one store.
type Server struct {
	Engine  *smem.Engine
	Version string
	// Session, if set, overrides the session the store's hook recorded.
	Session string
	// ActorFromClient signs as the connecting client (e.g. claude-code)
	// instead of Engine.Actor.
	ActorFromClient bool
}

var unsafeActor = regexp.MustCompile(`[^a-z0-9._-]+`)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from in and writes responses to out until in ends.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification: nothing to answer
		}
		result, rerr := s.handle(ctx, req)
		if err := enc.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		json.Unmarshal(req.Params, &p)
		if name := strings.Trim(unsafeActor.ReplaceAllString(strings.ToLower(p.ClientInfo.Name), "-"), "-."); s.ActorFromClient && name != "" {
			s.Engine.Actor = name
		}
		v := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "smem", "version": s.Version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, len(tools))
		for i, t := range tools {
			list[i] = map[string]any{"name": t.name, "description": t.desc, "inputSchema": t.schema}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		i := slices.IndexFunc(tools, func(t tool) bool { return t.name == p.Name })
		if i < 0 {
			return nil, &rpcError{-32602, "unknown tool " + p.Name}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		s.syncSession()
		out, err := tools[i].call(ctx, s.Engine, p.Arguments)
		// Tool failures go back to the model as results it can act on,
		// not as protocol errors.
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		if p, ok := out.(plain); ok {
			return textResult(string(p), false), nil
		}
		b, err := json.MarshalIndent(out, "", " ")
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		return textResult(string(b), false), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// syncSession picks up the session the hook recorded most recently, so that
// recalls, decisions and the hook's actions land in the same session.
func (s *Server) syncSession() {
	switch {
	case s.Session != "":
		s.Engine.Session = s.Session
	case os.Getenv("SMEM_SESSION") != "":
	default:
		if id := s.Engine.CurrentSession(); id != "" {
			s.Engine.Session = id
		}
	}
}

var zeroTime time.Time

func round(x float64) float64 { return math.Round(x*1000) / 1000 }

func textResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// tool is one MCP tool. call decodes its own arguments.
type tool struct {
	name   string
	desc   string
	schema map[string]any
	call   func(context.Context, *smem.Engine, json.RawMessage) (any, error)
}

// handler adapts a typed function to a tool call.
func handler[A any](fn func(context.Context, *smem.Engine, A) (any, error)) func(context.Context, *smem.Engine, json.RawMessage) (any, error) {
	return func(ctx context.Context, e *smem.Engine, raw json.RawMessage) (any, error) {
		var a A
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}
		return fn(ctx, e, a)
	}
}

func required(fields map[string]string) error {
	for name, v := range fields {
		if v == "" {
			return errors.New(name + " is required")
		}
	}
	return nil
}
