// Command smem is the CLI for the smem agent memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rahulsub/synapse-mem/mcp"
	"github.com/rahulsub/synapse-mem/smem"
)

const usage = `smem: agent memory with decay, decision provenance and a signed audit log

Usage: smem [--db path] [--actor name] [--session id] <command> [args]

Memory
  remember <content>       --kind fact|episode|procedure|preference|insight --scope --volatility
                           --confidence --valid-from --from id,...
  supersede <mem> <content> new version of a memory (flags as remember)
  invalidate <mem>         it stopped being true   --reason --valid-to
  retract <mem>            it was never true       --reason
  reconfirm <mem>          checked, still true     --confidence --note
  signal <mem> used|confirmed|contradicted  --note
  recall [query]           --kind --scope --limit --as-of --known-at --archived --no-record --explain --json
  context                  memories for session start   --budget
  sweep                    archive faded memories   --threshold --idle --dry-run

Decisions and work
  decide                   --question --choice --why --reject "opt|reason" --assume "stmt|check"
                           --used mem,... --evidence ref,... --context mem,... --bead --supersedes --model --prompt-hash
  outcome <dec> success|failure|mixed  --note
  approve <dec>            --by --scope --note
  reaffirm <dec>           --note
  revert <dec>             --reason
  assumptions              --status open|held|broken
  assumption break|hold <asm>  --reason
  action                   --tool --summary --decision --bead --commit
  source <ref>             --kind --hash --note
  trailer <dec>            print the commit trailer for a decision

Audit
  why <ref>                decision, memory, assumption, source, action, commit sha or bead id   --json
  review                   suspect, at-risk and conflicting records; untrusted keys   --json
  log                      --subject --type --limit --json
  verify                   check every hash, chain link and signature   --json
  heads                    latest hash per replica (publish these to anchor the log)
  rebuild                  drop and replay all projections

Keys and sync
  keys                     list signing keys
  keys trust|distrust <key-id>
  remote add <name> <dir>  | remote list
  sync [remote]            pull then push
  push [remote] | pull [remote]
  export [--replica id]    events as JSON lines on stdout
  import <file|->          verify and merge events

Fact-checking
  factcheck [mem...]       user checkers first, then judges (Jev, then Claude if unsure)
                           --due --threshold --limit --parallel --dry-run --json
  judges                   which judges and checkers are configured

Setup
  init                     create .smem/smem.db here
  mcp                      serve this store to an agent over MCP (stdio)   --init --session
  version                  print the version
  hook post-tool-use       record a Claude Code tool call (reads hook JSON on stdin)  --all
  hook session-start       print context for a Claude Code SessionStart hook  --budget
`

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "smem:", err)
		os.Exit(1)
	}
}

type cli struct {
	e   *smem.Engine
	in  io.Reader
	out io.Writer
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	global := flag.NewFlagSet("smem", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(stdout, usage) }
	dbPath := global.String("db", "", "database path (default: nearest .smem/smem.db, or $SMEM_DB)")
	actor := global.String("actor", "", "actor name (default: $SMEM_ACTOR or $USER)")
	session := global.String("session", "", "session id (default: $SMEM_SESSION)")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if global.NArg() == 0 {
		fmt.Fprint(stdout, usage)
		return nil
	}
	cmd, rest := global.Arg(0), global.Args()[1:]
	switch cmd {
	case "help":
		fmt.Fprint(stdout, usage)
		return nil
	case "version":
		fmt.Fprintf(stdout, "smem %s\n", version)
		return nil
	}

	path := findDB(*dbPath)
	switch {
	case cmd == "init", cmd == "mcp" && path == "" && slices.Contains(rest, "--init"):
		if path == "" || *dbPath == "" {
			path = filepath.Join(".smem", "smem.db")
		}
	case path == "" && cmd == "hook":
		return nil // hooks must never fail the agent just because smem isn't set up
	case path == "":
		return errors.New("no smem database found; run `smem init` or pass --db")
	}
	e, err := smem.Open(path)
	if err != nil {
		return err
	}
	defer e.Close()
	if *actor != "" {
		e.Actor = *actor
	}
	if *session != "" {
		e.Session = *session
	}
	cfg, err := smem.LoadConfig(smem.ConfigPath(path))
	if err != nil {
		return err
	}
	if err := e.Configure(cfg); err != nil {
		return err
	}
	c := &cli{e: e, in: stdin, out: stdout}
	fn, ok := commands[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q (see `smem help`)", cmd)
	}
	if cmd == "hook" {
		// Report problems but never block the agent.
		if err := fn(c, rest); err != nil {
			fmt.Fprintln(os.Stderr, "smem hook:", err)
		}
		return nil
	}
	return fn(c, rest)
}

func findDB(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("SMEM_DB"); p != "" {
		return p
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(dir, ".smem", "smem.db")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

var commands = map[string]func(*cli, []string) error{
	"init": (*cli).initCmd, "remember": (*cli).remember, "supersede": (*cli).supersede,
	"invalidate": (*cli).invalidate, "retract": (*cli).retract, "reconfirm": (*cli).reconfirm,
	"signal": (*cli).signal, "recall": (*cli).recall, "context": (*cli).context, "sweep": (*cli).sweep,
	"decide": (*cli).decide, "outcome": (*cli).outcome, "approve": (*cli).approve, "reaffirm": (*cli).reaffirm,
	"revert": (*cli).revert, "assumptions": (*cli).assumptions, "assumption": (*cli).assumption,
	"action": (*cli).action, "source": (*cli).source, "trailer": (*cli).trailer,
	"why": (*cli).why, "review": (*cli).review, "log": (*cli).log, "verify": (*cli).verify,
	"heads": (*cli).heads, "rebuild": (*cli).rebuild, "keys": (*cli).keys, "remote": (*cli).remote,
	"sync": (*cli).sync, "push": (*cli).push, "pull": (*cli).pull, "export": (*cli).export,
	"import": (*cli).importCmd, "hook": (*cli).hook,
	"factcheck": (*cli).factcheck, "judges": (*cli).judges, "mcp": (*cli).mcp,
}

// parse parses flags that may appear before, between or after positional args.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func need(pos []string, n int, what string) error {
	if len(pos) < n {
		return fmt.Errorf("usage: %s", what)
	}
	return nil
}

type list []string

func (l *list) String() string { return strings.Join(*l, ",") }
func (l *list) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// multi collects a repeatable flag without splitting on commas.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, "; ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var relTime = regexp.MustCompile(`^-(\d+)([smhdw])$`)

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if s == "now" {
		return time.Now(), nil
	}
	if m := relTime.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return time.Now().Add(-time.Duration(n) * unit), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("can't parse time %q (use RFC3339, YYYY-MM-DD, or -3d / -12h)", s)
}

func parseDuration(s string) (time.Duration, error) {
	if m := regexp.MustCompile(`^(\d+)([dw])$`).FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]], nil
	}
	return time.ParseDuration(s)
}

func (c *cli) printf(format string, a ...any) { fmt.Fprintf(c.out, format, a...) }

func (c *cli) json(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *cli) flags(fl []smem.Flag) {
	for _, f := range fl {
		c.printf("  ⚠ %s is now suspect (because %s changed)\n", f.Subject, f.Cause)
	}
	if len(fl) > 0 {
		c.printf("  run `smem review` to look at them\n")
	}
}

// --- commands ---------------------------------------------------------------

func (c *cli) initCmd(args []string) error {
	c.printf("smem database: %s\nreplica: %s\nactor: %s (keys in %s)\n", c.e.Path(), c.e.Replica(), c.e.Actor, c.e.KeyDir)
	c.printf(`
To capture Claude Code activity automatically, add to .claude/settings.json:
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "smem hook session-start"}]}],
    "PostToolUse":  [{"hooks": [{"type": "command", "command": "smem hook post-tool-use"}]}]
  }
`)
	return nil
}

func memoryFlags(fs *flag.FlagSet) (*smem.MemoryInput, func() error) {
	in := &smem.MemoryInput{}
	var validFrom string
	var from list
	fs.StringVar(&in.Kind, "kind", "", "fact|episode|procedure|preference|insight")
	fs.StringVar(&in.Scope, "scope", "", "scope")
	fs.StringVar(&in.Volatility, "volatility", "", "volatile|normal|stable|permanent")
	fs.Float64Var(&in.Confidence, "confidence", 0, "0..1")
	fs.StringVar(&validFrom, "valid-from", "", "when it became true")
	fs.Var(&from, "from", "ids this was derived from")
	return in, func() error {
		t, err := parseTime(validFrom)
		in.ValidFrom, in.DerivedFrom = t, from
		return err
	}
}

func (c *cli) remember(args []string) error {
	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	in, finish := memoryFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, `smem remember "content" [--kind ...]`); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	in.Content = strings.Join(pos, " ")
	id, err := c.e.Remember(*in)
	if err != nil {
		return err
	}
	c.printf("%s\n", id)
	return nil
}

func (c *cli) supersede(args []string) error {
	fs := flag.NewFlagSet("supersede", flag.ContinueOnError)
	in, finish := memoryFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, `smem supersede <mem> "new content"`); err != nil {
		return err
	}
	if err := finish(); err != nil {
		return err
	}
	if in.Kind == "" {
		if old, err := c.e.Memory(pos[0]); err == nil {
			in.Kind = old.Kind
			if in.Scope == "" {
				in.Scope = old.Scope
			}
		}
	}
	in.Content = strings.Join(pos[1:], " ")
	id, fl, err := c.e.Supersede(pos[0], *in)
	if err != nil {
		return err
	}
	c.printf("%s supersedes %s\n", id, pos[0])
	c.flags(fl)
	return nil
}

func (c *cli) invalidate(args []string) error {
	fs := flag.NewFlagSet("invalidate", flag.ContinueOnError)
	reason := fs.String("reason", "", "why")
	validTo := fs.String("valid-to", "", "when it stopped being true (default now)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem invalidate <mem> --reason ..."); err != nil {
		return err
	}
	t, err := parseTime(*validTo)
	if err != nil {
		return err
	}
	fl, err := c.e.Invalidate(pos[0], *reason, t)
	if err != nil {
		return err
	}
	c.printf("%s invalidated\n", pos[0])
	c.flags(fl)
	return nil
}

func (c *cli) retract(args []string) error {
	fs := flag.NewFlagSet("retract", flag.ContinueOnError)
	reason := fs.String("reason", "", "why")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem retract <mem> --reason ..."); err != nil {
		return err
	}
	fl, err := c.e.Retract(pos[0], *reason)
	if err != nil {
		return err
	}
	c.printf("%s retracted\n", pos[0])
	c.flags(fl)
	return nil
}

func (c *cli) reconfirm(args []string) error {
	fs := flag.NewFlagSet("reconfirm", flag.ContinueOnError)
	conf := fs.Float64("confidence", 0, "new confidence (default keep)")
	note := fs.String("note", "", "how it was checked")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem reconfirm <mem>"); err != nil {
		return err
	}
	return c.e.Reconfirm(pos[0], *conf, *note)
}

func (c *cli) signal(args []string) error {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	note := fs.String("note", "", "note")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "smem signal <mem> used|confirmed|contradicted"); err != nil {
		return err
	}
	return c.e.Signal(pos[0], pos[1], *note)
}

func (c *cli) recall(args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	var kinds list
	fs.Var(&kinds, "kind", "kinds to include")
	scope := fs.String("scope", "", "scope")
	limit := fs.Int("limit", 10, "max results")
	asOf := fs.String("as-of", "", "what was true at this time")
	knownAt := fs.String("known-at", "", "as believed at this time")
	archived := fs.Bool("archived", false, "include archived memories")
	noRecord := fs.Bool("no-record", false, "don't record this recall")
	explain := fs.Bool("explain", false, "show score breakdown")
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	q := smem.RecallQuery{Text: strings.Join(pos, " "), Kinds: kinds, Scope: *scope, Limit: *limit,
		IncludeArchived: *archived, NoRecord: *noRecord}
	if q.AsOf, err = parseTime(*asOf); err != nil {
		return err
	}
	if q.KnownAt, err = parseTime(*knownAt); err != nil {
		return err
	}
	hits, err := c.e.Recall(q)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(hits)
	}
	if len(hits) == 0 {
		c.printf("(nothing)\n")
	}
	for _, h := range hits {
		mark := ""
		if h.Status != "active" {
			mark = " [" + h.Status + "]"
		}
		c.printf("%s  %.3f  %-10s %s%s\n", h.ID, h.Score, h.Kind, oneLine(h.Content, 100), mark)
		if *explain {
			c.printf("    relevance %.2f × salience %.2f × confidence %.2f   (activation %.2f, %d uses, last %s, %s)\n",
				h.Relevance, h.Salience, h.Confidence, h.Activation, h.Uses, h.LastAccess.Local().Format("2006-01-02"), h.Volatility)
		}
	}
	return nil
}

func (c *cli) context(args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	budget := fs.Int("budget", 4000, "max characters")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := c.e.Context(*budget)
	if err != nil {
		return err
	}
	c.printf("%s", s)
	return nil
}

func (c *cli) sweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	threshold := fs.Float64("threshold", 0.15, "archive below this salience")
	idle := fs.String("idle", "30d", "only if untouched this long")
	dry := fs.Bool("dry-run", false, "list without archiving")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	d, err := parseDuration(*idle)
	if err != nil {
		return err
	}
	hits, err := c.e.Sweep(*threshold, d, *dry)
	if err != nil {
		return err
	}
	verb := "archived"
	if *dry {
		verb = "would archive"
	}
	for _, h := range hits {
		c.printf("%s %s (salience %.3f) %s\n", verb, h.ID, h.Salience, oneLine(h.Content, 80))
	}
	c.printf("%d memories %s\n", len(hits), verb)
	return nil
}

func splitPipe(s string) (string, string) {
	a, b, _ := strings.Cut(s, "|")
	return strings.TrimSpace(a), strings.TrimSpace(b)
}

func (c *cli) decide(args []string) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	var in smem.DecisionInput
	var rejects, assumes multi
	var used, evidence, ctx, skills list
	fs.StringVar(&in.Question, "question", "", "what was decided")
	fs.StringVar(&in.Choice, "choice", "", "what was chosen")
	fs.StringVar(&in.Rationale, "why", "", "rationale")
	fs.Var(&rejects, "reject", `"option|reason" (repeatable)`)
	fs.Var(&assumes, "assume", `"statement|how to check" (repeatable)`)
	fs.Var(&used, "used", "memories relied on")
	fs.Var(&evidence, "evidence", "ids or references")
	fs.Var(&ctx, "context", "memories in view (default: last recall in session)")
	fs.Var(&skills, "skills", "skills/rules loaded")
	fs.StringVar(&in.Bead, "bead", "", "bead id")
	fs.StringVar(&in.Supersedes, "supersedes", "", "decision this replaces")
	fs.StringVar(&in.Model, "model", "", "model id")
	fs.StringVar(&in.PromptHash, "prompt-hash", "", "hash of the prompt")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	for _, r := range rejects {
		o, why := splitPipe(r)
		in.Rejected = append(in.Rejected, smem.Option{Option: o, Reason: why})
	}
	for _, a := range assumes {
		s, hint := splitPipe(a)
		in.Assumptions = append(in.Assumptions, smem.AssumptionInput{Statement: s, CheckHint: hint})
	}
	in.Used, in.Evidence, in.Context, in.Skills = used, evidence, ctx, skills
	id, fl, err := c.e.Decide(in)
	if err != nil {
		return err
	}
	c.printf("%s\ncommit trailer: Decision: %s\n", id, id)
	c.flags(fl)
	return nil
}

func (c *cli) outcome(args []string) error {
	fs := flag.NewFlagSet("outcome", flag.ContinueOnError)
	note := fs.String("note", "", "note")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "smem outcome <dec> success|failure|mixed"); err != nil {
		return err
	}
	return c.e.Outcome(pos[0], pos[1], *note)
}

func (c *cli) approve(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	by := fs.String("by", "", "approver (default: actor)")
	scope := fs.String("scope", "", "what the approval covers")
	note := fs.String("note", "", "note")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem approve <dec> --by name"); err != nil {
		return err
	}
	if *by == "" {
		*by = c.e.Actor
	}
	return c.e.Approve(pos[0], *by, *scope, *note)
}

func (c *cli) reaffirm(args []string) error {
	fs := flag.NewFlagSet("reaffirm", flag.ContinueOnError)
	note := fs.String("note", "", "why it still stands")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem reaffirm <dec> --note ..."); err != nil {
		return err
	}
	return c.e.Reaffirm(pos[0], *note)
}

func (c *cli) revert(args []string) error {
	fs := flag.NewFlagSet("revert", flag.ContinueOnError)
	reason := fs.String("reason", "", "why")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem revert <dec> --reason ..."); err != nil {
		return err
	}
	fl, err := c.e.Revert(pos[0], *reason)
	if err != nil {
		return err
	}
	c.printf("%s reverted\n", pos[0])
	c.flags(fl)
	return nil
}

func (c *cli) assumptions(args []string) error {
	fs := flag.NewFlagSet("assumptions", flag.ContinueOnError)
	st := fs.String("status", "", "open|held|broken")
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	as, err := c.e.Assumptions(*st)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(as)
	}
	for _, a := range as {
		c.printf("%s [%s] %s  (for %s)\n", a.ID, a.Status, a.Statement, a.DecisionID)
		if a.CheckHint != "" {
			c.printf("    check: %s\n", a.CheckHint)
		}
	}
	return nil
}

func (c *cli) assumption(args []string) error {
	fs := flag.NewFlagSet("assumption", flag.ContinueOnError)
	reason := fs.String("reason", "", "what was found")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 2, "smem assumption break|hold <asm> --reason ..."); err != nil {
		return err
	}
	switch pos[0] {
	case "break":
		fl, err := c.e.BreakAssumption(pos[1], *reason)
		if err != nil {
			return err
		}
		c.printf("%s broken\n", pos[1])
		c.flags(fl)
		return nil
	case "hold":
		return c.e.HoldAssumption(pos[1], *reason)
	}
	return errors.New("usage: smem assumption break|hold <asm>")
}

func (c *cli) action(args []string) error {
	fs := flag.NewFlagSet("action", flag.ContinueOnError)
	var in smem.ActionInput
	fs.StringVar(&in.Tool, "tool", "", "tool or kind of work")
	fs.StringVar(&in.Summary, "summary", "", "what was done")
	fs.StringVar(&in.Decision, "decision", "", "decision this carries out")
	fs.StringVar(&in.Bead, "bead", "", "bead id")
	fs.StringVar(&in.Commit, "commit", "", "commit sha")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	id, err := c.e.RecordAction(in)
	if err != nil {
		return err
	}
	c.printf("%s\n", id)
	return nil
}

func (c *cli) source(args []string) error {
	fs := flag.NewFlagSet("source", flag.ContinueOnError)
	kind := fs.String("kind", "", "transcript|doc|commit|url|bead|human|...")
	hash := fs.String("hash", "", "content hash")
	note := fs.String("note", "", "note")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem source <ref>"); err != nil {
		return err
	}
	id, err := c.e.CaptureSource(*kind, pos[0], *hash, *note)
	if err != nil {
		return err
	}
	c.printf("%s\n", id)
	return nil
}

func (c *cli) trailer(args []string) error {
	if err := need(args, 1, "smem trailer <dec>"); err != nil {
		return err
	}
	if _, err := c.e.Decision(args[0]); err != nil {
		return err
	}
	c.printf("Decision: %s\n", args[0])
	return nil
}

func (c *cli) why(args []string) error {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem why <ref>"); err != nil {
		return err
	}
	t, err := c.e.Why(pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(t)
	}
	c.printf("%s", t.Text())
	return nil
}

func (c *cli) review(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rv, err := c.e.Review()
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(rv)
	}
	section := func(title string, items []smem.ReviewItem, line func(smem.ReviewItem)) {
		if len(items) == 0 {
			return
		}
		c.printf("%s\n", title)
		for _, it := range items {
			line(it)
		}
		c.printf("\n")
	}
	section("Suspect: something they relied on changed", rv.Suspect, func(it smem.ReviewItem) {
		c.printf("  %s (%s) %s\n    because %s: %s\n", it.ID, it.Kind, oneLine(it.Summary, 90), it.Cause, it.Reason)
	})
	section("At risk: downstream of a suspect record (not flagged automatically)", rv.AtRisk, func(it smem.ReviewItem) {
		c.printf("  %s (%s) %s\n    via %s\n", it.ID, it.Kind, oneLine(it.Summary, 90), it.Via)
	})
	section("Conflicts: more than one live successor", rv.Conflicts, func(it smem.ReviewItem) {
		c.printf("  %s → %s\n", it.ID, it.Summary)
	})
	if len(rv.UntrustedKeys) > 0 {
		c.printf("Untrusted keys (smem keys trust <key-id>)\n")
		for _, k := range rv.UntrustedKeys {
			c.printf("  %s %s (%d events from %s)\n", k.KeyID, k.Actor, k.Events, k.Replica)
		}
		c.printf("\n")
	}
	if len(rv.Suspect)+len(rv.AtRisk)+len(rv.Conflicts)+len(rv.UntrustedKeys) == 0 {
		c.printf("nothing to review\n")
	} else {
		c.printf("Resolve with: reaffirm/revert <dec>, reconfirm/invalidate <mem>, supersede to pick a version.\n")
	}
	return nil
}

func (c *cli) log(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	subject := fs.String("subject", "", "record id")
	typ := fs.String("type", "", "event type")
	limit := fs.Int("limit", 30, "max events")
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	evs, err := c.e.Events(*subject, *typ, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(evs)
	}
	for _, ev := range evs {
		c.printf("%s  %s\n", ev.String(), oneLine(string(ev.Payload), 100))
	}
	return nil
}

func (c *cli) verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rep, err := c.e.Verify()
	if err != nil {
		return err
	}
	if *asJSON {
		return c.json(rep)
	}
	reps := make([]string, 0, len(rep.Replicas))
	for r, n := range rep.Replicas {
		reps = append(reps, fmt.Sprintf("%s:%d", r, n))
	}
	sort.Strings(reps)
	c.printf("%d events across %d replica(s): %s\n", rep.Events, len(rep.Replicas), strings.Join(reps, " "))
	for _, p := range rep.Problems {
		c.printf("  ✗ %s\n", p)
	}
	if !rep.OK {
		return fmt.Errorf("verification failed: %d problem(s)", len(rep.Problems))
	}
	c.printf("ok: every hash, chain link and signature checks out\n")
	return nil
}

func (c *cli) heads(args []string) error {
	hs, err := c.e.Heads()
	if err != nil {
		return err
	}
	for _, h := range hs {
		c.printf("%s %d %s\n", h.Replica, h.RSeq, h.Hash)
	}
	return nil
}

func (c *cli) rebuild(args []string) error {
	if err := c.e.Rebuild(); err != nil {
		return err
	}
	c.printf("rebuilt all projections from the log\n")
	return nil
}

func (c *cli) keys(args []string) error {
	if len(args) >= 2 && (args[0] == "trust" || args[0] == "distrust") {
		return c.e.SetTrust(args[1], args[0] == "trust", "")
	}
	ks, err := c.e.Keys()
	if err != nil {
		return err
	}
	for _, k := range ks {
		t := "untrusted"
		if k.Trusted {
			t = "trusted"
		}
		c.printf("%s  %-12s %-9s %d events  replica %s\n", k.KeyID, k.Actor, t, k.Events, k.Replica)
	}
	return nil
}

func (c *cli) remote(args []string) error {
	if len(args) >= 3 && args[0] == "add" {
		return c.e.AddRemote(args[1], args[2])
	}
	rs, err := c.e.Remotes()
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		c.printf("no remotes (local only). Add one: smem remote add origin /path/to/shared/dir\n")
	}
	for n, d := range rs {
		c.printf("%s  %s\n", n, d)
	}
	return nil
}

func (c *cli) pickRemote(args []string) (smem.DirRemote, error) {
	rs, err := c.e.Remotes()
	if err != nil {
		return smem.DirRemote{}, err
	}
	name := "origin"
	if len(args) > 0 {
		name = args[0]
	} else if len(rs) == 1 {
		for n := range rs {
			name = n
		}
	}
	dir, ok := rs[name]
	if !ok {
		return smem.DirRemote{}, fmt.Errorf("no remote %q (smem remote add %s <dir>)", name, name)
	}
	return smem.DirRemote{Dir: dir}, nil
}

func (c *cli) report(rep *smem.ImportReport) {
	c.printf("pulled %d new event(s), %d already present", rep.Accepted, rep.Skipped)
	if rep.Rebuilt {
		c.printf(" (replayed projections)")
	}
	c.printf("\n")
	for _, r := range rep.Rejected {
		c.printf("  ✗ rejected %s\n", r)
	}
	c.flags(rep.Flags)
}

func (c *cli) sync(args []string) error {
	r, err := c.pickRemote(args)
	if err != nil {
		return err
	}
	rep, n, err := c.e.Sync(r)
	if rep != nil {
		c.report(rep)
	}
	if err != nil {
		return err
	}
	c.printf("pushed %d event(s)\n", n)
	return nil
}

func (c *cli) push(args []string) error {
	r, err := c.pickRemote(args)
	if err != nil {
		return err
	}
	n, err := c.e.Push(r)
	if err != nil {
		return err
	}
	c.printf("pushed %d event(s)\n", n)
	return nil
}

func (c *cli) pull(args []string) error {
	r, err := c.pickRemote(args)
	if err != nil {
		return err
	}
	rep, err := c.e.Pull(r)
	if err != nil {
		return err
	}
	c.report(rep)
	return nil
}

func (c *cli) export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	replica := fs.String("replica", "", "only this replica's feed")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	_, err := c.e.Export(c.out, *replica)
	return err
}

func (c *cli) importCmd(args []string) error {
	if err := need(args, 1, "smem import <file|->"); err != nil {
		return err
	}
	r := c.in
	if args[0] != "-" {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	evs, err := smem.ReadEvents(r)
	if err != nil {
		return err
	}
	rep, err := c.e.Import(evs)
	if err != nil {
		return err
	}
	c.report(rep)
	return nil
}

func (c *cli) hook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	all := fs.Bool("all", false, "also record read-only tools")
	budget := fs.Int("budget", 4000, "context budget (session-start)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "smem hook post-tool-use|session-start"); err != nil {
		return err
	}
	var in smem.HookInput
	if b, _ := io.ReadAll(c.in); len(b) > 0 {
		if err := json.Unmarshal(b, &in); err != nil {
			return fmt.Errorf("hook input: %w", err)
		}
	}
	if os.Getenv("SMEM_ACTOR") == "" && c.e.Actor == os.Getenv("USER") {
		c.e.Actor = "claude-code"
	}
	switch pos[0] {
	case "post-tool-use":
		_, err := c.e.RecordToolUse(in, *all)
		return err
	case "session-start":
		if in.SessionID != "" {
			if err := c.e.SetCurrentSession(in.SessionID); err != nil {
				return err
			}
		}
		s, err := c.e.Context(*budget)
		if err != nil {
			return err
		}
		c.printf("%s", s)
		return nil
	}
	return fmt.Errorf("unknown hook %q", pos[0])
}

func (c *cli) factcheck(args []string) error {
	fs := flag.NewFlagSet("factcheck", flag.ContinueOnError)
	due := fs.Bool("due", false, "check memories whose confidence has decayed")
	threshold := fs.Float64("threshold", 0.6, "with --due: confidence below which a memory is due")
	limit := fs.Int("limit", 10, "with --due: max memories")
	dry := fs.Bool("dry-run", false, "record the judgment but don't reconfirm or flag")
	parallel := fs.Int("parallel", 4, "checks to run at once")
	asJSON := fs.Bool("json", false, "JSON output")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *due {
		hits, err := c.e.FactCheckDue(*threshold, *limit)
		if err != nil {
			return err
		}
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 {
		return errors.New("usage: smem factcheck <mem>... or --due")
	}
	// Judge calls dominate and run outside the database lock, so checks
	// overlap well; results print in the order asked.
	results := make([]*smem.FactCheckResult, len(ids))
	errs := make([]error, len(ids))
	sem := make(chan struct{}, max(*parallel, 1))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i], errs[i] = c.e.FactCheck(context.Background(), id, !*dry)
		}()
	}
	wg.Wait()
	for i, r := range results {
		if errs[i] != nil {
			return fmt.Errorf("%s: %w", ids[i], errs[i])
		}
		if *asJSON {
			continue
		}
		c.printf("%s  %s %.2f by %s → %s\n    %s\n", r.Memory, r.Verdict.Label, r.Verdict.Confidence, r.DecidedBy, r.Action, oneLine(r.Content, 100))
		if r.Verdict.Reason != "" {
			c.printf("    why: %s\n", oneLine(r.Verdict.Reason, 160))
		}
		for _, t := range r.Trail {
			a := t.Answers["relation"]
			c.printf("    %s (%s): %s %.2f, same subject %.2f, %d ms\n", t.Judge, t.Model, a.Choice, a.Confidence,
				t.Answers["same_subject"].Probability, t.LatencyMs)
		}
		for _, e := range r.Errors {
			c.printf("    ! %s\n", e)
		}
	}
	if *asJSON {
		return c.json(results)
	}
	return nil
}

func (c *cli) judges(args []string) error {
	names := c.e.Judges.Names()
	if len(names) == 0 {
		c.printf("judges: none (set TYPESAFE_API_KEY for Jev, ANTHROPIC_API_KEY for Claude)\n")
	} else {
		c.printf("judges, in order: %s\n", strings.Join(names, " → "))
	}
	if len(c.e.Checkers) == 0 {
		c.printf("checkers: none (add them to %s)\n", smem.ConfigPath(c.e.Path()))
	}
	for _, k := range c.e.Checkers {
		c.printf("checker: %s\n", k.Name())
	}
	return nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

func (c *cli) mcp(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.Bool("init", false, "create .smem/smem.db here if no store is found")
	session := fs.String("session", "", "file everything under this session (default: the session the hook recorded)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	fromClient := os.Getenv("SMEM_ACTOR") == "" && c.e.Actor == os.Getenv("USER")
	s := &mcp.Server{Engine: c.e, Version: version, Session: *session, ActorFromClient: fromClient}
	return s.Serve(context.Background(), c.in, c.out)
}
