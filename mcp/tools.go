package mcp

import (
	"cmp"
	"context"
	"errors"

	"github.com/rahulsub/synapse-mem/smem"
)

// plain is a tool result sent as-is rather than as JSON.
type plain string

func obj(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}
func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

var kinds = []string{"fact", "episode", "procedure", "preference", "insight"}

// hit is a recalled memory trimmed to what a model needs.
type hit struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Content    string  `json:"content"`
	Scope      string  `json:"scope,omitempty"`
	Status     string  `json:"status,omitempty"`
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
	Recorded   string  `json:"recorded"`
}

type rememberArgs struct {
	Content     string   `json:"content"`
	Kind        string   `json:"kind"`
	Scope       string   `json:"scope"`
	Volatility  string   `json:"volatility"`
	Confidence  float64  `json:"confidence"`
	DerivedFrom []string `json:"derived_from"`
}

func (a rememberArgs) input() smem.MemoryInput {
	return smem.MemoryInput{Kind: a.Kind, Content: a.Content, Scope: a.Scope, Volatility: a.Volatility,
		Confidence: a.Confidence, DerivedFrom: a.DerivedFrom}
}

var memoryProps = map[string]any{
	"content":      str("The memory, as one self-contained statement."),
	"kind":         enum("fact: true about the world or system. episode: something that happened. procedure: how to do something. preference: what a person wants. insight: a lesson drawn from experience.", kinds...),
	"scope":        str("Optional: who or what it applies to, e.g. user, project:billing, agent:reviewer."),
	"volatility":   enum("How fast it goes stale. Default normal (30-day half-life).", "volatile", "normal", "stable", "permanent"),
	"confidence":   num("0..1, default 0.8."),
	"derived_from": strs("Ids of memories, decisions or sources this came from, or file paths, URLs and commit shas."),
}

func withID(props map[string]any) map[string]any {
	out := map[string]any{"id": str("The id of the memory to replace (mem-...).")}
	for k, v := range props {
		out[k] = v
	}
	return out
}

var tools = []tool{
	{
		name: "memory_recall",
		desc: "Search memory. Results are ranked by relevance, how recently and often each memory was used, and how much it can still be trusted. Use at the start of a task and before deciding anything that depends on past knowledge.",
		schema: obj([]string{"query"}, map[string]any{
			"query": str("What to look for."),
			"kind":  strs("Only these kinds: " + "fact, episode, procedure, preference, insight."),
			"scope": str("Only this scope."),
			"limit": num("Max results, default 10."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Query string   `json:"query"`
			Kind  []string `json:"kind"`
			Scope string   `json:"scope"`
			Limit int      `json:"limit"`
		}) (any, error) {
			res, err := e.Recall(smem.RecallQuery{Text: a.Query, Kinds: a.Kind, Scope: a.Scope, Limit: a.Limit})
			if err != nil {
				return nil, err
			}
			out := make([]hit, len(res))
			for i, r := range res {
				status := r.Status
				if status == "active" {
					status = ""
				}
				out[i] = hit{ID: r.ID, Kind: r.Kind, Content: r.Content, Scope: r.Scope, Status: status,
					Score: round(r.Score), Confidence: round(r.Confidence), Recorded: r.RecordedAt.Format("2006-01-02")}
			}
			return out, nil
		}),
	},
	{
		name:   "memory_context",
		desc:   "The most salient memories, within a character budget. For agents with no session-start hook: call once at the start of a session.",
		schema: obj(nil, map[string]any{"budget": num("Max characters, default 4000.")}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Budget int `json:"budget"`
		}) (any, error) {
			s, err := e.Context(a.Budget)
			return plain(s), err
		}),
	},
	{
		name:   "memory_remember",
		desc:   "Store something worth knowing in a later session. One statement per call. Don't store what is in the code, git history or docs, or what only matters to this task.",
		schema: obj([]string{"content", "kind"}, memoryProps),
		call: handler(func(_ context.Context, e *smem.Engine, a rememberArgs) (any, error) {
			if err := required(map[string]string{"content": a.Content, "kind": a.Kind}); err != nil {
				return nil, err
			}
			id, err := e.Remember(a.input())
			return map[string]string{"id": id}, err
		}),
	},
	{
		name:   "memory_update",
		desc:   "Replace a memory with a new version because the world changed or it was imprecise. The old version stays in history. Decisions that used it are flagged for review.",
		schema: obj([]string{"id", "content"}, withID(memoryProps)),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			ID string `json:"id"`
			rememberArgs
		}) (any, error) {
			if err := required(map[string]string{"id": a.ID, "content": a.Content}); err != nil {
				return nil, err
			}
			in := a.input()
			if in.Kind == "" {
				old, err := e.Memory(a.ID)
				if err != nil {
					return nil, err
				}
				in.Kind = old.Kind
			}
			id, flags, err := e.Supersede(a.ID, in)
			return map[string]any{"id": id, "flagged": flags}, err
		}),
	},
	{
		name: "memory_invalidate",
		desc: "Mark a memory as no longer true (it was true before), or with never_true as a mistake. Decisions that used it are flagged for review.",
		schema: obj([]string{"id", "reason"}, map[string]any{
			"id":         str("Memory id (mem-...)."),
			"reason":     str("Why, with the evidence."),
			"never_true": boolean("It was wrong from the start, not just outdated."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			ID        string `json:"id"`
			Reason    string `json:"reason"`
			NeverTrue bool   `json:"never_true"`
		}) (any, error) {
			if err := required(map[string]string{"id": a.ID, "reason": a.Reason}); err != nil {
				return nil, err
			}
			var flags []smem.Flag
			var err error
			if a.NeverTrue {
				flags, err = e.Retract(a.ID, a.Reason)
			} else {
				flags, err = e.Invalidate(a.ID, a.Reason, zeroTime)
			}
			return map[string]any{"flagged": flags}, err
		}),
	},
	{
		name: "memory_signal",
		desc: "Report that a recalled memory was used, confirmed true, or contradicted. This is what keeps useful memories from fading and flags doubtful ones.",
		schema: obj([]string{"id", "signal"}, map[string]any{
			"id":     str("Memory id (mem-...)."),
			"signal": enum("used: it informed your work. confirmed: you checked and it holds. contradicted: you saw evidence against it.", "used", "confirmed", "contradicted"),
			"note":   str("Optional: what you saw."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			ID     string `json:"id"`
			Signal string `json:"signal"`
			Note   string `json:"note"`
		}) (any, error) {
			if err := required(map[string]string{"id": a.ID, "signal": a.Signal}); err != nil {
				return nil, err
			}
			return map[string]bool{"ok": true}, e.Signal(a.ID, a.Signal, a.Note)
		}),
	},
	{
		name: "decision_record",
		desc: "Record a non-obvious decision so it can be explained and revisited: what you chose, why, what you rejected, what you assumed, and which memories you relied on. If a relied-on memory is later found wrong, the decision is flagged.",
		schema: obj([]string{"question", "choice", "why"}, map[string]any{
			"question": str("What was being decided."),
			"choice":   str("What you chose."),
			"why":      str("The reasoning."),
			"rejected": map[string]any{"type": "array", "description": "Options considered and not taken.", "items": obj([]string{"option"}, map[string]any{
				"option": str("The alternative."), "reason": str("Why not."),
			})},
			"assumptions": map[string]any{"type": "array", "description": "What must hold for this to stay right.", "items": obj([]string{"statement"}, map[string]any{
				"statement": str("The assumption."), "check": str("Optional: how someone could check it."),
			})},
			"used":       strs("Ids of memories the decision relies on."),
			"evidence":   strs("Record ids, file paths, URLs, commit shas or bead ids that support it."),
			"bead":       str("Optional: the bead (issue) this is for."),
			"supersedes": str("Optional: id of an earlier decision this replaces."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Question string        `json:"question"`
			Choice   string        `json:"choice"`
			Why      string        `json:"why"`
			Rejected []smem.Option `json:"rejected"`
			Assume   []struct {
				Statement string `json:"statement"`
				Check     string `json:"check"`
			} `json:"assumptions"`
			Used       []string `json:"used"`
			Evidence   []string `json:"evidence"`
			Bead       string   `json:"bead"`
			Supersedes string   `json:"supersedes"`
		}) (any, error) {
			if err := required(map[string]string{"question": a.Question, "choice": a.Choice, "why": a.Why}); err != nil {
				return nil, err
			}
			in := smem.DecisionInput{Question: a.Question, Choice: a.Choice, Rationale: a.Why, Rejected: a.Rejected,
				Used: a.Used, Evidence: a.Evidence, Bead: a.Bead, Supersedes: a.Supersedes}
			for _, as := range a.Assume {
				in.Assumptions = append(in.Assumptions, smem.AssumptionInput{Statement: as.Statement, CheckHint: as.Check})
			}
			id, flags, err := e.Decide(in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": id, "flagged": flags, "commit_trailer": "Decision: " + id}, nil
		}),
	},
	{
		name: "decision_outcome",
		desc: "Report how a decision turned out. Success reinforces the memories it used.",
		schema: obj([]string{"id", "outcome"}, map[string]any{
			"id":      str("Decision id (dec-...)."),
			"outcome": enum("How it went.", "success", "failure", "mixed"),
			"note":    str("What happened."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			ID      string `json:"id"`
			Outcome string `json:"outcome"`
			Note    string `json:"note"`
		}) (any, error) {
			if err := required(map[string]string{"id": a.ID, "outcome": a.Outcome}); err != nil {
				return nil, err
			}
			return map[string]bool{"ok": true}, e.Outcome(a.ID, a.Outcome, a.Note)
		}),
	},
	{
		name:   "why",
		desc:   "Explain the history and reasons behind a decision, memory, assumption, source, action, commit sha or bead id: who recorded it, what it relied on, what was rejected, and what changed since.",
		schema: obj([]string{"ref"}, map[string]any{"ref": str("An id (dec-, mem-, asm-, src-, act-), a commit sha or a bead id.")}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Ref string `json:"ref"`
		}) (any, error) {
			if err := required(map[string]string{"ref": a.Ref}); err != nil {
				return nil, err
			}
			t, err := e.Why(a.Ref)
			if err != nil {
				return nil, err
			}
			return plain(t.Text()), nil
		}),
	},
	{
		name:   "review",
		desc:   "Records that need attention: suspect memories and decisions, decisions at risk because something they rely on changed, conflicting versions, and fact-checks that need a person.",
		schema: obj(nil, map[string]any{}),
		call: handler(func(_ context.Context, e *smem.Engine, _ struct{}) (any, error) {
			return e.Review()
		}),
	},
	{
		name: "failure_record",
		desc: "Record a failure: a broken build, a failing test, a change that had to be reverted, a mistake a person corrected. Use the same label for the same kind of failure. Repeated failures with one label are what justify a new rule.",
		schema: obj([]string{"label", "summary"}, map[string]any{
			"label":   str("Short kebab-case kind of failure, e.g. flaky-integration-test, forgot-migration, wrong-port."),
			"summary": str("What went wrong, in one or two sentences."),
			"ref":     str("Optional: test path, commit, bead id or URL."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Label   string `json:"label"`
			Summary string `json:"summary"`
			Ref     string `json:"ref"`
		}) (any, error) {
			id, promoted, err := e.RecordFailure(smem.FailureInput{Label: a.Label, Summary: a.Summary, Ref: a.Ref})
			return map[string]any{"id": id, "admitted_rules": promoted}, err
		}),
	},
	{
		name: "rule_propose",
		desc: "Propose a standing rule for agents in this project. Never write rules into CLAUDE.md or rule files yourself. A rule becomes active only with a reason: a person explicitly asked for it (why=requested, with by and their exact words in quote), repeated failures of one kind (why=incident, with the failure label, ideally enforced_by a test), or a recorded decision (why=decision). Anything else is kept as a held proposal for a person to review.",
		schema: obj([]string{"text", "why"}, map[string]any{
			"text":        str("The rule, as one instruction."),
			"why":         enum("The reason.", "requested", "incident", "decision"),
			"by":          str("requested: who asked."),
			"quote":       str("requested: what they said, verbatim."),
			"transcript":  str("requested: where they said it (transcript path or link)."),
			"label":       str("incident: the failure label this rule prevents."),
			"enforced_by": str("incident: the test or check that now catches it, e.g. test:./ci/migrations_test.go."),
			"decision":    str("decision: the dec- id it carries out."),
			"scope":       str("Optional scope; default scopes.write."),
		}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Text       string `json:"text"`
			Why        string `json:"why"`
			By         string `json:"by"`
			Quote      string `json:"quote"`
			Transcript string `json:"transcript"`
			Label      string `json:"label"`
			EnforcedBy string `json:"enforced_by"`
			Decision   string `json:"decision"`
			Scope      string `json:"scope"`
		}) (any, error) {
			if err := required(map[string]string{"text": a.Text, "why": a.Why}); err != nil {
				return nil, err
			}
			return e.ProposeRule(smem.RuleInput{Text: a.Text, Scope: a.Scope, Label: a.Label,
				Why: smem.Justification{Kind: a.Why, By: a.By, Quote: a.Quote, Transcript: a.Transcript,
					Decision: a.Decision, EnforcedBy: a.EnforcedBy}})
		}),
	},
	{
		name:   "rules_list",
		desc:   "The project's rules with their reasons. Active rules are in force; proposed ones are held for review.",
		schema: obj(nil, map[string]any{"status": enum("Default active.", "active", "proposed", "suspect", "retired", "rejected")}),
		call: handler(func(_ context.Context, e *smem.Engine, a struct {
			Status string `json:"status"`
		}) (any, error) {
			type item struct {
				ID     string `json:"id"`
				Text   string `json:"text"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			}
			rs, err := e.Rules(cmp.Or(a.Status, "active"))
			out := []item{}
			for _, r := range rs {
				out = append(out, item{r.ID, r.Text, r.Status, cmp.Or(r.Basis, r.Held)})
			}
			return out, err
		}),
	},
	{
		name: "factcheck",
		slow: true,
		desc: "Check memories against their sources and related memories, using the configured checkers and judges. Confidently supported memories are reconfirmed; confidently contradicted ones are marked suspect (never deleted).",
		schema: obj(nil, map[string]any{
			"ids":     strs("Memory ids to check. If empty, checks memories whose confidence has decayed."),
			"limit":   num("With no ids: max memories to check, default 5."),
			"dry_run": boolean("Record the judgment but change nothing."),
		}),
		call: handler(func(ctx context.Context, e *smem.Engine, a struct {
			IDs    []string `json:"ids"`
			Limit  int      `json:"limit"`
			DryRun bool     `json:"dry_run"`
		}) (any, error) {
			if len(a.IDs) == 0 {
				if a.Limit <= 0 {
					a.Limit = 5
				}
				due, err := e.FactCheckDue(0.6, a.Limit)
				if err != nil {
					return nil, err
				}
				for _, d := range due {
					a.IDs = append(a.IDs, d.ID)
				}
			}
			if len(a.IDs) > 20 {
				return nil, errors.New("at most 20 ids per call")
			}
			type result struct {
				ID        string       `json:"id"`
				Content   string       `json:"content,omitempty"`
				Verdict   smem.Verdict `json:"verdict"`
				DecidedBy string       `json:"decided_by,omitempty"`
				Action    string       `json:"action,omitempty"`
				Error     string       `json:"error,omitempty"`
			}
			out := []result{}
			for _, id := range a.IDs {
				r, err := e.FactCheck(ctx, id, !a.DryRun)
				if err != nil {
					out = append(out, result{ID: id, Error: err.Error()})
					continue
				}
				out = append(out, result{ID: id, Content: r.Content, Verdict: r.Verdict, DecidedBy: r.DecidedBy, Action: r.Action})
			}
			return out, nil
		}),
	},
}
