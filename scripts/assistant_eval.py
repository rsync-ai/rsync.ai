#!/usr/bin/env python3
"""Ask rsync.ai's eval: 20 fixed questions, each asked on every model entry, and a report.

docs/internal/assistant-token-design.md § 8.2 says what it measures and the bar a
prompt change must meet; docs/runbook.md § "Ask rsync.ai eval" says how to run it.
It runs by hand, never in CI: every question is a real model call, paid from the
signed-in person's assistant quota. CI runs it against a stub instead
(llm-service/tests/test_assistant_eval.py).

It asks through the same API the panel does (POST /api/v1/assistant/messages), as
a person, with a session token read from the environment:

  RSYNC_EVAL_TOKEN               a member's session token (required)
  RSYNC_EVAL_WORKSPACE_ID        the workspace it asks in (required)
  RSYNC_EVAL_ADMIN_TOKEN         optional: a session token for a throwaway workspace
  RSYNC_EVAL_ADMIN_WORKSPACE_ID  where that person is admin. The workflow and dashboard
                                 drafts need a workspace admin; without these they run
                                 as the member and report "draft refused (member)"
  RSYNC_EVAL_BASE_URL            default https://app.rsync.ai

The questions only read and draft. A member cannot write models or generate a
connector (services/pipeline_policy.go), and a call to any write tool fails its
question whatever it returned. Neither token is printed or written to the report.

The report holds tool names, outcomes and counts, never an answer's text or a
tool's arguments: an answer can carry values a sample_rows call read.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import secrets
import statistics
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_QUESTIONS = os.path.join(HERE, "assistant_eval_questions.json")
DEFAULT_BASE_URL = "https://app.rsync.ai"
LOCAL_HOSTS = frozenset({"localhost", "127.0.0.1", "::1"})

# Connections the SQL questions and the workflow drafts may name: a SQL engine
# whose tables describe_schema lists.
SQL_CONNECTORS = frozenset({
    "postgresql", "postgres", "mysql", "mariadb", "bigquery", "snowflake", "redshift",
    "mssql", "sqlserver", "clickhouse", "oracle", "cockroachdb", "alloydb", "supabase",
    "neon", "timescaledb", "databricks",
})

REFUSED_STATUSES = frozenset({400, 422})

# The parts a run's input tokens are split into, in the report's column order.
SPLIT_PARTS = ("fixed", "guides", "conversation", "tool_results", "unattributed")


class EvalError(Exception):
    """A problem that stops the run: printed, exit code 2."""


class QuotaExceeded(Exception):
    """The person's assistant quota is spent: the run stops and reports what it has."""


class EntryUnavailable(Exception):
    """The entry lost its key mid-run: its remaining questions are not asked."""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # A redirect would resend the session token to wherever it points.
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def check_base_url(base_url: str) -> str:
    """The URL a session token may go to: https, or http on this machine."""
    u = urllib.parse.urlsplit(base_url)
    if u.scheme == "https" or (u.scheme == "http" and u.hostname in LOCAL_HOSTS):
        if u.hostname:
            return f"{u.scheme}://{u.netloc}"
    raise EvalError(f"RSYNC_EVAL_BASE_URL must be https (or http on localhost): {base_url!r}")


class Client:
    """The API as one person in one workspace."""

    def __init__(self, base_url: str, token: str, workspace_id: str, timeout: float = 240.0):
        self.base_url = base_url
        self._token = token
        self.workspace_id = workspace_id
        self.timeout = timeout
        # CSRF is a double submit: any cookie and header that match.
        self._csrf = secrets.token_urlsafe(24)
        self._opener = urllib.request.build_opener(_NoRedirect)

    def request(self, method: str, path: str, body: dict | None = None) -> tuple[int, object]:
        headers = {
            "Authorization": f"Bearer {self._token}",
            "X-Workspace-ID": self.workspace_id,
            "Cookie": f"csrf_token={self._csrf}",
            "X-CSRF-Token": self._csrf,
            "Accept": "application/json",
            "User-Agent": "rsync-ai-assistant-eval",
        }
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self._opener.open(req, timeout=self.timeout) as r:
                return r.status, _json(r.read())
        except urllib.error.HTTPError as e:
            return e.code, _json(e.read())
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            reason = getattr(e, "reason", e)
            raise EvalError(f"{method} {path}: {reason}") from None

    def get(self, path: str) -> tuple[int, object]:
        return self.request("GET", path)

    def post(self, path: str, body: dict) -> tuple[int, object]:
        return self.request("POST", path, body)


def _json(raw: bytes) -> object:
    try:
        return json.loads(raw) if raw else None
    except ValueError:
        return None


def _error_code(body: object) -> str:
    if isinstance(body, dict) and isinstance(body.get("error"), str):
        return body["error"][:80]
    return ""


def _list(client: Client, path: str, key: str) -> list[dict]:
    status, body = client.get(path)
    if status != 200 or not isinstance(body, dict):
        raise EvalError(f"GET {path} answered {status} {_error_code(body)}".rstrip())
    items = body.get(key) or []
    return sorted((i for i in items if isinstance(i, dict) and i.get("name")), key=lambda i: str(i["name"]).casefold())


def workspace_slots(client: Client) -> dict[str, str]:
    """Names to put in the questions' {slots}, read with GETs only.

    Each slot takes the first match by name, so the same workspace fills the same
    names on every run and two reports stay comparable."""
    pipelines = _list(client, "/api/v1/pipelines?per_page=100", "pipelines")
    connections = _list(client, "/api/v1/connections", "connections")
    dashboards = _list(client, "/api/v1/dashboards", "dashboards")
    workflows = _list(client, "/api/v1/workflows", "workflows")
    channels = _list(client, "/api/v1/delivery-channels", "channels")

    def first(items, match=lambda i: True):
        return next((str(i["name"]) for i in items if match(i)), None)

    slots = {
        "pipeline": first(pipelines),
        "failed_pipeline": first(pipelines, lambda p: p.get("derived_status") == "failed"),
        "connection": first(connections),
        "source_connection": first(connections, lambda c: c.get("type") == "source"),
        "destination_connection": first(connections, lambda c: c.get("type") == "destination"),
        "sql_connection": first(connections, lambda c: str(c.get("connector_type", "")).lower() in SQL_CONNECTORS),
        "dashboard_with_tiles": first(dashboards, lambda d: (d.get("tile_count") or 0) > 0),
        "workflow": first(workflows),
        "channel": first(channels),
    }
    return {k: v for k, v in slots.items() if v}


def fill(text: str, slots: dict[str, str]) -> tuple[str | None, list[str]]:
    """The question with its slots filled, or None and the slots it lacks."""
    missing = []
    out = text
    for name in _slot_names(text):
        if name in slots:
            out = out.replace("{" + name + "}", slots[name])
        else:
            missing.append(name)
    return (None, missing) if missing else (out, [])


def _slot_names(text: str) -> list[str]:
    names, i = [], 0
    while (start := text.find("{", i)) != -1:
        end = text.find("}", start)
        if end == -1:
            break
        names.append(text[start + 1:end])
        i = end + 1
    return names


def load_questions(path: str) -> tuple[list[dict], frozenset[str]]:
    """The questions, and the tools that change rsync.ai rather than draft for
    the person to review. A member's role refuses each write server-side;
    calling one still fails the question, because no question asks for one."""
    with open(path) as f:
        doc = json.load(f)
    questions, write_tools = doc["questions"], frozenset(doc["write_tools"])
    if not write_tools:
        raise EvalError(f"{path}: write_tools is empty")
    seen = set()
    for q in questions:
        if q["id"] in seen:
            raise EvalError(f"{path}: question id {q['id']} twice")
        seen.add(q["id"])
        if q["role"] not in ("member", "admin"):
            raise EvalError(f"{path}: {q['id']} role must be member or admin")
        for tool in _flat(q.get("must_call", [])) + q.get("must_not_call", []) + [q.get("draft") or ""]:
            if tool in write_tools:
                raise EvalError(f"{path}: {q['id']} names the write tool {tool}; the questions only read and draft")
    return questions, write_tools


def _flat(items: list) -> list[str]:
    out = []
    for i in items:
        out.extend(i if isinstance(i, list) else [i])
    return out


# ---- one answer -------------------------------------------------------------------


def grade(q: dict, status: int, body: object, as_member: bool, write_tools: frozenset[str]) -> dict:
    """What one run of a question measured, and whether it passed."""
    run = {
        "http_status": status,
        "passed": False,
        "reasons": [],
        "input_tokens": 0,
        "cached_tokens": 0,
        "output_tokens": 0,
        "reasoning_tokens": 0,
        "split": {k: 0.0 for k in SPLIT_PARTS},
        "steps": 0,
        "calls": 0,
        "tools": [],
        "counts": {k: 0 for k in ("unknown_tool", "refused_arguments", "not_found", "forbidden", "limit",
                                  "dropped", "too_large", "error", "write")},
        "draft_ok": None,
        "stopped": "",
        "cost_cents": 0,
        "answer_chars": 0,
    }
    if status != 200 or not isinstance(body, dict):
        run["reasons"].append(f"HTTP {status} {_error_code(body)}".rstrip())
        return run

    usage = body.get("usage") or {}
    for key in ("input_tokens", "cached_tokens", "output_tokens", "reasoning_tokens"):
        run[key] = int(usage.get(key) or 0)
    run["stopped"] = body.get("stopped") or ""
    run["cost_cents"] = int(body.get("cost_cents") or 0)
    run["answer_chars"] = len(body.get("answer") or "")

    calls = []
    for step in usage.get("steps") or []:
        run["steps"] += 1
        _split_step(step, run["split"])
        calls.extend(step.get("calls") or [])
    run["calls"] = len(calls)
    run["tools"] = [{"tool": c.get("tool", ""), "outcome": c.get("outcome", ""), "status": c.get("status", 0)}
                    for c in calls]
    counts = run["counts"]
    for c in calls:
        outcome, code = c.get("outcome", ""), int(c.get("status") or 0)
        if c.get("tool") in write_tools:
            counts["write"] += 1
        if outcome in ("unknown_tool", "limit", "dropped", "too_large"):
            counts[outcome] += 1
        elif outcome == "invalid_arguments" or code in REFUSED_STATUSES:
            counts["refused_arguments"] += 1
        elif code == 404:
            counts["not_found"] += 1
        elif code == 403 or outcome == "forbidden":
            counts["forbidden"] += 1
        elif code >= 500 or outcome == "error":
            counts["error"] += 1

    reasons = run["reasons"]
    ok_tools = {c.get("tool") for c in calls if c.get("outcome") == "ok"}
    called = {c.get("tool") for c in calls}
    for item in q.get("must_call", []):
        options = item if isinstance(item, list) else [item]
        if not ok_tools.intersection(options):
            reasons.append("no ok call to " + " or ".join(options))
    for tool in q.get("must_not_call", []):
        if tool in called:
            reasons.append(f"called {tool}")
    if counts["write"]:
        reasons.append("called a write tool: " + ", ".join(sorted(called & write_tools)))
    if counts["unknown_tool"]:
        reasons.append("called a tool that does not exist")
    if not run["answer_chars"]:
        reasons.append("empty answer")
    draft = q.get("draft")
    if draft:
        run["draft_ok"] = any(
            la.get("tool") == draft and la.get("ok") and la.get("resource_id")
            for la in body.get("looked_at") or [] if isinstance(la, dict)
        )
        if not run["draft_ok"]:
            reasons.append(f"{draft} stored no draft")
    if as_member and draft:
        # An admin draft asked as a member: the role refuses it, so it is
        # reported apart, never as passed or failed.
        refused = any(c.get("tool") == draft and int(c.get("status") or 0) == 403 for c in calls)
        run["reasons"] = ["draft refused (member)" if refused else "asked as a member: " + "; ".join(reasons)]
        return run
    run["passed"] = not reasons
    return run


def _split_step(step: dict, split: dict) -> None:
    """Share one step's input tokens by the characters each part sent.

    The provider counts tokens for the whole request only, so the split is an
    estimate: system and tool list (the fixed part), the topic guides, the
    person's questions and the model's own turns (conversation), and tool
    results. A guide is sent as a tool result, so its characters (guide_chars,
    absent before the guides) come out of the tool results."""
    tokens = int(step.get("input_tokens") or 0)
    chars = step.get("chars") or {}
    tool = int(chars.get("tool") or 0)
    guides = min(int(step.get("guide_chars") or 0), tool)
    parts = {
        "fixed": int(chars.get("system") or 0) + int(chars.get("tools") or 0),
        "guides": guides,
        "conversation": int(chars.get("user") or 0) + int(chars.get("assistant") or 0),
        "tool_results": tool - guides,
    }
    total = sum(parts.values())
    if not total:
        split["unattributed"] += tokens
        return
    for k, v in parts.items():
        split[k] += tokens * v / total


# ---- the run ----------------------------------------------------------------------


def run_eval(questions: list[dict], write_tools: frozenset[str], member: Client, admin: Client | None,
             entries: list[str], runs: int, log=print) -> dict:
    slots = {"member": workspace_slots(member)}
    if admin:
        slots["admin"] = workspace_slots(admin)
    report = {
        "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "base_url": member.base_url,
        "runs_per_question": runs,
        "admin_workspace": admin is not None,
        "slots": slots,
        "entries": {},
        "stopped": "",
    }
    try:
        for entry in entries:
            out = report["entries"][entry] = {"model": "", "questions": []}
            try:
                _ask_entry(entry, out, questions, write_tools, member, admin, slots, runs, log)
            except EntryUnavailable:
                out["error"] = "model_not_configured: the entry lost its key during the run"
                log(f"  {entry}: {out['error']}")
    except QuotaExceeded:
        report["stopped"] = "quota_exceeded: the person's assistant quota is spent; the report holds the runs before it"
        log("  " + report["stopped"])
    for out in report["entries"].values():
        out["summary"] = summarize(out["questions"])
    return report


def _ask_entry(entry: str, out: dict, questions: list[dict], write_tools: frozenset[str], member: Client,
               admin: Client | None, slots: dict, runs: int, log) -> None:
    for q in questions:
        as_member = q["role"] == "admin" and admin is None
        client = admin if (q["role"] == "admin" and admin) else member
        text, missing = fill(q["question"], slots["admin" if client is admin else "member"])
        rec = {"id": q["id"], "topic": q["topic"], "role": q["role"], "as_member": as_member,
               "draft": q.get("draft") or "", "runs": []}
        out["questions"].append(rec)
        if text is None:
            rec["skipped"] = "nothing to fill " + ", ".join("{" + m + "}" for m in missing)
            log(f"  {entry} {q['id']} skipped: {rec['skipped']}")
            continue
        for n in range(runs):
            t0 = time.monotonic()
            status, body = client.post("/api/v1/assistant/messages", {
                "question": text, "model": entry, "request_id": uuid.uuid4().hex,
            })
            latency = time.monotonic() - t0
            if status == 402:
                raise QuotaExceeded()
            if status == 409 and _error_code(body) == "model_not_configured":
                raise EntryUnavailable()
            run = grade(q, status, body, as_member, write_tools)
            run["latency_s"] = round(latency, 2)
            if isinstance(body, dict) and isinstance(body.get("model"), dict) and not out["model"]:
                out["model"] = str(body["model"].get("model") or "")
            rec["runs"].append(run)
            verdict = "pass" if run["passed"] else "; ".join(run["reasons"])
            log(f"  {entry} {q['id']} run {n + 1}: {verdict} "
                f"({run['input_tokens']} in, {run['steps']} steps, {run['latency_s']}s)")


def summarize(questions: list[dict]) -> dict:
    graded = [r for q in questions if not q["as_member"] for r in q["runs"]]
    every = [r for q in questions for r in q["runs"]]
    s: dict = {
        "questions": sum(1 for q in questions if q["runs"]),
        "skipped": [q["id"] for q in questions if q.get("skipped")],
        "asked_as_member": [q["id"] for q in questions if q["as_member"] and q["runs"]],
        "runs": len(graded),
        "runs_passed": sum(1 for r in graded if r["passed"]),
        "questions_passed": sorted(q["id"] for q in questions
                                   if not q["as_member"] and any(r["passed"] for r in q["runs"])),
        "questions_failed": sorted(q["id"] for q in questions
                                   if not q["as_member"] and q["runs"] and not any(r["passed"] for r in q["runs"])),
    }
    n = len(every) or 1
    total_in = sum(r["input_tokens"] for r in every)
    s["input_tokens_per_question"] = round(total_in / n)
    s["output_tokens_per_question"] = round(sum(r["output_tokens"] for r in every) / n)
    s["reasoning_tokens_per_question"] = round(sum(r["reasoning_tokens"] for r in every) / n)
    passed_in = sum(r["input_tokens"] for r in graded)
    s["input_tokens_per_passed_question"] = round(passed_in / s["runs_passed"]) if s["runs_passed"] else None
    s["cached_share"] = round(sum(r["cached_tokens"] for r in every) / total_in, 3) if total_in else 0.0
    split = {k: sum(r["split"][k] for r in every) for k in SPLIT_PARTS}
    s["input_split_tokens_per_question"] = {k: round(v / n) for k, v in split.items()}
    s["input_split_share"] = {k: round(v / total_in, 3) if total_in else 0.0 for k, v in split.items()}
    s["steps_per_question"] = round(sum(r["steps"] for r in every) / n, 2)
    s["calls_per_question"] = round(sum(r["calls"] for r in every) / n, 2)
    s["counts"] = {k: sum(r["counts"][k] for r in every) for k in every[0]["counts"]} if every else {}
    drafts = [r for q in questions if q["draft"] and not q["as_member"] for r in q["runs"]]
    s["drafts_passed"] = sum(1 for r in drafts if r["draft_ok"])
    s["drafts"] = len(drafts)
    s["drafts_refused_member"] = sum(1 for q in questions if q["as_member"] for r in q["runs"]
                                     if r["reasons"] == ["draft refused (member)"])
    latencies = sorted(r["latency_s"] for r in every)
    s["latency_p50_s"] = round(statistics.median(latencies), 2) if latencies else None
    s["latency_max_s"] = latencies[-1] if latencies else None
    s["http_errors"] = sum(1 for r in every if r["http_status"] != 200)
    s["stopped_at_limit"] = sum(1 for r in every if r["stopped"] == "limit")
    s["cost_cents_charged"] = sum(r["cost_cents"] for r in every)
    return s


def check_bar(report: dict, baseline: dict | None) -> dict:
    """§ 8.2's bar, per entry: no question that passed before fails both runs, no
    call to a tool that does not exist, refused arguments no more than before."""
    bar = {}
    for entry, out in report["entries"].items():
        s = out["summary"]
        b = {"unknown_tools": {"value": s["counts"].get("unknown_tool", 0)}}
        b["unknown_tools"]["ok"] = b["unknown_tools"]["value"] == 0
        before = (baseline or {}).get("entries", {}).get(entry)
        if before is None:
            b["regressions"] = {"ok": None, "note": "needs a --baseline report with this entry"}
            b["refused_arguments"] = {"ok": None, "value": s["counts"].get("refused_arguments", 0),
                                      "note": "needs a --baseline report with this entry"}
        else:
            bs = before["summary"]
            asked = {q["id"] for q in out["questions"] if q["runs"] and not q["as_member"]}
            lost = sorted((set(bs["questions_passed"]) & asked) - set(s["questions_passed"]))
            b["regressions"] = {"ok": not lost, "questions": lost}
            rate = s["counts"].get("refused_arguments", 0) / max(s["runs"], 1)
            base_rate = bs["counts"].get("refused_arguments", 0) / max(bs["runs"], 1)
            b["refused_arguments"] = {"ok": rate <= base_rate, "per_run": round(rate, 3),
                                      "baseline_per_run": round(base_rate, 3)}
            notes = []
            if report.get("questions_sha256") != baseline.get("questions_sha256"):
                notes.append("the questions file changed since the baseline; the reports are not comparable")
            if report["slots"] != baseline.get("slots"):
                notes.append("the workspace filled different names than the baseline's; questions may differ")
            if notes:
                b["note"] = "; ".join(notes)
        checks = [b["unknown_tools"]["ok"], b["regressions"]["ok"], b["refused_arguments"]["ok"]]
        # A check that failed fails the bar even when another needs a baseline.
        b["met"] = False if False in checks else (None if None in checks else True)
        if out.get("error"):
            # Questions it never asked cannot regress, so a partial entry would
            # pass on less than the whole set.
            b["met"] = False
            b["note"] = "; ".join(filter(None, [out["error"], b.get("note")]))
        bar[entry] = b
    return bar


# ---- the report -------------------------------------------------------------------


def _pct(x: float) -> str:
    return f"{x * 100:.0f}%"


def markdown(report: dict) -> str:
    lines = [
        "# Ask rsync.ai eval",
        "",
        f"Started {report['started_at']} on {report['base_url']}; each question asked "
        f"{report['runs_per_question']}×. Admin workspace: {'yes' if report['admin_workspace'] else 'no'}.",
    ]
    if report.get("stopped"):
        lines += ["", f"**Stopped early:** {report['stopped']}"]
    lines += [
        "",
        "Input tokens are split by the characters each part sent (an estimate: the provider counts the whole "
        "request). **Fixed** = system prompt + tool list; **guides** = the topic guides loaded into the "
        "conversation; **conversation** = the questions and the model's own turns; **tool results** = what the "
        "tools returned.",
        "",
        "| Entry | Model | Runs passed | Input / question | Input / passed | Fixed | Guides | Conversation "
        "| Tool results | Cached | Output / q | Steps / q | Calls / q | Unknown tools | Refused args | Not found "
        "| Forbidden | Drafts passed | p50 latency | Charged |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for entry, out in report["entries"].items():
        s = out["summary"]
        sp, st, c = s["input_split_tokens_per_question"], s["input_split_share"], s["counts"]
        per_pass = s["input_tokens_per_passed_question"]
        lines.append(
            f"| {entry} | {out['model'] or '?'} | {s['runs_passed']}/{s['runs']} "
            f"| {s['input_tokens_per_question']:,} | {f'{per_pass:,}' if per_pass is not None else '—'} "
            f"| {sp['fixed']:,} ({_pct(st['fixed'])}) | {sp['guides']:,} ({_pct(st['guides'])}) "
            f"| {sp['conversation']:,} ({_pct(st['conversation'])}) "
            f"| {sp['tool_results']:,} ({_pct(st['tool_results'])}) | {_pct(s['cached_share'])} "
            f"| {s['output_tokens_per_question']:,} | {s['steps_per_question']} | {s['calls_per_question']} "
            f"| {c.get('unknown_tool', 0)} | {c.get('refused_arguments', 0)} | {c.get('not_found', 0)} "
            f"| {c.get('forbidden', 0)} | {s['drafts_passed']}/{s['drafts']} | {s['latency_p50_s']}s "
            f"| {s['cost_cents_charged']}¢ |"
        )
    lines += ["", "## The bar", ""]
    for entry, b in report.get("bar", {}).items():
        met = {True: "MET", False: "NOT MET", None: "not judged (no baseline)"}[b["met"]]
        lines.append(f"- **{entry}: {met}.**")
        lines.append(f"  - tools that do not exist: {b['unknown_tools']['value']}")
        r = b["regressions"]
        lines.append("  - questions that passed before and now fail every run: "
                     + (r.get("note") or (", ".join(r["questions"]) or "none")))
        ra = b["refused_arguments"]
        lines.append("  - refused arguments: " + (ra.get("note") or f"{ra['per_run']} a run, "
                                                  f"baseline {ra['baseline_per_run']}"))
        if b.get("note"):
            lines.append(f"  - note: {b['note']}")
    for entry, out in report["entries"].items():
        s = out["summary"]
        lines += ["", f"## {entry}: each question", ""]
        if out.get("error"):
            lines += [f"**Not finished:** {out['error']}", ""]
        lines += ["| Question | Topic | Runs passed | Input tokens | Steps | Calls | Why it failed |",
                  "|---|---|---|---|---|---|---|"]
        for q in out["questions"]:
            if q.get("skipped"):
                lines.append(f"| {q['id']} | {q['topic']} | skipped | | | | {q['skipped']} |")
                continue
            runs = q["runs"]
            passed = "member" if q["as_member"] else f"{sum(r['passed'] for r in runs)}/{len(runs)}"
            why = sorted({reason for r in runs for reason in r["reasons"]})
            lines.append(
                f"| {q['id']} | {q['topic']} | {passed} | {' / '.join(str(r['input_tokens']) for r in runs)} "
                f"| {' / '.join(str(r['steps']) for r in runs)} | {' / '.join(str(r['calls']) for r in runs)} "
                f"| {'; '.join(why)} |")
        if s["skipped"]:
            lines += ["", f"Skipped: {', '.join(s['skipped'])}. Add what their slots name to the workspace."]
    lines += ["", "Names the workspace filled into the questions:", ""]
    for who, slots in report["slots"].items():
        lines.append(f"- {who}: " + (", ".join(f"{{{k}}} = {v}" for k, v in sorted(slots.items())) or "none"))
    return "\n".join(lines) + "\n"


# ---- main -------------------------------------------------------------------------


def _env(name: str, required: bool = True) -> str:
    value = os.environ.get(name, "").strip()
    if required and not value:
        raise EvalError(f"set {name} (see docs/runbook.md § Ask rsync.ai eval)")
    return value


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Ask rsync.ai's eval (docs/internal/assistant-token-design.md § 8.2)")
    ap.add_argument("--out", help="directory for report.json and report.md (default ./assistant-eval-<time>)")
    ap.add_argument("--baseline", help="an earlier report.json to judge the bar against")
    ap.add_argument("--runs", type=int, default=2, help="times each question is asked (default 2)")
    ap.add_argument("--entries", help="comma-separated model entries (default every entry with a key)")
    ap.add_argument("--only", help="comma-separated question ids")
    ap.add_argument("--questions", default=DEFAULT_QUESTIONS, help=argparse.SUPPRESS)
    ap.add_argument("--dry-run", action="store_true",
                    help="fill the questions from the workspace and print them; ask nothing")
    args = ap.parse_args(argv)
    tokens: list[str] = []
    try:
        if args.runs < 1:
            raise EvalError("--runs must be at least 1")
        base_url = check_base_url(_env("RSYNC_EVAL_BASE_URL", required=False) or DEFAULT_BASE_URL)
        token = _env("RSYNC_EVAL_TOKEN")
        tokens.append(token)
        member = Client(base_url, token, _env("RSYNC_EVAL_WORKSPACE_ID"))
        admin = None
        admin_token = _env("RSYNC_EVAL_ADMIN_TOKEN", required=False)
        if admin_token:
            tokens.append(admin_token)
            admin = Client(base_url, admin_token, _env("RSYNC_EVAL_ADMIN_WORKSPACE_ID"))

        questions, write_tools = load_questions(args.questions)
        if args.only:
            wanted = {s.strip() for s in args.only.split(",") if s.strip()}
            unknown = wanted - {q["id"] for q in questions}
            if unknown:
                raise EvalError("--only names no such question: " + ", ".join(sorted(unknown)))
            questions = [q for q in questions if q["id"] in wanted]

        if args.dry_run:
            for who, client in (("member", member), ("admin", admin)):
                if client is None:
                    continue
                slots = workspace_slots(client)
                for q in questions:
                    if (q["role"] == "admin") != (who == "admin") and not (admin is None and who == "member"):
                        continue
                    text, missing = fill(q["question"], slots)
                    print(f"{q['id']} [{who}] " + (text or "skipped: nothing to fill " + ", ".join(missing)))
            return 0

        status, body = member.get("/api/v1/assistant/models")
        if status != 200 or not isinstance(body, dict):
            raise EvalError(f"GET /api/v1/assistant/models answered {status} {_error_code(body)}".rstrip())
        available = [e["entry"] for e in body.get("entries") or [] if isinstance(e, dict) and e.get("entry")]
        entries = available
        if args.entries:
            entries = [e.strip() for e in args.entries.split(",") if e.strip()]
            missing = [e for e in entries if e not in available]
            if missing:
                raise EvalError(f"no key for {', '.join(missing)}; entries with a key: {', '.join(available) or 'none'}")
        if not entries:
            raise EvalError("no model entry has a key; an admin adds one in Admin > Settings > AI models")

        baseline = None
        if args.baseline:
            with open(args.baseline) as f:
                baseline = json.load(f)

        print(f"Asking {len(questions)} questions × {args.runs} on {', '.join(entries)} at {base_url}")
        report = run_eval(questions, write_tools, member, admin, entries, args.runs)
        with open(args.questions, "rb") as f:
            report["questions_sha256"] = hashlib.sha256(f.read()).hexdigest()
        report["bar"] = check_bar(report, baseline)

        out_dir = args.out or f"assistant-eval-{datetime.now().strftime('%Y%m%d-%H%M%S')}"
        os.makedirs(out_dir, exist_ok=True)
        text_json = _redact(json.dumps(report, indent=2), tokens)
        text_md = _redact(markdown(report), tokens)
        with open(os.path.join(out_dir, "report.json"), "w") as f:
            f.write(text_json)
        with open(os.path.join(out_dir, "report.md"), "w") as f:
            f.write(text_md)
        print(text_md)
        print(f"Wrote {os.path.join(out_dir, 'report.json')} and report.md")
        if report["stopped"]:
            return 1
        return 1 if any(b["met"] is False for b in report["bar"].values()) else 0
    except EvalError as e:
        print("assistant_eval: " + _redact(str(e), tokens), file=sys.stderr)
        return 2


def _redact(text: str, tokens: list[str]) -> str:
    """A last guard: no token reaches a report or the terminal, whatever echoed it."""
    for t in tokens:
        if t:
            text = text.replace(t, "[token]")
    return text


if __name__ == "__main__":
    sys.exit(main())
