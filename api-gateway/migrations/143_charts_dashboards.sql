-- Migration 143: saved charts, dashboards and their tiles, and the dashboards Ask
-- rsync.ai or an agent token proposes (dashboards-design.md § 7.1, build step D2b).
--
-- A chart is one source, a saved query or a model (saved_queries), plus its settings:
-- the type, the columns, the aggregate, titles and a colour token per series
-- (internal/charts). rsync.ai writes the chart's SQL from them each time it is drawn;
-- no row, result or colour value is stored. source_columns are the source's column
-- names when the chart was saved, so a dashboard filter knows which tiles it applies to.
-- A chart whose source is deleted keeps its row with saved_query_id NULL and draws a
-- message, never a chart.
--
-- A dashboard holds tiles on a 12-column grid: a saved chart (bar, line, a scorecard,
-- a table…) or plain text. A chart on a dashboard cannot be deleted (RESTRICT); the API
-- names the dashboards instead. filters are the dashboard's date-range and value-list
-- filters, column names only; their values are bound parameters at draw time.
--
-- dashboard_proposals are as pipeline_plans (128): a draft that creates nothing, held
-- 24 hours for the person who asked, who saves it from the card in the thread. token_id
-- is not a foreign key, as in audit_logs.
--
-- Note: the migration runner wraps each migration in a transaction; no BEGIN/COMMIT.
-- New tables only. Idempotent.

CREATE TABLE IF NOT EXISTS charts (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            VARCHAR(200) NOT NULL,
    saved_query_id  UUID REFERENCES saved_queries(id) ON DELETE SET NULL,
    settings        JSONB NOT NULL,
    settings_version SMALLINT NOT NULL DEFAULT 1,
    source_columns  JSONB NOT NULL DEFAULT '[]',
    visibility      VARCHAR(20) NOT NULL DEFAULT 'private'
                    CHECK (visibility IN ('private', 'workspace')),
    proposed_by     VARCHAR(20) NOT NULL DEFAULT 'none'
                    CHECK (proposed_by IN ('none', 'assistant', 'agent')),
    created_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_charts_workspace
    ON charts(workspace_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_charts_saved_query
    ON charts(saved_query_id);

CREATE TABLE IF NOT EXISTS dashboards (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            VARCHAR(200) NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    filters         JSONB NOT NULL DEFAULT '[]',
    visibility      VARCHAR(20) NOT NULL DEFAULT 'private'
                    CHECK (visibility IN ('private', 'workspace')),
    proposed_by     VARCHAR(20) NOT NULL DEFAULT 'none'
                    CHECK (proposed_by IN ('none', 'assistant', 'agent')),
    created_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_dashboards_workspace
    ON dashboards(workspace_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS dashboard_tiles (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    dashboard_id    UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind            VARCHAR(10) NOT NULL CHECK (kind IN ('chart', 'text')),
    chart_id        UUID REFERENCES charts(id) ON DELETE RESTRICT,
    body            TEXT,
    x               SMALLINT NOT NULL CHECK (x BETWEEN 0 AND 11),
    y               SMALLINT NOT NULL CHECK (y BETWEEN 0 AND 999),
    w               SMALLINT NOT NULL CHECK (w BETWEEN 1 AND 12),
    h               SMALLINT NOT NULL CHECK (h BETWEEN 1 AND 24),
    CHECK (x + w <= 12),
    CHECK ((kind = 'chart' AND chart_id IS NOT NULL AND body IS NULL)
        OR (kind = 'text' AND chart_id IS NULL AND body IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_dashboard_tiles_dashboard
    ON dashboard_tiles(dashboard_id);
CREATE INDEX IF NOT EXISTS idx_dashboard_tiles_chart
    ON dashboard_tiles(chart_id);

CREATE TABLE IF NOT EXISTS dashboard_proposals (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id       UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_by         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_kind         VARCHAR(20) NOT NULL CHECK (actor_kind IN ('assistant', 'agent')),
    token_id           UUID,
    proposal           JSONB NOT NULL,
    status             VARCHAR(20) NOT NULL DEFAULT 'proposed'
                       CHECK (status IN ('proposed', 'saved')),
    saved_dashboard_id UUID REFERENCES dashboards(id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ NOT NULL,
    saved_at           TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_dashboard_proposals_owner
    ON dashboard_proposals(workspace_id, created_by, created_at DESC);
