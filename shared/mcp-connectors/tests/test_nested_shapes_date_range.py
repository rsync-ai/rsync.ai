"""Runtime half of the Upstox generator fix (2026-10-28): records the paginator missed.

Bug class: the generator knows where the records are and what a path param
means, and the paginator in ``base_connector.py`` cannot act on it:

* ``record_is_object`` wrapped the envelope, not the object under the records
  key — ``{"status": "ok", "data": {...}}`` became one row of ``status`` +
  ``data``;
* positional rows (``[ts, open, high, ...]``) had no way to be named, so a
  list of lists exported lists;
* a resource whose URL takes a date range (``.../{to_date}/{from_date}``) had
  no way to fill it: the start from the run's ``since`` (or the last 365 days on
  a first run), the end today;
* the settings in the URL (the instrument a candle belongs to) were not on the
  rows, though they are part of the row's key.

Everything is opt-in per resource config; an unflagged resource behaves exactly
as before (the controls below).
"""
from __future__ import annotations

import importlib.util
import os
from datetime import date, timedelta

_BC_PATH = os.path.join(os.path.dirname(__file__), "..", "base_connector.py")
_spec = importlib.util.spec_from_file_location("base_connector_nsdr", _BC_PATH)
_bc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bc)

PaginationHandler = _bc.PaginationHandler
ApiHandler = _bc.ApiHandler

PROFILE = {"email": "someone@example.invalid", "exchanges": ["NSE"], "user_id": "T01"}
COLUMNS = ["timestamp", "open", "high", "low", "close", "volume", "open_interest"]
ROW = ["2026-10-27T00:00:00+05:30", 100.0, 101.5, 99.5, 100.75, 12345, 0]
DATE_RANGE = {"start": "from_date", "end": "to_date", "format": "date"}


# --- the object under the records key ---------------------------------------

def test_a_flagged_object_under_the_records_key_is_the_row():
    h = PaginationHandler(response_data_key="data", record_is_object=True)
    assert h._extract_records({"status": "success", "data": PROFILE}) == [PROFILE]


def test_an_unflagged_object_under_the_records_key_is_still_no_rows():  # control
    h = PaginationHandler(response_data_key="data")
    assert h._extract_records({"status": "success", "data": PROFILE}) == []


def test_a_flagged_list_under_the_records_key_is_still_the_list():  # control
    h = PaginationHandler(response_data_key="data", record_is_object=True)
    assert h._extract_records({"data": [PROFILE, PROFILE]}) == [PROFILE, PROFILE]


# --- positional rows ---------------------------------------------------------

def test_positional_rows_are_zipped_to_the_column_names():
    h = PaginationHandler(response_data_key="data.candles", record_columns=COLUMNS)
    assert h._extract_records({"data": {"candles": [ROW]}}) == [dict(zip(COLUMNS, ROW))]


def test_a_longer_row_keeps_its_extra_values_under_numbered_names():
    h = PaginationHandler(response_data_key="data.candles", record_columns=COLUMNS[:2])
    assert h._extract_records({"data": {"candles": [ROW[:3]]}}) == [
        {"timestamp": ROW[0], "open": ROW[1], "field_3": ROW[2]}]


def test_object_rows_are_left_alone_when_columns_are_set():
    h = PaginationHandler(response_data_key="data", record_columns=COLUMNS)
    assert h._extract_records({"data": [PROFILE]}) == [PROFILE]


def test_positional_rows_without_columns_are_unchanged():  # control
    h = PaginationHandler(response_data_key="data.candles")
    assert h._extract_records({"data": {"candles": [ROW]}}) == [ROW]


# --- the date range ----------------------------------------------------------

TODAY = date(2026, 10, 28)


def test_since_is_the_start_and_today_the_end():
    assert _bc.date_range_params(DATE_RANGE, since="2026-10-20", today=TODAY) == {
        "from_date": "2026-10-20", "to_date": "2026-10-28"}


def test_a_timestamp_since_starts_on_its_day():
    out = _bc.date_range_params(DATE_RANGE, since="2026-10-27T00:00:00+05:30", today=TODAY)
    assert out["from_date"] == "2026-10-27"


def test_a_first_run_reads_the_last_365_days():
    assert _bc.date_range_params(DATE_RANGE, today=TODAY) == {
        "from_date": (TODAY - timedelta(days=365)).isoformat(), "to_date": "2026-10-28"}
    assert _bc.DATE_RANGE_FIRST_RUN_DAYS == 365


def test_a_since_after_today_never_makes_an_inverted_range():
    assert _bc.date_range_params(DATE_RANGE, since="2026-10-30", today=TODAY) == {
        "from_date": "2026-10-30", "to_date": "2026-10-30"}


def test_a_date_time_range_is_sent_as_date_times():
    dr = dict(DATE_RANGE, format="date-time")
    assert _bc.date_range_params(dr, since="2026-10-20", today=TODAY) == {
        "from_date": "2026-10-20T00:00:00Z", "to_date": "2026-10-28T00:00:00Z"}


def test_an_unreadable_since_falls_back_to_the_first_run_window():
    out = _bc.date_range_params(DATE_RANGE, since="not a date", today=TODAY)
    assert out["from_date"] == (TODAY - timedelta(days=365)).isoformat()


# --- export_resource end to end ---------------------------------------------

class _FakeConnector:
    def __init__(self, body):
        self.body = body
        self.calls = []

    def _make_request_v2(self, method, endpoint, config, params=None, data=None):
        self.calls.append({"endpoint": endpoint, "config": dict(config), "params": dict(params or {})})
        return True, 200, self.body, {}


CANDLE_RC = {
    "pagination_type": "none", "response_data_key": "data.candles", "record_columns": COLUMNS,
    "date_range": DATE_RANGE, "stamp_params": ["instrument_key", "unit", "interval"],
    "incremental_param": "from_date", "incremental_field": "timestamp", "supports_incremental": True,
}
CANDLE_CFG = {"instrument_key": "NSE_EQ|INE000000000", "unit": "days", "interval": "1"}
CANDLE_EP = "/v3/historical-candle/{instrument_key}/{unit}/{interval}/{to_date}/{from_date}"


def _export(conn, rc, config, params):
    return ApiHandler(conn).export_resource(
        connector=conn, config=config, resource="historical_candle", endpoint=CANDLE_EP,
        params=params, resource_config=rc, incremental_field=rc.get("incremental_field"))


def test_export_fills_the_range_from_since_and_stamps_the_url_settings():
    conn = _FakeConnector({"data": {"candles": [ROW]}})
    out = _export(conn, CANDLE_RC, dict(CANDLE_CFG), {"from_date": "2026-10-20"})
    assert out.success is True
    params = conn.calls[0]["params"]
    assert params["from_date"] == "2026-10-20"
    assert params["to_date"] >= "2026-10-28"
    assert out.records == [{**dict(zip(COLUMNS, ROW)), **CANDLE_CFG}]


def test_export_with_no_since_reads_the_first_run_window():
    conn = _FakeConnector({"data": {"candles": []}})
    _export(conn, CANDLE_RC, dict(CANDLE_CFG), {})
    params = conn.calls[0]["params"]
    start, end = date.fromisoformat(params["from_date"]), date.fromisoformat(params["to_date"])
    assert end - start == timedelta(days=365)


def test_a_date_on_the_connection_config_never_overrides_the_range():
    conn = _FakeConnector({"data": {"candles": []}})
    _export(conn, CANDLE_RC, dict(CANDLE_CFG, from_date="2025-01-01", to_date="2025-02-01"),
            {"from_date": "2026-10-20"})
    call = conn.calls[0]
    assert "from_date" not in call["config"] and "to_date" not in call["config"]
    assert call["params"]["from_date"] == "2026-10-20"


def test_an_unflagged_resource_sends_its_params_and_config_unchanged():  # control
    conn = _FakeConnector({"data": [{"id": 1}]})
    rc = {"pagination_type": "none", "response_data_key": "data"}
    cfg = dict(CANDLE_CFG, from_date="2025-01-01")
    out = _export(conn, rc, cfg, {"q": "x"})
    assert conn.calls[0]["params"] == {"q": "x"}
    assert conn.calls[0]["config"] == cfg
    assert out.records == [{"id": 1}]
