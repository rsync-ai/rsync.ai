-- Migration 139: workflow runs move onto Temporal (docs/internal/workflows-design.md § 4.1,
-- build step 4).
--
-- A run is now a WorkflowRunWorkflow on temporal-adapter's `workflow-runs` queue. Its
-- steps call api-gateway's internal workflow-run endpoints, which still run the query and
-- the sends; the gateway's ticker, its run goroutines and its stale-run sweep are gone.
-- Three changes:
--
--   1. workflow_runs.temporal_workflow_id: the Temporal workflow a run belongs to. A
--      scheduled run is inserted by its workflow's first step, so this id is how a
--      retried step finds the run it already inserted instead of inserting a second.
--      Unique where set. NULL on every run from before this migration.
--   2. workflow_sends, the sends ledger (design § 5): one row per channel a run sends
--      to, keyed (run_id, channel_id). A send claims its row (pending -> sending) before
--      it posts and records the answer after, so a step Temporal retries after a send
--      the channel accepted finds 'sent' and sends nothing twice. channel_id has no
--      foreign key: the row is the record of where a message went and outlives the
--      channel, as workflow_runs.deliveries does. Build step 5 adds the step to the key.
--   3. A run left 'running' by the old in-process runner can never finish now: no
--      goroutine owns it and no Temporal workflow will. It is failed with the sentence
--      the old stale-run sweep wrote, so it no longer blocks Run now ("already running").
--
-- lock_timeout bounds the wait behind a running write: a blocked migration fails fast
-- and the next api-gateway start re-runs it (as 132, 133, 135, 136 and 138). Every
-- statement is re-runnable.

BEGIN;

SET LOCAL lock_timeout = '3s';

ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS temporal_workflow_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_runs_temporal_id
    ON workflow_runs (temporal_workflow_id) WHERE temporal_workflow_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS workflow_sends (
    run_id      UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    channel_id  UUID NOT NULL,
    position    SMALLINT NOT NULL,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'skipped')),
    sent        INTEGER NOT NULL DEFAULT 0,
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (run_id, channel_id)
);

UPDATE workflow_runs
SET status = 'failed', finished_at = NOW(),
    error = 'rsync.ai restarted while this run was in progress; the next run starts fresh.'
WHERE status = 'running' AND temporal_workflow_id IS NULL;

COMMIT;
