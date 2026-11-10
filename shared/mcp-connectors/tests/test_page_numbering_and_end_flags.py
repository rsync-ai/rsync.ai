"""PaginationHandler must not lose rows on page/offset APIs (2026-10-04 real-SaaS round).

Found by running a generated ClickUp connector against a list seeded with 120
tasks: it returned 20. ClickUp numbers pages from 0 and serves a fixed 100 tasks
per page with a ``last_page`` flag; the handler asked for page 1 (skipping tasks
1-100), got 20 < 100, and stopped.

Bug class — the page/offset loop assumed facts the API never declared:
  * page numbers start at 1 (``page_start`` now carries the spec's 0 or 1, and a
    resume at page 0 is no longer coerced to 1 by ``or 1``);
  * the server honours our page size, so an offset advances by ``max_page_size``
    (it now advances by the rows actually received) and a short page means the
    end even when no limit param was ever sent (that stop now needs a sent limit);
  * an explicit end-of-data flag in the body (``last_page``, ``has_more``) means
    nothing (it now decides, both ways).
"""
from __future__ import annotations

import importlib.util
import os

_BC_PATH = os.path.join(os.path.dirname(__file__), "..", "base_connector.py")
_spec = importlib.util.spec_from_file_location("base_connector_pagenum", _BC_PATH)
_bc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bc)
PaginationHandler = _bc.PaginationHandler


def _clickup_like(total=120, page_size=100):
    """Zero-based pages, fixed page size, ``last_page`` flag; ignores any limit."""
    rows = [{"id": f"t{i:03d}"} for i in range(1, total + 1)]
    seen = []

    def fetch(params):
        seen.append(dict(params))
        page = int(params.get("page", 0))
        chunk = rows[page * page_size:(page + 1) * page_size]
        last = (page + 1) * page_size >= total
        return True, 200, {"tasks": chunk, "last_page": last}, {}

    return fetch, seen


def test_zero_based_pages_fetch_every_row():
    fetch, seen = _clickup_like()
    ph = PaginationHandler(pagination_type="page", pagination_param="page",
                           limit_param="", max_page_size=100,
                           response_data_key="tasks", page_start=0)
    records, errors, _ = ph.fetch_all_pages(fetch, max_pages=10, max_records=10000)
    assert [p["page"] for p in seen] == [0, 1]
    assert len(records) == 120 and errors == []
    assert records[0]["id"] == "t001" and records[-1]["id"] == "t120"


def test_resume_at_page_zero_is_not_coerced_to_one():
    fetch, seen = _clickup_like()
    ph = PaginationHandler(pagination_type="page", pagination_param="page",
                           limit_param="", max_page_size=100,
                           response_data_key="tasks", page_start=0)
    ph.fetch_all_pages(fetch, max_pages=10, max_records=10000,
                       initial_params={"page": 0})
    assert seen[0]["page"] == 0


def test_default_page_start_is_still_one():
    ph = PaginationHandler(pagination_type="page", pagination_param="page",
                           limit_param="per_page", max_page_size=2)
    seen = []

    def fetch(params):
        seen.append(dict(params))
        return True, 200, {"data": [{"id": 1}]}, {}

    records, _, _ = ph.fetch_all_pages(fetch, max_pages=5, max_records=100)
    # A short FIRST page cannot end the read on its own (the server may cap
    # pages below our limit), so page 2 is asked for once; the same rows back
    # mean the end.
    assert [p["page"] for p in seen] == [1, 2]
    assert records == [{"id": 1}]


def test_last_page_flag_stops_on_a_full_page():
    fetch, seen = _clickup_like(total=100)
    ph = PaginationHandler(pagination_type="page", pagination_param="page",
                           limit_param="", max_page_size=100,
                           response_data_key="tasks", page_start=0)
    records, _, _ = ph.fetch_all_pages(fetch, max_pages=10, max_records=10000)
    assert len(records) == 100
    assert len(seen) == 1  # last_page=true → no probe request for page 1


def test_has_more_true_continues_past_a_short_page():
    # API caps pages at 30 although we ask for 100; has_more says keep going.
    rows = [{"id": i} for i in range(75)]
    seen = []

    def fetch(params):
        seen.append(dict(params))
        off = int(params["offset"])
        chunk = rows[off:off + 30]
        return True, 200, {"data": chunk, "has_more": off + 30 < len(rows)}, {}

    ph = PaginationHandler(pagination_type="offset", pagination_param="offset",
                           limit_param="limit", max_page_size=100)
    records, _, _ = ph.fetch_all_pages(fetch, max_pages=10, max_records=10000)
    assert [p["offset"] for p in seen] == [0, 30, 60]
    assert len(records) == 75


def test_no_limit_sent_short_page_is_not_the_end():
    # Spec declares `page` but no size param: the server's page size is unknown,
    # so a page shorter than our max_page_size proves nothing. Page until empty.
    rows = [{"id": i} for i in range(45)]
    seen = []

    def fetch(params):
        seen.append(dict(params))
        p = int(params["page"])
        return True, 200, {"data": rows[(p - 1) * 20:p * 20]}, {}

    ph = PaginationHandler(pagination_type="page", pagination_param="page",
                           limit_param="", max_page_size=100)
    records, _, _ = ph.fetch_all_pages(fetch, max_pages=10, max_records=10000)
    assert len(records) == 45
    assert [p["page"] for p in seen] == [1, 2, 3, 4]  # page 4 is empty → stop
    assert "limit" not in seen[0] and "" not in seen[0]
