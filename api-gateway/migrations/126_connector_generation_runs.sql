-- 126_connector_generation_runs.sql
-- One row per connector generation that writes to the registry, whoever
-- started it: an agent token (generate_connector), Ask rsync, the chat
-- "generate <name>" command, or the Generate page
-- (docs/internal/ask-rsync-and-mcp-roadmap.md §2.2, connector_generations.go).
--
-- A run starts 'running' and ends 'succeeded' or 'failed'. An agent and Ask
-- rsync run in the background and read their run back by id. The connection
-- form reads the latest succeeded run to say that an AI agent generated the
-- connector, and when.
--
-- The connector registry is shared by every workspace, so only one run per
-- connector name may be in flight at a time, across workspaces: the partial
-- unique index below. A row still 'running' after 20 minutes was interrupted
-- (a run's own limit is 15) and is marked failed: at startup, every 5 minutes,
-- and before each new run is admitted. token_id is not a foreign key, as in audit_logs, so a
-- deleted token's runs keep saying which token it was.

CREATE TABLE IF NOT EXISTS connector_generation_runs (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id           UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_kind        VARCHAR(20) NOT NULL,
    token_id          UUID,
    connector_name    VARCHAR(100) NOT NULL,
    source_url        TEXT,
    status            VARCHAR(20) NOT NULL DEFAULT 'running'
                      CHECK (status IN ('running', 'succeeded', 'failed')),
    version           VARCHAR(32),
    validation_passed BOOLEAN,
    tool_count        INT,
    error             TEXT,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_connector_generation_runs_workspace
    ON connector_generation_runs(workspace_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_connector_generation_runs_name
    ON connector_generation_runs(connector_name, finished_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS uq_connector_generation_runs_running_name
    ON connector_generation_runs(connector_name) WHERE status = 'running';
