"""Every place in llm-service that calls an LLM, and what each one sends.

The guard refuses a component that is not listed here, and a prompt a
component does not list. ``scripts/gen_llm_egress_doc.py`` renders this table
and each prompt's ``llm_fields`` into docs/security/llm-egress.md, so the
published answer to "what reaches the model" is generated from the code that
enforces it.

Three paths:

``prompt``
    The guard renders a prompt YAML itself, projecting every variable by its
    field class (src/egress/fields.py). This is the path for anything that
    carries metadata about a customer's systems.
``messages``
    The caller builds the messages. Allowed only for components whose inputs
    are not about a customer's systems at all: rsync.ai's own connector registry,
    public API documentation, a connector name the user typed.
``tools``
    The guard renders a prompt YAML as on the ``prompt`` path, then appends the
    caller's tool conversation: the user's typed question and the model's own
    turns, plus tool results. Those results are built by api-gateway's agent
    gateway, whose Output types list every field a tool can return and are
    canary-tested in api-gateway/internal/agentgw, so they carry names, types,
    statuses and scrubbed error text -- never row values. The caller may not
    send a system message: the only one is the rendered prompt's.

A call site's ``data_classes`` lists the row data it may send beyond that.
Only ``sample_rows`` exists (src/egress/sample_rows.py): up to five rows of a
table, sent only while a platform admin approved the destination's model and
the workspace owner turned sample rows on, with every value checked by
Presidio at send time. A site that lists none sends no row value.
"""

from __future__ import annotations

from dataclasses import dataclass

__all__ = ["CALL_SITES", "CallSite", "REFUSED_PROMPTS", "call_site"]


@dataclass(frozen=True)
class CallSite:
    component: str
    where: str
    path: str  # "prompt" | "messages" | "tools"
    prompts: tuple[str, ...]
    sends: str
    data_classes: tuple[str, ...] = ()


_SITES = (
    CallSite(
        "gateway.completion",
        "src/gateway/main.py: POST /v1/completion (chat, HITL and planner prompts from api-gateway and "
        "llm-service; the orchestrator healer's failure classification)",
        "prompt",
        (
            "chat/assistant",
            "chat/help_response",
            "chat/intent_classification",
            "chat/slot_filling",
            "diagnose/healer_classify",
            "hitl/node_input",
            "planner/cdc_config",
            "planner/dag_planning",
            "planner/dynamic_planning",
        ),
        "The user's chat message, the chat's typed state, connection ids, names "
        "and types, connector names, config key names and table names. For the "
        "healer: the failed stage, statuses, connector types, yes/no for whether "
        "rows were read and written, and scrubbed error text.",
    ),
    CallSite(
        "gateway.diagnose",
        "src/gateway/main.py: POST /v1/diagnose/pipeline",
        "prompt",
        ("diagnose/pipeline_failure",),
        "A pipeline id and its diagnosis evidence: statuses, stage names, "
        "timestamps and scrubbed error text. Counts and sizes arrive as yes/no.",
    ),
    CallSite(
        "explorer.query_spec",
        "src/gateway/main.py: _generate_query_spec, _repair_query_spec",
        "prompt",
        ("explorer/query_spec", "explorer/query_spec_repair"),
        "The question, the SQL dialect, table and column names and types, "
        "foreign-key relationships, and the model's own earlier answer.",
    ),
    CallSite(
        "explorer.sql",
        "src/gateway/main.py: POST /api/v1/sql/generate (generation and its repairs: the Explorer's "
        "with the server's SQL model, Ask rsync.ai's generate_sql with the model the question is answered with)",
        "prompt",
        (
            "explorer/sql_generate",
            "explorer/sql_generate_offline",
            "explorer/sql_repair",
            "explorer/sql_repair_offline",
        ),
        "The question, the SQL dialect, table and column names and types, the "
        "join plan, and earlier SQL with every literal replaced by a placeholder.",
    ),
    CallSite(
        "explorer.table_link",
        "src/agents/explorer/api.py: POST /api/v1/explorer/nl/resolve-tables",
        "prompt",
        ("explorer/table_link",),
        "The question and the names, columns and types of up to 30 tables.",
    ),
    CallSite(
        "explorer.column_link",
        "src/agents/explorer/api.py: POST /api/v1/explorer/nl/resolve-columns",
        "prompt",
        ("explorer/column_link",),
        "The question and the chosen tables' column names and types.",
    ),
    CallSite(
        "explorer.next_steps",
        "src/agents/explorer/api.py: POST /api/v1/explorer/nl/next-steps",
        "prompt",
        ("explorer/next_steps",),
        "The question, the query with its literals replaced, and the result's column names.",
    ),
    CallSite(
        "explorer.rank_tables",
        "src/agents/explorer/rank_tables.py: POST /agents/rank-tables",
        "prompt",
        ("explorer/rank_tables",),
        "The user's intent and table and column names.",
    ),
    CallSite(
        "suggestions.transforms",
        "src/agents/suggestions/service.py: transform suggestions",
        "prompt",
        ("suggestions/transforms",),
        "Column names and types, and the source and destination connector types.",
    ),
    CallSite(
        "assistant.step",
        "src/gateway/assistant_routes.py: POST /v1/assistant/step (the in-app assistant)",
        "tools",
        ("assistant/step",),
        "The user's typed question, the page they are on (a kind and an id), "
        "the model's own earlier turns, and the results of agent gateway "
        "tools (the read tools, connector generation, the model tools, "
        "pipeline proposals, workflow drafts, lists and branch changes, and each "
        "workflow step's type, parents, rule, minutes and the pipeline or model it runs "
        "or the chart or dashboard it draws, by id and name, "
        "and plan_dashboard's chart sources, their column names and kinds, and each "
        "drafted chart's name, type, source and filters, and each dashboard's name, "
        "filters, tiles' places and charts' settings (column names, aggregates, sort, "
        "the measure's colour token; never a series colour, which names a value, or a "
        "text tile's text) and a proposed tile change's chart name, type and size): "
        "names, ids, descriptions, "
        "types, table and "
        "column names, schedules, statuses, timestamps and scrubbed error text, "
        "and from generate_sql the connection's id and the tables the SQL uses "
        "(the SQL itself goes to the person only), "
        "and from get_usage the workspace's plan name, its pipeline and NL→SQL "
        "query limits, how many pipelines it has, how many NL→SQL queries it used "
        "this month and how many members it has (counts only, never a member's e-mail). "
        "With sample rows on (the install allows them, an admin approved the model, "
        "the workspace owner turned them on): up to 5 rows of a table, at most 3 tables a question, "
        "values cut to 64 characters, with every column whose name, PII scan or "
        "a Presidio check at send time marks as personal or secret withheld.",
        ("sample_rows",),
    ),
    CallSite(
        "connector_resolver",
        "src/agents/connector_resolver/agent.py: resolve a typed connector name",
        "messages",
        (),
        "The connector name the user typed, and rsync.ai's own connector registry "
        "(names, categories, descriptions) returned by the resolver's tools.",
    ),
    CallSite(
        "logo_downloader",
        "src/utils/logo_downloader.py: infer a connector's company domain",
        "messages",
        (),
        "A connector's name and its registry description.",
    ),
    CallSite(
        "tool_generator.agent",
        "src/agents/tool_generator/agents/base.py: connector generation",
        "messages",
        (),
        "Public API documentation for the service a connector is being built for, "
        "the generated connector code, and its test results against a local mock "
        "with dummy credentials. Never a response from the real API.",
    ),
)

CALL_SITES: dict[str, CallSite] = {s.component: s for s in _SITES}

# Prompt files with no caller that the guard will not render: their variables
# are free text of unknown origin. Every prompt file is either on a component
# above or here (tests/test_egress_prompts.py), so a new one is classified
# before it can ship.
REFUSED_PROMPTS = frozenset(
    {
        "chat/decision_agent",
        "tool_generator/generate_connector",
        "tool_generator/validate_connector",
    }
)


def call_site(component: str) -> CallSite | None:
    return CALL_SITES.get(component)
