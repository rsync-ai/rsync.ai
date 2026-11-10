"""No NL request reaches the DAG planner: in-pipeline DAG mode is switched off.

ADR-001 (docs/internal/adr-001-dag-mode-switched-off.md). The planner used
to put DAGPlanningStrategy in front of the ordinary planner, and its can_handle
took any request with "then", "when", "if", "notify", "workflow" and the like
whose destination was not a SQL or warehouse store. Such a request became a
graph that ran once, lived only in the run's Temporal state and could not
notify. "Sync these tables, then tell me when it's done" is a plain pipeline
plus the alerts under Admin -> Notifications, so every request now plans as one.

The DAG planner package and the adapter's DAG runtime stay until the later
cleanup, so a DAG-mode run already open can finish; only the route in is gone.
"""

import sys
from pathlib import Path

import pytest

LLM_SERVICE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(LLM_SERVICE))

import src.agents.planner.service as service  # noqa: E402
from src.agents.dag_planner.strategy import DAGPlanningStrategy  # noqa: E402
from src.agents.planner.strategies import (  # noqa: E402
    CompositePlanningStrategy,
    PlanningContext,
    PlanResult,
)

# Each of these took the DAG route before the switch-off (the control in the
# behavioural test re-checks that), across the non-SQL destinations it served.
REQUESTS = [
    "sync the orders table to {dest}, then notify me on Slack when it finishes",
    "load orders then customers into {dest}",
    "copy the events table to {dest} and alert me when it is done",
    "if the orders table changed, export it to {dest}",
    "build a workflow that moves the users table to {dest}",
]
DESTINATIONS = ["gcs", "mongodb", "aws-s3", "azure-blob"]


def _strategies_in(strategy):
    """Every strategy a composite can hand a request to, depth first."""
    yield strategy
    if isinstance(strategy, CompositePlanningStrategy):
        yield from _strategies_in(strategy.primary)
        yield from _strategies_in(strategy.fallback)


def test_the_planner_chain_holds_no_dag_strategy():
    names = [s.name for s in _strategies_in(service.planning_strategy)]
    assert not any(isinstance(s, DAGPlanningStrategy) for s in _strategies_in(service.planning_strategy)), names
    # Control: the walk does see the ordinary planners, so an empty walk cannot pass.
    assert {"llm", "heuristic"} <= set(names), names


@pytest.mark.parametrize("dest", DESTINATIONS)
@pytest.mark.parametrize("template", REQUESTS)
def test_a_branching_request_plans_as_a_plain_pipeline(monkeypatch, template, dest):
    ctx = PlanningContext(
        natural_language=template.format(dest=dest),
        detected_tools=["postgresql", dest],
        source_connection={"connector_type": "postgresql"},
        destination_connection={"connector_type": dest},
        sync_mode="batch",
    )
    # Control: the request is one DAG mode would have taken, so this test can
    # fail for the reason it exists.
    assert DAGPlanningStrategy(llm_client=None).can_handle(ctx) is True

    dag_calls = []

    def dag_plan(self, c):
        dag_calls.append(c)
        return PlanResult(success=False, pipeline_id="p", error="recorded", strategy_used="dag_planner")

    monkeypatch.setattr(DAGPlanningStrategy, "create_plan", dag_plan)

    def plain(name):
        return lambda c: PlanResult(success=True, pipeline_id="p", plan={"steps": []}, strategy_used=name)

    monkeypatch.setattr(service.llm_strategy, "create_plan", plain("llm"))
    monkeypatch.setattr(service.heuristic_strategy, "create_plan", plain("heuristic"))

    result = service.planning_strategy.create_plan(ctx)

    assert dag_calls == []
    assert result.strategy_used == "llm"
