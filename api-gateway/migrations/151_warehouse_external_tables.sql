-- Migration 151: a warehouse's external tables and where their files are, for the
-- Lineage page (handlers/lake_external_tables.go).
--
-- A lake pipeline writes Parquet files to a bucket; a BigQuery external table reads
-- them; models read the external table. The asset graph links the pipeline to those
-- models through this row: an external table whose source URIs sit in a pipeline's
-- table folder is fed by that pipeline.
--
-- One row per warehouse connection, written each time its table list is read
-- (explorer_warehouse.go buildWarehouseSchemaIndex), and re-read in the background
-- when the Lineage page finds it more than an hour old. The list is metadata the
-- warehouse returns with each table (BigQuery's tables.get): no query is run and
-- no row value is stored.
--
-- tables   [{"schema", "name", "source_uris": [...], "source_uri_prefix"}]
-- read_at  when the warehouse was last listed.
--
-- The runner wraps this file in one transaction; it is safe to run again.

CREATE TABLE IF NOT EXISTS warehouse_external_tables (
    connection_id UUID        PRIMARY KEY REFERENCES connections(id) ON DELETE CASCADE,
    tables        JSONB       NOT NULL DEFAULT '[]',
    read_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
