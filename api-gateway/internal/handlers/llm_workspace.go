package handlers

import (
	"context"
	"net/http"
)

// The workspace a model call is made for, carried from the request to the hop.
//
// llm-service, the planner and the tool-generator record every model call in an
// egress log line (llm-service src/egress/scope.py), and name its workspace
// from the X-Workspace-ID header of the request that caused it. The gateway is
// where that workspace is known: WorkspaceContextMiddleware resolves it
// (membership-checked) and puts it on the request context, and every request
// built for one of those services calls stampLLMWorkspace. Without it each
// line read workspace=-.

type llmWorkspaceKey struct{}

// withLLMWorkspace returns ctx carrying workspaceID for stampLLMWorkspace.
func withLLMWorkspace(ctx context.Context, workspaceID string) context.Context {
	if workspaceID == "" {
		return ctx
	}
	return context.WithValue(ctx, llmWorkspaceKey{}, workspaceID)
}

// llmWorkspaceFrom returns the workspace ctx carries, or "". Nil-safe.
func llmWorkspaceFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ws, _ := ctx.Value(llmWorkspaceKey{}).(string)
	return ws
}

// detachedLLMContext is a context for work that outlives the request (a
// goroutine that calls a model after the response is sent): no deadline and no
// cancellation from the request, but still the request's workspace.
func detachedLLMContext(ctx context.Context) context.Context {
	return withLLMWorkspace(context.Background(), llmWorkspaceFrom(ctx))
}

// stampLLMWorkspace sets X-Workspace-ID on a request bound for a service that
// makes model calls, from the workspace its context carries. Any value already
// on the request is dropped first, so only the checked workspace goes out.
func stampLLMWorkspace(req *http.Request) {
	req.Header.Del(workspaceHeader)
	if ws := llmWorkspaceFrom(req.Context()); ws != "" {
		req.Header.Set(workspaceHeader, ws)
	}
}
