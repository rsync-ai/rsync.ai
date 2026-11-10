-- Migration 147: chart settings by role (version 2), and each connection's calendar
-- (dashboards-design.md § 14.2, § 14.3, decision 14).
--
-- 1. A connection's first day of the week (1 Monday … 7 Sunday) and the first month of
--    its fiscal year (1 January … 12 December). A chart's week, quarter and year grains
--    start on them; ISO Monday and January by default, never the engine's or the
--    session's default.
--
-- 2. Every saved chart's settings rewritten from version 1 (x, y, series, measure,
--    columns and compare as keys of their own) to version 2 (each a role holding a list
--    of fields): charts.settings, each chart of a dashboard_proposals draft, and both
--    sides of a dashboard_changes change. The gateway's v1 reader
--    (internal/charts/settings_v1.go upgrade) does the same for one release, for a draft
--    written by the old gateway while the new one starts; shared/chart_settings_v1_v2.json
--    pins the two to each other (internal/db/migration_147_integration_test.go). A field
--    keeps only its non-empty attributes, as the gateway's JSON does. Kept dashboard
--    versions hold chart ids, never settings, so they need nothing. charts.updated_at is
--    left alone: a pending change compares it to the chart it was drafted on, and the
--    chart has not changed.
--
-- The runner wraps this file in one transaction; every step is safe to run again.

ALTER TABLE connections
    ADD COLUMN IF NOT EXISTS week_start SMALLINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS fiscal_year_start_month SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE connections DROP CONSTRAINT IF EXISTS connections_week_start_check;
ALTER TABLE connections ADD CONSTRAINT connections_week_start_check
    CHECK (week_start BETWEEN 1 AND 7);
ALTER TABLE connections DROP CONSTRAINT IF EXISTS connections_fiscal_year_start_month_check;
ALTER TABLE connections ADD CONSTRAINT connections_fiscal_year_start_month_check
    CHECK (fiscal_year_start_month BETWEEN 1 AND 12);

-- chart_field_v2 is f's attributes named in keys that are a non-empty string or a
-- non-empty list.
CREATE OR REPLACE FUNCTION pg_temp.chart_field_v2(f jsonb, keys text[]) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(jsonb_object_agg(k, f -> k), '{}'::jsonb)
    FROM unnest(keys) AS k
    WHERE (jsonb_typeof(f -> k) = 'string' AND f ->> k <> '')
       OR (jsonb_typeof(f -> k) = 'array' AND jsonb_array_length(f -> k) > 0)
$$;

-- chart_settings_v2 is s as version 2; anything but a version 1 object is returned as
-- it is.
CREATE OR REPLACE FUNCTION pg_temp.chart_settings_v2(s jsonb) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    roles jsonb := '{}'::jsonb;
    out   jsonb;
BEGIN
    IF jsonb_typeof(s) IS DISTINCT FROM 'object' OR (s ? 'version' AND s -> 'version' <> '1'::jsonb) THEN
        RETURN s;
    END IF;
    IF jsonb_typeof(s -> 'x') = 'object' THEN
        roles := roles || jsonb_build_object('x', jsonb_build_array(pg_temp.chart_field_v2(s -> 'x', ARRAY['column', 'title'])));
    END IF;
    IF jsonb_typeof(s -> 'y') = 'object' THEN
        roles := roles || jsonb_build_object('y', jsonb_build_array(pg_temp.chart_field_v2(s -> 'y', ARRAY['column', 'title'])));
    END IF;
    IF jsonb_typeof(s -> 'series') = 'object' THEN
        roles := roles || jsonb_build_object('series', jsonb_build_array(pg_temp.chart_field_v2(s -> 'series', ARRAY['column', 'colors'])));
    END IF;
    IF jsonb_typeof(s -> 'columns') = 'array' AND jsonb_array_length(s -> 'columns') > 0 THEN
        roles := roles || jsonb_build_object('columns', (
            SELECT jsonb_agg(pg_temp.chart_field_v2(jsonb_build_object('column', c), ARRAY['column']) ORDER BY n)
            FROM jsonb_array_elements(s -> 'columns') WITH ORDINALITY AS e(c, n)));
    END IF;
    IF jsonb_typeof(s -> 'measure') = 'object' THEN
        roles := roles || jsonb_build_object('measure', jsonb_build_array(pg_temp.chart_field_v2(s -> 'measure',
            ARRAY['column', 'aggregate', 'title', 'format', 'color'])));
    END IF;
    IF jsonb_typeof(s -> 'compare') = 'object' THEN
        roles := roles || jsonb_build_object('date', jsonb_build_array(pg_temp.chart_field_v2(s -> 'compare', ARRAY['column'])));
    END IF;
    out := jsonb_build_object('version', 2, 'type', COALESCE(s ->> 'type', ''), 'roles', roles);
    IF jsonb_typeof(s -> 'description') = 'string' AND s ->> 'description' <> '' THEN
        out := out || jsonb_build_object('description', s -> 'description');
    END IF;
    IF s -> 'stacked' = 'true'::jsonb THEN
        out := out || '{"stacked": true}'::jsonb;
    END IF;
    IF jsonb_typeof(s -> 'sort') = 'string' AND s ->> 'sort' <> '' THEN
        out := out || jsonb_build_object('sort', s -> 'sort');
    END IF;
    IF jsonb_typeof(s -> 'compare') = 'object' THEN
        out := out || jsonb_build_object('compare', jsonb_build_object('days',
            CASE WHEN jsonb_typeof(s -> 'compare' -> 'days') = 'number' THEN s -> 'compare' -> 'days' ELSE '0'::jsonb END));
    END IF;
    RETURN out;
END
$$;

UPDATE charts
SET settings = pg_temp.chart_settings_v2(settings), settings_version = 2
WHERE settings_version <> 2 OR settings -> 'version' IS DISTINCT FROM '2'::jsonb;

UPDATE dashboard_proposals p
SET proposal = jsonb_set(p.proposal, '{charts}', (
    SELECT jsonb_agg(CASE WHEN jsonb_typeof(e.c -> 'settings') = 'object'
                          THEN jsonb_set(e.c, '{settings}', pg_temp.chart_settings_v2(e.c -> 'settings'))
                          ELSE e.c END ORDER BY e.n)
    FROM jsonb_array_elements(p.proposal -> 'charts') WITH ORDINALITY AS e(c, n)))
WHERE jsonb_typeof(p.proposal -> 'charts') = 'array'
  AND EXISTS (SELECT 1 FROM jsonb_array_elements(p.proposal -> 'charts') AS x(c)
              WHERE jsonb_typeof(x.c -> 'settings') = 'object'
                AND x.c -> 'settings' -> 'version' IS DISTINCT FROM '2'::jsonb);

UPDATE dashboard_changes
SET change = jsonb_set(change, '{before,settings}', pg_temp.chart_settings_v2(change -> 'before' -> 'settings'))
WHERE jsonb_typeof(change -> 'before' -> 'settings') = 'object'
  AND change -> 'before' -> 'settings' -> 'version' IS DISTINCT FROM '2'::jsonb;

UPDATE dashboard_changes
SET change = jsonb_set(change, '{after,settings}', pg_temp.chart_settings_v2(change -> 'after' -> 'settings'))
WHERE jsonb_typeof(change -> 'after' -> 'settings') = 'object'
  AND change -> 'after' -> 'settings' -> 'version' IS DISTINCT FROM '2'::jsonb;

DROP FUNCTION pg_temp.chart_settings_v2(jsonb);
DROP FUNCTION pg_temp.chart_field_v2(jsonb, text[]);
