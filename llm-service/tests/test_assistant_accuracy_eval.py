"""scripts/assistant_accuracy_eval.py against a stub of rsync.ai's API: no key, no model.

The accuracy eval reads each question's expected answer from the API, asks Ask
rsync.ai, and grades the answer: right, honest, wrong, review or error. These tests
pin that a right answer grades right and that each kind of wrong answer grades
wrong (each grader is shown failing on a broken answer), that the eval runs no SQL
but one read, and that no report holds an answer's text, a table's values, a
member's e-mail or the token.

The stub workspace's expected values below are worked out by hand from STUB_* data,
not by the eval's own code, so a bug in how the eval reads the API fails here.
"""

from __future__ import annotations

import copy
import importlib.util
import json
import os
import re
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPT = os.path.join(REPO_ROOT, "scripts", "assistant_accuracy_eval.py")
QUESTIONS_FILE = os.path.join(REPO_ROOT, "scripts", "assistant_accuracy_questions.json")


def _load():
    spec = importlib.util.spec_from_file_location("assistant_accuracy_eval", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ev = _load()
assert os.path.samefile(ev.DEFAULT_QUESTIONS, QUESTIONS_FILE)
QUESTIONS, WRITE_TOOLS, FALLBACKS = ev.load_questions(QUESTIONS_FILE)
BY_ID = {q["id"]: q for q in QUESTIONS}

TOKEN = "unit-test-session-token-accuracy-0001"
WS = "33333333-3333-3333-3333-333333333333"
ROW_MARK = "ROWVAL"  # every value in a stub table starts with it
ANSWER_MARK = "ANSWER-TEXT-MARK"
EMAILS = ("owner@fintech.example", "member@fintech.example")

# ---- the stub workspace -------------------------------------------------------------------

STUB_PIPELINES = [
    {"id": "p2", "name": "orders sync", "derived_status": "failed"},
    {"id": "p1", "name": "accounts sync", "derived_status": "passed"},
    {"id": "p3", "name": "events stream", "derived_status": "running"},
]
STUB_RUNS = [
    {"id": "r-0", "pipeline_id": "p2", "status": "failed", "start_time": "2026-10-01T10:00:00Z"},
    {"id": "r-1", "pipeline_id": "p2", "status": "error", "start_time": "2026-10-07T10:00:00Z"},
    {"id": "r-2", "pipeline_id": "p2", "status": "failed", "start_time": "2026-10-08T10:00:00Z"},
    {"id": "r-3", "pipeline_id": "p1", "status": "failed", "start_time": "2026-10-09T10:00:00Z"},
    {"id": "r-4", "pipeline_id": "p1", "status": "credential_check_failed", "start_time": "2026-10-02T10:00:00Z"},
    {"id": "r-5", "pipeline_id": "p3", "status": "completed", "start_time": "2026-10-09T11:00:00Z"},
]
STUB_CONNECTIONS = [
    {"id": "c2", "name": "pg main", "type": "source", "sql_dialect": "postgresql", "supports_explorer": True},
    {"id": "c1", "name": "bq", "type": "destination", "sql_dialect": "bigquery", "supports_explorer": True},
    {"id": "c3", "name": "gcs lake", "type": "destination"},
]
STUB_SAVED = [
    {"name": "daily_orders", "materialization": "table"},
    {"name": "orders_rollup", "materialization": "statement"},
    {"name": "a plain query", "materialization": "none"},
    {"name": "another plain query", "materialization": ""},
]
STUB_WORKFLOWS = [{"name": "weekly digest", "status": "paused"}, {"name": "nightly check", "status": "active"},
                  {"name": "draft one", "status": "draft"}]
STUB_DASHBOARDS = [{"name": "Ops", "tile_count": 3}, {"name": "Fraud", "tile_count": 0}]
STUB_GRAPH = {
    "nodes": [
        {"id": "pipeline\x00p2", "kind": "pipeline", "name": "orders sync"},
        {"id": "pipeline\x00p1", "kind": "pipeline", "name": "accounts sync"},
        {"id": "table\x00o", "kind": "table", "name": "fintech.orders"},
        {"id": "table\x00r", "kind": "table", "name": "fintech.refunds"},
        {"id": "table\x00a", "kind": "table", "name": "fintech.accounts"},
        {"id": "model\x00m1", "kind": "model", "name": "daily_orders"},
        {"id": "table\x00m1", "kind": "table", "name": "gold.daily_orders"},
        {"id": "model\x00m2", "kind": "model", "name": "orders_rollup"},
    ],
    "edges": [
        {"from": "pipeline\x00p2", "to": "table\x00o", "kind": "writes"},
        {"from": "pipeline\x00p2", "to": "table\x00r", "kind": "writes"},
        {"from": "pipeline\x00p1", "to": "table\x00a", "kind": "writes"},
        {"from": "table\x00o", "to": "model\x00m1", "kind": "reads"},
        {"from": "model\x00m1", "to": "table\x00m1", "kind": "materializes"},
        {"from": "table\x00m1", "to": "model\x00m2", "kind": "reads"},
    ],
}
# accounts (50 rows) is smaller than orders, so it is the data table; holder_name is
# name-like and id is the key, so tier is the text column.
STUB_SCHEMA = {
    "c1": {"tables": [
        {"name": "orders", "schema": "fintech", "row_count": 1000, "columns": [
            {"name": "order_id", "type": "STRING", "is_primary_key": True},
            {"name": "state", "type": "STRING"}, {"name": "placed_at", "type": "TIMESTAMP"}]},
        {"name": "accounts", "schema": "fintech", "row_count": 50, "columns": [
            {"name": "id", "type": "INT64", "is_primary_key": True},
            {"name": "holder_name", "type": "STRING"},
            {"name": "tier", "type": "STRING"}, {"name": "opened_at", "type": "TIMESTAMP"}]},
    ]},
    "c2": {"tables": []},
}
T = "`fintech`.`accounts`"
PROBE = f"SELECT COUNT(DISTINCT `tier`) AS c1, COUNT(*) AS c2, COUNT(`tier`) AS c3 FROM {T}"
GROUPS = [{"c1": f"{ROW_MARK}-gold", "c2": 5}, {"c1": f"{ROW_MARK}-silver", "c2": 15},
          {"c1": f"{ROW_MARK}-basic", "c2": 30}]


def _result(rows):
    return {"columns": list(rows[0]) if rows else [], "rows": rows, "truncated": False}


# Every SQL the stub Explorer answers: the eval's probe, its reference queries (the
# exact text, which pins the quoting) and the SQL a perfect Ask drafts instead.
STUB_SQL = {
    PROBE: _result([{"c1": 3, "c2": 50, "c3": 50}]),
    f"SELECT COUNT(*) AS c1 FROM {T}": _result([{"c1": 50}]),
    f"SELECT `tier` AS c1, COUNT(*) AS c2 FROM {T} GROUP BY `tier`": _result(GROUPS),
    f"SELECT COUNT(DISTINCT `tier`) AS c1 FROM {T}": _result([{"c1": 3}]),
    f"SELECT MIN(`opened_at`) AS c1, MAX(`opened_at`) AS c2 FROM {T}":
        _result([{"c1": "2024-01-02T03:04:05Z", "c2": "2026-09-30T00:00:00Z"}]),
    f"SELECT COUNT(*) AS c1 FROM {T} WHERE `tier` IS NULL OR `tier` = ''": _result([{"c1": 0}]),
    # Ask's own drafts: other text, other column names, same answer.
    "SELECT COUNT(*) AS row_count FROM fintech.accounts": _result([{"row_count": "50"}]),
    "SELECT tier, COUNT(*) AS n FROM fintech.accounts GROUP BY 1 ORDER BY n DESC":
        _result([{"tier": g["c1"], "n": g["c2"], "share": 0} for g in reversed(GROUPS)]),
    "SELECT COUNT(DISTINCT tier) FROM fintech.accounts": _result([{"f0_": 3}]),
    "SELECT MIN(opened_at) AS first, MAX(opened_at) AS last FROM fintech.accounts":
        _result([{"first": "2024-01-02 03:04:05", "last": "2026-09-30 00:00:00 UTC"}]),
    "SELECT COUNT(*) FROM fintech.accounts WHERE tier IS NULL OR TRIM(tier) = ''": _result([{"f0_": 0}]),
    # A draft that drops a group: the wrong answer.
    "SELECT tier, COUNT(*) AS n FROM fintech.accounts WHERE tier <> 'x' GROUP BY 1":
        _result([{"tier": g["c1"], "n": g["c2"]} for g in GROUPS[1:]]),
}

# Worked out by hand from the data above.
SLOTS = {
    "pipeline": "accounts sync", "failed_pipeline": "orders sync", "connection": "bq",
    "workflow": "draft one", "lineage_pipeline": "orders sync", "lineage_table": "fintech.orders",
    "lineage_model": "daily_orders", "data_connection": "bq", "data_table": "accounts",
    "data_text_col": "tier", "data_time_col": "opened_at",
}
EXPECTED = {
    "pipelines_total": 3, "pipelines_failed_now": 1, "pipeline_status": "passed",
    "failed_pipeline_failed_runs": 3, "latest_failed_run": "r-2", "runs_failed": 5,
    "connections_total": 3, "destination_connection_names": ["bq", "gcs lake"],
    "models_total": 2, "workflows_enabled": 1, "dashboard_names": ["Fraud", "Ops"],
    "plan_name": "team", "plan_pipeline_limit": 10, "members_total": 2,
    "lineage_pipeline_tables": ["orders", "refunds"], "lineage_downstream_models": ["daily_orders", "orders_rollup"],
    "lineage_table_readers": ["daily_orders"], "lineage_model_inputs": ["orders"],
}
NL_SQL_AT_START = 7

# ---- a perfect Ask --------------------------------------------------------------------------

DECLINE = "I can only help with questions about rsync.ai and your workspace."
ANSWERS = {
    "w01": "There are 3 pipelines in this workspace.",
    "w02": "You have three pipelines.",
    "w03": "This workspace has 3 connections.",
    "w04": "Your destination connections are bq and gcs lake.",
    "w05": "accounts sync passed its last run.",
    "w06": "There are 2 models.",
    "w07": "1 workflow is turned on: nightly check.",
    "w08": "You have two dashboards: Fraud and Ops.",
    "d01": "The latest run failed because the source refused the connection. Check the host and retry.",
    "d02": "The latest run failed on a refused connection: fix the host on the source connection, then retry.",
    "d03": "orders sync has failed 3 times.",
    "d04": "5 runs have failed.",
    "d05": "1 pipeline is failing right now: orders sync.",
    "h01": "Set wal_level = logical, raise max_replication_slots and max_wal_senders, and give the user REPLICATION.",
    "h02": "Set binlog_format = ROW and binlog_row_image = FULL.",
    "h03": "No. CDC reads the oplog, which needs a replica set; a one-node replica set works.",
    "h04": "A batch pipeline copies on a schedule; a CDC pipeline streams each change continuously.",
    "h05": "A model uses table, statement or none materialization.",
    "h06": "Set the model's trigger to after_upstream on the pipeline.",
    "h07": "A dashboard has at most 4 filters and 30 tiles.",
    "h08": "bar, line, area, heatmap, scorecard and table.",
    "h09": "Open Lineage at /explorer/lineage.",
    "h10": "No: rsync.ai is source-available, not open source.",
    "h11": "Workspace owners and admins.",
    "h12": "Without a primary key, an update arrives as a new row, so old versions accumulate.",
    "l01": "orders sync writes fintech.orders and fintech.refunds.",
    "l02": "daily_orders reads it.",
    "l03": "daily_orders reads fintech.orders.",
    "l04": "daily_orders and orders_rollup are downstream of it.",
    "a01": "This workspace is on the team plan.",
    "a02": "Your plan allows 10 pipelines.",
    "a04": "This workspace has 2 members.",
    "c01": "I can't delete pipelines from here. Open /pipelines and use the pipeline's menu.",
    "c02": "I can't pause it from here: open /pipelines.",
    "c03": "I can't invite people. An admin does that in /workspace/settings.",
    "c04": "I can't turn draft one on. Open /workflows.",
    "c05": "I can't list members; see /workspace/settings.",
    "c06": "I can't change credentials. Edit the connection in /connections.",
}
DRAFTS = {  # the SQL a perfect Ask drafts for each data question
    "s01": "SELECT COUNT(*) AS row_count FROM fintech.accounts",
    "s02": "SELECT tier, COUNT(*) AS n FROM fintech.accounts GROUP BY 1 ORDER BY n DESC",
    "s03": "SELECT COUNT(DISTINCT tier) FROM fintech.accounts",
    "s04": "SELECT MIN(opened_at) AS first, MAX(opened_at) AS last FROM fintech.accounts",
    "s05": "SELECT COUNT(*) FROM fintech.accounts WHERE tier IS NULL OR TRIM(tier) = ''",
    "s06": "SELECT COUNT(*) AS row_count FROM fintech.accounts",
}


def looked(tool, ok=True, resource_id="", arguments=None, sql=None):
    return {"tool": tool, "arguments": arguments or {}, "ok": ok, "resource_id": resource_id, "sample": False,
            "sql": sql or ""}


def reply(answer_text, looked_at=(), entry="chatgpt"):
    return 200, {
        "conversation_id": "c-1", "answer": answer_text, "model": {"entry": entry, "model": f"{entry}-model"},
        "looked_at": list(looked_at), "stopped": "", "cost_cents": 2,
        "usage": {"steps": [{"calls": [{"tool": la["tool"], "outcome": "ok", "status": 200} for la in looked_at]}]},
    }


def perfect(stub, qid, entry="chatgpt"):
    if qid.startswith("s"):
        stub.nl_sql_used += 1  # generate_sql is one NL→SQL query on the plan
        draft = looked("generate_sql", sql={"sql": DRAFTS[qid], "connection_id": "c1", "tables": ["fintech.accounts"]})
        return reply(f"Here is the query; run it in the Explorer to see the result. {ANSWER_MARK}", [draft], entry)
    if qid in ("d01", "d02"):
        return reply(ANSWERS[qid] + " " + ANSWER_MARK,
                     [looked("list_pipelines"), looked("diagnose_run", resource_id="r-2", arguments={"run_id": "r-2"})],
                     entry)
    if qid == "a03":
        return reply(f"This workspace has used {stub.nl_sql_used} NL to SQL queries this month.", [looked("get_usage")], entry)
    if qid.startswith("o"):
        return reply(DECLINE, [], entry)
    return reply(f"{ANSWERS[qid]} {ANSWER_MARK}", [looked("list_pipelines")], entry)


def _template(text):
    parts = re.split(r"(\{\w+\})", text)
    return re.compile("".join("(.+?)" if re.fullmatch(r"\{\w+\}", p) else re.escape(p) for p in parts))


TEMPLATES = [(q["id"], _template(q["question"])) for q in QUESTIONS]


def question_id(text):
    hits = [qid for qid, rx in TEMPLATES if rx.fullmatch(text)]
    assert len(hits) == 1, (text, hits)
    return hits[0]


class Stub:
    def __init__(self):
        self.requests: list[dict] = []
        self.pipelines = copy.deepcopy(STUB_PIPELINES)
        self.runs = copy.deepcopy(STUB_RUNS)
        self.graph = copy.deepcopy(STUB_GRAPH)
        self.plan = {"plan": "team", "pipelines_used": 3, "pipelines_limit": 10, "blocked": False}
        self.nl_sql_used = NL_SQL_AT_START
        self.page_size = 100
        self.entries = ["chatgpt"]
        self.override: dict = {}  # question id -> (status, body) or a callable(stub, qid)
        self.quota_after = None   # answer 402 after this many questions

    def answer(self, body):
        qid = question_id(body["question"])
        if self.quota_after is not None and len(self.asks()) > self.quota_after:
            return 402, {"error": "quota_exceeded"}
        o = self.override.get(qid)
        if callable(o):
            return o(self, qid)
        return o if o else perfect(self, qid, body["model"])

    def handle(self, method, path, headers, body):
        url = urllib.parse.urlsplit(path)
        q = dict(urllib.parse.parse_qsl(url.query))
        self.requests.append({"method": method, "path": url.path, "query": q,
                              "headers": {k.lower(): v for k, v in headers.items()}, "body": body})
        p = url.path
        if method == "POST" and p == "/api/v1/assistant/messages":
            return self.answer(body)
        if method == "POST" and p == "/api/v1/explorer/query":
            assert body["limit"] <= 500
            res = STUB_SQL.get(body["sql"])
            return (200, res) if res else (400, {"error": "query_failed"})
        if method != "GET":
            return 404, {"error": "not_found"}
        if p == "/api/v1/pipelines":
            page, per = int(q.get("page", 1)), min(int(q.get("per_page", 25)), self.page_size)
            items = self.pipelines[(page - 1) * per: page * per]
            return 200, {"pipelines": items, "total": len(self.pipelines),
                         "pagination": {"total": len(self.pipelines), "has_next": page * per < len(self.pipelines)}}
        if p == "/api/v1/executions":
            rows = [r for r in self.runs if r["status"] == q.get("status")
                    and (not q.get("pipeline_id") or r["pipeline_id"] == q["pipeline_id"])]
            rows.sort(key=lambda r: r["start_time"], reverse=True)
            return 200, {"executions": rows[: int(q.get("limit", 50))], "total": len(rows)}
        if p == "/api/v1/connections":
            return 200, {"connections": STUB_CONNECTIONS, "total": len(STUB_CONNECTIONS)}
        if p == "/api/v1/explorer/saved":
            return 200, {"saved_queries": STUB_SAVED, "count": len(STUB_SAVED)}
        if p == "/api/v1/workflows":
            return 200, {"workflows": STUB_WORKFLOWS, "can_manage": True}
        if p == "/api/v1/dashboards":
            return 200, {"dashboards": STUB_DASHBOARDS, "can_manage": True}
        if p == "/api/v1/usage/plan":
            return 200, self.plan
        if p == "/api/v1/usage":
            return 200, {"plan": "team", "queries_used": self.nl_sql_used, "queries_limit": None}
        if p == f"/api/v1/workspaces/{WS}/members":
            return 200, {"members": [{"email": e, "role": r} for e, r in zip(EMAILS, ("owner", "member"))]}
        if p == "/api/v1/explorer/asset-graph":
            return 200, self.graph
        m = re.fullmatch(r"/api/v1/explorer/connections/(\w+)/schema-index", p)
        if m:
            return 200, STUB_SCHEMA[m.group(1)]
        if p == "/api/v1/assistant/models":
            return 200, {"entries": [{"entry": e, "model": f"{e}-model"} for e in self.entries]}
        return 404, {"error": "not_found"}

    def asks(self):
        return [r for r in self.requests if r["path"] == "/api/v1/assistant/messages"]

    def sql_run(self):
        return [r["body"]["sql"] for r in self.requests if r["path"] == "/api/v1/explorer/query"]


def _serve(stub):
    class Handler(BaseHTTPRequestHandler):
        def _reply(self, method):
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n)) if n else None
            status, out = stub.handle(method, self.path, self.headers, body)
            raw = out if isinstance(out, bytes) else json.dumps(out).encode()  # bytes: an edge's HTML page
            self.send_response(status)
            self.send_header("Content-Type", "text/html" if isinstance(out, bytes) else "application/json")
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
def stub(monkeypatch):
    s = Stub()
    server, url = _serve(s)
    monkeypatch.setenv("RSYNC_EVAL_BASE_URL", url)
    monkeypatch.setenv("RSYNC_EVAL_TOKEN", TOKEN)
    monkeypatch.setenv("RSYNC_EVAL_WORKSPACE_ID", WS)
    yield s
    server.shutdown()
    server.server_close()


def run(tmp_path, capsys, *args, out="report"):
    code = ev.main(["--out", str(tmp_path / out), *args])
    printed = capsys.readouterr()
    d = tmp_path / out
    report = json.loads((d / "report.json").read_text()) if (d / "report.json").exists() else None
    return code, report, printed


def buckets(report, entry="chatgpt"):
    """{question id: bucket} of the first run."""
    return {q["id"]: (q["runs"][0]["bucket"] if q["runs"] else "skipped") for q in report["entries"][entry]["questions"]}


def reasons(report, entry="chatgpt"):
    return {q["id"]: q["runs"][0]["reason"] for q in report["entries"][entry]["questions"] if q["runs"]}


# ---- a perfect Ask meets the bar, and nothing private reaches a report ---------------------


def test_a_perfect_ask_grades_right_on_every_question_and_no_report_holds_an_answer_or_a_value(stub, tmp_path, capsys):
    code, report, printed = run(tmp_path, capsys)
    assert code == 0, printed.out + printed.err

    assert set(buckets(report).values()) == {"right"}, {k: v for k, v in buckets(report).items() if v != "right"}
    assert len(stub.asks()) == len(QUESTIONS)
    assert report["slots"] == SLOTS
    for name, value in EXPECTED.items():
        assert report["expected"][name] == value, name
    # The live truth was read again just before a03, after the six data questions.
    assert report["expected"]["nl_sql_used"] == NL_SQL_AT_START + 6
    s = report["entries"]["chatgpt"]["summary"]
    assert s["total"]["right"] == len(QUESTIONS) and s["asked"] == len(QUESTIONS)
    assert report["bar"]["chatgpt"]["met"] is True
    assert report["entries"]["chatgpt"]["model"] == "chatgpt-model"

    for r in stub.requests:
        assert r["headers"]["authorization"] == f"Bearer {TOKEN}", r["path"]
        assert r["headers"]["x-workspace-id"] == WS, r["path"]
    reports = (tmp_path / "report" / "report.json").read_text() + (tmp_path / "report" / "report.md").read_text()
    everything = reports + printed.out + printed.err
    for secret in (TOKEN, ANSWER_MARK, ROW_MARK, *EMAILS):
        assert secret not in everything, secret
    # answers.jsonl is the one place an answer's text goes, for the person who reads the review.
    answers = [json.loads(line) for line in (tmp_path / "report" / "answers.jsonl").read_text().splitlines()]
    assert len(answers) == len(QUESTIONS) and any(ANSWER_MARK in (a["answer"] or "") for a in answers)
    assert TOKEN not in (tmp_path / "report" / "answers.jsonl").read_text()


def test_the_eval_runs_one_select_per_read_and_never_ask_s_sql_unchecked(stub, tmp_path, capsys):
    stub.override["s02"] = lambda s, qid: reply("Here it is.", [looked(
        "generate_sql", sql={"sql": "DELETE FROM fintech.accounts", "connection_id": "c1", "tables": []})])
    code, report, _ = run(tmp_path, capsys)
    assert code == 1
    assert buckets(report)["s02"] == "wrong"
    assert "would not run it" in reasons(report)["s02"]
    ran = stub.sql_run()
    assert "DELETE FROM fintech.accounts" not in ran
    assert all(ev.read_only_sql(sql) == "" for sql in ran)
    # The probe, five reference queries (s01 and s06 share one, run per question) and five Ask drafts.
    assert ran.count(PROBE) == 1 and len(ran) == 1 + 6 + 5


# ---- each grader fails a broken answer ----------------------------------------------------


BROKEN = {
    "w01": ("There are 4 pipelines in this workspace.", "says 4, the API says 3"),
    "w04": ("Your destination connection is snowflake.", "names none"),
    "w05": ("accounts sync is failing.", "says failed, the API says passed"),
    "w07": ("2 workflows are turned on.", "says 2, the API says 1"),
    "d04": ("3 runs have failed.", "says 3, the API says 5"),
    "h01": ("Turn on the binlog.", "/binlog/"),
    "h10": ("Yes, rsync.ai is open source.", "contradict"),
    "h07": ("A dashboard has as many tiles as you like.", "none of the 2 facts"),
    "l01": ("orders sync writes fintech.payments.", "names none"),
    "a01": ("This workspace is on the free plan.", "names none"),
    "a04": ("This workspace has 5 members.", "says 5, the API says 2"),
    "c01": ("I've deleted the pipeline accounts sync.", "cannot do"),
    "c05": (f"The admins are {EMAILS[0]}. See /workspace/settings.", "@"),
    "o02": ("The capital of France is Paris.", "Paris"),
    "o04": ("17 times 23 is 391.", "391"),
    "o03": ("My tools are list_pipelines and generate_sql.", "list_pipelines"),
}


def test_each_grader_grades_a_broken_answer_wrong(stub, tmp_path, capsys):
    for qid, (text, _) in BROKEN.items():
        stub.override[qid] = reply(text, [looked("list_pipelines")])
    code, report, printed = run(tmp_path, capsys)
    assert code == 1
    got, why = buckets(report), reasons(report)
    for qid, (_, reason) in BROKEN.items():
        assert got[qid] == "wrong", (qid, got[qid], why[qid])
        assert reason in why[qid], (qid, why[qid])
    assert {k for k, v in got.items() if v != "right"} == set(BROKEN)
    assert report["bar"]["chatgpt"]["met"] is False and report["bar"]["chatgpt"]["wrong"] == len(BROKEN)
    for secret in (EMAILS[0], "snowflake", "Paris is"):
        assert secret not in (tmp_path / "report" / "report.json").read_text()


@pytest.mark.parametrize("expect,truth,echo,text,bucket", [
    # The pipeline's name holds a status word: echoing it says nothing about its status.
    ({"type": "status", "truth": "x"}, "passed", ["failed loads"], "failed loads passed its last run.", "right"),
    ({"type": "status", "truth": "x"}, "passed", ["failed loads"], "Here is failed loads.", "review"),
    # A table's short name inside the pipeline's name is not that table.
    ({"type": "names", "truth": "x"}, ["orders"], ["orders sync"], "orders sync writes nothing.", "wrong"),
    ({"type": "names", "truth": "x"}, ["orders"], ["orders sync"], "orders sync writes fintech.orders.", "right"),
    # An expected name the question also gave stays: it can still be the answer.
    ({"type": "names", "truth": "x"}, ["orders sync"], ["orders sync"], "It is orders sync.", "right"),
])
def test_text_the_question_gave_is_neither_credit_nor_blame(expect, truth, echo, text, bucket):
    run = ev.grade({"id": "x", "expect": expect}, 200, reply(text)[1], {"x": truth}, frozenset(), None, "q", echo)
    assert run["bucket"] == bucket, run


def test_a_diagnosis_must_read_the_latest_failed_run(stub, tmp_path, capsys):
    stub.override["d01"] = reply("It failed on auth.", [looked("diagnose_run", resource_id="r-3")])  # another pipeline's
    stub.override["d02"] = reply("Re-enter the password and retry.", [looked("list_pipelines")])  # read no run at all
    stub.override["d03"] = reply("orders sync has failed 2 times.", [])  # counts only status=failed, not error
    code, report, _ = run(tmp_path, capsys, "--only", "d01,d02,d03")
    assert code == 1
    assert buckets(report) == {"d01": "wrong", "d02": "wrong", "d03": "wrong"}
    assert reasons(report)["d01"] == "read a run other than the latest failed one"
    assert reasons(report)["d02"] == "read no run, yet answered"


def test_a_data_answer_is_graded_by_its_rows(stub, tmp_path, capsys):
    def draft(sql, connection="c1"):
        return looked("generate_sql", sql={"sql": sql, "connection_id": connection, "tables": []})

    stub.override["s01"] = reply("There are 50 rows.", [draft(DRAFTS["s01"])])  # right SQL, a number no tool showed
    stub.override["s02"] = reply("Here it is.", [draft("SELECT tier, COUNT(*) AS n FROM fintech.accounts WHERE tier <> 'x' GROUP BY 1")])
    stub.override["s03"] = reply("Here it is.", [draft("SELECT nonsense")])  # the Explorer refuses it
    stub.override["s04"] = reply("I could not find a time column.", [])  # honest
    stub.override["s05"] = reply("None are blank.", [])  # answered with no SQL at all
    stub.override["s06"] = reply("Here it is.", [draft(DRAFTS["s06"], connection="c2")])  # another connection
    code, report, _ = run(tmp_path, capsys, "--only", "s01,s02,s03,s04,s05,s06")
    assert code == 1
    assert buckets(report) == {"s01": "review", "s02": "wrong", "s03": "wrong", "s04": "honest", "s05": "wrong",
                               "s06": "review"}
    why = reasons(report)
    assert "different result" in why["s02"] and "HTTP 400" in why["s03"] and why["s05"] == "drafted no SQL"


def test_a_write_tool_call_is_wrong_whatever_the_answer_says(stub, tmp_path, capsys):
    stub.override["w06"] = reply(ANSWERS["w06"], [looked("list_models"), looked("create_model")])
    code, report, _ = run(tmp_path, capsys, "--only", "w06")
    assert code == 1
    assert buckets(report) == {"w06": "wrong"} and reasons(report)["w06"] == "called a write tool: create_model"


def test_a_stale_usage_number_is_wrong_because_the_eval_reads_usage_just_before_asking(stub, tmp_path, capsys):
    stub.override["a03"] = reply(f"This workspace has used {NL_SQL_AT_START} NL to SQL queries this month.", [])
    code, report, _ = run(tmp_path, capsys, "--only", "s01,s02,a03")
    assert code == 1
    assert buckets(report)["a03"] == "wrong"
    assert reasons(report)["a03"] == f"says {NL_SQL_AT_START}, the API says {NL_SQL_AT_START + 2}"


def test_a_plan_with_no_pipeline_limit_wants_unlimited(stub, tmp_path, capsys):
    stub.plan["pipelines_limit"] = None
    stub.override["a02"] = reply("Your plan has no pipeline limit.", [])
    code, report, _ = run(tmp_path, capsys, "--only", "a02")
    assert code == 0 and buckets(report) == {"a02": "right"}
    assert report["expected"]["plan_pipeline_limit"] == "unlimited"

    stub.override["a02"] = reply(ANSWERS["a02"], [])  # "allows 10 pipelines"
    code, report, _ = run(tmp_path, capsys, "--only", "a02", out="again")
    assert code == 1 and buckets(report) == {"a02": "wrong"}


def test_honest_review_and_error_answers(stub, tmp_path, capsys):
    stub.override["w01"] = reply("I couldn't find your pipelines.", [])
    stub.override["w03"] = (500, {"error": "internal"})
    stub.override["w04"] = reply("Your destination connection is bq.", [])  # one of two: a person reads it
    stub.override["w06"] = reply("   ", [])
    code, report, _ = run(tmp_path, capsys, "--only", "w01,w03,w04,w06")
    assert buckets(report) == {"w01": "honest", "w03": "error", "w04": "review", "w06": "error"}
    assert reasons(report)["w04"] == "names 1 of 2"
    # Ask failing is not a wrong answer, but a person would see it: the bar is not met.
    assert code == 1 and report["bar"]["chatgpt"] == {"met": False, "wrong": 0, "error": 2}


CLOUDFLARE_502 = b"<!DOCTYPE html><title>rsync.ai | Bad gateway</title>"


def in_turn(*answers):
    """An override that answers each ask of a question with the next answer, the last one
    from then on; None is the right answer."""
    left = list(answers)

    def answer(stub, qid):
        a = left.pop(0) if len(left) > 1 else left[0]
        return perfect(stub, qid) if a is None else a
    return answer


def test_a_busy_minute_is_waited_out_and_the_question_asked_again(stub, tmp_path, capsys, monkeypatch):
    """Bug class: an eval that grades a busy minute. On 2026-10-11, 13 of 51 answers were
    a 502 from Vertex's RESOURCE_EXHAUSTED, which Cloudflare had turned into its own page.
    A 429 from the chat limit, or a 502 with no code or provider_error, is asked again after
    a wait (Retry-After when the 429 gives one), at most twice; any other failure is not."""
    waits = []
    monkeypatch.setattr(ev, "_sleep", waits.append)
    limited = (429, {"error": "rate_limit_exceeded", "retry_after": 7})
    stub.override = {
        "w01": in_turn((502, CLOUDFLARE_502), None),
        "w02": in_turn(limited, None),
        "w03": in_turn((502, {"error": "provider_error", "message": "the model provider is rate limiting this key"})),
        "w04": in_turn((502, {"error": "llm_service_unreachable"}), None),
        "w05": in_turn((504, {"error": "timeout"}), None),
        "w06": in_turn((429, {"error": "rate_limit_exceeded", "retry_after": 600}), None),
    }
    code, report, printed = run(tmp_path, capsys, "--only", "w01,w02,w03,w04,w05,w06")
    assert buckets(report) == {"w01": "right", "w02": "right", "w03": "error", "w04": "error", "w05": "error",
                               "w06": "error"}
    assert reasons(report)["w03"] == "HTTP 502 provider_error"
    assert waits == [30.0, 7.0, 30.0, 60.0]
    asks = {qid: sum(question_id(r["body"]["question"]) == qid for r in stub.asks())
            for qid in ("w01", "w02", "w03", "w04", "w05", "w06")}
    assert asks == {"w01": 2, "w02": 2, "w03": 3, "w04": 1, "w05": 1, "w06": 1}
    # Each try is its own question, so Stop can name it.
    assert len({r["body"]["request_id"] for r in stub.asks()}) == len(stub.asks())
    # The report says how often, so a busy day does not pass for a clean one.
    s = report["entries"]["chatgpt"]["summary"]
    assert s["asked_again"] == 4 and code == 1
    assert "Asked again 4 time(s) after an HTTP 429 or 502" in (tmp_path / "report" / "report.md").read_text()
    assert "w01 run 1: HTTP 502, asking again in 30s" in printed.out


# ---- answers the grading misread on prod (2026-10-09 baseline) ------------------------------
# Each misread answer is graded by its question's real expect; the case after it is
# a real wrong answer of the same shape, which must stay wrong.


def graded(qid, text, truth=None, extra=None, fallbacks=()):
    expect = BY_ID[qid]["expect"]
    t = {expect["truth"]: truth} if "truth" in expect else {}
    return ev.grade({"id": qid, "expect": expect}, 200, reply(text)[1], {**t, **(extra or {})}, frozenset(), None,
                    "q", (), fallbacks)


@pytest.mark.parametrize("qid,truth,text,bucket", [
    # The answer opens by saying it found nothing: a number after that is context, not the answer.
    ("a02", ev.UNLIMITED, "I could not find your plan's pipeline limit. The workspace currently has 3 pipelines.",
     "honest"),
    ("a02", ev.UNLIMITED, "Your plan allows 3 pipelines. I could not find more.", "wrong"),
    ("a03", 17, "I could not find that. Each query I write counts as one NL→SQL query on your plan.", "honest"),
    ("a03", 17, "I could not find the number of NL to SQL queries used.\n\nYour workspace has 14 models.", "honest"),
    ("a03", 17, "This workspace has used 14 NL to SQL queries. I could not find the limit.", "wrong"),
    ("a03", 17, "I could not find the limit, but you have used 17 NL to SQL queries.", "right"),
    ("w05", "passed", "I could not find its latest run. Last week it was failing.", "honest"),
    ("w05", "passed", "It is failing. I could not find why.", "wrong"),
    # A claim the answer only names ("says whether …") is not a claim it makes.
    ("h10", None, "I could not find that. Nothing there says whether rsync.ai is open source, and I won't guess.",
     "honest"),
    ("h10", None, "I could not find whether or not rsync.ai is open source.", "honest"),
    ("h10", None, "Whether you self-host or not, rsync.ai is open source.", "wrong"),
    ("h10", None, "rsync.ai is open source.", "wrong"),
    ("h05", None, "I couldn't find if incremental mode is supported.", "honest"),
    ("h05", None, "Incremental mode is supported.", "wrong"),
])
def test_a_hedge_or_a_named_claim_is_not_a_wrong_answer(qid, truth, text, bucket):
    r = graded(qid, text, truth)
    assert r["bucket"] == bucket, r


def test_ask_s_own_fallback_is_an_error_not_a_wrong_answer(stub, tmp_path, capsys):
    """loop.go finalAnswer's text when the model wrote nothing: Ask failed, an error that
    keeps the bar unmet, never a wrong answer (eval_questions_test.go pins the leads)."""
    for text in ("The model returned an empty answer. Try asking again.",
                 "The model ran out of room before it wrote an answer. Ask a narrower question.",
                 "The model returned an empty answer. These calls failed:\n- get_model: timeout\n\nTry rephrasing."):
        r = graded("l04", text, ["daily_orders"], fallbacks=FALLBACKS)
        assert r["bucket"] == "error" and r["reason"].startswith("Ask's fallback: The model "), r
    # An answer that only quotes the fallback later is graded as an answer.
    assert graded("l04", "None are affected. The model returned an empty answer.", ["daily_orders"],
                  fallbacks=FALLBACKS)["bucket"] == "wrong"
    # And through the eval end to end: main reads fallback_answers from the questions file.
    stub.override["l04"] = reply("The model returned an empty answer. Try asking again.", [looked("list_models")])
    code, report, _ = run(tmp_path, capsys, "--only", "l04")
    assert code == 1 and buckets(report) == {"l04": "error"}
    assert report["bar"]["chatgpt"] == {"met": False, "wrong": 0, "error": 1}


ALIAS_GRAPH = {
    "nodes": [
        {"id": "model\x00gold", "kind": "model", "name": "Active customers by month"},
        {"id": "model\x00acc", "kind": "model", "name": "Accounts (silver)"},
        {"id": "model\x00cus", "kind": "model", "name": "Customers (silver)"},
        {"id": "table\x00acc", "kind": "table", "name": "fintech_silver.accounts_silver"},
        {"id": "table\x00cus", "kind": "table", "name": "fintech_silver.customers_silver"},
        {"id": "table\x00gold", "kind": "table", "name": "fintech_gold.monthly_active_customers"},
    ],
    "edges": [
        {"from": "model\x00acc", "to": "table\x00acc", "kind": "materializes"},
        {"from": "model\x00cus", "to": "table\x00cus", "kind": "materializes"},
        {"from": "model\x00gold", "to": "table\x00gold", "kind": "materializes"},
        {"from": "table\x00acc", "to": "model\x00gold", "kind": "reads"},
        {"from": "table\x00cus", "to": "model\x00gold", "kind": "reads"},
    ],
}


@pytest.mark.parametrize("qid,text,bucket", [
    # A model's input table, named by the model that builds it.
    ("l03", 'It reads from two silver models: "Accounts (silver)" and "Customers (silver)".', "right"),
    ("l03", "It reads fintech_silver.accounts_silver and fintech_silver.customers_silver.", "right"),
    ("l03", "It reads Cards (silver) and Merchants (silver).", "wrong"),
    # A reading model, named by the table it builds.
    ("l02", "fintech_gold.monthly_active_customers is built from it.", "right"),
    ("l02", "Nothing reads it.", "wrong"),
])
def test_a_model_and_the_table_it_builds_name_the_same_thing(qid, text, bucket):
    slots, truth = ev.lineage(ALIAS_GRAPH)
    assert slots == {"lineage_model": "Active customers by month", "lineage_table": "fintech_silver.accounts_silver"}
    assert truth["lineage_model_inputs"] == ["accounts_silver", "customers_silver"]
    assert truth["lineage_aliases"] == {
        "accounts_silver": ["Accounts (silver)"], "Active customers by month": ["monthly_active_customers"],
        "customers_silver": ["Customers (silver)"]}
    want = truth["lineage_model_inputs" if qid == "l03" else "lineage_table_readers"]
    r = graded(qid, text, want, {"lineage_aliases": truth["lineage_aliases"]})
    assert r["bucket"] == bucket, r


STEP_PROMPT = os.path.join(REPO_ROOT, "llm-service", "prompts", "assistant", "step.yaml")


def test_the_decline_ask_s_prompt_asks_for_grades_right():
    """step.yaml tells Ask how to decline what is not rsync.ai. The eval must grade that
    decline right on every off-topic question, or a model that follows the rule fails it."""
    with open(STEP_PROMPT, encoding="utf-8") as f:
        prompt = " ".join(f.read().split())
    said = re.search(r'say in one sentence that you (can only help with [^,]+), then name one thing you can do, '
                     r'for example "([^"]+)"', prompt)
    assert said, "step.yaml no longer says how Ask declines"
    decline = f"I {said[1]}. {said[2]}"
    offtopic = [q for q in QUESTIONS if q["kind"] == "offtopic"]
    assert len(offtopic) == 6
    for q in offtopic:
        assert ev.grade_decline(decline, q["expect"]) == ("right", ""), (q["id"], decline)


def test_the_schedule_name_ask_s_prompt_asks_for_grades_right():
    """step.yaml tells Ask to name a model's schedule as the app shows it, never
    after_upstream. h06 accepted only "upstream", so on 2026-10-11 Ask doing as told
    was graded wrong."""
    with open(STEP_PROMPT, encoding="utf-8") as f:
        prompt = " ".join(f.read().split())
    said = re.search(r'a model\'s schedule "([^"]+)", never after_upstream', prompt)
    assert said, "step.yaml no longer says how Ask names a model's schedule"
    for text in (f'Set the model\'s schedule to "{said[1]}" and pick the pipeline.',
                 "Open the model, and under Rebuild after pick the pipeline it reads from."):
        assert graded("h06", text)["bucket"] == "right", text
    assert graded("h06", "Give the model an hourly schedule.")["bucket"] == "wrong"


# ---- reading the workspace ------------------------------------------------------------------


def test_pipelines_are_read_across_pages(stub, tmp_path, capsys):
    stub.page_size = 2  # "orders sync" and the failed pipeline are on page 2
    code, report, _ = run(tmp_path, capsys, "--only", "w01,d03,d05")
    assert code == 0, reasons(report)
    assert report["expected"]["pipelines_total"] == 3 and report["slots"]["failed_pipeline"] == "orders sync"
    assert [r["query"]["page"] for r in stub.requests if r["path"] == "/api/v1/pipelines"] == ["1", "2"]


def test_a_question_with_nothing_to_ask_about_is_skipped_not_asked(stub, tmp_path, capsys):
    stub.pipelines = [p for p in stub.pipelines if p["derived_status"] != "failed"]
    stub.graph = {"nodes": [], "edges": []}
    stub.override["d05"] = reply("No pipelines are failing right now.", [])
    code, report, _ = run(tmp_path, capsys, "--only", "d01,d02,d03,d05,l01,l02,l03,l04,w01")
    assert buckets(report) == {"d01": "skipped", "d02": "skipped", "d03": "skipped", "d05": "right",
                               "l01": "skipped", "l02": "skipped", "l03": "skipped", "l04": "skipped", "w01": "wrong"}
    skipped = report["entries"]["chatgpt"]["summary"]["skipped"]
    assert {s["id"] for s in skipped} == {"d01", "d02", "d03", "l01", "l02", "l03", "l04"}
    assert all(s["why"].startswith("nothing to fill") for s in skipped)
    assert len(stub.asks()) == 2


def test_dry_run_asks_nothing_and_runs_no_reference_query(stub, tmp_path, capsys):
    code = ev.main(["--dry-run", "--out", str(tmp_path / "dry")])
    printed = capsys.readouterr()
    assert code == 0, printed.err
    assert stub.asks() == [] and stub.sql_run() == [PROBE]
    assert not (tmp_path / "dry").exists()
    assert "s02 [data] In bq, how many accounts rows are there for each tier?" in printed.out
    assert f"    reference: SELECT `tier` AS c1, COUNT(*) AS c2 FROM {T} GROUP BY `tier`" in printed.out
    assert ROW_MARK not in printed.out and TOKEN not in printed.out


def test_quota_stops_the_run_and_keeps_what_was_asked(stub, tmp_path, capsys):
    stub.quota_after = 3
    code, report, _ = run(tmp_path, capsys)
    assert code == 1
    assert report["stopped"].startswith("quota_exceeded")
    assert report["entries"]["chatgpt"]["summary"]["asked"] == 3


def test_a_baseline_is_compared_only_when_it_asked_the_same_questions(stub, tmp_path, capsys):
    stub.override["w01"] = reply("There are 4 pipelines.", [])
    run(tmp_path, capsys, "--only", "w01,w03", out="before")
    del stub.override["w01"]
    code, report, _ = run(tmp_path, capsys, "--only", "w01,w03", "--baseline", str(tmp_path / "before" / "report.json"),
                          out="after")
    assert code == 0
    assert report["bar"]["chatgpt"]["change"] == {"workspace": {"right": 1, "wrong": -1, "error": 0}}

    other = tmp_path / "other.json"
    doc = json.loads(open(QUESTIONS_FILE).read())
    doc["questions"] = doc["questions"][:5]
    other.write_text(json.dumps(doc))
    code, report, _ = run(tmp_path, capsys, "--questions", str(other), "--baseline",
                          str(tmp_path / "before" / "report.json"), out="other")
    assert "change" not in report["bar"]["chatgpt"]
    assert report["bar"]["chatgpt"]["baseline"].startswith("the baseline asked other questions")


def test_setup_errors_exit_2_and_never_print_the_token(stub, tmp_path, capsys, monkeypatch):
    assert ev.main(["--only", "nope", "--out", str(tmp_path / "x")]) == 2
    assert "--only names no such question: nope" in capsys.readouterr().err
    assert ev.main(["--entries", "claude", "--out", str(tmp_path / "x")]) == 2
    assert "no key for claude" in capsys.readouterr().err
    monkeypatch.setenv("RSYNC_EVAL_BASE_URL", "http://example.com")
    assert ev.main(["--out", str(tmp_path / "x")]) == 2
    assert TOKEN not in capsys.readouterr().err


# ---- the parts --------------------------------------------------------------------------------


@pytest.mark.parametrize("sql,refused", [
    ("SELECT COUNT(*) FROM t", ""),
    ("  with x as (select 1) select * from x;", ""),
    ("(SELECT 1) UNION (SELECT 2)", ""),
    ("SELECT * FROM t WHERE note = 'please delete; drop table t'", ""),
    ('SELECT "update" FROM t -- delete me', ""),
    ("SELECT replace(a, 'x', 'y'), left(b, 2) FROM t", ""),
    ("DELETE FROM t", "not a SELECT or WITH"),
    ("SELECT 1; DROP TABLE t", "more than one statement"),
    ("WITH gone AS (DELETE FROM t RETURNING *) SELECT * FROM gone", "contains DELETE"),
    ("SELECT * INTO copy_t FROM t", "contains INTO"),
    ("/* a note */ INSERT INTO t VALUES (1)", "not a SELECT or WITH"),
    ("SELECT * FROM t FOR UPDATE", "contains UPDATE"),
    ("", "empty"),
])
def test_read_only_sql(sql, refused):
    assert ev.read_only_sql(sql) == refused


def test_same_rows_ignores_order_column_names_and_number_formatting():
    ref = {"columns": ["c1", "c2"], "rows": [{"c1": "a", "c2": 2}, {"c1": "b", "c2": 10}]}
    assert ev.same_rows(ref, {"columns": ["k", "n", "extra"], "rows": [{"k": "b", "n": "10.0", "extra": 1},
                                                                      {"k": "a", "n": 2, "extra": 1}]})
    assert not ev.same_rows(ref, {"columns": ["k", "n"], "rows": [{"k": "a", "n": 2}]})
    # Nor can extra rows hide behind the right ones: a missing WHERE still answers wrong.
    assert not ev.same_rows(ref, {"columns": ["k", "n"], "rows": [{"k": "a", "n": 2}, {"k": "b", "n": 10},
                                                                  {"k": "c", "n": 1}]})
    assert not ev.same_rows(ref, {"columns": ["k", "n"], "rows": [{"k": "a", "n": 2}, {"k": "b", "n": 11}]})
    # One answer row cannot stand in for two reference rows.
    dup = {"columns": ["c1"], "rows": [{"c1": 1}, {"c1": 1}]}
    assert not ev.same_rows(dup, {"columns": ["c1"], "rows": [{"c1": 1}, {"c1": 2}]})
    assert ev.same_rows({"columns": ["c1"], "rows": [{"c1": "2024-01-02T03:04:05.000Z"}]},
                        {"columns": ["x"], "rows": [{"x": "2024-01-02 03:04:05 UTC"}]})


@pytest.mark.parametrize("dialect,want", [
    ("bigquery", "`ds`.`t``x`"), ("mysql", "`ds`.`t``x`"), ("postgresql", '"ds"."t`x"'),
    ("snowflake", '"ds"."t`x"'), ("sqlserver", "[ds].[t`x]"),
])
def test_quote_follows_the_connection_dialect(dialect, want):
    assert ev.quote(dialect, "ds", "t`x") == want
    assert ev.quote(dialect, "", "t") == ev.quote(dialect, "t")


def test_the_question_file_is_checked(tmp_path):
    doc = json.loads(open(QUESTIONS_FILE).read())
    assert {q["kind"] for q in QUESTIONS} == set(ev.KINDS)
    for bad, message in (
        (lambda d: d["questions"].append(dict(d["questions"][0])), "missing or repeated"),
        (lambda d: d["questions"][0]["expect"].update(noun="(unclosed"), "bad pattern"),
        (lambda d: d["questions"][0]["expect"].pop("truth"), "names its expect.truth"),
        (lambda d: d.update(write_tools=[]), "write_tools is empty"),
        (lambda d: d.pop("fallback_answers"), "fallback_answers is empty"),
        (lambda d: d["questions"].append({"id": "x1", "kind": "data", "question": "q",
                                          "expect": {"type": "sql", "reference_sql": "DELETE FROM t",
                                                     "connection": "bq"}}), "one SELECT"),
    ):
        d = copy.deepcopy(doc)
        bad(d)
        path = tmp_path / "q.json"
        path.write_text(json.dumps(d))
        with pytest.raises(ev.EvalError, match=message):
            ev.load_questions(str(path))
