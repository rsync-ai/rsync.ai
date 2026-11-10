"""Column lineage for a batch of models: ``POST /v1/lineage/columns``.

api-gateway calls it from the background read behind the Lineage page
(handlers/asset_column_lineage.go) and keeps each answer in model_column_lineage.
Lineage design step L1, docs/internal/lineage-catalog-design.md § 3.

Service to service (``X-Internal-Secret``). Read with sqlglot (src/lineage/columns.py):
no model is called, so this is not an egress call site, and nothing it is sent leaves
this process. What it is sent is metadata: each model's SQL and the column names of the
tables it reads.
"""

from __future__ import annotations

import logging

from fastapi import APIRouter, Depends
from pydantic import BaseModel, ConfigDict, Field

from src.agents.tool_generator.deployment.routes import require_internal_secret
from src.lineage.columns import STATUS_FAILED, ModelLineage, SourceTable, model_column_lineage

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/v1/lineage", tags=["lineage"], dependencies=[Depends(require_internal_secret)])

# api-gateway sends at most this many models per call (columnLineageBatch).
MAX_MODELS = 100


class LineageTable(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)
    schema_name: str = Field(default="", alias="schema", max_length=512)
    name: str = Field(min_length=1, max_length=512)
    columns: list[str] = Field(default_factory=list, max_length=5000)


class LineageModel(BaseModel):
    model_config = ConfigDict(extra="forbid")
    id: str = Field(pattern=r"^[A-Za-z0-9-]{1,64}$")
    sql: str = Field(max_length=200_000)
    connector_type: str = Field(default="", max_length=64)
    tables: list[LineageTable] = Field(default_factory=list, max_length=500)


class LineageRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    models: list[LineageModel] = Field(max_length=MAX_MODELS)


@router.post("/columns")
def column_lineage(req: LineageRequest) -> dict:
    """One answer per model, in the order asked. One model's failure is its own answer."""
    answers = []
    for m in req.models:
        tables = [SourceTable(t.schema_name, t.name, tuple(t.columns)) for t in m.tables]
        try:
            res = model_column_lineage(m.sql, m.connector_type, tables)
        except Exception:  # noqa: BLE001 — a parser bug on one model must not cost the batch
            logger.warning("column lineage: model %s could not be read", m.id, exc_info=True)
            res = ModelLineage(STATUS_FAILED, reason="column lineage could not read this model")
        answers.append(
            {
                "id": m.id,
                "status": res.status,
                "columns": res.columns,
                "edges": [
                    {
                        "table": list(e.table),
                        "column": e.column,
                        "to_column": e.to_column,
                        "type": e.type,
                        "subtype": e.subtype,
                    }
                    for e in res.edges
                ],
                "reason": res.reason,
                "truncated": res.truncated,
            }
        )
    return {"models": answers}
