-- 133_workflow_branches.sql
-- Branches on a workflow's one query (internal/workflow, definition version 2).
--
-- A workflow now holds up to three branches on its query's result, each a
-- condition, a message and its own delivery channels, plus an optional
-- "otherwise". Branches are checked top to bottom and only the first that holds
-- sends. A version 1 definition (one condition, one message) reads as one
-- branch, so existing workflows and their runs need no rewrite.
--
--   - workflow_channels.branch: the 0-based branch a channel belongs to. Every
--     existing row is branch 0, the one branch a version 1 workflow has. The
--     same channel may serve two branches, so branch joins the primary key.
--   - workflow_runs.branch: the 0-based branch that sent, NULL when none held
--     or the run failed before deciding.
--   - workflow_changes: a change to a workflow's branches that Ask rsync.ai
--     proposed (plan_workflow_change, handlers/workflow_changes.go). It changes
--     nothing until an owner or admin opens it and clicks Apply, which saves it
--     through the same path as an edit, refused if the workflow changed since
--     the change was proposed (base_updated_at). Same shape and 24-hour expiry
--     as pipeline_plans (128). definition is the proposed definition as the
--     server checked it; it never reaches a model.
--
-- The primary-key swap locks workflow_channels, so lock_timeout bounds the wait
-- behind a running workflow: a blocked migration fails fast and the next
-- api-gateway start re-runs it. Same reasoning and timeout as 132.

SET LOCAL lock_timeout = '3s';

ALTER TABLE workflow_channels ADD COLUMN IF NOT EXISTS branch SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE workflow_channels DROP CONSTRAINT IF EXISTS workflow_channels_pkey;
ALTER TABLE workflow_channels
    ADD CONSTRAINT workflow_channels_pkey PRIMARY KEY (workflow_id, branch, channel_id);

ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS branch SMALLINT;

CREATE TABLE IF NOT EXISTS workflow_changes (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id       UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    workflow_id        UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    created_by         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_kind         VARCHAR(20) NOT NULL CHECK (actor_kind IN ('assistant', 'agent')),
    token_id           UUID,
    base_updated_at    TIMESTAMPTZ NOT NULL,
    definition         JSONB NOT NULL,
    branch_channel_ids JSONB NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    status             VARCHAR(20) NOT NULL DEFAULT 'proposed'
                       CHECK (status IN ('proposed', 'applied')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ NOT NULL,
    applied_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_workflow_changes_owner
    ON workflow_changes(workspace_id, created_by, created_at DESC);
