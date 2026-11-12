-- Migration 142: the tables a connection could read when it was last checked
-- (generator-trust plan, phase 3b).
--
-- A connection test checks one endpoint (the auth check). Every other table of an API
-- connector can still answer 404, or a shape its connector does not parse, and that
-- was found only on the pipeline's first run. Saving an API connection, or "Check
-- tables" on its page, now reads one page of every table (orchestrator CheckTables,
-- no row kept) and stores the answer here until the next check:
--   {"checked_at": "...", "tables": [{"name", "status", "error"?, "missing_settings"?,
--    "warnings"?}], "ok": n, "failed": n, "needs_setting": n, "not_checked": n}
-- or {"checked_at": "...", "error": "..."} when the connector could not list its tables.
-- NULL: never checked (not an API connector, or saved before 142).
--
-- A check is not an edit, so the column joins 109's test-only columns: an UPDATE that
-- changes only test results keeps the connection's updated_at.
--
-- Note: the migration runner wraps each migration in a transaction; no BEGIN/COMMIT.
-- Idempotent.

ALTER TABLE connections ADD COLUMN IF NOT EXISTS last_table_check JSONB;

CREATE OR REPLACE FUNCTION connections_touch_updated_at() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    test_only_columns text[] := ARRAY['last_tested_at', 'last_test_status', 'last_test_error', 'last_table_check', 'updated_at'];
BEGIN
    IF (to_jsonb(NEW) - test_only_columns) IS NOT DISTINCT FROM (to_jsonb(OLD) - test_only_columns) THEN
        NEW.updated_at = OLD.updated_at;
    ELSE
        NEW.updated_at = NOW();
    END IF;
    RETURN NEW;
END;
$$;
