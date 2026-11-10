#!/usr/bin/env python3
"""Offline unit tests for the generated Upstox source connector.

Every HTTP request is answered in-process by a fake Upstox (``HTTPAdapter.send``
is patched; DNS lookups and socket connects raise), so the tests never reach
Upstox and need no real access token. The profile, holidays and candles below
are made up.
"""

import json
import socket
import time
from datetime import date, datetime, timedelta, timezone
from unittest import mock
from urllib.parse import unquote, urlparse

import pytest
import requests
from requests.adapters import HTTPAdapter

import connector

TOKEN = "dummy-token-for-offline-test"
INSTRUMENT = "NSE_EQ|INE000000000"
CANDLE_SETTINGS = {"instrument_key": INSTRUMENT, "unit": "days", "interval": "1"}

PROFILE = {"status": "success", "data": {
    "email": "someone@example.invalid", "exchanges": ["NSE", "BSE"], "products": ["D"],
    "broker": "UPSTOX", "user_id": "TEST01", "user_name": "Test User",
    "order_types": ["MARKET"], "user_type": "individual", "poa": False, "ddpi": False, "is_active": True}}
HOLIDAYS = {"status": "success", "data": [
    {"date": "2026-01-26", "description": "Made-up holiday A", "holiday_type": "TRADING_HOLIDAY",
     "closed_exchanges": ["NSE", "BSE"], "open_exchanges": []},
    {"date": "2026-03-03", "description": "Made-up holiday B", "holiday_type": "SETTLEMENT_HOLIDAY",
     "closed_exchanges": [], "open_exchanges": [{"exchange": "NSE", "start_time": 1, "end_time": 2}]}]}
CANDLES = {"status": "success", "data": {"candles": [
    ["2026-10-27T00:00:00+05:30", 100.0, 101.5, 99.5, 100.75, 12345, 0],
    ["2026-10-26T00:00:00+05:30", 99.0, 100.5, 98.5, 100.0, 11111, 0],
    ["2026-10-23T00:00:00+05:30", 98.0, 99.5, 97.5, 99.0, 10000, 0]]}}


# --------------------------------------------------------------------------
# Fake Upstox
# --------------------------------------------------------------------------

class _FakeUpstox:
    """Answers api.upstox.com like Upstox does for a bearer token; records
    every request as (host, decoded path, query, whether it sent our token)."""

    def __init__(self):
        self.requests = []

    def send(self, adapter, request, **kwargs):
        u = urlparse(request.url)
        path = unquote(u.path)
        authed = request.headers.get("Authorization") == f"Bearer {TOKEN}"
        self.requests.append((u.hostname, path, u.query, authed))
        if u.hostname != "api.upstox.com":
            status, body = 599, {"error": "offline test: unexpected host"}
        elif not authed:
            status, body = 401, {"status": "error", "errors": [{"errorCode": "UDAPI100050", "message": "Invalid token"}]}
        elif path == "/v2/user/profile":
            status, body = 200, PROFILE
        elif path == "/v2/market/holidays":
            status, body = 200, HOLIDAYS
        elif path.startswith("/v3/historical-candle/"):
            status, body = 200, CANDLES
        else:
            status, body = 404, {"status": "error", "errors": [{"message": "Not found"}]}
        r = requests.Response()
        r.status_code = status
        r._content = json.dumps(body).encode()
        r.headers["Content-Type"] = "application/json"
        r.url = request.url
        r.request = request
        r.connection = adapter
        return r

    def candle_window(self):
        """(to_date, from_date) of the one candle request, from its path."""
        paths = [p for _, p, _, _ in self.requests if p.startswith("/v3/historical-candle/")]
        assert len(paths) == 1, paths
        to_date, from_date = paths[0].rsplit("/", 2)[1:]
        return date.fromisoformat(to_date), date.fromisoformat(from_date)


def _refuse(*a, **k):
    raise OSError(f"offline test: network refused {a!r}")


def _no_dns(*a, **k):
    # gaierror, like a failed lookup: base_connector's SSRF check lets it
    # through to requests, whose send is faked above.
    raise socket.gaierror(f"offline test: no DNS {a!r}")


@pytest.fixture
def upstox():
    fake = _FakeUpstox()
    def send(adapter, request, **kwargs):  # a plain function, so the adapter binds as self
        return fake.send(adapter, request, **kwargs)

    with mock.patch.object(HTTPAdapter, "send", send), \
            mock.patch.object(socket, "getaddrinfo", _no_dns), \
            mock.patch.object(socket, "create_connection", _refuse), \
            mock.patch.object(socket.socket, "connect", _refuse), \
            mock.patch.object(time, "sleep", lambda s: None):
        yield fake
    # Whatever a test did, nothing left the process for another host.
    assert {host for host, _, _, _ in fake.requests} <= {"api.upstox.com"}


def _call(tool, **arguments):
    c = connector.UpstoxConnector()
    return c._handle_tool_call({"name": f"{c.connector_type}_{tool}", "arguments": arguments})


def _tables(result):
    return {t["name"]: t for t in result["tables"]}


# --------------------------------------------------------------------------
# Connection: the token every call needs
# --------------------------------------------------------------------------

def test_validate_config_accepts_a_token():
    assert _call("validate_config", config={"access_token": TOKEN})["valid"] is True


def test_validate_config_refuses_a_missing_token():
    result = _call("validate_config", config={})
    assert result["valid"] is False
    assert "Missing required field: access_token" in result["errors"]


def test_metadata_requires_the_token():
    import os
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "metadata.json")) as f:
        meta = json.load(f)
    assert meta["required_config"] == ["access_token"]
    assert meta["config_aliases"] == {"access_token": ["token"]}


def test_connection_sends_the_bearer_token(upstox):
    result = _call("test_connection", config={"access_token": TOKEN})
    assert result["success"] is True and result["credentials_validated"] is True
    assert ("api.upstox.com", "/v2/user/profile", "limit=1", True) in upstox.requests


def test_connection_without_a_token_fails(upstox):
    result = _call("test_connection", config={})
    assert result["success"] is False
    assert "401" in result["error"]


# --------------------------------------------------------------------------
# Schema: three tables, each keyed
# --------------------------------------------------------------------------

def test_discover_schema_holds_back_candles_until_their_settings_are_set(upstox):
    result = _call("discover_schema", config={"access_token": TOKEN})
    assert set(_tables(result)) == {"user_profile", "market_holidays"}
    assert result["tables_needing_settings"] == [
        {"name": "historical_candle", "missing_settings": ["instrument_key", "unit", "interval"]}]


def test_discover_schema_keys_every_table(upstox):
    tables = _tables(_call("discover_schema", config={"access_token": TOKEN, **CANDLE_SETTINGS}))
    assert {name: t["primary_keys"] for name, t in tables.items()} == {
        "user_profile": ["user_id"],
        "market_holidays": ["date", "holiday_type"],
        "historical_candle": ["instrument_key", "timestamp"],
    }
    candle_columns = [c["name"] for c in tables["historical_candle"]["columns"]]
    assert candle_columns[:7] == ["timestamp", "open", "high", "low", "close", "volume", "open_interest"]


# --------------------------------------------------------------------------
# Export
# --------------------------------------------------------------------------

def test_export_user_profile_is_one_row(upstox):
    result = _call("export", config={"access_token": TOKEN}, object="user_profile")
    assert result["success"] is True
    assert [r["user_id"] for r in result["records"]] == ["TEST01"]


def test_export_market_holidays(upstox):
    result = _call("export", config={"access_token": TOKEN}, object="market_holidays")
    assert result["success"] is True
    assert [(r["date"], r["holiday_type"]) for r in result["records"]] == [
        ("2026-01-26", "TRADING_HOLIDAY"), ("2026-03-03", "SETTLEMENT_HOLIDAY")]


def test_export_candles_names_the_positional_columns(upstox):
    result = _call("export", config={"access_token": TOKEN, **CANDLE_SETTINGS}, object="historical_candle")
    assert result["success"] is True
    assert result["records"][0] == {
        "timestamp": "2026-10-27T00:00:00+05:30", "open": 100.0, "high": 101.5, "low": 99.5,
        "close": 100.75, "volume": 12345, "open_interest": 0,
        "instrument_key": INSTRUMENT, "unit": "days", "interval": "1"}
    assert len(result["records"]) == 3


def test_a_first_run_reads_the_last_365_days(upstox):
    _call("export", config={"access_token": TOKEN, **CANDLE_SETTINGS}, object="historical_candle")
    to_date, from_date = upstox.candle_window()
    assert to_date - from_date == timedelta(days=365)
    assert abs((to_date - datetime.now(timezone.utc).date()).days) <= 1


def test_a_later_run_starts_at_since(upstox):
    _call("export", config={"access_token": TOKEN, **CANDLE_SETTINGS}, object="historical_candle",
          since="2026-10-20")
    to_date, from_date = upstox.candle_window()
    assert from_date == date(2026, 10, 20)
    assert to_date >= from_date


def test_from_date_in_the_connection_config_does_not_move_the_window(upstox):
    _call("export", config={"access_token": TOKEN, **CANDLE_SETTINGS, "from_date": "2026-10-20"},
          object="historical_candle")
    to_date, from_date = upstox.candle_window()
    assert to_date - from_date == timedelta(days=365)


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-q"]))
