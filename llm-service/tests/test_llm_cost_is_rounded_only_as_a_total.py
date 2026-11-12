"""An LLM call's cost is kept exactly; only a total is rounded.

Bug class: a cost rounded up to a whole cent per call before it is totalled.
``compute_cost_cents`` rounded every call up, so a 10-token gpt-4o-mini call
that cost 0.00045¢ was charged 1¢ -- against the user's monthly quota, in the
``llm_usage_events`` ledger and on the Prometheus counter -- and a month of
small calls ran a $50 quota out at a fraction of that spend. Prices are whole
cents per million tokens, so six decimal places hold every call's cost exactly
(migration 149 stores NUMERIC(14,6)).

The api-gateway siblings are guarded in Go: the assistant sums its steps'
exact costs before rounding the answer's (internal/assistant/loop.go), and the
quota pre-check reads the month's exact total rounded down, which against a
whole-cent quota is exact (internal/handlers/llm_quota.go; llm_quota_pg_test.go
runs migration 149 on a real Postgres).
"""

from __future__ import annotations

import importlib
import json
import queue
import sys
from decimal import Decimal
from types import SimpleNamespace

USER = "11111111-2222-4333-8444-555555555555"
# 10 prompt tokens at 15¢ and 5 completion tokens at 60¢ per million.
SMALL = dict(provider="openai", model="gpt-4o-mini", prompt_tokens=10, completion_tokens=5)
SMALL_CENTS = Decimal("0.00045")


def _mod(name):
    """Looked up when a test runs: test_explorer_endpoints.py purges every
    ``src.*`` module at collection."""
    return importlib.import_module(name)


def _cost(provider, model, prompt_tokens, completion_tokens):
    return _mod("src.utils.llm_cost").compute_cost_cents(provider, model, prompt_tokens, completion_tokens)


def test_a_small_call_costs_a_fraction_of_a_cent():
    assert _cost(**SMALL) == SMALL_CENTS


def test_small_calls_add_up_to_their_exact_total():
    assert sum(_cost(**SMALL) for _ in range(1000)) == Decimal("0.45")


def test_every_price_is_exact_to_six_decimal_places():
    # One token of each side of every listed price: the smallest cost a call
    # can have, which NUMERIC(14,6) must hold without rounding.
    llm_cost = _mod("src.utils.llm_cost")
    for (provider, model), (in_rate, out_rate) in llm_cost._PRICE_TABLE.items():
        for tokens, rate in (((1, 0), in_rate), ((0, 1), out_rate)):
            cents = _cost(provider, model, *tokens)
            assert cents == Decimal(rate) / 1_000_000, (provider, model)
            assert cents == cents.quantize(Decimal("0.000001")), (provider, model)


def test_a_free_model_and_an_empty_call_cost_nothing():
    assert _cost("ollama", "llama3", 1_000, 1_000) == 0
    assert _cost("openai", "gpt-4o-mini", 0, 0) == 0


def test_record_usage_queues_the_exact_cost_and_returns_it_as_a_json_number(monkeypatch):
    llm_cost = _mod("src.utils.llm_cost")
    q: "queue.Queue[dict]" = queue.Queue()
    monkeypatch.setattr(llm_cost, "_event_queue", q)
    monkeypatch.setattr(llm_cost, "_ensure_writer", lambda: None)

    returned = llm_cost.record_usage(user_id=USER, pipeline_id=None, prompt_name="chat/help_response", **SMALL)

    assert q.get_nowait()["cost_usd_cents"] == SMALL_CENTS
    # The assistant step and /v1/completion put it in a JSON body.
    assert json.loads(json.dumps({"cost_cents": returned})) == {"cost_cents": float(SMALL_CENTS)}


class _Cursor:
    def __init__(self, calls):
        self.calls = calls

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def execute(self, sql, params):
        self.calls.append((sql, params))


def test_the_ledger_row_and_the_quota_are_charged_the_exact_cost(monkeypatch):
    llm_cost = _mod("src.utils.llm_cost")
    calls: list = []
    conn = SimpleNamespace(closed=False, autocommit=False, cursor=lambda: _Cursor(calls))
    monkeypatch.setitem(sys.modules, "psycopg2", SimpleNamespace(connect=lambda _dsn: conn))
    monkeypatch.setenv("DATABASE_URL", "postgresql://unit-test/db")
    q: "queue.Queue[dict]" = queue.Queue()
    monkeypatch.setattr(llm_cost, "_event_queue", q)
    monkeypatch.setattr(llm_cost, "_ensure_writer", lambda: None)
    llm_cost.record_usage(user_id=USER, pipeline_id=None, prompt_name="chat/help_response", **SMALL)
    q.put(None)  # the writer's shutdown sentinel

    llm_cost._writer_loop()

    (_insert, row), (charge, charged) = calls
    assert row[-1] == SMALL_CENTS
    assert charge.startswith("SELECT check_and_charge_user(")
    assert charged == (USER, SMALL_CENTS)


def test_the_prometheus_counter_adds_the_exact_cost():
    metrics = _mod("src.utils.metrics")
    labels = ("unit-test-exact-cost", SMALL["provider"], SMALL["model"])
    counter = metrics.LLM_COST_CENTS_TOTAL.labels(*labels)
    before = counter._value.get()

    metrics.record_llm_call(
        service=labels[0],
        provider=SMALL["provider"],
        model=SMALL["model"],
        result="ok",
        duration_s=0.1,
        prompt_tokens=SMALL["prompt_tokens"],
        completion_tokens=SMALL["completion_tokens"],
    )

    assert abs((counter._value.get() - before) - float(SMALL_CENTS)) < 1e-12
