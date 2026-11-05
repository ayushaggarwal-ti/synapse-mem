package smem

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The ungated numbers for the coverage fixture and "staging database outages".
var ungated = []struct {
	prefix         string
	relevance, sco float64
}{
	{"The staging ", 1, 0.786263},
	{"Office espre", 0.174789, 0.137430},
	{"Staging data", 1, 0.009828},
}

func checkUngated(t *testing.T, name string, hits []Scored) {
	t.Helper()
	if len(hits) != len(ungated) {
		t.Fatalf("%s: broad returns every match, got %d", name, len(hits))
	}
	for i, want := range ungated {
		h := hits[i]
		if h.Content[:len(want.prefix)] != want.prefix ||
			math.Abs(h.Relevance-want.relevance) > 1e-5 || math.Abs(h.Score-want.sco) > 1e-5 {
			t.Fatalf("%s: hit %d is %q rel %.6f score %.6f", name, i, h.Content, h.Relevance, h.Score)
		}
	}
}

func TestRecallBroadRestoresOneWordMatches(t *testing.T) {
	e, _, _, _ := coverageStore(t)
	q := RecallQuery{Text: "staging database outages", NoRecord: true, Broad: true}
	checkUngated(t, "query", must[[]Scored](t)(e.Recall(q)))

	q.Broad = false
	e.Policy.Recall.Broad = true
	checkUngated(t, "policy", must[[]Scored](t)(e.Recall(q)))
}

func TestRecallPolicyBroadConfig(t *testing.T) {
	if DefaultPolicy().Recall.Broad {
		t.Fatal("gated by default")
	}
	load := func(body string) *Config {
		p := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return must[*Config](t)(LoadConfig(p))
	}
	if load(`{"recall":{"dedupe_window":"5m"}}`).Recall.Broad {
		t.Fatal("a partial recall config keeps the gate on")
	}
	if !load(`{"recall":{"broad":true}}`).Recall.Broad {
		t.Fatal("recall.broad turns the gate off")
	}
}
