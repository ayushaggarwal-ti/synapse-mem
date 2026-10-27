package smem

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Option is an alternative that was considered and rejected.
type Option struct {
	Option string `json:"option"`
	Reason string `json:"reason,omitempty"`
}

// AssumptionInput is something a decision takes to be true. CheckHint says
// how to check it (a command, file or query). smem stores the hint but never
// runs it, because a hint that came from a synced peer is untrusted input.
type AssumptionInput struct {
	Statement string
	CheckHint string
}

// Snapshot is what the agent had in view when it decided.
type Snapshot struct {
	Memories   []string `json:"memories,omitempty"` // memory versions in context
	Recall     string   `json:"recall,omitempty"`   // the recall event that produced them
	Model      string   `json:"model,omitempty"`
	PromptHash string   `json:"prompt_hash,omitempty"`
	Skills     []string `json:"skills,omitempty"`
}

// DecisionInput describes a decision and why it was made.
type DecisionInput struct {
	Question    string
	Choice      string
	Rationale   string
	Rejected    []Option
	Assumptions []AssumptionInput
	// Used lists the memories the decision relies on. If one of them is
	// invalidated later, the decision is marked suspect.
	Used []string
	// Evidence is record ids or free references (URLs, commits, bead ids,
	// file paths). Free references are captured as sources.
	Evidence   []string
	Bead       string
	Supersedes string
	// Context lists memories in view. If it is empty, the memories returned
	// by this session's most recent recall are used.
	Context    []string
	Model      string
	PromptHash string
	Skills     []string
}

// Decision is a recorded decision.
type Decision struct {
	ID           string          `json:"id"`
	Question     string          `json:"question"`
	Choice       string          `json:"choice"`
	Rationale    string          `json:"rationale,omitempty"`
	Status       string          `json:"status"`
	DecidedAt    time.Time       `json:"decided_at"`
	Actor        string          `json:"actor"`
	Session      string          `json:"session,omitempty"`
	Bead         string          `json:"bead,omitempty"`
	Snapshot     json.RawMessage `json:"snapshot"`
	Outcome      string          `json:"outcome,omitempty"`
	OutcomeNote  string          `json:"outcome_note,omitempty"`
	OutcomeAt    *time.Time      `json:"outcome_at,omitempty"`
	Supersedes   string          `json:"supersedes,omitempty"`
	SupersededBy string          `json:"superseded_by,omitempty"`
}

const decisionCols = `id, question, choice, rationale, status, decided_at, actor, session, bead, snapshot,
	outcome, outcome_note, outcome_at, supersedes, superseded_by`

func scanDecision(s scanner) (Decision, error) {
	var d Decision
	var at int64
	var snap string
	var oat sql.NullInt64
	err := s.Scan(&d.ID, &d.Question, &d.Choice, &d.Rationale, &d.Status, &at, &d.Actor, &d.Session, &d.Bead, &snap,
		&d.Outcome, &d.OutcomeNote, &oat, &d.Supersedes, &d.SupersededBy)
	d.DecidedAt, d.Snapshot, d.OutcomeAt = fromMs(at), json.RawMessage(snap), optTime(oat)
	return d, err
}

var hexRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// sourceKind classifies a free reference. Bead ids follow beads.prefixes.
func (e *Engine) sourceKind(ref string) string {
	switch {
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		return "url"
	case e.IsBead(ref):
		return "bead"
	case hexRef.MatchString(ref):
		return "commit"
	case strings.Contains(ref, "/") || strings.Contains(ref, "."):
		return "file"
	}
	return "ref"
}

// Decide records a decision with its rationale, rejected options,
// assumptions, the memories it relied on and a snapshot of what was in view.
func (e *Engine) Decide(in DecisionInput) (string, []Flag, error) {
	in.Question, in.Choice = strings.TrimSpace(in.Question), strings.TrimSpace(in.Choice)
	if in.Question == "" || in.Choice == "" {
		return "", nil, errors.New("a decision needs a question and a choice")
	}
	p := decisionMade{ID: newID("dec"), Question: in.Question, Choice: in.Choice, Rationale: strings.TrimSpace(in.Rationale),
		Rejected: in.Rejected, Used: dedupe(in.Used), Bead: in.Bead, Supersedes: in.Supersedes,
		Snapshot: Snapshot{Memories: dedupe(in.Context), Model: in.Model, PromptHash: in.PromptHash, Skills: in.Skills}}
	for _, a := range in.Assumptions {
		if s := strings.TrimSpace(a.Statement); s != "" {
			p.Assumptions = append(p.Assumptions, assumptionIn{ID: newID("asm"), Statement: s, CheckHint: a.CheckHint})
		}
	}
	evs, err := e.write(func(w *wtx) error {
		for _, id := range p.Used {
			if err := w.require(id, "mem"); err != nil {
				return err
			}
		}
		for _, ref := range dedupe(in.Evidence) {
			if isRecordID(ref) {
				if _, _, err := w.lookup(ref); err != nil {
					return err
				}
				p.Evidence = append(p.Evidence, ref)
				continue
			}
			src := sourceCaptured{ID: newID("src"), Kind: w.e.sourceKind(ref), Ref: ref}
			if _, err := w.emit(evSourceCaptured, src.ID, src); err != nil {
				return err
			}
			p.Evidence = append(p.Evidence, src.ID)
		}
		if p.Supersedes != "" {
			if err := w.require(p.Supersedes, "dec", "active", "suspect"); err != nil {
				return err
			}
		}
		if len(p.Snapshot.Memories) == 0 {
			id, mems, err := w.lastRecall()
			if err != nil {
				return err
			}
			p.Snapshot.Recall, p.Snapshot.Memories = id, mems
		}
		if _, err := w.emit(evDecisionMade, p.ID, p); err != nil {
			return err
		}
		if p.Supersedes != "" {
			return w.propagate(p.Supersedes, "decision superseded by "+p.ID)
		}
		return nil
	})
	return p.ID, flagsFrom(evs), err
}

// lastRecall returns the memories this session's latest recall returned.
func (w *wtx) lastRecall() (string, []string, error) {
	if w.e.Session == "" {
		return "", nil, nil
	}
	var id, payload string
	err := w.tx.QueryRow(`SELECT id, payload FROM events WHERE type = ? AND session = ? ORDER BY at DESC LIMIT 1`,
		evRecall, w.e.Session).Scan(&id, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	var p recallPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", nil, err
	}
	ids := make([]string, len(p.Results))
	for i, h := range p.Results {
		ids[i] = h.ID
	}
	return id, ids, nil
}

// Outcome records how a decision turned out: success, failure or mixed.
func (e *Engine) Outcome(id, outcome, note string) error {
	switch outcome {
	case "success", "failure", "mixed":
	default:
		return errors.New("outcome must be success, failure or mixed")
	}
	_, err := e.write(func(w *wtx) error {
		if err := w.require(id, "dec"); err != nil {
			return err
		}
		_, err := w.emit(evDecisionOutcome, id, outcomePayload{ID: id, Outcome: outcome, Note: note})
		return err
	})
	return err
}

// Approve records a human or agent approval. The approval is bound to the
// hash of the decision's latest event, so it is clear what was approved.
func (e *Engine) Approve(id, approver, scope, note string) error {
	if approver == "" {
		return errors.New("approver is required")
	}
	_, err := e.write(func(w *wtx) error {
		if err := w.require(id, "dec"); err != nil {
			return err
		}
		var seen string
		if err := w.tx.QueryRow(`SELECT hash FROM events WHERE subject = ? ORDER BY at DESC LIMIT 1`, id).Scan(&seen); err != nil {
			return err
		}
		_, err := w.emit(evDecisionApproved, id, approvalPayload{ID: id, Approver: approver, Scope: scope, Note: note, Seen: seen})
		return err
	})
	return err
}

// Reaffirm clears a suspect decision after review.
func (e *Engine) Reaffirm(id, note string) error {
	_, err := e.write(func(w *wtx) error {
		if err := w.require(id, "dec", "suspect"); err != nil {
			return err
		}
		_, err := w.emit(evDecisionReaffirmed, id, statusChange{ID: id, Reason: note})
		return err
	})
	return err
}

// Revert records that a decision was undone. Memories derived from it are
// marked suspect.
func (e *Engine) Revert(id, reason string) ([]Flag, error) {
	evs, err := e.write(func(w *wtx) error {
		if err := w.require(id, "dec", "active", "suspect"); err != nil {
			return err
		}
		if _, err := w.emit(evDecisionReverted, id, statusChange{ID: id, Reason: reason}); err != nil {
			return err
		}
		return w.propagate(id, reason)
	})
	return flagsFrom(evs), err
}

// BreakAssumption records that an assumption no longer holds. Its decision
// is marked suspect.
func (e *Engine) BreakAssumption(id, reason string) ([]Flag, error) {
	evs, err := e.write(func(w *wtx) error {
		if err := w.require(id, "asm", "open", "held"); err != nil {
			return err
		}
		if _, err := w.emit(evAssumptionBroken, id, statusChange{ID: id, Reason: reason}); err != nil {
			return err
		}
		return w.propagate(id, reason)
	})
	return flagsFrom(evs), err
}

// HoldAssumption records that an assumption was checked and still holds.
func (e *Engine) HoldAssumption(id, note string) error {
	_, err := e.write(func(w *wtx) error {
		if err := w.require(id, "asm"); err != nil {
			return err
		}
		_, err := w.emit(evAssumptionHeld, id, statusChange{ID: id, Reason: note})
		return err
	})
	return err
}

// Assumption is a recorded assumption.
type Assumption struct {
	ID         string     `json:"id"`
	DecisionID string     `json:"decision_id"`
	Statement  string     `json:"statement"`
	CheckHint  string     `json:"check_hint,omitempty"`
	Status     string     `json:"status"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	Note       string     `json:"note,omitempty"`
}

const assumptionCols = `id, decision_id, statement, check_hint, status, checked_at, note`

func scanAssumption(s scanner) (Assumption, error) {
	var a Assumption
	var at sql.NullInt64
	err := s.Scan(&a.ID, &a.DecisionID, &a.Statement, &a.CheckHint, &a.Status, &at, &a.Note)
	a.CheckedAt = optTime(at)
	return a, err
}

// Assumptions lists assumptions, optionally filtered by status.
func (e *Engine) Assumptions(status string) ([]Assumption, error) {
	q := `SELECT ` + assumptionCols + ` FROM assumptions`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	rows, err := e.db.Query(q+` ORDER BY decision_id, id`, args...)
	return queryAll(rows, err, scanAssumption)
}

// ActionInput describes work that was done.
type ActionInput struct {
	Tool       string
	Summary    string
	Decision   string
	Bead       string
	Commit     string
	ArgsHash   string
	ResultHash string
	Transcript string
}

// RecordAction records a unit of work, linked to the decision it carries out.
func (e *Engine) RecordAction(in ActionInput) (string, error) {
	p := actionPayload{ID: newID("act"), Decision: in.Decision, Bead: in.Bead, Tool: in.Tool, Summary: in.Summary,
		ArgsHash: in.ArgsHash, ResultHash: in.ResultHash, Commit: in.Commit, Transcript: in.Transcript}
	_, err := e.write(func(w *wtx) error {
		if p.Decision != "" {
			if err := w.require(p.Decision, "dec"); err != nil {
				return err
			}
		}
		_, err := w.emit(evActionRecorded, p.ID, p)
		return err
	})
	return p.ID, err
}

// CaptureSource records an external source (a transcript, doc, commit or URL).
func (e *Engine) CaptureSource(kind, ref, contentHash, note string) (string, error) {
	if ref == "" {
		return "", errors.New("source ref is required")
	}
	if kind == "" {
		kind = e.sourceKind(ref)
	}
	p := sourceCaptured{ID: newID("src"), Kind: kind, Ref: ref, ContentHash: contentHash, Note: note}
	_, err := e.write(func(w *wtx) error {
		_, err := w.emit(evSourceCaptured, p.ID, p)
		return err
	})
	return p.ID, err
}

// LatestDecision returns the session's most recent active decision, or "".
func (e *Engine) LatestDecision(session string) (string, error) {
	if session == "" {
		return "", nil
	}
	var id string
	err := e.db.QueryRow(`SELECT id FROM decisions WHERE session = ? AND status = 'active' ORDER BY decided_at DESC LIMIT 1`, session).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Decision loads one decision by id.
func (e *Engine) Decision(id string) (Decision, error) {
	d, err := scanDecision(e.db.QueryRow(`SELECT `+decisionCols+` FROM decisions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("no such decision %s", id)
	}
	return d, err
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
