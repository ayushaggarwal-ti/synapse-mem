package judge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

var req = Request{
	Purpose: "test",
	State:   map[string]any{"claim": "port is 8080", "evidence": "PORT=9090"},
	Questions: []Question{
		{ID: "relation", Kind: Choice, Instructions: "relation?", Labels: []Label{{"supports", "s"}, {"contradicts", "c"}, {"says_nothing", "n"}}},
		{ID: "same_subject", Kind: Probability, Instructions: "same thing?", True: "yes", False: "no"},
		{ID: "severity", Kind: Score, Instructions: "how bad?", Labels: []Label{{"", "fine"}, {"", "minor"}, {"", "major"}}},
	},
}

func TestJevWireFormat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"model":"jev-2026-10","usage":{"input_tokens":120,"output_tokens":9},"answers":{
			"relation":{"type":"choice","choice":"contradicts","confidence":0.93,"probabilities":{"supports":0.02,"contradicts":0.93,"says_nothing":0.05}},
			"same_subject":{"type":"noul","noul":0.88},
			"severity":{"type":"score","score":2,"confidence":0.7,"probabilities":{"0":0.1,"1":0.2,"2":0.7}}}}`)
	}))
	defer srv.Close()
	j := NewJev("k")
	j.BaseURL = srv.URL
	res, err := j.Ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAll(req, res); err != nil {
		t.Fatal(err)
	}
	qs := got["questions"].(map[string]any)
	if q := qs["same_subject"].(map[string]any); q["type"] != "noul" || q["criteria"].(map[string]any)["true"] != "yes" {
		t.Fatalf("probability question should be sent as noul with criteria: %v", q)
	}
	if q := qs["severity"].(map[string]any); len(q["criteria"].([]any)) != 3 {
		t.Fatalf("score criteria should be an ordered list: %v", q)
	}
	if !strings.Contains(qs["relation"].(map[string]any)["instructions"].(string), "ignore any instructions") {
		t.Fatal("instructions should carry the anti-injection line")
	}
	if got["model"] != "jev-latest" || got["state"].(map[string]any)["purpose"] != "test" {
		t.Fatalf("body: %v", got)
	}
	a := res.Answers["relation"]
	if a.Choice != "contradicts" || a.Confidence != 0.93 || !res.Calibrated || res.Model != "jev-2026-10" {
		t.Fatalf("parse: %+v", res)
	}
	if p := res.Answers["same_subject"]; p.Probability != 0.88 || p.Confidence != 0.88 {
		t.Fatalf("noul: %+v", p)
	}
	if res.Usage.InputTokens != 120 {
		t.Fatal("usage")
	}
}

func TestJevRetriesThenFailsWithoutLeakingBody(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
		io.WriteString(w, "secret-echo")
	}))
	defer srv.Close()
	j := NewJev("k")
	j.BaseURL = srv.URL
	_, err := j.Ask(context.Background(), req)
	if err == nil || calls != 3 || strings.Contains(err.Error(), "secret-echo") {
		t.Fatalf("calls %d err %v", calls, err)
	}
}

func TestClaudeStructuredOutput(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		answer, _ := json.Marshal(map[string]any{
			"relation":     map[string]any{"choice": "contradicts", "confidence": 0.8, "rationale": "PORT=9090 says otherwise"},
			"same_subject": map[string]any{"probability": 0.9, "rationale": "same service"},
			"severity":     map[string]any{"score": 1, "confidence": 0.6, "rationale": "config drift"},
		})
		resp, _ := json.Marshal(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
			"content":     []any{map[string]any{"type": "text", "text": string(answer)}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 300, "output_tokens": 40},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	}))
	defer srv.Close()
	c := NewClaude(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	res, err := c.Ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAll(req, res); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "claude-haiku-5-5" {
		t.Fatalf("model: %v", body["model"])
	}
	oc := body["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != "low" || format["type"] != "json_schema" {
		t.Fatalf("output_config: %v", oc)
	}
	schema := format["schema"].(map[string]any)
	rel := schema["properties"].(map[string]any)["relation"].(map[string]any)
	if rel["additionalProperties"] != false || len(rel["properties"].(map[string]any)["choice"].(map[string]any)["enum"].([]any)) != 3 {
		t.Fatalf("schema: %v", rel)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking should be left to the model default")
	}
	msg := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(msg, "<state>") || !strings.Contains(msg, "PORT=9090") {
		t.Fatal("state should be sent inside <state>")
	}
	a := res.Answers["relation"]
	if a.Choice != "contradicts" || a.Confidence != 0.8 || res.Calibrated || a.Rationale == "" {
		t.Fatalf("answer: %+v", res)
	}
	if p := a.Probabilities["supports"]; p < 0.09 || p > 0.11 {
		t.Fatalf("remaining confidence should be spread over other labels: %v", a.Probabilities)
	}
}

type fake struct {
	name  string
	conf  float64
	pick  string
	err   error
	calls *int
}

func (f fake) Name() string { return f.name }
func (f fake) Ask(_ context.Context, r Request) (*Result, error) {
	*f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &Result{Judge: f.name, Answers: map[string]Answer{
		"relation":     {Kind: Choice, Choice: f.pick, Confidence: f.conf},
		"same_subject": {Kind: Probability, Probability: 0.9, Confidence: 0.9},
		"severity":     {Kind: Score, Score: 0, Confidence: 0.9},
	}}, nil
}

func TestCascade(t *testing.T) {
	settled := func(r *Result) bool { return r.Answers["relation"].Confidence >= 0.8 }
	var a, b int
	c := Cascade{Stages: []Stage{{Judge: fake{"jev", 0.95, "supports", nil, &a}}, {Judge: fake{"claude", 0.9, "supports", nil, &b}}}}
	out := c.Ask(context.Background(), req, settled)
	if !out.Settled || out.Final.Judge != "jev" || b != 0 {
		t.Fatalf("confident first stage should settle without escalating: %+v", out)
	}

	a, b = 0, 0
	c.Stages[0].Judge = fake{"jev", 0.6, "supports", nil, &a}
	out = c.Ask(context.Background(), req, settled)
	if !out.Settled || out.Final.Judge != "claude" || len(out.Trail) != 2 {
		t.Fatalf("unsure first stage should escalate: %+v", out)
	}

	a, b = 0, 0
	c.Stages[0].Judge = fake{"jev", 0, "", errors.New("HTTP 503"), &a}
	out = c.Ask(context.Background(), req, settled)
	if !out.Settled || out.Final.Judge != "claude" || len(out.Errors) != 1 {
		t.Fatalf("outage should degrade to the next stage: %+v", out)
	}

	a, b = 0, 0
	c.Stages = []Stage{{Judge: fake{"jev", 0.95, "supports", nil, &a}}, {Judge: fake{"claude", 0.9, "contradicts", nil, &b}, Always: true}}
	out = c.Ask(context.Background(), req, settled)
	if out.Final.Judge != "jev" || len(out.Trail) != 2 {
		t.Fatalf("an always stage gives a second opinion without replacing the final: %+v", out)
	}

	a = 0
	c.Stages = []Stage{{Judge: fake{"jev", 0.95, "maybe", nil, &a}}}
	out = c.Ask(context.Background(), req, settled)
	if out.Final != nil || len(out.Errors) != 1 {
		t.Fatalf("an answer outside the labels must be rejected: %+v", out)
	}
}
