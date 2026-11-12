-- Migration 153: lineage history and the OpenLineage push target, for the Lineage page
-- (handlers/lineage_events.go, lineage_history.go, openlineage*.go; lineage design steps
-- L2 and L3: docs/internal/lineage-catalog-design.md §§ 4–5).
--
-- lineage_events: one row per run, with the tables it read and wrote. A model run
-- (saved_query_runs), a batch pipeline run (an execution that ended) or a CDC
-- pipeline's trailing-edge window. Names only: never a row, a row count, the SQL or an
-- error.
--
--   job_kind    'model' (saved_query_id set) or 'pipeline' (pipeline_id set).
--   run_ref     the model run's run_id, the execution's id, or the CDC window's
--               occurrence. With the job, the key that makes recording a run twice a
--               no-op.
--   event_type  COMPLETE, FAIL, or ABORT (a model run that was skipped, an execution
--               that was cancelled).
--   inputs, outputs  [{"connection_id", "name", "captured"?}], the name lower case as
--               the run knew it: "schema.table", "table", or a pipeline's source name.
--               "captured": true marks a pipeline output written under no destination
--               name: its name is the source's.
--   table_keys  what the history looks a table node up by: "<connection>\x1f<name>"
--               ("schema.table" or "table", the graph's key) and, for a table a
--               pipeline wrote under no destination name, "<connection>\x1fcaptured
--               \x1f<name>". A pipeline's source tables have no node and no key.
--   sql_hash    the model's SQL version (asset_column_lineage.go columnLineageSQLHash),
--               "" when the model was edited after the run started, so the SQL that ran
--               is not known. model_column_lineage keeps the version's columns while an
--               event names it.
--
-- Kept LINEAGE_EVENT_RETENTION_DAYS (365) and deleted by the recorder's daily sweep.
--
-- openlineage_targets: where a workspace's events are pushed, one per workspace. The
-- auth header's value is encrypted (shared/crypto) and never returned by the API.
--
-- The runner wraps this file in one transaction; it is safe to run again.

CREATE TABLE IF NOT EXISTS lineage_events (
    id             UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    job_kind       TEXT        NOT NULL CHECK (job_kind IN ('model', 'pipeline')),
    saved_query_id UUID        REFERENCES saved_queries(id) ON DELETE CASCADE,
    pipeline_id    UUID        REFERENCES pipelines(id) ON DELETE CASCADE,
    run_ref        TEXT        NOT NULL,
    event_type     TEXT        NOT NULL CHECK (event_type IN ('COMPLETE', 'FAIL', 'ABORT')),
    started_at     TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ NOT NULL,
    inputs         JSONB       NOT NULL DEFAULT '[]',
    outputs        JSONB       NOT NULL DEFAULT '[]',
    table_keys     TEXT[]      NOT NULL DEFAULT '{}',
    sql_hash       TEXT        NOT NULL DEFAULT '',
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT lineage_events_one_job CHECK (
        (job_kind = 'model' AND saved_query_id IS NOT NULL AND pipeline_id IS NULL)
        OR (job_kind = 'pipeline' AND pipeline_id IS NOT NULL AND saved_query_id IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_lineage_events_model_run
    ON lineage_events (saved_query_id, run_ref) WHERE saved_query_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_lineage_events_pipeline_run
    ON lineage_events (pipeline_id, run_ref) WHERE pipeline_id IS NOT NULL;
-- A model's SQL versions that events still name (the column lineage cleanup).
CREATE INDEX IF NOT EXISTS idx_lineage_events_model_sql
    ON lineage_events (saved_query_id, sql_hash) WHERE saved_query_id IS NOT NULL;
-- History and the OpenLineage pull, newest first or by time range, per workspace.
CREATE INDEX IF NOT EXISTS idx_lineage_events_workspace_ended
    ON lineage_events (workspace_id, ended_at DESC, id);
-- A table node's history.
CREATE INDEX IF NOT EXISTS idx_lineage_events_table_keys
    ON lineage_events USING GIN (table_keys);
-- The retention sweep.
CREATE INDEX IF NOT EXISTS idx_lineage_events_ended
    ON lineage_events (ended_at);

-- The recorder reads the model runs that finished in the last two days; no other index
-- on saved_query_runs leads with finished_at.
CREATE INDEX IF NOT EXISTS idx_saved_query_runs_finished
    ON saved_query_runs (finished_at);

CREATE TABLE IF NOT EXISTS openlineage_targets (
    workspace_id      UUID        PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    url               TEXT        NOT NULL,
    auth_header_enc   TEXT        NOT NULL DEFAULT '',
    enabled           BOOLEAN     NOT NULL DEFAULT true,
    -- 'delivered', 'failed' or 'refused' (a private address), and when; the target's
    -- response body is never kept.
    last_status       TEXT        NOT NULL DEFAULT '',
    last_status_code  INTEGER,
    last_delivered_at TIMESTAMPTZ,
    updated_by        UUID,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
