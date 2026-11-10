"""LLM-based table ranking endpoint.

Ranks a list of tables by relevance to a user's natural-language intent using
an LLM. Falls back gracefully: if called with no sample data the model uses
column names only, which is cheap and avoids accessing any actual row data.
"""

from __future__ import annotations

import json
import logging
import os
import re
from typing import List, Optional

from fastapi import APIRouter, HTTPException
from openai import AsyncOpenAI
from pydantic import BaseModel

logger = logging.getLogger("llm-rank-tables")

router = APIRouter(tags=["Explorer"])


# ---------------------------------------------------------------------------
# Provider resolution — shared, NOT duplicated
# ---------------------------------------------------------------------------
# This module used to carry its own copies of _env_bool / _ollama_base_url /
# _resolve_provider / _create_async_client "to keep it self-contained", and they
# had drifted into three live defects:
#
#   1. Its _resolve_provider auto-detected from OPENAI_API_KEY and never read
#      LLM_PROVIDER, so `LLM_PROVIDER=ollama` with a stale key in .env still
#      sent table names, column names and row counts to api.openai.com.
#   2. It ignored EXPLORER_OFFLINE_ONLY entirely — the flag whose whole purpose
#      is "schema metadata does not leave this deployment".
#   3. Its default model was the literal "gpt-4o-mini" on every non-Azure
#      provider, so even an operator who found RANK_TABLES_LLM_PROVIDER=ollama
#      asked Ollama for a model it has never pulled.
#
# Self-contained is not worth a silent egress. Import the shared resolver.
# ---------------------------------------------------------------------------

from src.utils.openai_client import (  # noqa: E402
    env_bool as _env_bool,
    make_async_client as _create_async_client,
    rank_tables_default_model as _rank_tables_default_model,
    resolve_explorer_provider as _resolve_explorer_provider,
    _ollama_base_url,
    explorer_llm_configured as _explorer_llm_configured,
    with_reasoning_headroom,
)
from src.egress import EgressRefused, guarded_acall  # noqa: E402


def _extract_json_array(raw: str) -> str:
    """Extract JSON array from LLM output, handling markdown fences."""
    text = str(raw or "").strip()
    m = re.search(r"```(?:json)?\s*([\s\S]*?)\s*```", text, re.IGNORECASE)
    if m:
        text = m.group(1).strip()
    start = text.find("[")
    end = text.rfind("]")
    if start >= 0 and end > start:
        return text[start : end + 1]
    return "[]"


# ---------------------------------------------------------------------------
# Runtime config
# ---------------------------------------------------------------------------

USE_MOCK = _env_bool("USE_MOCK_LLM", False)
EXPLORER_OFFLINE_ONLY = _env_bool("EXPLORER_OFFLINE_ONLY", False)

# RANK_TABLES_LLM_PROVIDER overrides; unset inherits EXPLORER_LLM_PROVIDER then
# LLM_PROVIDER. EXPLORER_OFFLINE_ONLY beats all of them.
RANK_TABLES_PROVIDER = _resolve_explorer_provider("RANK_TABLES_LLM_PROVIDER")
RANK_TABLES_MODEL = _rank_tables_default_model(RANK_TABLES_PROVIDER)

_client: Optional[AsyncOpenAI] = None if USE_MOCK else _create_async_client(RANK_TABLES_PROVIDER)

# This endpoint sends table names, column names and types. Say where they
# go — the previous silence is what let it point at OpenAI for months while
# the operator believed EXPLORER_OFFLINE_ONLY covered the whole Explorer.
logger.info(
    "rank-tables llm: provider=%s offline_only=%s model=%s ollama_base=%s",
    RANK_TABLES_PROVIDER,
    EXPLORER_OFFLINE_ONLY,
    RANK_TABLES_MODEL,
    _ollama_base_url() if RANK_TABLES_PROVIDER == "ollama" else "-",
)
if EXPLORER_OFFLINE_ONLY and RANK_TABLES_PROVIDER != "ollama":
    logger.error(
        "EXPLORER_OFFLINE_ONLY=true but rank-tables resolved to provider=%s — "
        "schema metadata WILL leave this deployment",
        RANK_TABLES_PROVIDER,
    )


# ---------------------------------------------------------------------------
# Request / Response models
# ---------------------------------------------------------------------------

class ColumnInfo(BaseModel):
    name: str
    data_type: Optional[str] = None


class TableInfo(BaseModel):
    name: str
    schema_name: Optional[str] = None
    row_count: Optional[int] = None
    columns: Optional[List[ColumnInfo]] = None


class RankTablesRequest(BaseModel):
    tables: List[TableInfo]
    intent: str
    use_case: Optional[str] = "replication"
    max_tables: Optional[int] = 10
    provider: Optional[str] = None


class RankedTable(BaseModel):
    name: str
    schema_name: Optional[str] = None
    row_count: Optional[int] = None
    confidence: float
    reason: str
    category: str
    has_pii: bool


# ---------------------------------------------------------------------------
# Endpoint
# ---------------------------------------------------------------------------

@router.post("/agents/rank-tables", response_model=List[RankedTable])
async def rank_tables(request: RankTablesRequest) -> List[RankedTable]:
    """Rank a list of database tables by relevance to the user's intent.

    Uses an LLM to produce semantic rankings with natural-language reasons.
    Sends only table/column names — no row data is transmitted to the model.
    """
    if not request.tables:
        return []

    # `provider` in the request body used to pick another provider per call:
    # a request body choosing where schema metadata goes. Only the deployment's
    # configuration chooses that, so the field is refused, offline or not.
    if request.provider:
        raise HTTPException(
            status_code=400,
            detail=(
                "A per-request provider is not accepted. rank-tables uses the "
                "provider the deployment configures (RANK_TABLES_LLM_PROVIDER)."
            ),
        )

    client = _client
    model = RANK_TABLES_MODEL
    llm_ready = _explorer_llm_configured("RANK_TABLES_LLM_PROVIDER")
    max_tables = min(request.max_tables or 10, len(request.tables))

    if USE_MOCK or client is None or not llm_ready:
        # Offline fallback, also used when no LLM is set up: table selection
        # must keep working without one.
        return [
            RankedTable(
                name=t.name,
                schema_name=t.schema_name,
                row_count=t.row_count,
                confidence=0.3,
                reason="Heuristic fallback (LLM unavailable)",
                category="general",
                has_pii=False,
            )
            for t in request.tables[:max_tables]
        ]

    # Names and types only, ten columns a table. row_count stays out: it
    # describes the customer's data, and the egress guard would drop it anyway.
    tables = [
        {
            "name": t.name,
            "schema_name": t.schema_name,
            "columns": [{"name": c.name, "data_type": c.data_type} for c in (t.columns or [])][:10],
        }
        for t in request.tables
    ]

    try:
        response = await guarded_acall(
            client,
            component="explorer.rank_tables",
            model=model,
            prompt="explorer/rank_tables",
            variables={
                "tables": tables,
                "intent": request.intent,
                "use_case": request.use_case or "replication",
                "max_tables": max_tables,
            },
            temperature=0.2,
            max_tokens=with_reasoning_headroom(1024),
        )
        ranked_raw: list = json.loads(_extract_json_array(response.text or "[]"))
    except EgressRefused as exc:
        logger.warning("LLM ranking refused by the egress guard: %s", exc.reason)
        raise HTTPException(status_code=503, detail="LLM ranking unavailable: the request was refused")
    except Exception as exc:
        # The exception text can quote the provider's reply: the log keeps it,
        # the response does not.
        logger.warning("LLM ranking failed: %s", exc)
        raise HTTPException(status_code=503, detail="LLM ranking unavailable")

    # Build a lookup for original table metadata
    table_meta = {t.name: t for t in request.tables}

    results: List[RankedTable] = []
    for item in ranked_raw:
        if not isinstance(item, dict):
            continue
        name = str(item.get("name", ""))
        meta = table_meta.get(name)
        results.append(
            RankedTable(
                name=name,
                schema_name=meta.schema_name if meta else None,
                row_count=meta.row_count if meta else None,
                confidence=float(item.get("confidence", 0.5)),
                reason=str(item.get("reason", "")),
                category=str(item.get("category", "general")),
                has_pii=bool(item.get("has_pii", False)),
            )
        )

    return results
