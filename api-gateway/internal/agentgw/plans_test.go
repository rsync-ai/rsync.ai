package agentgw

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/handlers"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// planRecorder is a ProposePipelinePlan that keeps what it was asked.
func planRecorder(got *[]handlers.PipelinePlanInput) func(*gin.Context, services.Actor, handlers.PipelinePlanInput) (*handlers.PipelinePlan, error) {
	return func(_ *gin.Context, _ services.Actor, in handlers.PipelinePlanInput) (*handlers.PipelinePlan, error) {
		*got = append(*got, in)
		return leakyPlan(in), nil
	}
}

// A token needs the pipelines:plan scope and a member user; the refusal names
// both, and the store is never reached. Control: a member token with the scope
// proposes, and the audit row names the plan it stored.
func TestPlanPipeline_NeedsScopeAndMember(t *testing.T) {
	var got []handlers.PipelinePlanInput
	h := leakyHandlers()
	h.ProposePipelinePlan = planRecorder(&got)

	member := adminActor
	member.Role = security.WSMember
	noScope := member
	noScope.Scopes = []string{services.ScopeMetadataRead, services.ScopeConnectorsGenerate, services.ScopeModelsWrite}
	viewer := member
	viewer.Role = security.WSViewer
	for who, actor := range map[string]services.Actor{"no scope": noScope, "viewer": viewer} {
		r, audit := testServer(t, h, actor)
		res := call(t, r, "plan_pipeline", validArgs["plan_pipeline"])
		if !isError(res) || !strings.Contains(resultText(res), "pipelines:plan") || !strings.Contains(resultText(res), "member") {
			t.Fatalf("%s: want a refusal naming the scope and role, got %v", who, res)
		}
		if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_pipeline", "pipeline_plan", "", "forbidden", 403}) {
			t.Fatalf("%s: audit = %+v", who, *audit)
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused call reached the store: %+v", got)
	}

	r, audit := testServer(t, h, member)
	d := data(t, call(t, r, "plan_pipeline", validArgs["plan_pipeline"]))
	if d["plan_id"] != planID || d["url"] != "https://app.rsync.example/pipeline-plans/"+planID {
		t.Fatalf("plan = %v", d)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_pipeline", "pipeline_plan", planID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}
	want := handlers.PipelinePlanInput{SourceConnectionID: connID, DestinationConnectionID: destID,
		Tables: []string{"orders", "customers", "refunds"}, SyncMode: "batch", ScheduleType: "cron", Cron: "0 * * * *"}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("store got %+v, want %+v", got, want)
	}
}

// The words a model uses for a mode reach the store as the mode; empty list
// entries are dropped; a schema the source has twice comes back as a choice
// for the person, never picked.
func TestPlanPipeline_ArgumentsAndChoices(t *testing.T) {
	var got []handlers.PipelinePlanInput
	h := leakyHandlers()
	h.ProposePipelinePlan = planRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	d := data(t, call(t, r, "plan_pipeline", map[string]any{"source_connection_id": connID,
		"destination_connection_id": destID, "tables": " orders,, public.customers ,", "sync_mode": "Real-time"}))
	if len(got) != 1 || got[0].SyncMode != "cdc" || !reflect.DeepEqual(got[0].Tables, []string{"orders", "public.customers"}) {
		t.Fatalf("store got %+v", got)
	}
	choices, _ := d["table_choices"].([]any)
	if len(choices) != 1 || choices[0].(map[string]any)["name"] != "customers" {
		t.Fatalf("table_choices = %v", d["table_choices"])
	}
	if next, _ := d["next"].(string); !strings.Contains(next, "url") || !strings.Contains(next, "no tool can") {
		t.Fatalf("next = %q", next)
	}

	// A one-time or scheduled copy is batch, whatever word the model uses.
	for _, mode := range []string{"once", "one-off", "one time", "ad hoc", "periodic"} {
		got = nil
		if res := call(t, r, "plan_pipeline", map[string]any{"source_connection_id": connID,
			"destination_connection_id": destID, "sync_mode": mode}); isError(res) {
			t.Fatalf("sync_mode %q: %s", mode, resultText(res))
		}
		if len(got) != 1 || got[0].SyncMode != "batch" {
			t.Fatalf("sync_mode %q: store got %+v, want batch", mode, got)
		}
	}

	// An unknown mode is refused before the store.
	got = nil
	res := call(t, r, "plan_pipeline", map[string]any{"source_connection_id": connID,
		"destination_connection_id": destID, "sync_mode": "hourly"})
	if !isError(res) || len(got) != 0 {
		t.Fatalf("want an argument refusal, got %v (store got %+v)", res, got)
	}
}

// The store's refusal reaches the agent as its text, and the audit row
// records its code and status.
func TestPlanPipeline_RefusalPassesThrough(t *testing.T) {
	h := leakyHandlers()
	h.ProposePipelinePlan = func(*gin.Context, services.Actor, handlers.PipelinePlanInput) (*handlers.PipelinePlan, error) {
		return nil, &handlers.PlanRefusal{Code: "draft_connector", Status: http.StatusUnprocessableEntity,
			Message: "warehouse uses a draft connector.‮ Publish it first."}
	}
	r, audit := testServer(t, h, adminActor)
	res := call(t, r, "plan_pipeline", validArgs["plan_pipeline"])
	if !isError(res) || resultText(res) != "warehouse uses a draft connector. Publish it first." {
		t.Fatalf("want the cleaned refusal text, got %q", resultText(res))
	}
	if (*audit)[0] != (auditRow{"plan_pipeline", "pipeline_plan", "", "draft_connector", http.StatusUnprocessableEntity}) {
		t.Fatalf("audit = %+v", *audit)
	}

	h.ProposePipelinePlan = nil
	r, _ = testServer(t, h, adminActor)
	if res := call(t, r, "plan_pipeline", validArgs["plan_pipeline"]); !isError(res) || !strings.Contains(resultText(res), "not available") {
		t.Fatalf("a missing store: got %v", res)
	}
}

// The assistant proposes for any member, and Run hands back the plan id so the
// page can show the plan as a card; the model is told the card is there.
func TestPlanPipeline_AssistantRunReturnsThePlan(t *testing.T) {
	member := assistantAdmin
	member.Role = security.WSMember
	s, c, rows := inProcess(t, member)
	args, _ := json.Marshal(validArgs["plan_pipeline"])
	r := s.Run(c, "plan_pipeline", args)
	text, isErr, made := r.Text, r.IsError, r.ResourceID
	if isErr || made != planID || !strings.Contains(text, "card") {
		t.Fatalf("Run = (%q, %v, %q)", text, isErr, made)
	}
	if len(*rows) != 1 || (*rows)[0] != (auditRow{"plan_pipeline", "pipeline_plan", planID, "ok", 200}) {
		t.Fatalf("audit = %+v", *rows)
	}

	// A viewer is refused, and Run names no resource for a refusal.
	s, c, _ = inProcess(t, assistantActor)
	if r := s.Run(c, "plan_pipeline", args); !r.IsError || r.ResourceID != "" || !strings.Contains(r.Text, "member") ||
		r.Audited != (Audited{Outcome: "forbidden", Status: 403}) {
		t.Fatalf("viewer: Run = %+v", r)
	}
}

// No token and no assistant can create or run a pipeline, whatever scopes it
// carries: proposing is the most a tool does, and the person applies a plan
// with their session. Control: the same owner, signed in, may.
func TestNoToolCreatesOrRunsAPipeline(t *testing.T) {
	all := []string{services.ScopeMetadataRead, services.ScopeConnectorsGenerate, services.ScopeModelsWrite,
		services.ScopePipelinesPlan, "pipelines:write", "pipelines:run"}
	for _, ch := range []services.Channel{services.ChannelAgent, services.ChannelAssistant} {
		owner := services.Actor{UserID: "u1", WorkspaceID: "w1", Role: security.WSOwner, Channel: ch, Scopes: all}
		for _, a := range []services.Action{services.ActionPipelineCreate, services.ActionPipelineRun} {
			if services.Authorize(owner, a) == nil {
				t.Fatalf("%s may %s", ch, a)
			}
		}
	}
	rest := services.Actor{UserID: "u1", WorkspaceID: "w1", Role: security.WSOwner, Channel: services.ChannelREST}
	if services.Authorize(rest, services.ActionPipelineCreate) != nil {
		t.Fatal("control: a signed-in owner is refused create")
	}
	s, _, _ := inProcess(t, assistantAdmin)
	for _, tool := range s.Tools() {
		if tool.Action == services.ActionPipelineCreate || tool.Action == services.ActionPipelineRun {
			t.Fatalf("tool %s creates or runs a pipeline", tool.Name)
		}
	}
}
