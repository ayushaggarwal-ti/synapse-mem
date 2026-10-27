package smem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"synapse-mem/judge"
)

// Verdict labels.
const (
	Supported    = "supported"
	Contradicted = "contradicted"
	Insufficient = "insufficient"
)

// Evidence is one piece of text a claim is checked against.
type Evidence struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // file, commit, memory, checker, ...
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text"`
}

// Claim is what checkers and judges see: a memory and the evidence gathered
// for it so far.
type Claim struct {
	Memory   Memory     `json:"memory"`
	Evidence []Evidence `json:"evidence"`
}

// Verdict is a fact-check conclusion.
type Verdict struct {
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason,omitempty"`
}

// CheckResult is what a checker returns. A checker may decide (Verdict),
// contribute evidence for the judges, both, or neither (abstain).
type CheckResult struct {
	Verdict  *Verdict   `json:"verdict,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Checker is a user-written check. Checkers run before any model, in order;
// the first one to return a verdict decides. Use them for anything that can
// be checked exactly: does the file exist, does the config say port 8080,
// is the test green.
type Checker interface {
	Name() string
	Check(ctx context.Context, c Claim) (CheckResult, error)
}

type funcChecker struct {
	name string
	fn   func(context.Context, Claim) (CheckResult, error)
}

func (f funcChecker) Name() string { return f.name }
func (f funcChecker) Check(ctx context.Context, c Claim) (CheckResult, error) {
	return f.fn(ctx, c)
}

// CheckFunc adapts a function into a Checker.
func CheckFunc(name string, fn func(context.Context, Claim) (CheckResult, error)) Checker {
	return funcChecker{name, fn}
}

// CommandChecker runs an external program, so checks can be written in any
// language. The program receives the Claim as JSON on stdin and prints a
// CheckResult as JSON on stdout; empty output means abstain. Commands come
// only from local configuration, never from synced data.
type CommandChecker struct {
	CheckName string
	Command   []string
	// Match limits the checker to memories whose content matches. Kinds
	// limits it to memory kinds. Empty means all.
	Match   *regexp.Regexp
	Kinds   []string
	Dir     string
	Timeout time.Duration
}

// Name implements Checker.
func (c *CommandChecker) Name() string { return c.CheckName }

// Check implements Checker.
func (c *CommandChecker) Check(ctx context.Context, claim Claim) (CheckResult, error) {
	if c.Match != nil && !c.Match.MatchString(claim.Memory.Content) {
		return CheckResult{}, nil
	}
	if len(c.Kinds) > 0 && !contains(c.Kinds, claim.Memory.Kind) {
		return CheckResult{}, nil
	}
	if len(c.Command) == 0 {
		return CheckResult{}, errors.New("no command")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	in, err := json.Marshal(claim)
	if err != nil {
		return CheckResult{}, err
	}
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Dir = c.Dir
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return CheckResult{}, fmt.Errorf("%v: %s", err, oneLine(stderr.String(), 200))
	}
	var res CheckResult
	if out := bytes.TrimSpace(stdout.Bytes()); len(out) > 0 {
		if err := json.Unmarshal(out, &res); err != nil {
			return CheckResult{}, fmt.Errorf("output is not a CheckResult: %w", err)
		}
	}
	return res, nil
}

// FactCheckPolicy sets when judgments are trusted and acted on.
type FactCheckPolicy struct {
	// AutoAccept is the confidence at which a judge's answer settles the
	// question; below it the next judge is asked.
	AutoAccept float64 `json:"auto_accept"`
	// AutoReconfirm is the confidence needed to reconfirm a memory without
	// review. Contradictions never invalidate automatically; they mark the
	// memory suspect.
	AutoReconfirm float64 `json:"auto_reconfirm"`
	// SubjectAt is how sure a judge must be that the evidence is about the
	// same thing before a contradiction counts.
	SubjectAt float64 `json:"subject_at"`
	// RelatedMemories is how many similar memories to include as evidence.
	RelatedMemories int `json:"related_memories"`
	// MaxEvidenceChars truncates each evidence item.
	MaxEvidenceChars int `json:"max_evidence_chars"`
}

// DefaultFactCheckPolicy is used for zero fields.
var DefaultFactCheckPolicy = FactCheckPolicy{AutoAccept: 0.8, AutoReconfirm: 0.9, SubjectAt: 0.5,
	RelatedMemories: 3, MaxEvidenceChars: 4000}

func (p FactCheckPolicy) withDefaults() FactCheckPolicy {
	d := DefaultFactCheckPolicy
	if p.AutoAccept > 0 {
		d.AutoAccept = p.AutoAccept
	}
	if p.AutoReconfirm > 0 {
		d.AutoReconfirm = p.AutoReconfirm
	}
	if p.SubjectAt > 0 {
		d.SubjectAt = p.SubjectAt
	}
	if p.RelatedMemories != 0 {
		d.RelatedMemories = max(p.RelatedMemories, 0)
	}
	if p.MaxEvidenceChars > 0 {
		d.MaxEvidenceChars = p.MaxEvidenceChars
	}
	return d
}

// FactCheckResult is the outcome of checking one memory.
type FactCheckResult struct {
	Memory    string     `json:"memory"`
	Content   string     `json:"content"`
	Verdict   Verdict    `json:"verdict"`
	DecidedBy string     `json:"decided_by"` // checker:<name>, a judge name, or "none"
	Settled   bool       `json:"settled"`
	Action    string     `json:"action"` // reconfirmed | suspected | review | none
	Evidence  []Evidence `json:"evidence"`
	Judgment  string     `json:"judgment"` // id of the recorded judgment
	Trail     []judgeRun `json:"trail"`
	Errors    []string   `json:"errors,omitempty"`
}

// judgeRun is one judge's contribution, as recorded in the log.
type judgeRun struct {
	Judge      string                  `json:"judge"`
	Model      string                  `json:"model"`
	Calibrated bool                    `json:"calibrated"`
	Answers    map[string]judge.Answer `json:"answers"`
	Usage      judge.Usage             `json:"usage"`
	LatencyMs  int64                   `json:"latency_ms"`
}

type evidenceRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
	Hash string `json:"hash"`
}

type checkerRun struct {
	Name    string   `json:"name"`
	Verdict *Verdict `json:"verdict,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// judgmentRecorded is the event payload. Evidence is recorded by reference
// and hash, not content: the log stays small and doesn't copy file contents.
type judgmentRecorded struct {
	ID        string        `json:"id"`
	Subject   string        `json:"subject"`
	Check     string        `json:"check"`
	Verdict   Verdict       `json:"verdict"`
	DecidedBy string        `json:"decided_by"`
	Settled   bool          `json:"settled"`
	Action    string        `json:"action"`
	Evidence  []evidenceRef `json:"evidence"`
	Checkers  []checkerRun  `json:"checkers,omitempty"`
	Judges    []judgeRun    `json:"judges,omitempty"`
	Errors    []string      `json:"errors,omitempty"`
}

var factQuestions = []judge.Question{
	{ID: "relation", Kind: judge.Choice,
		Instructions: "How does the evidence relate to the claim? Judge only from the evidence.",
		Labels: []judge.Label{
			{Key: "supports", Description: "The evidence states the claim or directly implies it is true as of the evidence's date"},
			{Key: "contradicts", Description: "The evidence states the opposite of the claim or implies it is false or no longer true"},
			{Key: "says_nothing", Description: "The evidence does not address what the claim asserts, either way"},
		}},
	{ID: "same_subject", Kind: judge.Probability,
		Instructions: "Is the evidence about the very same thing the claim is about (the same system, file, setting, person or object)?",
		True:         "At least one evidence item reports on exactly what the claim asserts",
		False:        "The evidence is about something else, or only something similar"},
}

// FactCheck checks one memory: user checkers first, then the judge cascade.
// With apply, a confident "supported" reconfirms the memory and a confident
// "contradicted" marks it suspect; nothing is ever invalidated
// automatically. The judgment is recorded as a signed event either way.
func (e *Engine) FactCheck(ctx context.Context, id string, apply bool) (*FactCheckResult, error) {
	m, err := e.Memory(id)
	if err != nil {
		return nil, err
	}
	if m.Status != "active" && m.Status != "suspect" {
		return nil, fmt.Errorf("%s is %s; only active or suspect memories are checked", id, m.Status)
	}
	pol := e.FactCheckPolicy.withDefaults()
	res := &FactCheckResult{Memory: id, Content: m.Content, DecidedBy: "none", Action: "none",
		Verdict: Verdict{Label: Insufficient}}
	rec := judgmentRecorded{ID: newID("jdg"), Subject: id, Check: "factcheck"}

	claim := Claim{Memory: m}
	claim.Evidence, err = e.gatherEvidence(m, pol)
	if err != nil {
		return nil, err
	}
	for _, c := range e.Checkers {
		run := checkerRun{Name: c.Name()}
		out, err := c.Check(ctx, claim)
		if err != nil {
			run.Error = err.Error()
			res.Errors = append(res.Errors, "checker "+c.Name()+": "+err.Error())
		}
		for _, ev := range out.Evidence {
			if ev.ID == "" {
				ev.ID = fmt.Sprintf("%s-%d", c.Name(), len(claim.Evidence))
			}
			ev.Kind = cmp(ev.Kind, "checker")
			claim.Evidence = append(claim.Evidence, ev)
		}
		if v := out.Verdict; v != nil && err == nil {
			if v.Confidence == 0 {
				v.Confidence = 1
			}
			if !contains([]string{Supported, Contradicted, Insufficient}, v.Label) {
				run.Error = "unknown verdict " + v.Label
			} else {
				run.Verdict = v
				res.Verdict, res.DecidedBy, res.Settled = *v, "checker:"+c.Name(), v.Label != Insufficient
			}
		}
		rec.Checkers = append(rec.Checkers, run)
		if res.Settled {
			break
		}
	}
	for i := range claim.Evidence {
		claim.Evidence[i].Text = truncate(Redact(claim.Evidence[i].Text), pol.MaxEvidenceChars)
	}
	res.Evidence = claim.Evidence

	if !res.Settled && len(claim.Evidence) > 0 && len(e.Judges.Stages) > 0 {
		req := judge.Request{
			Purpose: "Fact-check a stored memory against the evidence.",
			State: map[string]any{
				"claim":    map[string]any{"text": m.Content, "kind": m.Kind, "recorded": m.RecordedAt.Format("2006-01-02"), "valid_from": m.ValidFrom.Format("2006-01-02")},
				"evidence": claim.Evidence,
			},
			Questions: factQuestions,
		}
		out := e.Judges.Ask(ctx, req, func(r *judge.Result) bool {
			v := factVerdict(r, pol)
			return v.Label != Insufficient && v.Confidence >= pol.AutoAccept
		})
		res.Errors = append(res.Errors, out.Errors...)
		for _, r := range out.Trail {
			run := judgeRun{Judge: r.Judge, Model: r.Model, Calibrated: r.Calibrated, Answers: r.Answers,
				Usage: r.Usage, LatencyMs: r.Latency.Milliseconds()}
			res.Trail = append(res.Trail, run)
		}
		if out.Final != nil {
			res.Verdict, res.DecidedBy, res.Settled = factVerdict(out.Final, pol), out.Final.Judge, out.Settled
		}
		// Judges that disagree on the relation leave it for a person.
		if len(out.Trail) > 1 {
			first := out.Trail[0].Answers["relation"].Choice
			for _, r := range out.Trail[1:] {
				if r.Answers["relation"].Choice != first {
					res.Settled = false
					res.Verdict.Reason = "judges disagree"
				}
			}
		}
	}

	switch {
	case !res.Settled && res.Verdict.Label != Insufficient:
		res.Action = "review"
	case res.Settled && res.Verdict.Label == Supported && res.Verdict.Confidence >= pol.AutoReconfirm:
		res.Action = "reconfirmed"
	case res.Settled && res.Verdict.Label == Contradicted:
		res.Action = "suspected"
	case res.Settled && res.Verdict.Label == Supported:
		res.Action = "review"
	}
	if !apply && (res.Action == "reconfirmed" || res.Action == "suspected") {
		res.Action = map[string]string{"reconfirmed": "would-reconfirm", "suspected": "would-suspect"}[res.Action]
	}

	rec.Verdict, rec.DecidedBy, rec.Settled, rec.Action, rec.Judges, rec.Errors =
		res.Verdict, res.DecidedBy, res.Settled, res.Action, res.Trail, res.Errors
	for _, ev := range claim.Evidence {
		sum := sha256.Sum256([]byte(ev.Text))
		rec.Evidence = append(rec.Evidence, evidenceRef{ID: ev.ID, Kind: ev.Kind, Ref: ev.Ref, Hash: hex.EncodeToString(sum[:8])})
	}
	res.Judgment = rec.ID
	_, err = e.write(func(w *wtx) error {
		if _, err := w.emit(evJudgmentRecorded, id, rec); err != nil {
			return err
		}
		reason := fmt.Sprintf("fact-check %s: %s (%.2f) by %s", rec.ID, res.Verdict.Label, res.Verdict.Confidence, res.DecidedBy)
		switch res.Action {
		case "reconfirmed":
			_, err := w.emit(evMemoryReconfirmed, id, statusChange{ID: id, Reason: reason})
			return err
		case "suspected":
			if m.Status == "active" {
				_, err := w.emit(evMemorySuspected, id, statusChange{ID: id, Cause: rec.ID, Root: rec.ID, Reason: reason})
				return err
			}
		}
		return nil
	})
	return res, err
}

// factVerdict turns a judge's answers into a verdict. A contradiction only
// counts when the judge is confident the evidence is about the same thing.
func factVerdict(r *judge.Result, pol FactCheckPolicy) Verdict {
	rel, subj := r.Answers["relation"], r.Answers["same_subject"]
	v := Verdict{Confidence: rel.Confidence, Reason: rel.Rationale}
	switch rel.Choice {
	case "supports":
		v.Label = Supported
	case "contradicts":
		v.Label = Contradicted
		if subj.Probability < pol.SubjectAt {
			v.Label, v.Reason = Insufficient, "contradiction is about a different subject"
		}
	default:
		v.Label = Insufficient
	}
	v.Confidence = math.Round(v.Confidence*1000) / 1000
	return v
}

// gatherEvidence collects the memory's linked sources and similar memories.
func (e *Engine) gatherEvidence(m Memory, pol FactCheckPolicy) ([]Evidence, error) {
	var out []Evidence
	srcs, err := selectAll(e.db, scanSource, `SELECT `+prefixed("s", sourceCols)+` FROM edges e JOIN sources s ON s.id = e.dst
		WHERE e.src = ? AND e.type = 'derived_from' ORDER BY e.at`, m.ID)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(filepath.Dir(e.path)) // the directory holding .smem
	for _, s := range srcs {
		switch s.Kind {
		case "file":
			p := s.Ref
			if !filepath.IsAbs(p) {
				p = filepath.Join(root, p)
			}
			if b, err := os.ReadFile(p); err == nil {
				out = append(out, Evidence{ID: s.ID, Kind: "file", Ref: s.Ref, Text: string(b)})
			} else {
				out = append(out, Evidence{ID: s.ID, Kind: "file", Ref: s.Ref, Text: "(file not found: " + s.Ref + ")"})
			}
		case "commit":
			cmd := exec.Command("git", "show", "--stat", "--format=%H %ad%n%B", s.Ref)
			cmd.Dir = root
			if b, err := cmd.Output(); err == nil {
				out = append(out, Evidence{ID: s.ID, Kind: "commit", Ref: s.Ref, Text: string(b)})
			}
		default:
			if s.Note != "" {
				out = append(out, Evidence{ID: s.ID, Kind: s.Kind, Ref: s.Ref, Text: s.Note})
			}
		}
	}
	if pol.RelatedMemories > 0 {
		related, err := e.score(RecallQuery{Text: m.Content})
		if err != nil {
			return nil, err
		}
		lineage := map[string]bool{m.ID: true, m.Supersedes: true, m.SupersededBy: true}
		n := 0
		for _, r := range related {
			if n >= pol.RelatedMemories {
				break
			}
			if lineage[r.ID] || r.Status != "active" {
				continue
			}
			out = append(out, Evidence{ID: r.ID, Kind: "memory",
				Text: fmt.Sprintf("Recorded %s: %s", r.RecordedAt.Format("2006-01-02"), r.Content)})
			n++
		}
	}
	return out, nil
}

// FactCheckDue picks memories worth checking: facts and insights whose
// confidence has decayed below threshold, most salient first, skipping any
// checked in the last day.
func (e *Engine) FactCheckDue(threshold float64, limit int) ([]Scored, error) {
	hits, err := e.score(RecallQuery{Kinds: []string{"fact", "insight"}})
	if err != nil {
		return nil, err
	}
	recent, err := e.strings(`SELECT DISTINCT subject FROM judgments WHERE at > ?`, ms(e.Now().Add(-24*time.Hour)))
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, s := range recent {
		skip[s] = true
	}
	var due []Scored
	for _, h := range hits {
		if !skip[h.ID] && (h.Confidence < threshold || h.Volatility == "volatile") {
			due = append(due, h)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		return due[i].Salience*(1-due[i].Confidence) > due[j].Salience*(1-due[j].Confidence)
	})
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

// Judgment is a recorded fact-check.
type Judgment struct {
	ID         string    `json:"id"`
	Subject    string    `json:"subject"`
	At         time.Time `json:"at"`
	Verdict    string    `json:"verdict"`
	Confidence float64   `json:"confidence"`
	DecidedBy  string    `json:"decided_by"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason,omitempty"`
}

func (e *Engine) judgments(subject string) ([]Judgment, error) {
	return selectAll(e.db, scanJudgment, `SELECT `+judgmentCols+` FROM judgments WHERE subject = ? ORDER BY at`, subject)
}

const judgmentCols = `id, subject, at, verdict, confidence, decided_by, action, reason`

func scanJudgment(s scanner) (Judgment, error) {
	var j Judgment
	var at int64
	err := s.Scan(&j.ID, &j.Subject, &at, &j.Verdict, &j.Confidence, &j.DecidedBy, &j.Action, &j.Reason)
	j.At = fromMs(at)
	return j, err
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "\n…(truncated)"
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func cmp(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
