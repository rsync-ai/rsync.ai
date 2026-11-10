"""One export call reads a bounded part of a table; the next call resumes where it
stopped, losing and repeating nothing (KI-GENERATED-REST-STOPS-PAGING-EARLY).

Found on prod 2026-10-04: a generated ClickUp connector read 1,000 of a 1,250-task
list and the run reported completed. The handler stopped at DEFAULT_MAX_PAGES=10
with no way to say where it stopped, and the executor took the short batch as the
end of the table.

Bug class — a cap that ends the read without handing back a resume position.
Each paging mode must, when a cap (pages or seconds) stops it, return the
position of the first row not yet read, and accept that position on the next
call. Also covered: a spec with no paging is probed instead of read as one page,
and a probe that finds nothing leaves a warning instead of a silent partial.
"""
from __future__ import annotations

import importlib.util
import os

import pytest

_BC_PATH = os.path.join(os.path.dirname(__file__), "..", "base_connector.py")
_spec = importlib.util.spec_from_file_location("base_connector_bounded", _BC_PATH)
_bc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bc)
PaginationHandler = _bc.PaginationHandler
ExportResult = _bc.ExportResult

_ROWS = [{"id": f"r{i:04d}"} for i in range(1, 251)]
_PAGE = 20


def _api(mode):
    """A server for ``mode`` holding _ROWS, _PAGE rows a page."""

    def fetch(params):
        if mode == "offset":
            start = int(params.get("offset", 0))
            return True, 200, {"data": _ROWS[start:start + _PAGE]}, {}
        if mode == "page0":
            page = int(params.get("page", 0))
            chunk = _ROWS[page * _PAGE:(page + 1) * _PAGE]
            return True, 200, {"data": chunk, "last_page": (page + 1) * _PAGE >= len(_ROWS)}, {}
        if mode == "cursor":
            start = int(params.get("cursor") or 0)
            chunk = _ROWS[start:start + _PAGE]
            nxt = str(start + _PAGE) if start + _PAGE < len(_ROWS) else None
            return True, 200, {"data": chunk, "next_cursor": nxt}, {}
        if mode == "stripe":
            ids = [r["id"] for r in _ROWS]
            after = params.get("starting_after")
            start = ids.index(after) + 1 if after else 0
            chunk = _ROWS[start:start + _PAGE]
            return True, 200, {"data": chunk, "has_more": start + _PAGE < len(_ROWS)}, {}
        if mode == "link":
            url = params.get("_override_url") or "https://api.test/items?page=1"
            page = int(url.rsplit("=", 1)[1])
            chunk = _ROWS[(page - 1) * _PAGE:page * _PAGE]
            headers = {}
            if page * _PAGE < len(_ROWS):
                headers["Link"] = f'<https://api.test/items?page={page + 1}>; rel="next"'
            return True, 200, chunk, headers
        raise AssertionError(mode)

    return fetch


_HANDLERS = {
    "offset": dict(pagination_type="offset", pagination_param="offset", limit_param=""),
    "page0": dict(pagination_type="page", pagination_param="page", limit_param="", page_start=0),
    "cursor": dict(pagination_type="cursor", pagination_param="cursor", limit_param="",
                   cursor_path="next_cursor"),
    "stripe": dict(pagination_type="cursor", pagination_param="starting_after", limit_param="",
                   cursor_mode="last_item_id"),
    "link": dict(pagination_type="link", pagination_param="", limit_param=""),
}


def _read_in_calls(mode, *, max_pages=3, **kw):
    """Read the whole table the way the executor does: call, resume, repeat."""
    handler = PaginationHandler(max_page_size=_PAGE, **_HANDLERS[mode])
    fetch = _api(mode)
    rows, cursor, calls = [], None, 0
    while True:
        calls += 1
        assert calls < 100, "the read never ended"
        got, errors, cursor = handler.fetch_all_pages(fetch, max_pages=max_pages, max_records=10_000,
                                                      initial_params={}, resume_cursor=cursor, **kw)
        assert errors == []
        rows.extend(got)
        if not handler.last_run_hit_cap:
            assert cursor is None, "a finished read must not hand back a resume position"
            return rows, calls
        assert cursor, "a capped read must say where the next call starts"


@pytest.mark.parametrize("mode", sorted(_HANDLERS))
def test_a_page_cap_resumes_on_the_next_call_with_no_row_lost_or_repeated(mode):
    rows, calls = _read_in_calls(mode)
    assert [r["id"] for r in rows] == [r["id"] for r in _ROWS]
    assert calls > 1, "the cap must have stopped at least one call, or this test proves nothing"


def test_a_record_cap_cuts_an_offset_page_and_resumes_at_the_first_unread_row():
    handler = PaginationHandler(max_page_size=_PAGE, **_HANDLERS["offset"])
    first, _, cursor = handler.fetch_all_pages(_api("offset"), max_pages=100, max_records=30)
    assert len(first) == 30 and handler.last_run_hit_cap and cursor == "30"
    rest, _, _ = handler.fetch_all_pages(_api("offset"), max_pages=100, max_records=10_000,
                                         resume_cursor=cursor)
    assert [r["id"] for r in first + rest] == [r["id"] for r in _ROWS]


def test_the_time_budget_stops_the_call_between_pages_and_says_where(monkeypatch):
    # A page takes 8 s; a 20 s budget lets 3 pages start (0, 8, 16 s), not a 4th.
    clock = [1000.0]
    monkeypatch.setattr(_bc.time, "time", lambda: clock[0])
    slow = _api("offset")

    def fetch(params):
        clock[0] += 8
        return slow(params)

    handler = PaginationHandler(max_page_size=_PAGE, **_HANDLERS["offset"])
    rows, errors, cursor = handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000,
                                                   time_budget_s=20)
    assert errors == [] and len(rows) == 3 * _PAGE
    assert handler.last_run_hit_cap and cursor == str(3 * _PAGE)


def test_stripe_style_pages_until_an_empty_page_when_the_api_sends_no_has_more():
    stripe = _api("stripe")

    def fetch(params):
        ok, status, body, headers = stripe(params)
        body.pop("has_more")
        return ok, status, body, headers

    handler = PaginationHandler(max_page_size=_PAGE, **_HANDLERS["stripe"])
    rows, errors, cursor = handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000)
    assert errors == [] and cursor is None
    assert [r["id"] for r in rows] == [r["id"] for r in _ROWS]


# --- a spec with no paging: probe, then warn ------------------------------

_UNDOCUMENTED = dict(pagination_type="cursor", pagination_param="after", limit_param="")


def test_undocumented_paging_is_found_by_probing_offset_and_resumes_across_calls():
    offset = _api("offset")
    handler = PaginationHandler(max_page_size=_PAGE, **_UNDOCUMENTED)
    rows, cursor, calls = [], None, 0
    while True:
        calls += 1
        got, errors, cursor = handler.fetch_all_pages(offset, max_pages=4, max_records=10_000,
                                                      resume_cursor=cursor, probe_paging=True)
        assert errors == [] and handler.last_run_warnings == []
        rows.extend(got)
        if not handler.last_run_hit_cap:
            break
    assert [r["id"] for r in rows] == [r["id"] for r in _ROWS]
    assert calls > 1


def test_undocumented_paging_found_by_probing_page_numbers():
    page0 = _api("page0")

    def fetch(params):  # ignores offset: only `page` moves it
        return page0({k: v for k, v in params.items() if k != "offset"})

    handler = PaginationHandler(max_page_size=_PAGE, **_UNDOCUMENTED)
    rows, errors, _ = handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000, probe_paging=True)
    assert errors == [] and handler.last_run_warnings == []
    assert [r["id"] for r in rows] == [r["id"] for r in _ROWS]


def test_when_no_probe_moves_the_page_the_rows_are_kept_and_the_run_warns():
    def fetch(params):  # one page, whatever is asked
        return True, 200, {"data": _ROWS[:_PAGE]}, {}

    handler = PaginationHandler(max_page_size=_PAGE, **_UNDOCUMENTED)
    rows, errors, cursor = handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000,
                                                   probe_paging=True)
    assert errors == [] and len(rows) == _PAGE and cursor is None
    assert handler.last_run_warnings == [f"paging not documented — rows past {_PAGE} may be missing"]


def test_a_probe_that_finds_no_more_rows_means_the_table_fit_on_one_page():
    def fetch(params):
        if "offset" in params or "page" in params:
            return True, 200, {"data": []}, {}
        return True, 200, {"data": _ROWS[:5]}, {}

    handler = PaginationHandler(max_page_size=_PAGE, **_UNDOCUMENTED)
    rows, errors, _ = handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000, probe_paging=True)
    assert errors == [] and len(rows) == 5 and handler.last_run_warnings == []


def test_without_the_flag_an_undocumented_list_is_not_probed():
    asked = []

    def fetch(params):
        asked.append(dict(params))
        return True, 200, {"data": _ROWS[:_PAGE]}, {}

    handler = PaginationHandler(max_page_size=_PAGE, **_UNDOCUMENTED)
    handler.fetch_all_pages(fetch, max_pages=100, max_records=10_000)
    assert asked == [{}]


def test_export_result_carries_has_more_and_warnings_to_the_executor():
    out = ExportResult(success=True, records=[{"id": 1}], has_more=True, next_cursor="40",
                       warnings=["paging not documented — rows past 1 may be missing"]).to_dict()
    assert out["has_more"] is True and out["next_cursor"] == "40"
    assert out["warnings"] == ["paging not documented — rows past 1 may be missing"]
