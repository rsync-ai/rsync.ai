-- Migration 140: workflow steps (docs/internal/workflows-design.md §§ 3.1, 5 and 8, build
-- step 5).
--
-- A workflow is now a graph of steps: query, condition and send as before, plus
-- run_pipeline, refresh_model and wait. Every step has an id, and what used to be keyed by
-- a branch's position is keyed by its send step's id. A definition stored before step 5
-- reads as query -> condition -> send_1 .. send_N (workflowdef.FromBranches), so branch i
-- is step 'send_<i+1>' everywhere below.
--
--   1. workflow_step_runs: one row per step a run reaches, per attempt. execution_id is a
--      run_pipeline step's pipeline run, saved_query_run_id a refresh_model step's model
--      run, row_count and case_index a query's and a condition's outcome. reused_from is
--      written by "rerun from a step" (build step 6). Outcomes only, never a row value.
--   2. workflow_sends gains step_id and joins it to the key, so two send steps of one run
--      may use the same channel. Existing rows belong to the branch their run sent.
--   3. workflow_runs:
--        - deliveries: each element gains the step_id of the branch its run sent, so the
--          quiet period (workflowSentRecently) still sees the sends from before this
--          migration;
--        - status 'stopped', for a run a person stopped;
--        - stop_requested_at / stop_requested_by: the Stop button's record, also read by
--          each step so a stop requested mid-step is honoured at the next one;
--        - alive_until: how long a running run may go without a step before it counts as
--          abandoned. A step bumps it; a wait step bumps it past its own wait. NULL keeps
--          step 4's 15 minutes after started_at.
--   4. executions.workflow_run_id: the workflow run a pipeline run was started by (a
--      run_pipeline step). ON DELETE SET NULL: the pipeline run outlives the workflow.
--   5. workflow_channels: branch -> node_id, the send step a channel belongs to.
--   6. workflow_changes: branch_channel_ids (a list per branch) -> node_channel_ids (an
--      object keyed by send step id).
--   7. saved_query_runs.upstream_kind accepts 'workflow': a model run a refresh_model step
--      asked for names the workflow run as its upstream.
--
-- lock_timeout bounds the wait behind a running write: a blocked migration fails fast and
-- the next api-gateway start re-runs it (as 139). Every statement is re-runnable. The new
-- constraints are added NOT VALID and validated after COMMIT, as 104 does, so the scan of
-- executions and saved_query_runs holds no lock that blocks their writers.

BEGIN;

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS workflow_step_runs (
    run_id             UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_id            TEXT NOT NULL,
    attempt            SMALLINT NOT NULL DEFAULT 1,
    workspace_id       UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    step_type          TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'waiting', 'succeeded', 'warned', 'failed',
                                         'skipped', 'stopped')),
    execution_id       TEXT,
    saved_query_run_id UUID,
    reused_from        UUID REFERENCES workflow_runs(id) ON DELETE SET NULL,
    row_count          INT,
    case_index         SMALLINT,
    started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- requested_at: when a refresh_model step last asked for its model's run (DB clock);
    -- the run it waits for is the first one started at or after it.
    requested_at       TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ,
    error              TEXT,
    note               TEXT,
    PRIMARY KEY (run_id, step_id, attempt)
);
ALTER TABLE workflow_step_runs ADD COLUMN IF NOT EXISTS requested_at TIMESTAMPTZ;

ALTER TABLE workflow_sends ADD COLUMN IF NOT EXISTS step_id TEXT NOT NULL DEFAULT '';
UPDATE workflow_sends s
SET step_id = 'send_' || (COALESCE(r.branch, 0) + 1)
FROM workflow_runs r
WHERE r.id = s.run_id AND s.step_id = '';
ALTER TABLE workflow_sends DROP CONSTRAINT IF EXISTS workflow_sends_pkey;
ALTER TABLE workflow_sends ADD CONSTRAINT workflow_sends_pkey PRIMARY KEY (run_id, step_id, channel_id);

UPDATE workflow_runs r
SET deliveries = (
    SELECT jsonb_agg(CASE WHEN e.d ? 'step_id' THEN e.d
                          ELSE e.d || jsonb_build_object('step_id', 'send_' || (COALESCE(r.branch, 0) + 1))
                     END ORDER BY e.n)
    FROM jsonb_array_elements(r.deliveries) WITH ORDINALITY AS e(d, n))
WHERE jsonb_typeof(r.deliveries) = 'array'
  AND EXISTS (SELECT 1 FROM jsonb_array_elements(r.deliveries) AS x(d)
              WHERE jsonb_typeof(x.d) = 'object' AND NOT x.d ? 'step_id');

ALTER TABLE workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_check;
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_runs_status_check
    CHECK (status IN ('running', 'succeeded', 'failed', 'stopped')) NOT VALID;

ALTER TABLE workflow_runs
    ADD COLUMN IF NOT EXISTS stop_requested_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS stop_requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS alive_until TIMESTAMPTZ;

ALTER TABLE executions ADD COLUMN IF NOT EXISTS workflow_run_id UUID;
ALTER TABLE executions DROP CONSTRAINT IF EXISTS executions_workflow_run_id_fkey;
ALTER TABLE executions ADD CONSTRAINT executions_workflow_run_id_fkey
    FOREIGN KEY (workflow_run_id) REFERENCES workflow_runs(id) ON DELETE SET NULL NOT VALID;
CREATE INDEX IF NOT EXISTS idx_executions_workflow_run
    ON executions (workflow_run_id) WHERE workflow_run_id IS NOT NULL;

ALTER TABLE workflow_channels ADD COLUMN IF NOT EXISTS node_id TEXT;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = current_schema() AND table_name = 'workflow_channels'
                 AND column_name = 'branch') THEN
        EXECUTE 'UPDATE workflow_channels SET node_id = ''send_'' || (branch + 1) WHERE node_id IS NULL';
        ALTER TABLE workflow_channels DROP CONSTRAINT IF EXISTS workflow_channels_pkey;
        ALTER TABLE workflow_channels DROP COLUMN branch;
    END IF;
END $$;
ALTER TABLE workflow_channels ALTER COLUMN node_id SET NOT NULL;
ALTER TABLE workflow_channels DROP CONSTRAINT IF EXISTS workflow_channels_pkey;
ALTER TABLE workflow_channels
    ADD CONSTRAINT workflow_channels_pkey PRIMARY KEY (workflow_id, node_id, channel_id);

ALTER TABLE workflow_changes ADD COLUMN IF NOT EXISTS node_channel_ids JSONB;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = current_schema() AND table_name = 'workflow_changes'
                 AND column_name = 'branch_channel_ids') THEN
        EXECUTE $q$
            UPDATE workflow_changes c
            SET node_channel_ids = CASE WHEN jsonb_typeof(c.branch_channel_ids) = 'array' THEN
                COALESCE((SELECT jsonb_object_agg('send_' || e.n, e.ids)
                          FROM jsonb_array_elements(c.branch_channel_ids) WITH ORDINALITY AS e(ids, n)),
                         '{}'::jsonb)
                ELSE '{}'::jsonb END
            WHERE c.node_channel_ids IS NULL
        $q$;
        ALTER TABLE workflow_changes DROP COLUMN branch_channel_ids;
    END IF;
END $$;
UPDATE workflow_changes SET node_channel_ids = '{}'::jsonb WHERE node_channel_ids IS NULL;
ALTER TABLE workflow_changes ALTER COLUMN node_channel_ids SET NOT NULL;

ALTER TABLE saved_query_runs DROP CONSTRAINT IF EXISTS saved_query_runs_upstream_kind_check;
ALTER TABLE saved_query_runs ADD CONSTRAINT saved_query_runs_upstream_kind_check
    CHECK (upstream_kind IS NULL OR upstream_kind IN ('pipeline', 'model', 'workflow')) NOT VALID;

COMMIT;

ALTER TABLE workflow_runs VALIDATE CONSTRAINT workflow_runs_status_check;
ALTER TABLE executions VALIDATE CONSTRAINT executions_workflow_run_id_fkey;
ALTER TABLE saved_query_runs VALIDATE CONSTRAINT saved_query_runs_upstream_kind_check;
