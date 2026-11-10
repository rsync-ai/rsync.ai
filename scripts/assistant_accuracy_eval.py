#!/usr/bin/env python3
"""Ask rsync.ai's accuracy eval: is the answer right, for every kind of question a person types?

scripts/assistant_eval.py measures which tools Ask called and what it cost. This one
measures the answer: each question's expected answer is read from rsync.ai's own API
when the eval runs (or, for how-to questions, written from the user docs), and each
answer lands in one bucket:

  right   says what the API (or the docs) say
  honest  says it could not find it, without a wrong claim
  wrong   says something the API, the docs or the product rules contradict
  review  the grader cannot tell; a person reads it in answers.jsonl
  error   Ask failed (HTTP error, empty answer): not a verdict on accuracy

The bar is no wrong answer and no failed one (an empty answer is what a person
would see too). There is no model judge: a judge would send answers to
another model, a new egress path (docs/security/llm-egress.md). Grading is by
numbers, names, the run a tool looked at, required and forbidden phrases, and, for a
data question, by running Ask's SQL and a reference query through the Explorer
(POST /api/v1/explorer/query, the person's own rights) and comparing the rows.

It runs by hand, never in CI: every question is a real model call, paid from the
signed-in person's assistant quota, and each data question's generate_sql counts as
one NL→SQL query on the plan. CI runs it against a stub instead
(llm-service/tests/test_assistant_accuracy_eval.py). docs/runbook.md § "Ask rsync.ai
accuracy eval" says how to run it. The environment is assistant_eval.py's:

  RSYNC_EVAL_TOKEN          a member's session token (required)
  RSYNC_EVAL_WORKSPACE_ID   the workspace it asks in (required)
  RSYNC_EVAL_BASE_URL       default https://app.rsync.ai

What it writes, to --out:
  report.json, report.md  buckets per kind and model, reasons, expected names and
                          counts read from the API; never an answer's text, a
                          query's rows or a value read from a table
  answers.jsonl           each answer's text and the SQL Ask drafted, for the review;
                          it can hold values a sample_rows call read, so it stays on
                          the machine that ran the eval
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import re
import sys
import time
import urllib.parse
import uuid
from collections import Counter
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_QUESTIONS = os.path.join(HERE, "assistant_accuracy_questions.json")


def _load_base():
    # scripts/ is not a package: load the token eval by path for its client.
    spec = importlib.util.spec_from_file_location("assistant_eval", os.path.join(HERE, "assistant_eval.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


base = _load_base()
Client = base.Client
EvalError = base.EvalError
QuotaExceeded = base.QuotaExceeded
EntryUnavailable = base.EntryUnavailable

KINDS = ("workspace", "diagnose", "howto", "data", "lineage", "account", "cannot", "offtopic")
BUCKETS = ("right", "honest", "wrong", "review", "error")
EXPECT_TYPES = ("count", "names", "name", "status", "run", "facts", "page", "decline", "sql")
EXPLORER_LIMIT = 500

# A question Ask could not answer for a busy minute is asked again after a wait: a 429
# is our own chat limit (20 a minute a person), and a 502 is most often the model
# provider out of capacity (Vertex's RESOURCE_EXHAUSTED, which llm-service reports
# after its own waits; 13 of the 14 errors in the 2026-10-11 eval). app.rsync.ai is
# behind Cloudflare, which swaps a 502's JSON for its own page, so a 502 with no
# error code is asked again too; one that says the model service is down is not.
# Each re-ask is counted in the report and costs a question from the quota.
REASK_WAITS = (30.0, 60.0)
RETRY_AFTER_MOST = 60.0
_sleep = time.sleep


def reask_wait(status: int, body: object, asked_again: int) -> float | None:
    """Seconds to wait before asking a question again after (status, body), or None."""
    if asked_again >= len(REASK_WAITS):
        return None
    code = base._error_code(body)
    if status == 429 and code == "rate_limit_exceeded":
        after = body.get("retry_after") if isinstance(body, dict) else None
        if isinstance(after, (int, float)) and not isinstance(after, bool):
            return float(after) if 0 < after <= RETRY_AFTER_MOST else None
        return REASK_WAITS[asked_again]
    if status == 502 and code in ("", "provider_error"):
        return REASK_WAITS[asked_again]
    return None

# ---- reading an answer ---------------------------------------------------------------

HONEST = re.compile(
    r"\b(could not|couldn't|can't|cannot|can not|unable to|wasn't able to|was not able to|did not|didn't)"
    r" (find|see|read|locate|access|get|determine|tell)\b"
    r"|\b(no|don't have|do not have) (information|access|data|record)s?\b"
    r"|\bnot (available|visible|shown) (to me|in (what|the tools))",
    re.I,
)
NUMBER_WORDS = {
    "zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
    "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
    "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20,
}
ZERO_WORDS = ("no", "none", "zero", "never", "not any")
_NUM = r"\d{1,3}(?:,\d{3})+|\d+|" + "|".join(NUMBER_WORDS)
_NUM_RE = re.compile(rf"(?<![\w.,/-])({_NUM})(?![\w,]\d|\.\d|[\w-])", re.I)

# Each derived_status a pipeline reports (pipelines.go pipelineDerivedStatusCaseSQL),
# and the words an answer may use for it.
STATUS_WORDS = {
    "failed": ("failed", "failing", "failure", "errored"),
    "passed": ("passed", "completed", "succeeded", "successful", "success", "healthy", "finished"),
    "running": ("running", "in progress", "syncing", "streaming"),
    "paused": ("paused",),
    "stopped": ("stopped", "cancelled", "canceled"),
    "idle": ("idle", "not running", "never run", "has not run", "hasn't run", "not started"),
    "scheduled": ("scheduled",),
    "waiting_for_user": ("waiting for", "waiting on", "needs your", "needs input", "awaiting"),
}
_STATUS_ALIASES = {"completed": "passed", "succeeded": "passed", "success": "passed", "error": "failed",
                   "cancelled": "stopped", "canceled": "stopped"}


def _numbers(text: str) -> list[int]:
    out = []
    for m in _NUM_RE.finditer(text):
        tok = m.group(1).lower()
        out.append(NUMBER_WORDS[tok] if tok in NUMBER_WORDS else int(tok.replace(",", "")))
    return out


def _noun_numbers(text: str, noun: str) -> list[int]:
    """Numbers an answer ties to the noun: "3 pipelines", "three failed pipelines"."""
    out = []
    for m in re.finditer(rf"(?<![\w.,/-])({_NUM})\s+(?:[\w-]+\s+){{0,2}}?(?:{noun})\b", text, re.I):
        tok = m.group(1).lower()
        out.append(NUMBER_WORDS[tok] if tok in NUMBER_WORDS else int(tok.replace(",", "")))
    return out


def _says_zero(text: str, noun: str) -> bool:
    words = "|".join(re.escape(w) for w in ZERO_WORDS)
    return bool(re.search(rf"\b({words})\b\s+(?:[\w-]+\s+){{0,2}}?(?:{noun})\b", text, re.I)
                or re.search(rf"\b(there are|you have|has|have)\s+(no|none|zero)\b", text, re.I)
                or re.search(r"\bnever\b", text, re.I))


def _honest_lead(text: str) -> bool:
    """The answer opens by saying it could not find the thing: what follows is context
    ("I could not find your plan's limit. You have 3 pipelines."), not the answer."""
    first = re.split(r"(?<=[.!?])\s|\n", text.strip(), maxsplit=1)[0]
    return HONEST.search(first) is not None


def _has(text: str, phrase: str) -> bool:
    return re.search(rf"(?<![\w]){re.escape(phrase)}(?![\w])", text, re.I) is not None


def _unecho(answer: str, echo: list[str], keep: list[str]) -> str:
    """The answer less the names the question gave it: "orders sync writes fintech.payments"
    copies the pipeline's name from the question, which does not name the table orders."""
    keep_l = {k.lower() for k in keep}
    for e in sorted((e for e in echo if e and e.lower() not in keep_l), key=len, reverse=True):
        answer = re.sub(rf"(?<![\w]){re.escape(e)}(?![\w])", " ", answer, flags=re.I)
    return answer


def grade_count(answer: str, expected: int, noun: str) -> tuple[str, str]:
    tied = _noun_numbers(answer, noun)
    if expected == 0 and _says_zero(answer, noun) and not [n for n in tied if n != 0]:
        return "right", ""
    found = _numbers(answer)
    if expected in (tied or found):
        return "right", ""
    if _honest_lead(answer):
        return "honest", ""
    if tied:
        return "wrong", f"says {tied[0]}, the API says {expected}"
    if found:
        return "wrong", f"says {found[0]}, the API says {expected}"
    if HONEST.search(answer):
        return "honest", ""
    return "review", "no number in the answer"


UNLIMITED = "unlimited"


def grade_unlimited(answer: str) -> tuple[str, str]:
    """A limit the plan does not set (the API's null)."""
    if re.search(r"\bunlimited\b|\bno (\w+ )?(limit|cap|maximum)\b|\bas many\b|\bnot limited\b", answer, re.I):
        return "right", ""
    if _honest_lead(answer):
        return "honest", ""
    found = _numbers(answer)
    if found:
        return "wrong", f"says {found[0]}, the plan sets no limit"
    if HONEST.search(answer):
        return "honest", ""
    return "review", "does not say unlimited"


def grade_names(answer: str, expected: list[str], most: int, aliases: dict | None = None) -> tuple[str, str]:
    """aliases: other names that identify the same thing (a table, or the model that writes it)."""
    if not expected:
        if HONEST.search(answer) or _says_zero(answer, r"\w+"):
            return "right", ""
        return "review", "the API lists none; the answer does not say none"
    want = expected[:most]
    aliases = aliases or {}
    present = [n for n in want if any(_has(answer, a) for a in (n, *aliases.get(n, ())))]
    if len(present) == len(want):
        return "right", ""
    if present:
        return "review", f"names {len(present)} of {len(want)}"
    if HONEST.search(answer):
        return "honest", ""
    return "wrong", f"names none of the {len(want)} the API lists"


def _status_key(status: str) -> str:
    s = status.strip().lower()
    return _STATUS_ALIASES.get(s, s)


def grade_status(answer: str, expected: str) -> tuple[str, str]:
    key = _status_key(expected)
    words = STATUS_WORDS.get(key, (key,))
    if any(_has(answer, w) for w in words):
        return "right", ""
    if _honest_lead(answer):
        return "honest", ""
    others = [k for k, ws in STATUS_WORDS.items() if k != key and any(_has(answer, w) for w in ws)]
    if others:
        return "wrong", f"says {others[0]}, the API says {key}"
    if HONEST.search(answer):
        return "honest", ""
    return "review", "no status word in the answer"


# "Nothing says whether rsync.ai is open source": a claim the answer only names, not makes.
_NAMED_NOT_MADE = re.compile(r"\b(whether|if)(\s+or\s+not)?\s+$", re.I)


def _claims(pat: str, answer: str) -> bool:
    return any(not _NAMED_NOT_MADE.search(answer[max(0, m.start() - 20):m.start()])
               for m in re.finditer(pat, answer, re.I | re.M))


def grade_facts(answer: str, expect: dict) -> tuple[str, str]:
    for pat in expect.get("must_not", []):
        if _claims(pat, answer):
            return "wrong", f"says what the docs contradict: /{pat}/"
    needed = list(expect.get("all", [])) + ["|".join(group) for group in expect.get("any", [])]
    hit = [p for p in needed if re.search(p, answer, re.I | re.M)]
    if len(hit) == len(needed):
        return "right", ""
    if hit:
        return "review", f"{len(hit)} of {len(needed)} facts"
    if HONEST.search(answer):
        return "honest", ""
    return "wrong", f"none of the {len(needed)} facts"


CLAIMS_DONE = re.compile(
    r"\bI(?:'ve| have)? (?:now |just |successfully )?(deleted|paused|stopped|created|added|invited|removed|"
    r"turned on|enabled|disabled|changed|updated|renamed)\b|\b(has|have) been (deleted|paused|stopped|added|"
    r"invited|removed|turned on|enabled|changed|updated)\b",
    re.I,
)


def grade_page(answer: str, expect: dict) -> tuple[str, str]:
    if CLAIMS_DONE.search(answer):
        return "wrong", "says it did something it cannot do"
    for pat in expect.get("must_not", []):
        if re.search(pat, answer, re.I | re.M):
            return "wrong", f"/{pat}/"
    if any(re.search(rf"(?<![\w/]){re.escape(p)}(?![\w-])", answer) for p in expect["pages"]):
        return "right", ""
    return "review", "does not name " + " or ".join(expect["pages"])


DECLINE = re.compile(
    r"\b(I can only|I only|only help|can help (you )?with|I'm here to help with|I am here to help with|"
    r"outside (of )?(what|my)|not something I|can't help|cannot help|can not help|isn't something|is not something|"
    r"not related to|unrelated to|I'm the rsync\.ai assistant|I am the rsync\.ai assistant|"
    r"questions about (your )?(rsync\.ai|workspace))",
    re.I,
)


def grade_decline(answer: str, expect: dict) -> tuple[str, str]:
    for pat in expect.get("must_not", []):
        if re.search(pat, answer, re.I | re.M):
            return "wrong", f"answered off-topic: /{pat}/"
    if DECLINE.search(answer):
        return "right", ""
    return "review", "no decline or redirect phrase"


# ---- data questions -------------------------------------------------------------------

_WRITE_WORDS = ("insert", "update", "delete", "merge", "drop", "truncate", "alter", "create", "grant", "revoke",
                "call", "exec", "execute", "copy", "into", "replace", "upsert", "attach", "detach", "vacuum",
                "analyze", "lock", "set", "use", "begin", "commit", "rollback", "load", "export", "unload")


def _strip_sql(sql: str) -> str:
    """The SQL with comments, string literals and quoted identifiers blanked."""
    out, i, n = [], 0, len(sql)
    while i < n:
        c = sql[i]
        if sql.startswith("--", i) or c == "#":
            j = sql.find("\n", i)
            i = n if j == -1 else j
            out.append(" ")
        elif sql.startswith("/*", i):
            j = sql.find("*/", i + 2)
            i = n if j == -1 else j + 2
            out.append(" ")
        elif c in "'\"`[":
            close = "]" if c == "[" else c
            j = i + 1
            while j < n:
                if sql[j] == close:
                    if j + 1 < n and sql[j + 1] == close and close != "]":
                        j += 2
                        continue
                    break
                if sql[j] == "\\" and close != "]":
                    j += 1
                j += 1
            out.append(" x ")
            i = j + 1
        else:
            out.append(c)
            i += 1
    return "".join(out)


def read_only_sql(sql: str) -> str:
    """Why the eval will not run this SQL, or "" when it is one read.

    The Explorer checks the person's role too, but an owner may run the eval: the
    eval refuses anything but one SELECT or WITH on its own side."""
    body = _strip_sql(sql).strip().rstrip(";").strip()
    if not body:
        return "empty"
    if ";" in body:
        return "more than one statement"
    first = re.match(r"\(*\s*([A-Za-z]+)", body)
    if not first or first.group(1).lower() not in ("select", "with"):
        return "not a SELECT or WITH"
    for m in re.finditer(r"\b([A-Za-z_]+)\b(\s*\()?", body):
        w = m.group(1).lower()
        if w in _WRITE_WORDS and not m.group(2):
            return f"contains {w.upper()}"
    return ""


def _canon(v) -> str:
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float, Decimal)) or (isinstance(v, str) and re.fullmatch(r"\s*-?\d+(\.\d+)?([eE][-+]?\d+)?\s*", v)):
        try:
            d = Decimal(str(v).strip())
        except InvalidOperation:
            return str(v)
        if d == d.to_integral_value():
            return str(d.quantize(Decimal(1)))
        return format(d.normalize(), "f") if abs(d.adjusted()) < 6 else f"{float(d):.6g}"
    if isinstance(v, (dict, list)):
        return json.dumps(v, sort_keys=True, default=str)
    s = str(v).strip()
    m = re.fullmatch(r"(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(\.\d+)?(Z|\+00:?00| UTC)?", s)
    if m:
        frac = (m.group(3) or "").rstrip("0").rstrip(".")
        return f"{m.group(1)} {m.group(2)}{frac}"
    return s


def _rows(result: dict) -> list[Counter]:
    cols = result.get("columns") or []
    out = []
    for row in result.get("rows") or []:
        if isinstance(row, dict):
            values = [row.get(c) for c in cols] if cols else list(row.values())
        else:
            values = list(row)
        out.append(Counter(_canon(v) for v in values))
    return out


def same_rows(reference: dict, answer: dict) -> bool:
    """Every reference row is inside its own answer row (extra columns allowed),
    as many rows as the reference, in any order."""
    ref, got = _rows(reference), _rows(answer)
    if len(ref) != len(got):
        return False
    unused = list(range(len(got)))
    for r in sorted(ref, key=lambda c: -sum(c.values())):
        match = next((i for i in unused if not (r - got[i])), None)
        if match is None:
            return False
        unused.remove(match)
    return True


def run_sql(client: Client, connection_id: str, sql: str) -> tuple[str, dict | None]:
    """("", result) or (why it did not run, None)."""
    refused = read_only_sql(sql)
    if refused:
        return f"the eval would not run it: {refused}", None
    status, body = client.post("/api/v1/explorer/query",
                               {"connection_id": connection_id, "sql": sql, "limit": EXPLORER_LIMIT})
    if status != 200 or not isinstance(body, dict):
        return f"the Explorer refused it: HTTP {status}", None
    return "", body


def _drafts(body: dict) -> list[dict]:
    out = []
    for la in body.get("looked_at") or []:
        if isinstance(la, dict) and la.get("tool") == "generate_sql" and la.get("ok") and isinstance(la.get("sql"), dict):
            if la["sql"].get("sql") and la["sql"].get("connection_id"):
                out.append(la["sql"])
    return out


def grade_sql(client: Client, body: dict, answer: str, question: str, reference: dict | None) -> tuple[str, str]:
    """reference: {"connection_id", "result"} from the reference query, run when the
    eval read the workspace."""
    if not reference or reference.get("result") is None:
        return "review", "no reference result (the reference query did not run)"
    drafts = _drafts(body)
    if not drafts:
        if HONEST.search(answer):
            return "honest", "drafted no SQL"
        return "wrong", "drafted no SQL"
    sql = drafts[-1]
    if sql["connection_id"] != reference["connection_id"]:
        return "review", "Ask queried another connection than the reference query"
    why, result = run_sql(client, sql["connection_id"], sql["sql"])
    if result is None:
        return "wrong", "Ask's SQL did not run: " + why
    ref = reference["result"]
    if result.get("truncated") or ref.get("truncated"):
        return "review", f"more than {EXPLORER_LIMIT} rows; not compared"
    if not same_rows(ref, result):
        return "wrong", "Ask's SQL returns a different result from the reference query"
    seen = set(_numbers(question)) | set(_numbers(sql["sql"]))
    claimed = [n for n in _numbers(answer) if n >= 10 and n not in seen]
    if claimed:
        return "review", "the SQL is right; the answer states a number no tool showed it"
    return "right", ""


# ---- one answer -----------------------------------------------------------------------


def grade(q: dict, status: int, body: object, truth: dict, write_tools: frozenset[str],
          client: Client, question: str, echo: list[str] = (), fallbacks: tuple[str, ...] = ()) -> dict:
    """fallbacks: how Ask's answer opens when the model wrote none (loop.go finalAnswer).
    That is Ask failing, an error, not a wrong answer."""
    run = {"bucket": "error", "reason": "", "tools": [], "calls": 0}
    if status != 200 or not isinstance(body, dict):
        run["reason"] = f"HTTP {status} {base._error_code(body)}".rstrip()
        return run
    answer = body.get("answer") or ""
    looked = [la for la in body.get("looked_at") or [] if isinstance(la, dict)]
    run["tools"] = [la.get("tool", "") for la in looked]
    run["calls"] = len(looked)
    run["stopped"] = body.get("stopped") or ""
    if not answer.strip():
        run["reason"] = "empty answer"
        return run
    lead = next((f for f in fallbacks if answer.lstrip().startswith(f)), "")
    if lead:
        run["reason"] = "Ask's fallback: " + lead
        return run
    wrote = sorted({t for t in run["tools"] if t in write_tools})
    if wrote:
        run["bucket"], run["reason"] = "wrong", "called a write tool: " + ", ".join(wrote)
        return run
    expect, kind = q["expect"], q["expect"]["type"]
    want = truth.get(expect["truth"]) if "truth" in expect else None
    if "truth" in expect and want is None:
        run["bucket"], run["reason"] = "review", f"the API gave no {expect['truth']}"
        return run
    named = [str(w) for w in want] if isinstance(want, list) else [str(want)] if want is not None else []
    aliases = truth.get("lineage_aliases") or {}
    answer = _unecho(answer, list(echo), named + [a for n in named for a in aliases.get(n, ())])
    if kind == "count" and want == UNLIMITED:
        bucket, reason = grade_unlimited(answer)
    elif kind == "count":
        bucket, reason = grade_count(answer, int(want), expect.get("noun", r"\w+"))
    elif kind == "names":
        bucket, reason = grade_names(answer, list(want), int(expect.get("most", 10)), aliases)
    elif kind == "name":
        bucket, reason = grade_names(answer, [str(want)], 1)
    elif kind == "status":
        bucket, reason = grade_status(answer, str(want))
    elif kind == "run":
        bucket, reason = grade_run(looked, answer, str(want))
    elif kind == "facts":
        bucket, reason = grade_facts(answer, expect)
    elif kind == "page":
        bucket, reason = grade_page(answer, expect)
    elif kind == "decline":
        bucket, reason = grade_decline(answer, expect)
    else:
        bucket, reason = grade_sql(client, body, answer, question, truth.get("reference:" + q["id"]))
    run["bucket"], run["reason"] = bucket, reason
    return run


def grade_run(looked: list[dict], answer: str, run_id: str) -> tuple[str, str]:
    """The answer rests on the run that failed: a run tool read that run."""
    ids = []
    for la in looked:
        if la.get("tool") in ("diagnose_run", "get_run_status") and la.get("ok"):
            args = la.get("arguments") if isinstance(la.get("arguments"), dict) else {}
            ids.append(str(la.get("resource_id") or args.get("run_id") or ""))
    if run_id in ids:
        return "right", ""
    if ids:
        return "wrong", "read a run other than the latest failed one"
    if HONEST.search(answer):
        return "honest", "read no run"
    return "wrong", "read no run, yet answered"


# ---- what the API says ----------------------------------------------------------------

# run_history.go runOutcomeStatuses["failed"]: every status a failed run reports,
# the same fold Ask's list_runs counts by.
FAILED_RUN_STATUSES = ("failed", "error", "credential_check_failed", "silent_drop_detected",
                       "silent_partial_drop_detected")
MAX_PIPELINE_PAGES = 20


def _get(client: Client, path: str) -> dict:
    status, body = client.get(path)
    if status != 200 or not isinstance(body, dict):
        raise EvalError(f"GET {path} answered {status} {base._error_code(body)}".rstrip())
    return body


def _by_name(items) -> list[dict]:
    return sorted((i for i in items or [] if isinstance(i, dict) and i.get("name")),
                  key=lambda i: str(i["name"]).casefold())


def _pipelines(client: Client) -> tuple[list[dict], int]:
    items, total = [], 0
    for page in range(1, MAX_PIPELINE_PAGES + 1):
        body = _get(client, f"/api/v1/pipelines?page={page}&per_page=100")
        items += [p for p in body.get("pipelines") or [] if isinstance(p, dict)]
        pag = body.get("pagination") if isinstance(body.get("pagination"), dict) else {}
        total = int(pag.get("total") or body.get("total") or len(items))
        if not pag.get("has_next"):
            break
    return _by_name(items), total


def _failed_runs(client: Client, pipeline_id: str = "") -> tuple[int, str]:
    """How many runs failed, and the latest failed run's id ("" when none).

    GET /executions filters on one status at a time. Like the Runs page, it also
    repairs run rows a newer run left stale."""
    count, latest, latest_at = 0, "", ""
    for s in FAILED_RUN_STATUSES:
        params = {"status": s, "limit": 1}
        if pipeline_id:
            params["pipeline_id"] = pipeline_id
        body = _get(client, "/api/v1/executions?" + urllib.parse.urlencode(params))
        count += int(body.get("total") or 0)
        for e in body.get("executions") or []:
            at = str(e.get("start_time") or "") if isinstance(e, dict) else ""
            if at and e.get("id") and at >= latest_at:
                latest, latest_at = str(e["id"]), at
    return count, latest


def _nl_sql_used(client: Client) -> int | None:
    used = _get(client, "/api/v1/usage").get("queries_used")
    return None if used is None else int(used)


# Truths that move while the eval runs (a data question's generate_sql counts as one
# NL→SQL query): read again just before the question is asked.
LIVE_TRUTHS = {"nl_sql_used": _nl_sql_used}


def _short(name: str) -> str:
    return name.rsplit(".", 1)[-1]


def lineage(graph: dict) -> tuple[dict, dict]:
    """Slots and truths from GET /explorer/asset-graph: what a pipeline writes, which
    models read a table, what a model reads, and every model downstream of a pipeline.
    A table is expected by its bare name, which a qualified name also contains. A model
    and the table it materializes name the same thing ("Accounts (silver)" builds
    accounts_silver): each is the other's alias in lineage_aliases."""
    nodes = {n["id"]: n for n in graph.get("nodes") or [] if isinstance(n, dict) and n.get("id")}
    out: dict[str, list] = {}
    into: dict[str, list] = {}
    for e in graph.get("edges") or []:
        if isinstance(e, dict) and e.get("from") in nodes and e.get("to") in nodes:
            out.setdefault(e["from"], []).append(e)
            into.setdefault(e["to"], []).append(e)

    def kind(nid):
        return nodes[nid].get("kind")

    def name(nid):
        return str(nodes[nid].get("name") or "")

    def ordered(k):
        return sorted((nid for nid in nodes if kind(nid) == k and name(nid)), key=lambda nid: name(nid).casefold())

    def names(items):
        return sorted(set(items), key=str.casefold)

    def downstream_models(start):
        seen, stack, found = {start}, [start], []
        while stack:
            for e in out.get(stack.pop(), []):
                if e["to"] not in seen:
                    seen.add(e["to"])
                    stack.append(e["to"])
                    if kind(e["to"]) == "model":
                        found.append(name(e["to"]))
        return names(found)

    slots, truth = {}, {}
    writers = []
    for pid in ordered("pipeline"):
        tables = names(_short(name(e["to"])) for e in out.get(pid, []) if e.get("kind") == "writes")
        if tables:
            writers.append((pid, tables, downstream_models(pid)))
    # A pipeline with models downstream, so the impact question has an answer to find.
    pick = next((w for w in writers if w[2]), writers[0] if writers else None)
    if pick:
        slots["lineage_pipeline"] = name(pick[0])
        truth["lineage_pipeline_tables"], truth["lineage_downstream_models"] = pick[1], pick[2]
    for tid in ordered("table"):
        readers = names(name(e["to"]) for e in out.get(tid, []) if e.get("kind") == "reads" and kind(e["to"]) == "model")
        if readers:
            slots["lineage_table"], truth["lineage_table_readers"] = name(tid), readers
            break
    for mid in ordered("model"):
        inputs = names(_short(name(e["from"])) for e in into.get(mid, [])
                       if e.get("kind") == "reads" and kind(e["from"]) == "table")
        if inputs:
            slots["lineage_model"], truth["lineage_model_inputs"] = name(mid), inputs
            break
    pairs: dict[str, set] = {}
    for mid in ordered("model"):
        for e in out.get(mid, []):
            if e.get("kind") == "materializes" and kind(e["to"]) == "table":
                model, table = name(mid), _short(name(e["to"]))
                if model.casefold() != table.casefold():
                    pairs.setdefault(model, set()).add(table)
                    pairs.setdefault(table, set()).add(model)
    wanted = {n for v in truth.values() for n in v}
    aliases = {n: names(pairs[n]) for n in sorted(wanted, key=str.casefold) if n in pairs}
    if aliases:
        truth["lineage_aliases"] = aliases
    return slots, truth


# ---- data questions: a table, a text column and a time column to ask about -------------

TEXT_TYPE = re.compile(r"char|text|string|enum", re.I)
TIME_TYPE = re.compile(r"timestamp|datetime|^date\b", re.I)
ID_LIKE = re.compile(r"(^|_)(id|uuid|guid|key|hash|token|email|phone|password|secret|name)$", re.I)
MAX_DATA_CONNECTIONS = 3
MAX_PROBES = 6
MAX_GROUPS = 50

# The reference query for each data question; columns are aliased c1, c2 because a
# result row is keyed by column name.
REFERENCES = {
    "count": "SELECT COUNT(*) AS c1 FROM {t}",
    "group_count": "SELECT {tc} AS c1, COUNT(*) AS c2 FROM {t} GROUP BY {tc}",
    "distinct": "SELECT COUNT(DISTINCT {tc}) AS c1 FROM {t}",
    "min_max": "SELECT MIN({tm}) AS c1, MAX({tm}) AS c2 FROM {t}",
    "blank_count": "SELECT COUNT(*) AS c1 FROM {t} WHERE {tc} IS NULL OR {tc} = ''",
}


def quote(dialect: str, *parts: str) -> str:
    """An identifier quoted for the connection's sql_dialect (explorer.go SQLDialect)."""
    d = (dialect or "").lower()
    if d in ("mysql", "mariadb", "clickhouse", "databricks", "bigquery"):
        return ".".join("`" + p.replace("`", "``") + "`" for p in parts if p)
    if d in ("sqlserver", "tsql", "mssql"):
        return ".".join("[" + p.replace("]", "]]") + "]" for p in parts if p)
    return ".".join('"' + p.replace('"', '""') + '"' for p in parts if p)


def _first_row(result: dict) -> list:
    rows = result.get("rows") or []
    if not rows:
        return []
    row, cols = rows[0], result.get("columns") or []
    if isinstance(row, dict):
        return [row.get(c) for c in cols] if cols else list(row.values())
    return list(row)


def _int(v) -> int | None:
    try:
        return int(Decimal(str(v)))
    except (InvalidOperation, ValueError):
        return None


def pick_data_table(client: Client, connections: list[dict]) -> dict | None:
    """The smallest table, on the first SQL connections by name, that has a time
    column and a text column with 2 to 50 values (no NULLs preferred, so "how many
    distinct" has one answer). It reads counts only, and keeps them in memory."""
    probes, fallback = 0, None
    for c in [c for c in connections if c.get("supports_explorer") and c.get("sql_dialect")][:MAX_DATA_CONNECTIONS]:
        dialect = str(c["sql_dialect"])
        index = _get(client, f"/api/v1/explorer/connections/{c['id']}/schema-index")
        tables = [t for t in index.get("tables") or [] if isinstance(t, dict) and t.get("name")]
        bare = Counter(str(t["name"]).lower() for t in tables)
        found = []
        for t in tables:
            cols = [col for col in t.get("columns") or [] if isinstance(col, dict) and col.get("name")]
            pk = {str(k).lower() for k in t.get("primary_key") or []}
            pk |= {str(col["name"]).lower() for col in cols if col.get("is_primary_key")}
            texts = [str(col["name"]) for col in cols if TEXT_TYPE.search(str(col.get("type") or ""))
                     and str(col["name"]).lower() not in pk and not ID_LIKE.search(str(col["name"]))]
            times = [str(col["name"]) for col in cols if TIME_TYPE.search(str(col.get("type") or ""))]
            if texts and times:
                found.append((t, texts, times))
        found.sort(key=lambda f: (f[0].get("row_count") is None, f[0].get("row_count") or 0, str(f[0]["name"]).casefold()))
        for t, texts, times in found:
            schema, table = str(t.get("schema") or ""), str(t["name"])
            target = quote(dialect, schema, table)
            for col in texts[:2]:
                if probes == MAX_PROBES:
                    return fallback
                probes += 1
                qc = quote(dialect, col)
                _, res = run_sql(client, c["id"], f"SELECT COUNT(DISTINCT {qc}) AS c1, COUNT(*) AS c2, COUNT({qc}) AS c3 FROM {target}")
                if res is None:
                    continue
                values = [_int(v) for v in _first_row(res)]
                if len(values) != 3 or None in values or not 2 <= values[0] <= MAX_GROUPS:
                    continue
                pick = {
                    "connection_id": str(c["id"]), "dialect": dialect,
                    "slots": {"data_connection": str(c["name"]),
                              "data_table": f"{schema}.{table}" if schema and bare[table.lower()] > 1 else table,
                              "data_text_col": col, "data_time_col": times[0]},
                    "sql": {"t": target, "tc": qc, "tm": quote(dialect, times[0])},
                }
                if values[1] == values[2]:
                    return pick
                fallback = fallback or pick
    return fallback


# ---- reading the workspace --------------------------------------------------------------


def read_workspace(client: Client, questions: list[dict], run_references: bool = True) -> tuple[dict, dict, dict]:
    """(slots, truth, unread): names for the questions' {slots}; each expect.truth and
    each data question's reference result; and what could not be read, by part.

    Every read is a GET, except the Explorer reads (one SELECT each) that pick the
    data table and run the reference queries. A question whose truth was not read
    is skipped, not asked."""
    slots: dict[str, str] = {}
    truth: dict = {}
    unread: dict[str, str] = {}

    pipelines, truth["pipelines_total"] = _pipelines(client)
    failed = [p for p in pipelines if p.get("derived_status") == "failed"]
    truth["pipelines_failed_now"] = len(failed)
    if pipelines:
        slots["pipeline"] = str(pipelines[0]["name"])
        if pipelines[0].get("derived_status"):
            truth["pipeline_status"] = str(pipelines[0]["derived_status"])
    if failed:
        slots["failed_pipeline"] = str(failed[0]["name"])
        truth["failed_pipeline_failed_runs"], run_id = _failed_runs(client, str(failed[0]["id"]))
        if run_id:
            truth["latest_failed_run"] = run_id
    truth["runs_failed"] = _failed_runs(client)[0]

    connections = _by_name(_get(client, "/api/v1/connections").get("connections"))
    truth["connections_total"] = len(connections)
    truth["destination_connection_names"] = [str(c["name"]) for c in connections if c.get("type") == "destination"]
    if connections:
        slots["connection"] = str(connections[0]["name"])

    # Ask's list_models: a saved query is a model when it materializes (models.go listModels).
    saved = _get(client, "/api/v1/explorer/saved").get("saved_queries") or []
    truth["models_total"] = sum(1 for s in saved if isinstance(s, dict)
                                and str(s.get("materialization") or "").strip().lower() not in ("", "none"))
    workflows = _by_name(_get(client, "/api/v1/workflows").get("workflows"))
    truth["workflows_enabled"] = sum(1 for w in workflows if w.get("status") == "active")
    if workflows:
        slots["workflow"] = str(workflows[0]["name"])
    truth["dashboard_names"] = [str(d["name"]) for d in _by_name(_get(client, "/api/v1/dashboards").get("dashboards"))]

    def part(name, read):
        try:
            read()
        except EvalError as e:
            unread[name] = str(e)

    def plan():
        body = _get(client, "/api/v1/usage/plan")
        if body.get("plan"):
            truth["plan_name"] = str(body["plan"])
        limit = body.get("pipelines_limit")
        truth["plan_pipeline_limit"] = UNLIMITED if limit is None or int(limit) < 0 else int(limit)

    def usage():
        used = _nl_sql_used(client)
        if used is not None:
            truth["nl_sql_used"] = used

    def members():
        # Counted only: the list carries e-mail addresses, which the eval never keeps.
        body = _get(client, f"/api/v1/workspaces/{urllib.parse.quote(client.workspace_id)}/members")
        truth["members_total"] = len(body.get("members") or [])

    def graph():
        s, t = lineage(_get(client, "/api/v1/explorer/asset-graph"))
        slots.update(s)
        truth.update(t)

    data: dict = {}

    def data_table():
        pick = pick_data_table(client, connections)
        if pick:
            data.update(pick)
            slots.update(pick["slots"])

    for name, read in (("plan", plan), ("usage", usage), ("members", members), ("lineage", graph), ("data", data_table)):
        part(name, read)

    by_name = {str(c["name"]): c for c in connections}
    for q in questions:
        e = q["expect"]
        if e["type"] != "sql":
            continue
        if e.get("reference_sql"):
            conn = by_name.get(str(e.get("connection")))
            if conn is None:
                unread["reference:" + q["id"]] = f"no connection named {e.get('connection')!r}"
                continue
            cid, sql = str(conn["id"]), e["reference_sql"]
        elif data:
            cid, sql = data["connection_id"], REFERENCES[e["reference"]].format(**data["sql"])
        else:
            continue
        ref = {"connection_id": cid, "sql": sql, "result": None, "why": "not run (--dry-run)"}
        if run_references:
            ref["why"], ref["result"] = run_sql(client, cid, sql)
        truth["reference:" + q["id"]] = ref
    return slots, truth, unread


def public_truth(truth: dict) -> tuple[dict, dict]:
    """What the report may hold: the expected values read from the API, and each
    reference query's SQL and whether it ran, never its rows."""
    expected = {k: v for k, v in truth.items() if not k.startswith("reference:")}
    references = {k.split(":", 1)[1]: {"sql": v["sql"], "ran": v["result"] is not None, "why": v["why"]}
                  for k, v in truth.items() if k.startswith("reference:")}
    return expected, references


# ---- the questions ----------------------------------------------------------------------

TRUTH_TYPES = ("count", "names", "name", "status", "run")


def load_questions(path: str) -> tuple[list[dict], frozenset[str], tuple[str, ...]]:
    """(questions, write_tools, fallback_answers): the last two are Ask's, pinned to the
    gateway by eval_questions_test.go."""
    with open(path) as f:
        doc = json.load(f)
    questions, write_tools = doc.get("questions") or [], frozenset(doc.get("write_tools") or [])
    fallbacks = tuple(f for f in doc.get("fallback_answers") or [] if isinstance(f, str) and f.strip())
    if not write_tools:
        raise EvalError(f"{path}: write_tools is empty")
    if not fallbacks:
        raise EvalError(f"{path}: fallback_answers is empty")
    seen: set[str] = set()
    for q in questions:
        qid = q.get("id")
        if not qid or qid in seen:
            raise EvalError(f"{path}: question id {qid!r} is missing or repeated")
        seen.add(qid)
        e = q.get("expect") or {}
        if q.get("kind") not in KINDS or e.get("type") not in EXPECT_TYPES:
            raise EvalError(f"{path}: {qid}: kind must be one of {KINDS}, expect.type one of {EXPECT_TYPES}")
        if e["type"] in TRUTH_TYPES and not e.get("truth"):
            raise EvalError(f"{path}: {qid}: a {e['type']} question names its expect.truth")
        if e["type"] == "page" and not e.get("pages"):
            raise EvalError(f"{path}: {qid}: a page question lists its pages")
        if e["type"] == "sql":
            if e.get("reference_sql"):
                if not e.get("connection") or read_only_sql(e["reference_sql"]):
                    raise EvalError(f"{path}: {qid}: reference_sql needs a connection name and one SELECT")
            elif e.get("reference") not in REFERENCES:
                raise EvalError(f"{path}: {qid}: reference must be one of {tuple(REFERENCES)}")
        for pat in [*e.get("all", []), *e.get("must_not", []), *[p for g in e.get("any", []) for p in g],
                    *([e["noun"]] if e.get("noun") else [])]:
            try:
                re.compile(pat)
            except re.error as err:
                raise EvalError(f"{path}: {qid}: bad pattern /{pat}/: {err}") from None
    return questions, write_tools, fallbacks


# ---- asking -----------------------------------------------------------------------------


def run_eval(questions: list[dict], write_tools: frozenset[str], client: Client, entries: list[str], runs: int,
             slots: dict, truth: dict, log=print, fallbacks: tuple[str, ...] = ()) -> tuple[dict, list[dict]]:
    """(report, answers): the report holds no answer text; answers does, for the review."""
    expected, references = public_truth(truth)
    report = {
        "started_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "base_url": client.base_url,
        "runs_per_question": runs,
        "slots": slots,
        "expected": expected,
        "references": references,
        "entries": {},
        "stopped": "",
    }
    answers: list[dict] = []
    try:
        for entry in entries:
            out = report["entries"][entry] = {"model": "", "questions": []}
            try:
                _ask_entry(entry, out, questions, write_tools, client, slots, truth, runs, answers, log, fallbacks)
            except EntryUnavailable:
                out["error"] = "model_not_configured: the entry lost its key during the run"
                log(f"  {entry}: {out['error']}")
    except QuotaExceeded:
        report["stopped"] = "quota_exceeded: the person's assistant quota is spent; the report holds the runs before it"
        log("  " + report["stopped"])
    report["expected"] = public_truth(truth)[0]  # live truths, as last read
    for out in report["entries"].values():
        out["summary"] = summarize(out["questions"])
    return report, answers


def _skip_reason(q: dict, text: str | None, missing: list[str], truth: dict) -> str:
    if text is None:
        return "nothing to fill " + ", ".join("{" + m + "}" for m in missing)
    need = q["expect"].get("truth")
    if need and need not in truth and need not in LIVE_TRUTHS:
        return f"the API gave no {need}"
    if q["expect"]["type"] == "sql":
        ref = truth.get("reference:" + q["id"])
        if not ref:
            return "no table to ask about"
        if ref["result"] is None:
            return "the reference query did not run: " + ref["why"]
    return ""


def _ask_entry(entry: str, out: dict, questions: list[dict], write_tools: frozenset[str], client: Client,
               slots: dict, truth: dict, runs: int, answers: list[dict], log, fallbacks: tuple[str, ...]) -> None:
    for q in questions:
        text, missing = base.fill(q["question"], slots)
        rec = {"id": q["id"], "kind": q["kind"], "type": q["expect"]["type"], "question": text or q["question"],
               "runs": []}
        out["questions"].append(rec)
        skip = _skip_reason(q, text, missing, truth)
        if skip:
            rec["skipped"] = skip
            log(f"  {entry} {q['id']} skipped: {skip}")
            continue
        need = q["expect"].get("truth")
        echo = [str(slots[k]) for k in re.findall(r"\{(\w+)\}", q["question"]) if k in slots]
        for n in range(runs):
            if need in LIVE_TRUTHS:
                try:
                    value = LIVE_TRUTHS[need](client)
                except EvalError:
                    value = None
                if value is not None:
                    truth[need] = value
            asked_again = 0
            while True:
                t0 = time.monotonic()
                status, body = client.post("/api/v1/assistant/messages", {
                    "question": text, "model": entry, "request_id": uuid.uuid4().hex,
                })
                latency = time.monotonic() - t0
                wait = reask_wait(status, body, asked_again)
                if wait is None:
                    break
                asked_again += 1
                log(f"  {entry} {q['id']} run {n + 1}: HTTP {status} {base._error_code(body)}".rstrip()
                    + f", asking again in {wait:g}s")
                _sleep(wait)
            if status == 402:
                raise QuotaExceeded()
            if status == 409 and base._error_code(body) == "model_not_configured":
                raise EntryUnavailable()
            run = grade(q, status, body, truth, write_tools, client, text, echo, fallbacks)
            run["latency_s"] = round(latency, 2)
            run["asked_again"] = asked_again
            run["cost_cents"] = int(body.get("cost_cents") or 0) if isinstance(body, dict) else 0
            if isinstance(body, dict) and isinstance(body.get("model"), dict) and not out["model"]:
                out["model"] = str(body["model"].get("model") or "")
            rec["runs"].append(run)
            answers.append({
                "entry": entry, "id": q["id"], "run": n + 1, "question": text,
                "bucket": run["bucket"], "reason": run["reason"], "tools": run["tools"],
                "answer": body.get("answer") if isinstance(body, dict) else None,
                "sql": [d["sql"] for d in _drafts(body)] if isinstance(body, dict) else [],
            })
            note = f": {run['reason']}" if run["reason"] else ""
            log(f"  {entry} {q['id']} run {n + 1}: {run['bucket']}{note} ({run['calls']} tool calls, {run['latency_s']}s)")


def summarize(questions: list[dict]) -> dict:
    by_kind: dict[str, dict] = {}
    total = dict.fromkeys(BUCKETS, 0)
    listed: dict[str, list] = {"wrong": [], "review": [], "error": []}
    for q in questions:
        counts = by_kind.setdefault(q["kind"], dict.fromkeys(BUCKETS, 0))
        for n, r in enumerate(q["runs"], 1):
            counts[r["bucket"]] += 1
            total[r["bucket"]] += 1
            if r["bucket"] in listed:
                listed[r["bucket"]].append({"id": q["id"], "run": n, "reason": r["reason"]})
    return {
        "asked": sum(total.values()),
        "total": total,
        "by_kind": {k: by_kind[k] for k in KINDS if k in by_kind},
        **listed,
        "skipped": [{"id": q["id"], "why": q["skipped"]} for q in questions if q.get("skipped")],
        "cost_cents": sum(r.get("cost_cents", 0) for q in questions for r in q["runs"]),
        "asked_again": sum(r.get("asked_again", 0) for q in questions for r in q["runs"]),
    }


def check_bar(report: dict, baseline: dict | None) -> dict:
    """The bar, per entry: no wrong answer and no error. Against a baseline that asked the same
    questions, each kind's change in right and wrong answers."""
    bar = {}
    for entry, out in report["entries"].items():
        s = out["summary"]
        t = s["total"]
        b: dict = {"met": t["wrong"] == 0 and t["error"] == 0 and s["asked"] > 0, "wrong": t["wrong"], "error": t["error"]}
        before = ((baseline or {}).get("entries") or {}).get(entry, {}).get("summary")
        if baseline is not None and baseline.get("questions_sha256") != report.get("questions_sha256"):
            b["baseline"] = "the baseline asked other questions: not compared"
        elif before:
            b["change"] = {k: {bk: c[bk] - before.get("by_kind", {}).get(k, {}).get(bk, 0) for bk in ("right", "wrong", "error")}
                           for k, c in s["by_kind"].items()}
        bar[entry] = b
    return bar


def markdown(report: dict) -> str:
    lines = [
        "# Ask rsync.ai accuracy eval",
        "",
        f"{report['started_at']} · {report['base_url']} · {report['runs_per_question']} run(s) per question"
        f" · questions sha256 {str(report.get('questions_sha256', ''))[:12]}",
        "",
        "The bar is no wrong answer and no error. Answers are in answers.jsonl, on this machine only.",
    ]
    if report["stopped"]:
        lines += ["", f"**Stopped:** {report['stopped']}"]
    for entry, out in report["entries"].items():
        s, bar = out["summary"], report.get("bar", {}).get(entry, {})
        met = "met" if bar.get("met") else "NOT met"
        lines += ["", f"## {entry}" + (f" ({out['model']})" if out["model"] else ""), "",
                  f"Bar: {met} · {s['asked']} answers · {s['cost_cents']}¢", ""]
        if s.get("asked_again"):
            lines += [f"Asked again {s['asked_again']} time(s) after an HTTP 429 or 502 (a busy model "
                      "provider or the chat limit); each answer is graded on its last try.", ""]
        if out.get("error"):
            lines += [f"**{out['error']}**", ""]
        lines += ["| kind | " + " | ".join(BUCKETS) + " |", "|---|" + "---:|" * len(BUCKETS)]
        for k, c in s["by_kind"].items():
            lines.append(f"| {k} | " + " | ".join(str(c[b]) for b in BUCKETS) + " |")
        lines.append("| **all** | " + " | ".join(f"**{s['total'][b]}**" for b in BUCKETS) + " |")
        for bucket, title in (("wrong", "Wrong"), ("review", "To review (read the answer in answers.jsonl)"),
                              ("error", "Errors (Ask failed; not graded)")):
            if s[bucket]:
                lines += ["", f"{title}:", ""] + [f"- {r['id']} run {r['run']}: {r['reason']}" for r in s[bucket]]
        if s["skipped"]:
            lines += ["", "Skipped:", ""] + [f"- {r['id']}: {r['why']}" for r in s["skipped"]]
        if bar.get("baseline"):
            lines += ["", bar["baseline"]]
        elif bar.get("change"):
            lines += ["", "Change from the baseline:", ""]
            lines += [f"- {k}: right {c['right']:+d}, wrong {c['wrong']:+d}, error {c['error']:+d}"
                      for k, c in bar["change"].items()]
    return "\n".join(lines) + "\n"


# ---- main ---------------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Ask rsync.ai's accuracy eval (docs/runbook.md § Ask rsync.ai accuracy eval)")
    ap.add_argument("--out", help="directory for report.json, report.md and answers.jsonl "
                                  "(default ./assistant-accuracy-<time>)")
    ap.add_argument("--baseline", help="an earlier report.json to compare against")
    ap.add_argument("--runs", type=int, default=1, help="times each question is asked (default 1)")
    ap.add_argument("--entries", help="comma-separated model entries (default every entry with a key)")
    ap.add_argument("--only", help="comma-separated question ids")
    ap.add_argument("--questions", default=DEFAULT_QUESTIONS,
                    help="a question file (default scripts/assistant_accuracy_questions.json)")
    ap.add_argument("--dry-run", action="store_true",
                    help="read the workspace and print each filled question and reference query; ask nothing")
    args = ap.parse_args(argv)
    tokens: list[str] = []
    try:
        if args.runs < 1:
            raise EvalError("--runs must be at least 1")
        base_url = base.check_base_url(base._env("RSYNC_EVAL_BASE_URL", required=False) or base.DEFAULT_BASE_URL)
        token = base._env("RSYNC_EVAL_TOKEN")
        tokens.append(token)
        client = Client(base_url, token, base._env("RSYNC_EVAL_WORKSPACE_ID"))

        questions, write_tools, fallbacks = load_questions(args.questions)
        if args.only:
            wanted = {s.strip() for s in args.only.split(",") if s.strip()}
            unknown = wanted - {q["id"] for q in questions}
            if unknown:
                raise EvalError("--only names no such question: " + ", ".join(sorted(unknown)))
            questions = [q for q in questions if q["id"] in wanted]

        slots, truth, unread = read_workspace(client, questions, run_references=not args.dry_run)
        if args.dry_run:
            for q in questions:
                text, missing = base.fill(q["question"], slots)
                ref = truth.get("reference:" + q["id"])
                skip = "" if ref and text else _skip_reason(q, text, missing, truth)
                print(f"{q['id']} [{q['kind']}] " + (f"skipped: {skip}" if skip else text))
                if ref and text:
                    print(f"    reference: {ref['sql']}")
            for name, why in unread.items():
                print(f"not read: {name}: {_redact(why, tokens)}")
            return 0

        status, body = client.get("/api/v1/assistant/models")
        if status != 200 or not isinstance(body, dict):
            raise EvalError(f"GET /api/v1/assistant/models answered {status} {base._error_code(body)}".rstrip())
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
        report, answers = run_eval(questions, write_tools, client, entries, args.runs, slots, truth,
                                   fallbacks=fallbacks)
        report["unread"] = unread
        with open(args.questions, "rb") as f:
            report["questions_sha256"] = hashlib.sha256(f.read()).hexdigest()
        report["bar"] = check_bar(report, baseline)

        out_dir = args.out or f"assistant-accuracy-{datetime.now().strftime('%Y%m%d-%H%M%S')}"
        os.makedirs(out_dir, exist_ok=True)
        text_md = _redact(markdown(report), tokens)
        files = {
            "report.json": _redact(json.dumps(report, indent=2), tokens),
            "report.md": text_md,
            "answers.jsonl": _redact("".join(json.dumps(a) + "\n" for a in answers), tokens),
        }
        for name, text in files.items():
            with open(os.path.join(out_dir, name), "w") as f:
                f.write(text)
        print(text_md)
        print(f"Wrote report.json, report.md and answers.jsonl to {out_dir}")
        if report["stopped"]:
            return 1
        return 0 if all(b["met"] for b in report["bar"].values()) else 1
    except EvalError as e:
        print("assistant_accuracy_eval: " + _redact(str(e), tokens), file=sys.stderr)
        return 2


_redact = base._redact

if __name__ == "__main__":
    sys.exit(main())
