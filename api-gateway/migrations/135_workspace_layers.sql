-- 135_workspace_layers.sql
-- A workspace's own model layers (handlers/workspace_layers.go), replacing the
-- fixed 'silver'/'gold' label of 127.
--
-- An owner or admin adds, renames, orders and deletes them in Workspace
-- settings; a model points at one (saved_queries.layer_id) or at none. A layer
-- is still a label only: it changes nothing about how a model runs or who may
-- run it. A table a pipeline writes is not a layer: the lineage graph marks it
-- "Pipeline table" from the pipelines themselves.
--
--   - workspace_layers: name is unique per workspace ignoring case, as model
--     names are (084). position orders the layers, 0 first, densely; it is
--     DEFERRABLE so a reorder can rewrite every position in one transaction.
--   - saved_queries.layer_id: the composite foreign key keeps a model on a
--     layer of its own workspace. No ON DELETE action: deleting a layer clears
--     its models first, in the handler's transaction, because SET NULL on a
--     composite key would null workspace_id too before PostgreSQL 15.
--   - Backfill: every workspace with a silver or gold model gets both a
--     "Silver" (0) and a "Gold" (1) layer, and those models point at them.
--     Other workspaces start with none. updated_at is the saved-query edit
--     concurrency token (saved_queries.go), so its trigger is off for the
--     backfill: an editor open on a model must not see a change nobody made.
--   - saved_queries.layer stays, unwritten. A rollback reads the labels as they
--     were when this ran: a layer set afterwards is not written back to it. A
--     later migration drops it once this has run everywhere.
--
-- ADD COLUMN locks saved_queries (ACCESS EXCLUSIVE, reads too) and the new
-- table's keys lock users and workspaces against writes, all until commit, so
-- lock_timeout bounds the wait behind a running model: a blocked migration
-- fails fast and the next api-gateway start re-runs it. Same reasoning and
-- timeout as 132 and 133.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS workspace_layers (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL
                 CHECK (char_length(name) BETWEEN 1 AND 40 AND name = btrim(name)),
    description  TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 200),
    position     INTEGER NOT NULL CHECK (position >= 0),
    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT workspace_layers_workspace_id_id_key UNIQUE (workspace_id, id),
    CONSTRAINT workspace_layers_position_key UNIQUE (workspace_id, position)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_layers_name
    ON workspace_layers (workspace_id, lower(name));

DROP TRIGGER IF EXISTS update_workspace_layers_updated_at ON workspace_layers;
CREATE TRIGGER update_workspace_layers_updated_at
BEFORE UPDATE ON workspace_layers
FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- The column, its key and the backfill happen once: in the transaction that
-- adds layer_id. Should this file run again (a restore that lost its
-- schema_migrations row), the backfill must not bring back a layer an admin
-- renamed or deleted since, nor put back a model they took out of one: the 127
-- label it reads is never cleared.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = 'saved_queries'::regclass AND attname = 'layer_id' AND NOT attisdropped
    ) THEN
        RETURN;
    END IF;

    ALTER TABLE saved_queries ADD COLUMN layer_id UUID;
    ALTER TABLE saved_queries
        ADD CONSTRAINT saved_queries_layer_id_fkey
        FOREIGN KEY (workspace_id, layer_id)
        REFERENCES workspace_layers (workspace_id, id);

    INSERT INTO workspace_layers (workspace_id, name, description, position)
    SELECT w.workspace_id, l.name, l.description, l.position
    FROM (
        SELECT DISTINCT workspace_id FROM saved_queries WHERE layer IN ('silver', 'gold')
    ) w
    CROSS JOIN (VALUES
        ('Silver', 'Cleaned from one pipeline table: current rows, typed, deduplicated', 0),
        ('Gold',   'Joined or aggregated from silver tables, ready for reporting',       1)
    ) AS l(name, description, position)
    ON CONFLICT (workspace_id, lower(name)) DO NOTHING;

    ALTER TABLE saved_queries DISABLE TRIGGER update_saved_queries_updated_at;

    UPDATE saved_queries sq
    SET layer_id = wl.id
    FROM workspace_layers wl
    WHERE wl.workspace_id = sq.workspace_id
      AND lower(wl.name) = sq.layer;

    ALTER TABLE saved_queries ENABLE TRIGGER update_saved_queries_updated_at;
END $$;

-- Serves the foreign key's check when a layer is deleted, and the per-layer
-- model counts the settings card shows.
CREATE INDEX IF NOT EXISTS idx_saved_queries_layer_id
    ON saved_queries (workspace_id, layer_id) WHERE layer_id IS NOT NULL;

COMMENT ON TABLE  workspace_layers IS 'A workspace''s own model layers, in order; a label on a model, nothing more';
COMMENT ON COLUMN saved_queries.layer_id IS 'The workspace layer this model is labelled with; NULL = none';
COMMENT ON COLUMN saved_queries.layer IS 'Superseded by layer_id (135); unwritten, kept for a rollback';
