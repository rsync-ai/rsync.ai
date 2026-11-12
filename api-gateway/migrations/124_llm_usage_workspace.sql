-- 124_llm_usage_workspace.sql
-- The LLM cost ledger names the workspace each call was for.
--
-- llm_usage_events (057) was keyed by user and pipeline only, so per-workspace
-- LLM spend could not be read for a user in several workspaces, or for a call
-- with no pipeline. Its sibling ledgers data_transfer_usage_events (073) and
-- nl_query_usage_events (075) already carry workspace_id.
--
-- llm-service writes it from the request's X-Workspace-ID (src/utils/llm_cost.py
-- record_usage), which the api-gateway stamps from the membership-checked
-- workspace. Nullable with no foreign key, on purpose: a call made outside any
-- workspace still records its cost, spend history outlives a deleted workspace,
-- and an unknown id can never fail the INSERT (the user's quota charge runs in
-- the same try block). Rows written before this migration stay NULL.
--
-- ADD COLUMN and the index build lock the table, so lock_timeout bounds the
-- wait behind llm-service's writer: a blocked migration fails fast and the next
-- api-gateway start re-runs it, instead of queueing every cost row behind it.
-- Same reasoning and timeout as 098 / 121. The runner owns the transaction.

SET LOCAL lock_timeout = '3s';

ALTER TABLE llm_usage_events ADD COLUMN IF NOT EXISTS workspace_id UUID;

CREATE INDEX IF NOT EXISTS idx_llm_usage_events_workspace_created
    ON llm_usage_events (workspace_id, created_at)
    WHERE workspace_id IS NOT NULL;
