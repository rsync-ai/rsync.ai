from __future__ import annotations

import json
import logging
import os
import re

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, ConfigDict, ValidationError

from src.utils.prompt_registry import PromptRegistry
from src.utils.openai_client import (
    env_bool as _env_bool,
    resolve_explorer_provider as _resolve_explorer_provider,
    explorer_default_model as _explorer_default_model,
    # Imported, not reimplemented. The private copies this router used to carry
    # accepted LLM_PROVIDER=groq with no key check and had no groq branch to
    # build, so an explicit groq silently became an OpenAI client.
    make_async_client as _create_async_client,
    client_egress_host as _client_egress_host,
    explorer_llm_configured as _explorer_llm_configured,
    with_reasoning_headroom,
)
from src.utils.llm_gate import require_explorer_llm
from src.egress import EgressRefused, guarded_acall
from src.utils.rate_limit_waits import RateLimitWaits, wait_out

logger = logging.getLogger("llm-explorer")

router = APIRouter(tags=["Explorer"])


_REFUSED_DETAIL = "The LLM request was refused: it did not fit what may be sent to a model."


def _extract_json_text(raw: str) -> str:
    """Best-effort extraction of a JSON object from model output."""
    if raw is None:
        return "{}"
    text = str(raw).strip()
    # Prefer fenced JSON blocks
    m = re.search(r"```(?:json)?\s*([\s\S]*?)\s*```", text, flags=re.IGNORECASE)
    if m:
        text = m.group(1).strip()
    # Fall back to first {...} span
    start = text.find("{")
    end = text.rfind("}")
    if start >= 0 and end > start:
        return text[start : end + 1].strip()
    return text


# The model's reply is the one input to these endpoints that neither the user nor
# this code controls: it can be empty (seen after a provider 503 retry), a refusal
# in prose, cut off mid-object, or valid JSON of the wrong shape. None of that is a
# server fault and none of it stops the user from answering by hand, so each
# endpoint degrades instead of answering 500: tables and columns to "nothing
# matched, pick it yourself", next steps to the rule-based list.


def _parse_model_reply(raw: str | None, step: str) -> dict | None:
    """The model's reply as a JSON object, or None when it is not one."""
    text = (raw or "").strip()
    if not text:
        logger.warning("%s: the model's reply was empty", step)
        return None
    try:
        parsed = json.loads(_extract_json_text(text))
    except ValueError as e:
        # Length and error only: the reply itself stays out of the log.
        logger.warning("%s: the model's reply is not JSON (%s, %d chars)", step, type(e).__name__, len(text))
        return None
    if not isinstance(parsed, dict):
        logger.warning("%s: the model's reply is JSON but not an object (%s)", step, type(parsed).__name__)
        return None
    return parsed


def _reply_list(result: dict, *keys: str) -> list:
    """The first of `keys` holding a list, else []."""
    for key in keys:
        value = result.get(key)
        if isinstance(value, list):
            return value
    return []


def _str_list(value) -> list[str]:
    return [v for v in value if isinstance(v, str)] if isinstance(value, list) else []


def _opt_str(value) -> str | None:
    return value if isinstance(value, str) and value.strip() else None


def _confidence(value, default: float) -> float:
    """A 0..1 score from the model, or `default` when it did not send a number."""
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        return default
    try:
        score = float(value)
    except ValueError:  # "high"
        return default
    if score != score:  # NaN
        return default
    return min(max(score, 0.0), 1.0)


# ------------------------------------------------------------------------------
# Runtime config (kept local to Explorer)
# ------------------------------------------------------------------------------

# Default should be FALSE; this is mainly for tests.
USE_MOCK = _env_bool("USE_MOCK_LLM", False)

# Operational guardrail (mirrors gateway/main.py).
if USE_MOCK:
    env = (
        (os.getenv("ENVIRONMENT") or os.getenv("APP_ENV") or os.getenv("ENV") or os.getenv("NODE_ENV") or "")
        .strip()
        .lower()
    )
    allowed = {"development", "dev", "test", "testing", "local", "docker"}
    if env not in allowed:
        env_display = env or "<unset>"
        raise RuntimeError(
            f"USE_MOCK_LLM=true is not allowed when ENVIRONMENT={env_display!r}. "
            "Set USE_MOCK_LLM=false (recommended) or run with ENVIRONMENT=development/test."
        )

# Explorer offline mode.
# Default is now False — Explorer uses the same LLM_PROVIDER as the rest of the stack
# (Azure OpenAI in production). Set EXPLORER_OFFLINE_ONLY=true to force Ollama for air-gapped
# or on-prem deployments where schema metadata must not leave the system.
EXPLORER_OFFLINE_ONLY = _env_bool("EXPLORER_OFFLINE_ONLY", False)
# Provider + model defaults come from src.utils.openai_client, shared with the
# gateway (src/gateway/main.py). This router previously resolved both itself and
# ignored LLM_MODEL, so on Azure its three endpoints sent the literal
# "gpt-4o-mini" as a *deployment* name — 404 DeploymentNotFound — while the rest
# of Explorer, resolving in main.py, used the configured deployment and worked.
EXPLORER_LLM_PROVIDER = _resolve_explorer_provider()

explorer_client = None if USE_MOCK else _create_async_client(EXPLORER_LLM_PROVIDER)

_default_explorer_model = _explorer_default_model(EXPLORER_LLM_PROVIDER)
EXPLORER_TABLE_LINK_MODEL = (os.getenv("EXPLORER_TABLE_LINK_MODEL") or _default_explorer_model).strip()
EXPLORER_COLUMN_LINK_MODEL = (os.getenv("EXPLORER_COLUMN_LINK_MODEL") or _default_explorer_model).strip()
EXPLORER_NEXT_STEPS_MODEL = (os.getenv("EXPLORER_NEXT_STEPS_MODEL") or _default_explorer_model).strip()

# `egress=` is read off the constructed client, so this line reports where
# prompts go rather than restating what LLM_PROVIDER said. See
# client_egress_host in src/utils/openai_client.py for why that matters.
logger.info(
    "explorer router llm: provider=%s offline_only=%s table_link_model=%s "
    "column_link_model=%s next_steps_model=%s egress=%s",
    EXPLORER_LLM_PROVIDER,
    EXPLORER_OFFLINE_ONLY,
    EXPLORER_TABLE_LINK_MODEL,
    EXPLORER_COLUMN_LINK_MODEL,
    EXPLORER_NEXT_STEPS_MODEL,
    _client_egress_host(explorer_client),
)

registry = PromptRegistry(prompts_dir="prompts")


# ------------------------------------------------------------------------------
# Models
# ------------------------------------------------------------------------------


class TableCandidate(BaseModel):
    """A candidate table with confidence score"""

    table: str
    # Schema must come from the user's source connection config, not a
    # hardcoded "public" default — many production deployments use other
    # schemas (analytics, tenant_*, app_*) and silent "public" fallback
    # makes queries against those schemas return empty.
    schema_name: str | None = None
    confidence: float
    reason: str


class TableLinkRequest(BaseModel):
    """Request to link question to tables"""

    question: str
    tables: list[dict]  # List of table metadata
    previous_context: str | None = None
    conversation_id: str | None = None


class TableLinkResponse(BaseModel):
    """Response with table candidates"""

    candidates: list[TableCandidate]
    confidence_overall: float
    needs_hitl: bool
    hitl_reason: str | None = None
    suggested_join_keys: list[str] | None = None


class ColumnMapping(BaseModel):
    """Column mapping for query parts"""

    select_cols: list[str] = []
    where_cols: list[str] = []
    group_by_cols: list[str] = []
    order_by_cols: list[str] = []


class JoinPlan(BaseModel):
    """Join plan between tables"""

    join_type: str = "INNER"
    left_table: str
    right_table: str
    condition: str


class ColumnLinkRequest(BaseModel):
    """Request to link question to columns"""

    question: str
    selected_tables: list[dict]  # Tables selected from table_link
    previous_context: str | None = None
    conversation_id: str | None = None


class ColumnLinkResponse(BaseModel):
    """Response with column mapping and join plan"""

    columns: ColumnMapping
    join_plan: list[JoinPlan]
    confidence: float
    needs_hitl: bool
    hitl_reason: str | None = None
    ambiguous_columns: list[str] | None = None


class NextStepSuggestion(BaseModel):
    """A suggested next action"""

    action_type: str  # download_csv, slack, email
    title: str
    description: str
    confidence: float = 0.8
    required_inputs: list[str] = []
    cta: str = "Execute"


class ResultProfile(BaseModel):
    """Metadata-only profile of a query result.

    Privacy contract (CLAUDE.md "LLM data privacy"): query RESULT ROWS never
    reach the LLM — only this profile does. extra="forbid" is the enforcement:
    a caller that attaches sample rows / result data fails validation (422)
    instead of silently leaking. Mirrors the Go producer struct
    (api-gateway/internal/cache/explorer_cache.go ResultProfile).
    """

    model_config = ConfigDict(extra="forbid")

    row_count: int = 0
    columns: list[str] = []
    min_timestamp: str | None = None
    max_timestamp: str | None = None


class NextStepsRequest(BaseModel):
    """Request for next step suggestions"""

    question: str
    sql: str
    # The SQL's dialect. The guard finds the query's literals with it and sends
    # placeholders in their place; with none, or one it does not know, it
    # withholds the query rather than guess where the literals are.
    dialect: str | None = None
    result_profile: ResultProfile
    available_actions: list[str] = ["download_csv", "slack", "email"]


class NextStepsResponse(BaseModel):
    """Response with suggested next actions"""

    suggestions: list[NextStepSuggestion]


# ------------------------------------------------------------------------------
# Endpoints
# ------------------------------------------------------------------------------


@router.post("/api/v1/explorer/nl/resolve-tables", response_model=TableLinkResponse)
async def resolve_tables(request: TableLinkRequest):
    """Resolve natural language question to relevant database tables."""
    try:
        logger.info("📊 Table link request: %s...", (request.question or "")[:100])

        # Explorer is offline-only.
        if EXPLORER_OFFLINE_ONLY and USE_MOCK:
            return _mock_table_link(request)
        require_explorer_llm()
        if explorer_client is None:
            raise HTTPException(status_code=503, detail="Explorer LLM client unavailable (offline-only)")

        prompt_name = "explorer/table_link"
        variables = {
            "question": request.question,
            "tables": (request.tables or [])[:30],
        }
        cfg = registry.get_config(prompt_name)

        # A 429 is tried again, at most 15 seconds asleep: api-gateway gives the
        # table match 30 (handlers/explorer.go ResolveExplorerTables).
        response = await wait_out(
            lambda: guarded_acall(
                explorer_client,
                component="explorer.table_link",
                model=EXPLORER_TABLE_LINK_MODEL,
                prompt=prompt_name,
                variables=variables,
                temperature=cfg["parameters"].get("temperature", 0.1),
                max_tokens=with_reasoning_headroom(cfg["parameters"].get("max_tokens", 1024)),
            ),
            RateLimitWaits(most=15.0),
            "explorer.table_link",
        )

        result = _parse_model_reply(response.text, "table link")
        if result is None:
            return _pick_tables_yourself("The table match could not be read")

        # Accept both 'schema' and 'schema_name' from models. An entry of the wrong
        # shape is dropped on its own; the rest of the reply still counts.
        parsed_candidates = []
        for c in _reply_list(result, "candidates"):
            if not isinstance(c, dict):
                continue
            if "schema_name" not in c and "schema" in c:
                c = {**c, "schema_name": c.get("schema")}
            try:
                parsed_candidates.append(TableCandidate(**c))
            except ValidationError as e:
                logger.warning("table link: dropped a candidate of the wrong shape (%d errors)", e.error_count())

        if not parsed_candidates:
            return _pick_tables_yourself(_opt_str(result.get("hitl_reason")) or "No table matched the question")

        return TableLinkResponse(
            candidates=parsed_candidates,
            confidence_overall=_confidence(result.get("confidence_overall"), 0.5),
            needs_hitl=bool(result.get("needs_hitl", False)),
            hitl_reason=_opt_str(result.get("hitl_reason")),
            suggested_join_keys=_str_list(result.get("suggested_join_keys")) or None,
        )
    except HTTPException:
        raise
    except EgressRefused as e:
        logger.warning("Table link refused by the LLM egress guard: %s", e.reason)
        raise HTTPException(status_code=503, detail=_REFUSED_DETAIL)
    except Exception as e:
        # The exception text can quote a provider reply; the log keeps it.
        logger.exception("Table link failed: %s", e)
        raise HTTPException(status_code=500, detail="Table link failed. See the llm-service log.")


def _pick_tables_yourself(reason: str) -> TableLinkResponse:
    """No usable match from the model: the user picks the tables."""
    return TableLinkResponse(
        candidates=[],
        confidence_overall=0.0,
        needs_hitl=True,
        hitl_reason=f"{reason} — pick the tables to query.",
        suggested_join_keys=None,
    )


def _mock_table_link(request: TableLinkRequest) -> TableLinkResponse:
    """Mock table linking for testing (intentionally minimal)."""
    fallback = []
    for t in (request.tables or [])[:5]:
        fallback.append(
            TableCandidate(
                table=t.get("name", ""),
                # Mock fallback: use whatever the caller provided (typically
                # the source connection's schema). If absent, leave None and
                # let the caller resolve it from connection config.
                schema_name=t.get("schema") or t.get("schema_name"),
                confidence=0.35,
                reason="Mock mode: please confirm the right table(s)",
            )
        )
    return TableLinkResponse(
        candidates=fallback,
        confidence_overall=0.35 if fallback else 0.0,
        needs_hitl=True,
        hitl_reason="Mock mode enabled — need human confirmation",
        suggested_join_keys=None,
    )


@router.post("/api/v1/explorer/nl/resolve-columns", response_model=ColumnLinkResponse)
async def resolve_columns(request: ColumnLinkRequest):
    """Resolve natural language question to specific columns and join plan."""
    try:
        logger.info("📊 Column link request: %s...", (request.question or "")[:100])

        # Explorer is offline-only.
        if EXPLORER_OFFLINE_ONLY and USE_MOCK:
            return _mock_column_link(request)
        require_explorer_llm()
        if explorer_client is None:
            raise HTTPException(status_code=503, detail="Explorer LLM client unavailable (offline-only)")

        prompt_name = "explorer/column_link"
        variables = {
            "question": request.question,
            "selected_tables": request.selected_tables,
        }
        cfg = registry.get_config(prompt_name)

        response = await guarded_acall(
            explorer_client,
            component="explorer.column_link",
            model=EXPLORER_COLUMN_LINK_MODEL,
            prompt=prompt_name,
            variables=variables,
            temperature=cfg["parameters"].get("temperature", 0.1),
            max_tokens=with_reasoning_headroom(cfg["parameters"].get("max_tokens", 1024)),
        )

        result = _parse_model_reply(response.text, "column link")
        if result is None:
            return _pick_columns_yourself("The column match could not be read")

        cols = result.get("columns")
        if not isinstance(cols, dict):
            cols = {}
        mapping = ColumnMapping(
            select_cols=_str_list(cols.get("select_cols", cols.get("select"))),
            where_cols=_str_list(cols.get("where_cols", cols.get("where"))),
            group_by_cols=_str_list(cols.get("group_by_cols", cols.get("group_by"))),
            order_by_cols=_str_list(cols.get("order_by_cols", cols.get("order_by"))),
        )
        join_plan = []
        for j in _reply_list(result, "join_plan"):
            if not isinstance(j, dict):
                continue
            try:
                join_plan.append(
                    JoinPlan(
                        join_type=j.get("join_type", j.get("type", "INNER")),
                        left_table=j.get("left_table", j.get("left", "")),
                        right_table=j.get("right_table", j.get("right", "")),
                        condition=j.get("condition", ""),
                    )
                )
            except ValidationError as e:
                logger.warning("column link: dropped a join of the wrong shape (%d errors)", e.error_count())

        if not join_plan and not any(
            (mapping.select_cols, mapping.where_cols, mapping.group_by_cols, mapping.order_by_cols)
        ):
            return _pick_columns_yourself(_opt_str(result.get("hitl_reason")) or "No column matched the question")

        return ColumnLinkResponse(
            columns=mapping,
            join_plan=join_plan,
            confidence=_confidence(result.get("confidence"), 0.5),
            needs_hitl=bool(result.get("needs_hitl", False)),
            hitl_reason=_opt_str(result.get("hitl_reason")),
            ambiguous_columns=_str_list(result.get("ambiguous_columns")) or None,
        )
    except HTTPException:
        raise
    except EgressRefused as e:
        logger.warning("Column link refused by the LLM egress guard: %s", e.reason)
        raise HTTPException(status_code=503, detail=_REFUSED_DETAIL)
    except Exception as e:
        # The exception text can quote a provider reply; the log keeps it.
        logger.exception("Column link failed: %s", e)
        raise HTTPException(status_code=500, detail="Column link failed. See the llm-service log.")


def _pick_columns_yourself(reason: str) -> ColumnLinkResponse:
    """No usable match from the model: the user picks the columns."""
    return ColumnLinkResponse(
        columns=ColumnMapping(),
        join_plan=[],
        confidence=0.0,
        needs_hitl=True,
        hitl_reason=f"{reason} — pick the columns to use.",
        ambiguous_columns=None,
    )


def _mock_column_link(request: ColumnLinkRequest) -> ColumnLinkResponse:
    """Mock column linking for testing."""
    select_cols = []
    for t in request.selected_tables:
        table_name = t.get("name", "")
        for col in t.get("columns", [])[:5]:
            select_cols.append(f"{table_name}.{col.get('name', '')}")
    return ColumnLinkResponse(
        columns=ColumnMapping(
            select_cols=select_cols[:10],
            where_cols=[],
            group_by_cols=[],
            order_by_cols=[],
        ),
        join_plan=[],
        confidence=0.7,
        needs_hitl=len(request.selected_tables) > 1,
        hitl_reason="Multiple tables - please confirm join" if len(request.selected_tables) > 1 else None,
        ambiguous_columns=None,
    )


@router.post("/api/v1/explorer/nl/next-steps", response_model=NextStepsResponse)
async def get_next_steps(request: NextStepsRequest):
    """Suggest next actions based on query results."""
    try:
        logger.info("💡 Next steps request for: %s...", (request.question or "")[:50])

        # Explorer is offline-only.
        if EXPLORER_OFFLINE_ONLY and USE_MOCK:
            return _mock_next_steps(request)
        if not USE_MOCK and not _explorer_llm_configured():
            # Next steps are offered after every query, including raw SQL, so
            # without an LLM they fall back to the rule-based list rather than
            # turning a successful query into an error.
            return _mock_next_steps(request)
        if explorer_client is None:
            raise HTTPException(status_code=503, detail="Explorer LLM client unavailable (offline-only)")

        prompt_name = "explorer/next_steps"
        variables = {
            "question": request.question,
            "sql": request.sql,
            # Column names only. The row count and time range describe the
            # customer's data.
            "columns": list(request.result_profile.columns or []),
        }
        cfg = registry.get_config(prompt_name)

        response = await guarded_acall(
            explorer_client,
            component="explorer.next_steps",
            model=EXPLORER_NEXT_STEPS_MODEL,
            prompt=prompt_name,
            variables=variables,
            dialect=request.dialect,
            temperature=cfg["parameters"].get("temperature", 0.3),
            max_tokens=with_reasoning_headroom(cfg["parameters"].get("max_tokens", 512)),
        )

        result = _parse_model_reply(response.text, "next steps")
        if result is None:
            return _mock_next_steps(request)

        offered = _reply_list(result, "suggestions")
        suggestions = []
        for s in offered:
            if not isinstance(s, dict):
                continue
            # Accept both 'type' (prompt spec) and 'action_type' (API model).
            if "action_type" not in s and "type" in s:
                s = {**s, "action_type": s.get("type")}
            try:
                suggestions.append(NextStepSuggestion(**s))
            except ValidationError as e:
                logger.warning("next steps: dropped a suggestion of the wrong shape (%d errors)", e.error_count())

        # The model offered suggestions and none could be used: serve the
        # rule-based list rather than an empty one. An empty list it chose stays.
        if offered and not suggestions:
            return _mock_next_steps(request)
        return NextStepsResponse(suggestions=suggestions)
    except HTTPException:
        raise
    except EgressRefused as e:
        logger.warning("Next steps refused by the LLM egress guard: %s", e.reason)
        raise HTTPException(status_code=503, detail=_REFUSED_DETAIL)
    except Exception as e:
        # The exception text can quote a provider reply; the log keeps it.
        logger.exception("Next steps failed: %s", e)
        raise HTTPException(status_code=500, detail="Next steps failed. See the llm-service log.")


def _mock_next_steps(request: NextStepsRequest) -> NextStepsResponse:
    """Rule-based next steps: mock mode, and installs without an LLM."""
    suggestions = []
    row_count = request.result_profile.row_count

    # Suggest download for larger results
    if row_count > 10:
        suggestions.append(
            NextStepSuggestion(
                action_type="download_csv",
                title="Download CSV",
                description=f"Export {row_count} rows to CSV",
                confidence=0.8,
                required_inputs=[],
                cta="Download",
            )
        )

    # Suggest sharing
    suggestions.append(
        NextStepSuggestion(
            action_type="slack",
            title="Share to Slack",
            description="Send results to a Slack channel",
            confidence=0.6,
            required_inputs=["channel"],
            cta="Share",
        )
    )

    return NextStepsResponse(suggestions=suggestions[:3])

