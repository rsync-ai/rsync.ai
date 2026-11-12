-- 127_saved_query_layer.sql
-- Which medallion layer a model builds: 'silver' (cleaned, one row per key) or
-- 'gold' (business tables built from silver). NULL = not set. Bronze is never
-- stored here: a bronze table is one a pipeline writes, which the lineage graph
-- already knows, so a model cannot claim it.
--
-- A label only. It changes nothing about how a model runs or who may run it;
-- the Scheduled Queries list, the model page and the lineage graph show it and
-- filter on it. Set with the model's materialization (PUT
-- /explorer/saved/:id/materialization), so it carries the same admin gate.

ALTER TABLE saved_queries
    ADD COLUMN IF NOT EXISTS layer VARCHAR(16);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'saved_queries_layer_check'
    ) THEN
        ALTER TABLE saved_queries
            ADD CONSTRAINT saved_queries_layer_check
            CHECK (layer IS NULL OR layer IN ('silver', 'gold'));
    END IF;
END $$;
