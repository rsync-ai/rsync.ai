"""The DAG planner emits only node kinds the pipeline runtime can run.

KI-NODE-KINDS-DECLARED-BUT-UNROUTED, the planner's half. The temporal adapter
refuses a workflow_graph holding a kind it has no runtime for, or a graph
without exactly one destination, before any node starts. A planner that still
offered those kinds would plan pipelines that can never run. The list of
runnable kinds lives in shared/workflow_node_kinds_golden.json, which the
adapter and the executor tests read too.
"""

import json
import os
import re

import pytest
import yaml

from src.agents.dag_planner.agent import DAGPlannerAgent, PlannerState
from src.agents.dag_planner import types as planner_types
from src.agents.dag_planner.types import DAG_PLANNER_TOOLS, NodeKind

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
with open(os.path.join(REPO_ROOT, "shared", "workflow_node_kinds_golden.json")) as f:
    _GOLDEN = json.load(f)
with open(os.path.join(REPO_ROOT, "llm-service", "prompts", "planner", "dag_planning.yaml")) as f:
    _PROMPT = yaml.safe_load(f)["system"]

RUNNABLE = set(_GOLDEN["runnable"])
NOT_RUNNABLE = set(_GOLDEN["not_runnable"])
MAX_WAIT = _GOLDEN["wait"]["max_seconds"]


def test_golden_is_readable():
    assert RUNNABLE and NOT_RUNNABLE, "the golden lists no runnable or no refused kind"
    assert not RUNNABLE & NOT_RUNNABLE


def test_runnable_kinds_match_the_golden():
    assert set(planner_types.RUNNABLE_NODE_KINDS) == RUNNABLE
    assert planner_types.WAIT_SECONDS_KEY == _GOLDEN["wait"]["config_key"]
    assert planner_types.MAX_WAIT_SECONDS == MAX_WAIT


def test_declared_kinds_are_the_golden_union():
    assert {k.value for k in NodeKind} == RUNNABLE | NOT_RUNNABLE


def test_prompt_offers_only_runnable_kinds():
    section = _PROMPT.split("## Node Types", 1)[1].split("##", 1)[0]
    bullets = set(re.findall(r"^\s*-\s*\*\*(\w+)\*\*", section, re.MULTILINE))
    assert bullets == RUNNABLE, f"Node Types bullets {sorted(bullets)}"

    enum_lines = [line for line in _PROMPT.splitlines() if '"kind":' in line]
    assert len(enum_lines) == 1, enum_lines
    offered = set(re.search(r'"kind":\s*"([^"]+)"', enum_lines[0]).group(1).split("|"))
    assert offered == RUNNABLE, f"Output Format kind enum {sorted(offered)}"

    for kind in NOT_RUNNABLE:
        assert f"**{kind}**" not in _PROMPT, f"the prompt still describes {kind}"


def test_prompt_states_the_destination_and_wait_rules():
    assert "exactly one destination" in _PROMPT.lower()
    assert _GOLDEN["wait"]["config_key"] in _PROMPT


def test_node_template_tool_offers_only_runnable_kinds():
    tool = next(t for t in DAG_PLANNER_TOOLS if t.name == "get_node_template")
    assert set(tool.parameters["properties"]["node_kind"]["enum"]) == RUNNABLE


def _validate(nodes, edges=()):
    agent = DAGPlannerAgent(llm_client=None)
    state = PlannerState(user_request="x", pipeline_id="p")
    state.current_nodes = list(nodes)
    state.current_edges = list(edges)
    return agent._validate_dag(state).validation_errors


def _node(node_id, kind, config=None):
    return {"id": node_id, "kind": kind, "display_name": node_id, "config": config or {}}


def _chain(*nodes):
    ids = [n["id"] for n in nodes]
    return list(nodes), [{"from": a, "to": b, "type": "data"} for a, b in zip(ids, ids[1:])]


def test_validator_accepts_a_graph_of_runnable_kinds():
    nodes, edges = _chain(
        _node("extract", "source"),
        _node("wait_1", "wait", {"wait_seconds": 5}),
        _node("split_1", "split"),
        _node("merge_1", "merge"),
        _node("transform_1", "transform"),
        _node("load", "destination"),
    )
    assert _validate(nodes, edges) == []


@pytest.mark.parametrize("kind", sorted(NOT_RUNNABLE))
def test_validator_refuses_a_kind_with_no_runtime(kind):
    nodes, edges = _chain(_node("extract", "source"), _node("load", "destination"), _node("x_1", kind))
    errors = _validate(nodes, edges)
    hit = [e for e in errors if "x_1" in e and kind in e]
    assert hit, errors
    # The model reads the error on its retry, so it must say what IS allowed.
    assert all(k in hit[0] for k in RUNNABLE), hit[0]


@pytest.mark.parametrize("destinations", [0, 2])
def test_validator_wants_exactly_one_destination(destinations):
    nodes = [_node("extract", "source")] + [
        _node(f"load_{i}", "destination") for i in range(destinations)
    ]
    edges = [{"from": "extract", "to": n["id"], "type": "data"} for n in nodes[1:]]
    errors = _validate(nodes, edges)
    assert any("destination" in e for e in errors), errors


@pytest.mark.parametrize("config", [
    {},
    {"wait_seconds": 0},
    {"wait_seconds": -3},
    {"wait_seconds": MAX_WAIT + 1},
    {"wait_seconds": "5"},
    {"wait_seconds": True},
    {"wait_seconds": float("nan")},
])
def test_validator_refuses_a_wait_without_a_usable_duration(config):
    nodes, edges = _chain(_node("extract", "source"), _node("wait_1", "wait", config), _node("load", "destination"))
    errors = _validate(nodes, edges)
    assert any("wait_1" in e and "wait_seconds" in e for e in errors), errors


@pytest.mark.parametrize("seconds", [1, 2.5, MAX_WAIT])
def test_validator_accepts_a_wait_in_range(seconds):
    nodes, edges = _chain(
        _node("extract", "source"), _node("wait_1", "wait", {"wait_seconds": seconds}), _node("load", "destination")
    )
    assert _validate(nodes, edges) == []


def test_fallback_dag_drops_the_notification_it_cannot_run():
    agent = DAGPlannerAgent(llm_client=None)
    state = PlannerState(user_request="sync postgres to s3 then notify slack", pipeline_id="p")
    state = agent._generate_simple_dag(state)
    kinds = [n["kind"] for n in state.current_nodes]
    assert set(kinds) <= RUNNABLE, kinds
    assert kinds.count("destination") == 1, kinds
    assert agent._validate_dag(state).validation_errors == []
