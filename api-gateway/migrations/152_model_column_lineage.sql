-- Migration 152: each model's column lineage, for the Lineage page
-- (handlers/asset_column_lineage.go, lineage design step L1:
-- docs/internal/lineage-catalog-design.md § 3.3).
--
-- llm-service reads a model's SQL with sqlglot (POST /v1/lineage/columns, no model
-- call) and says which source column feeds which output column, and which columns
-- only filter, join or group the rows. That answer is saved here and read again in
-- the background when the model's SQL changes, when the warehouse's columns could not
-- be read (after 10 minutes), or once a day (a warehouse's columns change without the
-- model changing).
--
-- One row per model and SQL version. sql_hash is sha256 of the connection type, a NUL
-- and the SQL, so an edit is a new row; until lineage history (step L2) keeps the
-- versions a past run used, writing a new row deletes the model's older ones.
--
-- result      {"columns": [...], "edges": [{"table": [...], "column", "to_column",
--              "type", "subtype"}], "truncated": bool}: names only, never a row value.
-- had_schema  false when the warehouse's columns could not be read, so a SELECT * or
--             an unqualified column may be partial; read again sooner.
--
-- The runner wraps this file in one transaction; it is safe to run again.

CREATE TABLE IF NOT EXISTS model_column_lineage (
    saved_query_id UUID        NOT NULL REFERENCES saved_queries(id) ON DELETE CASCADE,
    sql_hash       TEXT        NOT NULL,
    workspace_id   UUID        NOT NULL,
    status         TEXT        NOT NULL CHECK (status IN ('ok', 'partial', 'failed', 'unsupported')),
    reason         TEXT        NOT NULL DEFAULT '',
    result         JSONB       NOT NULL DEFAULT '{}',
    schema_hash    TEXT        NOT NULL DEFAULT '',
    had_schema     BOOLEAN     NOT NULL DEFAULT false,
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (saved_query_id, sql_hash)
);

CREATE INDEX IF NOT EXISTS idx_model_column_lineage_workspace
    ON model_column_lineage (workspace_id);
