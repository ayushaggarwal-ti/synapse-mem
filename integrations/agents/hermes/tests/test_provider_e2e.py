"""Real-CLI tests for the synapse-mem provider (no mocks), loading the plugin from this repo.

Run with the Hermes venv python. HERMES_AGENT_DIR points at the hermes-agent checkout (default ~/.hermes/hermes-agent).
"""

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
check("7 tool schemas", names == {"memory_remember", "memory_recall", "memory_update", "decision_record",
                                  "decision_outcome", "decision_why", "memory_review"}, str(sorted(names)))

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

failed = [n for n, ok in results if not ok]
print(f"\n{len(results) - len(failed)}/{len(results)} passed" + (f"; FAILED: {failed}" if failed else ""))
sys.exit(1 if failed else 0)
