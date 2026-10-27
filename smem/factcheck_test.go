package smem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"synapse-mem/judge"
)

// scripted is a judge that returns a fixed relation.
type scripted struct {
	name    string
	choice  string
	conf    float64
	subject float64
	err     error
	seen    *judge.Request
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Ask(_ context.Context, r judge.Request) (*judge.Result, error) {
	s.seen = &r
	if s.err != nil {
		return nil, s.err
	}
	return &judge.Result{Judge: s.name, Model: s.name + "-model", Calibrated: s.name == "jev", Answers: map[string]judge.Answer{
		"relation":     {Kind: judge.Choice, Choice: s.choice, Confidence: s.conf, Rationale: "because"},
		"same_subject": {Kind: judge.Probability, Probability: s.subject, Confidence: max(s.subject, 1-s.subject)},
	}}, nil
}

func stages(js ...judge.Judge) judge.Cascade {
	var c judge.Cascade
	for _, j := range js {
		c.Stages = append(c.Stages, judge.Stage{Judge: j})
	}
	return c
}

func TestFactCheckJudgesAndActions(t *testing.T) {
	c := start()
	e := newEngine(t, "alice", c)
	old := remember(t, e, "fact", "the api listens on port 8080")
	remember(t, e, "fact", "after the migration the api listens on port 9090")
	dec, _, _ := e.Decide(DecisionInput{Question: "which port in the firewall rule?", Choice: "8080", Used: []string{old}})

	jev := &scripted{name: "jev", choice: "contradicts", conf: 0.93, subject: 0.9}
	e.Judges = stages(jev)
	res, err := e.FactCheck(context.Background(), old, true)
	ok(t, err)
	if res.Verdict.Label != Contradicted || res.Action != "suspected" || res.DecidedBy != "jev" {
		t.Fatalf("result: %+v", res)
	}
	if status(t, e, old) != "suspect" {
		t.Fatal("a confident contradiction marks the memory suspect")
	}
	if status(t, e, dec) != "active" {
		t.Fatal("suspicion is not invalidation: dependents are only at risk")
	}
	if rv := must[*Review](t)(e.Review()); len(rv.AtRisk) != 1 || rv.AtRisk[0].ID != dec {
		t.Fatalf("the decision should show as at risk: %+v", rv.AtRisk)
	}
	// The related memory went to the judge as evidence.
	st := jev.seen.State.(map[string]any)["evidence"].([]Evidence)
	if len(st) != 1 || !strings.Contains(st[0].Text, "9090") {
		t.Fatalf("evidence: %+v", st)
	}
	tr := must[*Trace](t)(e.Why(old))
	if !strings.Contains(tr.Text(), "fact-check "+res.Judgment) || !strings.Contains(tr.Text(), "suspected because "+res.Judgment) {
		t.Fatalf("why should show the judgment as the cause:\n%s", tr.Text())
	}
}

func TestFactCheckEscalatesAndDisagreementGoesToReview(t *testing.T) {
	c := start()
	e := newEngine(t, "alice", c)
	m := remember(t, e, "fact", "builds run on the m5 runner")
	remember(t, e, "fact", "builds moved to the m6 runner pool")

	jev := &scripted{name: "jev", choice: "contradicts", conf: 0.6, subject: 0.9}
	claude := &scripted{name: "claude", choice: "supports", conf: 0.85, subject: 0.9}
	e.Judges = stages(jev, claude)
	res, err := e.FactCheck(context.Background(), m, true)
	ok(t, err)
	if len(res.Trail) != 2 || res.Settled || res.Action != "review" || res.Verdict.Reason != "judges disagree" {
		t.Fatalf("unsure jev escalates; disagreement goes to review: %+v", res)
	}
	if status(t, e, m) != "active" {
		t.Fatal("review must not change the memory")
	}
	if rv := must[*Review](t)(e.Review()); len(rv.FactCheck) != 1 || rv.FactCheck[0].ID != m {
		t.Fatalf("review queue: %+v", rv.FactCheck)
	}

	// An outage at the first stage degrades to the second.
	jev.err = errors.New("jev: HTTP 503")
	claude.choice, claude.conf = "supports", 0.95
	c.advance(25 * time.Hour)
	res, err = e.FactCheck(context.Background(), m, true)
	ok(t, err)
	if res.DecidedBy != "claude" || res.Action != "reconfirmed" || len(res.Errors) != 1 {
		t.Fatalf("outage: %+v", res)
	}

	// A contradiction about a different subject doesn't count.
	jev.err = nil
	jev.choice, jev.conf, jev.subject = "contradicts", 0.95, 0.2
	claude.choice, claude.conf = "says_nothing", 0.9
	res, _ = e.FactCheck(context.Background(), m, true)
	if res.Verdict.Label != Insufficient || res.Action != "none" {
		t.Fatalf("off-subject contradiction: %+v", res)
	}
}

func TestCheckersRunFirstAndCanBeCommands(t *testing.T) {
	c := start()
	e := newEngine(t, "alice", c)
	root := filepath.Dir(filepath.Dir(e.Path()))
	os.WriteFile(filepath.Join(root, "app.env"), []byte("PORT=9090\n"), 0o644)
	src := must[string](t)(e.CaptureSource("file", "app.env", "", ""))
	m := must[string](t)(e.Remember(MemoryInput{Content: "the api listens on port 8080", DerivedFrom: []string{src}}))

	jev := &scripted{name: "jev", choice: "supports", conf: 0.99, subject: 0.99}
	e.Judges = stages(jev)
	portRe := regexp.MustCompile(`port (\d+)`)
	e.Checkers = []Checker{CheckFunc("port", func(_ context.Context, cl Claim) (CheckResult, error) {
		want := portRe.FindStringSubmatch(cl.Memory.Content)
		for _, ev := range cl.Evidence {
			if ev.Kind == "file" && want != nil {
				if strings.Contains(ev.Text, "PORT="+want[1]) {
					return CheckResult{Verdict: &Verdict{Label: Supported, Reason: "config matches"}}, nil
				}
				return CheckResult{Verdict: &Verdict{Label: Contradicted, Reason: "config says " + strings.TrimSpace(ev.Text)}}, nil
			}
		}
		return CheckResult{}, nil
	})}
	res, err := e.FactCheck(context.Background(), m, false)
	ok(t, err)
	if res.DecidedBy != "checker:port" || res.Verdict.Label != Contradicted || res.Action != "would-suspect" || jev.seen != nil {
		t.Fatalf("a deciding checker settles it without calling judges: %+v", res)
	}
	if status(t, e, m) != "active" {
		t.Fatal("dry run must not change the memory")
	}

	// A command checker in any language: claim JSON in, result JSON out.
	script := filepath.Join(root, "check.sh")
	os.WriteFile(script, []byte(`#!/bin/sh
grep -q '"content":"the api listens on port 8080"' && echo '{"evidence":[{"id":"probe","text":"curl :8080 -> connection refused"}]}'
`), 0o755)
	e.Checkers = []Checker{&CommandChecker{CheckName: "probe", Command: []string{script}, Match: regexp.MustCompile(`port`)}}
	res, err = e.FactCheck(context.Background(), m, false)
	ok(t, err)
	var found bool
	for _, ev := range jev.seen.State.(map[string]any)["evidence"].([]Evidence) {
		found = found || (ev.ID == "probe" && ev.Kind == "checker")
	}
	if !found || res.DecidedBy != "jev" {
		t.Fatalf("command checker evidence should reach the judge: %+v", res)
	}

	// Configure from a config file + environment.
	cfgPath := ConfigPath(e.Path())
	os.WriteFile(cfgPath, []byte(`{"factcheck":{"auto_reconfirm":0.95},"judges":{"claude":{"when":"never"}},
		"checkers":[{"name":"probe","command":["`+script+`"],"match":"port","timeout":"5s"}]}`), 0o644)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	cfg := must[*Config](t)(LoadConfig(cfgPath))
	ok(t, e.Configure(cfg))
	if len(e.Checkers) != 1 || len(e.Judges.Stages) != 0 || e.FactCheckPolicy.withDefaults().AutoReconfirm != 0.95 {
		t.Fatalf("configure: %d checkers, judges %v", len(e.Checkers), e.Judges.Names())
	}
	t.Setenv("TYPESAFE_API_KEY", "k")
	ok(t, e.Configure(&Config{}))
	if names := e.Judges.Names(); len(names) != 2 || names[0] != "jev" || names[1] != "claude" {
		t.Fatalf("keys enable jev then claude: %v", names)
	}
}

func TestFactCheckWithoutEvidenceOrJudges(t *testing.T) {
	c := start()
	e := newEngine(t, "alice", c)
	m := remember(t, e, "fact", "a lonely fact with nothing to check it against")
	res, err := e.FactCheck(context.Background(), m, true)
	ok(t, err)
	if res.Verdict.Label != Insufficient || res.Action != "none" || res.DecidedBy != "none" {
		t.Fatalf("%+v", res)
	}
	// Judgments are projections like everything else.
	before := dumpViews(t, e)
	ok(t, e.Rebuild())
	if dumpViews(t, e) != before {
		t.Fatal("rebuild changed judgments")
	}
	if must[*VerifyReport](t)(e.Verify()).OK == false {
		t.Fatal("verify")
	}
}

func TestFactCheckDue(t *testing.T) {
	c := start()
	e := newEngine(t, "alice", c)
	fresh := remember(t, e, "fact", "fresh fact")
	stale := remember(t, e, "fact", "stale fact")
	must[string](t)(e.Remember(MemoryInput{Kind: "preference", Content: "never checked: preferences"}))
	c.advance(40 * 24 * time.Hour)
	ok(t, e.Reconfirm(fresh, 0, ""))
	due := must[[]Scored](t)(e.FactCheckDue(0.6, 10))
	if len(due) != 1 || due[0].ID != stale {
		t.Fatalf("due: %v", ids(due))
	}
	must[*FactCheckResult](t)(e.FactCheck(context.Background(), stale, true))
	if due := must[[]Scored](t)(e.FactCheckDue(0.6, 10)); len(due) != 0 {
		t.Fatal("recently checked memories are skipped")
	}
}
