-- Migration 137: a CDC pipeline says how far it has landed, and "run after pipeline"
-- fires on a window's trailing edge (docs/internal/workflows-design.md § 6, gaps 1, 2, 5).
--
-- A CDC pipeline never completes, so nothing told a model or a workflow what it had
-- landed. Three things follow from that, and this migration holds the state for all
-- three.
--
-- cdc_table_watermarks — one row per (pipeline, table), upserted by the kafka-mcp-sink
-- after each commit it records in the ack ledger:
--   source_time  the newest source time landed (Debezium source.ts_ms, the same value as
--                pipeline_batch_acks.cdc_source_ts, migration 036). The SOURCE clock, so
--                "built from CDC data up to T" means data the source committed by T.
--   landed_at    when the sink last landed rows for this table, on THIS database's clock
--                (now() at the upsert), so the trailing-edge sweep compares it with its
--                own now() and never with a second host's clock.
-- table_name is the source-side qualified name, the key pipeline_run_table_stats.qualified_name
-- uses for the same table; destination_qualified_name is where the rows landed, NULL when
-- the sink cannot name a destination namespace (object storage).
--
-- cdc_trailing_edge_state — one row per CDC pipeline, owned by the trailing-edge sweep
-- (the gateway's POST /api/v1/internal/cdc/trailing-edge/sweep, driven by one Temporal
-- timer workflow). window_end is the window boundary it last fired at; landed_through is
-- the newest landing that fire covered; pending_since is the first later landing a sweep
-- has seen. A fire happens at the first boundary after a landing, at most once per
-- window, so a burst followed by silence builds its tail once and a continuous stream
-- builds once per window. It is written AFTER the fire it records: a crash in between
-- fires again with the same occurrence, which the fan-out workflow id and the
-- workflow_runs (workflow_id, trigger_ref) index both drop as a duplicate.
--
-- saved_query_runs.cdc_watermarks — what a model run was built from: the CDC tables its
-- SQL reads and each one's watermark when the run started, as
-- {"<destination table>": {"pipeline_id": "...", "source_time": "..."}}. NULL when the
-- model reads no CDC table.
--
-- The tables are new, so creating them takes no lock anything else waits on. The column
-- is nullable with no default, which is a catalog-only change, but the ALTER still takes
-- ACCESS EXCLUSIVE on saved_query_runs for an instant: lock_timeout bounds the wait behind
-- a running model write, and the next api-gateway start re-runs a blocked migration (same
-- timeout as 132, 133, 135 and 136). Every statement is re-runnable.

BEGIN;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS cdc_table_watermarks (
    pipeline_id                UUID        NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    table_name                 TEXT        NOT NULL,
    destination_qualified_name TEXT,
    source_time                TIMESTAMPTZ,
    landed_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, table_name)
);

COMMENT ON TABLE cdc_table_watermarks IS
    'Per CDC table: the newest source time landed (source clock) and when the sink last landed rows (database clock). Upserted by the kafka-mcp-sink; read by the trailing-edge sweep, the upstream_policy=all check and each model run.';

CREATE TABLE IF NOT EXISTS cdc_trailing_edge_state (
    pipeline_id    UUID        PRIMARY KEY REFERENCES pipelines(id) ON DELETE CASCADE,
    window_end     TIMESTAMPTZ,
    landed_through TIMESTAMPTZ,
    pending_since  TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE cdc_trailing_edge_state IS
    'Per CDC pipeline: the window boundary the trailing-edge sweep last fired at, the newest landing it covered, and the first later landing seen. Written only by the sweep, after the fire.';

ALTER TABLE saved_query_runs ADD COLUMN IF NOT EXISTS cdc_watermarks JSONB;

COMMIT;
