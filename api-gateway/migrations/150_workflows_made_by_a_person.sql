-- Migration 150: a workflow a person builds in the editor, with no model
-- (workflows-design.md § 0.8).
--
-- POST /api/v1/workflows (handlers/workflows.go CreateWorkflow) stores a draft
-- an owner or admin built by hand. Its proposed_by is 'none', as for a chart or
-- a dashboard made by hand (migration 143). Until now only Ask rsync.ai
-- ('assistant') or an agent token ('agent') drafted a workflow, and the column
-- refused anything else. Existing rows keep their value; the default stays
-- 'assistant', so an INSERT that names no proposer is read as before.
--
-- The runner wraps this file in one transaction; it is safe to run again.

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_proposed_by_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_proposed_by_check
    CHECK (proposed_by IN ('none', 'assistant', 'agent'));
