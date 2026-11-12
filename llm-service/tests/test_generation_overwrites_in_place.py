"""A regeneration the user asks to overwrite rewrites the connector's current
version in place (generator-trust P4a).

By default a regeneration still writes a NEW version (v1.0.0 → v1.0.1), so a hand
fix to the old version is never lost silently. With ``overwrite`` it rewrites the
version ``latest.json`` names (not the highest directory), so connections pinned
to that version get the new code:

* no new ``versions/`` directory, ``current_version`` unchanged, the changelog
  records the overwrite;
* the old directory is moved aside first and put back when any check after the
  write fails, so a refused overwrite leaves the old connector exactly as it was;
* nothing is left behind in the connector root but ``latest.json`` and
  ``versions/``;
* the old logo survives (the writer never re-downloads one it already has).

The writer runs for real here (no stub) on a temporary catalog.
"""
from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

import pytest

FLAG = "RSYNC_CONNECTOR_CATALOG_SHARED"

_TOOLGEN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "src", "agents", "tool_generator")
)
_BASE_CONNECTOR = Path(__file__).resolve().parents[2] / "shared" / "mcp-connectors" / "base_connector.py"


def _spec(title: str = "Music") -> dict:
    artist = {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}}
    return {
        "openapi": "3.0.3",
        "info": {"title": title, "version": "1"},
        "servers": [{"url": "https://api.music.test/v1"}],
        "components": {"securitySchemes": {"bearer": {"type": "http", "scheme": "bearer"}}},
        "security": [{"bearer": []}],
        "paths": {"/artists": {"get": {
            "operationId": "listArtists",
            "parameters": [{"name": "limit", "in": "query", "schema": {"type": "integer"}},
                           {"name": "offset", "in": "query", "schema": {"type": "integer"}}],
            "responses": {"200": {"description": "ok", "content": {"application/json": {
                "schema": {"type": "object", "properties": {"artists": {"type": "array", "items": artist}}},
                "example": {"artists": [{"id": "a1", "name": "Nina"}]},
            }}}},
        }}},
    }


@pytest.fixture
def catalog(tmp_path, monkeypatch) -> Path:
    """A connectors tree holding base_connector.py; returns the connector's dir."""
    if _TOOLGEN not in sys.path:
        sys.path.insert(0, _TOOLGEN)
    root = tmp_path / "mcp-connectors"
    (root / "public").mkdir(parents=True)
    (root / "base_connector.py").write_text(_BASE_CONNECTOR.read_text())
    monkeypatch.delenv(FLAG, raising=False)

    from src.agents.tool_generator.agents import session_fast_path as sfp

    # Offline: no logo download, no OAuth registry write.
    monkeypatch.setattr(sfp, "_download_logo_safe", lambda *a, **k: None)
    monkeypatch.setattr(sfp, "_upsert_oauth_provider_safe", lambda *a, **k: None)
    return root / "public" / "music"


def _generate(monkeypatch, output_dir: Path, *, overwrite: bool = False, title: str = "Music") -> dict:
    import utils.session_store as session_store_mod
    from agents.discoverer import build_contract_from_spec
    from src.agents.tool_generator.agents import session_fast_path as sfp

    openapi = _spec(title)
    contract = asyncio.run(build_contract_from_spec(
        "music", openapi_spec=json.dumps(openapi), base_url=openapi["servers"][0]["url"]))
    assert contract is not None and contract.can_generate, getattr(contract, "refusal_reason", None)

    class _Store:
        async def get(self, session_id):
            return contract

    async def _get_store():
        return _Store()

    monkeypatch.setattr(session_store_mod, "get_session_store", _get_store)
    return asyncio.run(sfp.try_session_fast_path(
        api_name="music",
        session_id="sess-overwrite",
        canonical_id="music",
        save_artifacts=True,
        output_dir=str(output_dir),
        overwrite=overwrite,
    ))


def _versions(conn: Path) -> list:
    return sorted(p.name for p in (conn / "versions").iterdir())


def _latest(conn: Path) -> dict:
    return json.loads((conn / "latest.json").read_text())


def _mark(conn: Path, version: str) -> Path:
    """Stand in for a hand fix: a line the generator never writes."""
    target = conn / "versions" / version / "connector.py"
    target.write_text(target.read_text() + "\n# HAND-FIX-MARKER\n")
    return target


def test_a_regeneration_writes_a_new_version_by_default(monkeypatch, catalog):
    first = _generate(monkeypatch, catalog)
    assert first["success"], first
    _mark(catalog, "v1.0.0")

    second = _generate(monkeypatch, catalog)

    assert second["success"], second
    assert second["version"] == "v1.0.1"
    assert second.get("overwritten") is not True
    assert _versions(catalog) == ["v1.0.0", "v1.0.1"]
    assert _latest(catalog)["current_version"] == "v1.0.1"
    assert "HAND-FIX-MARKER" in (catalog / "versions" / "v1.0.0" / "connector.py").read_text()


def test_overwrite_rewrites_the_current_version_in_place(monkeypatch, catalog):
    assert _generate(monkeypatch, catalog)["success"]
    marked = _mark(catalog, "v1.0.0")
    (catalog / "versions" / "v1.0.0" / "logo.svg").write_text("<svg/>")

    result = _generate(monkeypatch, catalog, overwrite=True)

    assert result["success"], result
    assert result["version"] == "v1.0.0"
    assert result["overwritten"] is True
    assert _versions(catalog) == ["v1.0.0"], "an overwrite wrote a new version"
    assert "HAND-FIX-MARKER" not in marked.read_text(), "the old code is still there"
    latest = _latest(catalog)
    assert latest["current_version"] == "v1.0.0"
    assert latest["all_versions"] == ["v1.0.0"]
    assert any("verwritten" in c for c in latest["changelog"]["v1.0.0"]["changes"]), latest["changelog"]
    assert (catalog / "versions" / "v1.0.0" / "logo.svg").read_text() == "<svg/>", "the logo was lost"
    assert sorted(p.name for p in catalog.iterdir()) == ["latest.json", "versions"]


def test_overwrite_follows_latest_json_not_the_highest_directory(monkeypatch, catalog):
    assert _generate(monkeypatch, catalog)["success"]
    assert _generate(monkeypatch, catalog)["version"] == "v1.0.1"
    latest = _latest(catalog)
    latest["current_version"] = "v1.0.0"  # rolled back by hand
    (catalog / "latest.json").write_text(json.dumps(latest))
    newer = _mark(catalog, "v1.0.1")

    result = _generate(monkeypatch, catalog, overwrite=True)

    assert result["success"], result
    assert result["version"] == "v1.0.0"
    assert _versions(catalog) == ["v1.0.0", "v1.0.1"]
    assert _latest(catalog)["current_version"] == "v1.0.0"
    assert "HAND-FIX-MARKER" in newer.read_text(), "the overwrite touched another version"


def test_a_refused_overwrite_puts_the_old_version_back(monkeypatch, catalog):
    assert _generate(monkeypatch, catalog)["success"]
    marked = _mark(catalog, "v1.0.0")
    before_files = sorted(p.name for p in (catalog / "versions" / "v1.0.0").iterdir())
    before_latest = (catalog / "latest.json").read_text()

    from src.agents.tool_generator.validation import protocol_invariants

    def _refuse(**_kw):
        raise protocol_invariants.ProtocolInvariantError("probe: refused after the write")

    monkeypatch.setattr(protocol_invariants, "validate_or_raise", _refuse)
    result = _generate(monkeypatch, catalog, overwrite=True, title="Music Two")

    assert result["success"] is False, result
    assert result["error_stage"] == "persist", result
    assert _versions(catalog) == ["v1.0.0"]
    assert "HAND-FIX-MARKER" in marked.read_text(), "a refused overwrite lost the old code"
    assert sorted(p.name for p in (catalog / "versions" / "v1.0.0").iterdir()) == before_files
    assert (catalog / "latest.json").read_text() == before_latest
    assert sorted(p.name for p in catalog.iterdir()) == ["latest.json", "versions"]


def test_overwrite_of_a_connector_that_does_not_exist_writes_v1_0_0(monkeypatch, catalog):
    result = _generate(monkeypatch, catalog, overwrite=True)

    assert result["success"], result
    assert result["version"] == "v1.0.0"
    assert result.get("overwritten") is not True, "nothing existed to overwrite"
    assert _versions(catalog) == ["v1.0.0"]
