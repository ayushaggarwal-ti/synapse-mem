package smem

import (
	"slices"
	"strings"
	"testing"
)

const (
	officeMem    = "Office espresso machine outages are reported to facilities"
	stagingPager = "The staging database outages page the on-call engineer"
	stagingLow   = "Staging database outages are run by the incident commander"
)

func coverageStore(t *testing.T) (e *Engine, office, pager, low string) {
	t.Helper()
	e = newEngine(t, "alice", start())
	office = remember(t, e, "fact", officeMem)
	pager = remember(t, e, "fact", stagingPager)
	low = must[string](t)(e.Remember(MemoryInput{Kind: "fact", Content: stagingLow, Confidence: 0.01}))
	return
}

func recallIDs(t *testing.T, e *Engine, q RecallQuery) []string {
	t.Helper()
	q.NoRecord = true
	return ids(must[[]Scored](t)(e.Recall(q)))
}

func TestRecallDropsSingleWordCollisions(t *testing.T) {
	e, office, pager, low := coverageStore(t)
	got := recallIDs(t, e, RecallQuery{Text: "staging database outages"})
	if slices.Contains(got, office) || !slices.Contains(got, pager) || !slices.Contains(got, low) || len(got) != 2 {
		t.Fatalf("want only the two staging memories, got %v", got)
	}
}

func TestRecallCollisionAloneIsEmptyAndUnrecorded(t *testing.T) {
	e := newEngine(t, "alice", start())
	remember(t, e, "fact", officeMem)
	hits := must[[]Scored](t)(e.Recall(RecallQuery{Text: "staging database outages"}))
	if len(hits) != 0 {
		t.Fatalf("a filtered no-match must stay empty, got %v", ids(hits))
	}
	if n := len(must[[]Event](t)(e.Events("", evRecall, 50))); n != 0 {
		t.Fatalf("no hits, no recall event; got %d", n)
	}
}

func TestRecallCoverageOfThreeWordQueries(t *testing.T) {
	e, office, _, _ := coverageStore(t)
	if got := recallIDs(t, e, RecallQuery{Text: "kubernetes rollout espresso"}); len(got) != 0 {
		t.Fatalf("one of three words must not match: %v", got)
	}
	if got := recallIDs(t, e, RecallQuery{Text: "kubernetes rollout teleport"}); len(got) != 0 {
		t.Fatalf("no overlap: %v", got)
	}
	two := remember(t, e, "fact", "kubernetes rollout notes")
	hits := must[[]Scored](t)(e.Recall(RecallQuery{Text: "kubernetes rollout espresso", NoRecord: true}))
	if len(hits) != 1 || hits[0].ID != two || hits[0].Relevance != 1 || slices.Contains(ids(hits), office) {
		t.Fatalf("a two-of-three match survives at relevance 1: %+v", hits)
	}
}

func TestRecallCoverageUsesStemsAndPrefixes(t *testing.T) {
	e := newEngine(t, "alice", start())
	m := remember(t, e, "fact", "rotating logs daily")
	if got := recallIDs(t, e, RecallQuery{Text: "log rotate weekly"}); !slices.Contains(got, m) {
		t.Fatalf("stemmed matches count toward coverage: %v", got)
	}
}

func TestRecallSingleWordUnchanged(t *testing.T) {
	e, office, pager, low := coverageStore(t)
	got := recallIDs(t, e, RecallQuery{Text: "outages"})
	if len(got) != 3 || !slices.Contains(got, office) || !slices.Contains(got, pager) || !slices.Contains(got, low) {
		t.Fatalf("a single word has no gate: %v", got)
	}
	if got := recallIDs(t, e, RecallQuery{Text: "outage outage"}); len(got) != 3 {
		t.Fatalf("a repeated word is still one word: %v", got)
	}
}

func TestRecallEmptyQueryKeepsEverything(t *testing.T) {
	e, office, pager, low := coverageStore(t)
	got := recallIDs(t, e, RecallQuery{})
	if len(got) != 3 || !slices.Contains(got, office) || !slices.Contains(got, pager) || !slices.Contains(got, low) {
		t.Fatalf("empty query returns all: %v", got)
	}
	if got := recallIDs(t, e, RecallQuery{Text: " !? "}); len(got) != 3 {
		t.Fatalf("punctuation only is empty: %v", got)
	}
	ctx := must[string](t)(e.Context(0))
	for _, s := range []string{"espresso", "staging database outages page", "incident commander"} {
		if !strings.Contains(ctx, s) {
			t.Fatalf("session context lost %q:\n%s", s, ctx)
		}
	}
}

func TestRecallManyFullMatchesSkipTheGate(t *testing.T) {
	e := newEngine(t, "alice", start())
	for range 20 {
		remember(t, e, "fact", "alpha beta shared")
	}
	solo := remember(t, e, "fact", "alpha only")
	got := recallIDs(t, e, RecallQuery{Text: "alpha beta", Limit: 100})
	if len(got) != 20 || slices.Contains(got, solo) {
		t.Fatalf("20 full matches fill the candidate set: %d", len(got))
	}
}

func TestFactCheckEvidenceKeepsLooselyRelatedMemories(t *testing.T) {
	e := newEngine(t, "alice", start())
	m := remember(t, e, "fact", "the staging database is rebuilt nightly")
	rel := remember(t, e, "fact", "database passwords rotate monthly")
	ev := must[[]Evidence](t)(e.gatherEvidence(must[Memory](t)(e.Memory(m)), e.FactCheckPolicy))
	if !slices.ContainsFunc(ev, func(x Evidence) bool { return x.ID == rel }) {
		t.Fatalf("related evidence must include a one-word overlap: %+v", ev)
	}
}
