package smem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahulsub/synapse-mem/judge"
)

func propose(t *testing.T, e *Engine, in RuleInput) *RuleOutcome {
	t.Helper()
	return must[*RuleOutcome](t)(e.ProposeRule(in))
}

func TestRulesNeedAReason(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)

	// What agents do on their own: a rule with no reason. Kept, not in force.
	o := propose(t, e, RuleInput{Text: "Always double-check imports before saving"})
	if o.Admitted || !strings.Contains(o.Reason, "no reason") {
		t.Fatalf("an unjustified rule must be held: %+v", o)
	}
	if rs := must[[]Rule](t)(e.Rules("active")); len(rs) != 0 {
		t.Fatal("held rules aren't active")
	}

	// "Someone asked" needs who and what they said.
	o = propose(t, e, RuleInput{Text: "Run make test before every commit", Why: Justification{Kind: "requested", By: "rahul"}})
	if o.Admitted || !strings.Contains(o.Reason, "words") {
		t.Fatalf("a request needs the person's words: %+v", o)
	}
	o = propose(t, e, RuleInput{Text: "Run make test before every commit",
		Why: Justification{Kind: "requested", By: "rahul", Quote: "please always run the tests before you commit"}})
	if !o.Admitted || o.Reason != "requested by rahul" {
		t.Fatalf("an explicit request admits it: %+v", o)
	}

	// A person can approve a held proposal.
	held := must[[]Rule](t)(e.Rules("proposed"))
	o = must[*RuleOutcome](t)(e.ApproveRule(held[0].ID, "rahul", "fine, keep it"))
	if !o.Admitted || must[Rule](t)(e.Rule(held[0].ID)).Status != "active" {
		t.Fatalf("approved: %+v", o)
	}
	if _, err := e.ApproveRule(held[1].ID, "", ""); err == nil {
		t.Fatal("approval needs a name")
	}
}

func TestIncidentsPromoteARule(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	o := propose(t, e, RuleInput{Text: "Run migrations in a transaction", Label: "migration-left-half-applied",
		Why: Justification{Kind: "incident"}})
	if o.Admitted || !strings.Contains(o.Reason, "only 0 failure") {
		t.Fatalf("no failures yet: %+v", o)
	}
	must[string](t)(func() (string, error) {
		id, promoted, err := e.RecordFailure(FailureInput{Label: "migration left half applied", Summary: "0042 died midway on staging"})
		if len(promoted) != 0 {
			t.Fatal("one failure isn't a pattern")
		}
		return id, err
	}())
	c.advance(24 * time.Hour)
	_, promoted, err := e.RecordFailure(FailureInput{Label: "migration-left-half-applied", Summary: "0043 too", Ref: "dojo-j6lv.2"})
	ok(t, err)
	if len(promoted) != 1 || promoted[0] != o.ID {
		t.Fatalf("the second failure with the label promotes it: %v", promoted)
	}
	r := must[Rule](t)(e.Rule(o.ID))
	if r.Status != "active" || !strings.Contains(r.Basis, `after 2 "migration-left-half-applied" failures`) {
		t.Fatalf("basis: %+v", r)
	}
	tr := must[*Trace](t)(e.Why(o.ID)).Text()
	if !strings.Contains(tr, "0042 died midway") || !strings.Contains(tr, "admitted: after 2") {
		t.Fatalf("why shows the failures behind it:\n%s", tr)
	}

	// Configurable: require a test that enforces it, and failures must be recent.
	e.Policy.Rules.RequireEnforcement = true
	e.RecordFailure(FailureInput{Label: "flaky-e2e", Summary: "a"})
	e.RecordFailure(FailureInput{Label: "flaky-e2e", Summary: "b"})
	o = propose(t, e, RuleInput{Text: "Retry e2e tests once", Label: "flaky-e2e", Why: Justification{Kind: "incident"}})
	if o.Admitted || !strings.Contains(o.Reason, "enforced_by") {
		t.Fatalf("require_enforcement: %+v", o)
	}
	o = propose(t, e, RuleInput{Text: "Quarantine flaky e2e tests behind a retry tag", Label: "flaky-e2e",
		Why: Justification{Kind: "incident", EnforcedBy: "test:./e2e/retry_test.go"}})
	if !o.Admitted {
		t.Fatalf("with enforcement: %+v", o)
	}
	c.advance(60 * 24 * time.Hour)
	o = propose(t, e, RuleInput{Text: "Never deploy on fridays", Label: "flaky-e2e",
		Why: Justification{Kind: "incident", EnforcedBy: "ci"}})
	if o.Admitted {
		t.Fatal("failures older than rules.failure_window don't count")
	}
}

func TestDecisionRulesFollowTheirDecision(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	m := remember(t, e, "fact", "the queue is sqs")
	dec, _, err := e.Decide(DecisionInput{Question: "how do services talk?", Choice: "through sqs only", Used: []string{m}})
	ok(t, err)
	o := propose(t, e, RuleInput{Text: "Never call another service's HTTP API directly; publish to SQS",
		Why: Justification{Kind: "decision", Decision: dec}})
	if !o.Admitted {
		t.Fatalf("%+v", o)
	}
	// The decision is reverted: the rule it justified is flagged, not left in force unexamined.
	flags := must[[]Flag](t)(e.Revert(dec, "moving to grpc"))
	if status := must[Rule](t)(e.Rule(o.ID)).Status; status != "suspect" {
		t.Fatalf("rule should be suspect, is %s (flags %+v)", status, flags)
	}
	rv := must[*Review](t)(e.Review())
	found := false
	for _, it := range rv.Suspect {
		found = found || it.ID == o.ID
	}
	if !found {
		t.Fatal("review lists the suspect rule")
	}
	ok(t, e.RetireRule(o.ID, "grpc now"))
	if _, err := e.ProposeRule(RuleInput{Text: "x", Why: Justification{Kind: "decision", Decision: dec}}); err != nil {
		t.Fatal(err)
	} else if rs := must[[]Rule](t)(e.Rules("active")); len(rs) != 0 {
		t.Fatal("a reverted decision justifies nothing new")
	}
}

func TestDuplicatesMergeAndCap(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	req := Justification{Kind: "requested", By: "rahul", Quote: "yes"}
	a := propose(t, e, RuleInput{Text: "Run the unit tests before committing", Why: req})
	dup := propose(t, e, RuleInput{Text: "Before committing, run the unit tests", Why: req})
	if dup.Admitted || !strings.Contains(dup.Reason, "duplicates "+a.ID) {
		t.Fatalf("a near-duplicate is held: %+v", dup)
	}
	b := propose(t, e, RuleInput{Text: "Run go vet before committing Go changes", Why: req})
	m := must[*RuleOutcome](t)(e.MergeRules([]string{a.ID, b.ID}, "Before committing, run go vet and the unit tests", Justification{}))
	if !m.Admitted || len(m.Retired) != 2 {
		t.Fatalf("a merge of active rules inherits their standing: %+v", m)
	}
	if r := must[Rule](t)(e.Rule(a.ID)); r.Status != "retired" || r.SupersededBy != m.ID {
		t.Fatalf("merged rules are retired: %+v", r)
	}

	e.Policy.Rules.MaxActive = 2
	propose(t, e, RuleInput{Text: "Keep PRs under 400 lines", Why: req})
	o := propose(t, e, RuleInput{Text: "Prefer table-driven tests", Why: req})
	if o.Admitted || !strings.Contains(o.Reason, "max_active") {
		t.Fatalf("over the cap: %+v", o)
	}
}

func TestRationalizeImportsAndReports(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	root := filepath.Dir(filepath.Dir(e.Path()))
	claude := filepath.Join(root, "CLAUDE.md")
	os.WriteFile(claude, []byte(`# Project

Some context about the project that is not a rule.

## Conventions
- Always run make test before committing
- Use the dojo- prefix for bead ids
  when filing issues
- Always run make test before you commit anything

`+"```"+`
- not a rule, inside a code block
`+"```"+`
You must never edit files under gen/ by hand.
`+rulesStart+`
- an old rendered rule is not re-imported
`+rulesEnd+`
`), 0o644)
	ids := must[[]string](t)(e.ImportRules([]string{claude}))
	if len(ids) != 4 {
		t.Fatalf("imported %d", len(ids))
	}
	r := must[Rule](t)(e.Rule(ids[1]))
	if r.Text != "Use the dojo- prefix for bead ids when filing issues" || r.Source != claude+":7" {
		t.Fatalf("continuation lines and source: %+v", r)
	}
	if again := must[[]string](t)(e.ImportRules([]string{claude})); len(again) != 0 {
		t.Fatal("importing twice adds nothing")
	}

	req := Justification{Kind: "requested", By: "rahul", Quote: "do it"}
	inc := propose(t, e, RuleInput{Text: "Pin the base image digest in Dockerfiles", Label: "image-drift", Why: req})
	stale := propose(t, e, RuleInput{Text: "Tag releases from main only", Why: req})
	ok(t, e.RuleViolated(inc.ID, "pushed :latest again"))
	c.advance(100 * 24 * time.Hour)
	must[string](t)(func() (string, error) {
		id, _, err := e.RecordFailure(FailureInput{Label: "image-drift", Summary: "prod pulled a new base image"})
		return id, err
	}())
	e.RecordFailure(FailureInput{Label: "gen-edits", Summary: "someone edited gen/api.go"})
	e.RecordFailure(FailureInput{Label: "gen-edits", Summary: "and again"})
	gen := propose(t, e, RuleInput{Text: "Don't hand-edit generated code", Label: "gen-edits"})

	rep := must[*RulesReport](t)(e.RationalizeRules(context.Background(), false))
	has := func(rs []Rule, id string) bool {
		for _, r := range rs {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if len(rep.Duplicates) != 1 || rep.Duplicates[0].A.ID != ids[0] || rep.Duplicates[0].B.ID != ids[2] {
		t.Errorf("duplicates: %+v", rep.Duplicates)
	}
	if !has(rep.Held, ids[3]) || !has(rep.Promotable, gen.ID) || has(rep.Promotable, ids[0]) {
		t.Errorf("held %d, promotable %v", len(rep.Held), rep.Promotable)
	}
	if len(rep.Ineffective) != 1 || rep.Ineffective[0].Rule.ID != inc.ID {
		t.Errorf("ineffective: %+v", rep.Ineffective)
	}
	if len(rep.Violated) != 1 || !has(rep.Stale, stale.ID) {
		t.Errorf("violated %+v stale %+v", rep.Violated, rep.Stale)
	}
	if must[Rule](t)(e.Rule(gen.ID)).Status != "proposed" {
		t.Fatal("rationalize reports; it doesn't change anything")
	}
}

type sameRuleJudge struct{}

func (sameRuleJudge) Name() string { return "jev" }
func (sameRuleJudge) Ask(_ context.Context, r judge.Request) (*judge.Result, error) {
	st := r.State.(map[string]any)
	same := strings.Contains(st["rule_a"].(string), "secret") && strings.Contains(st["rule_b"].(string), "credential")
	p := 0.05
	if same {
		p = 0.95
	}
	return &judge.Result{Judge: "jev", Answers: map[string]judge.Answer{"same_rule": {Kind: judge.Probability, Probability: p, Confidence: max(p, 1-p)}}}, nil
}

func TestRationalizeAsksJudgesAboutParaphrases(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	req := Justification{Kind: "requested", By: "rahul", Quote: "yes"}
	a := propose(t, e, RuleInput{Text: "Never commit secret values to the repository", Why: req})
	b := propose(t, e, RuleInput{Text: "Keep credential files out of the repository", Why: req})
	if !a.Admitted || !b.Admitted {
		t.Fatal("different words: both admitted by the gate")
	}
	e.Judges = judge.Cascade{Stages: []judge.Stage{{Judge: sameRuleJudge{}}}}
	rep := must[*RulesReport](t)(e.RationalizeRules(context.Background(), true))
	if len(rep.Duplicates) != 1 || rep.Duplicates[0].JudgedBy != "jev" {
		t.Fatalf("a judge finds the paraphrase: %+v", rep.Duplicates)
	}
}

func TestRenderManagesOnlyItsBlock(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	root := filepath.Dir(filepath.Dir(e.Path()))
	claude := filepath.Join(root, "CLAUDE.md")
	os.WriteFile(claude, []byte("# Project\n\nHand-written notes stay.\n"), 0o644)
	r := propose(t, e, RuleInput{Text: "Run make test before every commit",
		Why: Justification{Kind: "requested", By: "rahul", Quote: "always run the tests"}})
	propose(t, e, RuleInput{Text: "Held rules are not rendered"})

	if changed := must[bool](t)(e.RenderRules("", false)); !changed {
		t.Fatal("first render writes the block")
	}
	b, _ := os.ReadFile(claude)
	out := string(b)
	if !strings.HasPrefix(out, "# Project\n\nHand-written notes stay.\n\n"+rulesStart) || !strings.Contains(out, r.ID+": requested by rahul") ||
		strings.Contains(out, "Held rules") {
		t.Fatalf("render:\n%s", out)
	}
	if must[bool](t)(e.RenderRules("", true)) {
		t.Fatal("up to date after render")
	}
	ok(t, e.RetireRule(r.ID, "obsolete"))
	if !must[bool](t)(e.RenderRules("", true)) {
		t.Fatal("check notices the change")
	}
	must[bool](t)(e.RenderRules("", false))
	b, _ = os.ReadFile(claude)
	if strings.Count(string(b), rulesStart) != 1 || strings.Contains(string(b), "make test") || !strings.Contains(string(b), "Hand-written") {
		t.Fatalf("re-render replaces only the block:\n%s", b)
	}
}

func TestRulesReplayAndContext(t *testing.T) {
	c := start()
	e := newEngine(t, "agent", c)
	propose(t, e, RuleInput{Text: "Use the dojo- bead prefix", Why: Justification{Kind: "requested", By: "rahul", Quote: "dojo- please"}})
	e.RecordFailure(FailureInput{Label: "x", Summary: "y"})
	before := dumpViews(t, e)
	ok(t, e.Rebuild())
	if dumpViews(t, e) != before {
		t.Fatal("rules are projections like everything else")
	}
	if strings.Contains(must[string](t)(e.Context(0)), "dojo- bead prefix") {
		t.Fatal("rules stay out of context unless rules.in_context")
	}
	e.Policy.Rules.InContext = true
	if !strings.Contains(must[string](t)(e.Context(0)), "dojo- bead prefix") {
		t.Fatal("rules.in_context")
	}
	if tr := must[*Trace](t)(e.Why(must[[]Failure](t)(e.Failures("x", 1))[0].ID)); tr.Failure == nil {
		t.Fatal("why <fail-id>")
	}
}
