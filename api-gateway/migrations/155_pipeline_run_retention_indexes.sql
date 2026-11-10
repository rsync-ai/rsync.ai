-- 155_pipeline_run_retention_indexes.sql
-- Indexes for the run-history retention sweep (internal/retention/pipeline_run_retention.go),
-- on by default from this release: it deletes run events and run metrics past
-- PIPELINE_RUN_RETENTION_DAYS (90) once a day, a chunk at a time.
--
--   - pipeline_run_events(created_at) and pipeline_run_metrics(ts): every chunk starts
--     at the oldest row. Neither table had an index that leads with its age column, so
--     each chunk read the whole table.
--   - pipeline_run_events(pipeline_id, received_at DESC) WHERE PIPELINE_COMPLETED: the
--     sweep keeps the run holding each pipeline's newest completion (a model's upstream
--     gate reads it: handlers/saved_query_schedules.go unsatisfiedUpstreams) and looks
--     it up once per candidate row. The same lookup serves that gate. The existing
--     (pipeline_id, received_at DESC) index can't answer it for a pipeline that never
--     completes (a CDC stream): it walks every event the pipeline has, then finds none
--     (200k events: 21 ms per lookup, against 0.05 ms here).
--
-- executions already has (pipeline_id, start_time DESC) (031) for "the pipeline's
-- newest run".
--
-- The builds lock each table against writes for as long as they take, which grows with
-- the table: the projector's inserts wait, and catch up from Kafka afterwards.
-- lock_timeout bounds the wait to START, behind a long transaction: a blocked migration
-- fails fast and the next api-gateway start re-runs it. Same reasoning and timeout as
-- 098 / 121 / 132. The runner owns the transaction (no CONCURRENTLY inside one).

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_pipeline_run_events_created_at
    ON pipeline_run_events (created_at);

CREATE INDEX IF NOT EXISTS idx_pipeline_run_events_completed_latest
    ON pipeline_run_events (pipeline_id, received_at DESC)
    WHERE event_type = 'PIPELINE_COMPLETED';

CREATE INDEX IF NOT EXISTS idx_pipeline_run_metrics_ts
    ON pipeline_run_metrics (ts);
