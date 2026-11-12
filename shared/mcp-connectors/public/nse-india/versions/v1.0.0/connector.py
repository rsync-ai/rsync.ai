#!/usr/bin/env python3
"""
NSE India MCP Connector (public) — a SOURCE for the National Stock
Exchange of India's public end-of-day files.

Reads twelve tables. Seven walk the calendar, one file per day (or per year):
  bhavcopy_daily        the capital-market bhavcopy in the UDiFF layout (one zipped
                        CSV per trading day, from 2024-07-08 on), keyed by
                        (trade_date, symbol, series).
  bhavcopy_history      the older bhavcopy (cm<DD><MON><YYYY>bhav.csv.zip), from
                        2016-01-01 to 2024-07-05, its last day, with the same
                        column names as bhavcopy_daily.
  delivery_daily        sec_bhavdata_full: prices plus delivery quantity and %,
                        from 2019-09-30 on.
  index_daily           ind_close_all: every NSE index's close, India VIX included.
  participant_oi_daily  F&O open interest by client type (Client, DII, FII, Pro).
  participant_vol_daily F&O traded volume by client type.
  corporate_actions     splits, bonuses, dividends, one www API call per calendar
                        year of ex-dates.
These six day walks are incremental on trade_date, corporate_actions on ex_date.
Five are snapshots, read in full on every run and keyed so that the destination's
upsert keeps every day's rows:
  equity_list           EQUITY_L.csv, the list of listed equities, keyed by symbol.
  bulk_deals            today's bulk deals (bulk.csv).
  block_deals           today's block deals (block.csv).
  fii_dii_cash          the latest day's FII/FPI and DII cash buy, sell and net
                        (www API; NSE keeps no history of it).
  index_constituents    the NIFTY 500 and NIFTY COMMODITIES lists with each
                        company's industry, keyed by the day they were read.
bulk_deals, block_deals and fii_dii_cash only ever hold the latest day, so a day
no run reads is lost; a reload of the pipeline drops what earlier runs kept.

Implements the minimal SOURCE contract the orchestrator batch executor calls
(tool name = "<connector_type>_<operation>"):
  nse-india_test_connection   -> {success, message, version}
  nse-india_validate_config   -> {success, valid, errors, warnings}
  nse-india_discover_schema   -> {success, tables:[...], total_tables}
  nse-india_export            -> {success, data, columns, row_count, has_more, next_cursor?, stats?}
  nse-india_get_capabilities  -> {success, capabilities}

The connection has no settings. Incremental (INCREMENTAL.md §2-§4): a walk
starts at the day after `since` (or the table's first day on the first run), goes
up to today (Indian time) or the table's last day, and reports
`stats.watermark = {field, value: <last day fully emitted>}`. A day with no file
(a holiday, a weekend, or today before NSE publishes) is skipped and never
advances the watermark, so it is asked for again on the next run until a later
day is read. A year advances the watermark only once it is over, so the current
year of corporate actions is read again on every run. Any other failure stops
the walk where it is. A page holds at most DAYS_PER_PAGE files that have rows
(SMALL_FILE_DAYS_PER_PAGE for the small index and participant files): the batch
executor ends a table at the first page with no rows, so days with no file never
end a page on their own while there is time left.

The HTTP client is deliberately plain: an honest User-Agent, about one request a
second per host, a bounded retry on resets and 5xx, no cookies and no browser
impersonation. If NSE refuses such a client the connector fails with that error;
it does not work around bot protection. Tables run in parallel and share that
pace: each request books the next free second without holding up the others,
and a call that cannot get a second before its deadline stops where it is.

Two fixed hosts: the archive (NSE_ARCHIVE_BASE_URL, tests pass base_url) for the
files, and www.nseindia.com (NSE_WWW_BASE_URL, tests pass www_base_url) for the
corporate-action and FII/DII APIs. Neither comes from the connection config, so
a pipeline cannot point the connector at another host. Redirects to another host
are refused.

Self-contained stdlib JSON-RPC skeleton, cloned from sample-data (no
base_connector, no third-party deps): HTTP on /mcp in the image
(MCP_HTTP_MODE=true), one request per line on stdin/stdout when the orchestrator
starts it as a subprocess. Prices are emitted as decimal strings, so no
float rounding reaches the destination.
"""

import csv
import io
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile
from collections import OrderedDict
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal, InvalidOperation
from http.client import HTTPException
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

CONNECTOR_TYPE = "nse-india"
CONNECTOR_VERSION = "1.0.0"

DEFAULT_BASE_URL = "https://nsearchives.nseindia.com"
DEFAULT_WWW_BASE_URL = "https://www.nseindia.com"
USER_AGENT = "rsync.ai-nse-india/1.0"

# NSE replaced the old CM bhavcopy with the UDiFF file from this trading day on.
UDIFF_START = date(2024, 7, 8)
HISTORY_START = date(2016, 1, 1)
HISTORY_FINAL = UDIFF_START - timedelta(days=1)
# The first sec_bhavdata_full file with delivery data.
DELIVERY_START = date(2019, 9, 30)
# Files with rows per export call; days without one cost a request each and are
# bounded by the call budget instead.
DAYS_PER_PAGE = 5
SMALL_FILE_DAYS_PER_PAGE = 30
DEFAULT_LIMIT = 10000

# NSE publishes on Indian time; a fixed offset needs no tz database.
IST = timezone(timedelta(hours=5, minutes=30))

MAX_DOWNLOAD_BYTES = 20 * 1024 * 1024
MAX_UNZIPPED_BYTES = 50 * 1024 * 1024

# A day this old with no file never gets one; remembering it saves a request when
# a later run walks past it again.
MISSING_DAY_MIN_AGE = 7
MISSING_CACHE_SIZE = 20000
# Parsed files kept per table, so a file split across pages is downloaded once.
UNIT_CACHE_PER_TABLE = 4
CACHE_TTL = 600.0

WATERMARK_FIELD = "trade_date"


def _col(name: str, type_: str, nullable: bool = True) -> Dict[str, Any]:
    return {"name": name, "type": type_, "nullable": nullable}


# (output column, UDiFF header normalized by _norm_header, kind)
_BHAVCOPY_FIELDS: List[Tuple[str, str, str]] = [
    ("isin", "ISIN", "string"),
    ("instrument_type", "FININSTRMTP", "string"),
    ("instrument_id", "FININSTRMID", "string"),
    ("instrument_name", "FININSTRMNM", "string"),
    ("biz_date", "BIZDT", "date"),
    ("segment", "SGMT", "string"),
    ("source", "SRC", "string"),
    ("open", "OPNPRIC", "decimal"),
    ("high", "HGHPRIC", "decimal"),
    ("low", "LWPRIC", "decimal"),
    ("close", "CLSPRIC", "decimal"),
    ("last", "LASTPRIC", "decimal"),
    ("prev_close", "PRVSCLSGPRIC", "decimal"),
    ("settlement_price", "STTLMPRIC", "decimal"),
    ("volume", "TTLTRADGVOL", "integer"),
    ("turnover", "TTLTRFVAL", "decimal"),
    ("trades", "TTLNBOFTXSEXCTD", "integer"),
    ("board_lot", "NEWBRDLOTQTY", "integer"),
]
_BHAVCOPY_REQUIRED = ["TRADDT", "TCKRSYMB", "SCTYSRS", "OPNPRIC", "HGHPRIC", "LWPRIC", "CLSPRIC"]

# The old bhavcopy, under bhavcopy_daily's column names.
_HISTORY_FIELDS: List[Tuple[str, str, str]] = [
    ("isin", "ISIN", "string"),
    ("open", "OPEN", "decimal"),
    ("high", "HIGH", "decimal"),
    ("low", "LOW", "decimal"),
    ("close", "CLOSE", "decimal"),
    ("last", "LAST", "decimal"),
    ("prev_close", "PREVCLOSE", "decimal"),
    ("volume", "TOTTRDQTY", "integer"),
    ("turnover", "TOTTRDVAL", "decimal"),
    ("trades", "TOTALTRADES", "integer"),
]
_HISTORY_REQUIRED = ["SYMBOL", "SERIES", "OPEN", "HIGH", "LOW", "CLOSE", "TIMESTAMP"]

_DELIVERY_FIELDS: List[Tuple[str, str, str]] = [
    ("prev_close", "PREV_CLOSE", "decimal"),
    ("open", "OPEN_PRICE", "decimal"),
    ("high", "HIGH_PRICE", "decimal"),
    ("low", "LOW_PRICE", "decimal"),
    ("last", "LAST_PRICE", "decimal"),
    ("close", "CLOSE_PRICE", "decimal"),
    ("avg_price", "AVG_PRICE", "decimal"),
    ("volume", "TTL_TRD_QNTY", "integer"),
    ("turnover_lacs", "TURNOVER_LACS", "decimal"),
    ("trades", "NO_OF_TRADES", "integer"),
    ("delivery_qty", "DELIV_QTY", "integer"),
    ("delivery_pct", "DELIV_PER", "decimal"),
]
_DELIVERY_REQUIRED = ["SYMBOL", "SERIES", "DATE1", "CLOSE_PRICE", "TTL_TRD_QNTY", "DELIV_QTY", "DELIV_PER"]

_INDEX_FIELDS: List[Tuple[str, str, str]] = [
    ("open", "OPEN_INDEX_VALUE", "decimal"),
    ("high", "HIGH_INDEX_VALUE", "decimal"),
    ("low", "LOW_INDEX_VALUE", "decimal"),
    ("close", "CLOSING_INDEX_VALUE", "decimal"),
    ("points_change", "POINTS_CHANGE", "decimal"),
    ("change_pct", "CHANGE(%)", "decimal"),
    ("volume", "VOLUME", "decimal"),
    ("turnover_cr", "TURNOVER_(RS._CR.)", "decimal"),
    ("pe", "P/E", "decimal"),
    ("pb", "P/B", "decimal"),
    ("div_yield", "DIV_YIELD", "decimal"),
]
_INDEX_REQUIRED = ["INDEX_NAME", "INDEX_DATE", "CLOSING_INDEX_VALUE"]

_PARTICIPANT_FIELDS: List[Tuple[str, str, str]] = [
    (header.lower(), header, "integer")
    for header in (
        "FUTURE_INDEX_LONG", "FUTURE_INDEX_SHORT", "FUTURE_STOCK_LONG", "FUTURE_STOCK_SHORT",
        "OPTION_INDEX_CALL_LONG", "OPTION_INDEX_PUT_LONG", "OPTION_INDEX_CALL_SHORT", "OPTION_INDEX_PUT_SHORT",
        "OPTION_STOCK_CALL_LONG", "OPTION_STOCK_PUT_LONG", "OPTION_STOCK_CALL_SHORT", "OPTION_STOCK_PUT_SHORT",
        "TOTAL_LONG_CONTRACTS", "TOTAL_SHORT_CONTRACTS",
    )
]
_PARTICIPANT_REQUIRED = ["CLIENT_TYPE"] + [header for _, header, _ in _PARTICIPANT_FIELDS]

# (output column, key in the API's JSON, kind)
_CORPORATE_ACTION_FIELDS: List[Tuple[str, str, str]] = [
    ("company_name", "comp", "string"),
    ("isin", "isin", "string"),
    ("face_value", "faceVal", "decimal"),
    ("record_date", "recDate", "date"),
    ("bc_start_date", "bcStartDate", "date"),
    ("bc_end_date", "bcEndDate", "date"),
    ("nd_start_date", "ndStartDate", "date"),
    ("nd_end_date", "ndEndDate", "date"),
    ("broadcast_date", "caBroadcastDate", "string"),
]

_DEALS_REQUIRED = ["DATE", "SYMBOL", "CLIENT_NAME", "BUY/SELL", "QUANTITY_TRADED"]

_FII_DII_FIELDS: List[Tuple[str, str, str]] = [
    ("buy_value", "buyValue", "decimal"),
    ("sell_value", "sellValue", "decimal"),
    ("net_value", "netValue", "decimal"),
]

_INDEX_LIST_FIELDS: List[Tuple[str, str, str]] = [
    ("company_name", "COMPANY_NAME", "string"),
    ("industry", "INDUSTRY", "string"),
    ("series", "SERIES", "string"),
    ("isin", "ISIN_CODE", "string"),
]
_INDEX_LIST_REQUIRED = ["COMPANY_NAME", "INDUSTRY", "SYMBOL", "ISIN_CODE"]

_EQUITY_FIELDS: List[Tuple[str, str, str]] = [
    ("company_name", "NAME_OF_COMPANY", "string"),
    ("series", "SERIES", "string"),
    ("listing_date", "DATE_OF_LISTING", "date"),
    ("paid_up_value", "PAID_UP_VALUE", "decimal"),
    ("market_lot", "MARKET_LOT", "integer"),
    ("isin", "ISIN_NUMBER", "string"),
    ("face_value", "FACE_VALUE", "decimal"),
]
_EQUITY_REQUIRED = ["SYMBOL", "NAME_OF_COMPANY", "ISIN_NUMBER"]


def _cols(fields: List[Tuple[str, str, str]]) -> List[Dict[str, Any]]:
    return [_col(name, kind) for name, _, kind in fields]


_DAY_SYMBOL_SERIES = [_col("trade_date", "date", False), _col("symbol", "string", False), _col("series", "string", False)]
_DEAL_KEY_COLUMNS = [
    _col("deal_date", "date", False),
    _col("symbol", "string", False),
    _col("client_name", "string", False),
    _col("buy_sell", "string", False),
    _col("quantity", "integer", False),
    _col("price", "decimal", False),
    _col("occurrence", "integer", False),
]
_DEAL_KEY = ["deal_date", "symbol", "client_name", "buy_sell", "quantity", "price", "occurrence"]

TABLES: Dict[str, Dict[str, Any]] = {
    "bhavcopy_daily": {
        "description": "Capital-market end-of-day prices per trading day (UDiFF bhavcopy).",
        "columns": list(_DAY_SYMBOL_SERIES) + _cols(_BHAVCOPY_FIELDS),
        "primary_key": ["trade_date", "symbol", "series"],
        "incremental_field": WATERMARK_FIELD,
    },
    "equity_list": {
        "description": "Equities listed on NSE (EQUITY_L.csv); a full refresh each run.",
        "columns": [_col("symbol", "string", False)] + _cols(_EQUITY_FIELDS),
        "primary_key": ["symbol"],
        "incremental_field": None,
    },
    "bhavcopy_history": {
        "description": "Capital-market end-of-day prices from 2016-01-01 to 2024-07-05 (the bhavcopy before UDiFF).",
        "columns": list(_DAY_SYMBOL_SERIES) + _cols(_HISTORY_FIELDS),
        "primary_key": ["trade_date", "symbol", "series"],
        "incremental_field": WATERMARK_FIELD,
    },
    "delivery_daily": {
        "description": "Prices with delivery quantity and delivery % per trading day (sec_bhavdata_full), from 2019-09-30.",
        "columns": list(_DAY_SYMBOL_SERIES) + _cols(_DELIVERY_FIELDS),
        "primary_key": ["trade_date", "symbol", "series"],
        "incremental_field": WATERMARK_FIELD,
    },
    "index_daily": {
        "description": "Every NSE index's end-of-day values, India VIX included (ind_close_all), from 2016-01-01.",
        "columns": [_col("trade_date", "date", False), _col("index_name", "string", False)] + _cols(_INDEX_FIELDS),
        "primary_key": ["trade_date", "index_name"],
        "incremental_field": WATERMARK_FIELD,
    },
    "participant_oi_daily": {
        "description": "F&O open interest (contracts) by client type: Client, DII, FII, Pro, TOTAL; from 2016-01-01.",
        "columns": [_col("trade_date", "date", False), _col("client_type", "string", False)]
        + _cols(_PARTICIPANT_FIELDS),
        "primary_key": ["trade_date", "client_type"],
        "incremental_field": WATERMARK_FIELD,
    },
    "participant_vol_daily": {
        "description": "F&O traded volume (contracts) by client type: Client, DII, FII, Pro, TOTAL; from 2016-01-01.",
        "columns": [_col("trade_date", "date", False), _col("client_type", "string", False)]
        + _cols(_PARTICIPANT_FIELDS),
        "primary_key": ["trade_date", "client_type"],
        "incremental_field": WATERMARK_FIELD,
    },
    "corporate_actions": {
        "description": "Equity corporate actions (splits, bonuses, dividends) by ex-date, from 2016; "
        "the current year is read again each run.",
        "columns": [
            _col("symbol", "string", False),
            _col("series", "string", False),
            _col("ex_date", "date", False),
            _col("subject", "string", False),
        ] + _cols(_CORPORATE_ACTION_FIELDS),
        "primary_key": ["symbol", "series", "ex_date", "subject"],
        "incremental_field": "ex_date",
    },
    "bulk_deals": {
        "description": "Today's bulk deals (bulk.csv); each run keeps that day's rows.",
        "columns": list(_DEAL_KEY_COLUMNS) + [_col("security_name", "string"), _col("remarks", "string")],
        "primary_key": list(_DEAL_KEY),
        "incremental_field": None,
    },
    "block_deals": {
        "description": "Today's block deals (block.csv); each run keeps that day's rows.",
        "columns": list(_DEAL_KEY_COLUMNS) + [_col("security_name", "string")],
        "primary_key": list(_DEAL_KEY),
        "incremental_field": None,
    },
    "fii_dii_cash": {
        "description": "The latest day's FII/FPI and DII cash-market buy, sell and net (Rs crore); "
        "each run keeps that day's rows.",
        "columns": [_col("trade_date", "date", False), _col("category", "string", False)] + _cols(_FII_DII_FIELDS),
        "primary_key": ["trade_date", "category"],
        "incremental_field": None,
    },
    "index_constituents": {
        "description": "NIFTY 500 and NIFTY COMMODITIES members with their industry, keyed by the day they were read.",
        "columns": [
            _col("captured_on", "date", False),
            _col("index_name", "string", False),
            _col("symbol", "string", False),
        ] + _cols(_INDEX_LIST_FIELDS),
        "primary_key": ["captured_on", "index_name", "symbol"],
        "incremental_field": None,
    },
}


def _column_names(table: str) -> List[str]:
    return [c["name"] for c in TABLES[table]["columns"]]


def _resolve_table(raw: Any) -> str:
    """Normalize an incoming table name — tolerate schema-qualified or quoted
    forms (e.g. "public"."bhavcopy_daily") and return the bare table key."""
    return str(raw or "").split(".")[-1].strip().strip('"').strip("`").strip("'")


_MONTHS = ("JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC")


def _bhavcopy_path(day: date) -> str:
    return f"/content/cm/BhavCopy_NSE_CM_0_0_0_{day.strftime('%Y%m%d')}_F_0000.csv.zip"


def _history_path(day: date) -> str:
    mon = _MONTHS[day.month - 1]
    return f"/content/historical/EQUITIES/{day.year}/{mon}/cm{day.day:02d}{mon}{day.year}bhav.csv.zip"


def _delivery_path(day: date) -> str:
    return f"/products/content/sec_bhavdata_full_{day:%d%m%Y}.csv"


def _index_close_path(day: date) -> str:
    return f"/content/indices/ind_close_all_{day:%d%m%Y}.csv"


def _participant_path(kind: str, day: date) -> str:
    return f"/content/nsccl/fao_participant_{kind}_{day:%d%m%Y}.csv"


def _corporate_actions_path(year_start: date) -> str:
    y = year_start.year
    return f"/api/corporates-corporateActions?index=equities&from_date=01-01-{y}&to_date=31-12-{y}"


EQUITY_LIST_PATH = "/content/equities/EQUITY_L.csv"
BULK_DEALS_PATH = "/content/equities/bulk.csv"
BLOCK_DEALS_PATH = "/content/equities/block.csv"
INDEX_LIST_PATHS = {
    "NIFTY 500": "/content/indices/ind_nifty500list.csv",
    "NIFTY COMMODITIES": "/content/indices/ind_niftycommoditieslist.csv",
}
FII_DII_PATH = "/api/fiidiiTradeReact"


# --------------------------------------------------------------------------
# Value parsing
# --------------------------------------------------------------------------

class _BadValue(ValueError):
    pass


def _norm_header(h: str) -> str:
    return re.sub(r"\s+", "_", str(h or "").strip().lstrip("﻿").strip()).upper()


_DATE_FORMATS = ("%Y-%m-%d", "%d-%b-%Y", "%d-%m-%Y", "%Y%m%d", "%d/%m/%Y")


def _parse_date_value(raw: str) -> date:
    s = raw.strip()
    for fmt in _DATE_FORMATS:
        try:
            return datetime.strptime(s, fmt).date()
        except ValueError:
            continue
    raise _BadValue(f"not a date: {raw!r}")


def _blank(raw: Optional[str]) -> bool:
    return raw is None or raw.strip() in ("", "-")


def _convert(raw: Any, kind: str) -> Any:
    if raw is not None and not isinstance(raw, str):
        raw = str(raw)  # a JSON number or the like
    if _blank(raw):
        return None
    s = raw.strip()
    if kind == "string":
        return s
    if kind == "date":
        return _parse_date_value(s).isoformat()
    try:
        d = Decimal(s.replace(",", ""))
    except InvalidOperation:
        raise _BadValue(f"not a number: {raw!r}")
    if not d.is_finite():
        raise _BadValue(f"not a number: {raw!r}")
    if kind == "integer":
        if d != d.to_integral_value():
            raise _BadValue(f"not a whole number: {raw!r}")
        return int(d)
    # decimal: the source's own digits, as a string — never through float
    return s.replace(",", "")


def _key_text(raw: Any) -> Optional[str]:
    """A key column's text, or None when blank (the row is then skipped)."""
    s = "" if raw is None else str(raw).strip()
    return s or None


def _text(raw: Any) -> str:
    return "" if raw is None else str(raw).strip()


class _Rows:
    """Converted rows of one file, how many values would not parse, and notes
    on rows that were dropped."""

    def __init__(self) -> None:
        self.rows: List[Dict[str, Any]] = []
        self.bad: Dict[str, int] = {}
        self.notes: List[str] = []

    def warnings(self, what: str) -> List[str]:
        return [
            f"{what}: {n} value(s) in column {col} did not parse and were left null"
            for col, n in sorted(self.bad.items())
        ] + list(self.notes)


def _convert_fields(raw: Dict[str, Any], fields: List[Tuple[str, str, str]], result: _Rows) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for name, header, kind in fields:
        try:
            out[name] = _convert(raw.get(header), kind)
        except _BadValue:
            out[name] = None
            result.bad[name] = result.bad.get(name, 0) + 1
    return out


def _collate(result: _Rows, rows: List[Dict[str, Any]], key: List[str], what: str) -> None:
    """Keep one row per key. Unlike bhavcopy_daily, a repeat does not fail the
    file: one odd row in years of history would otherwise stop the walk there
    on every run. Identical repeats collapse; a conflicting one keeps the first
    and says so; a row with no value in a key column is skipped and counted."""
    kept: Dict[Tuple[Any, ...], Dict[str, Any]] = {}
    conflicts: List[Tuple[Any, ...]] = []
    no_key = 0
    for row in rows:
        k = tuple(row.get(f) for f in key)
        if any(v is None for v in k):
            no_key += 1
            continue
        first = kept.get(k)
        if first is None:
            kept[k] = row
            result.rows.append(row)
        elif first != row:
            conflicts.append(k)
    if no_key:
        result.notes.append(f"{what}: skipped {no_key} row(s) with no value in a key column {key}")
    if conflicts:
        result.notes.append(
            f"{what}: {len(conflicts)} row(s) repeat a key {key} with other values, e.g. {conflicts[:3]}; "
            "kept the first"
        )


def _check_file_day(file_days: Set[date], day: date, what: str, column: str) -> None:
    """Every dated row must carry the day asked for. A file whose rows all carry
    one earlier day is NSE re-publishing that day's file under a later name
    (Sunday 2026-11-01's delivery file is a byte copy of 2026-10-30's): it holds
    nothing for the day asked, so the walk treats it like a day with no file."""
    if not file_days or file_days == {day}:
        return
    if len(file_days) == 1 and min(file_days) < day:
        raise _EarlierCopy(min(file_days))
    other = min(d for d in file_days if d != day)
    raise _SourceError(
        f"the {what} file for {day.isoformat()} carries {column} {other.isoformat()}; "
        "refusing to label one day's data with another day's date"
    )


_NUMERIC_DATE = re.compile(r"(\d{1,2})([-/])(\d{1,2})\2(\d{4})")


def _month_first(value: str) -> Optional[date]:
    m = _NUMERIC_DATE.fullmatch(value.strip())
    if not m:
        return None
    try:
        return date(int(m.group(4)), int(m.group(1)), int(m.group(3)))
    except ValueError:
        return None


def _dates_in(raw_rows: List[Dict[str, str]], column: str, day: date) -> Set[date]:
    """A numeric date reads day first, or month first when that is the day asked
    or the only reading: NSE writes some index close files month first (2023-04-06's
    says 04-06-2023, 2023-04-13's says 13-04-2023). Read day first only, 2023-04-06's
    file would carry 4 June and stop the walk, and 2023-11-05's (11-05-2023) would
    pass for an earlier day's copy and be skipped."""
    out = set()
    for raw in raw_rows:
        value = raw.get(column)
        if _blank(value):
            continue
        swapped = _month_first(value)
        if swapped == day:
            out.add(day)
            continue
        try:
            out.add(_parse_date_value(value))
        except _BadValue:
            if swapped is not None:
                out.add(swapped)  # 01-31-2025: still another day's date, so refused
    return out


def _read_csv(text: str, required: List[str], what: str) -> Tuple[List[Dict[str, str]], List[str]]:
    # newline="": some NSE files end lines with a bare \r (participant files for
    # 2017-10-04), which the csv module reads only when it sees the raw endings.
    reader = csv.reader(io.StringIO(text, newline=""))
    try:
        header = next(reader)
        keys = [_norm_header(h) for h in header]
        missing = [r for r in required if r not in keys]
        if missing:
            raise _SourceError(
                f"{what} does not have the expected columns {missing}; headers found: {header}. "
                "NSE may have changed the file layout."
            )
        out = []
        for line in reader:
            if not any(cell.strip() for cell in line):
                continue
            out.append({k: (line[i] if i < len(line) else None) for i, k in enumerate(keys)})
    except StopIteration:
        raise _SourceError(f"{what} is empty")
    except csv.Error as e:
        raise _SourceError(f"{what} could not be read as CSV ({e})")
    return out, keys


def parse_bhavcopy(text: str, day: date) -> _Rows:
    raw_rows, _ = _read_csv(text, _BHAVCOPY_REQUIRED, f"bhavcopy {day.isoformat()}")
    result = _Rows()
    seen: Dict[Tuple[str, str], int] = {}
    for raw in raw_rows:
        symbol = (raw.get("TCKRSYMB") or "").strip()
        if not symbol:
            continue
        key = (symbol, (raw.get("SCTYSRS") or "").strip())
        seen[key] = seen.get(key, 0) + 1
        traded = raw.get("TRADDT")
        if not _blank(traded):
            try:
                file_day = _parse_date_value(traded)
            except _BadValue:
                file_day = None
            if file_day is not None and file_day != day:
                raise _SourceError(
                    f"the bhavcopy file for {day.isoformat()} carries TradDt {file_day.isoformat()}; "
                    "refusing to label one day's prices with another day's date"
                )
        row: Dict[str, Any] = {
            "trade_date": day.isoformat(),
            "symbol": symbol,
            "series": (raw.get("SCTYSRS") or "").strip(),
        }
        for name, header, kind in _BHAVCOPY_FIELDS:
            try:
                row[name] = _convert(raw.get(header), kind)
            except _BadValue:
                row[name] = None
                result.bad[name] = result.bad.get(name, 0) + 1
        result.rows.append(row)
    dupes = sorted(k for k, n in seen.items() if n > 1)
    if dupes:
        # (trade_date, symbol, series) is the key; two rows for one key would collapse at the destination
        raise _SourceError(
            f"bhavcopy {day.isoformat()} lists {len(dupes)} (symbol, series) pair(s) more than once, e.g. {dupes[:5]}"
        )
    return result


def _parse_symbol_day(
    text: str, day: date, what: str, required: List[str], date_column: str,
    fields: List[Tuple[str, str, str]], table: str,
) -> _Rows:
    raw_rows, _ = _read_csv(text, required, what)
    _check_file_day(_dates_in(raw_rows, date_column, day), day, what.rsplit(" ", 1)[0], date_column)
    result = _Rows()
    rows = []
    for raw in raw_rows:
        row: Dict[str, Any] = {
            "trade_date": day.isoformat(),
            "symbol": _key_text(raw.get("SYMBOL")),
            "series": _text(raw.get("SERIES")),
        }
        row.update(_convert_fields(raw, fields, result))
        rows.append(row)
    _collate(result, rows, TABLES[table]["primary_key"], what)
    return result


def parse_bhavcopy_history(text: str, day: date) -> _Rows:
    return _parse_symbol_day(
        text, day, f"bhavcopy history {day.isoformat()}", _HISTORY_REQUIRED, "TIMESTAMP",
        _HISTORY_FIELDS, "bhavcopy_history",
    )


def parse_delivery(text: str, day: date) -> _Rows:
    return _parse_symbol_day(
        text, day, f"delivery {day.isoformat()}", _DELIVERY_REQUIRED, "DATE1", _DELIVERY_FIELDS, "delivery_daily",
    )


def parse_index_close(text: str, day: date) -> _Rows:
    what = f"index close {day.isoformat()}"
    raw_rows, _ = _read_csv(text, _INDEX_REQUIRED, what)
    _check_file_day(_dates_in(raw_rows, "INDEX_DATE", day), day, "index close", "Index Date")
    result = _Rows()
    rows = []
    for raw in raw_rows:
        row: Dict[str, Any] = {"trade_date": day.isoformat(), "index_name": _key_text(raw.get("INDEX_NAME"))}
        row.update(_convert_fields(raw, _INDEX_FIELDS, result))
        rows.append(row)
    _collate(result, rows, TABLES["index_daily"]["primary_key"], what)
    return result


_TITLE_DATE_RE = re.compile(r"as on\s+([A-Za-z]{3})[A-Za-z]*\s+(\d{1,2})\s*,\s*(\d{4})", re.IGNORECASE)


def _split_preamble(text: str, day: date, what: str) -> str:
    """The participant files open with a title line ("... as on Jan 04, 2016")
    above the header. Drop it; a title naming an earlier day is a copy of that
    day's file (see _check_file_day), one naming a later day is refused."""
    parts = re.split(r"\r\n|\r|\n", text, maxsplit=1)
    first, rest = parts[0], parts[1] if len(parts) > 1 else ""
    if "client type" in first.lower():
        return text
    m = _TITLE_DATE_RE.search(first)
    if m:
        try:
            title_day = datetime.strptime(f"{m.group(1).title()} {m.group(2)} {m.group(3)}", "%b %d %Y").date()
        except ValueError:
            title_day = None
        if title_day is not None and title_day < day:
            raise _EarlierCopy(title_day)
        if title_day is not None and title_day != day:
            raise _SourceError(
                f"the {what.rsplit(' ', 1)[0]} file for {day.isoformat()} is titled {title_day.isoformat()}; "
                "refusing to label one day's data with another day's date"
            )
    return rest


def parse_participant(text: str, day: date, kind: str) -> _Rows:
    what = f"participant {'OI' if kind == 'oi' else 'volume'} {day.isoformat()}"
    raw_rows, _ = _read_csv(_split_preamble(text, day, what), _PARTICIPANT_REQUIRED, what)
    result = _Rows()
    rows = []
    for raw in raw_rows:
        row: Dict[str, Any] = {"trade_date": day.isoformat(), "client_type": _key_text(raw.get("CLIENT_TYPE"))}
        row.update(_convert_fields(raw, _PARTICIPANT_FIELDS, result))
        rows.append(row)
    _collate(result, rows, ["trade_date", "client_type"], what)
    return result


def _json_rows(obj: Any, what: str) -> List[Dict[str, Any]]:
    if not isinstance(obj, list):
        raise _SourceError(f"{what}: expected a list from NSE, got {type(obj).__name__}")
    return [item for item in obj if isinstance(item, dict)]


def parse_corporate_actions(obj: Any, year_start: date) -> _Rows:
    what = f"corporate actions {year_start.year}"
    result = _Rows()
    rows = []
    for item in _json_rows(obj, what):
        row: Dict[str, Any] = {
            "symbol": _key_text(item.get("symbol")),
            "series": _text(item.get("series")),
            "subject": _text(item.get("subject")),
        }
        row.update(_convert_fields(item, [("ex_date", "exDate", "date")] + _CORPORATE_ACTION_FIELDS, result))
        rows.append(row)
    _collate(result, rows, TABLES["corporate_actions"]["primary_key"], what)
    return result


def parse_deals(text: str, what: str, with_remarks: bool) -> _Rows:
    result = _Rows()
    if "," not in text:
        # Not a CSV at all (e.g. a "no records" line): nothing was dealt.
        if text.strip():
            result.notes.append(f"{what} lists no deals ({text.strip()[:60]!r})")
        return result
    raw_rows, keys = _read_csv(text, _DEALS_REQUIRED, what)
    price = next((k for k in keys if k.startswith("TRADE_PRICE")), None)
    if price is None:
        raise _SourceError(
            f"{what} has no trade price column; headers found: {keys}. NSE may have changed the file layout."
        )
    fields = [
        ("deal_date", "DATE", "date"),
        ("quantity", "QUANTITY_TRADED", "integer"),
        ("price", price, "decimal"),
        ("security_name", "SECURITY_NAME", "string"),
    ] + ([("remarks", "REMARKS", "string")] if with_remarks else [])
    seen: Dict[Tuple[Any, ...], int] = {}
    rows = []
    for raw in raw_rows:
        row: Dict[str, Any] = {
            "symbol": _key_text(raw.get("SYMBOL")),
            "client_name": _text(raw.get("CLIENT_NAME")),
            "buy_sell": _text(raw.get("BUY/SELL")),
        }
        row.update(_convert_fields(raw, fields, result))
        # Two identical deals in a day are two deals: number them in file order.
        same = tuple(row.get(f) for f in _DEAL_KEY[:-1])
        seen[same] = seen.get(same, 0) + 1
        row["occurrence"] = seen[same]
        rows.append(row)
    _collate(result, rows, _DEAL_KEY, what)
    return result


def parse_fii_dii(obj: Any) -> _Rows:
    what = "FII/DII cash"
    result = _Rows()
    rows = []
    for item in _json_rows(obj, what):
        row: Dict[str, Any] = {"category": _key_text(item.get("category"))}
        row.update(_convert_fields(item, [("trade_date", "date", "date")] + _FII_DII_FIELDS, result))
        rows.append(row)
    _collate(result, rows, TABLES["fii_dii_cash"]["primary_key"], what)
    return result


def parse_index_list(text: str, index_name: str, captured_on: date) -> _Rows:
    what = f"{index_name} list"
    raw_rows, _ = _read_csv(text, _INDEX_LIST_REQUIRED, what)
    result = _Rows()
    rows = []
    for raw in raw_rows:
        row: Dict[str, Any] = {
            "captured_on": captured_on.isoformat(),
            "index_name": index_name,
            "symbol": _key_text(raw.get("SYMBOL")),
        }
        row.update(_convert_fields(raw, _INDEX_LIST_FIELDS, result))
        rows.append(row)
    _collate(result, rows, TABLES["index_constituents"]["primary_key"], what)
    return result


def parse_equity_list(text: str) -> _Rows:
    raw_rows, _ = _read_csv(text, _EQUITY_REQUIRED, "EQUITY_L.csv")
    result = _Rows()
    seen: Dict[str, int] = {}
    for raw in raw_rows:
        symbol = (raw.get("SYMBOL") or "").strip()
        if not symbol:
            continue
        seen[symbol] = seen.get(symbol, 0) + 1
        row: Dict[str, Any] = {"symbol": symbol}
        for name, header, kind in _EQUITY_FIELDS:
            try:
                row[name] = _convert(raw.get(header), kind)
            except _BadValue:
                row[name] = None
                result.bad[name] = result.bad.get(name, 0) + 1
        result.rows.append(row)
    dupes = sorted(s for s, n in seen.items() if n > 1)
    if dupes:
        # symbol is the key; two rows for one symbol would collapse at the destination
        raise _SourceError(f"EQUITY_L.csv lists {len(dupes)} symbol(s) more than once, e.g. {dupes[:5]}")
    return result


def _unzip_single_csv(body: bytes, what: str) -> str:
    try:
        zf = zipfile.ZipFile(io.BytesIO(body))
    except zipfile.BadZipFile:
        raise _SourceError(f"{what} is not a zip file (got {len(body)} bytes starting {body[:16]!r})")
    with zf:
        members = [m for m in zf.infolist() if m.filename.lower().endswith(".csv")]
        if len(members) != 1:
            raise _SourceError(f"{what} should hold one .csv file, holds {[m.filename for m in zf.infolist()]}")
        member = members[0]
        if member.file_size > MAX_UNZIPPED_BYTES:
            raise _SourceError(f"{what} unzips to {member.file_size} bytes, over the {MAX_UNZIPPED_BYTES} limit")
        with zf.open(member) as fh:
            data = fh.read(MAX_UNZIPPED_BYTES + 1)
        if len(data) > MAX_UNZIPPED_BYTES:
            raise _SourceError(f"{what} unzips past the {MAX_UNZIPPED_BYTES} byte limit")
    return data.decode("utf-8-sig", errors="replace")


def _decode(body: bytes) -> str:
    # NSE serves some days' CSV as an Excel workbook under the same .csv name
    # (2022-08-08's sec_bhavdata_full): read its first sheet as that CSV.
    if body.startswith(b"PK\x03\x04"):
        return _xlsx_as_csv(body)
    return body.decode("utf-8-sig", errors="replace")


_XLSX_MAIN = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
_XLSX_REL_ID = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}id"
_XLSX_REL = "{http://schemas.openxmlformats.org/package/2006/relationships}Relationship"
_XLSX_MAX_COLUMNS = 16384  # Excel's own limit, column XFD


def _xlsx_as_csv(body: bytes) -> str:
    try:
        with zipfile.ZipFile(io.BytesIO(body)) as zf:
            return _xlsx_first_sheet(zf)
    except (zipfile.BadZipFile, KeyError, IndexError, ValueError, ET.ParseError) as e:
        raise _SourceError(f"an Excel file in place of a CSV could not be read: {e}")


def _xlsx_first_sheet(zf: zipfile.ZipFile) -> str:
    def part(name: str) -> ET.Element:
        info = zf.getinfo(name)
        if info.file_size > MAX_UNZIPPED_BYTES:
            raise ValueError(f"{name} unzips to {info.file_size} bytes, over the {MAX_UNZIPPED_BYTES} limit")
        with zf.open(info) as fh:
            data = fh.read(MAX_UNZIPPED_BYTES + 1)
        if len(data) > MAX_UNZIPPED_BYTES:
            raise ValueError(f"{name} unzips past the {MAX_UNZIPPED_BYTES} byte limit")
        if b"<!DOCTYPE" in data:
            raise ValueError(f"{name} declares a DOCTYPE, which a workbook never needs")
        return ET.fromstring(data)

    def target(rel: ET.Element) -> str:
        path = rel.get("Target", "")
        return path[1:] if path.startswith("/") else "xl/" + path

    sheet = part("xl/workbook.xml").find(f"{_XLSX_MAIN}sheets/{_XLSX_MAIN}sheet")
    if sheet is None:
        raise ValueError("the workbook has no sheet")
    rels = part("xl/_rels/workbook.xml.rels").findall(_XLSX_REL)
    sheet_rel = next((r for r in rels if r.get("Id") == sheet.get(_XLSX_REL_ID)), None)
    if sheet_rel is None:
        raise ValueError("the workbook does not say where its first sheet is")
    shared: List[str] = []
    for rel in rels:
        if rel.get("Type", "").endswith("/sharedStrings"):
            shared = [_xlsx_text(si) for si in part(target(rel)).findall(f"{_XLSX_MAIN}si")]

    out = io.StringIO()
    writer = csv.writer(out, lineterminator="\n")
    for row in part(target(sheet_rel)).iterfind(f"{_XLSX_MAIN}sheetData/{_XLSX_MAIN}row"):
        values: List[str] = []
        for c in row.findall(f"{_XLSX_MAIN}c"):
            letters = re.match(r"[A-Z]*", c.get("r", "")).group()
            col = len(values)
            if letters:
                col = 0
                for ch in letters:
                    col = col * 26 + ord(ch) - 64
                col -= 1
            if col >= _XLSX_MAX_COLUMNS:
                raise ValueError(f"cell {c.get('r')} is past Excel's last column")
            values.extend([""] * (col - len(values)))
            values.append(_xlsx_cell(c, shared))
        writer.writerow(values)
    return out.getvalue()


def _xlsx_text(node: ET.Element) -> str:
    # A string is one <t>, or rich-text runs <r><t> each holding a piece of it.
    pieces = node.findall(f"{_XLSX_MAIN}t") + node.findall(f"{_XLSX_MAIN}r/{_XLSX_MAIN}t")
    return "".join(t.text or "" for t in pieces)


def _xlsx_cell(c: ET.Element, shared: List[str]) -> str:
    kind = c.get("t", "n")
    if kind == "inlineStr":
        node = c.find(f"{_XLSX_MAIN}is")
        return "" if node is None else _xlsx_text(node)
    v = c.findtext(f"{_XLSX_MAIN}v")
    if v is None:
        return ""
    if kind == "s":
        return shared[int(v)]
    if kind != "n":
        return v  # str, b, e, d: as written
    # A number to Excel's own 15 significant digits: the file may hold the double
    # 100.50000000000001 where Excel shows, and the CSV would say, 100.5.
    return f"{float(v):.15g}"


def _parse_json(body: bytes, what: str) -> Any:
    text = body.decode("utf-8-sig", errors="replace").strip()
    if not text.startswith(("[", "{")):
        raise _SourceError(
            f"NSE answered {what} with a web page, not data (a bot check, most likely). "
            "nse-india uses a plain client and does not work around bot protection."
        )
    try:
        return json.loads(text)
    except ValueError as e:
        raise _SourceError(f"NSE answered {what} with JSON that does not parse: {e}")


# --------------------------------------------------------------------------
# Walks: one file per day (or per year) between a first and a last day
# --------------------------------------------------------------------------

class _Walk:
    def __init__(
        self, path: Callable[[date], str], parse: Callable[[bytes, date], "_Rows"], first: date, label: str,
        final: Optional[date] = None, unit: str = "day", host: str = "archive", small: bool = False,
    ) -> None:
        self.path = path
        self.parse = parse
        self.first = first
        self.label = label
        self.final = final
        self.unit = unit
        self.host = host
        self.small = small

    def align(self, day: date) -> date:
        return date(day.year, 1, 1) if self.unit == "year" else day

    def next(self, day: date) -> date:
        return date(day.year + 1, 1, 1) if self.unit == "year" else day + timedelta(days=1)

    def end_of(self, day: date) -> date:
        return date(day.year, 12, 31) if self.unit == "year" else day

    def name_of(self, day: date) -> str:
        return str(day.year) if self.unit == "year" else day.isoformat()


WALKS: Dict[str, _Walk] = {
    "bhavcopy_daily": _Walk(
        _bhavcopy_path,
        lambda body, day: parse_bhavcopy(_unzip_single_csv(body, f"bhavcopy {day.isoformat()}"), day),
        UDIFF_START, "bhavcopy",
    ),
    "bhavcopy_history": _Walk(
        _history_path,
        lambda body, day: parse_bhavcopy_history(
            _unzip_single_csv(body, f"bhavcopy history {day.isoformat()}"), day
        ),
        HISTORY_START, "bhavcopy history", final=HISTORY_FINAL,
    ),
    "delivery_daily": _Walk(
        _delivery_path, lambda body, day: parse_delivery(_decode(body), day), DELIVERY_START, "delivery",
    ),
    "index_daily": _Walk(
        _index_close_path, lambda body, day: parse_index_close(_decode(body), day), HISTORY_START, "index close",
        small=True,
    ),
    "participant_oi_daily": _Walk(
        lambda day: _participant_path("oi", day), lambda body, day: parse_participant(_decode(body), day, "oi"),
        HISTORY_START, "participant OI", small=True,
    ),
    "participant_vol_daily": _Walk(
        lambda day: _participant_path("vol", day), lambda body, day: parse_participant(_decode(body), day, "vol"),
        HISTORY_START, "participant volume", small=True,
    ),
    "corporate_actions": _Walk(
        _corporate_actions_path,
        lambda body, day: parse_corporate_actions(_parse_json(body, f"corporate actions {day.year}"), day),
        HISTORY_START, "corporate actions", unit="year", host="www",
    ),
}

# Snapshot table -> the name its warnings carry.
SNAPSHOT_LABELS = {
    "equity_list": "EQUITY_L.csv",
    "bulk_deals": "bulk.csv",
    "block_deals": "block.csv",
    "fii_dii_cash": "FII/DII cash",
    "index_constituents": "index lists",
}


# --------------------------------------------------------------------------
# HTTP client
# --------------------------------------------------------------------------

class _SourceError(Exception):
    """A failure the connector reports as is; never retried here."""


class _OutOfTime(_SourceError):
    """No request slot is left before the call's deadline."""


class _EarlierCopy(Exception):
    """A day's file that repeats an earlier day's data: no file for that day."""

    def __init__(self, file_day: date):
        super().__init__(file_day.isoformat())
        self.file_day = file_day


class _Transient(Exception):
    """A failure worth one more try: reset, timeout, 429, 5xx."""


class _SameHostRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        old = urllib.parse.urlsplit(req.full_url)
        new = urllib.parse.urlsplit(urllib.parse.urljoin(req.full_url, newurl))
        if (new.scheme, new.hostname, new.port) != (old.scheme, old.hostname, old.port):
            raise _SourceError(f"refused a redirect from {old.hostname} to {new.hostname or newurl!r}")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _normalize_base_url(raw: str) -> str:
    url = str(raw or "").strip().rstrip("/")
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise ValueError(f"NSE base URL must be an http(s) URL with a host, got {raw!r}")
    return url


class _Client:
    def __init__(
        self, base_url: str, min_interval: float, timeout: float, backoff: Tuple[float, ...], accept: str = "*/*",
    ) -> None:
        self.base_url = _normalize_base_url(base_url)
        self._min_interval = min_interval
        self._timeout = timeout
        self._backoff = backoff
        self._accept = accept
        # Do not start a request with less time than this left before the deadline.
        self._min_request = min(2.0, timeout)
        self._opener = urllib.request.build_opener(_SameHostRedirect())
        self._rate_lock = threading.Lock()
        self._next_slot = 0.0

    def _reserve(self, url: str, deadline: float) -> None:
        """Book the next free request slot, then wait for it outside the lock, so
        parallel tables queue in turn and none waits past its own deadline. A
        slot too late to use is not booked."""
        with self._rate_lock:
            slot = max(time.monotonic(), self._next_slot)
            if slot > deadline - self._min_request:
                raise _OutOfTime(f"no time left in this call to read {url}")
            self._next_slot = slot + self._min_interval
        wait = slot - time.monotonic()
        if wait > 0:
            time.sleep(wait)

    def _once(self, url: str, deadline: float) -> Optional[bytes]:
        self._reserve(url, deadline)
        timeout = max(0.1, min(self._timeout, deadline - time.monotonic()))
        req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": self._accept})
        try:
            with self._opener.open(req, timeout=timeout) as resp:
                body = resp.read(MAX_DOWNLOAD_BYTES + 1)
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return None
            if e.code in (401, 403):
                reason = "Unauthorized" if e.code == 401 else "Forbidden"
                raise _SourceError(
                    f"NSE answered {e.code} {reason} for {url}. NSE refused this client; "
                    "nse-india uses a plain client and does not work around bot protection. "
                    "Load the files another way (download them yourself, or drop them in object storage)."
                )
            if e.code == 429 or e.code >= 500:
                raise _Transient(f"HTTP {e.code} from {url}")
            raise _SourceError(f"HTTP {e.code} from {url}")
        except _SourceError:
            raise
        except (urllib.error.URLError, HTTPException, ConnectionError, TimeoutError, OSError) as e:
            reason = getattr(e, "reason", None) or e
            raise _Transient(f"{type(reason).__name__}: {reason} from {url}")
        if len(body) > MAX_DOWNLOAD_BYTES:
            raise _SourceError(f"{url} is larger than the {MAX_DOWNLOAD_BYTES} byte limit")
        return body

    def get(self, path: str, deadline: float) -> Optional[bytes]:
        """GET base_url+path. None for 404; _OutOfTime when no slot is left before
        the first try; _SourceError when it cannot be read."""
        url = self.base_url + path
        last: Optional[_Transient] = None
        for attempt in range(len(self._backoff) + 1):
            if attempt:
                pause = self._backoff[attempt - 1]
                if time.monotonic() + pause + self._min_request > deadline:
                    break
                time.sleep(pause)
            try:
                return self._once(url, deadline)
            except _OutOfTime:
                if last is None:
                    raise
                break
            except _Transient as e:
                last = e
        if last is None:
            raise _OutOfTime(f"no time left in this call to read {url}")
        raise _SourceError(f"could not read {url} after retries: {last}")


# --------------------------------------------------------------------------
# Since and cursor
# --------------------------------------------------------------------------

_SINCE_RE = re.compile(r"^(\d{4}-\d{2}-\d{2})(?:[T ][0-9:.+\-Z]*)?$")


def _parse_since(raw: Any, first: date) -> Tuple[Optional[date], Optional[str]]:
    if raw in (None, ""):
        return None, None
    m = _SINCE_RE.match(str(raw).strip())
    if m:
        try:
            return date.fromisoformat(m.group(1)), None
        except ValueError:
            pass
    return None, f"ignored since={raw!r}: not a date; reading from {first.isoformat()} instead (full fetch)"


def _parse_cursor(raw: Any) -> Tuple[date, int]:
    m = re.match(r"^(\d{4}-\d{2}-\d{2}):(\d+)$", str(raw).strip())
    try:
        if not m:
            raise ValueError("want YYYY-MM-DD:N")
        return date.fromisoformat(m.group(1)), int(m.group(2))
    except ValueError as e:
        raise ValueError(f"cursor {raw!r} is not one this connector issued ({e})") from None


def _first_since(params: Dict[str, Any]) -> Any:
    for key in ("since", "updated_since", "modified_since", "modified_after"):
        if params.get(key) not in (None, ""):
            return params[key]
    return None


def _as_int(value: Any, fallback: int) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return fallback


# --------------------------------------------------------------------------
# Server
# --------------------------------------------------------------------------

class NseIndiaMCPServer:
    def __init__(
        self,
        base_url: Optional[str] = None,
        min_interval: float = 1.0,
        timeout: float = 8.0,
        backoff: Tuple[float, ...] = (1.0, 2.0),
        call_budget: float = 20.0,
        today: Optional[Callable[[], date]] = None,
        days_per_page: int = DAYS_PER_PAGE,
        www_base_url: Optional[str] = None,
        small_days_per_page: int = SMALL_FILE_DAYS_PER_PAGE,
    ) -> None:
        self.connector_type = CONNECTOR_TYPE
        self._client = _Client(
            base_url or os.getenv("NSE_ARCHIVE_BASE_URL") or DEFAULT_BASE_URL,
            min_interval, timeout, backoff,
        )
        self._www = _Client(
            www_base_url or os.getenv("NSE_WWW_BASE_URL") or DEFAULT_WWW_BASE_URL,
            min_interval, timeout, backoff, accept="application/json",
        )
        # Start no new file after call_budget once one has rows; give up at
        # hard_budget. Both stay under the orchestrator's 30 s cap on one MCP call.
        self._call_budget = call_budget
        self._hard_budget = call_budget + 5.0
        self._today = today or (lambda: datetime.now(IST).date())
        self._days_per_page = days_per_page
        self._small_days_per_page = small_days_per_page
        self._cache_lock = threading.Lock()
        # table -> {day: (read at, parsed)}; a file split across pages is downloaded once.
        self._unit_cache: Dict[str, "OrderedDict[date, Tuple[float, _Rows]]"] = {}
        # (table, day) of old days with no file.
        self._missing: "OrderedDict[Tuple[str, date], bool]" = OrderedDict()
        self._snapshot_cache: Dict[str, Tuple[float, _Rows]] = {}

    # ---- data access ------------------------------------------------------

    def _unit(self, table: str, day: date, today: date, deadline: float) -> Optional[_Rows]:
        walk = WALKS[table]
        with self._cache_lock:
            cache = self._unit_cache.setdefault(table, OrderedDict())
            hit = cache.get(day)
            if hit is not None and time.monotonic() - hit[0] < CACHE_TTL:
                cache.move_to_end(day)
                return hit[1]
            if (table, day) in self._missing:
                self._missing.move_to_end((table, day))
                return None
        client = self._www if walk.host == "www" else self._client
        path = walk.path(day)
        body = client.get(path, deadline)
        if body is None and walk.unit == "year":
            raise _SourceError(f"{client.base_url}{path} was not found (404)")
        try:
            parsed = walk.parse(body, day) if body is not None else None
        except _EarlierCopy:
            self._note_missing(table, day, today)
            raise
        if parsed is None:
            self._note_missing(table, day, today)
            return None
        with self._cache_lock:
            cache[day] = (time.monotonic(), parsed)
            cache.move_to_end(day)
            while len(cache) > UNIT_CACHE_PER_TABLE:
                cache.popitem(last=False)
        return parsed

    def _note_missing(self, table: str, day: date, today: date) -> None:
        # Only an old day: today's file, or a late one, may still be published.
        if day < today - timedelta(days=MISSING_DAY_MIN_AGE):
            with self._cache_lock:
                self._missing[(table, day)] = True
                while len(self._missing) > MISSING_CACHE_SIZE:
                    self._missing.popitem(last=False)

    def _get_required(self, client: _Client, path: str, deadline: float) -> bytes:
        body = client.get(path, deadline)
        if body is None:
            raise _SourceError(f"{client.base_url}{path} was not found (404)")
        return body

    def _read_snapshot(self, table: str, deadline: float) -> _Rows:
        if table == "equity_list":
            return parse_equity_list(_decode(self._get_required(self._client, EQUITY_LIST_PATH, deadline)))
        if table == "bulk_deals":
            body = self._get_required(self._client, BULK_DEALS_PATH, deadline)
            return parse_deals(_decode(body), "bulk.csv", with_remarks=True)
        if table == "block_deals":
            body = self._get_required(self._client, BLOCK_DEALS_PATH, deadline)
            return parse_deals(_decode(body), "block.csv", with_remarks=False)
        if table == "fii_dii_cash":
            return parse_fii_dii(_parse_json(self._get_required(self._www, FII_DII_PATH, deadline), "FII/DII cash"))
        captured_on = self._today()
        result = _Rows()
        for index_name, path in INDEX_LIST_PATHS.items():
            part = parse_index_list(_decode(self._get_required(self._client, path, deadline)), index_name, captured_on)
            result.rows.extend(part.rows)
            for col, n in part.bad.items():
                result.bad[col] = result.bad.get(col, 0) + n
            result.notes.extend(part.notes)
        return result

    def _snapshot(self, table: str, deadline: float) -> _Rows:
        with self._cache_lock:
            hit = self._snapshot_cache.get(table)
            if hit is not None and time.monotonic() - hit[0] < CACHE_TTL:
                return hit[1]
        parsed = self._read_snapshot(table, deadline)
        with self._cache_lock:
            self._snapshot_cache[table] = (time.monotonic(), parsed)
        return parsed

    def _equity_list(self, deadline: float) -> _Rows:
        return self._snapshot("equity_list", deadline)

    # ---- core operations --------------------------------------------------

    def test_connection(self, params: Dict = None) -> Dict[str, Any]:
        try:
            rows = self._equity_list(time.monotonic() + self._hard_budget)
        except _SourceError as e:
            return {"success": False, "error": str(e)}
        return {
            "success": True,
            "message": f"Reached the NSE archive at {self._client.base_url}; {len(rows.rows)} listed equities",
            "version": CONNECTOR_VERSION,
        }

    def validate_config(self, params: Dict = None) -> Dict[str, Any]:
        # The connection has no settings; a config saved with the old date range is ignored.
        return {"success": True, "valid": True, "errors": [], "warnings": []}

    def get_capabilities(self, params: Dict = None) -> Dict[str, Any]:
        return {
            "success": True,
            "capabilities": {
                "supported_formats": ["json"],
                "supports_cdc": False,
                "supports_source": True,
                "supports_destination": False,
                "supports_incremental": True,
                "tables": list(TABLES.keys()),
                "incremental_fields": {
                    name: spec["incremental_field"] for name, spec in TABLES.items() if spec["incremental_field"]
                },
            },
        }

    def discover_schema(self, params: Dict = None) -> Dict[str, Any]:
        params = params or {}
        include_columns = params.get("include_columns", True)
        tables = []
        for name, spec in TABLES.items():
            entry: Dict[str, Any] = {"name": name, "description": spec["description"]}
            if include_columns:
                entry["columns"] = [dict(c) for c in spec["columns"]]
            entry["primary_keys"] = list(spec["primary_key"])
            tables.append(entry)
        return {"success": True, "tables": tables, "total_tables": len(tables)}

    def export(self, params: Dict = None) -> Dict[str, Any]:
        params = params or {}
        table = _resolve_table(params.get("table") or params.get("resource"))
        if table not in TABLES:
            return {
                "success": False,
                "error": f"Unknown table: {table!r}. Available tables: {', '.join(TABLES)}",
            }
        limit = _as_int(params.get("limit", DEFAULT_LIMIT), DEFAULT_LIMIT)
        if limit <= 0:
            limit = DEFAULT_LIMIT
        if table in WALKS:
            return self._export_walk(table, params, limit)
        return self._export_snapshot(table, params, limit)

    def _export_snapshot(self, table: str, params: Dict[str, Any], limit: int) -> Dict[str, Any]:
        offset = max(0, _as_int(params.get("offset", 0), 0))
        try:
            parsed = self._snapshot(table, time.monotonic() + self._hard_budget)
        except _SourceError as e:
            return {"success": False, "error": str(e)}
        page = [dict(r) for r in parsed.rows[offset:offset + limit]]
        out: Dict[str, Any] = {
            "success": True,
            "data": page,
            "columns": _column_names(table),
            "row_count": len(page),
            "has_more": (offset + len(page)) < len(parsed.rows),
        }
        warnings = parsed.warnings(SNAPSHOT_LABELS[table])
        if warnings and offset == 0:
            out["warnings"] = warnings
        return out

    def _export_walk(self, table: str, params: Dict[str, Any], limit: int) -> Dict[str, Any]:
        walk = WALKS[table]
        started = time.monotonic()
        today = self._today()
        end = min(today, walk.final) if walk.final else today
        per_page = self._small_days_per_page if walk.small else self._days_per_page
        warnings: List[str] = []
        since_day, since_warning = _parse_since(_first_since(params), walk.first)
        if since_warning:
            warnings.append(since_warning)

        raw_cursor = params.get("cursor")
        if raw_cursor not in (None, ""):
            try:
                day, skip = _parse_cursor(raw_cursor)
            except ValueError as e:
                return {"success": False, "error": str(e)}
        elif _as_int(params.get("offset", 0), 0) > 0:
            # Every page that can be followed carries next_cursor, so an offset
            # without one would restart the walk and write the same days twice.
            return {"success": False, "error": f"{table} pages by cursor; got an offset without a cursor"}
        else:
            day = since_day + timedelta(days=1) if since_day else walk.first
            skip = 0
        if walk.align(day) != day:
            day, skip = walk.align(day), 0
        if day < walk.first:
            day, skip = walk.first, 0

        rows: List[Dict[str, Any]] = []
        completed: Optional[date] = None
        copies: List[str] = []
        visited = read_units = 0
        out_of_time = False
        deadline = started + self._hard_budget
        # Calendar days, weekends included: NSE has held live Saturday sessions
        # (Budget day 2025-02-01), and a day with no file costs one 404. Only files
        # with rows count toward the page, and the call budget stops a page only
        # once it has rows, so a holiday week cannot end a page empty while later
        # days remain (the executor stops a table there).
        while day <= end:
            if read_units >= per_page or (read_units and time.monotonic() - started >= self._call_budget):
                break
            try:
                parsed = self._unit(table, day, today, deadline)
            except _OutOfTime:
                out_of_time = True
                break
            except _EarlierCopy as e:
                # Like a day with no file: it never moves the watermark past itself.
                copies.append(f"{walk.name_of(day)} carries {e.file_day.isoformat()}")
                visited += 1
                day, skip = walk.next(day), 0
                continue
            except _SourceError as e:
                if not visited:
                    return {"success": False, "error": str(e)}
                warnings.append(f"stopped before {walk.name_of(day)}: {e}; the next page asks for it again")
                break
            visited += 1
            if parsed is None:
                day, skip = walk.next(day), 0
                continue
            # A year counts as done only once it is over: the current one is read again next run.
            finished = walk.unit == "day" or walk.end_of(day) < today
            if not parsed.rows:
                if finished:
                    completed = walk.end_of(day)
                day, skip = walk.next(day), 0
                continue
            read_units += 1
            if skip == 0:
                warnings.extend(parsed.warnings(f"{walk.label} {walk.name_of(day)}"))
            remaining = parsed.rows[skip:]
            room = limit - len(rows)
            if len(remaining) <= room:
                rows.extend(dict(r) for r in remaining)
                if finished:
                    completed = walk.end_of(day)
                day, skip = walk.next(day), 0
                if len(rows) >= limit:
                    break
            else:
                rows.extend(dict(r) for r in remaining[:room])
                skip += room
                break

        if walk.final and day > walk.final:
            # Every day to the table's last one was visited; a last day with no
            # file must not leave the next runs asking for it forever.
            completed = walk.final
        has_more = day <= end
        out: Dict[str, Any] = {
            "success": True,
            "data": rows,
            "columns": _column_names(table),
            "row_count": len(rows),
            "has_more": has_more,
        }
        # Any page that can be followed by another call says where it stopped,
        # including a full last page (the executor asks once more after it).
        if has_more or rows:
            out["next_cursor"] = f"{day.isoformat()}:{skip}"
        # The watermark is the last day emitted in full: a day split across pages
        # counts only once its final rows are out, a day with no file never counts.
        watermark = max((d for d in (completed, since_day) if d), default=None)
        if watermark:
            out["stats"] = {
                "watermark": {"field": TABLES[table]["incremental_field"], "value": watermark.isoformat()}
            }
        if out_of_time and not rows:
            warnings.append(
                f"{table} stopped at {walk.name_of(day)} before reading a file: NSE allows about one request "
                "a second, shared by every table in this run, and this call ran out of time; "
                "the next run resumes here"
            )
        if copies:
            warnings.append(
                f"{table} skipped {len(copies)} day(s) whose file repeats an earlier day's data "
                f"({'; '.join(copies[:3])}{'; …' if len(copies) > 3 else ''}), as for a day with no file"
            )
        if warnings:
            out["warnings"] = warnings
        return out


# --------------------------------------------------------------------------
# stdlib JSON-RPC transport (cloned from sample-data): HTTP or stdin/stdout.
# --------------------------------------------------------------------------

def _jsonrpc_error(req_id: Any, code: int, message: str) -> Dict[str, Any]:
    return {"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}}


def _jsonrpc_result(req_id: Any, result: Dict[str, Any]) -> Dict[str, Any]:
    return {"jsonrpc": "2.0", "id": req_id, "result": result}


def _dispatch() -> Dict[str, Callable[[Dict[str, Any]], Dict[str, Any]]]:
    server = NseIndiaMCPServer()
    return {
        "test_connection": server.test_connection,
        "validate_config": server.validate_config,
        "discover_schema": server.discover_schema,
        "export": server.export,
        "get_capabilities": server.get_capabilities,
        "health": lambda _args: {"success": True, "status": "ok"},
    }


def handle_request(dispatch: Dict[str, Callable[[Dict[str, Any]], Dict[str, Any]]], req: Any) -> Dict[str, Any]:
    """Answer one parsed JSON-RPC request. Never raises: both transports send
    whatever this returns."""
    if not isinstance(req, dict):
        return _jsonrpc_error(None, -32600, "invalid request")
    req_id = req.get("id")
    if req.get("jsonrpc") != "2.0":
        return _jsonrpc_error(req_id, -32600, "invalid request")
    if req.get("method") != "tools/call":
        return _jsonrpc_error(req_id, -32601, "method not found")

    params = req.get("params") or {}
    if not isinstance(params, dict):
        return _jsonrpc_error(req_id, -32602, "params must be an object")
    tool = params.get("name") or ""
    args = params.get("arguments") or {}
    if not isinstance(tool, str) or not tool:
        return _jsonrpc_error(req_id, -32602, "missing tool name")
    if not isinstance(args, dict):
        return _jsonrpc_error(req_id, -32602, "arguments must be an object")
    prefix = CONNECTOR_TYPE + "_"
    if not tool.startswith(prefix):
        return _jsonrpc_error(req_id, -32601, "tool not found")

    fn = dispatch.get(tool[len(prefix):].strip())
    if fn is None:
        return _jsonrpc_error(req_id, -32601, "tool not found")
    try:
        res = fn(args)
    except Exception as e:  # never crash the server on a bad request
        res = {"success": False, "error": str(e)}
    return _jsonrpc_result(req_id, res)


def serve_http() -> None:
    dispatch = _dispatch()
    port = int(os.getenv("MCP_PORT") or os.getenv("PORT") or "8000")

    class Handler(BaseHTTPRequestHandler):
        def _send_json(self, status: int, obj: Dict[str, Any]) -> None:
            body = json.dumps(obj).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):  # noqa: N802
            if self.path == "/health":
                self._send_json(200, {"status": "ok"})
            else:
                self._send_json(404, {"error": "not found"})

        def do_POST(self):  # noqa: N802
            if self.path != "/mcp":
                self._send_json(404, {"error": "not found"})
                return

            try:
                length = int(self.headers.get("Content-Length", "0"))
                raw = self.rfile.read(length) if length > 0 else b"{}"
                req = json.loads(raw.decode("utf-8") or "{}")
            except Exception:
                self._send_json(200, _jsonrpc_error(None, -32700, "parse error"))
                return
            self._send_json(200, handle_request(dispatch, req))

        def log_message(self, format: str, *args):  # noqa: A002
            return

    httpd = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    httpd.serve_forever()


def serve_stdio() -> None:
    """One JSON-RPC request per line on stdin, one JSON line back on stdout:
    how the orchestrator runs the connector when it starts it as a subprocess
    (server_manager.go sets MCP_HTTP_MODE=false). Nothing else goes to stdout."""
    dispatch = _dispatch()
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError:
            resp = _jsonrpc_error(None, -32700, "parse error")
        else:
            resp = handle_request(dispatch, req)
        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    # The image sets MCP_HTTP_MODE=true and DOCKER_CONTAINER; the orchestrator's
    # subprocess fallback strips both and sets MCP_HTTP_MODE=false.
    if os.getenv("MCP_HTTP_MODE", "false").lower() == "true" or os.getenv("DOCKER_CONTAINER"):
        serve_http()
    else:
        serve_stdio()
