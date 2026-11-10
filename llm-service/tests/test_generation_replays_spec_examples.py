"""A connector generated from an OpenAPI spec reads the spec's own response
examples before it is saved, and is refused when it reads 0 rows from records
the spec documents (generator-trust P3c).

Such a connector would run, report success and land nothing: the records sit
under a key its runtime does not follow (``artists.items`` looked up as one
literal key, the bug fixed with this check). The fast path now replays each list
GET's documented example through the ``base_connector.py`` the connector ships
with — after preflight, before anything is written:

* 0 rows from documented records refuses like a preflight error:
  ``success: false``, ``error_stage: "replay"``, the issue returned, the writer
  never reached;
* the other findings (no primary key in the rows, a missed cursor, …) are
  warnings: the connector is saved and the issues ride ``preflight_issues``;
* a spec that documents no example, or a replay that cannot run, skips the
  stage — never a reason to refuse.

``tests/../src/agents/tool_generator/tests/test_spec_example_replay.py`` covers
the capture, the judging and the child process on their own.
"""
from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

from src.agents.tool_generator.contracts.generate_v1 import GenerateResponseV1

FLAG = "RSYNC_CONNECTOR_CATALOG_SHARED"

# Imported the way the fast path imports its siblings (bare ``generator.``,
# ``agents.``), as test_generation_refuses_a_failed_preflight.py does.
_TOOLGEN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "src", "agents", "tool_generator")
)
_BASE_CONNECTOR = Path(__file__).resolve().parents[2] / "shared" / "mcp-connectors" / "base_connector.py"

_ARTIST = {"type": "object", "properties": {
    "id": {"type": "string"}, "name": {"type": "string"},
}}


def _spec(rows=None, *, example: bool = True) -> dict:
    """A list GET whose records sit two levels down (``artists.items``)."""
    media = {"schema": {"type": "object", "properties": {"artists": {"type": "object", "properties": {
        "items": {"type": "array", "items": _ARTIST},
    }}}}}
    if example:
        media["example"] = {"artists": {"items": rows if rows is not None else [
            {"id": "a1", "name": "Nina"}, {"id": "a2", "name": "Miles"}]}}
    return {
        "openapi": "3.0.3",
        "info": {"title": "Music", "version": "1"},
        "servers": [{"url": "https://api.music.test/v1"}],
        "components": {"securitySchemes": {"bearer": {"type": "http", "scheme": "bearer"}}},
        "security": [{"bearer": []}],
        "paths": {"/artists": {"get": {
            "operationId": "listArtists",
            "parameters": [{"name": "limit", "in": "query", "schema": {"type": "integer"}},
                           {"name": "offset", "in": "query", "schema": {"type": "integer"}}],
            "responses": {"200": {"description": "ok", "content": {"application/json": media}}},
        }}},
    }


def _runtime(tmp_path: Path, *, one_key: bool = False) -> Path:
    """A connectors tree holding base_connector.py; ``one_key`` makes it look a
    records key up as one literal key only (the runtime before the fix)."""
    root = tmp_path / "mcp-connectors"
    (root / "public").mkdir(parents=True)
    text = _BASE_CONNECTOR.read_text()
    if one_key:
        text += (
            "\n\ndef resolve_key_path(data, path):\n"
            "    if isinstance(data, dict) and path in data:\n"
            "        return data[path]\n"
            "    return _KEY_PATH_MISSING\n"
        )
    (root / "base_connector.py").write_text(text)
    return root / "public" / "music"


def _generate(monkeypatch, openapi: dict, output_dir: Path) -> tuple[dict, list]:
    """Run a real spec-first fast-path generation; the writer only records."""
    if _TOOLGEN not in sys.path:
        sys.path.insert(0, _TOOLGEN)
    import utils.session_store as session_store_mod
    from agents import session_fast_path
    from agents.discoverer import build_contract_from_spec

    contract = asyncio.run(build_contract_from_spec(
        "music", openapi_spec=json.dumps(openapi), base_url=openapi["servers"][0]["url"]))
    assert contract is not None and contract.can_generate, getattr(contract, "refusal_reason", None)

    class _Store:
        async def get(self, session_id):
            return contract

    async def _get_store():
        return _Store()

    writes: list = []

    def _record_write(generated, output_dir, connector_id, spec, *, replace_version=None):
        writes.append(json.loads(generated.metadata))
        return "v1.0.0"

    monkeypatch.delenv(FLAG, raising=False)
    monkeypatch.setattr(session_store_mod, "get_session_store", _get_store)
    monkeypatch.setattr(session_fast_path, "write_generated_artifacts", _record_write)
    result = asyncio.run(
        session_fast_path.try_session_fast_path(
            api_name="music",
            session_id="sess-replay",
            canonical_id="music",
            save_artifacts=True,
            output_dir=str(output_dir),
        )
    )
    return result, writes


def _stage(result: dict, name: str) -> str:
    return next(s["status"] for s in result["workflow_stages"] if s["name"] == name)


def _codes(result: dict) -> list:
    return [i["code"] for i in result.get("preflight_issues", []) if i["code"].startswith("RPL")]


def test_records_the_connector_cannot_read_refuse_it_and_save_nothing(monkeypatch, tmp_path):
    result, writes = _generate(monkeypatch, _spec(), _runtime(tmp_path, one_key=True))
    assert writes == [], "a connector that read 0 of the documented records still reached the writer"
    assert result["success"] is False, result
    assert result["status"] == "failed", result
    assert result["error_stage"] == "replay", result
    assert "reads 0 rows" in result["error_message"], result["error_message"]
    assert _codes(result) == ["RPL001"], result["preflight_issues"]
    assert _stage(result, "preflight") == "completed"
    assert _stage(result, "replay") == "failed"
    assert _stage(result, "persist") == "skipped"
    assert result.get("output_path") is None, "a refusal must not name a path it did not write"


def test_a_connector_that_reads_the_documented_records_is_saved(monkeypatch, tmp_path):
    # Control: the same spec against the shipped runtime. Without it the refusal
    # above would also pass on a replay that refused everything.
    result, writes = _generate(monkeypatch, _spec(), _runtime(tmp_path))
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert _stage(result, "replay") == "completed"
    assert _codes(result) == [], result.get("preflight_issues")


def test_a_replay_warning_still_saves_and_reports_the_warning(monkeypatch, tmp_path):
    # Rows without the "id" the schema names: the table would get no primary key.
    result, writes = _generate(monkeypatch, _spec([{"name": "Nina"}, {"name": "Miles"}]), _runtime(tmp_path))
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert _codes(result) == ["RPL005"], result.get("preflight_issues")
    assert result.get("error_stage") is None, "a warning is not a failed stage"
    assert _stage(result, "replay") == "completed"


def test_a_spec_without_examples_skips_the_replay(monkeypatch, tmp_path):
    result, writes = _generate(monkeypatch, _spec(example=False), _runtime(tmp_path, one_key=True))
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert _stage(result, "replay") == "skipped"


def test_a_replay_that_cannot_run_skips_and_saves(monkeypatch, tmp_path):
    # No base_connector.py anywhere above the output dir.
    out = tmp_path / "bare" / "public" / "music"
    result, writes = _generate(monkeypatch, _spec(), out)
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert _stage(result, "replay") == "skipped"


def test_the_response_model_keeps_the_replay_issues(monkeypatch, tmp_path):
    result, _ = _generate(monkeypatch, _spec(), _runtime(tmp_path, one_key=True))
    dumped = GenerateResponseV1.model_validate(result).model_dump()
    assert dumped["success"] is False
    assert dumped["error_stage"] == "replay"
    assert [i["code"] for i in dumped["preflight_issues"]] == [i["code"] for i in result["preflight_issues"]]
