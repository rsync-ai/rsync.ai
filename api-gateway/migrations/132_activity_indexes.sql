-- 132_activity_indexes.sql
-- Indexes for the Runs page (GET /api/v1/activity, handlers/activity.go).
--
-- The Runs page lists a workspace's pipeline, scheduled-query and workflow runs
-- newest first, one page at a time. Each source is read newest first and cut to
-- the page size before the three are merged, so each needs an index that walks
-- its runs in start order:
--   - executions already has one on start_time (021b);
--   - workflow_runs had only (workflow_id, started_at), one workflow at a time,
--     so a workspace's runs across all its workflows were sorted in full;
--   - saved_query_runs had only (saved_query_id, finished_at), the same.
--
-- The index builds lock each table against writes, so lock_timeout bounds the
-- wait behind a running workflow or scheduled query: a blocked migration fails
-- fast and the next api-gateway start re-runs it. Same reasoning and timeout as
-- 098 / 121 / 124. The runner owns the transaction (no CONCURRENTLY inside one).

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_workflow_runs_workspace_started
    ON workflow_runs (workspace_id, started_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_saved_query_runs_started
    ON saved_query_runs (started_at DESC, run_id DESC);
