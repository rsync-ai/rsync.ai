-- 123_agent_tokens.sql
-- Tokens an AI agent (Claude Code, Cursor, ...) uses to call the agent gateway
-- at POST /api/agent/v1/mcp (docs/internal/agent-gateway-design.md §5).
--
-- A token acts as the user who made it, in ONE workspace, and can only narrow
-- what that user may do: every call re-checks the user's current membership and
-- role (agent_auth_middleware.go), so removing the user from the workspace or
-- deactivating the account stops the token on its next call. Only the sha256 of
-- the token is stored (shared/go/crypto HashSessionToken), the same as
-- user_sessions; token_prefix is the first characters, shown in the UI so a
-- person can tell two tokens apart. expires_at is required (30 days by default,
-- a year at most, enforced in agent_tokens.go).

CREATE TABLE IF NOT EXISTS agent_tokens (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          VARCHAR(100) NOT NULL,
    token_hash    VARCHAR(64) NOT NULL UNIQUE,
    token_prefix  VARCHAR(16) NOT NULL DEFAULT '',
    scopes        TEXT[] NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_tokens_user_workspace ON agent_tokens(user_id, workspace_id);

-- Who acted: a person in the UI (NULL, as every existing row) or an agent
-- token. token_id is not a foreign key so a revoked-then-deleted token's audit
-- rows keep saying which token it was.
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS actor_kind VARCHAR(20);
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS token_id UUID;
