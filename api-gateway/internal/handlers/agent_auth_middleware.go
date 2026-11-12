package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	log "github.com/sirupsen/logrus"
)

// Context keys the agent path sets on top of the ones a session sets.
const (
	ctxActorKind   = "actor_kind"
	ctxTokenID     = "token_id"
	ctxTokenScopes = "agent_token_scopes"

	actorKindAgent = "agent"

	// agentTokenPrefix starts every agent token, so a person (or a secret
	// scanner) can tell one from a session token at a glance.
	agentTokenPrefix = "rsat_"
)

var (
	// 120 tool calls a minute per token. An agent loops faster than a person
	// clicks, but every call is a DB read against the same tables the UI uses.
	agentTokenLimiter = newFixedWindowLimiter(120, time.Minute)

	// agentTokenTouched throttles the last_used_at write to once a minute per
	// token and process; the SQL guard below covers the other replica.
	agentTokenTouched sync.Map // token id -> time.Time

	// agentRefusalAudited keeps a refused caller's retries from becoming one
	// audit write each: one row a minute per token (or, with no live token,
	// per client IP) and refusal code, per process.
	agentRefusalAudited = newFixedWindowLimiter(1, time.Minute)
)

// AgentTokenMiddleware authenticates the agent gateway (POST /api/agent/v1/mcp)
// with an agent token made in Settings (agent_tokens.go). It reads ONLY the
// Authorization header: never the session cookie (so a browser cannot be made
// to call the gateway with a person's session) and never the dev X-User-ID
// fallback.
//
// The token acts as the user who made it, in the one workspace it was made
// for. Every call re-checks that the token is live, the user is active, and the
// user is still a member of that workspace, and pins the member's CURRENT role:
// demote the user and their token is demoted on its next call. X-Workspace-ID
// is ignored. The global role is always "user", so no token is ever a platform
// admin, whatever the person who made it is.
func AgentTokenMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		database := db.GetDB()
		if database == nil {
			refuseAgentCall(c, agentCaller{}, http.StatusServiceUnavailable, "database_unavailable", "The database is not connected.")
			return
		}

		token := agentBearerToken(c.GetHeader("Authorization"))
		if token == "" {
			refuseAgentCall(c, agentCaller{}, http.StatusUnauthorized, "missing_agent_token", "Send an agent token as Authorization: Bearer rsat_...")
			return
		}

		var tokenID, userID, workspaceID, email, status, scopeList string
		err := database.QueryRow(`
			SELECT t.id, t.user_id, t.workspace_id, array_to_string(t.scopes, ','),
			       u.email, COALESCE(u.status, 'active')
			FROM agent_tokens t
			JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND t.expires_at > NOW()
		`, crypto.HashSessionToken(token)).Scan(&tokenID, &userID, &workspaceID, &scopeList, &email, &status)
		if err == sql.ErrNoRows {
			refuseAgentCall(c, agentCaller{}, http.StatusUnauthorized, "invalid_agent_token", "The agent token is unknown, revoked or expired.")
			return
		}
		if err != nil {
			log.WithError(err).Warn("agent token lookup failed")
			refuseAgentCall(c, agentCaller{}, http.StatusInternalServerError, "db_error", "Could not check the agent token.")
			return
		}
		if strings.EqualFold(strings.TrimSpace(status), "deactivated") {
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusUnauthorized, "account_deactivated", "The account that made this token is deactivated.")
			return
		}

		role, err := lookupWorkspaceMembership(database, workspaceID, userID)
		if err == sql.ErrNoRows {
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusUnauthorized, "not_a_member", "The account that made this token is no longer in its workspace.")
			return
		}
		if err != nil {
			log.WithError(err).Warn("agent token membership lookup failed")
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusInternalServerError, "db_error", "Could not check the agent token.")
			return
		}
		if !role.Meets(security.WSViewer) {
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusForbidden, "unknown_workspace_role", "The token's workspace role is not one rsync.ai knows.")
			return
		}

		scopes := knownScopes(scopeList)
		if len(scopes) == 0 {
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusForbidden, "no_known_scope", "The token carries no scope this server knows.")
			return
		}

		if ok, wait := agentTokenLimiter.Allow(tokenID, time.Now()); !ok {
			retry := int(wait.Seconds())
			if retry < 1 {
				retry = 1
			}
			c.Header("Retry-After", strconv.Itoa(retry))
			refuseAgentCall(c, agentCaller{tokenID, userID, workspaceID}, http.StatusTooManyRequests, "rate_limit_exceeded", "Too many calls with this token. Wait and retry.")
			return
		}

		c.Set("user_id", userID)
		bindCallerToRequest(c, userID)
		c.Set("user_email", email)
		c.Set("user_role", "user")
		c.Set(ctxWorkspaceID, workspaceID)
		c.Set(ctxWorkspaceRole, string(role))
		c.Set(ctxActorKind, actorKindAgent)
		c.Set(ctxTokenID, tokenID)
		c.Set(ctxTokenScopes, scopes)

		touchAgentToken(database, tokenID, time.Now())
		c.Next()
	}
}

// agentActor is the agent caller as services.Authorize sees it.
func agentActor(c *gin.Context) services.Actor {
	scopes, _ := c.Get(ctxTokenScopes)
	list, _ := scopes.([]string)
	return services.Actor{
		UserID:      c.GetString("user_id"),
		WorkspaceID: activeWorkspaceID(c),
		Role:        security.WorkspaceRole(c.GetString(ctxWorkspaceRole)),
		Channel:     services.ChannelAgent,
		Scopes:      list,
	}
}

// AgentActor exposes agentActor to the agent gateway package.
func AgentActor(c *gin.Context) services.Actor { return agentActor(c) }

// AgentTokenSpend charges one more tool call to the request's token. The
// middleware charges one per HTTP request; the gateway calls this for each
// further tools/call a JSON-RPC batch carries, so a batch costs what its calls
// would cost one by one. False when the token is out of calls, or the request
// carries no token.
func AgentTokenSpend(c *gin.Context) bool {
	tokenID := c.GetString(ctxTokenID)
	if tokenID == "" {
		return false
	}
	ok, _ := agentTokenLimiter.Allow(tokenID, time.Now())
	return ok
}

// agentBearerToken returns the rsat_ token from an Authorization header, or ""
// for anything else — including a session token, which never works here.
func agentBearerToken(header string) string {
	h := strings.TrimSpace(header)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	t := strings.TrimSpace(h[7:])
	if !strings.HasPrefix(t, agentTokenPrefix) || len(t) <= len(agentTokenPrefix) {
		return ""
	}
	return t
}

func knownScopes(list string) []string {
	var out []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if services.KnownScope(s) {
			out = append(out, s)
		}
	}
	return out
}

// touchAgentToken records when a token was last used, at most once a minute.
// Best effort: a failed write never fails the call.
func touchAgentToken(database *sql.DB, tokenID string, now time.Time) {
	if prev, ok := agentTokenTouched.Load(tokenID); ok {
		if now.Sub(prev.(time.Time)) < time.Minute {
			return
		}
	}
	agentTokenTouched.Store(tokenID, now)
	if _, err := database.Exec(`
		UPDATE agent_tokens SET last_used_at = NOW()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < NOW() - INTERVAL '1 minute')
	`, tokenID); err != nil {
		log.WithError(err).Debug("agent token last_used_at update failed (ignored)")
	}
}

// agentCaller is who a refused call came as, as far as the gateway could tell:
// nobody for a missing token, or one that is unknown, revoked or expired.
type agentCaller struct{ tokenID, userID, workspaceID string }

// refuseAgentCall answers an auth failure and records it in the audit log as
// agent_auth_refused, as the token's user and workspace when the gateway could
// tell them, with the refusal code and status in its details. A refused caller
// that retries writes one row a minute per token, or per client IP when no live
// token named it, and code (agentRefusalAudited), not one row a retry.
func refuseAgentCall(c *gin.Context, who agentCaller, status int, code, message string) {
	key := who.tokenID
	if key == "" {
		key = "ip:" + c.ClientIP()
	}
	if ok, _ := agentRefusalAudited.Allow(key+"|"+code, time.Now()); ok {
		if who.userID != "" {
			c.Set("user_id", who.userID)
		}
		if who.workspaceID != "" {
			c.Set(ctxWorkspaceID, who.workspaceID)
		}
		if who.tokenID != "" {
			c.Set(ctxTokenID, who.tokenID)
		}
		c.Set(ctxActorKind, actorKindAgent)
		logAudit(c, "agent_auth_refused", "agent_token", who.tokenID, map[string]interface{}{
			"code":   code,
			"status": status,
		})
	}
	agentAuthAbort(c, status, code, message)
}

// agentAuthAbort answers an auth failure. The body is plain JSON, not
// JSON-RPC: the request never reached the MCP server.
func agentAuthAbort(c *gin.Context, status int, code, message string) {
	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", `Bearer realm="rsync-agent-gateway"`)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": code, "message": message})
}

// LogAgentToolCall records one agent gateway tool call in the audit log as the
// user the token acts for, with actor_kind "agent" and the token id
// (logAudit reads both from the context). resourceID is a uuid or "": the
// column is a uuid, and the gateway only passes ids it has already parsed.
func LogAgentToolCall(c *gin.Context, tool, resourceType, resourceID, outcome string, status int) {
	logAudit(c, "agent_tool_call", resourceType, resourceID, map[string]interface{}{
		"tool":    tool,
		"outcome": outcome,
		"status":  status,
	})
}
