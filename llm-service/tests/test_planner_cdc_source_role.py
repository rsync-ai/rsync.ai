"""A CDC plan streams from the SOURCE connector, never the destination.

Prod pipeline a6ca2103 (PostgreSQL -> MongoDB CDC, 2026-10-01) logged
``CDC metadata: position_method=oplog, connector_class=...MongoDbConnector``.
MongoDB and PostgreSQL can each be a source or a destination, so the heuristic
planner treated both as sources and took the first of a ``set()`` — random
order. The roles it should have used never arrived: the orchestrator sent the
connection ids only, and the planner's own lookup of those ids goes to the
gateway's ``/api/v1/connections``, which needs a session the planner does not
hold. The Debezium connector replaces a mismatched class, which is the only
reason the deployed connector was right.

The class is "the planner guesses roles from detected tools": these tests give
the planner the types the orchestrator resolved, with the connection lookup
failing as it does in prod, and require one CDC step on the source.
"""

import asyncio
import sys
from pathlib import Path

import pytest

LLM_SERVICE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(LLM_SERVICE))

import src.agents.planner.service as service  # noqa: E402
from src.agents.planner.strategies import (  # noqa: E402
    CDCProviderRegistry,
    HeuristicPlanningStrategy,
)

_CONNECTORS = LLM_SERVICE.parent / "shared" / "mcp-connectors"
PG_CLASS = "io.debezium.connector.postgresql.PostgresConnector"
MONGO_CLASS = "io.debezium.connector.mongodb.MongoDbConnector"


class _Registry:
    """Both engines are sources AND destinations, as in the live registry."""

    def get_databases(self):
        return ["mongodb", "postgresql", "mysql"]

    def get_destinations(self):
        return ["mongodb", "postgresql", "mysql", "gcs"]

    def get_apis(self):
        return []

    def get_all_tools(self):
        return sorted(set(self.get_databases() + self.get_destinations()))


class _NoTopics:
    def provision_topic(self, **_kwargs):
        raise RuntimeError("no kafka in unit tests")


@pytest.fixture
def heuristic():
    strategy = HeuristicPlanningStrategy(_Registry(), connectors_dir=str(_CONNECTORS))
    # The module keeps one registry, built at import from /app paths; read the repo's.
    strategy.cdc_registry = CDCProviderRegistry(str(_CONNECTORS))
    assert strategy.cdc_registry.get_cdc_provider("postgresql"), "no CDC metadata read"
    strategy.topic_provisioner = _NoTopics()
    return strategy


@pytest.fixture
def planner(monkeypatch, heuristic):
    async def _ensure(tools, _nl=None):
        return list(tools), []

    monkeypatch.setattr(service, "planning_strategy", heuristic)
    monkeypatch.setattr(service, "fetch_registry_tools", lambda: [])
    # The prod lookup: the gateway answers 401, so no connection resolves.
    monkeypatch.setattr(service, "fetch_connections", lambda _user=None: [])
    monkeypatch.setattr(service.connector_checker, "ensure_connectors_exist_async", _ensure)

    def _plan(context):
        req = service.PlanRequest(
            natural_language="Stream CDC from postgresql to mongodb. Stream changes from Postgresql Source to MongoDB Dest",
            pipeline_id="a6ca2103-0000-0000-0000-000000000000",
            context=context,
        )
        resp = asyncio.run(service.create_plan(req))
        assert resp.success, resp.error
        return resp.plan

    return _plan


def _cdc_steps(plan):
    return [s for s in plan["steps"] if (s.get("params") or {}).get("connector_class")]


@pytest.mark.parametrize(
    "context",
    [
        # What the orchestrator's planner worker sends (planner.go).
        {"source_type": "postgresql", "destination_type": "mongodb"},
        # The V2 task's capability-resolver output, forwarded as-is.
        {"connectors": {"resolved_connectors": {"source": "postgresql", "destination": "mongodb"}}},
        # The legacy shape: connector maps.
        {"source_connector": {"type": "postgresql"}, "destination_connector": {"type": "mongodb"}},
    ],
    ids=["types", "resolved_connectors", "connector_maps"],
)
def test_pg_to_mongo_cdc_streams_from_postgres(planner, context):
    plan = planner({**context, "sync_mode": "cdc",
                    "source_connection_id": "src-1", "destination_connection_id": "dst-1"})
    steps = _cdc_steps(plan)
    assert [s["params"]["connector_class"] for s in steps] == [PG_CLASS], plan["steps"]
    assert plan["cdc_metadata"]["connector_class"] == PG_CLASS


def test_mongo_to_pg_cdc_streams_from_mongo(planner):
    """Control: the same two engines the other way round pick Mongo's class."""
    plan = planner({"source_type": "mongodb", "destination_type": "postgresql", "sync_mode": "cdc"})
    assert [s["params"]["connector_class"] for s in _cdc_steps(plan)] == [MONGO_CLASS]
    assert plan["cdc_metadata"]["connector_class"] == MONGO_CLASS


@pytest.mark.parametrize(
    "context, want",
    [
        ({"source_type": "PostgreSQL", "destination_type": "google_cloud_storage"}, ("postgresql", "google-cloud-storage")),
        ({"source_connector": "mysql", "destination_connector": {"connector_type": "gcs"}}, ("mysql", "gcs")),
        ({"connectors": {"resolved_connectors": {"source": "mongodb", "destination": "gcs"}}}, ("mongodb", "gcs")),
        ({"source_connection": {"connector_type": "postgresql"}}, ("postgresql", None)),
        ({}, (None, None)),
        (None, (None, None)),
    ],
)
def test_connector_role_types(context, want):
    from src.agents.planner.strategies import connector_role_types

    assert connector_role_types(context) == want
