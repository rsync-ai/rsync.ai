#!/usr/bin/env python3
"""Offline unit tests for the nse-india source connector.

Every request goes to a local stdlib HTTP server serving synthetic files built
here — the tests never reach NSE, and no NSE data is committed (it is not ours
to redistribute). Symbols, prices and ISINs below are made up.

Runs with either pytest (`pytest test_connector.py`) or plain stdlib
(`python3 test_connector.py`) — no third-party dependencies, matching the
connector itself.
"""

import io
import json
import os
import select
import socket
import subprocess
import sys
import threading
import time
import urllib.request
import zipfile
from datetime import date, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape

import connector
from connector import (
    BLOCK_DEALS_PATH,
    BULK_DEALS_PATH,
    EQUITY_LIST_PATH,
    FII_DII_PATH,
    INDEX_LIST_PATHS,
    TABLES,
    USER_AGENT,
    NseIndiaMCPServer,
    _bhavcopy_path,
    _corporate_actions_path,
    _delivery_path,
    _history_path,
    _index_close_path,
    _participant_path,
)

HERE = os.path.dirname(os.path.abspath(__file__))

# Mon 2025-02-03. The Saturday before it, 2025-02-01, was Budget day: NSE held
# a live session, so a weekday-only walk would have missed a trading day.
TODAY = date(2025, 2, 3)
THU, FRI, SAT, SUN, MON = (date(2025, 1, 30), date(2025, 1, 31), date(2025, 2, 1), date(2025, 2, 2), TODAY)
# The watermark an earlier run left: most tests read the days after it.
LAST_RUN = "2025-01-29"
FIRST_DAY = date(2024, 7, 8)  # the first UDiFF bhavcopy; a first run starts here
SINCE_KEYS = ("since", "updated_since", "modified_since", "modified_after")

UDIFF_HEADER = [
    "TradDt", "BizDt", "Sgmt", "Src", "FinInstrmTp", "FinInstrmId", "ISIN", "TckrSymb", "SctySrs",
    "XpryDt", "FininstrmActlXpryDt", "StrkPric", "OptnTp", "FinInstrmNm", "OpnPric", "HghPric",
    "LwPric", "ClsPric", "LastPric", "PrvsClsgPric", "UndrlygPric", "SttlmPric", "OpnIntrst",
    "ChngInOpnIntrst", "TtlTradgVol", "TtlTrfVal", "TtlNbOfTxsExctd", "SsnId", "NewBrdLotQty",
    "Rmks", "Rsvd1", "Rsvd2", "Rsvd3", "Rsvd4",
]


# --------------------------------------------------------------------------
# Fixture archive
# --------------------------------------------------------------------------

class _Archive:
    """A local stand-in for the archive host. routes[path] is a list of
    responses served in order (the last one repeats); a response is
    (status, body, headers) or "reset" (close the socket without answering).
    Any other path is a 404, like a holiday's missing file."""

    def __init__(self):
        self.routes = {}
        self.hits = []
        archive = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                archive.hits.append((self.path, self.headers.get("User-Agent")))
                queue = archive.routes.get(self.path)
                if not queue:
                    self.send_response(404)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                resp = queue[0] if len(queue) == 1 else queue.pop(0)
                if resp == "reset":
                    self.close_connection = True
                    return
                status, body, headers = resp
                self.send_response(status)
                for k, v in headers.items():
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, format, *args):  # noqa: A002
                return

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def reset(self):
        self.routes = {}
        self.hits = []

    def hits_for(self, path):
        return sum(1 for p, _ in self.hits if p == path)


ARCHIVE = _Archive()


def bhav_csv(day, symbols, header=UDIFF_HEADER, overrides=None, trad_dt=None):
    lines = [",".join(header)]
    for i, sym in enumerate(symbols):
        sym, series = sym if isinstance(sym, tuple) else (sym, "EQ")
        vals = {
            "TradDt": (trad_dt or day).isoformat(), "BizDt": day.isoformat(), "Sgmt": "CM", "Src": "NSE",
            "FinInstrmTp": "STK", "FinInstrmId": str(1000 + i), "ISIN": f"INE{i:09d}", "TckrSymb": sym,
            "SctySrs": series, "FinInstrmNm": f"{sym} SYNTHETIC LTD", "OpnPric": "100.05", "HghPric": "101.10",
            "LwPric": "99.95", "ClsPric": f"{100 + i}.50", "LastPric": "100.40", "PrvsClsgPric": "99.00",
            "SttlmPric": "100.50", "TtlTradgVol": str(1000 * (i + 1)), "TtlTrfVal": "123456.78",
            "TtlNbOfTxsExctd": "42", "NewBrdLotQty": "1",
        }
        vals.update((overrides or {}).get(sym, {}))
        lines.append(",".join(vals.get(h, "") for h in header))
    return "\n".join(lines) + "\n"


def zipped(text, name="BhavCopy_NSE_CM.csv"):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        zf.writestr(name, text)
    return buf.getvalue()


def ok(body):
    return (200, body, {"Content-Type": "application/zip"})


def publish(day, symbols, **kw):
    ARCHIVE.routes[_bhavcopy_path(day)] = [ok(zipped(bhav_csv(day, symbols, **kw)))]


def server(today=TODAY, **kw):
    ARCHIVE.reset()
    return NseIndiaMCPServer(base_url=ARCHIVE.url, www_base_url=ARCHIVE.url, min_interval=0, backoff=(0, 0), timeout=2,
                             today=lambda: today, **kw)


def bhav(srv, **params):
    """One export call. Without a since of its own it reads the days after
    LAST_RUN; pass since=None for a first run."""
    if not any(k in params for k in SINCE_KEYS):
        params["since"] = LAST_RUN
    return srv.export({"table": "bhavcopy_daily", "offset": 0, **params})


def sweep(srv, since=LAST_RUN, limit=10000, table="bhavcopy_daily"):
    """Page the way the batch executor does: follow next_cursor until a short
    page that says nothing more follows, or a page with no rows, which ends the
    table whatever has_more says (executor.go, the len(rows) == 0 branch)."""
    pages, rows, cursor = [], [], None
    for _ in range(50):
        params = {"limit": limit, "since": since}
        if cursor:
            params["cursor"] = cursor
        res = srv.export({"table": table, "offset": 0, **params})
        assert res["success"] is True, res
        pages.append(res)
        rows.extend(res["data"])
        if not res["data"] or (res["row_count"] < limit and not res["has_more"]):
            return pages, rows
        cursor = res.get("next_cursor")
        assert cursor, "a page that can be followed must say where it stopped"
    raise AssertionError("sweep did not finish")


def watermark(res):
    return (res.get("stats") or {}).get("watermark")


def keys(rows):
    return [(r["trade_date"], r["symbol"], r["series"]) for r in rows]


# --------------------------------------------------------------------------
# Schema and config
# --------------------------------------------------------------------------

def test_discover_schema_shape():
    res = server().discover_schema({})
    assert res["success"] is True and res["total_tables"] == 12
    assert {t["name"] for t in res["tables"]} == {
        "bhavcopy_daily", "equity_list", "bhavcopy_history", "delivery_daily", "index_daily",
        "participant_oi_daily", "participant_vol_daily", "corporate_actions",
        "bulk_deals", "block_deals", "fii_dii_cash", "index_constituents",
    }
    for t in res["tables"]:
        assert t["primary_keys"] == TABLES[t["name"]]["primary_key"]
        assert "row_count" not in t  # remote files: unknown until read
        for col in t["columns"]:
            assert set(col.keys()) == {"name", "type", "nullable"}
            # A bare "numeric" is PostgreSQL's unconstrained NUMERIC; "decimal" maps to NUMERIC(38,9).
            assert col["type"] in {"string", "date", "decimal", "integer"}, f"{t['name']}.{col['name']}"
        pk = set(TABLES[t["name"]]["primary_key"])
        assert all(not c["nullable"] for c in t["columns"] if c["name"] in pk)


def test_capabilities_name_the_incremental_field():
    caps = server().get_capabilities({})["capabilities"]
    assert caps["supports_incremental"] is True and caps["supports_destination"] is False
    assert caps["incremental_fields"] == {
        "bhavcopy_daily": "trade_date", "bhavcopy_history": "trade_date", "delivery_daily": "trade_date",
        "index_daily": "trade_date", "participant_oi_daily": "trade_date", "participant_vol_daily": "trade_date",
        "corporate_actions": "ex_date",
    }
    # bhavcopy_history sits beside bhavcopy_daily under the same column names, so one UNION ALL joins them
    daily = {c["name"]: c["type"] for c in TABLES["bhavcopy_daily"]["columns"]}
    assert all(daily[c["name"]] == c["type"] for c in TABLES["bhavcopy_history"]["columns"])


def test_the_connection_has_no_settings():
    with open(os.path.join(HERE, "metadata.json"), encoding="utf-8") as f:
        meta = json.load(f)
    for key in ("config_schema", "configuration_schema"):
        assert meta[key]["properties"] == {} and meta[key]["required"] == [], key
    srv = server(today=date(2024, 9, 2))
    assert srv.validate_config({}) == {"success": True, "valid": True, "errors": [], "warnings": []}
    # The dates and the page size were settings once. A config that still carries
    # them is ignored, not obeyed: a run with no watermark starts at the first day.
    old = {"start_date": "2024-08-20", "end_date": "2024-08-21", "max_dates_per_page": 1}
    assert srv.validate_config({"config": old})["valid"] is True
    publish(FIRST_DAY, ["ALPHA"])
    res = bhav(srv, since=None, config=old)
    assert res["success"] is True and keys(res["data"]) == [("2024-07-08", "ALPHA", "EQ")]
    assert res["has_more"] is False and watermark(res)["value"] == "2024-07-08"


def test_unknown_table_fails_cleanly():
    res = server().export({"table": "quotes"})
    assert res["success"] is False and "Unknown table" in res["error"]


def test_archive_host_comes_from_the_environment_never_the_config():
    srv = server()
    publish(THU, ["ALPHA"])
    res = bhav(srv, config={"base_url": "http://example.invalid"})
    assert res["success"] is True and res["row_count"] == 1
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 1

    old = os.environ.get("NSE_ARCHIVE_BASE_URL")
    os.environ["NSE_ARCHIVE_BASE_URL"] = "http://archive.test:8080/"
    try:
        assert NseIndiaMCPServer()._client.base_url == "http://archive.test:8080"
    finally:
        if old is None:
            os.environ.pop("NSE_ARCHIVE_BASE_URL")
        else:
            os.environ["NSE_ARCHIVE_BASE_URL"] = old
    for bad in ("ftp://archive.test", "archive.test", "http://"):
        try:
            NseIndiaMCPServer(base_url=bad)
        except ValueError:
            continue
        raise AssertionError(f"accepted base URL {bad!r}")


# --------------------------------------------------------------------------
# bhavcopy_daily: the walk and the watermark (INCREMENTAL.md §3)
# --------------------------------------------------------------------------

def test_bhavcopy_url_layout():
    assert _bhavcopy_path(date(2026, 10, 23)) == "/content/cm/BhavCopy_NSE_CM_0_0_0_20261023_F_0000.csv.zip"
    assert EQUITY_LIST_PATH == "/content/equities/EQUITY_L.csv"


def test_a_run_reads_every_calendar_day_after_the_watermark():
    srv = server()
    publish(THU, ["ALPHA", "BETA"])
    publish(FRI, ["ALPHA", "BETA", ("GAMMA", "BE")])
    publish(SAT, ["ALPHA"])  # a Saturday session
    res = bhav(srv)

    assert res["success"] is True
    assert keys(res["data"]) == [
        ("2025-01-30", "ALPHA", "EQ"), ("2025-01-30", "BETA", "EQ"),
        ("2025-01-31", "ALPHA", "EQ"), ("2025-01-31", "BETA", "EQ"), ("2025-01-31", "GAMMA", "BE"),
        ("2025-02-01", "ALPHA", "EQ"),
    ]
    assert res["has_more"] is False
    # Sunday and today have no file yet: skipped, and they do not move the watermark
    assert watermark(res) == {"field": "trade_date", "value": "2025-02-01"}
    asked = [p for p, _ in ARCHIVE.hits]
    assert asked == [_bhavcopy_path(d) for d in (THU, FRI, SAT, SUN, MON)]
    assert {ua for _, ua in ARCHIVE.hits} == {USER_AGENT}


def test_the_first_run_starts_at_the_first_udiff_day():
    # Mon 2024-07-08 to Tue 07-16: two pages of days, the weekend between them.
    srv = server(today=date(2024, 7, 16))
    days = [FIRST_DAY + timedelta(days=n) for n in (0, 1, 2, 3, 4, 7)]
    for d in days:
        publish(d, ["ALPHA"])
    pages, rows = sweep(srv, since=None)

    assert [r["trade_date"] for r in rows] == [d.isoformat() for d in days]
    assert pages[0]["next_cursor"] == "2024-07-13:0" and watermark(pages[0])["value"] == "2024-07-12"
    assert watermark(pages[-1])["value"] == "2024-07-15" and pages[-1]["has_more"] is False
    walked = [FIRST_DAY + timedelta(days=n) for n in range(9)]
    assert [p for p, _ in ARCHIVE.hits] == [_bhavcopy_path(d) for d in walked]  # each day once
    # however long after it the first run happens, and from a watermark older than it
    for since in (None, "2024-01-01"):
        srv = server(today=date(2026, 10, 27))
        for d in days[:5]:
            publish(d, ["ALPHA"])
        res = bhav(srv, since=since)
        assert res["success"] is True and res["has_more"] is True, since
        assert [p for p, _ in ARCHIVE.hits] == [_bhavcopy_path(d) for d in days[:5]], since


def test_a_daily_schedule_reads_each_trading_day_once():
    """A run every weekday evening, from the watermark Friday 2025-01-24 left:
    a weekend, a file published a day late, a holiday and a Saturday session."""
    today = [date(2025, 1, 27)]
    srv = server()
    srv._today = lambda: today[0]
    mark, read, requests = "2025-01-24", [], []

    def run_on(day, *published):
        nonlocal mark
        today[0] = day
        for d in published:
            publish(d, ["ALPHA", "BETA"])
        before = len(ARCHIVE.hits)
        pages, rows = sweep(srv, since=mark)
        mark = watermark(pages[-1])["value"]
        read.extend(rows)
        requests.append(len(ARCHIVE.hits) - before)
        return {r["trade_date"] for r in rows}

    mon, tue, wed, thu, fri = (date(2025, 1, 27) + timedelta(days=n) for n in range(5))
    assert run_on(mon, mon) == {"2025-01-27"}  # Sat and Sun: no file
    assert run_on(tue) == set() and mark == "2025-01-27"  # Tuesday's file is late
    assert run_on(wed, tue, wed) == {"2025-01-28", "2025-01-29"}
    assert run_on(thu) == set() and mark == "2025-01-29"  # a holiday
    assert run_on(fri, fri) == {"2025-01-31"}
    assert run_on(MON, SAT, MON) == {"2025-02-01", "2025-02-03"}  # the Saturday session
    assert mark == "2025-02-03"
    assert run_on(MON) == set() and requests[-1] == 0  # a re-run the same evening asks for nothing
    assert len(read) == len(set(keys(read))) == 12  # six trading days, two symbols, no repeats
    assert requests[:-1] == [3, 1, 2, 1, 2, 3]


def test_days_in_a_row_without_a_file_do_not_end_the_run_early():
    # The executor stops a table at the first page with no rows. Missing days
    # (a long weekend, holidays, or Diwali week) must not fill a page on their own,
    # or every run would stop at the same day and never read past it.
    srv = server(today=date(2025, 2, 7))
    publish(date(2025, 2, 6), ["ALPHA"])
    pages, rows = sweep(srv)  # 2025-01-30 to 02-05 have no file
    assert keys(rows) == [("2025-02-06", "ALPHA", "EQ")]
    assert watermark(pages[-1])["value"] == "2025-02-06" and pages[-1]["has_more"] is False
    assert len(ARCHIVE.hits) == 9


def test_values_keep_their_exact_digits():
    srv = server()
    publish(THU, ["ALPHA", "BETA"], overrides={"BETA": {"TtlTrfVal": "98765432109876.123456789", "ClsPric": "0.05"}})
    res = bhav(srv)
    alpha, beta = res["data"]
    assert res["columns"] == [c["name"] for c in TABLES["bhavcopy_daily"]["columns"]]
    assert set(alpha) == set(res["columns"])
    assert alpha["close"] == "100.50" and alpha["open"] == "100.05"  # strings, never floats
    assert beta["turnover"] == "98765432109876.123456789" and beta["close"] == "0.05"
    assert alpha["volume"] == 1000 and beta["volume"] == 2000 and alpha["trades"] == 42 and alpha["board_lot"] == 1
    assert alpha["biz_date"] == "2025-01-30" and alpha["isin"] == "INE000000000"
    assert alpha["instrument_name"] == "ALPHA SYNTHETIC LTD" and alpha["segment"] == "CM"


def test_rerun_after_the_watermark_reads_nothing_new():
    srv = server()
    publish(THU, ["ALPHA"])
    publish(SAT, ["ALPHA"])
    res = bhav(srv, since="2025-02-01")
    assert res["success"] is True and res["row_count"] == 0 and res["has_more"] is False
    assert "next_cursor" not in res
    assert watermark(res) == {"field": "trade_date", "value": "2025-02-01"}  # echoes since
    assert [p for p, _ in ARCHIVE.hits] == [_bhavcopy_path(SUN), _bhavcopy_path(MON)]


def test_a_day_published_after_the_run_is_read_on_the_next_run():
    srv = server()
    first = bhav(srv, since="2025-02-01")
    assert first["row_count"] == 0 and watermark(first)["value"] == "2025-02-01"
    publish(MON, ["ALPHA", "BETA"])
    second = bhav(srv, since=watermark(first)["value"])
    assert keys(second["data"]) == [("2025-02-03", "ALPHA", "EQ"), ("2025-02-03", "BETA", "EQ")]
    assert watermark(second)["value"] == "2025-02-03"


def test_no_rows_and_no_since_report_no_watermark():
    res = bhav(server(today=date(2024, 7, 10)), since=None)
    assert res["success"] is True and res["row_count"] == 0 and res["has_more"] is False
    assert watermark(res) is None and "next_cursor" not in res


def test_malformed_since_warns_and_reads_from_the_first_day():
    srv = server(today=date(2024, 7, 9))
    publish(FIRST_DAY, ["ALPHA"])
    res = bhav(srv, since="yesterday")
    assert res["success"] is True and keys(res["data"]) == [("2024-07-08", "ALPHA", "EQ")]
    assert any("ignored since" in w and "2024-07-08" in w for w in res["warnings"])
    assert watermark(res)["value"] == "2024-07-08"


def test_since_aliases_and_timestamps_are_read_as_a_date():
    srv = server()
    publish(FRI, ["ALPHA"])
    publish(SAT, ["BETA"])
    for params in ({"since": "2025-01-31T00:00:00Z"}, {"updated_since": "2025-01-31"}, {"modified_after": "2025-01-31 10:00:00"}):
        res = bhav(srv, **params)
        assert keys(res["data"]) == [("2025-02-01", "BETA", "EQ")], params


def test_paging_by_dates_follows_the_cursor_without_gaps_or_repeats():
    srv = server(days_per_page=2)
    publish(THU, ["ALPHA", "BETA"])
    publish(FRI, ["ALPHA", "BETA", ("GAMMA", "BE")])
    publish(SAT, ["ALPHA"])
    pages, rows = sweep(srv)

    assert pages[0]["has_more"] is True and pages[0]["next_cursor"] == "2025-02-01:0"
    assert len(rows) == 6 and len(set(keys(rows))) == 6
    marks = [watermark(p)["value"] for p in pages if watermark(p)]
    assert marks == sorted(marks) and marks[-1] == "2025-02-01"
    # each day asked once across the whole sweep
    for d in (THU, FRI, SAT, SUN, MON):
        assert ARCHIVE.hits_for(_bhavcopy_path(d)) == 1, d


def test_a_day_larger_than_the_page_is_split_and_counted_once():
    srv = server()
    publish(THU, ["A1", "A2", "A3", "A4", "A5"])
    publish(FRI, ["B1", "B2", "B3"])
    pages, rows = sweep(srv, limit=4)

    assert pages[0]["row_count"] == 4 and pages[0]["next_cursor"] == "2025-01-30:4"
    # Thursday is only part-way out, so the first page claims no day past the last run
    assert watermark(pages[0])["value"] == LAST_RUN
    assert pages[1]["row_count"] == 4 and watermark(pages[1])["value"] == "2025-01-31"
    assert len(rows) == 8 and len(set(keys(rows))) == 8
    assert [r["symbol"] for r in rows] == ["A1", "A2", "A3", "A4", "A5", "B1", "B2", "B3"]
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 1  # the split day is downloaded once


def test_a_full_last_page_still_says_where_it_stopped():
    srv = server(today=THU)
    publish(THU, ["ALPHA", "BETA"])
    first = bhav(srv, limit=2)
    assert first["row_count"] == 2 and first["has_more"] is False
    # without next_cursor the executor would fall back to offset paging
    assert first["next_cursor"] == "2025-01-31:0"
    last = bhav(srv, limit=2, cursor=first["next_cursor"])
    assert last["success"] is True and last["row_count"] == 0 and "next_cursor" not in last


def test_the_walk_never_passes_today():
    srv = server()
    res = bhav(srv)
    assert res["success"] is True and res["has_more"] is False
    assert [p for p, _ in ARCHIVE.hits][-1] == _bhavcopy_path(TODAY)
    future = bhav(srv, since="2025-02-10")  # a watermark ahead of the clock
    assert future["success"] is True and future["row_count"] == 0 and future["has_more"] is False
    assert len(ARCHIVE.hits) == 5


def test_a_bad_cursor_and_an_offset_without_a_cursor_fail_closed():
    srv = server()
    for cursor in ("2025-01-30", "abc:1", "2025-13-01:0", "2025-01-30:-1"):
        res = bhav(srv, cursor=cursor)
        assert res["success"] is False and "cursor" in res["error"], cursor
    res = bhav(srv, offset=10000)
    assert res["success"] is False and "offset" in res["error"]
    assert ARCHIVE.hits == []


# --------------------------------------------------------------------------
# bhavcopy_daily: the HTTP client
# --------------------------------------------------------------------------

def test_retries_a_server_error_then_reads_the_file():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(503, b"busy", {}), ok(zipped(bhav_csv(THU, ["ALPHA"])))]
    res = bhav(srv)
    assert res["success"] is True and res["row_count"] == 1
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 2


def test_retries_a_reset_connection():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = ["reset", ok(zipped(bhav_csv(THU, ["ALPHA"])))]
    res = bhav(srv)
    assert res["success"] is True and res["row_count"] == 1
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 2


def test_gives_up_after_three_tries():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(502, b"", {})]
    res = bhav(srv)
    assert res["success"] is False and "after retries" in res["error"]
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 3


def test_a_403_is_reported_not_worked_around():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(403, b"denied", {})]
    res = bhav(srv)
    assert res["success"] is False
    assert "403" in res["error"] and "does not work around bot protection" in res["error"]
    assert ARCHIVE.hits == [(_bhavcopy_path(THU), USER_AGENT)]  # one plain request, no retry


def test_other_client_errors_are_not_retried():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(400, b"", {})]
    res = bhav(srv)
    assert res["success"] is False and "HTTP 400" in res["error"]
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 1


def test_a_failure_after_progress_keeps_the_rows_and_stops_the_walk():
    srv = server()
    publish(THU, ["ALPHA"])
    ARCHIVE.routes[_bhavcopy_path(FRI)] = [(403, b"", {})]
    publish(SAT, ["BETA"])
    res = bhav(srv)
    assert res["success"] is True and keys(res["data"]) == [("2025-01-30", "ALPHA", "EQ")]
    assert res["has_more"] is True and res["next_cursor"] == "2025-01-31:0"
    assert watermark(res)["value"] == "2025-01-30"  # never past the day that failed
    assert any("stopped before 2025-01-31" in w for w in res["warnings"])
    assert ARCHIVE.hits_for(_bhavcopy_path(SAT)) == 0


def test_refuses_a_redirect_to_another_host():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(302, b"", {"Location": "http://example.invalid/x.csv.zip"})]
    res = bhav(srv)
    assert res["success"] is False and "refused a redirect" in res["error"]
    assert ARCHIVE.hits_for(_bhavcopy_path(THU)) == 1


def test_follows_a_redirect_on_the_same_host():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(302, b"", {"Location": ARCHIVE.url + "/moved/thu.zip"})]
    ARCHIVE.routes["/moved/thu.zip"] = [ok(zipped(bhav_csv(THU, ["ALPHA"])))]
    res = bhav(srv)
    assert res["success"] is True and keys(res["data"]) == [("2025-01-30", "ALPHA", "EQ")]


# --------------------------------------------------------------------------
# bhavcopy_daily: the file
# --------------------------------------------------------------------------

def test_a_changed_file_layout_fails_with_the_headers_it_found():
    srv = server()
    header = [h for h in UDIFF_HEADER if h != "ClsPric"]
    publish(THU, ["ALPHA"], header=header)
    res = bhav(srv)
    assert res["success"] is False
    assert "CLSPRIC" in res["error"] and "headers found" in res["error"] and "TckrSymb" in res["error"]


def test_a_file_for_another_day_is_refused():
    srv = server()
    publish(THU, ["ALPHA"], trad_dt=FRI)
    res = bhav(srv)
    assert res["success"] is False and "carries TradDt 2025-01-31" in res["error"]


def test_a_non_zip_answer_is_an_error():
    srv = server()
    ARCHIVE.routes[_bhavcopy_path(THU)] = [(200, b"<html>maintenance</html>", {"Content-Type": "text/html"})]
    res = bhav(srv)
    assert res["success"] is False and "not a zip file" in res["error"]


def test_a_zip_without_exactly_one_csv_is_an_error():
    srv = server()
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("a.csv", bhav_csv(THU, ["ALPHA"]))
        zf.writestr("b.csv", bhav_csv(THU, ["BETA"]))
    ARCHIVE.routes[_bhavcopy_path(THU)] = [ok(buf.getvalue())]
    res = bhav(srv)
    assert res["success"] is False and "one .csv" in res["error"]


def test_unparseable_numbers_become_null_with_a_warning():
    srv = server()
    publish(THU, ["ALPHA", "BETA"], overrides={"ALPHA": {"ClsPric": "n/a", "LastPric": "-", "TtlTradgVol": "12.5"}})
    res = bhav(srv)
    alpha = res["data"][0]
    assert alpha["close"] is None and alpha["last"] is None and alpha["volume"] is None
    assert res["data"][1]["close"] == "101.50"
    assert any("column close" in w for w in res["warnings"])
    assert any("column volume" in w for w in res["warnings"])
    assert not any("column last" in w for w in res["warnings"])  # "-" is NSE's blank, not an error


def test_a_repeated_key_in_a_day_file_fails():
    srv = server()
    publish(THU, ["ALPHA", "ALPHA"])
    res = bhav(srv)
    assert res["success"] is False and "more than once" in res["error"]
    # the same symbol in two series is two keys
    srv = server()
    publish(THU, [("ALPHA", "EQ"), ("ALPHA", "BE")])
    assert bhav(srv)["row_count"] == 2


# --------------------------------------------------------------------------
# equity_list
# --------------------------------------------------------------------------

EQUITY_CSV = (
    "SYMBOL,NAME OF COMPANY, SERIES, DATE OF LISTING, PAID UP VALUE, MARKET LOT, ISIN NUMBER, FACE VALUE\n"
    "ALPHA,Alpha Synthetic Limited,EQ,06-OCT-2008,10,1,INE000000001,10\n"
    "BETA,Beta Synthetic Limited,BE,15-Mar-2021,2,1,INE000000002,2\n"
    "GAMMA,Gamma Synthetic Limited,EQ,01-JAN-2000,1,1,INE000000003,1\n"
)


def publish_equity_list(text=EQUITY_CSV):
    ARCHIVE.routes[EQUITY_LIST_PATH] = [(200, text.encode("utf-8"), {"Content-Type": "text/csv"})]


def test_equity_list_pages_by_offset_and_carries_no_watermark():
    srv = server()
    publish_equity_list()
    p1 = srv.export({"table": "equity_list", "limit": 2, "offset": 0, "since": "2025-01-01"})
    p2 = srv.export({"table": "equity_list", "limit": 2, "offset": 2})
    assert p1["success"] is True and p1["row_count"] == 2 and p1["has_more"] is True
    assert p2["row_count"] == 1 and p2["has_more"] is False
    assert [r["symbol"] for r in p1["data"] + p2["data"]] == ["ALPHA", "BETA", "GAMMA"]
    assert "stats" not in p1 and "next_cursor" not in p1
    alpha = p1["data"][0]
    assert alpha == {
        "symbol": "ALPHA", "company_name": "Alpha Synthetic Limited", "series": "EQ",
        "listing_date": "2008-10-06", "paid_up_value": "10", "market_lot": 1,
        "isin": "INE000000001", "face_value": "10",
    }
    assert p1["data"][1]["listing_date"] == "2021-03-15"
    assert ARCHIVE.hits_for(EQUITY_LIST_PATH) == 1  # the list is cached between pages


def test_equity_list_with_a_repeated_symbol_fails():
    srv = server()
    publish_equity_list(EQUITY_CSV + "ALPHA,Alpha Again,EQ,06-OCT-2008,10,1,INE000000009,10\n")
    res = srv.export({"table": "equity_list"})
    assert res["success"] is False and "more than once" in res["error"]


def test_test_connection_reads_the_equity_list():
    srv = server()
    publish_equity_list()
    res = srv.test_connection({})
    assert res["success"] is True and "3 listed equities" in res["message"]
    assert res["version"] == connector.CONNECTOR_VERSION
    # a connection saved with the old date settings still tests: they are ignored
    res = srv.test_connection({"config": {"start_date": "2020-01-01"}})
    assert res["success"] is True
    srv = server()
    res = srv.test_connection({})
    assert res["success"] is False and "404" in res["error"]


# --------------------------------------------------------------------------
# The other day files: old bhavcopy, delivery, index close, participant OI/volume
# --------------------------------------------------------------------------

def serve(path, body, status=200, ctype="text/csv"):
    ARCHIVE.routes[path] = [(status, body.encode("utf-8") if isinstance(body, str) else body, {"Content-Type": ctype})]


def serve_json(path, obj):
    serve(path, json.dumps(obj), ctype="application/json")


def export(srv, table, **params):
    return srv.export({"table": table, "offset": 0, **params})


HIST_PREV, HIST_LAST = date(2024, 7, 4), date(2024, 7, 5)  # the last two trading days before UDiFF


def history_csv(day, symbols, stamp=None):
    # The old file ends its header and every row with a comma.
    lines = ["SYMBOL,SERIES,OPEN,HIGH,LOW,CLOSE,LAST,PREVCLOSE,TOTTRDQTY,TOTTRDVAL,TIMESTAMP,TOTALTRADES,ISIN,"]
    for i, sym in enumerate(symbols):
        lines.append(f"{sym},EQ,100.05,101.10,99.95,{100 + i}.50,100.40,99.00,{1000 * (i + 1)},123456.78,"
                     f"{(stamp or day).strftime('%d-%b-%Y').upper()},42,INE{i:09d},")
    return "\n".join(lines) + "\n"


def test_day_file_url_layouts():
    assert _history_path(date(2016, 1, 4)) == "/content/historical/EQUITIES/2016/JAN/cm04JAN2016bhav.csv.zip"
    assert _delivery_path(FRI) == "/products/content/sec_bhavdata_full_31012025.csv"
    assert _index_close_path(FRI) == "/content/indices/ind_close_all_31012025.csv"
    assert _participant_path("oi", FRI) == "/content/nsccl/fao_participant_oi_31012025.csv"
    assert _participant_path("vol", FRI) == "/content/nsccl/fao_participant_vol_31012025.csv"
    assert _corporate_actions_path(date(2024, 1, 1)) == (
        "/api/corporates-corporateActions?index=equities&from_date=01-01-2024&to_date=31-12-2024")


def test_history_reads_the_old_bhavcopy_and_ends_at_its_last_day():
    srv = server()
    for day in (HIST_PREV, HIST_LAST):
        serve(_history_path(day), zipped(history_csv(day, ["ALPHA", "BETA"]), f"cm{day:%d}JUL2024bhav.csv"))
    res = export(srv, "bhavcopy_history", since="2024-07-03")
    assert res["success"] is True and "warnings" not in res, res
    assert keys(res["data"]) == [("2024-07-04", "ALPHA", "EQ"), ("2024-07-04", "BETA", "EQ"),
                                 ("2024-07-05", "ALPHA", "EQ"), ("2024-07-05", "BETA", "EQ")]
    assert res["data"][2] == {
        "trade_date": "2024-07-05", "symbol": "ALPHA", "series": "EQ", "isin": "INE000000000",
        "open": "100.05", "high": "101.10", "low": "99.95", "close": "100.50", "last": "100.40",
        "prev_close": "99.00", "volume": 1000, "turnover": "123456.78", "trades": 42,
    }
    # The weekend after the last file has none; the walk still ends at 2024-07-07,
    # so later runs do not ask for those two days again, and never for UDiFF days.
    assert res["has_more"] is False and watermark(res) == {"field": "trade_date", "value": "2024-07-07"}
    assert not any("cm08JUL2024" in p for p, _ in ARCHIVE.hits)
    again = export(srv, "bhavcopy_history", since="2024-07-07")
    assert again["success"] is True and again["data"] == [] and again["has_more"] is False
    assert watermark(again)["value"] == "2024-07-07" and "next_cursor" not in again


def test_history_first_run_starts_in_2016_and_old_days_without_a_file_are_asked_once():
    srv = server(days_per_page=1)
    first = date(2016, 1, 4)
    serve(_history_path(first), zipped(history_csv(first, ["ALPHA"]), "cm04JAN2016bhav.csv"))
    res = export(srv, "bhavcopy_history", since=None)
    assert res["success"] is True and keys(res["data"]) == [("2016-01-04", "ALPHA", "EQ")]
    assert ARCHIVE.hits[0][0] == _history_path(date(2016, 1, 1))
    assert watermark(res)["value"] == "2016-01-04" and res["next_cursor"] == "2016-01-05:0"
    export(srv, "bhavcopy_history", since=None)
    assert ARCHIVE.hits_for(_history_path(date(2016, 1, 1))) == 1
    assert ARCHIVE.hits_for(_history_path(first)) == 1


def test_history_refuses_a_file_stamped_with_a_later_day():
    srv = server()
    serve(_history_path(HIST_PREV), zipped(history_csv(HIST_PREV, ["ALPHA"], stamp=HIST_LAST), "x.csv"))
    res = export(srv, "bhavcopy_history", since="2024-07-03")
    assert res["success"] is False and "carries TIMESTAMP 2024-07-05" in res["error"]


DELIVERY_HEADER = ("SYMBOL, SERIES, DATE1, PREV_CLOSE, OPEN_PRICE, HIGH_PRICE, LOW_PRICE, LAST_PRICE, CLOSE_PRICE,"
                   " AVG_PRICE, TTL_TRD_QNTY, TURNOVER_LACS, NO_OF_TRADES, DELIV_QTY, DELIV_PER")


def delivery_csv(stamp):
    return "\n".join([
        DELIVERY_HEADER,
        f"ALPHA, EQ, {stamp}, 99.00, 100.05, 101.10, 99.95, 100.40, 100.50, 100.31, 1000, 1.23, 42, 600, 60.00",
        f"BETA, BL, {stamp}, 50.00, 50.00, 50.00, 50.00, 50.00, 50.00, 50.00, 500, 0.25, 1, -, -",
    ]) + "\n"


def test_delivery_reads_the_full_bhavdata_with_its_spaces_and_dashes():
    srv = server()
    serve(_delivery_path(FRI), delivery_csv("31-Jan-2025"))
    res = export(srv, "delivery_daily", since=THU.isoformat())
    assert res["success"] is True and "warnings" not in res, res
    assert res["data"] == [
        {"trade_date": "2025-01-31", "symbol": "ALPHA", "series": "EQ", "prev_close": "99.00", "open": "100.05",
         "high": "101.10", "low": "99.95", "last": "100.40", "close": "100.50", "avg_price": "100.31",
         "volume": 1000, "turnover_lacs": "1.23", "trades": 42, "delivery_qty": 600, "delivery_pct": "60.00"},
        {"trade_date": "2025-01-31", "symbol": "BETA", "series": "BL", "prev_close": "50.00", "open": "50.00",
         "high": "50.00", "low": "50.00", "last": "50.00", "close": "50.00", "avg_price": "50.00",
         "volume": 500, "turnover_lacs": "0.25", "trades": 1, "delivery_qty": None, "delivery_pct": None},
    ]
    assert watermark(res)["value"] == "2025-01-31" and res["has_more"] is False


def test_delivery_refuses_a_later_or_mixed_day_and_starts_on_its_first_day():
    srv = server()
    serve(_delivery_path(THU), delivery_csv("31-Jan-2025"))
    res = export(srv, "delivery_daily", since=LAST_RUN)
    assert res["success"] is False and "carries DATE1 2025-01-31" in res["error"]
    srv = server()
    mixed = delivery_csv("31-Jan-2025").splitlines()
    mixed[2] = mixed[2].replace("31-Jan-2025", "30-Jan-2025")
    serve(_delivery_path(FRI), "\n".join(mixed) + "\n")
    res = export(srv, "delivery_daily", since=THU.isoformat())
    assert res["success"] is False and "carries DATE1 2025-01-30" in res["error"]
    srv = server(days_per_page=1)
    first = date(2019, 9, 30)
    serve(_delivery_path(first), delivery_csv("30-Sep-2019"))
    res = export(srv, "delivery_daily", since="2016-05-01")
    assert ARCHIVE.hits[0][0] == _delivery_path(first)
    assert res["success"] is True and {r["trade_date"] for r in res["data"]} == {"2019-09-30"}


def test_old_days_without_a_file_are_asked_for_once_recent_ones_every_run():
    srv = server()
    for _ in range(2):
        res = export(srv, "delivery_daily", since="2025-01-24")
        assert res["success"] is True and res["data"] == [] and res["has_more"] is False
    old, recent = date(2025, 1, 26), date(2025, 1, 27)  # 8 and 7 days before TODAY
    assert ARCHIVE.hits_for(_delivery_path(old)) == 1
    assert ARCHIVE.hits_for(_delivery_path(recent)) == 2 and ARCHIVE.hits_for(_delivery_path(MON)) == 2


INDEX_HEADER = ("Index Name,Index Date,Open Index Value,High Index Value,Low Index Value,Closing Index Value,"
                "Points Change,Change(%),Volume,Turnover (Rs. Cr.),P/E,P/B,Div Yield")


def index_csv(stamp):
    return "\n".join([
        INDEX_HEADER,
        f"Synthetic 50,{stamp},23000.10,23100.20,22950.30,23050.40,50.30,0.22,250000000,15000.55,22.10,3.50,1.25",
        f"India VIX,{stamp},14.10,14.50,13.90,14.20,0.10,0.71,-,-,-,-,-",
    ]) + "\n"


def test_index_close_reads_every_index_and_vix_without_volume():
    srv = server(days_per_page=1, small_days_per_page=2)
    for day in (THU, FRI, MON):
        serve(_index_close_path(day), index_csv(day.strftime("%d-%m-%Y")))
    first = export(srv, "index_daily", since=LAST_RUN)
    assert first["success"] is True and "warnings" not in first, first
    # A small file pages by small_days_per_page, not days_per_page.
    assert {r["trade_date"] for r in first["data"]} == {"2025-01-30", "2025-01-31"}
    assert first["has_more"] is True and first["next_cursor"] == "2025-02-01:0"
    vix = next(r for r in first["data"] if r["index_name"] == "India VIX")
    assert vix == {"trade_date": "2025-01-30", "index_name": "India VIX", "open": "14.10", "high": "14.50",
                   "low": "13.90", "close": "14.20", "points_change": "0.10", "change_pct": "0.71",
                   "volume": None, "turnover_cr": None, "pe": None, "pb": None, "div_yield": None}
    nifty = next(r for r in first["data"] if r["index_name"] == "Synthetic 50")
    assert nifty["volume"] == "250000000" and nifty["turnover_cr"] == "15000.55" and nifty["pe"] == "22.10"
    pages, rows = sweep(srv, table="index_daily")
    assert sorted({r["trade_date"] for r in rows}) == ["2025-01-30", "2025-01-31", "2025-02-03"]
    assert watermark(pages[-1])["value"] == "2025-02-03" and pages[-1]["has_more"] is False


def test_index_close_refuses_a_later_days_file():
    srv = server()
    serve(_index_close_path(THU), index_csv("31-01-2025"))
    res = export(srv, "index_daily", since=LAST_RUN)
    assert res["success"] is False and "carries Index Date 2025-01-31" in res["error"]


def test_an_index_date_written_month_first_is_read_as_the_day_asked():
    # NSE's index close files for some days write the date month first:
    # 2023-04-06's says 04-06-2023, while 2023-04-13's says 13-04-2023.
    srv = server()
    serve(_index_close_path(THU), index_csv("01-30-2025"))
    serve(_index_close_path(SAT), index_csv("02-01-2025"))  # day first, an earlier day: not a copy of it
    serve(_index_close_path(MON), index_csv("02-03-2025"))  # day first, a later day
    res = export(srv, "index_daily", since=LAST_RUN)
    assert res["success"] is True and "warnings" not in res, res
    assert sorted({r["trade_date"] for r in res["data"]}) == ["2025-01-30", "2025-02-01", "2025-02-03"]
    assert watermark(res)["value"] == "2025-02-03"
    # Read month first, a date for another day is still refused.
    srv = server()
    serve(_index_close_path(THU), index_csv("01-31-2025"))
    res = export(srv, "index_daily", since=LAST_RUN)
    assert res["success"] is False and "carries Index Date 2025-01-31" in res["error"]


# The real header has tabs and spaces after some names.
PARTICIPANT_HEADER = ("Client Type,Future Index Long,Future Index Short,Future Stock Long,Future Stock Short\t,"
                      "Option Index Call Long,Option Index Put Long,Option Index Call Short,Option Index Put Short,"
                      "Option Stock Call Long,Option Stock Put Long,Option Stock Call Short,Option Stock Put Short,"
                      "Total Long Contracts\t,Total Short Contracts\t ")
CLIENT_TYPES = ("Client", "DII", "FII", "Pro", "TOTAL")


def participant_csv(title, eol="\r\n"):
    lines = ([title] if title else []) + [PARTICIPANT_HEADER] + [
        ",".join([ct] + [str(i * 100 + j) for j in range(14)]) for i, ct in enumerate(CLIENT_TYPES)
    ]
    return eol.join(lines) + eol


def test_participant_files_drop_their_title_line_however_it_is_quoted():
    titles = {
        "oi": '"Participant wise Open Interest (no. of contracts) in Equity Derivatives as on Jan 31, 2025",,,,',
        "vol": '""Participant wise Trading Volumes (no. of contracts) in Equity Derivatives as on Jan 31, 2025""',
    }
    for kind, title in titles.items():
        srv = server()
        serve(_participant_path(kind, FRI), participant_csv(title))
        res = export(srv, f"participant_{kind}_daily", since=THU.isoformat())
        assert res["success"] is True and "warnings" not in res, res
        assert [r["client_type"] for r in res["data"]] == list(CLIENT_TYPES)
        fii = res["data"][2]
        assert fii["trade_date"] == "2025-01-31" and fii["future_index_long"] == 200
        assert fii["future_stock_short"] == 203 and fii["total_short_contracts"] == 213
        assert watermark(res)["value"] == "2025-01-31"


def test_participant_title_for_a_later_day_is_refused_and_a_file_without_one_is_read():
    srv = server()
    serve(_participant_path("oi", THU), participant_csv('"Participant wise Open Interest as on Jan 31, 2025"'))
    res = export(srv, "participant_oi_daily", since=LAST_RUN)
    assert res["success"] is False and "is titled 2025-01-31" in res["error"]
    srv = server()
    serve(_participant_path("vol", FRI), participant_csv(None, eol="\n"))
    res = export(srv, "participant_vol_daily", since=THU.isoformat())
    assert res["success"] is True and res["row_count"] == 5


def test_files_that_end_lines_with_a_bare_carriage_return_are_read():
    # NSE's participant files for 2017-10-04 end the title line with \r\n and
    # every later line with a bare \r. Any of its CSV files could do the same.
    title = '"Participant wise Open Interest (no. of contracts) in Equity Derivatives as on Jan 31, 2025",,,,'
    for body in (title + "\r\n" + participant_csv(None, eol="\r"), participant_csv(title, eol="\r")):
        srv = server()
        serve(_participant_path("oi", FRI), body)
        res = export(srv, "participant_oi_daily", since=THU.isoformat())
        assert res["success"] is True and "warnings" not in res, res
        assert [r["client_type"] for r in res["data"]] == list(CLIENT_TYPES)
        assert res["data"][2]["total_short_contracts"] == 213
    srv = server()
    serve(_delivery_path(FRI), delivery_csv("31-Jan-2025").replace("\n", "\r"))
    res = export(srv, "delivery_daily", since=THU.isoformat())
    assert res["success"] is True and res["row_count"] == 2, res


def test_a_file_the_csv_reader_rejects_stops_the_walk_before_that_day():
    srv = server()
    serve(_delivery_path(THU), delivery_csv("30-Jan-2025"))
    serve(_delivery_path(FRI), DELIVERY_HEADER + "\n" + "A" * 200_000 + "\n")
    res = export(srv, "delivery_daily", since=LAST_RUN)
    assert res["success"] is True and {r["trade_date"] for r in res["data"]} == {"2025-01-30"}, res
    assert watermark(res)["value"] == "2025-01-30"
    assert any("stopped before 2025-01-31" in w and "could not be read as CSV" in w for w in res["warnings"]), res


class Num(str):
    """An Excel number cell, written exactly as given."""


class Inline(str):
    """An Excel inline string cell."""


def xlsx(rows):
    # Laid out as NSE's own workbook: text in the shared strings, numbers in the
    # cell, a cell left out where there is no value.
    shared = []

    def cell(ref, value):
        if isinstance(value, Num):
            return f'<c r="{ref}"><v>{value}</v></c>'
        if isinstance(value, Inline):
            return f'<c r="{ref}" t="inlineStr"><is><t>{escape(value)}</t></is></c>'
        if value not in shared:
            shared.append(value)
        return f'<c r="{ref}" t="s"><v>{shared.index(value)}</v></c>'

    sheet = "".join(
        f'<row r="{r + 1}">'
        + "".join(cell(f"{chr(65 + c)}{r + 1}", v) for c, v in enumerate(row) if v is not None)
        + "</row>"
        for r, row in enumerate(rows)
    )
    ns = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
    rel = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("[Content_Types].xml", "<Types/>")
        zf.writestr("xl/workbook.xml", f'<workbook {ns} xmlns:r="{rel}"><sheets>'
                    '<sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>')
        zf.writestr("xl/_rels/workbook.xml.rels",
                    '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
                    f'<Relationship Id="rId3" Type="{rel}/styles" Target="styles.xml"/>'
                    f'<Relationship Id="rId1" Type="{rel}/worksheet" Target="worksheets/sheet1.xml"/>'
                    f'<Relationship Id="rId4" Type="{rel}/sharedStrings" Target="sharedStrings.xml"/>'
                    "</Relationships>")
        zf.writestr("xl/sharedStrings.xml",
                    f"<sst {ns}>" + "".join(f"<si><t>{escape(s)}</t></si>" for s in shared) + "</sst>")
        zf.writestr("xl/worksheets/sheet1.xml", f"<worksheet {ns}><sheetData>{sheet}</sheetData></worksheet>")
    return buf.getvalue()


def test_an_excel_file_served_as_a_csv_is_read_like_the_csv():
    # NSE serves 2022-08-08's sec_bhavdata_full as an Excel workbook under the .csv name.
    srv = server()
    serve(_delivery_path(THU), delivery_csv("30-Jan-2025"))
    serve(_delivery_path(FRI), xlsx([
        DELIVERY_HEADER.split(","),
        ["ALPHA", " EQ", " 31-Jan-2025"] + [Num(v) for v in (
            "99", "100.05", "101.1", "99.95", "100.4", "100.50000000000001", "100.31", "1000", "1.23", "42", "600",
            "60")],
        [Inline("BETA"), " BL", " 31-Jan-2025"] + [Num(v) for v in (
            "50", "50", "50", "50", "50", "50", "50", "500", "0.25", "1")] + [None, " -"],
    ]))
    serve(_delivery_path(MON), zipped(delivery_csv("03-Feb-2025"), "sec_bhavdata_full_03022025.csv"))
    res = export(srv, "delivery_daily", since=LAST_RUN)
    assert res["success"] is True and sorted({r["trade_date"] for r in res["data"]}) == ["2025-01-30", "2025-01-31"]
    friday = [r for r in res["data"] if r["trade_date"] == "2025-01-31"]
    # Numbers to Excel's own 15 digits: 100.50000000000001 is the 100.5 Excel shows.
    assert friday == [
        {"trade_date": "2025-01-31", "symbol": "ALPHA", "series": "EQ", "prev_close": "99", "open": "100.05",
         "high": "101.1", "low": "99.95", "last": "100.4", "close": "100.5", "avg_price": "100.31",
         "volume": 1000, "turnover_lacs": "1.23", "trades": 42, "delivery_qty": 600, "delivery_pct": "60"},
        {"trade_date": "2025-01-31", "symbol": "BETA", "series": "BL", "prev_close": "50", "open": "50",
         "high": "50", "low": "50", "last": "50", "close": "50", "avg_price": "50",
         "volume": 500, "turnover_lacs": "0.25", "trades": 1, "delivery_qty": None, "delivery_pct": None},
    ], res
    # A zip that is not a workbook stops the walk before its day, as any unreadable file does.
    assert watermark(res)["value"] == "2025-01-31"
    assert any("stopped before 2025-02-03" in w and "Excel" in w for w in res["warnings"]), res


def test_a_file_that_repeats_an_earlier_day_is_skipped_like_a_missing_one():
    # NSE served Sunday 2026-11-01's delivery file as a byte copy of Friday's.
    srv = server()
    serve(_delivery_path(THU), delivery_csv("30-Jan-2025"))
    serve(_delivery_path(FRI), delivery_csv("31-Jan-2025"))
    serve(_delivery_path(SUN), delivery_csv("31-Jan-2025"))
    serve(_delivery_path(MON), delivery_csv("31-Jan-2025"))
    serve(_participant_path("oi", SUN), participant_csv('"Participant wise Open Interest as on Jan 31, 2025"'))
    res = export(srv, "delivery_daily", since=LAST_RUN)
    assert res["success"] is True and res["has_more"] is False, res
    assert sorted({r["trade_date"] for r in res["data"]}) == ["2025-01-30", "2025-01-31"]
    assert res["warnings"] == [
        "delivery_daily skipped 2 day(s) whose file repeats an earlier day's data "
        "(2025-02-02 carries 2025-01-31; 2025-02-03 carries 2025-01-31), as for a day with no file"]
    # A copy never moves the watermark past itself: today's file is asked for again next run.
    assert watermark(res)["value"] == "2025-01-31"
    res = export(srv, "participant_oi_daily", since=SAT.isoformat())
    assert res["success"] is True and res["data"] == [] and "2025-02-02 carries 2025-01-31" in res["warnings"][0]
    # An old copy is asked for once, like an old day with no file.
    srv = server()
    for day in (HIST_PREV, HIST_LAST):
        serve(_history_path(day), zipped(history_csv(day, ["ALPHA"]), "x.csv"))
    serve(_history_path(date(2024, 7, 6)), zipped(history_csv(HIST_LAST, ["ALPHA"]), "x.csv"))
    for _ in range(2):
        res = export(srv, "bhavcopy_history", since="2024-07-03")
        assert res["success"] is True and len(res["data"]) == 2 and watermark(res)["value"] == "2024-07-07"
    assert ARCHIVE.hits_for(_history_path(date(2024, 7, 6))) == 1


# --------------------------------------------------------------------------
# Corporate actions: one www API call per calendar year
# --------------------------------------------------------------------------

Y2024, Y2025 = date(2024, 1, 1), date(2025, 1, 1)
BOT_PAGE = "<!DOCTYPE html><html><head><title>Access Denied</title></head><body>Please wait</body></html>"


def corp_action(symbol, ex_date, subject="Bonus 1:1", **extra):
    item = {"symbol": symbol, "series": "EQ", "subject": subject, "exDate": ex_date, "recDate": ex_date,
            "bcStartDate": "-", "bcEndDate": "-", "ndStartDate": "-", "ndEndDate": "-",
            "comp": f"{symbol.title()} Synthetic Limited", "isin": "INE000000001", "faceVal": "10",
            "caBroadcastDate": None}
    item.update(extra)
    return item


def test_corporate_actions_read_by_year_and_the_current_year_again_next_run():
    srv = server()
    split = "Face Value Split (Sub-Division) - From Rs 10/- Per Share To Rs 2/- Per Share"
    serve_json(_corporate_actions_path(Y2024), [corp_action("ALPHA", "15-Mar-2024", split),
                                                corp_action("BETA", "02-Dec-2024")])
    serve_json(_corporate_actions_path(Y2025), [corp_action("GAMMA", "28-Jan-2025", "Dividend - Rs 5 Per Share")])
    res = export(srv, "corporate_actions", since="2023-12-31")
    assert res["success"] is True and "warnings" not in res, res
    assert [(r["symbol"], r["ex_date"]) for r in res["data"]] == [
        ("ALPHA", "2024-03-15"), ("BETA", "2024-12-02"), ("GAMMA", "2025-01-28")]
    assert res["data"][0] == {
        "symbol": "ALPHA", "series": "EQ", "ex_date": "2024-03-15", "subject": split,
        "company_name": "Alpha Synthetic Limited", "isin": "INE000000001", "face_value": "10",
        "record_date": "2024-03-15", "bc_start_date": None, "bc_end_date": None, "nd_start_date": None,
        "nd_end_date": None, "broadcast_date": None,
    }
    # 2025 is not over: its rows are out, but the watermark stays at the end of
    # 2024, so the next run reads 2025 again for actions announced since.
    assert res["has_more"] is False and watermark(res) == {"field": "ex_date", "value": "2024-12-31"}
    again = export(srv, "corporate_actions", since=watermark(res)["value"])
    assert [r["symbol"] for r in again["data"]] == ["GAMMA"] and watermark(again)["value"] == "2024-12-31"
    # A since inside a year reads that year from its start.
    mid = export(srv, "corporate_actions", since="2024-06-30")
    assert [r["symbol"] for r in mid["data"]] == ["ALPHA", "BETA", "GAMMA"]


def test_corporate_actions_fail_on_a_bot_page_or_a_missing_year():
    srv = server()
    serve(_corporate_actions_path(Y2024), BOT_PAGE, ctype="text/html")
    res = export(srv, "corporate_actions", since="2023-12-31")
    assert res["success"] is False and "web page, not data" in res["error"]
    srv = server()
    res = export(srv, "corporate_actions", since="2023-12-31")
    assert res["success"] is False and "was not found (404)" in res["error"]


def test_corporate_actions_keep_the_first_of_a_conflicting_repeat_and_skip_keyless_rows():
    srv = server()
    serve_json(_corporate_actions_path(Y2024), [
        corp_action("ALPHA", "15-Mar-2024"),
        corp_action("ALPHA", "15-Mar-2024"),  # an identical repeat collapses quietly
        corp_action("ALPHA", "15-Mar-2024", recDate="16-Mar-2024"),
        corp_action("BETA", "-"),
        corp_action("", "20-Mar-2024"),
    ])
    serve_json(_corporate_actions_path(Y2025), [])
    res = export(srv, "corporate_actions", since="2023-12-31")
    assert res["success"] is True and len(res["data"]) == 1 and res["data"][0]["record_date"] == "2024-03-15"
    assert any("1 row(s) repeat a key" in w and "kept the first" in w for w in res["warnings"]), res
    assert any("skipped 2 row(s) with no value in a key column" in w for w in res["warnings"]), res


# --------------------------------------------------------------------------
# Today-only tables: bulk and block deals, FII/DII cash, index lists
# --------------------------------------------------------------------------

DEALS_HEADER = "Date,Symbol,Security Name,Client Name,Buy/Sell,Quantity Traded,Trade Price / Wght. Avg. Price"


def test_bulk_deals_number_identical_deals_instead_of_merging_them():
    srv = server()
    deal = '31-JAN-2025,ALPHA,Alpha Synthetic Ltd,SYNTHETIC FUND {},{},"1,50,000",123.45,-'
    serve(BULK_DEALS_PATH, "\n".join([DEALS_HEADER + ",Remarks", deal.format("A", "BUY"), deal.format("A", "BUY"),
                                      deal.format("B", "SELL")]) + "\n")
    res = export(srv, "bulk_deals", since=LAST_RUN)
    assert res["success"] is True and "warnings" not in res and res["has_more"] is False, res
    assert "stats" not in res and "next_cursor" not in res
    assert [(r["client_name"], r["occurrence"]) for r in res["data"]] == [
        ("SYNTHETIC FUND A", 1), ("SYNTHETIC FUND A", 2), ("SYNTHETIC FUND B", 1)]
    assert res["data"][0] == {
        "deal_date": "2025-01-31", "symbol": "ALPHA", "client_name": "SYNTHETIC FUND A", "buy_sell": "BUY",
        "quantity": 150000, "price": "123.45", "occurrence": 1, "security_name": "Alpha Synthetic Ltd",
        "remarks": None,
    }


def test_block_deals_a_day_without_deals_and_a_file_without_prices():
    srv = server()
    serve(BLOCK_DEALS_PATH, DEALS_HEADER + "\n31-JAN-2025,BETA,Beta Synthetic Ltd,SYNTHETIC FUND C,BUY,500000,77.10\n")
    res = export(srv, "block_deals")
    assert res["success"] is True and res["data"] == [{
        "deal_date": "2025-01-31", "symbol": "BETA", "client_name": "SYNTHETIC FUND C", "buy_sell": "BUY",
        "quantity": 500000, "price": "77.10", "occurrence": 1, "security_name": "Beta Synthetic Ltd"}]
    for body, warned in (("", False), ("No Records\n", True)):
        srv = server()
        serve(BULK_DEALS_PATH, body)
        res = export(srv, "bulk_deals")
        assert res["success"] is True and res["data"] == [] and res["has_more"] is False
        assert ("warnings" in res) is warned, res
    srv = server()
    serve(BULK_DEALS_PATH, "Date,Symbol,Client Name,Buy/Sell,Quantity Traded\n31-JAN-2025,ALPHA,X,BUY,1\n")
    res = export(srv, "bulk_deals")
    assert res["success"] is False and "no trade price column" in res["error"]


def test_fii_dii_cash_reads_the_latest_day():
    srv = server()
    serve_json(FII_DII_PATH, [
        {"category": "DII **", "date": "31-Jan-2025", "buyValue": "12345.67", "sellValue": "11111.11",
         "netValue": "1234.56"},
        {"category": "FII/FPI *", "date": "31-Jan-2025", "buyValue": 10000.5, "sellValue": "12000.25",
         "netValue": "-1999.75"},
    ])
    res = export(srv, "fii_dii_cash", since=LAST_RUN)
    assert res["success"] is True and "warnings" not in res and "stats" not in res, res
    assert res["data"] == [
        {"trade_date": "2025-01-31", "category": "DII **", "buy_value": "12345.67", "sell_value": "11111.11",
         "net_value": "1234.56"},
        {"trade_date": "2025-01-31", "category": "FII/FPI *", "buy_value": "10000.5", "sell_value": "12000.25",
         "net_value": "-1999.75"},
    ]


def test_fii_dii_cash_reports_a_bot_page_and_a_refusal_without_retrying():
    srv = server()
    serve(FII_DII_PATH, BOT_PAGE, ctype="text/html")
    res = export(srv, "fii_dii_cash")
    assert res["success"] is False and "web page, not data" in res["error"]
    srv = server()
    serve(FII_DII_PATH, "", status=401)
    res = export(srv, "fii_dii_cash")
    assert res["success"] is False and "401 Unauthorized" in res["error"]
    assert "does not work around bot protection" in res["error"] and ARCHIVE.hits_for(FII_DII_PATH) == 1


def index_list_csv(*rows):
    return "Company Name,Industry,Symbol,Series,ISIN Code\n" + "".join(f"{r}\n" for r in rows)


def test_index_constituents_read_both_lists_stamped_with_today():
    srv = server()
    alpha = "Alpha Synthetic Ltd.,Financial Services,ALPHA,EQ,INE000000001"
    gamma = "Gamma Synthetic Ltd.,Metals & Mining,GAMMA,EQ,INE000000003"
    serve(INDEX_LIST_PATHS["NIFTY 500"], index_list_csv(alpha, gamma))
    serve(INDEX_LIST_PATHS["NIFTY COMMODITIES"], index_list_csv(gamma))
    res = export(srv, "index_constituents")
    assert res["success"] is True and "warnings" not in res, res
    assert [(r["captured_on"], r["index_name"], r["symbol"]) for r in res["data"]] == [
        ("2025-02-03", "NIFTY 500", "ALPHA"), ("2025-02-03", "NIFTY 500", "GAMMA"),
        ("2025-02-03", "NIFTY COMMODITIES", "GAMMA")]
    assert res["data"][1] == {"captured_on": "2025-02-03", "index_name": "NIFTY 500", "symbol": "GAMMA",
                              "company_name": "Gamma Synthetic Ltd.", "industry": "Metals & Mining",
                              "series": "EQ", "isin": "INE000000003"}
    srv = server()
    serve(INDEX_LIST_PATHS["NIFTY 500"], index_list_csv(alpha))
    res = export(srv, "index_constituents")
    assert res["success"] is False and "ind_niftycommoditieslist.csv was not found (404)" in res["error"]


# --------------------------------------------------------------------------
# The shared request pace and the www host
# --------------------------------------------------------------------------

def test_a_waiting_request_does_not_hold_up_one_that_is_out_of_time():
    client = connector._Client(ARCHIVE.url, min_interval=0.6, timeout=2, backoff=())
    start = time.monotonic()
    client._reserve("first", start + 30)
    waiter = threading.Thread(target=client._reserve, args=("second", start + 30))
    waiter.start()
    while client._next_slot < start + 1.0 and time.monotonic() < start + 2:
        time.sleep(0.01)
    t0 = time.monotonic()
    try:
        client._reserve("third", t0 + 0.5)
        raise AssertionError("booked a slot past the deadline")
    except connector._OutOfTime:
        pass
    assert time.monotonic() - t0 < 0.3  # it did not wait behind the second request's sleep
    waiter.join()


def test_a_call_with_no_request_slot_left_ends_cleanly_and_the_next_run_resumes():
    srv = server()
    srv._client._next_slot = time.monotonic() + 1000  # other tables have booked every slot this call could use
    res = export(srv, "delivery_daily", since=LAST_RUN)
    assert res["success"] is True and res["data"] == [] and res["has_more"] is True
    assert res["next_cursor"] == "2025-01-30:0" and watermark(res)["value"] == LAST_RUN
    assert any("one request a second, shared by every table" in w for w in res["warnings"]), res
    assert ARCHIVE.hits == []


def test_the_www_host_comes_from_the_environment_never_from_the_request():
    saved = os.environ.get("NSE_WWW_BASE_URL")
    try:
        os.environ.pop("NSE_WWW_BASE_URL", None)
        assert NseIndiaMCPServer(base_url=ARCHIVE.url)._www.base_url == "https://www.nseindia.com"
        os.environ["NSE_WWW_BASE_URL"] = ARCHIVE.url + "/"
        ARCHIVE.reset()
        srv = NseIndiaMCPServer(base_url=ARCHIVE.url, min_interval=0, backoff=(0, 0), timeout=2, today=lambda: TODAY)
    finally:
        if saved is None:
            os.environ.pop("NSE_WWW_BASE_URL", None)
        else:
            os.environ["NSE_WWW_BASE_URL"] = saved
    assert srv._www.base_url == ARCHIVE.url
    serve_json(FII_DII_PATH, [])
    res = srv.export({"table": "fii_dii_cash", "www_base_url": "http://203.0.113.9",
                      "config": {"www_base_url": "http://203.0.113.9", "base_url": "http://203.0.113.9"}})
    assert res["success"] is True and res["data"] == [] and ARCHIVE.hits_for(FII_DII_PATH) == 1


# --------------------------------------------------------------------------
# Transport: `python connector.py` the way the orchestrator and the image start it
# --------------------------------------------------------------------------

def _env(**extra):
    env = {k: v for k, v in os.environ.items() if k not in ("MCP_HTTP_MODE", "DOCKER_CONTAINER", "MCP_PORT", "PORT")}
    env.update(NSE_ARCHIVE_BASE_URL=ARCHIVE.url, NSE_WWW_BASE_URL=ARCHIVE.url, **extra)
    return env


def _call(rid, op, arguments):
    return {"jsonrpc": "2.0", "id": rid, "method": "tools/call",
            "params": {"name": f"nse-india_{op}", "arguments": arguments}}


def test_stdio_mode_answers_one_json_line_per_request():
    # The orchestrator's fallback (server_manager.go) starts the connector with
    # MCP_HTTP_MODE=false and no DOCKER_CONTAINER, writes one request per line
    # to stdin and reads one JSON value back from stdout.
    ARCHIVE.reset()
    publish_equity_list()
    requests = [
        _call(1, "test_connection", {"config": {}}),
        _call(2, "get_capabilities", {}),
        _call(3, "export", {"table": "equity_list", "limit": 2, "offset": 0}),
        _call(4, "drop_table", {}),
    ]
    stdin = "\n".join(json.dumps(r) for r in requests[:2]) + "\n{not json\n\n" + \
        "\n".join(json.dumps(r) for r in requests[2:]) + "\n"
    proc = subprocess.run([sys.executable, "connector.py"], cwd=HERE, input=stdin, capture_output=True,
                          text=True, timeout=20, env=_env(MCP_HTTP_MODE="false"))
    assert proc.returncode == 0, proc.stderr
    out = [json.loads(line) for line in proc.stdout.splitlines()]  # nothing but JSON on stdout
    assert [r["id"] for r in out] == [1, 2, None, 3, 4]
    by_id = {r["id"]: r for r in out}
    assert by_id[1]["result"]["success"] is True and "3 listed equities" in by_id[1]["result"]["message"]
    assert by_id[2]["result"]["capabilities"]["supports_incremental"] is True
    assert [r["symbol"] for r in by_id[3]["result"]["data"]] == ["ALPHA", "BETA"]
    assert by_id[None]["error"]["code"] == -32700
    assert by_id[4]["error"]["code"] == -32601
    assert ARCHIVE.hits_for(EQUITY_LIST_PATH) == 1  # one process answered every line, so the list was cached


def test_stdio_mode_answers_while_stdin_stays_open():
    # The orchestrator keeps one process per connector and writes each call to
    # its open stdin (client.go executeViaStdio): an answer that waits for stdin
    # to close never arrives.
    proc = subprocess.Popen([sys.executable, "connector.py"], cwd=HERE, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                            env=_env(MCP_HTTP_MODE="false"))
    try:
        for rid in (1, 2):
            proc.stdin.write(json.dumps(_call(rid, "get_capabilities", {})) + "\n")
            proc.stdin.flush()
            ready, _, _ = select.select([proc.stdout], [], [], 10)
            assert ready, f"no answer to request {rid} within 10 s"
            assert json.loads(proc.stdout.readline())["id"] == rid
    finally:
        proc.kill()
        proc.wait(timeout=10)
        for pipe in (proc.stdin, proc.stdout, proc.stderr):
            pipe.close()


def test_http_mode_still_serves_mcp_when_asked():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    proc = subprocess.Popen([sys.executable, "connector.py"], cwd=HERE, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env=_env(MCP_HTTP_MODE="true", MCP_PORT=str(port)))
    try:
        body = json.dumps(_call(7, "get_capabilities", {})).encode()
        deadline = time.monotonic() + 10
        while True:
            try:
                req = urllib.request.Request(f"http://127.0.0.1:{port}/mcp", data=body,
                                             headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(req, timeout=2) as resp:
                    answer = json.loads(resp.read())
                break
            except OSError:
                assert proc.poll() is None and time.monotonic() < deadline, proc.stderr.read() if proc.poll() is not None else "no answer"
                time.sleep(0.1)
        assert answer["id"] == 7 and answer["result"]["capabilities"]["supports_incremental"] is True
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        proc.stdout.close()
        proc.stderr.close()


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except Exception as e:  # a KeyError is as much a failure as a failed assert
                failures += 1
                print(f"FAIL {name}: {type(e).__name__}: {e}")
    raise SystemExit(1 if failures else 0)
