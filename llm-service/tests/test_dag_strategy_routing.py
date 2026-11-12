"""Which requests the DAG planner takes: the destination decides, not the source.

KI-DAG-ROUTING-KEYED-ON-SOURCE. DAGPlanningStrategy.can_handle sends a
relational/warehouse DESTINATION to the heuristic planner. It also read
detected_tools, which names the source as well, so the same "sync X to GCS,
then notify me on Slack" request went to the DAG planner for a MongoDB source
and to the heuristic planner for a PostgreSQL one. detected_tools stands in
for the destination only when the destination connection is unknown.
"""

import pytest

from src.agents.dag_planner.strategy import DAGPlanningStrategy
from src.agents.planner.strategies import PlanningContext

NOTIFY = "sync the orders table to GCS, then notify me on Slack when it finishes"


def _ctx(source, dest, detected=None):
    return PlanningContext(
        natural_language=NOTIFY,
        detected_tools=detected if detected is not None else [t for t in (source, dest) if t],
        source_connection={"connector_type": source} if source else None,
        destination_connection={"connector_type": dest} if dest else None,
    )


@pytest.fixture
def strategy():
    return DAGPlanningStrategy(llm_client=None)


@pytest.mark.parametrize("source", ["mongodb", "postgresql", "mysql", "bigquery", "snowflake"])
def test_the_source_type_does_not_decide_the_route(strategy, source):
    assert strategy.can_handle(_ctx(source, "gcs")) is True


@pytest.mark.parametrize("dest", ["postgresql", "mysql", "bigquery", "snowflake", "redshift"])
def test_a_warehouse_destination_still_goes_to_the_heuristic_planner(strategy, dest):
    assert strategy.can_handle(_ctx("mongodb", dest)) is False


def test_detected_tools_decide_only_without_a_destination_connection(strategy):
    assert strategy.can_handle(_ctx("mongodb", None, detected=["mongodb", "postgresql"])) is False
    # Control: no warehouse among the detected tools.
    assert strategy.can_handle(_ctx("mongodb", None, detected=["mongodb", "gcs"])) is True
