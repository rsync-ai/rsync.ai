package services

import (
	"errors"
	"testing"

	"api-gateway/internal/security"
)

// TestAuthorize locks the one role table every channel asks (KI-CHAT-SKIPS-
// WORKSPACE-ROLE): the chat path used to create, run and generate with no
// workspace-role check at all, while REST refused a viewer. Fail closed on an
// unknown action, an unknown role, and a missing workspace.
func TestAuthorize(t *testing.T) {
	const ws = "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name   string
		actor  Actor
		action Action
		allow  bool
	}{
		{"viewer cannot create", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelChat}, ActionPipelineCreate, false},
		{"member creates", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelChat}, ActionPipelineCreate, true},
		{"owner creates", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelREST}, ActionPipelineCreate, true},
		{"viewer cannot run", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelChat}, ActionPipelineRun, false},
		{"member runs", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelChat}, ActionPipelineRun, true},
		{"member cannot generate", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelChat}, ActionConnectorGenerate, false},
		{"admin generates", Actor{WorkspaceID: ws, Role: security.WSAdmin, Channel: ChannelChat}, ActionConnectorGenerate, true},
		{"empty role", Actor{WorkspaceID: ws, Channel: ChannelChat}, ActionPipelineCreate, false},
		{"unknown role", Actor{WorkspaceID: ws, Role: "superuser", Channel: ChannelChat}, ActionPipelineCreate, false},
		{"no workspace", Actor{Role: security.WSOwner, Channel: ChannelChat}, ActionPipelineCreate, false},
		{"unknown action", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelChat}, Action("pipeline.delete_everything"), false},
		{"viewer reads pipelines", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelREST}, ActionPipelineRead, true},
		{"agent with scope reads", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionRunRead, true},
		{"agent without scope cannot read", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent}, ActionConnectionRead, false},
		{"agent with another scope cannot read", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{"pipeline:write"}}, ActionPipelineRead, false},
		// A scope only narrows: an owner's token still cannot run or create,
		// because no scope opens those to an agent, and cannot generate
		// without the generate scope.
		{"owner agent cannot run", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeConnectorsGenerate}}, ActionPipelineRun, false},
		{"owner agent cannot create", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeConnectorsGenerate}}, ActionPipelineCreate, false},
		{"owner agent without the generate scope cannot generate", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionConnectorGenerate, false},
		// The generate scope opens generation to an admin's token, and only
		// to one: the role rule still applies.
		{"admin agent with the generate scope generates", Actor{WorkspaceID: ws, Role: security.WSAdmin, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeConnectorsGenerate}}, ActionConnectorGenerate, true},
		{"member agent with the generate scope cannot generate", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeConnectorsGenerate}}, ActionConnectorGenerate, false},
		{"viewer agent with the generate scope cannot generate", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAgent, Scopes: []string{ScopeConnectorsGenerate}}, ActionConnectorGenerate, false},
		{"the generate scope alone does not read", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeConnectorsGenerate}}, ActionPipelineRead, false},
		// ...and cannot lift a role: a scoped agent with no role is refused.
		{"agent with scope but no role", Actor{WorkspaceID: ws, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionPipelineRead, false},
		{"agent with scope but no workspace", Actor{Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionPipelineRead, false},
		// The in-app assistant is held to the agent's scope rule: it reads, and
		// an owner's assistant still cannot run or create anything.
		{"assistant with scope reads", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAssistant, Scopes: []string{ScopeMetadataRead}}, ActionPipelineRead, true},
		{"assistant without scope cannot read", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAssistant}, ActionPipelineRead, false},
		{"owner assistant cannot run", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAssistant, Scopes: []string{ScopeMetadataRead}}, ActionPipelineRun, false},
		{"owner assistant cannot create", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionPipelineCreate, false},
		{"owner assistant cannot run with its scopes", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionPipelineRun, false},
		{"admin assistant generates", Actor{WorkspaceID: ws, Role: security.WSAdmin, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionConnectorGenerate, true},
		{"member assistant cannot generate", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionConnectorGenerate, false},
		// A plan creates nothing, so a member's assistant or token may propose
		// one; a viewer may not, as a viewer may not create the pipeline, and
		// the plan scope still opens neither create nor run.
		{"member assistant plans", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionPipelinePlan, true},
		{"viewer assistant cannot plan", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionPipelinePlan, false},
		{"member agent with the plan scope plans", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopePipelinesPlan}}, ActionPipelinePlan, true},
		{"owner agent without the plan scope cannot plan", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeModelsWrite}}, ActionPipelinePlan, false},
		{"owner agent with the plan scope cannot create", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopePipelinesPlan}}, ActionPipelineCreate, false},
		{"owner agent with the plan scope cannot run", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopePipelinesPlan}}, ActionPipelineRun, false},
		// A dashboard draft is admin-level, as saving a dashboard is; reading a
		// chart's sources is viewer-level, as the chart editor is.
		{"admin assistant drafts a dashboard", Actor{WorkspaceID: ws, Role: security.WSAdmin, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionDashboardPlan, true},
		{"member assistant cannot draft a dashboard", Actor{WorkspaceID: ws, Role: security.WSMember, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionDashboardPlan, false},
		{"owner agent without the dashboards scope cannot draft", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeWorkflowsWrite}}, ActionDashboardPlan, false},
		{"admin agent with the dashboards scope drafts", Actor{WorkspaceID: ws, Role: security.WSAdmin, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead, ScopeDashboardsWrite}}, ActionDashboardPlan, true},
		{"viewer agent reads chart sources", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionChartRead, true},
		{"agent without metadata:read cannot read chart sources", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeDashboardsWrite}}, ActionChartRead, false},
		{"viewer agent reads a guide", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAgent, Scopes: []string{ScopeMetadataRead}}, ActionGuideRead, true},
		{"viewer assistant reads a guide", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionGuideRead, true},
		{"agent without metadata:read cannot read a guide", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeWorkflowsWrite}}, ActionGuideRead, false},
		{"viewer assistant reads the plan's usage", Actor{WorkspaceID: ws, Role: security.WSViewer, Channel: ChannelAssistant, Scopes: AssistantScopes}, ActionUsageRead, true},
		{"agent without metadata:read cannot read the plan's usage", Actor{WorkspaceID: ws, Role: security.WSOwner, Channel: ChannelAgent, Scopes: []string{ScopeWorkflowsWrite}}, ActionUsageRead, false},
		{"no role cannot read the plan's usage", Actor{WorkspaceID: ws, Channel: ChannelREST}, ActionUsageRead, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Authorize(c.actor, c.action)
			if c.allow && err != nil {
				t.Fatalf("Authorize(%+v, %s) = %v, want allowed", c.actor, c.action, err)
			}
			if !c.allow {
				if err == nil {
					t.Fatalf("Authorize(%+v, %s) allowed, want refused", c.actor, c.action)
				}
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("Authorize(%+v, %s) = %v, want ErrForbidden", c.actor, c.action, err)
				}
			}
		})
	}
}

func TestKnownScope(t *testing.T) {
	for scope, want := range map[string]bool{
		ScopeMetadataRead: true, ScopeConnectorsGenerate: true, ScopeModelsWrite: true, ScopePipelinesPlan: true,
		ScopeWorkflowsWrite: true, ScopeDashboardsWrite: true, "dashboards:read": false,
		"pipelines:write": false, "": false, "pipeline:write": false, "connectors:generate ": false, "*": false,
	} {
		if got := KnownScope(scope); got != want {
			t.Errorf("KnownScope(%q) = %v, want %v", scope, got, want)
		}
	}
}

// TestEveryAgentScopeIsKnown: a scope the policy asks for that a token can
// never carry would leave its action closed to every agent without saying so.
func TestEveryAgentScopeIsKnown(t *testing.T) {
	for action, scope := range agentScope {
		if !KnownScope(scope) {
			t.Errorf("action %s needs scope %q, which KnownScope rejects", action, scope)
		}
	}
	for _, scope := range AssistantScopes {
		if !KnownScope(scope) {
			t.Errorf("assistant scope %q is not a known scope", scope)
		}
	}
}
