"""What each prompt sends, rendered from a fixture that carries customer data.

Every prompt the guard renders is rendered here from a fixture whose metadata
comes wrapped in the things a prompt must never hold: row counts and sizes,
an e-mail address, an enum label, a host name and a password, SQL literals,
config values, and error text quoting all of them. The rendered prompt that
reaches the (fake) provider must hold none of them, and must still hold the
table, column and type names the model needs -- a projection that drops
everything would pass the first check and break every feature.

Three controls keep that from passing on nothing:

- the canaries are absent from every prompt's own template text, so their
  absence in a render is the projection's doing;
- rendering the same fixture WITHOUT the guard leaks a canary, for every prompt
  that has a class which removes something;
- each prompt's render holds that prompt's metadata controls.

The classes themselves are src/egress/fields.py; the census is
src/egress/call_sites.py.
"""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import pytest

from _cut_collection import removed_by_the_cut
from src.egress import EgressRefused, guarded_call
from src.egress.call_sites import CALL_SITES, REFUSED_PROMPTS
from src.egress.fields import FIELD_CLASSES, WITHHELD_SQL, FieldRefused, mask_counts, mask_endpoints, project
from src.egress.render import prompt_fields, render_for_egress, template_variables
from src.egress.sql_placeholders import restore_sql_literals
from src.utils.masking import scrub_secrets_for_llm
from src.utils.prompt_registry import PromptRegistry

PROMPTS = Path(__file__).resolve().parents[1] / "prompts"
REGISTRY = PromptRegistry(prompts_dir=str(PROMPTS))

CENSUS = {p: site.component for site in CALL_SITES.values() for p in site.prompts}

COUNT = 7340033
EMAIL = "alice@example.com"
HOST = "db.internal.acme.io"
PASSWORD = "hunter2-canary"
CANARIES = (
    str(COUNT),
    "7,340,033",
    "7.3M",
    EMAIL,
    "banned",
    HOST,
    PASSWORD,
    "4412.5",
    "sample-row-canary",
    "comment-canary",
)
# The shape shared/scrubber_golden.json uses: the scrubber redacts it, and its
# segments are too short for gitleaks' jwt rule, so no allowlist entry is needed.
JWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdef"


def _table(name="orders", schema="public", schema_key="schema"):
    return {
        schema_key: schema,
        "name": name,
        "alias": name[0],
        "primary_key": ["id"],
        "row_count": COUNT,
        "size_bytes": COUNT,
        "comment": f"comment-canary {EMAIL}",
        "sample_rows": [{"email": EMAIL, "status": "banned", "note": "sample-row-canary"}],
        "columns": [
            {"name": "id", "type": "bigint", "is_primary_key": True, "is_nullable": False, "distinct_count": COUNT},
            {
                "name": "status",
                "type": "enum('active','banned')",
                "is_nullable": True,
                "comment": "comment-canary",
                "sample_values": ["banned"],
                "default": "banned",
                "min": "active",
                "max": "banned",
            },
            {"name": "email", "type": "varchar(255)", "sample_values": [EMAIL]},
            {"name": "amount", "type": "numeric(12,2)", "max": 4412.5},
        ],
    }


TABLES = [_table(), _table("customers")]
RELATIONSHIPS = ["orders.customer_id = customers.id"]
PREVIOUS_SQL = (
    f"SELECT id, status FROM orders WHERE email = '{EMAIL}' AND status = 'banned' "
    "AND amount > 4412.5 -- sample-row-canary"
)
ERROR_TEXT = f"scan read 7,340,033 rows (7.3M) from {HOST}:5432 for '{EMAIL}'"
CONNECTIONS = [
    {
        "id": "conn-1",
        "name": "Orders DB",
        "connector_type": "postgresql",
        "host": HOST,
        "password": PASSWORD,
        "config": {"host": HOST, "port": 5432, "password": PASSWORD},
        "last_test_error": ERROR_TEXT,
    }
]
EVIDENCE = {
    "pipeline": {"name": "orders-sync", "status": "failed", "sync_mode": "cdc", "source_type": "postgresql"},
    "progress": {
        "current_stage": "sink",
        "percent": 62,
        "blocking_reason": "dest_write_failed",
        "blocking_description": ERROR_TEXT,
    },
    "latest_execution": {"id": "exec-1", "status": "failed", "error_message": ERROR_TEXT},
    "events": [{"type": "error", "stage_id": "sink", "at": "2026-09-30T10:00:00Z", "message": ERROR_TEXT}],
    "flow": {
        "available": True,
        "events_considered": COUNT,
        "stalled_stage": "sink_write",
        # Not a free-text key: the evidence class's own text mask is what covers it.
        "summary": f"sink stalled after 7.3M rows at {HOST}:5432",
        "stalled_stage_logs": [{"service": "kafka-sink-worker", "body": ERROR_TEXT}],
    },
    "table_stats_summary": {"total_events": COUNT, "inserted_rows": 0},
}
QUESTION = "monthly order count by status"

FIXTURES: dict[str, dict] = {
    "chat/assistant": {"user_message": "how do I add a destination?"},
    "chat/help_response": {"user_message": "what is CDC?"},
    "chat/intent_classification": {"user_message": "move orders to bigquery"},
    "chat/slot_filling": {
        "user_message": "use the orders table",
        "state": "awaiting_tables",
        "pending_source": "Orders DB",
        "pending_destination": "warehouse",
    },
    "hitl/node_input": {
        "node_id": "sink-1",
        "node_kind": "destination",
        "hitl_prompt": f"Which table should receive the 7,340,033 rows from {HOST}:5432?",
        "node_context": {"host": HOST, "password": PASSWORD, "table": "orders"},
        "required_fields": ["table"],
        "user_response": "orders_copy",
    },
    "planner/cdc_config": {
        "source_type": "postgresql",
        "database": "app",
        "tables": ["public.orders"],
        "snapshot_mode": "initial",
        "topic_prefix": "rsync.cdc-p1",
    },
    "planner/dag_planning": {
        "available_connections": CONNECTIONS,
        "available_tools": ["postgres_read", "bigquery_write"],
        "phase": "generate",
        "user_request": "copy orders to the warehouse",
        "current_nodes": [{"id": "src-1"}],
        "current_edges": [],
        "validation_errors": [ERROR_TEXT],
    },
    "planner/dynamic_planning": {
        "available_tools": json.dumps(["postgres_read", "bigquery_write"]),
        "available_connections": json.dumps(CONNECTIONS),
        "user_request": "copy orders to the warehouse",
        "sync_mode": "cdc",
        "cdc_mode": "initial",
    },
    "diagnose/pipeline_failure": {"pipeline_id": "p-1", "evidence_json": EVIDENCE},
    # What backend-orchestrator's healer sends (pkg/diagnose/llm.go): its error
    # text arrives llmscrub-scrubbed, and the guard scrubs and masks it again.
    "diagnose/healer_classify": {
        "stage": "executor",
        "executor_status": "failed",
        "source_type": "postgresql",
        "destination_type": "bigquery",
        "source_rows_read": "yes",
        "rows_written": "no",
        "tables_landed_none": "yes",
        "error_message": ERROR_TEXT,
    },
    "explorer/query_spec": {"dialect": "postgres", "question": QUESTION, "tables": TABLES, "relationships": RELATIONSHIPS},
    "explorer/query_spec_repair": {
        "dialect": "postgres",
        "question": QUESTION,
        "tables": TABLES,
        "relationships": RELATIONSHIPS,
        "previous_spec": {"select": ["status"]},
        "errors": [ERROR_TEXT],
        "fix_hints": ["filter on status first"],
    },
    "explorer/sql_generate": {
        "dialect": "postgres",
        "question": QUESTION,
        "time_grain": "month",
        "time_column": "created_at",
        "tables": TABLES,
        "join_plan": [{"type": "LEFT", "right": "customers c", "condition": "o.customer_id = c.id"}],
        "column_mapping": {"select": ["o.status"], "where": [], "group_by": ["o.status"], "order_by": []},
        "previous_sql": PREVIOUS_SQL,
    },
    "explorer/sql_generate_offline": {
        "dialect": "postgres",
        "question": QUESTION,
        "time_grain": "month",
        "time_column": "created_at",
        "tables": TABLES,
        "relationships": RELATIONSHIPS,
    },
    "explorer/sql_repair": {
        "dialect": "postgres",
        "question": QUESTION,
        "time_grain": "month",
        "time_column": "created_at",
        "tables": TABLES,
        "issues": [{"type": "syntax", "message": ERROR_TEXT, "rows": COUNT}],
        "previous_sql": PREVIOUS_SQL,
    },
    "explorer/sql_repair_offline": {
        "dialect": "postgres",
        "question": QUESTION,
        "time_grain": "month",
        "time_column": "created_at",
        "tables": TABLES,
        "issues": [ERROR_TEXT],
        "previous_sql": PREVIOUS_SQL,
    },
    "explorer/table_link": {"question": QUESTION, "tables": TABLES},
    "explorer/column_link": {"question": QUESTION, "selected_tables": TABLES},
    "explorer/next_steps": {"question": QUESTION, "sql": PREVIOUS_SQL, "columns": ["status", "order_count"]},
    "explorer/rank_tables": {
        "tables": [_table(schema_key="schema_name"), _table("customers", schema_key="schema_name")],
        "intent": "revenue reporting",
        "use_case": "analytics",
        "max_tables": 5,
    },
    "assistant/step": {"page_kind": "pipeline", "page_id": "p-1", "data_mode": "sample_rows"},
    "suggestions/transforms": {
        "table": _table(),
        "available_columns": ["id", "status", "email", "amount"],
        "source_type": "postgresql",
        "destination_type": "bigquery",
        "operation": "mask",
    },
}

# Metadata each render must still carry.
CATALOG_CONTROLS = ("orders", "customers", "status", "email")
TYPED_CATALOG_CONTROLS = CATALOG_CONTROLS + ("bigint", "enum", "varchar(255)")
SQL_CONTROLS = ("SELECT", "orders", "{{L1:str}}")
CONTROLS: dict[str, tuple[str, ...]] = {
    "chat/assistant": ("add a destination",),
    "chat/help_response": ("what is CDC",),
    "chat/intent_classification": ("move orders to bigquery",),
    "chat/slot_filling": ("awaiting_tables", "Orders DB", "use the orders table"),
    "hitl/node_input": ("sink-1", "destination", "Which table", "[count]", "[host]", "'host'", "'table'", "orders_copy"),
    "planner/cdc_config": ("postgresql", "public.orders", "initial", "rsync.cdc-p1"),
    "planner/dag_planning": ("conn-1", "Orders DB", "postgresql", "bigquery_write", "copy orders", "[count]"),
    "planner/dynamic_planning": ("conn-1", "Orders DB", "postgresql", "bigquery_write", "cdc"),
    "diagnose/pipeline_failure": ("p-1", "orders-sync", "failed", "sink_write", "dest_write_failed", "2026-09-30T10:00:00Z", '"inserted_rows": false', '"total_events": true'),
    "diagnose/healer_classify": (
        "Stage: executor",
        "Executor status: failed",
        "Source connector type: postgresql",
        "Destination connector type: bigquery",
        "Source rows read: yes",
        "Rows written to the destination: no",
        "Some tables read rows but landed none: yes",
        "[count]",
        "[host]",
    ),
    "explorer/query_spec": TYPED_CATALOG_CONTROLS + ("orders.customer_id = customers.id", QUESTION),
    "explorer/query_spec_repair": TYPED_CATALOG_CONTROLS + ("filter on status first", "[count]"),
    "explorer/sql_generate": TYPED_CATALOG_CONTROLS + SQL_CONTROLS + ("LEFT JOIN customers c ON o.customer_id = c.id", "o.status", "created_at"),
    "explorer/sql_generate_offline": CATALOG_CONTROLS + ("orders.customer_id = customers.id",),
    "explorer/sql_repair": TYPED_CATALOG_CONTROLS + SQL_CONTROLS + ("[count]",),
    # The offline prompts print table(col, col): names, no types.
    "explorer/sql_repair_offline": CATALOG_CONTROLS + SQL_CONTROLS,
    "explorer/table_link": TYPED_CATALOG_CONTROLS + ("[PK]",),
    "explorer/column_link": TYPED_CATALOG_CONTROLS + ("[PK]", "[NULL]"),
    "explorer/next_steps": SQL_CONTROLS + ("order_count",),
    "explorer/rank_tables": ("public.orders", "public.customers", "status", "revenue reporting", "analytics", "top 5"),
    "assistant/step": (
        "looking at a pipeline with id p-1",
        "Answer only from what the tools return",
        "sample_rows reads up to 5 rows",
    ),
    "suggestions/transforms": ("status (enum)", "email (varchar(255))", "postgresql", "bigquery", "mask"),
}

# Prompts whose every variable is a class that refuses a bad value rather than
# removing part of it: nothing in them can be masked, so the raw-render leak
# control below does not apply. Spelled out so a new tag on one of them is seen.
REFUSE_ONLY = {"user_text", "identifier", "enum"}
NOTHING_TO_MASK = {
    "assistant/step",
    "chat/assistant",
    "chat/help_response",
    "chat/intent_classification",
    "chat/slot_filling",
    "planner/cdc_config",
}

# Prompts whose template prints only names from a catalog it is handed, so
# even an unguarded render of the fixture shows no canary. For them the leak
# control is on the variables instead: the fixture carries the canaries, the
# projection removes them, so a later template line that prints more still
# gets nothing.
NAMES_ONLY = {"explorer/rank_tables", "explorer/sql_generate_offline"}

# Prompts whose SQL dialect comes with the call instead of as a variable.
CALL_DIALECTS = {"explorer/next_steps": "postgres"}


def _text(messages) -> str:
    return "\n".join(str(m.get("content", "")) for m in messages)


class _Client:
    base_url = "https://api.openai.com/v1"

    def __init__(self):
        self.sent: list[dict] = []
        self.chat = SimpleNamespace(completions=SimpleNamespace(create=self._create))

    def _create(self, **kwargs):
        self.sent.append(kwargs)
        return SimpleNamespace(choices=[SimpleNamespace(message=SimpleNamespace(content="ok"))])


def _send(prompt: str, variables: dict, **kwargs) -> str:
    kwargs.setdefault("dialect", CALL_DIALECTS.get(prompt))
    if CALL_SITES[CENSUS[prompt]].path == "tools":
        # A tool-using component sends its conversation after the prompt.
        kwargs.setdefault("messages", [{"role": "user", "content": QUESTION}])
    client = _Client()
    guarded_call(
        client,
        component=CENSUS[prompt],
        model="m",
        prompt=prompt,
        variables=variables,
        registry=REGISTRY,
        **kwargs,
    )
    assert len(client.sent) == 1
    return _text(client.sent[0]["messages"])


# ---------------------------------------------------------------------------
# Every prompt is classified, and every variable in it is tagged.


def test_every_prompt_file_is_on_a_component_or_refused():
    files = {p.relative_to(PROMPTS).with_suffix("").as_posix() for p in PROMPTS.rglob("*.yaml")}
    assert len(files) >= 20, files
    assert not set(CENSUS) & REFUSED_PROMPTS
    unclassified = sorted(files - set(CENSUS) - REFUSED_PROMPTS)
    assert not unclassified, (
        f"prompt file(s) {unclassified} are on no census component and not refused. "
        "Add each to a component in src/egress/call_sites.py with llm_fields tags, "
        "or to REFUSED_PROMPTS."
    )
    # The public cut strips prompts/tool_generator; its two refused prompts stay listed.
    stale = sorted(
        p for p in (set(CENSUS) | REFUSED_PROMPTS) - files if not removed_by_the_cut(f"prompts/{p}.yaml")
    )
    assert not stale, f"the census names prompt(s) with no file: {stale}"


@pytest.mark.parametrize("prompt", sorted(CENSUS))
def test_every_variable_a_prompt_reads_is_tagged_with_a_known_class(prompt):
    used = template_variables(REGISTRY, prompt)
    fields = prompt_fields(REGISTRY, prompt)
    assert used, f"{prompt} reads no variables; the walk stopped matching"
    assert used == set(fields), (
        f"{prompt}: untagged {sorted(used - set(fields))}, tagged but unused {sorted(set(fields) - used)}"
    )
    assert set(fields.values()) <= set(FIELD_CLASSES)


def test_every_census_prompt_has_a_fixture_and_controls():
    assert set(FIXTURES) == set(CENSUS)
    assert set(CONTROLS) == set(CENSUS)
    for prompt, fixture in FIXTURES.items():
        assert set(fixture) == set(prompt_fields(REGISTRY, prompt)), prompt


# ---------------------------------------------------------------------------
# The controls: the canaries are not in the templates, and without the guard
# the fixtures leak them.


@pytest.mark.parametrize("prompt", sorted(CENSUS))
def test_no_canary_is_in_a_template_itself(prompt):
    source = (PROMPTS / f"{prompt}.yaml").read_text()
    assert not [c for c in CANARIES if c in source]


def test_the_prompts_with_nothing_to_mask_are_the_expected_ones():
    computed = {p for p in CENSUS if set(prompt_fields(REGISTRY, p).values()) <= REFUSE_ONLY}
    assert computed == NOTHING_TO_MASK


@pytest.mark.parametrize("prompt", sorted(set(CENSUS) - NOTHING_TO_MASK - NAMES_ONLY))
def test_without_the_guard_the_fixture_leaks(prompt):
    raw = _text(REGISTRY.render_messages(prompt, FIXTURES[prompt]))
    assert [c for c in CANARIES if c in raw], (
        f"{prompt}'s fixture renders no canary even unguarded, so the test below proves nothing for it"
    )


@pytest.mark.parametrize("prompt", sorted(NAMES_ONLY))
def test_a_names_only_prompt_gets_the_data_removed_from_its_variables(prompt):
    raw = _text(REGISTRY.render_messages(prompt, FIXTURES[prompt]))
    assert not [c for c in CANARIES if c in raw], (
        f"{prompt} now prints fixture data unguarded; move it to the render leak control above"
    )
    fields = prompt_fields(REGISTRY, prompt)
    before = json.dumps(FIXTURES[prompt], default=str)
    after = json.dumps(
        {name: project(fields[name], value, name=name) for name, value in FIXTURES[prompt].items()}, default=str
    )
    assert [c for c in CANARIES if c in before], f"{prompt}'s fixture carries no canary"
    assert not [c for c in CANARIES if c in after], f"{prompt}'s projected variables still carry data"


# ---------------------------------------------------------------------------
# Through the guard: no canary, all the metadata.


@pytest.mark.parametrize("prompt", sorted(CENSUS))
def test_the_sent_prompt_holds_no_customer_data(prompt):
    sent = _send(prompt, FIXTURES[prompt])
    leaked = [c for c in CANARIES if c in sent]
    assert not leaked, f"{prompt} sent {leaked} to the model"


@pytest.mark.parametrize("prompt", sorted(CENSUS))
def test_the_sent_prompt_still_holds_the_metadata(prompt):
    sent = _send(prompt, FIXTURES[prompt])
    missing = [c for c in CONTROLS[prompt] if c not in sent]
    assert not missing, f"{prompt} lost {missing}"


@pytest.mark.parametrize("prompt", sorted(CENSUS))
def test_the_projection_alone_removes_the_data(prompt):
    """The secret scrub is the backstop for what a user pastes, not what makes a
    render clean -- and it must not mangle one (placeholders, type names)."""
    messages, _ = render_for_egress(REGISTRY, prompt, FIXTURES[prompt], dialect=CALL_DIALECTS.get(prompt))
    for message in messages:
        content = str(message.get("content", ""))
        assert scrub_secrets_for_llm(content) == content, prompt


def test_a_token_the_user_pastes_is_scrubbed_and_the_question_is_not():
    sent = _send("chat/assistant", {"user_message": f"my token is {JWT} and password=hunter2 -- why 401?"})
    assert JWT not in sent
    assert "hunter2" not in sent
    assert "why 401?" in sent


def test_sql_literals_come_back_after_the_call():
    messages, literals = render_for_egress(
        REGISTRY, "explorer/next_steps", FIXTURES["explorer/next_steps"], dialect="postgres"
    )
    sent = _text(messages)
    assert "{{L1:str}}" in sent and EMAIL not in sent
    assert EMAIL in restore_sql_literals("SELECT * FROM orders WHERE email = {{L1:str}}", literals, "postgres")


@pytest.mark.parametrize("dialect", [None, "", "generic"])
def test_sql_with_no_known_dialect_is_withheld_not_guessed(dialect):
    """Quoting differs by dialect (MySQL's "x" is a string, Postgres's is a
    name), so SQL whose dialect is missing or unknown is not sent at all. The
    explorer sends "generic" for a connector it has no editor dialect for."""
    sent = _send("explorer/next_steps", FIXTURES["explorer/next_steps"], dialect=dialect)
    assert WITHHELD_SQL in sent
    assert not [c for c in CANARIES if c in sent]
    assert "{{L1:str}}" not in sent
    # Control: the same call with a dialect sends the query.
    assert WITHHELD_SQL not in _send("explorer/next_steps", FIXTURES["explorer/next_steps"])


# ---------------------------------------------------------------------------
# The classes, value by value.


def test_the_catalog_keeps_names_types_and_keys_and_nothing_else():
    assert project("catalog", _table(), name="t") == {
        "schema": "public",
        "name": "orders",
        "alias": "o",
        "primary_key": ["id"],
        "columns": [
            {"name": "id", "type": "bigint", "is_primary_key": True, "is_nullable": False},
            {"name": "status", "type": "enum", "is_nullable": True},
            {"name": "email", "type": "varchar(255)"},
            {"name": "amount", "type": "numeric(12,2)"},
        ],
    }


@pytest.mark.parametrize(
    "declared, kept",
    [
        ("enum('active','banned')", "enum"),
        ("Enum8('active' = 1, 'banned' = 2)", "Enum8"),
        ('set("a","banned")', "set"),
        ("varchar(255)", "varchar(255)"),
        ("timestamp with time zone", "timestamp with time zone"),
    ],
)
def test_a_declared_type_keeps_its_name_but_not_its_labels(declared, kept):
    col = project("catalog", {"name": "t", "columns": [{"name": "c", "type": declared}]}, name="t")
    assert col["columns"][0]["type"] == kept


def test_evidence_counts_arrive_as_yes_or_no():
    out = json.loads(project("evidence", EVIDENCE, name="e"))
    assert out["table_stats_summary"] == {"total_events": True, "inserted_rows": False}
    assert out["flow"]["events_considered"] is True
    assert out["progress"]["percent"] == 62


def test_config_keeps_its_key_names_only():
    assert project("config_keys", {"password": PASSWORD, "host": HOST}, name="c") == ["host", "password"]


def test_connections_keep_id_name_and_type_only():
    assert project("connections", CONNECTIONS, name="c") == [
        {"id": "conn-1", "name": "Orders DB", "connector_type": "postgresql"}
    ]
    assert json.loads(project("connections", json.dumps(CONNECTIONS), name="c")) == [
        {"id": "conn-1", "name": "Orders DB", "connector_type": "postgresql"}
    ]


@pytest.mark.parametrize(
    "text, masked",
    [
        ("flushed 7,340,033 rows", "flushed [count] rows"),
        ("7.3M rows last week", "[count] rows last week"),
        ("about 7M", "about [count]"),
        ("12k events", "[count] events"),
        ("4 GiB written", "[count] written"),
        ("2.1 billion", "[count]"),
        ("45000 rows", "[count] rows"),
        ("1,000.5", "[count]"),
        # Zero is what a diagnosis needs to know about volume.
        ("0 rows inserted", "0 rows inserted"),
        # Not counts.
        ("SQLSTATE 23505", "SQLSTATE 23505"),
        ("line 12 column 5", "line 12 column 5"),
        ("timeout after 5m", "timeout after 5m"),
        ("HTTP 503", "HTTP 503"),
        ("2026-07-17T10:00:00", "2026-07-17T10:00:00"),
        ("v1.0.0", "v1.0.0"),
        ("orders_2024", "orders_2024"),
        ("attempt 3 of 5", "attempt 3 of 5"),
    ],
)
def test_the_count_mask(text, masked):
    assert mask_counts(text) == masked


@pytest.mark.parametrize(
    "text, masked",
    [
        (f"dial tcp {HOST}:5432: connect: connection refused", "dial tcp [host]: connect: connection refused"),
        (f"postgres://[redacted]@{HOST}:5432/app", "postgres://[host]/app"),
        ("at diagnose.go:95", "at diagnose.go:95"),
        ("kafka:9092 is down", "kafka:9092 is down"),
        ("column public.orders.id", "column public.orders.id"),
        # A host name with no port, where error text puts one.
        (f"lookup {HOST}: no such host", "lookup [host]: no such host"),
        (f'could not translate host name "{HOST}" to address', 'could not translate host name "[host]" to address'),
        (f'connection to server at "{HOST}" failed', 'connection to server at "[host]" failed'),
        # A compose service has no dot, and prose after lookup is not a host.
        ("dial tcp: lookup postgres: no such host", "dial tcp: lookup postgres: no such host"),
        ("lookup failed: schema.table missing", "lookup failed: schema.table missing"),
    ],
)
def test_the_endpoint_mask(text, masked):
    assert mask_endpoints(text) == masked


@pytest.mark.parametrize("key", ["error", "note"])
def test_a_host_name_in_evidence_does_not_reach_the_model(key):
    # KI-LLM-TEXT-MASK-KEEPS-BARE-HOSTNAMES: a DNS failure in a diagnosis's
    # evidence sent the customer's host name. "error" is free text (the error
    # scrubber takes it); "note" is not, and only the text mask sees it.
    out = project("evidence", {key: f"dial tcp: lookup {HOST}: no such host"}, name="e")
    assert HOST not in out
    assert "lookup [host" in out


@pytest.mark.parametrize(
    "field_class, value",
    [
        ("identifier", "orders\nDROP"),
        ("identifier", "x" * 257),
        ("identifier", 4412.5),
        ("enum", "x" * 65),
        ("enum", COUNT),
        ("catalog", "orders"),
        ("config_keys", ["host"]),
        ("connections", {"id": "c"}),
        ("evidence", ["not", "an", "object"]),
        ("user_text", {"a": 1}),
        ("sql", 42),
        ("no_such_class", "x"),
    ],
)
def test_a_value_that_does_not_fit_its_class_is_refused(field_class, value):
    with pytest.raises(FieldRefused):
        project(field_class, value, name="v")


def test_a_refused_value_refuses_the_call_before_anything_is_sent():
    client = _Client()
    bad = dict(FIXTURES["chat/slot_filling"], pending_source=f"orders\n{EMAIL}")
    with pytest.raises(EgressRefused):
        guarded_call(client, component="gateway.completion", model="m", prompt="chat/slot_filling", variables=bad, registry=REGISTRY)
    assert client.sent == []


def _one_prompt_registry(tmp_path, llm_fields: dict) -> PromptRegistry:
    (tmp_path / "t").mkdir()
    (tmp_path / "t" / "p.yaml").write_text(
        "user: 'Q: {{ question }} {{ extra }}'\n" + ("llm_fields:\n" + "".join(f"  {k}: {v}\n" for k, v in llm_fields.items()) if llm_fields else "")
    )
    return PromptRegistry(prompts_dir=str(tmp_path))


def test_an_untagged_variable_refuses_the_render(tmp_path):
    with pytest.raises(FieldRefused, match="extra"):
        render_for_egress(_one_prompt_registry(tmp_path, {"question": "user_text"}), "t/p", {"question": "q", "extra": "x"})


def test_a_variable_the_template_never_reads_is_dropped(tmp_path):
    # Checked at the renderer's input: a template that loops over its whole
    # context would otherwise print a key no llm_fields tag ever saw.
    (tmp_path / "t").mkdir()
    (tmp_path / "t" / "p.yaml").write_text("user: 'Q: {{ question }}'\nllm_fields:\n  question: user_text\n")
    registry = PromptRegistry(prompts_dir=str(tmp_path))
    seen = {}
    render = registry.render_messages
    registry.render_messages = lambda name, variables: seen.update(variables) or render(name, variables)

    render_for_egress(registry, "t/p", {"question": "q", "extra": "hunter2-canary"})
    assert seen.get("question") == "q"  # control: the render happened
    assert "extra" not in seen


def test_slot_filling_never_sends_the_previous_reply():
    # The gateway still passes last_response (the mock slot-filler reads it);
    # the template does not, so the guard drops it before the provider.
    variables = dict(FIXTURES["chat/slot_filling"], last_response="3 rows from db.internal.acme.io: hunter2-canary")
    assert "last_response" not in template_variables(REGISTRY, "chat/slot_filling")
    messages, _ = render_for_egress(REGISTRY, "chat/slot_filling", variables)
    assert "hunter2-canary" not in json.dumps(messages)


def test_an_unknown_class_refuses_the_render(tmp_path):
    reg = _one_prompt_registry(tmp_path, {"question": "user_text", "extra": "row_values"})
    with pytest.raises(FieldRefused, match="row_values"):
        render_for_egress(reg, "t/p", {"question": "q", "extra": "x"})
