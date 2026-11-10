-- Migration 145: a change to one dashboard tile that Ask rsync.ai or an agent token
-- proposes, and every saved version of a dashboard (dashboards-design.md § 7.6, § 7.7).
--
-- dashboard_changes are as dashboard_proposals (143): a draft that changes nothing,
-- held 24 hours for the person who asked, who applies it from the card. It names one
-- tile of one dashboard and that tile's chart; change holds the tile's chart and size
-- before and after, and the tiles moved down to make room. base_updated_at is the
-- dashboard's updated_at when the change was drafted: a dashboard saved since makes the
-- change out of date, never applied over the newer one. tile_id and chart_id are not
-- foreign keys: a save re-creates a dashboard's tiles, and the stale check guards them.
-- applied_chart_id is the chart the change wrote, a copy when the chart was also on
-- another dashboard (copied).
--
-- dashboard_versions keep a dashboard as it was before each save: by hand in the
-- editor, from a change (ask) or from a restore. saved_by, how and created_at are that
-- save's, so version N reads "the dashboard before saved_by's save". It holds the
-- board's own text, filters and tiles (each chart by id, never its settings), so a
-- restore puts back the board and each chart is drawn as it is now. Versions are
-- append-only and kept while the dashboard exists. No row, result or colour value is
-- stored.
--
-- Note: the migration runner wraps each migration in a transaction; no BEGIN/COMMIT.
-- New tables only. Idempotent.

CREATE TABLE IF NOT EXISTS dashboard_changes (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id     UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    dashboard_id     UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    tile_id          UUID NOT NULL,
    chart_id         UUID NOT NULL,
    created_by       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_kind       VARCHAR(20) NOT NULL CHECK (actor_kind IN ('assistant', 'agent')),
    token_id         UUID,
    base_updated_at  TIMESTAMPTZ NOT NULL,
    change           JSONB NOT NULL,
    summary          TEXT NOT NULL DEFAULT '',
    status           VARCHAR(20) NOT NULL DEFAULT 'proposed'
                     CHECK (status IN ('proposed', 'applied')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ NOT NULL,
    applied_at       TIMESTAMPTZ,
    applied_chart_id UUID,
    copied           BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_dashboard_changes_owner
    ON dashboard_changes (workspace_id, created_by, created_at DESC);

CREATE TABLE IF NOT EXISTS dashboard_versions (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    dashboard_id  UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version       INTEGER NOT NULL CHECK (version >= 1),
    name          VARCHAR(200) NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    filters       JSONB NOT NULL DEFAULT '[]',
    visibility    VARCHAR(20) NOT NULL CHECK (visibility IN ('private', 'workspace')),
    tiles         JSONB NOT NULL DEFAULT '[]',
    saved_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    how           VARCHAR(10) NOT NULL CHECK (how IN ('edit', 'ask', 'restore')),
    change_id     UUID,
    restored_from INTEGER,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (dashboard_id, version)
);
