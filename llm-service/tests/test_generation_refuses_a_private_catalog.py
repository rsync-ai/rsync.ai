"""A generated connector is refused where no other service could ever read it.

The Helm chart gives every pod its OWN copy of the connector catalog: an
initContainer copies the ``connector-seed`` image into a per-pod emptyDir
(deploy/helm/rsync-ai/templates/_helpers.tpl, ``rsync-ai.catalogInitContainer``).
There is no shared volume, by design -- it would need ReadWriteMany storage. So
when tool-generator saved a connector generated from an OpenAPI document (#1264),
the files landed in tool-generator's emptyDir only: the api-gateway, the
orchestrator and the temporal-adapter read their own copies and never saw it,
and the next tool-generator restart deleted it. The spec-upload screen still
said "Generated" and named the path it was saved to.

The chart now sets ``RSYNC_CONNECTOR_CATALOG_SHARED=false`` on tool-generator,
and both writers refuse before they write anything. The class is "a generated
connector saved into a catalog only its writer reads", so the guard sits on each
writer, not on one route:

  * ``DeploymentService.deploy``: the agentic pipeline and the community route.
  * ``session_fast_path.try_session_fast_path``: the discovery fast path and
    spec-first, which reuses it. It writes to disk itself and never reaches
    ``deploy``, so it was saving past the guard until an agent or Ask rsync.ai
    could start a generation and it was swept with the rest.

The route test below checks the refusal reaches the screen as a readable message
rather than as a success.

Unset (every Docker install) keeps today's behaviour, and each accepted spelling
of "false" is pinned, because a guard that only knew one spelling would let the
chart's value drift past it.
"""
from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.agents.tool_generator.deployment.service import (
    ConnectorArtifacts,
    DeploymentService,
    DeploymentStatus,
)
from src.lifecycle.scaffold_routes import scaffold_router

FLAG = "RSYNC_CONNECTOR_CATALOG_SHARED"

SPEC = json.dumps(
    {
        "openapi": "3.0.0",
        "info": {"title": "Widget API", "version": "1.0"},
        "servers": [{"url": "https://api.example.com/v1"}],
        "paths": {
            "/widgets": {
                "get": {"operationId": "listWidgets", "responses": {"200": {"description": "ok"}}}
            }
        },
    }
)


def _artifacts(name: str) -> ConnectorArtifacts:
    return ConnectorArtifacts(
        name=name,
        code="def handler():\n    return 1\n",
        metadata_json=json.dumps({"id": name, "connector_type": name, "name": name}),
        requirements_txt="requests\n",
        version="latest",
    )


@pytest.fixture
def catalog(tmp_path: Path, monkeypatch) -> Path:
    (tmp_path / "public").mkdir()
    (tmp_path / "public" / "base_connector.py").write_text("# base\n")
    monkeypatch.setenv("TOOLS_DIR", str(tmp_path))
    monkeypatch.delenv("RSYNC_MANAGED_CONNECTORS", raising=False)

    async def _no_logo(self, connector_name, output_dir):
        return False

    monkeypatch.setattr(DeploymentService, "_download_logo_to_path", _no_logo)
    return tmp_path


def _written(catalog: Path) -> list[str]:
    return sorted(
        str(p.relative_to(catalog))
        for p in catalog.rglob("*")
        if p.is_file() and p.name != "base_connector.py"
    )


@pytest.mark.parametrize("value", ["false", "False", "0", "no"])
def test_a_private_catalog_refuses_before_writing(catalog, monkeypatch, value):
    monkeypatch.setenv(FLAG, value)
    result = asyncio.run(
        DeploymentService(connectors_path=str(catalog)).deploy(
            _artifacts("xkcd"), build_docker=False, start_container=False
        )
    )
    assert result.success is False
    assert result.status == DeploymentStatus.FAILED
    assert FLAG in result.error_message, result.error_message
    assert "Docker install" in result.error_message, (
        "the refusal must say where generated connectors do work: " + result.error_message
    )
    assert _written(catalog) == [], "a refused generation still wrote files"


@pytest.mark.parametrize("value", [None, "", "true", "1"])
def test_a_shared_catalog_still_saves(catalog, monkeypatch, value):
    # Control: without it, the refusal above would also pass on a writer that
    # could not write at all.
    if value is None:
        monkeypatch.delenv(FLAG, raising=False)
    else:
        monkeypatch.setenv(FLAG, value)
    result = asyncio.run(
        DeploymentService(connectors_path=str(catalog)).deploy(
            _artifacts("xkcd"), build_docker=False, start_container=False, verify=False
        )
    )
    assert result.success is True, result.error_message
    assert any(p.endswith("connector.py") for p in _written(catalog)), _written(catalog)


@pytest.fixture
def client(monkeypatch):
    monkeypatch.setenv("ENVIRONMENT", "development")
    monkeypatch.delenv("INTERNAL_SERVICE_SECRET", raising=False)
    app = FastAPI()
    app.include_router(scaffold_router, prefix="/v1")
    return TestClient(app)


def test_the_spec_upload_screen_gets_the_refusal_not_a_success(catalog, client, monkeypatch):
    monkeypatch.setenv(FLAG, "false")
    res = client.post(
        "/v1/generate",
        json={"api_name": "widget_api", "openapi_spec": SPEC, "save_artifacts": True},
    )
    body = res.json()
    # frontend/src/components/connectors/discovery/SpecUploadFlow.tsx shows
    # "Generated" on success and error_message otherwise.
    assert body["success"] is False, body
    assert FLAG in (body["error_message"] or ""), body
    assert body["output_path"] is None, "a refusal must not name a path it did not keep"
    assert _written(catalog) == []


def test_a_dry_run_is_still_allowed_on_a_private_catalog(catalog, client, monkeypatch):
    # Previewing the rendered code writes nothing, so it stays available.
    monkeypatch.setenv(FLAG, "false")
    res = client.post(
        "/v1/generate",
        json={"api_name": "widget_api", "openapi_spec": SPEC, "save_artifacts": False},
    )
    assert res.status_code == 200, res.text
    assert res.json()["status"] == "completed_dry_run"


# --------------------------------------------------------------------------- #
# The second writer: the session fast path
# --------------------------------------------------------------------------- #
# Imported the way the fast path imports its siblings (bare ``agents.``,
# ``deployment.``), with the tool_generator directory on sys.path, as the
# test_gen_* suites in this process already do. Only dicts and strings cross the
# boundary, so the second module spelling compares nothing by class.
#
# The disk write is replaced by a recorder: a real one needs the package import
# layout for its protocol-invariant gate, which this flat layout cannot give.
# The question is only whether the fast path reaches its writer.

_TOOLGEN = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "src", "agents", "tool_generator")
)


def _fast_path_save(monkeypatch) -> tuple[dict, list]:
    if _TOOLGEN not in sys.path:
        sys.path.insert(0, _TOOLGEN)
    from schemas.contract import Dimension, Fact, FactSource, empty_contract
    import utils.session_store as session_store_mod
    from agents import session_fast_path

    contract = empty_contract("sess-private-catalog", "pipedrive")
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
        writes.append(str(output_dir))
        return "v1.0.0"

    monkeypatch.setattr(session_store_mod, "get_session_store", _get_store)
    monkeypatch.setattr(session_fast_path, "write_generated_artifacts", _record_write)
    result = asyncio.run(
        session_fast_path.try_session_fast_path(
            api_name="pipedrive",
            session_id="sess-private-catalog",
            canonical_id="pipedrive",
            save_artifacts=True,
            output_dir="/nonexistent/public/pipedrive",
        )
    )
    return result, writes


@pytest.mark.parametrize("value", ["false", "0"])
def test_the_fast_path_refuses_on_a_private_catalog(monkeypatch, value):
    monkeypatch.setenv(FLAG, value)
    result, writes = _fast_path_save(monkeypatch)
    assert result["success"] is False, result
    assert result["error_stage"] == "persist", result
    assert FLAG in result["error_message"], result["error_message"]
    assert writes == [], "a refused generation still reached the writer"


def test_the_fast_path_still_saves_on_a_shared_catalog(monkeypatch):
    # Control, as above: the same call reaches the writer when the catalog is shared.
    monkeypatch.delenv(FLAG, raising=False)
    result, writes = _fast_path_save(monkeypatch)
    assert result["success"] is True, result
    assert writes == ["/nonexistent/public/pipedrive"], writes
