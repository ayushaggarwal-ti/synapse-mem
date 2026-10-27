package smem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// HookInput is the JSON Claude Code sends to hooks on stdin.
type HookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

// readOnlyTools are skipped by default: they change nothing, and recording
// every read would swamp the log.
var readOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true, "WebSearch": true, "WebFetch": true,
	"TodoWrite": true, "ToolSearch": true, "NotebookRead": true,
}

var commitLine = regexp.MustCompile(`\[[^\]\s]+(?: \([^)]*\))? ([0-9a-f]{7,40})\]`)

// RecordToolUse records a PostToolUse hook call as an action. The action is
// linked to the session's latest active decision, which is a heuristic; an
// explicit `smem action --decision` is more precise.
func (e *Engine) RecordToolUse(in HookInput, includeReadOnly bool) (string, error) {
	if in.ToolName == "" || (!includeReadOnly && readOnlyTools[in.ToolName]) {
		return "", nil
	}
	if in.SessionID != "" {
		e.Session = in.SessionID
	}
	dec, err := e.LatestDecision(e.Session)
	if err != nil {
		return "", err
	}
	return e.RecordAction(ActionInput{
		Tool:       in.ToolName,
		Summary:    summarizeTool(in.ToolInput),
		Decision:   dec,
		Commit:     commitFrom(in.ToolName, in.ToolInput, in.ToolResponse),
		ArgsHash:   hashRaw(in.ToolInput),
		ResultHash: hashRaw(in.ToolResponse),
		Transcript: in.TranscriptPath,
	})
}

func summarizeTool(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) == nil {
		for _, k := range []string{"command", "file_path", "notebook_path", "url", "pattern", "description", "prompt"} {
			if s, ok := m[k].(string); ok && s != "" {
				return oneLine(s, 240)
			}
		}
	}
	return oneLine(string(input), 240)
}

// commitFrom finds the sha in `git commit` output, e.g. "[main 1a2b3c4] msg".
func commitFrom(tool string, input, response json.RawMessage) string {
	if tool != "Bash" || !strings.Contains(string(input), "git commit") {
		return ""
	}
	text := string(response)
	var r struct {
		Stdout string `json:"stdout"`
	}
	if json.Unmarshal(response, &r) == nil && r.Stdout != "" {
		text = r.Stdout
	}
	if m := commitLine.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func hashRaw(b json.RawMessage) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
