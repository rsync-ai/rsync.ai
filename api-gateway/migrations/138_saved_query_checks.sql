-- Migration 138: data checks on a model's table (docs/internal/data-checks-design.md).
--
-- A check is a rule on a model's output table, saved with the model and run on every
-- rebuild. A blocking check that fails keeps the new table from going live: the run
-- records 'failed', the old table stays, and every model below it is skipped with the
-- new reason 'upstream_check_failed'. Four changes:
--
--   1. saved_query_checks, a child of the saved query, keyed like
--      saved_query_schedule_upstreams (100). Isolation comes through the parent: every
--      handler reaches a check through the saved query's own workspace gate.
--   2. saved_query_check_results, one row per check per run. Results die with their run
--      (ON DELETE CASCADE), so they share its retention. Deleting a check sets check_id
--      NULL and keeps the name and type, as 086 does for schedule_id: removing a check
--      must not erase the evidence of what it blocked.
--   3. saved_query_pending_edits (096) also carries a proposed custom_sql check. Such a
--      check runs as the schedule's run_as user and may read any table on the
--      connection, so on a scheduled model it waits for an admin, through the same queue
--      the model's own SQL uses. kind says which of the two a row is; the one-open-
--      proposal index that guards a model's SQL now covers kind 'sql' only, and a check
--      proposal gets its own: one open proposal per check.
--   4. pipeline_notifications.saved_query_id: a check alert is about a model, not a
--      pipeline. Without it the row would read as an instance alert (pipeline_id NULL,
--      112) and reach every workspace's bell. The bell scopes a model alert through
--      saved_queries.workspace_id, the one source of truth, as 086 argues for runs.
--
-- And the skip reason CHECK gains 'upstream_check_failed', dropped and re-added NOT
-- VALID and validated after COMMIT as in 104 and 136: validating under the ALTER's
-- ACCESS EXCLUSIVE lock would make every run write wait for the scan. The new foreign
-- key on pipeline_notifications is validated after COMMIT for the same reason.
-- lock_timeout bounds the wait behind a running write: a blocked migration fails fast
-- and the next api-gateway start re-runs it (as 132, 133, 135 and 136). Every statement
-- is re-runnable.

BEGIN;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS saved_query_checks (
    check_id       UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    saved_query_id UUID NOT NULL REFERENCES saved_queries(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    check_type     TEXT NOT NULL CHECK (check_type IN
                     ('row_count', 'row_count_change', 'not_null', 'unique', 'custom_sql')),
    columns        TEXT[] NOT NULL DEFAULT '{}',
    -- min, max (row_count); max_drop_pct, max_growth_pct (row_count_change).
    params         JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- custom_sql only: one read statement that names the model's table as {{this}}.
    sql_text       TEXT,
    severity       TEXT NOT NULL CHECK (severity IN ('block', 'warn')),
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    -- No FK, as saved_queries.created_by (084): the record outlives the account.
    created_by     UUID,
    updated_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (saved_query_id, name),
    CONSTRAINT saved_query_checks_sql_only_for_custom CHECK (
        (check_type = 'custom_sql') = (sql_text IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS saved_query_check_results (
    run_id     UUID NOT NULL REFERENCES saved_query_runs(run_id) ON DELETE CASCADE,
    check_id   UUID REFERENCES saved_query_checks(check_id) ON DELETE SET NULL,
    -- As it was at run time.
    check_name TEXT NOT NULL,
    check_type TEXT NOT NULL,
    -- What the check did on this run: 'warn' for a blocking check on an engine that
    -- cannot hold a table back.
    severity   TEXT NOT NULL CHECK (severity IN ('block', 'warn')),
    status     TEXT NOT NULL CHECK (status IN ('pass', 'fail', 'error')),
    -- The count the check saw. Stored for the person and for row_count_change; it never
    -- leaves through an LLM-bound tool.
    observed   BIGINT,
    -- What it was compared with, for display.
    threshold  TEXT,
    -- Scrubbed, as a run error is.
    message    TEXT,
    PRIMARY KEY (run_id, check_name)
);

-- "This check's last results" and row_count_change's baseline.
CREATE INDEX IF NOT EXISTS idx_sq_check_results_check
    ON saved_query_check_results(check_id);

ALTER TABLE saved_query_pending_edits
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'sql',
    -- The check a proposal edits; NULL for a new check. No FK: the verdict stays
    -- readable after the check is removed, and approving a proposal whose check is gone
    -- is refused by the handler.
    ADD COLUMN IF NOT EXISTS check_id UUID,
    -- The rest of the proposed check: name, severity, enabled.
    ADD COLUMN IF NOT EXISTS check_payload JSONB;

ALTER TABLE saved_query_pending_edits DROP CONSTRAINT IF EXISTS saved_query_pending_edits_kind_check;
ALTER TABLE saved_query_pending_edits ADD CONSTRAINT saved_query_pending_edits_kind_check
    CHECK (kind IN ('sql', 'check') AND (kind = 'sql' OR check_payload IS NOT NULL));

DROP INDEX IF EXISTS idx_sq_pending_edits_one_open;
CREATE UNIQUE INDEX IF NOT EXISTS idx_sq_pending_edits_one_open
    ON saved_query_pending_edits(saved_query_id)
    WHERE status = 'pending' AND kind = 'sql';

CREATE UNIQUE INDEX IF NOT EXISTS idx_sq_pending_edits_one_open_check
    ON saved_query_pending_edits(saved_query_id, (COALESCE(check_id::text, 'new:' || (check_payload->>'name'))))
    WHERE status = 'pending' AND kind = 'check';

ALTER TABLE pipeline_notifications
    ADD COLUMN IF NOT EXISTS saved_query_id UUID;

ALTER TABLE pipeline_notifications DROP CONSTRAINT IF EXISTS pipeline_notifications_saved_query_id_fkey;
ALTER TABLE pipeline_notifications ADD CONSTRAINT pipeline_notifications_saved_query_id_fkey
    FOREIGN KEY (saved_query_id) REFERENCES saved_queries(id) ON DELETE CASCADE NOT VALID;

CREATE INDEX IF NOT EXISTS idx_pipeline_notifications_saved_query
    ON pipeline_notifications(saved_query_id, created_at DESC)
    WHERE saved_query_id IS NOT NULL;

ALTER TABLE saved_query_runs DROP CONSTRAINT IF EXISTS saved_query_runs_skip_reason_check;
ALTER TABLE saved_query_runs ADD CONSTRAINT saved_query_runs_skip_reason_check
    CHECK (skip_reason IS NULL OR skip_reason IN
        ('upstream_failed', 'upstream_skipped', 'chain_depth_exceeded', 'waiting_on_upstreams', 'trigger_lost', 'upstream_check_failed')) NOT VALID;

COMMENT ON TABLE saved_query_checks IS
    'Data checks on a model''s output table; a failing block check keeps the new table from going live';
COMMENT ON COLUMN pipeline_notifications.saved_query_id IS
    'The model this alert is about (a data check). The bell scopes it through saved_queries.workspace_id.';

COMMIT;

ALTER TABLE saved_query_runs VALIDATE CONSTRAINT saved_query_runs_skip_reason_check;
ALTER TABLE pipeline_notifications VALIDATE CONSTRAINT pipeline_notifications_saved_query_id_fkey;
