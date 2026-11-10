package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/shared/crypto"
	log "github.com/sirupsen/logrus"
)

// Agent tokens: made by a person in Settings, used by an AI agent to call the
// agent gateway (agent_auth_middleware.go). These routes sit in the ordinary
// /api/v1 group, so only a signed-in person (session + CSRF) can make, list or
// revoke one; an agent token cannot reach them, because AuthRequiredMiddleware
// only knows session tokens.
const (
	agentTokenDefaultDays  = 30
	agentTokenMaxDays      = 365
	agentTokenMaxLiveCount = 20
	agentTokenNameMax      = 100
	agentTokenPrefixLen    = 12
)

// AgentTokenView is a token as the list shows it. The token itself is never
// stored, so it can never be listed; only its first characters are.
type AgentTokenView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	// OwnerEmail and Mine let a workspace admin, who sees every token in the
	// workspace, tell their own tokens from a member's.
	OwnerEmail string `json:"owner_email"`
	Mine       bool   `json:"mine"`
}

type createAgentTokenRequest struct {
	Name          string   `json:"name"`
	ExpiresInDays *int     `json:"expires_in_days"`
	Scopes        []string `json:"scopes"`
}

// ListAgentTokens handles GET /api/v1/agent-tokens: the tokens in the active
// workspace, newest first, revoked and expired ones included so a person can
// see what an agent used to have. A member sees their own; a workspace admin or
// owner sees every member's, so they can see which agents reach the workspace
// and revoke one.
func ListAgentTokens(c *gin.Context) {
	userID, workspaceID, ok := agentTokenCaller(c)
	if !ok {
		return
	}
	database := db.GetDB()
	rows, err := database.Query(`
		SELECT t.id, t.name, t.token_prefix, array_to_string(t.scopes, ','), t.expires_at,
		       t.revoked_at, t.last_used_at, t.created_at, u.email, t.user_id = $1
		FROM agent_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.workspace_id = $2 AND (t.user_id = $1 OR $3)
		ORDER BY t.created_at DESC
		LIMIT 100
	`, userID, workspaceID, agentTokenAdmin(c))
	if err != nil {
		log.WithError(err).Warn("list agent tokens failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not list agent tokens"})
		return
	}
	defer rows.Close()

	tokens := []AgentTokenView{}
	for rows.Next() {
		var t AgentTokenView
		var scopes string
		var revoked, lastUsed sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenPrefix, &scopes, &t.ExpiresAt, &revoked, &lastUsed, &t.CreatedAt, &t.OwnerEmail, &t.Mine); err != nil {
			log.WithError(err).Warn("scan agent token failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not list agent tokens"})
			return
		}
		t.Scopes = splitScopes(scopes)
		if revoked.Valid {
			t.RevokedAt = &revoked.Time
		}
		if lastUsed.Valid {
			t.LastUsedAt = &lastUsed.Time
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not list agent tokens"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

// CreateAgentToken handles POST /api/v1/agent-tokens. The token is returned
// once, in this response, and only its sha256 is stored. It acts as the caller
// in the active workspace with the metadata:read scope, plus any scope in
// scopes (see agentTokenScopes), and expires in expires_in_days (30 by default,
// 365 at most). Any member may make one: it can never do more than the member
// can.
func CreateAgentToken(c *gin.Context) {
	userID, workspaceID, ok := agentTokenCaller(c)
	if !ok {
		return
	}
	var req createAgentTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Send a JSON body with a name"})
		return
	}
	name := cleanAgentTokenName(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Give the token a name, such as the agent or laptop that will use it"})
		return
	}
	days := agentTokenDefaultDays
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	if days < 1 || days > agentTokenMaxDays {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expires_in_days must be between 1 and 365"})
		return
	}
	scopes, status, msg := agentTokenScopes(req.Scopes, agentTokenAdmin(c))
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	token, err := newAgentToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not create the agent token"})
		return
	}
	id := uuid.NewString()
	expiresAt := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Second)
	prefix := token[:agentTokenPrefixLen]
	live, err := insertAgentTokenUnderCap(c.Request.Context(), userID, workspaceID,
		id, name, crypto.HashSessionToken(token), prefix, strings.Join(scopes, ","), expiresAt)
	if err != nil {
		log.WithError(err).Warn("create agent token failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not create the agent token"})
		return
	}
	if live >= agentTokenMaxLiveCount {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
			"You have %d live agent tokens in this workspace. Revoke one first.", agentTokenMaxLiveCount)})
		return
	}

	logAudit(c, "create_agent_token", "agent_token", id, map[string]interface{}{
		"name":       name,
		"scopes":     scopes,
		"expires_at": expiresAt.Format(time.RFC3339),
	})

	c.JSON(http.StatusCreated, gin.H{
		"id":           id,
		"name":         name,
		"token":        token,
		"token_prefix": prefix,
		"scopes":       scopes,
		"expires_at":   expiresAt,
		"workspace_id": workspaceID,
	})
}

// insertAgentTokenUnderCap stores the token unless the caller already has
// agentTokenMaxLiveCount live tokens in the workspace, and returns the live
// count it found: at the cap, nothing was stored. The advisory lock makes the
// count and the insert one step per (user, workspace), so two requests at once
// cannot both pass a count of 19.
func insertAgentTokenUnderCap(ctx context.Context, userID, workspaceID, id, name, hash, prefix, scopes string, expiresAt time.Time) (int, error) {
	tx, err := db.GetDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		"agent_tokens:"+userID+":"+workspaceID); err != nil {
		return 0, err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_tokens
		WHERE user_id = $1 AND workspace_id = $2 AND revoked_at IS NULL AND expires_at > NOW()
	`, userID, workspaceID).Scan(&live); err != nil {
		return 0, err
	}
	if live >= agentTokenMaxLiveCount {
		return live, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_tokens (id, user_id, workspace_id, name, token_hash, token_prefix, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, string_to_array($7, ','), $8)
	`, id, userID, workspaceID, name, hash, prefix, scopes, expiresAt); err != nil {
		return 0, err
	}
	return live, tx.Commit()
}

// RevokeAgentToken handles DELETE /api/v1/agent-tokens/:id. The token's owner,
// or a workspace admin or owner, revokes it; the next call with it is refused
// with 401. The row stays so the list and the audit log can still name it.
func RevokeAgentToken(c *gin.Context) {
	userID, workspaceID, ok := agentTokenCaller(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent token not found"})
		return
	}
	res, err := db.GetDB().Exec(`
		UPDATE agent_tokens SET revoked_at = NOW()
		WHERE id = $1 AND workspace_id = $3 AND (user_id = $2 OR $4) AND revoked_at IS NULL
	`, id, userID, workspaceID, agentTokenAdmin(c))
	if err != nil {
		log.WithError(err).Warn("revoke agent token failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not revoke the agent token"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent token not found"})
		return
	}
	agentTokenTouched.Delete(id)
	logAudit(c, "revoke_agent_token", "agent_token", id, map[string]interface{}{})
	c.JSON(http.StatusOK, gin.H{"id": id, "revoked": true})
}

// agentTokenCaller returns the signed-in person and their active workspace, or
// answers the request itself. An agent token never gets here (see above), but
// refuse one explicitly in case a route is ever mounted in the wrong group.
func agentTokenCaller(c *gin.Context) (string, string, bool) {
	if c.GetString(ctxActorKind) != "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "An agent token cannot manage agent tokens"})
		return "", "", false
	}
	userID := strings.TrimSpace(c.GetString("user_id"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Sign in first"})
		return "", "", false
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return "", "", false
	}
	if _, ok := requireWorkspaceRole(c, security.WSViewer); !ok {
		return "", "", false
	}
	if db.GetDB() == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not connected"})
		return "", "", false
	}
	return userID, workspaceID, true
}

// agentTokenAdmin reports whether the caller may see and revoke every token in
// the active workspace.
func agentTokenAdmin(c *gin.Context) bool {
	return security.WorkspaceRole(c.GetString(ctxWorkspaceRole)).Meets(security.WSAdmin)
}

// agentTokenScopes is the scope list a new token gets: metadata:read always,
// then each requested scope once. An unknown scope is refused, and only a
// workspace admin or owner may grant connectors:generate, models:write,
// workflows:write or dashboards:write (services.AdminScope): each is their
// agreement that an agent may act — add connectors to the shared catalog,
// create, schedule, run and pause models, or draft workflows and dashboards
// (docs/internal/ask-rsync-and-mcp-roadmap.md § 2.2, § 6). Each call still
// needs the admin role, so a token outlives a demotion but cannot use the scope.
// Any user may grant pipelines:plan: a plan creates nothing until they create
// the pipeline from it in rsync.ai, and proposing one still needs the member role.
func agentTokenScopes(requested []string, admin bool) ([]string, int, string) {
	scopes := []string{services.ScopeMetadataRead}
	for _, s := range requested {
		s = strings.TrimSpace(s)
		switch {
		case slices.Contains(scopes, s):
			continue
		case !services.KnownScope(s):
			return nil, http.StatusBadRequest, "Unknown scope. A token can carry " +
				strings.Join(services.TokenScopes, ", ") + "."
		case services.AdminScope(s) && !admin:
			return nil, http.StatusForbidden, "Only a workspace admin or owner can give a token the " +
				s + " scope."
		}
		scopes = append(scopes, s)
	}
	return scopes, 0, ""
}

func newAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return agentTokenPrefix + hex.EncodeToString(b), nil
}

func cleanAgentTokenName(raw string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > agentTokenNameMax {
		name = string(r[:agentTokenNameMax])
	}
	return name
}

func splitScopes(list string) []string {
	out := []string{}
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
