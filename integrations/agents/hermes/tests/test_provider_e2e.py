"""Real-CLI tests for the synapse-mem provider (no mocks), loading the plugin from this repo.

Run with the Hermes venv python. HERMES_AGENT_DIR points at the hermes-agent checkout (default ~/.hermes/hermes-agent).
"""

import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

PLUGIN_DIR = Path(__file__).resolve().parent.parent / "plugin"
sys.path.insert(0, os.environ.get("HERMES_AGENT_DIR") or str(Path.home() / ".hermes" / "hermes-agent"))
HOME = tempfile.mkdtemp(prefix="smem-hh-")
os.environ["HERMES_HOME"] = HOME
os.environ.pop("SMEM_DB", None)
os.environ.pop("SMEM_BIN", None)
os.chdir(tempfile.mkdtemp(prefix="smem-cwd-"))
(Path(HOME) / "plugins").mkdir()
(Path(HOME) / "plugins" / "synapse-mem").symlink_to(PLUGIN_DIR)

from plugins.memory import find_provider_dir, load_memory_provider  # noqa: E402

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond)))
    print(f"{'PASS' if cond else 'FAIL'}  {name}  {detail}")


found = find_provider_dir("synapse-mem")
check("discovery: find_provider_dir (repo plugin)", found is not None and Path(found).resolve() == PLUGIN_DIR, str(found))
p = load_memory_provider("synapse-mem", register_skills=False)
check("discovery: load_memory_provider", p is not None and p.name == "synapse-mem", type(p).__name__)

check("is_available (fresh home, no store yet)", p.is_available())
p.initialize("test-session", hermes_home=HOME, platform="cli", agent_context="primary")
db = Path(HOME) / "synapse" / "smem.db"
check("initialize creates per-profile store", db.exists() and not (db.parent / ".smem").exists(), str(db))
check("system_prompt_block non-empty", "decision_why" in p.system_prompt_block())
names = {s["name"] for s in p.get_tool_schemas()}
check("13 tool schemas", names == {"memory_remember", "memory_recall", "memory_update", "decision_record",
                                   "decision_outcome", "decision_why", "memory_review", "memory_signal",
                                   "memory_context", "failure_record", "rule_propose", "rules_list", "factcheck"},
      str(sorted(names)))
check("every schema has a handler", names == set(p._TOOLS))
check("system_prompt_block names the new tools",
      all(t in p.system_prompt_block() for t in ("memory_signal", "failure_record", "rule_propose", "rules_list")))

r = json.loads(p.handle_tool_call("memory_remember", {"content": "The T2 work repo is hermes-setup",
                                                      "kind": "fact", "scope": "test", "volatility": "stable"}))
mem_id = r.get("memory_id", "")
check("memory_remember returns mem id", mem_id.startswith("mem-"), json.dumps(r))
r = json.loads(p.handle_tool_call("memory_recall", {"query": "T2 work repo", "limit": 5}))
check("memory_recall roundtrip", any(x["id"] == mem_id for x in r.get("results", [])), json.dumps(r)[:200])

r = json.loads(p.handle_tool_call("decision_record", {
    "question": "Where does the smem store live?", "choice": "<hermes_home>/synapse/smem.db",
    "rationale": "sessions move between cwds", "rejected_options": [{"option": "cwd walk-up", "reason": "cwd moves"}],
    "assumptions": [{"statement": "one store per profile", "check_hint": "ls synapse/"}],
    "used_memory_ids": [mem_id], "evidence": ["file:plugin"]}))
dec = r.get("decision_id", "")
check("decision_record returns dec id", dec.startswith("dec-"), json.dumps(r))
r = json.loads(p.handle_tool_call("decision_outcome", {"decision_id": dec, "outcome": "success", "note": "ok"}))
check("decision_outcome", r.get("success"), json.dumps(r))
r = json.loads(p.handle_tool_call("decision_why", {"reference": dec}))
d = (r.get("decisions") or [{}])[0]
check("decision_why provenance", d.get("rejected", [{}])[0].get("option") == "cwd walk-up"
      and d.get("used", [{}])[0].get("id") == mem_id and d["decision"]["outcome"] == "success")
r = json.loads(p.handle_tool_call("memory_update", {"memory_id": mem_id, "action": "supersede",
                                                    "content": "The T2 work repo is ~/hermes-setup"}))
check("memory_update supersede", "supersedes" in r.get("result", ""), json.dumps(r))
r = json.loads(p.handle_tool_call("memory_review", {}))
check("memory_review returns suspect decision", any(s.get("id") == dec or dec in json.dumps(s)
                                                    for s in (r.get("suspect") or [])), json.dumps(r)[:200])
r = json.loads(p.handle_tool_call("memory_update", {"memory_id": "mem-doesnotexist", "action": "retract"}))
check("tool error is JSON, not raise", "error" in r, json.dumps(r))
r = json.loads(p.handle_tool_call("memory_remember", {}))
check("missing param -> error", "error" in r)

out = p.prefetch("where is the T2 work repo")
check("prefetch returns formatted text", out.startswith("## synapse-mem") and "hermes-setup" in out, repr(out[:160]))
st = p.recall_status()
check("recall_status counts", st is not None and st.count == out.count("\n- ["), repr(st))
p.queue_prefetch("T2 work repo")
time.sleep(1.0)
out2 = p.prefetch("T2 work repo")
check("queue_prefetch -> prefetch consumes cached", "hermes-setup" in out2)
check("prefetch trivial prompt -> ''", p.prefetch("thanks!") == "" and p.recall_status() is None)
check("prefetch no match -> '' and no status", p.prefetch("zzqx unmatched qwerty") == "" and p.recall_status() is None)

p.on_memory_write("add", "user", "User likes terse code comments", {"write_origin": "test"})
p.shutdown()
r = json.loads(p.handle_tool_call("memory_recall", {"query": "terse code comments"}))
check("on_memory_write mirrors into smem", any(x.get("scope") == "hermes-native-user" for x in r["results"]),
      json.dumps(r["results"][:1]))

msgs = [{"role": "user", "content": "q1"}, {"role": "assistant", "content": "a1"},
        {"role": "user", "content": "what is the capital of Peru"}, {"role": "assistant", "content": "Lima."}]
p.on_session_end(msgs)
r = json.loads(p.handle_tool_call("memory_recall", {"query": "capital of Peru", "kind": "episode"}))
check("session-end episode off by default", not r["results"])
p.save_config({"enable_turn_capture": True}, HOME)
p2 = load_memory_provider("synapse-mem", register_skills=False)
p2.initialize("s2", hermes_home=HOME, platform="cli", agent_context="primary")
p2.on_session_end(msgs[:2])
p2.on_session_end(msgs)
r = json.loads(p2.handle_tool_call("memory_recall", {"query": "capital of Peru", "kind": "episode"}))
check("session-end episode with capture on (>=3 msgs)", len(r["results"]) == 1 and "Lima" in r["results"][0]["content"],
      json.dumps(r["results"])[:200])

def episodes(word):
    r = json.loads(p2.handle_tool_call("memory_recall", {"query": "largest moon", "kind": "episode", "limit": 20}))
    return [x for x in r["results"] if word in x["content"]]


idem = [{"role": "user", "content": "q1"}, {"role": "assistant", "content": "a1"},
        {"role": "user", "content": "largest moon of Saturn"}, {"role": "assistant", "content": "Titan."}]
p3 = load_memory_provider("synapse-mem", register_skills=False)
p3.initialize("s-idem", hermes_home=HOME, platform="cli", agent_context="primary")
p3.on_session_end(idem)
p3.on_session_end(idem)
p3b = load_memory_provider("synapse-mem", register_skills=False)
p3b.initialize("s-idem", hermes_home=HOME, platform="cli", agent_context="primary")
p3b.on_session_end(idem)
check("session-end idempotent across duplicate callbacks and re-instantiation", len(episodes("Saturn")) == 1,
      str(len(episodes("Saturn"))))
grown = idem + [{"role": "user", "content": "largest moon of Jupiter"}, {"role": "assistant", "content": "Ganymede."}]
p3b.on_session_end(grown)
p3b.on_session_end(grown)
check("appended completed turn yields a new episode", len(episodes("Jupiter")) == 1
      and len(episodes("Saturn")) == 1)
p4 = load_memory_provider("synapse-mem", register_skills=False)
p4.initialize("s-other", hermes_home=HOME, platform="cli", agent_context="primary")
p4.on_session_end(idem)
check("same messages in a different session are captured", len(episodes("Saturn")) == 2)


def _episodes(query):
    return json.loads(p2.handle_tool_call("memory_recall", {"query": query, "kind": "episode", "limit": 10}))["results"]


def _end(sid, msgs):
    p = load_memory_provider("synapse-mem", register_skills=False)
    p.initialize(sid, hermes_home=HOME, platform="cli", agent_context="primary")
    p.on_session_end(msgs)


call = {"id": "c1", "type": "function", "function": {"name": "terminal", "arguments": "{}"}}
_end("s-pair1", [{"role": "user", "content": "alpha question about pelicans"},
                   {"role": "assistant", "content": "alpha answer pelicans fly"},
                   {"role": "user", "content": "beta interrupted prompt about walruses"}])
eps = _episodes("pelicans")
check("pairing: interrupted prompt keeps last completed exchange",
      len(eps) == 1 and "alpha question about pelicans" in eps[0]["content"]
      and "alpha answer pelicans fly" in eps[0]["content"], json.dumps(eps)[:200])
check("pairing: interrupted prompt is not stored", not _episodes("walruses"))

_end("s-pair2", [{"role": "user", "content": "gamma question about lemurs"},
                   {"role": "assistant", "content": "gamma answer lemurs climb"},
                   {"role": "user", "content": "delta pending prompt about otters"},
                   {"role": "assistant", "content": "", "tool_calls": [call]},
                   {"role": "tool", "tool_call_id": "c1", "content": "tool output"}])
eps = _episodes("otters")
check("pairing: pending tool result not paired with older reply",
      not any("otters" in e["content"] and "lemurs climb" in e["content"] for e in eps), json.dumps(eps)[:200])
eps = _episodes("lemurs")
check("pairing: pending tool result keeps last completed exchange",
      len(eps) == 1 and "gamma question about lemurs" in eps[0]["content"], json.dumps(eps)[:200])

_end("s-pair3", [{"role": "user", "content": "epsilon tool-only turn about badgers"},
                   {"role": "assistant", "content": "", "tool_calls": [call]},
                   {"role": "tool", "tool_call_id": "c1", "content": "tool output"},
                   {"role": "assistant", "content": "", "tool_calls": [call]}])
check("pairing: tool-only turns store nothing", not _episodes("badgers"))

_end("s-pair4", [{"role": "user", "content": "zeta question about herons"},
                   {"role": "assistant", "content": "", "tool_calls": [call]},
                   {"role": "tool", "tool_call_id": "c1", "content": "tool output"},
                   {"role": "assistant", "content": "zeta answer herons wade"}])
eps = _episodes("herons")
check("pairing: completed reply after tool use is stored",
      len(eps) == 1 and "zeta question about herons" in eps[0]["content"]
      and "zeta answer herons wade" in eps[0]["content"], json.dumps(eps)[:200])

def _turns(*words):
    out = []
    for w in words:
        out += [{"role": "user", "content": f"question about {w}"}, {"role": "assistant", "content": f"answer about {w}"}]
    return out


def _provider(sid, context="primary"):
    pv = load_memory_provider("synapse-mem", register_skills=False)
    pv.initialize(sid, hermes_home=HOME, platform="cli", agent_context=context)
    return pv


check("compression: no checkpoint capability advertised", _provider("s-c0").pre_compress_checkpoint_api_version == 1)
p.save_config({"enable_turn_capture": False}, HOME)
_provider("s-coff").on_pre_compress(_turns("kiwis", "lychees", "mangos"))
p.save_config({"enable_turn_capture": True}, HOME)
check("compression: nothing stored with capture off", not _episodes("kiwis"))

pc = _provider("s-comp")
before = _turns("kiwis", "lychees", "mangos") + [{"role": "user", "content": "interrupted prompt about durians"}]
check("compression: returns no summary", pc.on_pre_compress(before) == "")
pc.on_pre_compress(before)
_provider("s-comp").on_pre_compress(before)
eps = _episodes("kiwis")
check("compression: one episode keeps every completed turn across repeated callbacks and restarts",
      len(eps) == 1 and all(f"about {w}" in eps[0]["content"] for w in ("kiwis", "lychees", "mangos")),
      json.dumps(eps)[:300])
check("compression: interrupted prompt is not stored", not _episodes("durians"))

squashed = [{"role": "user", "content": "[summary of earlier turns]"}, {"role": "assistant", "content": "understood"}]
after = squashed + _turns("papayas", "guavas")
pc.on_pre_compress(after)
pc.on_pre_compress(after)
eps = _episodes("papayas")
check("compression: continuation captures a later completed turn once even at a reused message index",
      len(eps) == 1 and "about guavas" in eps[0]["content"] and "kiwis" not in eps[0]["content"],
      json.dumps(eps)[:300])

pc.on_session_end(after)
pc.on_session_end(after)
ends = [e for e in _episodes("guavas") if e["content"].startswith("Session episode")]
check("compression then session end stores one session episode with the last exchange",
      len(ends) == 1 and "about guavas" in ends[0]["content"] and "papayas" not in ends[0]["content"],
      json.dumps(ends)[:300])

_provider("s-csub", "subagent").on_pre_compress(_turns("quinces", "figs", "plums"))
check("compression: non-primary context skips writes", not _episodes("quinces"))

sub = load_memory_provider("synapse-mem", register_skills=False)
sub.initialize("s3", hermes_home=HOME, platform="cli", agent_context="subagent")
sub.on_session_end([{"role": "user", "content": "subagent zebra"}, {"role": "assistant", "content": "x"},
                    {"role": "user", "content": "subagent zebra"}, {"role": "assistant", "content": "zebra done"}])
r = json.loads(p2.handle_tool_call("memory_recall", {"query": "subagent zebra"}))
check("non-primary context skips writes", not r["results"])

# Fail-open: broken binary.
p.save_config({"smem_bin": "/nonexistent/smem"}, HOME)
bad = load_memory_provider("synapse-mem", register_skills=False)
check("broken smem_bin -> is_available False", not bad.is_available())
bad.initialize("s4", hermes_home=HOME, platform="cli")
check("broken smem_bin -> prefetch ''", bad.prefetch("T2 work repo") == "")
check("broken smem_bin -> tool error JSON", "error" in json.loads(bad.handle_tool_call("memory_recall", {"query": "x"})))
bad.on_session_end(msgs)
check("broken smem_bin -> on_pre_compress fails open", bad.on_pre_compress(msgs) == "")
bad.on_memory_write("add", "memory", "x")
bad.shutdown()
p.save_config({"smem_bin": ""}, HOME)
os.environ["SMEM_BIN"] = "/nonexistent/smem"
check("SMEM_BIN env nonexistent -> is_available False", not load_memory_provider("synapse-mem", register_skills=False).is_available())
del os.environ["SMEM_BIN"]

# Fail-open: binary present but store broken mid-session.
p3 = load_memory_provider("synapse-mem", register_skills=False)
p3.initialize("s5", hermes_home=HOME, platform="cli")
p3._env["SMEM_DB"] = "/nonexistent/dir/smem.db"
check("broken store -> prefetch ''", p3.prefetch("T2 work repo") == "")
check("broken store -> tool error JSON", "error" in json.loads(p3.handle_tool_call("memory_remember", {"content": "x"})))

# max_prefetch: 0 disables injection; junk falls back to the default without raising.
p.save_config({"max_prefetch": 0}, HOME)
z = load_memory_provider("synapse-mem", register_skills=False)
z.initialize("s6", hermes_home=HOME, platform="cli")
check("max_prefetch=0 preserved", z._max_prefetch == 0, str(z._max_prefetch))
check("max_prefetch=0 -> prefetch ''", z.prefetch("where is the T2 work repo") == "")
p.save_config({"max_prefetch": "abc"}, HOME)
z = load_memory_provider("synapse-mem", register_skills=False)
try:
    z.initialize("s7", hermes_home=HOME, platform="cli")
    check("max_prefetch='abc' -> default, no raise", z._max_prefetch == 10, str(z._max_prefetch))
except Exception as e:  # noqa: BLE001
    check("max_prefetch='abc' -> default, no raise", False, repr(e))
p.save_config({"max_prefetch": 500}, HOME)
z = load_memory_provider("synapse-mem", register_skills=False)
z.initialize("s8", hermes_home=HOME, platform="cli")
check("max_prefetch above cap is clamped", 0 < z._max_prefetch < 500, str(z._max_prefetch))
p.save_config({"max_prefetch": None}, HOME)
z = load_memory_provider("synapse-mem", register_skills=False)
z.initialize("s9", hermes_home=HOME, platform="cli")
check("max_prefetch=null -> default", z._max_prefetch == 10, str(z._max_prefetch))

# Session-start context: first turn of a new or reset session only, like smem's SessionStart hook.
repo = tempfile.mkdtemp(prefix="smem-repo-")
git = lambda *a: subprocess.run(["git", "-C", repo, *a], capture_output=True, text=True, check=True).stdout  # noqa: E731
git("init", "-q", "-b", "main")
git("config", "user.email", "t@example.com")
git("config", "user.name", "t")
mem_lines = lambda s: len(re.findall(r"^- \[mem-", s, re.M))  # noqa: E731
c = load_memory_provider("synapse-mem", register_skills=False)
c.initialize("s10", hermes_home=HOME, platform="cli", agent_context="primary", cwd=repo)
out = c.prefetch("thanks!")
check("session context on first turn, even for a trivial prompt", out.startswith("## smem: recalled memories")
      and "terse code comments" in out, repr(out[:160]))
st = c.recall_status()
check("recall_status counts session-context memories", st is not None and st.count == mem_lines(out) > 0, repr(st))
log = json.loads(c._run(["log", "--type", "recall", "--json", "--limit", "5"]) or "[]")
check("session context logged as injected (shown, not used)",
      any(e.get("session") == "s10" and e.get("payload", {}).get("injected") for e in log), json.dumps(log)[:300])
check("session context only once per start", c.prefetch("thanks!") == "")
out = c.prefetch("terse code comments")
check("later turns: per-turn recall only", out.startswith("## synapse-mem") and "## smem:" not in out, repr(out[:120]))
c.on_session_switch("s11", reset=True)
check("reset switch (/new) re-injects (source clear)", "## smem: recalled memories" in c.prefetch("thanks!"))
c.on_session_switch("s12", parent_session_id="s11")
check("resume/branch switch does not inject (context.on default)", c.prefetch("thanks!") == "")
c.on_session_switch("s12c", parent_session_id="s12", reason="compression")
check("compression switch does not inject (context.on default)", c.prefetch("thanks!") == "")
(db.parent / "config.json").write_text(json.dumps({"context": {"on": ["startup", "clear", "compact"]}}))
c.on_session_switch("s12d", parent_session_id="s12c", reason="compression")
check("compression injects when the store's context.on has compact", "## smem:" in c.prefetch("thanks!"))
(db.parent / "config.json").unlink()
p.save_config({"session_context": False}, HOME)
off = load_memory_provider("synapse-mem", register_skills=False)
off.initialize("s13", hermes_home=HOME, platform="cli")
check("session_context=false -> no injection", off.prefetch("thanks!") == "")
off.shutdown()
p.save_config({"session_context": True, "max_prefetch": 0}, HOME)
off = load_memory_provider("synapse-mem", register_skills=False)
off.initialize("s14", hermes_home=HOME, platform="cli")
check("max_prefetch=0 -> no session context either", off.prefetch("thanks!") == "")
off.shutdown()
p.save_config({"max_prefetch": None}, HOME)
c2 = load_memory_provider("synapse-mem", register_skills=False)
c2.initialize("s15", hermes_home=HOME, platform="cli", agent_context="subagent")
check("non-primary context -> no session context", c2.prefetch("thanks!") == "")
c2.shutdown()

# Tool calls: through Hermes' own post_tool_call dispatch into smem's spool, then linked to the decision.
from hermes_cli.lifecycle import invoke_hook  # noqa: E402

c = load_memory_provider("synapse-mem", register_skills=False)
c.initialize("s20", hermes_home=HOME, platform="cli", agent_context="primary", cwd=repo)
r = json.loads(c.handle_tool_call("decision_record", {"question": "Where do probe files go?", "choice": "repo root",
                                                      "rationale": "simplest"}))
dec = r.get("decision_id", "")
check("decision_record returns the commit trailer", r.get("commit_trailer") == f"Decision: {dec}", json.dumps(r))
(Path(repo) / "a.txt").write_text("a\n")
git("add", "a.txt")
commit_out = git("commit", "-m", "add a")
sha = git("rev-parse", "--short", "HEAD").strip()
spool = db.parent / "spool"
before = set(spool.glob("*.json")) if spool.exists() else set()
secret = "sk-ant-api03-" + "x" * 90
calls = [("terminal", {"command": "git commit -m 'add a'"}, json.dumps({"output": commit_out, "exit_code": 0})),
         ("write_file", {"path": f"{repo}/b.txt", "content": "b"}, json.dumps({"bytes_written": 1})),
         ("terminal", {"command": f"curl -H 'x-api-key: {secret}' https://example.com"}, json.dumps({"output": ""})),
         ("read_file", {"path": f"{repo}/a.txt"}, json.dumps({"content": "a"})),
         ("memory_recall", {"query": "x"}, "{}")]
for name, a, res in calls:
    invoke_hook("post_tool_call", tool_name=name, args=a, result=res, session_id="s20", task_id="", tool_call_id="t",
                turn_id="", api_request_id="", duration_ms=1, status="ok", error_type=None, error_message=None)
spooled = sorted(set(spool.glob("*.json")) - before)
check("mutating calls spooled; read-only and own tools skipped", len(spooled) == 3, str(len(spooled)))
raw = "".join(f.read_text() for f in spooled)
check("spooled summaries are redacted", secret not in raw and "x-api-key" in raw, raw[:200])
check("file tools summarized by path", f"{repo}/b.txt" in raw)
r = json.loads(c.handle_tool_call("decision_why", {"reference": sha}))
acts = [a for d in r.get("decisions") or [] for a in d.get("actions") or []]
check("git commit captured with sha and linked to the session decision",
      any(a.get("commit") == sha and a.get("decision_id") == dec for a in acts), json.dumps(r)[:300])
check("actions signed by the Hermes actor", all(a.get("actor", "").startswith("hermes-cli@") for a in acts))
r = json.loads(c.handle_tool_call("decision_record",
                                  {"question": "Keep b.txt?", "choice": "yes", "rationale": "probe"}))
dec2 = r["decision_id"]
(Path(repo) / "c.txt").write_text("c\n")
git("add", "c.txt")
git("commit", "-q", "-m", f"add c\n\n{r['commit_trailer']}")
r = json.loads(c.handle_tool_call("decision_why", {"reference": git("rev-parse", "HEAD").strip()}))
check("decision_why <sha> follows the Decision: trailer in the session cwd",
      any(d.get("decision", {}).get("id") == dec2 for d in r.get("decisions") or []), json.dumps(r)[:300])


def fire(name, a):
    before = set(spool.glob("*.json"))
    invoke_hook("post_tool_call", tool_name=name, args=a, result="{}", session_id="s20", task_id="", tool_call_id="t",
                turn_id="", api_request_id="", duration_ms=1, status="ok", error_type=None, error_message=None)
    return len(set(spool.glob("*.json")) - before)


read_calls = [("tool_search", {"query": "github"}), ("tool_describe", {"name": "terminal"}),
              ("browser_navigate", {"url": "https://example.com"}), ("browser_snapshot", {}),
              ("mcp_github_list_issues", {"repo": "a/b"}), ("github_get_pull_request", {"number": 1}),
              ("mcp__drive__describe_file", {"id": "x"}),
              ("tool_call", {"name": "tool_search", "arguments": {"query": "x"}}),
              ("tool_call", {"tool_name": "mcp_jira_get_issue", "args": json.dumps({"key": "A-1"})}),
              ("tool_call", {"function": {"name": "read_file", "arguments": {"path": "a.txt"}}}),
              ("tool_call", {"name": "tool_call", "arguments": {"name": "list_files", "arguments": {}}})]
check("discovery and read-only calls yield zero actions", all(fire(n, a) == 0 for n, a in read_calls),
      str([n for n, a in read_calls if fire(n, a)]))
check("write_file yields one action", fire("write_file", {"path": f"{repo}/w.txt", "content": "w"}) == 1)
check("terminal yields one action", fire("terminal", {"command": "touch z"}) == 1)
check("nested terminal yields one action",
      fire("tool_call", {"name": "terminal", "arguments": {"command": "touch z"}}) == 1)
check("nested mutating connector call yields one action",
      fire("tool_call", {"name": "mcp_github_create_issue", "arguments": {"title": "t"}}) == 1)
lost = ["log_work", "log_time", "mark_as_read", "mark_notifications_read", "resolve_comment_thread", "check_in",
        "extract_and_store", "forward_email", "assign_issue", "approve_pull_request", "archive_thread"]
check("mutations with read-like words are captured", all(fire(n, {"id": "1"}) == 1 for n in lost),
      str([n for n in lost if fire(n, {"id": "1"}) != 1]))
check("browser back and scroll navigation yield zero actions",
      all(fire(n, {}) == 0 for n in ("browser_back", "browser_forward", "browser_scroll", "browser_vision")))
check("unreadable wrapper is still captured", fire("tool_call", {"foo": "bar"}) == 1)
p.save_config({"capture_tool_calls": False}, HOME)
nc = load_memory_provider("synapse-mem", register_skills=False)
nc.initialize("s21", hermes_home=HOME, platform="cli")
before = set(spool.glob("*.json"))
invoke_hook("post_tool_call", tool_name="terminal", args={"command": "touch x"}, result="{}", session_id="s21")
check("capture_tool_calls=false -> nothing spooled", set(spool.glob("*.json")) == before)
nc.shutdown()
c.shutdown()
p.save_config({"capture_tool_calls": True}, HOME)
broken = load_memory_provider("synapse-mem", register_skills=False)
broken.initialize("s22", hermes_home=HOME, platform="cli")
broken._env["SMEM_DB"] = "/nonexistent/dir/smem.db"
try:
    broken.capture_tool_call("terminal", {"command": "touch y"}, "{}", "s22")
    check("broken store -> tool-call capture fails open", True)
except Exception as e:  # noqa: BLE001
    check("broken store -> tool-call capture fails open", False, repr(e))
broken.shutdown()

# The tools beyond the core seven, on a store of their own.
t = load_memory_provider("synapse-mem", register_skills=False)
p.save_config({"store_path": str(Path(tempfile.mkdtemp(prefix="smem-tools-")) / "smem.db")}, HOME)
t.initialize("s30", hermes_home=HOME, platform="cli", agent_context="primary")
for k in ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
          "TYPESAFE_API_KEY", "OPENROUTER_API_KEY"):
    t._env.pop(k, None)
t._env["SMEM_CONFIG"] = str(Path(HOME) / "no-local-smem-config.json")
call = lambda name, a: json.loads(t.handle_tool_call(name, a))  # noqa: E731
why = lambda ref: json.loads(t._run(["why", ref, "--json"]))  # noqa: E731

base = call("memory_remember", {"content": "The deploy target is fly.io", "kind": "fact"})["memory_id"]
r = call("memory_remember", {"content": "Deploys need the fly CLI", "kind": "procedure",
                             "derived_from": [base, "file:fly.toml"]})
derived = r.get("memory_id", "")
lineage = lambda ref: [x.get("id") for x in why(ref).get("memory", {}).get("derived_from") or []]  # noqa: E731
links = lineage(derived)
check("memory_remember derived_from -> lineage", derived.startswith("mem-") and base in links
      and any(x.startswith("src-") for x in links), json.dumps(links))
check("memory_remember derived_from not a list -> error",
      "error" in call("memory_remember", {"content": "x", "derived_from": "mem-1"}))
r = call("memory_update", {"memory_id": derived, "action": "supersede", "content": "Deploys need flyctl >= 0.3",
                           "derived_from": [base]})
new = r.get("result", "").split()[0] if r.get("result") else ""
check("memory_update supersede derived_from -> lineage",
      base in lineage(new), json.dumps(r))

r = call("memory_signal", {"memory_id": base, "signal": "confirmed", "note": "checked fly.toml"})
check("memory_signal confirmed", r.get("success") and r.get("signal") == "confirmed", json.dumps(r))
sigs = why(base).get("memory", {}).get("signals") or {}
check("memory_signal recorded on the memory", sigs.get("confirmed") == 1, json.dumps(sigs))
check("memory_signal bad signal -> error",
      "error" in call("memory_signal", {"memory_id": base, "signal": "liked"}))
check("memory_signal unknown memory -> error",
      "error" in call("memory_signal", {"memory_id": "mem-doesnotexist", "signal": "used"}))
check("memory_signal missing signal -> error", "error" in call("memory_signal", {"memory_id": base}))

r = call("memory_context", {})
check("memory_context returns smem context", r.get("context", "").startswith("## smem: recalled memories")
      and "fly.io" in r["context"] and r.get("count", 0) >= 1, json.dumps(r)[:200])
r = call("memory_context", {"budget": 120})
check("memory_context budget limits memories", r.get("count") == 0 and "fly" not in r.get("context", "x"),
      json.dumps(r)[:200])
log = json.loads(t._run(["log", "--type", "recall", "--json", "--limit", "1"]) or "[]")
check("memory_context logged as injected", bool(log) and log[0].get("payload", {}).get("injected"), json.dumps(log)[:200])
check("memory_context bad budget -> error", "error" in call("memory_context", {"budget": "lots"}))

r = call("decision_record", {"question": "Where to deploy?", "choice": "fly.io", "rationale": "cheap"})
d1 = r.get("decision_id", "")
r = call("decision_record", {"question": "Where to deploy?", "choice": "fly.io, two regions",
                             "rationale": "latency", "bead": "bd-42", "supersedes": d1})
d2 = r.get("decision_id", "")
w = why("bd-42")
got = next((x["decision"] for x in w.get("decisions") or [] if x["decision"]["id"] == d2), {})
check("decision_record bead + supersedes pass through", got.get("bead") == "bd-42" and got.get("supersedes") == d1,
      json.dumps(got)[:300])
check("decision_record supersedes unknown -> error", "error" in call("decision_record", {
    "question": "q", "choice": "c", "rationale": "r", "supersedes": "dec-doesnotexist"}))

r = call("failure_record", {"label": "forgot-migration", "summary": "deploy failed: missing column", "ref": "bd-42"})
f1 = r.get("failure_id", "")
check("failure_record returns fail id", f1.startswith("fail-") and r.get("admitted_rules") == [], json.dumps(r))
failures = t._run(["failures", "--label", "forgot-migration"])
check("failure_record stored with label", f1 in failures and "missing column" in failures, failures)
r = call("failure_record", {"label": "dash-summary", "summary": "-v flag broke the build"})
check("failure_record summary starting with '-' is content, not a flag", r.get("failure_id", "").startswith("fail-"),
      json.dumps(r))
check("failure_record missing summary -> error", "error" in call("failure_record", {"label": "x"}))

r = call("rule_propose", {"rule": "Run migrations before deploying", "why": "incident",
                          "label": "forgot-migration", "enforced_by": "test:./ci/migrations_test.go"})
held = r.get("rule_id", "")
check("rule_propose incident below threshold -> held proposal",
      held.startswith("rule-") and r.get("status") == "proposed" and not r.get("admitted") and r.get("reason"),
      json.dumps(r))
r = call("failure_record", {"label": "forgot-migration", "summary": "again: missing column"})
check("second failure admits the held rule", r.get("admitted_rules") == [held], json.dumps(r))
r = call("rule_propose", {"rule": "Use tabs", "why": "requested", "by": "ayush", "quote": "use tabs please"})
asked = r.get("rule_id", "")
check("rule_propose requested with quote -> active", r.get("status") == "active" and r.get("admitted"), json.dumps(r))
r = call("rule_propose", {"rule": "Follow the deploy decision", "why": "decision", "decision": d2})
check("rule_propose decision -> active", r.get("status") == "active", json.dumps(r))
r = call("rule_propose", {"rule": "Be careful", "why": "requested"})
vague = r.get("rule_id", "")
check("rule_propose without by/quote -> held", r.get("status") == "proposed", json.dumps(r))
check("rule_propose bad why -> error", "error" in call("rule_propose", {"rule": "x", "why": "vibes"}))
check("rule_propose missing why -> error", "error" in call("rule_propose", {"rule": "x"}))

r = call("rules_list", {})
active = {x["id"]: x for x in r.get("rules") or []}
check("rules_list default active", held in active and asked in active and vague not in active
      and all(x["status"] == "active" and x["reason"] for x in active.values()), json.dumps(r)[:300])
r = call("rules_list", {"status": "proposed"})
check("rules_list proposed shows held reason",
      [x["id"] for x in r.get("rules") or []] == [vague] and r["rules"][0]["reason"], json.dumps(r)[:300])
check("rules_list all", call("rules_list", {"status": "all"}).get("count") == 4)
check("rules_list bad status -> error", "error" in call("rules_list", {"status": "everything"}))

r = call("factcheck", {"memory_ids": [base]})
res = (r.get("results") or [{}])[0]
check("factcheck without judges -> not_judged, not an error",
      res.get("id") == base and res.get("not_judged") and res.get("action") == "not-judged" and r.get("not_judged") == 1,
      json.dumps(r)[:300])
st = why(base).get("memory", {}).get("memory", {}).get("status")
check("factcheck without judges changes nothing", st == "active", str(st))
r = call("factcheck", {})
check("factcheck with nothing due -> empty result, not an error", r.get("results") == [] and r.get("count") == 0,
      json.dumps(r))
r = call("factcheck", {"memory_ids": [base, new], "dry_run": True})
check("factcheck several ids", [x["id"] for x in r.get("results") or []] == [base, new], json.dumps(r)[:200])
check("factcheck unknown id -> error", "error" in call("factcheck", {"memory_ids": ["mem-doesnotexist"]}))
check("factcheck superseded memory -> error", "error" in call("factcheck", {"memory_ids": [derived]}))
check("factcheck too many ids -> error", "error" in call("factcheck", {"memory_ids": [base] * 21}))
check("factcheck memory_ids not a list -> error", "error" in call("factcheck", {"memory_ids": base}))

t._env["SMEM_DB"] = "/nonexistent/dir/smem.db"
for name, a in [("memory_signal", {"memory_id": base, "signal": "used"}), ("memory_context", {}),
                ("failure_record", {"label": "x", "summary": "y"}), ("rule_propose", {"rule": "x", "why": "requested"}),
                ("rules_list", {}), ("factcheck", {"memory_ids": [base]})]:
    try:
        check(f"broken store -> {name} error JSON", "error" in call(name, a))
    except Exception as e:  # noqa: BLE001
        check(f"broken store -> {name} error JSON", False, repr(e))
t.shutdown()
p.save_config({"store_path": ""}, HOME)

# Relevance floor: a dedicated store loaded from the repo fixture, one memory per line.
from importlib import import_module  # noqa: E402

_mod = import_module(type(p).__module__)
STORE_LINES = [ln.strip() for ln in
               (Path(__file__).resolve().parents[4] / "smem" / "testdata" / "recall_store.txt").read_text().splitlines()
               if ln.strip()]
p.save_config({"store_path": str(Path(tempfile.mkdtemp(prefix="smem-floor-")) / "smem.db"),
               "session_context": False, "max_prefetch": 10}, HOME)
floor_p = load_memory_provider("synapse-mem", register_skills=False)
floor_p.initialize("s-floor", hermes_home=HOME, platform="cli", agent_context="primary")
for ln in STORE_LINES:
    floor_p._remember(ln)
check("relevance fixture loaded", len(STORE_LINES) == 198 and floor_p._run(["stats"]) != "")


def floor_provider(sid, **cfg):
    p.save_config({"min_relevance": None, "max_prefetch": 10, "session_context": False, **cfg}, HOME)
    pv = load_memory_provider("synapse-mem", register_skills=False)
    pv.initialize(sid, hermes_home=HOME, platform="cli", agent_context="primary")
    return pv


def injected(query, **cfg):
    return floor_provider("s-f-" + hashlib.md5((query + json.dumps(cfg)).encode()).hexdigest()[:8], **cfg).prefetch(query)


KUBECONFIG, CI_ROTATION = "kubeconfig for staging lives in the ops vault", "Rotate the CI deploy credentials"
ESPRESSO, STAGING_OUT = "Office espresso machine outages", "Staging outages do not page anyone"
check("22 default floor is 0.345", floor_provider("s-f-d")._min_relevance == 0.345)
out = injected("rotate kubeconfig credentials")
check("22 rotate kubeconfig credentials injects the CI rotation and kubeconfig memories",
      CI_ROTATION in out and KUBECONFIG in out, out[:300])
out = injected("staging database outages")
check("22 staging database outages injects a staging outage line and no espresso line",
      STAGING_OUT in out and ESPRESSO not in out, out[:400])
check("22 postgres lag alert injects the postgres memory", "Postgres replica lag pages the on-call" in injected("postgres lag alert"))
check("22 what is the cafeteria situation injects the cafeteria memory",
      "The cafeteria menu changes weekly" in injected("what is the cafeteria situation"))
for q, needle in [("where do we keep the kubeconfig", KUBECONFIG),
                  ("i want to know what the cafeteria menu looks like this week", "The cafeteria menu changes weekly"),
                  ("is the postgres replica lag alert paging anyone right now", "Postgres replica lag pages the on-call"),
                  ("who should i contact about the office espresso machine being broken", ESPRESSO)]:
    check(f"23 natural prompt injects its memory: {q}", needle in injected(q))
check("24 kubernetes rollout espresso injects nothing", injected("kubernetes rollout espresso") == "")
check("24 chatty kubeconfig prompt misses the kubeconfig memory (pinned trade-off)",
      KUBECONFIG not in injected("hey can you remind me where the kubeconfig for staging is kept"))
out = injected("i need to rotate the kubeconfig credentials before the staging deploy")
check("24 long rotation prompt keeps the CI rotation memory and drops the kubeconfig memory (pinned trade-off)",
      CI_ROTATION in out and KUBECONFIG not in out, out[:300])
check("24 staging outage page prompt drops the staging outage memory (pinned trade-off)",
      STAGING_OUT not in injected("can you check whether staging database outages page the on-call team"))
check("24 dog prompt injects the office Friday memory (pinned trade-off)",
      "The office closes early on the last Friday" in injected("can i bring my dog to the office on fridays"))

zero = floor_provider("s-f-zero", min_relevance=0)
fetches = []
_orig_recall = zero._recall
zero._recall = lambda q, limit, kind="", record=True: (fetches.append(limit), _orig_recall(q, limit, kind, record))[1]
out = zero.prefetch("kubernetes rollout espresso")
check("25 min_relevance 0 injects the espresso memory with fetch size max_prefetch",
      ESPRESSO in out and fetches == [10], f"{fetches} {out[:200]}")
check("26 min_relevance 0.3 injects the kubeconfig memory for the chatty prompt",
      KUBECONFIG in injected("hey can you remind me where the kubeconfig for staging is kept", min_relevance=0.3))
q = floor_provider("s-f-queue")
q.queue_prefetch("staging database outages")
time.sleep(1.5)
out = q.prefetch("staging database outages")
check("27 queued then consumed prefetch is also filtered", STAGING_OUT in out and ESPRESSO not in out, out[:300])

for raw, want in [(None, 0.345), ("", 0.345), ("abc", 0.345), (True, 0.345), (float("nan"), 0.345),
                  (float("inf"), 0.345), ("0.25", 0.25), (-1, 0.0), (5, 1.0)]:
    check(f"28 _min_relevance({raw!r}) == {want}", _mod._min_relevance(raw) == want, str(_mod._min_relevance(raw)))
rows = [{"id": "a", "relevance": 0.5}, {"id": "b", "relevance": 0.345}, {"id": "c", "relevance": 0.2},
        {"id": "d"}, {"id": "e", "relevance": "x"}]
check("29 _above_floor drops rows below the floor, keeps rows at the floor and rows without a numeric relevance",
      [r["id"] for r in _mod._above_floor(rows, 0.345)] == ["a", "b", "d", "e"])
check("29 _above_floor returns the input unchanged for floor 0", _mod._above_floor(rows, 0) is rows)

ctx = floor_provider("s-f-ctx", session_context=True)
out = ctx.prefetch("kubernetes rollout espresso")
check("31 session context still renders when every per-turn row is below the floor",
      out.startswith("## smem: recalled memories") or "## smem: recalled memories" in out, out[:200])

tool = floor_provider("s-f-tool")
r = json.loads(tool.handle_tool_call("memory_recall", {"query": "staging database outages", "limit": 50}))
check("30 memory_recall is unfiltered and carries relevance",
      any(ESPRESSO in x["content"] for x in r["results"]) and all(isinstance(x.get("relevance"), float) for x in r["results"]),
      json.dumps(r["results"][:1]))
p.save_config({"store_path": "", "min_relevance": None, "session_context": True}, HOME)

failed = [n for n, ok in results if not ok]
print(f"\n{len(results) - len(failed)}/{len(results)} passed" + (f"; FAILED: {failed}" if failed else ""))
sys.exit(1 if failed else 0)
