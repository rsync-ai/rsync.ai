"""The DAG planner never keeps a placeholder the model wrote into a node's config.

"Copy the matches collection from my MongoDB Source to my GCS dest, then notify
me on Slack" planned a graph on prod (2026-09-30) whose node config held
"your_database_name" and "your_bucket_name". The model is never shown a
connection's database or bucket (connections_for_llm keeps id, name and type), so
when it fills a config key it has nothing real to put there. The connection
supplies those, and the table step supplies the tables; the executor forwards a
destination's config `tables` / `selected_tables`, so a placeholder there would
run as a table name. Bug class: a model-invented value kept as if it were
configuration.
"""

import json

from src.agents.dag_planner.agent import (
    DAGPlannerAgent,
    PlannerState,
    drop_placeholder_config,
)


def test_placeholders_are_dropped_and_real_values_kept():
    nodes = [
        {"id": "extract_mongo", "kind": "source", "config": {
            "database": "your_database_name",
            "collection": "matches",
            "connector_type": "mongodb",
        }},
        {"id": "load_gcs", "kind": "destination", "config": {
            "bucket": "YOUR-BUCKET",
            "prefix": "<prefix>",
            "tables": ["your_table_name", "datingapp.matches"],
            "selected_tables": ["<table>"],
            "operation": "data_transfer",
            "options": {"project": "{{project_id}}", "format": "parquet"},
        }},
        {"id": "pause", "kind": "wait", "config": {"wait_seconds": 60}},
    ]
    drop_placeholder_config(nodes)
    assert nodes[0]["config"] == {"collection": "matches", "connector_type": "mongodb"}
    assert nodes[1]["config"] == {
        "tables": ["datingapp.matches"],
        "operation": "data_transfer",
        "options": {"format": "parquet"},
    }
    assert nodes[2]["config"] == {"wait_seconds": 60}


def test_a_name_that_only_starts_with_your_is_kept():
    nodes = [{"id": "s", "kind": "source", "config": {"table": "yourls", "schema": "yours"}}]
    drop_placeholder_config(nodes)
    assert nodes[0]["config"] == {"table": "yourls", "schema": "yours"}


class _Client:
    def __init__(self, content):
        self._content = content

    def complete(self, _prompt, _context):
        return {"content": self._content}


def test_a_generated_graph_reaches_the_plan_without_its_placeholders():
    graph = {
        "nodes": [
            {"id": "extract_mongo", "kind": "source", "display_name": "Extract matches",
             "config": {"database": "your_database_name", "collection": "matches"}},
            {"id": "load_gcs", "kind": "destination", "display_name": "Load to GCS",
             "config": {"bucket": "your_bucket_name", "operation": "data_transfer"}},
        ],
        "edges": [{"from": "extract_mongo", "to": "load_gcs", "type": "data"}],
    }
    agent = DAGPlannerAgent(llm_client=_Client(json.dumps(graph)))
    state = agent._generate_dag(PlannerState(user_request="copy matches to gcs then notify me", pipeline_id="p"))
    configs = {n["id"]: n["config"] for n in state.current_nodes}
    assert configs == {
        "extract_mongo": {"collection": "matches"},
        "load_gcs": {"operation": "data_transfer"},
    }


def test_the_prompt_forbids_an_invented_config_value():
    import pathlib

    import yaml

    system = yaml.safe_load(
        (pathlib.Path(__file__).resolve().parents[1] / "prompts/planner/dag_planning.yaml").read_text()
    )["system"]
    assert "never a placeholder" in system
    assert "table-selection step" in system
