"""A generated connector that fails preflight is refused, not saved as a draft.

The fast path (and spec-first, which reuses it) ran preflight after the build
and then saved the connector whatever it found: a preflight error downgraded the
tier to "draft", wrote the files over the version already on disk, and answered
``success: true`` with the error tucked into ``error_message``. A caller that
reads ``success`` -- the generation record in the api-gateway, Ask rsync.ai, an
agent -- took a connector whose metadata no service could load for a finished
one, and the working version it replaced was gone.

The class is "a generation reported as finished after one of its own checks
failed". The builder's ``is_valid`` gate already refused before saving; preflight
now refuses the same way: ``success: false``, ``error_stage: "preflight"``, the
issues returned, and the writer never reached. Warnings (an empty connection
form) still save, and their issues still reach the caller.

``GenerateResponseV1`` is checked too: it dropped ``preflight_issues``, so the
issues never left llm-service even when the fast path returned them.
"""
from __future__ import annotations

import asyncio
import json
import os
import sys

import pytest

from src.agents.tool_generator.contracts.generate_v1 import GenerateResponseV1

FLAG = "RSYNC_CONNECTOR_CATALOG_SHARED"

# Imported the way the fast path imports its siblings (bare ``generator.``,
# ``agents.``), as test_generation_refuses_a_private_catalog.py does.
_TOOLGEN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "src", "agents", "tool_generator")
)


def _generate(monkeypatch, metadata_edit=None) -> tuple[dict, list]:
    """Run a real fast-path generation; ``metadata_edit`` rewrites the built
    metadata.json before preflight sees it."""
    if _TOOLGEN not in sys.path:
        sys.path.insert(0, _TOOLGEN)
    from schemas.contract import Dimension, Fact, FactSource, empty_contract
    import utils.session_store as session_store_mod
    from agents import session_fast_path
    from generator.builder import ConnectorBuilder

    contract = empty_contract("sess-preflight-gate", "pipedrive")
    for dim, value in (
        (Dimension.PROTOCOL, "rest"),
        (Dimension.BASE_URL, "https://api.pipedrive.com/v1"),
        (Dimension.AUTH_TYPE, "pipedrive"),
        (Dimension.OPERATIONS, ["list_deals"]),
    ):
        contract.set_fact(Fact(dimension=dim, value=value, confidence=1.0, source=FactSource.VENDOR_YAML))
    contract.evaluate()
    assert contract.can_generate, contract.refusal_reason

    class _Store:
        async def get(self, session_id):
            return contract

    async def _get_store():
        return _Store()

    writes: list = []

    def _record_write(generated, output_dir, connector_id, spec, *, replace_version=None):
        writes.append(json.loads(generated.metadata))
        return "v1.0.0"

    real_build = ConnectorBuilder.build

    def _build(self, spec, *args, **kwargs):
        generated = real_build(self, spec, *args, **kwargs)
        if metadata_edit is not None:
            meta = json.loads(generated.metadata)
            metadata_edit(meta)
            generated.metadata = json.dumps(meta)
        return generated

    monkeypatch.delenv(FLAG, raising=False)
    monkeypatch.setattr(session_store_mod, "get_session_store", _get_store)
    monkeypatch.setattr(session_fast_path, "write_generated_artifacts", _record_write)
    monkeypatch.setattr(ConnectorBuilder, "build", _build)
    result = asyncio.run(
        session_fast_path.try_session_fast_path(
            api_name="pipedrive",
            session_id="sess-preflight-gate",
            canonical_id="pipedrive",
            save_artifacts=True,
            output_dir="/nonexistent/public/pipedrive",
        )
    )
    return result, writes


def _drop_required_keys(meta: dict) -> None:
    # PRE012: no service can load a connector without these.
    meta.pop("version", None)
    meta.pop("supports_source", None)


def _drop_form(meta: dict) -> None:
    # PRE014, a warning: the connection form is empty, the connector still loads.
    meta.pop("configuration_schema", None)
    meta.pop("config_schema", None)


def _stage(result: dict, name: str) -> str:
    return next(s["status"] for s in result["workflow_stages"] if s["name"] == name)


def test_a_preflight_error_saves_nothing_and_reports_a_failure(monkeypatch):
    result, writes = _generate(monkeypatch, _drop_required_keys)
    assert writes == [], "a connector that failed preflight still reached the writer"
    assert result["success"] is False, result
    assert result["status"] == "failed", result
    assert result["error_stage"] == "preflight", result
    assert "missing required key" in result["error_message"], result["error_message"]
    assert "PRE012" in [i["code"] for i in result["preflight_issues"]], result["preflight_issues"]
    assert _stage(result, "preflight") == "failed"
    assert _stage(result, "persist") == "skipped"
    assert result.get("output_path") is None, "a refusal must not name a path it did not write"
    assert result.get("version") is None


def test_a_healthy_build_passes_preflight_and_is_saved(monkeypatch):
    # Control: without it, the refusal above would also pass on a fast path that
    # refused everything.
    result, writes = _generate(monkeypatch)
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert _stage(result, "preflight") == "completed"
    assert not [i for i in result.get("preflight_issues", []) if i["severity"] == "error"]


def test_a_preflight_warning_still_saves_and_reports_the_warning(monkeypatch):
    result, writes = _generate(monkeypatch, _drop_form)
    assert result["success"] is True, result
    assert len(writes) == 1, writes
    assert "configuration_schema" not in writes[0]
    assert [i["code"] for i in result["preflight_issues"]] == ["PRE014"], result["preflight_issues"]
    assert result.get("error_stage") is None, "a warning is not a failed stage"
    assert _stage(result, "preflight") == "completed"


def test_the_response_model_keeps_the_preflight_issues(monkeypatch):
    # integration.py:_try_session_fast_path returns
    # GenerateResponseV1.model_validate(result), so a field the model does not
    # declare never reaches the api-gateway.
    result, _ = _generate(monkeypatch, _drop_required_keys)
    dumped = GenerateResponseV1.model_validate(result).model_dump()
    assert dumped["success"] is False
    assert [i["code"] for i in dumped["preflight_issues"]] == [i["code"] for i in result["preflight_issues"]]
