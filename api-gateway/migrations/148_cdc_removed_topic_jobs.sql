-- Migration 148: the orchestrator's removed-topic deletions survive a restart.
--
-- When a table leaves a CDC connector's include list, the orchestrator's
-- RemovedTopicReaper (backend-orchestrator/internal/handlers/cdc_removed_topics.go)
-- deletes the table's Kafka topic once nothing still needs it. Its jobs lived only in
-- memory, so an orchestrator restart while a job waited (for the old connector task
-- to restart, or the sink to apply the topic) dropped the job and the topic stayed
-- for good. One row per job; the orchestrator writes it when the job starts, deletes
-- it when the job ends (topic deleted, kept, or the deadline passed), and resumes
-- every remaining row at its next start.
--
--   topic          the exact Kafka topic the job may delete: the allowlist. Nothing
--                  else is ever deleted from these rows.
--   scheduled_at   when the table was removed; the job's deadline runs from here, so
--                  a restart does not extend it.
--
-- pipeline_id cascades: a deleted pipeline's topics belong to its teardown.
--
-- The runner wraps this file in one transaction. lock_timeout bounds the wait for the
-- brief lock the foreign key takes on pipelines: a blocked migration fails fast and the
-- next api-gateway start re-runs it. New table only; idempotent.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS cdc_removed_topic_jobs (
    pipeline_id     UUID         NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    topic           VARCHAR(512) NOT NULL,
    connector_name  VARCHAR(255) NOT NULL,
    table_name      VARCHAR(512) NOT NULL,
    scheduled_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, topic)
);

COMMENT ON TABLE cdc_removed_topic_jobs IS
    'Kafka topics of tables removed from a CDC connector, waiting for the orchestrator to delete them once applied (migration 148)';
