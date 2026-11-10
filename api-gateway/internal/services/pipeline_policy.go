package services

import (
	"errors"
	"fmt"
	"slices"

	"api-gateway/internal/security"
)

// Action names something a caller asks rsync.ai to do on a workspace's behalf. The
// chat path and the REST path ask the same question about the same Action, so a
// role rule cannot hold on one channel and not the other (KI-CHAT-SKIPS-
// WORKSPACE-ROLE: chat created, ran and generated with no role check at all).
type Action string

const (
	ActionPipelineCreate    Action = "pipeline.create"
	ActionPipelineRun       Action = "pipeline.run"
	ActionConnectorGenerate Action = "connector.generate"
	// ActionPipelinePlan stores a proposed pipeline that creates nothing and
	// moves no data: Ask rsync.ai or an agent proposes it, and the signed-in person
	// creates the pipeline from it in rsync.ai (handlers/pipeline_plans.go). It is
	// member-level, as creating the pipeline is. Applying a plan is a REST call
	// with a session, checked as ActionPipelineCreate and ActionPipelineRun.
	ActionPipelinePlan Action = "pipeline.plan"

	// Read-only actions. REST gates these at viewer; they are named here so the
	// agent gateway asks the same table (docs/internal/agent-gateway-design.md).
	ActionConnectionRead Action = "connection.read"
	ActionPipelineRead   Action = "pipeline.read"
	ActionRunRead        Action = "run.read"

	// Models (saved queries that build a table). Reading one is viewer-level, as
	// on REST. Writing one — creating a model, its schedule, a run, a pause or a
	// resume — is admin-level, the REST gate (handlers.modelRunMinRole): a model
	// rebuilds a table unattended, which is a DDL act.
	ActionModelRead  Action = "model.read"
	ActionModelWrite Action = "model.write"

	// Workflows (handlers/workflows.go): a trigger, one read-only query, a
	// condition and a message to a delivery channel. Reading one is
	// viewer-level. Writing one (drafting, testing, turning on, running,
	// deleting) is admin-level, as a model is: a workflow runs a query
	// unattended and sends its result out of rsync.ai.
	ActionWorkflowRead  Action = "workflow.read"
	ActionWorkflowWrite Action = "workflow.write"

	// Charts and dashboards (handlers/charts_api.go, dashboards_api.go,
	// dashboard_changes.go). Reading a chart's sources and their columns, or a
	// dashboard's tiles and their charts' settings, is viewer-level, as the
	// chart editor and the dashboard are. Drafting a dashboard, or a change to
	// one of its tiles, is admin-level, as saving one is: the draft runs each
	// changed chart's query once to check it, and only an owner or admin may
	// save what it proposes.
	ActionChartRead     Action = "chart.read"
	ActionDashboardPlan Action = "dashboard.plan"

	// ActionGuideRead reads one of the agent gateway's topic guides: rsync.ai's
	// own rules for a product area, compiled into the binary. It reads nothing
	// of the workspace, so it is viewer-level, as every read is.
	ActionGuideRead Action = "guide.read"

	// ActionUsageRead reads the workspace's plan and how much of it is used:
	// the plan's name, its pipeline and NL→SQL query limits, the counts
	// against them and how many members it has. Counts and limits only, never
	// a member's e-mail or transfer volume. Viewer-level, as GET
	// /api/v1/usage/plan is.
	ActionUsageRead Action = "usage.read"
)

// Channel is where a request came from. It is recorded, never used to relax a
// rule: every channel meets the same minimum role.
type Channel string

const (
	ChannelREST Channel = "rest"
	ChannelChat Channel = "chat"
	// ChannelAgent is an AI agent calling the agent gateway with a token the
	// user made. It acts as that user, so it meets the same role rule, and on
	// top of it the token's scopes: a scope can only narrow, never widen.
	ChannelAgent Channel = "agent"
	// ChannelAssistant is the in-app assistant running the agent gateway's
	// tools for the signed-in user. It meets the same role rule and carries
	// fixed scopes (AssistantScopes), so it can do what such a token can and
	// nothing more.
	ChannelAssistant Channel = "assistant"
)

const (
	// ScopeMetadataRead lets an agent token read names, types, statuses and
	// timestamps. Every token carries it.
	ScopeMetadataRead = "metadata:read"
	// ScopeConnectorsGenerate lets an agent token start a connector
	// generation run. Only an owner or admin may put it on a token, and the
	// action still needs the admin role on every call
	// (docs/internal/ask-rsync-and-mcp-roadmap.md § 2.2).
	ScopeConnectorsGenerate = "connectors:generate"
	// ScopeModelsWrite lets an agent token create a model, schedule it, run it
	// and pause or resume its schedule. Only an owner or admin may put it on a
	// token, and every call still needs the admin role. It never lets an agent
	// delete a model, change an existing model's SQL or write to a connection.
	ScopeModelsWrite = "models:write"
	// ScopePipelinesPlan lets an agent token propose a pipeline. A proposal
	// creates nothing: the token's user opens it in rsync.ai and creates the
	// pipeline there, and no token can apply one. Any member may put it on a
	// token, as any member may create a pipeline.
	ScopePipelinesPlan = "pipelines:plan"
	// ScopeWorkflowsWrite lets an agent token draft a workflow. A draft runs
	// nothing and sends nothing: the person tests it and turns it on in
	// rsync.ai. Only an owner or admin may put it on a token, and every call
	// still needs the admin role.
	ScopeWorkflowsWrite = "workflows:write"
	// ScopeDashboardsWrite lets an agent token draft a dashboard. A draft saves
	// nothing: the person saves it in rsync.ai. Only an owner or admin may put it
	// on a token, and every call still needs the admin role.
	ScopeDashboardsWrite = "dashboards:write"
)

// AssistantScopes are the in-app assistant's fixed scopes. It acts for the
// signed-in user, whose own request is the agreement a token's scope stands
// for; the role rule still applies, so a member is refused generation and
// every model write.
var AssistantScopes = []string{ScopeMetadataRead, ScopeConnectorsGenerate, ScopeModelsWrite, ScopePipelinesPlan,
	ScopeWorkflowsWrite, ScopeDashboardsWrite}

// CtxUserText is the gin context key under which the assistant puts the
// user's own words for the question it is answering (this question and the
// earlier ones in the conversation). A tool that acts on a URL checks the URL
// is in them, so a URL the model found by itself is never acted on. It lives
// here because the handlers that read it cannot import the assistant package.
const CtxUserText = "assistant_user_text"

// Actor is the caller as the auth and workspace middleware resolved it.
type Actor struct {
	UserID      string
	WorkspaceID string
	Role        security.WorkspaceRole
	Channel     Channel
	// Scopes are the agent token's scopes, or the assistant's fixed one. Only
	// read on ChannelAgent and ChannelAssistant.
	Scopes []string
}

// ErrForbidden wraps every refusal from Authorize.
var ErrForbidden = errors.New("forbidden")

// minRole is the least workspace role each action needs. It matches the REST
// gates: CreatePipeline and a run need member, connector generation needs admin
// (WorkspaceGeneratorMiddleware). An action missing from this table is refused.
var minRole = map[Action]security.WorkspaceRole{
	ActionPipelineCreate:    security.WSMember,
	ActionPipelineRun:       security.WSMember,
	ActionPipelinePlan:      security.WSMember,
	ActionConnectorGenerate: security.WSAdmin,
	ActionConnectionRead:    security.WSViewer,
	ActionPipelineRead:      security.WSViewer,
	ActionRunRead:           security.WSViewer,
	ActionModelRead:         security.WSViewer,
	ActionModelWrite:        security.WSAdmin,
	ActionWorkflowRead:      security.WSViewer,
	ActionWorkflowWrite:     security.WSAdmin,
	ActionChartRead:         security.WSViewer,
	ActionDashboardPlan:     security.WSAdmin,
	ActionGuideRead:         security.WSViewer,
	ActionUsageRead:         security.WSViewer,
}

// agentScope is the token scope an agent needs for each action. An action
// missing from it is refused to every agent, whatever its role: running and
// creating pipelines are not open to agents. Proposing one is
// (ActionPipelinePlan); the person creates it.
var agentScope = map[Action]string{
	ActionConnectionRead:    ScopeMetadataRead,
	ActionPipelineRead:      ScopeMetadataRead,
	ActionRunRead:           ScopeMetadataRead,
	ActionConnectorGenerate: ScopeConnectorsGenerate,
	ActionModelRead:         ScopeMetadataRead,
	ActionModelWrite:        ScopeModelsWrite,
	ActionPipelinePlan:      ScopePipelinesPlan,
	ActionWorkflowRead:      ScopeMetadataRead,
	ActionWorkflowWrite:     ScopeWorkflowsWrite,
	ActionChartRead:         ScopeMetadataRead,
	ActionDashboardPlan:     ScopeDashboardsWrite,
	ActionGuideRead:         ScopeMetadataRead,
	ActionUsageRead:         ScopeMetadataRead,
}

// Authorize reports whether actor may perform action in its active workspace.
// It fails closed: an unknown action, a missing workspace, or an empty or
// unknown role is refused. An agent, and the in-app assistant, also needs the
// action's scope.
func Authorize(actor Actor, action Action) error {
	min, ok := minRole[action]
	if !ok {
		return fmt.Errorf("%w: unknown action %q", ErrForbidden, action)
	}
	if actor.WorkspaceID == "" {
		return fmt.Errorf("%w: no active workspace", ErrForbidden)
	}
	if !actor.Role.Meets(min) {
		return fmt.Errorf("%w: %s needs workspace role %s or higher", ErrForbidden, action, min)
	}
	if actor.Channel == ChannelAgent || actor.Channel == ChannelAssistant {
		scope, ok := agentScope[action]
		if !ok {
			return fmt.Errorf("%w: %s is not open to an agent token", ErrForbidden, action)
		}
		if !hasScope(actor.Scopes, scope) {
			return fmt.Errorf("%w: %s needs the token scope %s", ErrForbidden, action, scope)
		}
	}
	return nil
}

// MinRole is the least workspace role action needs, and false for an action
// Authorize refuses outright. The agent gateway's reference page prints it.
func MinRole(action Action) (security.WorkspaceRole, bool) {
	r, ok := minRole[action]
	return r, ok
}

// AgentScope is the token scope an agent needs for action, and false when no
// agent may perform it.
func AgentScope(action Action) (string, bool) {
	s, ok := agentScope[action]
	return s, ok
}

// TokenScopes are the scopes a token may carry. A refusal of an unknown scope
// lists them from here, so a new scope cannot be left out of it.
var TokenScopes = []string{ScopeMetadataRead, ScopeConnectorsGenerate, ScopeModelsWrite, ScopePipelinesPlan,
	ScopeWorkflowsWrite, ScopeDashboardsWrite}

// KnownScope reports whether scope is one a token may carry.
func KnownScope(scope string) bool {
	return slices.Contains(TokenScopes, scope)
}

// AdminScope reports whether only a workspace owner or admin may put scope on
// a token: the scopes whose actions need the admin role.
func AdminScope(scope string) bool {
	return scope == ScopeConnectorsGenerate || scope == ScopeModelsWrite || scope == ScopeWorkflowsWrite ||
		scope == ScopeDashboardsWrite
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
