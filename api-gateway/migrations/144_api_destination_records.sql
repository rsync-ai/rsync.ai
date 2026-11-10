-- Migration 144: the key map of an API destination (generator trust plan P5).
--
-- A REST API names a record by an id it makes up when the record is created; the
-- source names a row by its key. api_destination_records holds, per pipeline,
-- source table and destination table, which record each source row became, so a
-- re-run updates the record it made last time instead of creating a second one,
-- and a Reload deletes the records whose source row is gone. Written only by the
-- kafka-mcp-sink (api_dest_rows.go), for a destination whose get_capabilities
-- says record_ids.
--
--   source_table       the table the rows were read from; a Reload of one source
--                      table never deletes the records another one wrote
--   source_key         "k:" + sha256 of the row's key values as JSON, or "r:" +
--                      sha256 of the whole row when the source has no key
--   record_id          the id the API gave the record
--   last_execution_id  the last run that read the source row; a Reload deletes the
--                      records it did not stamp
--
-- Holds ids and hashes, never row values. Deleting the pipeline deletes its map;
-- the next pipeline that writes there creates new records.
--
-- Note: the migration runner wraps each migration in a transaction; no BEGIN/COMMIT.
-- New table only. Idempotent.

CREATE TABLE IF NOT EXISTS api_destination_records (
    pipeline_id        UUID        NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    source_table       TEXT        NOT NULL,
    destination_table  TEXT        NOT NULL,
    source_key         TEXT        NOT NULL,
    record_id          TEXT        NOT NULL,
    last_execution_id  TEXT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pipeline_id, source_table, destination_table, source_key)
);

COMMENT ON TABLE api_destination_records IS
    'Per API destination record: a hash of the source row it was written from and the id the API gave it. Written by the kafka-mcp-sink; a re-run updates by record_id, a Reload deletes records the run did not read.';
