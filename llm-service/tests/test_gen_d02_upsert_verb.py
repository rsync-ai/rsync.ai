"""RED regression for defect d02_upsert_verb — upsert hard-codes PUT.

The generated REST connector's ``upsert_data`` operation issues the update
request with a HARD-CODED ``PUT`` verb for every resource
(``templates/connector.py.j2`` ~line 2110:
``self._make_request('PUT', f"{endpoint}/{rec_id}", ...)``). APIs whose item
route only accepts PATCH (HubSpot ``PATCH /crm/v3/objects/{object}/{id}``,
Intercom, …) answer PUT with **405 Method Not Allowed**, so every id-bearing
upsert row silently fails.

The fix introduces a per-resource update verb: ``ResourceConfig.update_method``
(default ``"PUT"``), populated from the spec item route (PATCH when the route
exposes PATCH, else PUT) / from the vendor resource dict, and emitted by the
template as a per-resource ``resource_update_methods`` map looked up by resource
name.

This test renders fully IN-PROCESS (no network, no docker, no persistence to
shared/mcp-connectors) via the production spec-build → template-render path:

    GenerationContract  ->  agents.architect_rest.build_rest_spec
                        ->  generator.builder.ConnectorBuilder().build(spec)
                        ->  assert on the returned connector.py source string

Import convention mirrors the tool_generator suite
(``tests/test_parity_phase_13f.py``): the tool_generator dir is placed on
sys.path so ``agents.*`` / ``schemas.*`` / ``generator.*`` resolve to the
tool_generator package (not the outer ``src/agents`` package).

RED today: the PATCH resource's generated upsert still emits ``PUT``.
GREEN after the fix: the PATCH resource maps to ``PATCH`` and the PUT resource
maps to ``PUT``, both wired per-resource.
"""

from __future__ import annotations

import os
import re
import sys

import pytest

# tool_generator package root: .../llm-service/src/agents/tool_generator
_TOOLGEN = os.path.abspath(
    os.path.join(
        os.path.dirname(__file__),  # .../llm-service/tests
        "..",                       # .../llm-service
        "src",
        "agents",
        "tool_generator",
    )
)
if _TOOLGEN not in sys.path:
    sys.path.insert(0, _TOOLGEN)

from schemas.contract import (  # noqa: E402
    Dimension,
    Fact,
    FactSource,
    empty_contract,
)
from agents.architect_rest import build_rest_spec  # noqa: E402
from generator.builder import ConnectorBuilder  # noqa: E402


# ---------------------------------------------------------------------------
# Inline minimal-contract helper (1-2 resources is enough for this render)
# ---------------------------------------------------------------------------


def _contract_with_resources(resources: list[dict]) -> "object":
    """A minimal generate-ready REST contract whose resources come straight from
    ``metadata['vendor_rest_resources']`` (the vendor-curated fast-path that
    ``_build_resources`` consumes first via ``_resource_from_dict``)."""
    contract = empty_contract("d02-sess", "acme")
    for dim, value in (
        (Dimension.PROTOCOL, "rest"),
        (Dimension.BASE_URL, "https://api.acme.example/v1"),
        (Dimension.AUTH_TYPE, "bearer"),
        (Dimension.OPERATIONS, [r["name"] for r in resources]),
    ):
        contract.set_fact(
            Fact(
                dimension=dim,
                value=value,
                confidence=1.0,
                source=FactSource.VENDOR_YAML,
            )
        )
    contract.metadata["vendor_rest_resources"] = resources
    contract.evaluate()
    return contract


def _render(resources: list[dict]) -> str:
    """Render connector.py fully in-process and return its source."""
    spec = build_rest_spec(
        _contract_with_resources(resources),
        connector_name="acme",
        display_name="Acme",
        description="Acme test connector",
    )
    generated = ConnectorBuilder().build(spec, include_dockerfile=False)
    assert generated.is_valid, generated.validation_errors
    assert generated.code, "builder returned empty connector code"
    return generated.code


# A resource whose item route updates via PATCH (HubSpot/Intercom style) and a
# resource that updates via PUT. Both must be writable (supports_update) so the
# generated `upsert_data` method is emitted.
# Writable = create-on-miss (append/import_data) + update-on-hit (upsert_data).
# supports_create is required so the spec emits import_data and validates as a
# destination connector; supports_update emits the upsert_data method under test.
_PATCH_RES = {
    "name": "contacts",
    "endpoint": "/crm/v3/objects/contacts",
    "id_field": "id",
    "supports_create": True,
    "supports_update": True,
    "update_method": "PATCH",
}
_PUT_RES = {
    "name": "widgets",
    "endpoint": "/widgets",
    "id_field": "id",
    "supports_create": True,
    "supports_update": True,
    "update_method": "PUT",
}


def _resource_verb_map(code: str) -> dict[str, str]:
    """Extract the generated per-resource update-verb wiring.

    Matches the fixed template's mapping entries, e.g.
        "contacts": "PATCH",
        "widgets": "PUT",
    Only PUT/PATCH values are considered so unrelated string maps
    (resource_endpoints / resource_id_fields) don't pollute the result.
    """
    return {
        name: verb
        for name, verb in re.findall(
            r'"([A-Za-z0-9_]+)"\s*:\s*"(PUT|PATCH)"', code
        )
    }


def test_upsert_emits_patch_for_patch_resource_and_put_for_put_resource():
    """The core d02 assertion.

    RED today: the PATCH resource is upserted with a hard-coded PUT.
    GREEN after fix: the PATCH resource maps to PATCH, the PUT resource to PUT.
    """
    code = _render([_PATCH_RES, _PUT_RES])

    # The upsert_data method must be present (both resources are writable).
    assert "def upsert_data(" in code, "upsert_data operation was not generated"

    verbs = _resource_verb_map(code)
    assert verbs.get("contacts") == "PATCH", (
        "upsert must issue PATCH for a PATCH-updatable resource "
        "(HubSpot/Intercom would 405 on PUT); generated verb map was "
        f"{verbs!r}"
    )
    assert verbs.get("widgets") == "PUT", (
        "upsert must still issue PUT for a PUT-updatable resource; "
        f"generated verb map was {verbs!r}"
    )


def test_upsert_does_not_hardcode_put_for_all_resources():
    """Guards against a per-resource verb that silently collapses back to PUT.

    A PATCH-only resource must NOT be updated with an unconditional
    ``self._make_request('PUT', ...)`` literal (the current defect).
    """
    code = _render([_PATCH_RES])

    # The old, broken template hard-codes the verb inside the upsert loop:
    #     result = self._make_request('PUT', f"{endpoint}/{rec_id}", ...)
    # The fixed template resolves the verb per-resource before the call, so this
    # exact hard-coded-PUT update call must be gone.
    assert "self._make_request('PUT', f\"{endpoint}/{rec_id}\"" not in code, (
        "upsert still hard-codes PUT for the item-route update call; it must "
        "use a per-resource update verb so PATCH-only APIs don't 405"
    )
    # And the PATCH resource must actually be wired to PATCH.
    assert _resource_verb_map(code).get("contacts") == "PATCH"


def test_update_method_defaults_to_put_when_unspecified():
    """A writable resource that declares no update verb defaults to PUT
    (backwards-compatible with today's PUT-only connectors)."""
    res = {
        "name": "orders",
        "endpoint": "/orders",
        "id_field": "id",
        "supports_create": True,
        "supports_update": True,
        # no update_method -> ResourceConfig default "PUT"
    }
    code = _render([res])
    assert _resource_verb_map(code).get("orders") == "PUT"


# ---------------------------------------------------------------------------
# The destination writes reach the record (d08's bug class, on the write path)
# ---------------------------------------------------------------------------
#
# An endpoint ending in its item placeholder (`/pet/{petId}`): upsert_data and
# delete_data built `{endpoint}/{id}` and left `{petId}` unbound, and every
# create POSTed to the item path. The CRUD helpers are covered in
# test_gen_d08_path_param_binding.py.

import importlib.util  # noqa: E402
import tempfile  # noqa: E402
from pathlib import Path  # noqa: E402

_SHARED = Path(__file__).resolve().parents[2] / "shared" / "mcp-connectors"
_PET_RES = {
    "name": "pet", "endpoint": "/pet/{petId}", "id_field": "id",
    "supports_create": True, "supports_update": True, "supports_delete": True,
}


def _load_generated(code: str):
    path = Path(tempfile.mkdtemp(prefix="gen_d02_")) / "acme" / "connector.py"
    path.parent.mkdir(parents=True)
    path.write_text(code)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [str(_SHARED / "public"), str(_SHARED)]
    spec = importlib.util.spec_from_file_location("_gen_d02_acme", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    return mod


@pytest.fixture
def pet_destination(monkeypatch):
    """The rendered connector with HTTP stubbed: each request is recorded as
    (method, url); an update of id 7 answers 404 so upsert falls back to create."""
    mod = _load_generated(_render([_PET_RES]))
    sent = []

    def fake_send(self, method, url, **kwargs):
        sent.append((method, url))
        if method == "PUT" and url.endswith("/7"):
            return False, 404, {}, {}
        return True, 200, {}, {}

    monkeypatch.setattr(mod.BaseMCPConnector, "_make_request_v2", fake_send)
    cls = next(v for k, v in vars(mod).items()
               if isinstance(v, type) and k.endswith("Connector") and v.__module__ == mod.__name__)
    return cls(), sent


def test_upsert_puts_to_the_item_and_creates_on_the_collection(pet_destination):
    srv, sent = pet_destination
    res = srv.upsert_data({"config": {"api_key": "k"}, "object": "pet",
                           "data": [{"id": 42, "name": "rex"}, {"id": 7}, {"name": "new"}]})
    assert res.get("success"), res
    assert sent == [
        ("PUT", "https://api.acme.example/v1/pet/42"),
        ("PUT", "https://api.acme.example/v1/pet/7"),
        ("POST", "https://api.acme.example/v1/pet"),
        ("POST", "https://api.acme.example/v1/pet"),
    ]


def test_delete_data_deletes_the_item(pet_destination):
    srv, sent = pet_destination
    res = srv.delete_data({"config": {"api_key": "k"}, "object": "pet", "data": [{"id": 42}]})
    assert res.get("success"), res
    assert sent == [("DELETE", "https://api.acme.example/v1/pet/42")]


def test_import_data_creates_on_the_collection(pet_destination):
    srv, sent = pet_destination
    res = srv.import_data({"config": {"api_key": "k"}, "object": "pet", "data": [{"name": "rex"}]})
    assert res.get("success"), res
    assert sent == [("POST", "https://api.acme.example/v1/pet")]
