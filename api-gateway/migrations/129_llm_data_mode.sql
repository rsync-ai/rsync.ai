-- 129_llm_data_mode.sql
-- Whether Ask rsync may read sample rows in this workspace
-- (docs/security/llm-egress.md, "Sample rows"). Two switches must both be on
-- before a model sees a single value:
--
--   * a platform admin approves a model entry (claude, chatgpt, gemini) in
--     Admin > Settings > AI models. That list is the instance_settings row
--     'llm_sample_rows_models', a comma-separated list of entries; no row,
--     or an empty value, approves none.
--   * the workspace owner turns it on here: 'sample_rows'.
--
-- 'names_only' is the default and what every existing workspace keeps: the
-- model gets names, types and ids, never a row value. api-gateway reads both
-- switches before it offers the sample_rows tool, and llm-service's egress
-- guard reads them again before each send; a failed read counts as off.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS llm_data_mode VARCHAR(20) NOT NULL DEFAULT 'names_only';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'workspaces_llm_data_mode_check'
    ) THEN
        ALTER TABLE workspaces
            ADD CONSTRAINT workspaces_llm_data_mode_check
            CHECK (llm_data_mode IN ('names_only', 'sample_rows'));
    END IF;
END $$;
