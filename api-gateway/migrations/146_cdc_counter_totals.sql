-- Migration 146: CDC counters as totals, not a row per change event
-- (KI-CDC-ACK-LEDGER-UNBOUNDED).
--
-- The kafka-sink worker wrote one pipeline_batch_acks row per CDC event, so a CDC
-- pipeline's ledger grew with every change it ever carried and nothing trimmed it: the
-- table is only cascaded away when its pipeline is deleted, and the run retention skips
-- the CDC execution (execution_id = pipeline_id). Its two jobs move to two tables whose
-- size follows the tables and partitions a pipeline reads, not its event count:
--
--   cdc_counter_totals  one running total per (pipeline, execution, table, op), op in
--                       c / r / u / d. The sink seeds its TABLE_STATS counters from it
--                       at restart, so the UI totals survive a restart.
--   cdc_offset_floors   the highest Kafka offset landed per (pipeline, execution,
--                       table, topic, partition). An offset at or below its floor is a
--                       replay after a restart and is not counted twice.
--
-- The sink adds to both in one statement per flush. Per-event CDC rows written before
-- this migration are folded into these tables by the api-gateway's batch-ack janitor
-- (internal/retention/batch_ack_janitor.go) and then deleted; until the fold reaches a
-- pipeline, the sink's seed adds its unfolded rows to these totals.
--
-- No foreign key to executions: the CDC execution row is a synthetic anchor the sink
-- creates on its own, and these rows must not depend on it. pipeline_id cascades, as
-- pipeline_batch_acks does, so deleting a pipeline deletes its counters.
--
-- The runner wraps this file in one transaction. lock_timeout bounds the wait for the
-- brief lock the foreign key takes on pipelines: a blocked migration fails fast and the
-- next api-gateway start re-runs it. New tables only; idempotent.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS cdc_counter_totals (
    pipeline_id   UUID         NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    execution_id  UUID         NOT NULL,
    table_name    VARCHAR(512) NOT NULL,
    op            VARCHAR(1)   NOT NULL CHECK (op IN ('c', 'r', 'u', 'd')),
    total         BIGINT       NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, execution_id, table_name, op)
);

CREATE TABLE IF NOT EXISTS cdc_offset_floors (
    pipeline_id      UUID         NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    execution_id     UUID         NOT NULL,
    table_name       VARCHAR(512) NOT NULL,
    kafka_topic      VARCHAR(512) NOT NULL,
    kafka_partition  INTEGER      NOT NULL,
    max_offset       BIGINT       NOT NULL,
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition)
);

COMMENT ON TABLE cdc_counter_totals IS
    'CDC rows landed per (pipeline, execution, table, op); the sink adds each flush and seeds TABLE_STATS from it at restart (migration 146)';
COMMENT ON TABLE cdc_offset_floors IS
    'Highest Kafka offset landed per CDC (pipeline, execution, table, topic, partition); an offset at or below it is a replay (migration 146)';
