"""scripts/assistant_eval.py against a stub of rsync.ai's API: no key, no model.

The eval asks Ask rsync.ai real questions with a person's session token and
grades each answer by the tools it called (docs/internal/assistant-token-design.md
§ 8.2). These tests pin what it sends (the session headers, to this host only),
what it never writes (the token, an answer's text, a tool's arguments) and how it
grades: a write-tool call fails a question, a draft must store something, admin
drafts asked as a member are reported apart, and the bar compares with a baseline.
"""

from __future__ import annotations

import importlib.util
import json
import os
import threading
import urllib.parse
from collections import Counter
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPT = os.path.join(REPO_ROOT, "scripts", "assistant_eval.py")
QUESTIONS_FILE = os.path.join(REPO_ROOT, "scripts", "assistant_eval_questions.json")


def _load():
    spec = importlib.util.spec_from_file_location("assistant_eval", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ev = _load()
assert os.path.samefile(ev.DEFAULT_QUESTIONS, QUESTIONS_FILE)
QUESTIONS, WRITE_TOOLS = ev.load_questions(QUESTIONS_FILE)
BY_ID = {q["id"]: q for q in QUESTIONS}

MEMBER_TOKEN = "unit-test-member-session-token-0001"
ADMIN_TOKEN = "unit-test-admin-session-token-0002"
MEMBER_WS = "11111111-1111-1111-1111-111111111111"
ADMIN_WS = "22222222-2222-2222-2222-222222222222"
ANSWER_MARK = "ROW-VALUE-FROM-A-SAMPLE"
ARGUMENT_MARK = "ARGUMENT-VALUE"

LISTS = {
    "/api/v1/pipelines": {"pipelines": [
        {"name": "orders sync", "derived_status": "failed", "sync_mode": "batch"},
        {"name": "accounts", "derived_status": "completed", "sync_mode": "batch"},
    ]},
    "/api/v1/connections": {"connections": [
        {"name": "pg main", "type": "source", "connector_type": "postgresql"},
        {"name": "bq", "type": "destination", "connector_type": "bigquery"},
    ]},
    "/api/v1/dashboards": {"dashboards": [{"name": "Ops", "tile_count": 3}, {"name": "Empty", "tile_count": 0}]},
    "/api/v1/workflows": {"workflows": [{"name": "nightly check"}]},
    "/api/v1/delivery-channels": {"channels": [{"name": "ops email"}]},
}
SLOTS = {
    "pipeline": "accounts", "failed_pipeline": "orders sync", "connection": "bq",
    "source_connection": "pg main", "destination_connection": "bq", "sql_connection": "bq",
    "dashboard_with_tiles": "Ops", "workflow": "nightly check", "channel": "ops email",
}
QUESTION_ID = {ev.fill(q["question"], SLOTS)[0]: q["id"] for q in QUESTIONS}


def perfect_calls(qid: str) -> list[tuple[str, str, int]]:
    """The calls a model that does exactly what the question asks makes."""
    q = BY_ID[qid]
    calls = [((item[0] if isinstance(item, list) else item), "ok", 200) for item in q["must_call"]]
    if q.get("draft") and q["draft"] not in [c[0] for c in calls]:
        calls.append((q["draft"], "ok", 200))
    return calls


def answer(calls, draft: str = "", resource_id: str = "draft-1", entry: str = "chatgpt") -> tuple[int, dict]:
    """A 200 from /assistant/messages: two turns, the first making the calls.

    Each turn's characters add up to its input tokens, so the split is exact."""
    first = {"input_tokens": 1000, "cached_tokens": 0, "output_tokens": 20, "reasoning_tokens": 0,
             "chars": {"system": 600, "tools": 200, "user": 100, "assistant": 0, "tool": 100},
             "calls": [{"tool": t, "outcome": o, "status": s} for t, o, s in calls]}
    second = {"input_tokens": 1200, "cached_tokens": 800, "output_tokens": 40, "reasoning_tokens": 4,
              "chars": {"system": 600, "tools": 200, "user": 100, "assistant": 50, "tool": 250},
              "guide_chars": 150, "calls": []}
    looked_at = [{"tool": t, "arguments": {"filter": ARGUMENT_MARK}, "ok": o == "ok",
                  "resource_id": resource_id if t == draft else "", "sample": False, "sql": ""}
                 for t, o, _ in calls if o not in ("unknown_tool", "limit", "dropped")]
    return 200, {
        "conversation_id": "c-1",
        "answer": f"Here it is: {ANSWER_MARK}",
        "model": {"entry": entry, "label": entry, "model": f"{entry}-model"},
        "looked_at": looked_at,
        "stopped": "",
        "cost_cents": 1,
        "usage": {"input_tokens": 2200, "cached_tokens": 800, "output_tokens": 60, "reasoning_tokens": 4,
                  "steps": [first, second]},
    }


def perfect(body: dict) -> tuple[int, dict]:
    qid = QUESTION_ID[body["question"]]
    return answer(perfect_calls(qid), draft=BY_ID[qid].get("draft", ""), entry=body["model"])


class Stub:
    def __init__(self):
        self.requests: list[dict] = []
        self.lists = json.loads(json.dumps(LISTS))
        self.entries = ["chatgpt"]
        self.answer = perfect
        self.redirect_to = ""

    def handle(self, method: str, path: str, headers, body: dict | None) -> tuple[int, object, dict]:
        url = urllib.parse.urlsplit(path)
        # urllib capitalizes header names (X-workspace-id); the gateway reads them case-blind.
        self.requests.append({"method": method, "path": url.path,
                              "headers": {k.lower(): v for k, v in headers.items()}, "body": body})
        if url.path == "/api/v1/assistant/models":
            if self.redirect_to:
                return 302, None, {"Location": self.redirect_to + url.path}
            return 200, {"entries": [{"entry": e, "label": e, "model": f"{e}-model"} for e in self.entries],
                         "default": self.entries[0] if self.entries else ""}, {}
        if method == "GET" and url.path in self.lists:
            return 200, self.lists[url.path], {}
        if method == "POST" and url.path == "/api/v1/assistant/messages":
            status, out = self.answer(body)
            return status, out, {}
        return 404, {"error": "not_found"}, {}

    def posts(self) -> list[dict]:
        return [r for r in self.requests if r["method"] == "POST"]


def _serve(stub: Stub):
    class Handler(BaseHTTPRequestHandler):
        def _reply(self, method):
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n)) if n else None
            status, out, extra = stub.handle(method, self.path, self.headers, body)
            raw = json.dumps(out).encode() if out is not None else b""
            self.send_response(status)
            for k, v in extra.items():
                self.send_header(k, v)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            self._reply("GET")

        def do_POST(self):
            self._reply("POST")

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


@pytest.fixture
def stub():
    s = Stub()
    server, s.url = _serve(s)
    yield s
    server.shutdown()
    server.server_close()


@pytest.fixture
def env(monkeypatch, stub):
    for name in ("RSYNC_EVAL_ADMIN_TOKEN", "RSYNC_EVAL_ADMIN_WORKSPACE_ID"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("RSYNC_EVAL_BASE_URL", stub.url)
    monkeypatch.setenv("RSYNC_EVAL_TOKEN", MEMBER_TOKEN)
    monkeypatch.setenv("RSYNC_EVAL_WORKSPACE_ID", MEMBER_WS)

    def with_admin():
        monkeypatch.setenv("RSYNC_EVAL_ADMIN_TOKEN", ADMIN_TOKEN)
        monkeypatch.setenv("RSYNC_EVAL_ADMIN_WORKSPACE_ID", ADMIN_WS)

    return with_admin


def run(tmp_path, capsys, *args, out="report"):
    code = ev.main(["--out", str(tmp_path / out), *args])
    printed = capsys.readouterr()
    report_dir = tmp_path / out
    report = json.loads((report_dir / "report.json").read_text()) if (report_dir / "report.json").exists() else None
    return code, report, printed


def written(tmp_path) -> str:
    return "".join(p.read_text() for p in tmp_path.rglob("report.*"))


# ---- what it sends and what it never writes -----------------------------------------


def test_every_question_carries_the_session_headers_and_no_report_holds_the_token(stub, env, tmp_path, capsys):
    env()
    code, report, printed = run(tmp_path, capsys, "--runs", "1")
    assert code == 0, printed.err

    assert len(stub.posts()) == 20
    for r in stub.requests:
        h = r["headers"]
        admin = r["method"] == "POST" and BY_ID[QUESTION_ID[r["body"]["question"]]]["role"] == "admin"
        token, ws = (ADMIN_TOKEN, ADMIN_WS) if admin else (MEMBER_TOKEN, MEMBER_WS)
        if r["method"] == "GET" and h["authorization"] == f"Bearer {ADMIN_TOKEN}":
            token, ws = ADMIN_TOKEN, ADMIN_WS  # the admin workspace's slot fills
        assert h["authorization"] == f"Bearer {token}", r["path"]
        assert h["x-workspace-id"] == ws, r["path"]
        assert h["cookie"] == f"csrf_token={h['x-csrf-token']}" and len(h["x-csrf-token"]) >= 20
    roles = Counter(BY_ID[QUESTION_ID[r["body"]["question"]]]["role"] for r in stub.posts()
                    if r["headers"]["authorization"] == f"Bearer {ADMIN_TOKEN}")
    assert roles == {"admin": 8}
    assert all(r["body"]["request_id"] and "conversation_id" not in r["body"] for r in stub.posts())

    everything = printed.out + printed.err + written(tmp_path)
    for secret in (MEMBER_TOKEN, ADMIN_TOKEN, ANSWER_MARK, ARGUMENT_MARK):
        assert secret not in everything, secret
    s = report["entries"]["chatgpt"]["summary"]
    assert (s["runs"], s["runs_passed"], s["drafts"], s["drafts_passed"]) == (20, 20, 10, 10)
    assert report["slots"]["member"] == SLOTS


def test_the_report_splits_input_into_the_fixed_part_guides_conversation_and_tool_results(stub, env, tmp_path, capsys):
    env()
    _, report, printed = run(tmp_path, capsys, "--runs", "1", "--only", "q10,q15")
    s = report["entries"]["chatgpt"]["summary"]
    # Turn 1: 1000 tokens over 800 fixed, 100 conversation, 100 tool chars;
    # turn 2: 1200 over 800 fixed, 150 conversation and 250 tool chars, 150 of
    # them a guide.
    assert s["input_split_tokens_per_question"] == {"fixed": 1600, "guides": 150, "conversation": 250,
                                                    "tool_results": 200, "unattributed": 0}
    assert s["input_tokens_per_question"] == 2200 and s["cached_share"] == round(800 / 2200, 3)
    assert "| Fixed | Guides | Conversation | Tool results |" in printed.out
    assert "1,600 (73%) | 150 (7%) |" in printed.out


def test_a_turn_that_sent_no_chars_is_unattributed():
    split = {k: 0.0 for k in ev.SPLIT_PARTS}
    ev._split_step({"input_tokens": 500}, split)
    ev._split_step({"input_tokens": 100, "chars": {"system": 1, "tools": 1, "user": 1, "assistant": 0, "tool": 1}}, split)
    assert split == {"fixed": 50.0, "guides": 0.0, "conversation": 25.0, "tool_results": 25.0, "unattributed": 500.0}


def test_guide_chars_come_out_of_the_tool_results_and_never_past_them():
    split = {k: 0.0 for k in ev.SPLIT_PARTS}
    ev._split_step({"input_tokens": 100, "guide_chars": 30,
                    "chars": {"system": 20, "tools": 20, "user": 10, "assistant": 0, "tool": 50}}, split)
    assert split == {"fixed": 40.0, "guides": 30.0, "conversation": 10.0, "tool_results": 20.0, "unattributed": 0.0}
    split = {k: 0.0 for k in ev.SPLIT_PARTS}
    ev._split_step({"input_tokens": 10, "guide_chars": 99, "chars": {"system": 5, "tool": 5}}, split)
    assert split == {"fixed": 5.0, "guides": 5.0, "conversation": 0.0, "tool_results": 0.0, "unattributed": 0.0}


def test_a_redirect_is_not_followed_so_the_token_stays_on_the_host(stub, env, tmp_path, capsys):
    other = Stub()
    server, other.url = _serve(other)
    try:
        stub.redirect_to = other.url
        code, report, printed = run(tmp_path, capsys)
    finally:
        server.shutdown()
        server.server_close()
    assert code == 2 and report is None
    assert other.requests == []
    assert "302" in printed.err


@pytest.mark.parametrize("url,ok", [
    ("https://app.rsync.ai", True),
    ("https://app.rsync.ai/some/path", True),
    ("http://localhost:8080", True),
    ("http://127.0.0.1:8080", True),
    ("http://[::1]:8080", True),
    ("http://app.rsync.ai", False),
    ("http://localhost.example.com", False),
    ("ftp://localhost", False),
    ("app.rsync.ai", False),
])
def test_the_token_only_goes_over_https_or_to_this_machine(url, ok):
    if ok:
        assert ev.check_base_url(url).startswith(url.split("/")[0])
    else:
        with pytest.raises(ev.EvalError):
            ev.check_base_url(url)


def test_an_http_host_is_refused_before_anything_is_sent(stub, env, monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("RSYNC_EVAL_BASE_URL", "http://app.rsync.ai")
    code, _, printed = run(tmp_path, capsys)
    assert code == 2 and "must be https" in printed.err
    assert stub.requests == []


def test_an_error_that_echoes_the_token_prints_it_redacted(stub, env, tmp_path, capsys):
    orig = stub.handle

    def echo(method, path, headers, body):
        if path.startswith("/api/v1/pipelines"):
            return 500, {"error": headers["Authorization"]}, {}
        return orig(method, path, headers, body)

    stub.handle = echo
    code, _, printed = run(tmp_path, capsys)
    assert code == 2
    assert MEMBER_TOKEN not in printed.err and "[token]" in printed.err


# ---- grading ------------------------------------------------------------------------


def grade(qid, calls, draft_id="draft-1"):
    q = BY_ID[qid]
    _, body = answer(calls, draft=q.get("draft", ""), resource_id=draft_id)
    return ev.grade(q, 200, body, False, WRITE_TOOLS)


def test_a_write_tool_call_fails_the_question_whatever_it_returned():
    r = grade("q10", perfect_calls("q10") + [("create_model", "forbidden", 403)])
    assert not r["passed"]
    assert "called a write tool: create_model" in r["reasons"]
    assert r["counts"]["write"] == 1 and r["counts"]["forbidden"] == 1


def test_tools_that_do_not_exist_and_refused_arguments_are_counted():
    r = grade("q10", perfect_calls("q10") + [
        ("drop_database", "unknown_tool", 0),
        ("get_pipeline", "invalid_arguments", 400),
        ("plan_workflow", "error", 422),
        ("get_pipeline", "pipeline_not_found", 404),
        ("list_runs", "unavailable", 503),
        ("list_runs", "limit", 0),
    ])
    c = r["counts"]
    assert (c["unknown_tool"], c["refused_arguments"], c["not_found"], c["error"], c["limit"]) == (1, 2, 1, 1, 1)
    assert not r["passed"] and "called a tool that does not exist" in r["reasons"]


def test_a_must_call_item_needs_an_ok_call_and_a_list_item_takes_any_of_its_tools():
    assert grade("q09", [("list_pipelines", "ok", 200), ("get_pipeline_health", "ok", 200)])["passed"]
    r = grade("q09", [("list_pipelines", "ok", 200), ("diagnose_run", "error", 404)])
    assert r["reasons"] == ["no ok call to diagnose_run or get_run_status or get_pipeline_health"]
    r = grade("q09", perfect_calls("q09") + [("plan_pipeline", "ok", 200)])
    assert r["reasons"] == ["called plan_pipeline"]


def test_a_draft_passes_only_when_its_call_stored_something():
    assert grade("q01", perfect_calls("q01"))["passed"]
    r = grade("q01", perfect_calls("q01"), draft_id="")
    assert r["reasons"] == ["plan_workflow stored no draft"] and r["draft_ok"] is False
    r = grade("q01", perfect_calls("q01")[:-1] + [("plan_workflow", "error", 422)])
    assert "no ok call to plan_workflow" in r["reasons"] and "plan_workflow stored no draft" in r["reasons"]


def test_an_answer_that_is_not_200_fails_with_its_status():
    r = ev.grade(BY_ID["q10"], 504, {"error": "timeout"}, False, WRITE_TOOLS)
    assert r["reasons"] == ["HTTP 504 timeout"] and not r["passed"]


# ---- the run ------------------------------------------------------------------------


def test_admin_drafts_without_an_admin_token_are_reported_apart(stub, env, tmp_path, capsys):
    def member_refused(body):
        qid = QUESTION_ID[body["question"]]
        q = BY_ID[qid]
        if q["role"] == "admin":
            return answer([c if c[0] != q["draft"] else (c[0], "forbidden", 403) for c in perfect_calls(qid)])
        return perfect(body)

    stub.answer = member_refused
    code, report, printed = run(tmp_path, capsys, "--runs", "1")
    assert code == 0, printed.err
    assert all(r["headers"]["authorization"] == f"Bearer {MEMBER_TOKEN}" for r in stub.requests)
    s = report["entries"]["chatgpt"]["summary"]
    assert sorted(s["asked_as_member"]) == sorted(q["id"] for q in QUESTIONS if q["role"] == "admin")
    assert s["drafts_refused_member"] == 8
    assert (s["runs"], s["runs_passed"]) == (12, 12)
    assert "admin" not in report["slots"]


def test_a_question_with_nothing_to_fill_is_skipped_and_never_asked(stub, env, tmp_path, capsys):
    stub.lists["/api/v1/pipelines"]["pipelines"][0]["derived_status"] = "completed"
    code, report, _ = run(tmp_path, capsys, "--runs", "1", "--only", "q09,q10")
    assert code == 0
    assert [QUESTION_ID.get(r["body"]["question"]) for r in stub.posts()] == ["q10"]
    s = report["entries"]["chatgpt"]["summary"]
    assert s["skipped"] == ["q09"] and s["runs"] == 1


def test_the_bar_fails_a_question_that_passed_in_the_baseline(stub, env, tmp_path, capsys):
    env()
    code, _, _ = run(tmp_path, capsys, "--runs", "2", out="baseline")
    assert code == 0

    def worse(body):
        qid = QUESTION_ID[body["question"]]
        if qid == "q10":
            return answer([("list_pipelines", "ok", 200), ("list_runs", "invalid_arguments", 400)])
        return perfect(body)

    stub.answer = worse
    code, report, printed = run(tmp_path, capsys, "--runs", "2", "--baseline",
                                str(tmp_path / "baseline" / "report.json"), out="after")
    assert code == 1
    bar = report["bar"]["chatgpt"]
    assert bar["met"] is False
    assert bar["regressions"] == {"ok": False, "questions": ["q10"]}
    assert bar["refused_arguments"]["ok"] is False
    assert "NOT MET" in printed.out and "note" not in bar


def test_without_a_baseline_only_an_unknown_tool_fails_the_bar(stub, env, tmp_path, capsys):
    code, report, _ = run(tmp_path, capsys, "--runs", "1", "--only", "q10")
    assert code == 0 and report["bar"]["chatgpt"]["met"] is None

    stub.answer = lambda body: answer(perfect_calls("q10") + [("drop_database", "unknown_tool", 0)])
    code, report, _ = run(tmp_path, capsys, "--runs", "1", "--only", "q10", out="again")
    assert code == 1 and report["bar"]["chatgpt"]["met"] is False


def test_a_spent_quota_stops_the_run_and_keeps_what_it_asked(stub, env, tmp_path, capsys):
    asked = []

    def quota(body):
        asked.append(body)
        return (402, {"error": "quota_exceeded"}) if len(asked) > 3 else perfect(body)

    env()
    stub.answer = quota
    code, report, _ = run(tmp_path, capsys, "--runs", "1")
    assert code == 1 and report["stopped"].startswith("quota_exceeded")
    assert len(asked) == 4 and report["entries"]["chatgpt"]["summary"]["runs"] == 3


def test_an_entry_that_loses_its_key_stops_only_that_entry(stub, env, tmp_path, capsys):
    stub.entries = ["chatgpt", "gemini"]
    stub.answer = lambda body: ((409, {"error": "model_not_configured"}) if body["model"] == "gemini"
                                else perfect(body))
    code, report, _ = run(tmp_path, capsys, "--runs", "1", "--only", "q10,q15")
    assert code == 1
    assert report["entries"]["chatgpt"]["summary"]["runs_passed"] == 2
    assert report["entries"]["gemini"]["error"].startswith("model_not_configured")
    assert sum(r["body"]["model"] == "gemini" for r in stub.posts()) == 1
    assert report["bar"]["gemini"]["met"] is False


def test_an_entry_with_no_key_is_refused_before_any_question(stub, env, tmp_path, capsys):
    code, _, printed = run(tmp_path, capsys, "--entries", "claude")
    assert code == 2 and "no key for claude" in printed.err and stub.posts() == []


def test_a_dry_run_fills_the_questions_and_asks_nothing(stub, env, tmp_path, capsys):
    code = ev.main(["--dry-run"])
    out = capsys.readouterr().out
    assert code == 0 and stub.posts() == []
    assert len(out.strip().splitlines()) == 20 and "on bq:" in out


# ---- the questions ------------------------------------------------------------------


def test_the_questions_are_the_mix_the_design_names():
    assert len(QUESTIONS) == 20
    assert Counter(q["topic"] for q in QUESTIONS) == {"workflows": 4, "dashboards": 4, "pipelines": 3, "models": 3,
                                                      "connectors": 2, "sql": 2, "general": 2}
    assert Counter(q["role"] for q in QUESTIONS) == {"member": 12, "admin": 8}
    # Workflow and dashboard drafts need a workspace admin; nothing else does.
    assert {q["id"] for q in QUESTIONS if q["role"] == "admin"} == {
        q["id"] for q in QUESTIONS if q["topic"] in ("workflows", "dashboards")}


def test_a_question_that_names_a_write_tool_is_refused(tmp_path):
    doc = json.loads(Path(QUESTIONS_FILE).read_text())
    doc["questions"][0]["must_call"].append("run_model")
    path = tmp_path / "q.json"
    path.write_text(json.dumps(doc))
    with pytest.raises(ev.EvalError, match="names the write tool run_model"):
        ev.load_questions(str(path))
