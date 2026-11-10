"""Every connector minted from the REST template reads a table to the end
(KI-GENERATED-REST-STOPS-PAGING-EARLY, sibling sweep).

The template fix (resume a capped read, per-call time budget) reaches only
connectors generated after it. stripe, notion-rest and github-rest were minted
earlier, and each image runs its OWN vendored base_connector.py (the Dockerfile
copies it over the shared one), so each needs the export call AND its base copy
patched in place. This test finds them by the template's export marker instead
of a hand list, so a REST connector added later is checked too.
"""
from __future__ import annotations

import glob
import importlib.util
import json
import os
import re

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
MARKER = "self._api_handler.export_resource("
MIN_REST_CONNECTORS = 3  # github-rest, notion-rest, stripe


def _discover():
    found = []
    for manifest in sorted(glob.glob(os.path.join(ROOT, "**", "latest.json"), recursive=True)):
        if os.sep + "versions" + os.sep in manifest:
            continue
        root = os.path.dirname(manifest)
        with open(manifest) as handle:
            version_dir = os.path.join(root, "versions", str(json.load(handle)["current_version"]))
        source = os.path.join(version_dir, "connector.py")
        if os.path.isfile(source) and MARKER in open(source, encoding="utf-8").read():
            found.append((os.path.basename(root), version_dir))
    return found


REST = _discover()


def test_the_rest_siblings_are_found():
    # An empty discovery passes every parametrized case below by running none.
    assert len(REST) >= MIN_REST_CONNECTORS, REST


@pytest.mark.parametrize("name,version_dir", REST, ids=[n for n, _ in REST])
def test_export_hands_the_runtime_its_resume_cursor_and_time_budget(name, version_dir):
    code = open(os.path.join(version_dir, "connector.py"), encoding="utf-8").read()
    call = code[code.find(MARKER):]
    call = call[:call.find("\n            )\n")]
    assert "resume_cursor=cursor" in call and "time_budget_s=time_budget_s" in call, call
    # Put into the query params, the cursor was re-sent on every page, so each
    # page re-read the first one.
    assert not re.search(r"query_params\[pagination_param\]\s*=\s*cursor", code)
    assert re.search(r"^API_MAX_PAGES_PER_CALL = \d+$", code, re.M)


_ROWS = [{"id": f"r{i:04d}"} for i in range(1, 251)]
_PAGE = 20


def _offset_api(params):
    start = int(params.get("offset", 0))
    return True, 200, {"data": _ROWS[start:start + _PAGE]}, {}


@pytest.mark.parametrize("name,version_dir", REST, ids=[n for n, _ in REST])
def test_the_base_copy_the_image_runs_resumes_a_capped_read(name, version_dir):
    vendored = os.path.join(version_dir, "base_connector.py")
    path = vendored if os.path.isfile(vendored) else os.path.join(ROOT, "base_connector.py")
    spec = importlib.util.spec_from_file_location(f"base_connector_{name.replace('-', '_')}", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    handler = module.PaginationHandler(max_page_size=_PAGE, pagination_type="offset",
                                       pagination_param="offset", limit_param="")
    rows, cursor, calls = [], None, 0
    while True:
        calls += 1
        assert calls < 100, "the read never ended"
        got, errors, cursor = handler.fetch_all_pages(_offset_api, max_pages=3, max_records=10_000,
                                                      resume_cursor=cursor)
        assert errors == []
        rows.extend(got)
        if not handler.last_run_hit_cap:
            break
    assert [r["id"] for r in rows] == [r["id"] for r in _ROWS]
    assert calls > 1, "the cap must have stopped at least one call, or this proves nothing"
