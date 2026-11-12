-- 128_pipeline_plans.sql
-- One row per pipeline Ask rsync or an agent token proposed
-- (plan_pipeline, handlers/pipeline_plans.go). A plan creates nothing and moves
-- no data: it is the source, destination, mode, tables and schedule the server
-- checked, held until the person who asked opens it in rsync and clicks Create.
-- Creating the pipeline is a REST call with that person's session, checked as
-- pipeline.create and pipeline.run; no agent token can apply a plan.
--
-- A plan is 'proposed' until applied and expires 24 hours after it was made.
-- Applying claims the row ('applied') before the pipeline is created, so two
-- clicks cannot create two pipelines; a failed create puts it back. plan holds
-- names, types and ids only, never a row value. token_id is not a foreign key,
-- as in audit_logs, so a deleted token's plans keep saying which token it was.

CREATE TABLE IF NOT EXISTS pipeline_plans (
    id                        UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id              UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_by                UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_kind                VARCHAR(20) NOT NULL CHECK (actor_kind IN ('assistant', 'agent')),
    token_id                  UUID,
    source_connection_id      UUID NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    destination_connection_id UUID NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    plan                      JSONB NOT NULL,
    status                    VARCHAR(20) NOT NULL DEFAULT 'proposed'
                              CHECK (status IN ('proposed', 'applied')),
    applied_pipeline_id       UUID REFERENCES pipelines(id) ON DELETE SET NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at                TIMESTAMPTZ NOT NULL,
    applied_at                TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_pipeline_plans_owner
    ON pipeline_plans(workspace_id, created_by, created_at DESC);
