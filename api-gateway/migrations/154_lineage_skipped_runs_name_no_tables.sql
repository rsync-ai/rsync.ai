-- Migration 154: a model run that was skipped names no table (Lineage page history and
-- the OpenLineage export; handlers/lineage_events.go lineageEventNamesTables).
--
-- A skipped run (saved_query_runs.status 'skipped', recorded as ABORT) ran nothing: it
-- read no table and wrote none. The recorder first named the tables of the model's SQL
-- for every run whatever its outcome, so those events show a skipped run as having read
-- and written, appear in the history of every table the SQL names, and leave in the
-- OpenLineage export as a run with datasets. The recorder now names none; this puts the
-- events already recorded in the same state.
--
-- sql_hash is left as it is: while an event names a SQL version, the version's column
-- lineage is kept (model_column_lineage, handlers/asset_column_lineage.go
-- saveColumnLineage), and the model's history still compares versions by it.
--
-- A cancelled pipeline run (ABORT, job_kind 'pipeline') is not touched: it may have
-- moved some tables before it stopped.
--
-- Safe to run again: a second run finds no event that still names a table.

UPDATE lineage_events
SET inputs = '[]'::jsonb, outputs = '[]'::jsonb, table_keys = '{}'
WHERE job_kind = 'model' AND event_type = 'ABORT'
  AND (inputs <> '[]'::jsonb OR outputs <> '[]'::jsonb OR cardinality(table_keys) > 0);
