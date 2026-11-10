-- 130_workflows.sql
-- Workflows, slice 1 (docs/internal/workflows-design.md § 0): a trigger, one
-- read-only SQL query against a workspace connection, a condition on its
-- result, and a message sent to one or more delivery channels. A workflow
-- never moves data between connections; that is a pipeline's job.
--
-- Ask rsync.ai (or an agent token) drafts a workflow with the plan_workflow tool.
-- A draft runs nothing and sends nothing: the person tests it and turns it on
-- in rsync.ai (handlers/workflows.go). Each run re-checks the run-as user's role
-- and re-validates the SQL as a single read (handlers/workflow_runtime.go).

-- A delivery channel is a named place a workflow sends to: one Slack channel
-- (its incoming webhook, which is the credential, so it is stored encrypted
-- like notification_channel_settings.slack_webhook_encrypted) or a list of
-- email addresses. Only a workspace owner or admin adds one; a workflow picks
-- channels by name. "Channel", not "destination": in rsync.ai a destination is
-- where a pipeline writes data.
CREATE TABLE IF NOT EXISTS delivery_channels (
    id                      UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id            UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                    VARCHAR(100) NOT NULL,
    kind                    VARCHAR(20) NOT NULL CHECK (kind IN ('slack', 'email')),
    slack_webhook_encrypted TEXT,
    email_recipients        TEXT[] NOT NULL DEFAULT '{}',
    created_by              UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT delivery_channels_kind_fields CHECK (
        (kind = 'slack' AND slack_webhook_encrypted IS NOT NULL AND cardinality(email_recipients) = 0)
        OR (kind = 'email' AND slack_webhook_encrypted IS NULL
            AND cardinality(email_recipients) BETWEEN 1 AND 20)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_channels_name
    ON delivery_channels(workspace_id, lower(name));

-- definition holds the trigger, the SQL, the condition and the message
-- (workflow.Definition). connection_id and trigger_pipeline_id mirror it as
-- columns so a deleted connection or pipeline is seen (SET NULL) instead of
-- deleting the workflow under the person. definition holds no row value.
CREATE TABLE IF NOT EXISTS workflows (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                VARCHAR(200) NOT NULL,
    status              VARCHAR(20) NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft', 'active', 'paused')),
    paused_reason       TEXT,
    definition          JSONB NOT NULL,
    trigger_kind        VARCHAR(20) NOT NULL
                        CHECK (trigger_kind IN ('schedule', 'after_pipeline', 'manual')),
    connection_id       UUID REFERENCES connections(id) ON DELETE SET NULL,
    trigger_pipeline_id UUID REFERENCES pipelines(id) ON DELETE SET NULL,
    next_run_at         TIMESTAMPTZ,
    run_as_user_id      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_by          UUID REFERENCES users(id) ON DELETE SET NULL,
    proposed_by         VARCHAR(20) NOT NULL DEFAULT 'assistant'
                        CHECK (proposed_by IN ('assistant', 'agent')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_workflows_workspace
    ON workflows(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_workflows_due
    ON workflows(next_run_at) WHERE status = 'active' AND trigger_kind = 'schedule';
CREATE INDEX IF NOT EXISTS idx_workflows_after_pipeline
    ON workflows(trigger_pipeline_id) WHERE status = 'active' AND trigger_kind = 'after_pipeline';

-- Which channels a workflow sends to. RESTRICT: a channel in use cannot be
-- deleted, so a workflow never silently loses where it sends.
CREATE TABLE IF NOT EXISTS workflow_channels (
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    channel_id  UUID NOT NULL REFERENCES delivery_channels(id) ON DELETE RESTRICT,
    position    INT NOT NULL DEFAULT 0,
    PRIMARY KEY (workflow_id, channel_id)
);

CREATE INDEX IF NOT EXISTS idx_workflow_channels_channel ON workflow_channels(channel_id);

-- One row per run. It records outcomes only: the row count, whether the
-- condition held, and each delivery's result. Never a row value.
-- trigger_ref is the pipeline execution an after_pipeline run follows; the
-- partial unique index makes a re-delivered completion event fire once.
CREATE TABLE IF NOT EXISTS workflow_runs (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    trigger       VARCHAR(20) NOT NULL CHECK (trigger IN ('schedule', 'after_pipeline', 'manual')),
    trigger_ref   TEXT,
    status        VARCHAR(20) NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'succeeded', 'failed')),
    condition_met BOOLEAN,
    row_count     INT,
    deliveries    JSONB NOT NULL DEFAULT '[]',
    notes         TEXT[] NOT NULL DEFAULT '{}',
    error         TEXT,
    started_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_workflow_runs_workflow
    ON workflow_runs(workflow_id, started_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_runs_after_pipeline_once
    ON workflow_runs(workflow_id, trigger_ref) WHERE trigger = 'after_pipeline';
CREATE INDEX IF NOT EXISTS idx_workflow_runs_running
    ON workflow_runs(started_at) WHERE status = 'running';
