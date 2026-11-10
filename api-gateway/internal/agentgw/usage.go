package agentgw

import (
	"net/http"

	"api-gateway/internal/services"
)

// usageOut is get_usage's answer: the workspace's plan and how much of it is
// used, counts and limits only. A null limit is one the plan does not set.
type usageOut struct {
	Plan             string `json:"plan"`
	PipelineLimit    *int   `json:"pipeline_limit"`
	PipelinesUsed    int    `json:"pipelines_used"`
	NLSQLQueryLimit  *int64 `json:"nl_sql_query_limit"`
	NLSQLQueriesUsed int64  `json:"nl_sql_queries_used"`
	MemberCount      int    `json:"member_count"`
}

// usageTools is get_usage. It reads the meters the create and query gates
// enforce (handlers.PlanUsageAs), never a member's e-mail or how much data the
// workspace moved.
func usageTools(h Handlers) []*Tool {
	return []*Tool{{
		Name:  "get_usage",
		Title: "Get the plan and its usage",
		Description: "The workspace's plan: its name, its pipeline limit and pipelines used, its NL→SQL query limit " +
			"and queries used this month, and how many members the workspace has. A null limit is unlimited. " +
			"Counts only, no member's e-mail. Rules: read_guide('pipelines').",
		Guide:        "pipelines",
		Action:       services.ActionUsageRead,
		ResourceType: "workspace",
		Output:       usageOut{},
		run: func(call *toolCall) (any, error) {
			return getUsage(call, h)
		},
	}}
}

func getUsage(call *toolCall, h Handlers) (any, error) {
	if h.PlanUsage == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Plan usage is not available on this rsync.ai."}
	}
	u, err := h.PlanUsage(call.c, call.s.opts.Actor(call.c))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	return usageOut{
		Plan:             ident(u.Plan),
		PipelineLimit:    u.PipelineLimit,
		PipelinesUsed:    u.PipelinesUsed,
		NLSQLQueryLimit:  u.QueryLimit,
		NLSQLQueriesUsed: u.QueriesUsed,
		MemberCount:      u.Members,
	}, nil
}
