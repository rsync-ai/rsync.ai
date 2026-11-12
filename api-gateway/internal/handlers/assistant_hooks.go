package handlers

import (
	"context"
	"net/http"

	"api-gateway/internal/db"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// What the in-app assistant (internal/assistant) needs from this package. The
// assistant lives in its own package because it imports agentgw, which
// imports this one.

const actorKindAssistant = "assistant"

// AssistantActor is the signed-in user as the assistant's tools see them: the
// same user, workspace and role the middleware pinned for REST, on the
// assistant channel, with the assistant's fixed scopes. So the assistant can
// read what that user can read through a token, and generate a connector only
// when that user is an admin or owner who asked for it with a URL
// (docs/internal/ask-rsync-and-mcp-roadmap.md § 2.2).
func AssistantActor(c *gin.Context) services.Actor {
	a := chatActor(c)
	a.Channel = services.ChannelAssistant
	a.Scopes = services.AssistantScopes
	return a
}

// LogAssistantToolCall writes the audit row for one tool the assistant ran,
// marked actor_kind=assistant (token_id NULL), so a person can tell their own
// clicks from what the assistant looked at on their behalf. Only this row is
// marked: the kind is cleared again before returning.
func LogAssistantToolCall(c *gin.Context, tool, resourceType, resourceID, outcome string, status int) {
	prev := c.GetString(ctxActorKind)
	c.Set(ctxActorKind, actorKindAssistant)
	defer c.Set(ctxActorKind, prev)
	logAudit(c, "assistant_tool_call", resourceType, resourceID, map[string]interface{}{
		"tool":    tool,
		"outcome": outcome,
		"status":  status,
	})
}

// MarkAssistantCall marks the context of a handler a tool of the assistant
// calls in-process (agentgw Options.Mark), so the rows that handler audits
// itself, such as the saved query create_model makes, say actor_kind=assistant
// as an agent token's say agent. The mark lives on that one handler call's
// context, never on the person's own request.
func MarkAssistantCall(sc *gin.Context) {
	sc.Set(ctxActorKind, actorKindAssistant)
}

// LogAudit writes one audit row as the request's user, for packages outside
// this one (the assistant's admin key routes).
func LogAudit(c *gin.Context, action, resourceType, resourceID string, details map[string]interface{}) {
	logAudit(c, action, resourceType, resourceID, details)
}

// UserCostQuotaOK reports whether userID has LLM-spend budget left this month.
// Like checkUserCostQuotaOK it allows on a DB error: llm-service enforces the
// quota on every completion.
func UserCostQuotaOK(ctx context.Context, userID string) bool {
	return checkUserCostQuotaOK(ctx, db.GetDB(), userID)
}

// RecordNLQueryUsage counts one NL→SQL query for the active workspace, as an
// Explorer generation does: the assistant calls it when a tool it wrote SQL for
// saved it (assistant.Options.MeterSQL). Display only and fail-open.
func RecordNLQueryUsage(c *gin.Context, connectionID string) {
	recordNLQueryUsage(c, connectionID)
}

// StampLLMWorkspace is stampLLMWorkspace for a hop built outside this package.
func StampLLMWorkspace(req *http.Request) { stampLLMWorkspace(req) }
