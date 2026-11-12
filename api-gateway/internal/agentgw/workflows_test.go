package agentgw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"api-gateway/internal/handlers"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
)

func workflowRecorder(got *[]handlers.WorkflowDraftInput) func(*gin.Context, services.Actor, handlers.WorkflowDraftInput) (*handlers.Workflow, error) {
	return func(_ *gin.Context, _ services.Actor, in handlers.WorkflowDraftInput) (*handlers.Workflow, error) {
		*got = append(*got, in)
		return leakyWorkflow(in), nil
	}
}

// A token needs the workflows:write scope and an admin user; the refusal names
// both and the store is never reached. Control: an admin token with the scope
// drafts, the SQL and message keep their line breaks, and the audit row names
// the draft.
func TestPlanWorkflow_NeedsScopeAndAdmin(t *testing.T) {
	var got []handlers.WorkflowDraftInput
	h := leakyHandlers()
	h.ProposeWorkflowDraft = workflowRecorder(&got)

	noScope := adminActor
	noScope.Scopes = []string{services.ScopeMetadataRead, services.ScopePipelinesPlan, services.ScopeModelsWrite}
	member := adminActor
	member.Role = security.WSMember
	for who, actor := range map[string]services.Actor{"no scope": noScope, "member": member} {
		r, audit := testServer(t, h, actor)
		res := call(t, r, "plan_workflow", validArgs["plan_workflow"])
		if !isError(res) || !strings.Contains(resultText(res), "workflows:write") || !strings.Contains(resultText(res), "admin") {
			t.Fatalf("%s: want a refusal naming the scope and role, got %v", who, res)
		}
		if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_workflow", "workflow", "", "forbidden", 403}) {
			t.Fatalf("%s: audit = %+v", who, *audit)
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused call reached the store: %+v", got)
	}

	r, audit := testServer(t, h, adminActor)
	d := data(t, call(t, r, "plan_workflow", validArgs["plan_workflow"]))
	if d["workflow_id"] != workflowID || d["status"] != "draft" || d["url"] != "https://app.rsync.example/workflows/"+workflowID {
		t.Fatalf("draft = %v", d)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_workflow", "workflow", workflowID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}
	want := handlers.WorkflowDraftInput{Name: "Weekly revenue", ConnectionID: connID,
		SQL:     "SELECT sum(total) AS revenue\nFROM gold.orders\nWHERE created_at > now() - interval '7 days'",
		Trigger: "schedule", Cron: "0 9 * * 1",
		Branches: []handlers.WorkflowBranchInput{{Condition: "always",
			Message: "Revenue last week: {{revenue}}\n{{link}}", Channels: []string{"#finance"}}}}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("store got %+v\nwant %+v", got, want)
	}
}

// The words a model uses reach the store as the enum values; channel names
// are split and trimmed; a word that is no enum value is refused first.
func TestPlanWorkflow_Arguments(t *testing.T) {
	var got []handlers.WorkflowDraftInput
	h := leakyHandlers()
	h.ProposeWorkflowDraft = workflowRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	args := map[string]any{"name": "Failed payments", "connection_id": connID, "sql": "SELECT count(*) AS failed FROM payments",
		"trigger": "hourly", "cron": "0 * * * *", "quiet_minutes": 120, "branches": []any{
			map[string]any{"condition": "threshold", "condition_op": "greater_than", "threshold": "50",
				"message": "{{failed}} failed", "channels": " #payments ,, On-call ,"},
			map[string]any{"condition": "otherwise", "message": "All clear", "channels": "Finance team"},
		}}
	data(t, call(t, r, "plan_workflow", args))
	want := []handlers.WorkflowBranchInput{
		{Condition: "value", ConditionOp: ">", Threshold: "50", Message: "{{failed}} failed", Channels: []string{"#payments", "On-call"}},
		{Condition: "always", Message: "All clear", Channels: []string{"Finance team"}},
	}
	if len(got) != 1 || got[0].Trigger != "schedule" || got[0].QuietMinutes != 120 || !reflect.DeepEqual(got[0].Branches, want) {
		t.Fatalf("store got %+v", got)
	}

	// A model that sends the array as a JSON string is read the same way.
	got = nil
	raw, _ := json.Marshal(args["branches"])
	args["branches"] = string(raw)
	data(t, call(t, r, "plan_workflow", args))
	if len(got) != 1 || !reflect.DeepEqual(got[0].Branches, want) {
		t.Fatalf("a stringified array: store got %+v", got)
	}

	got = nil
	bad := map[string]any{}
	for k, v := range validArgs["plan_workflow"] {
		bad[k] = v
	}
	bad["trigger"] = "whenever"
	if res := call(t, r, "plan_workflow", bad); !isError(res) || len(got) != 0 {
		t.Fatalf("want an argument refusal, got %v (store got %+v)", res, got)
	}
}

// A branch is checked like any argument, and its errors name it by its place
// in the array; too many branches, a branch that is no object, an unknown
// field and a missing message are refused before the store is reached.
func TestPlanWorkflow_BranchArgumentErrors(t *testing.T) {
	var got []handlers.WorkflowDraftInput
	h := leakyHandlers()
	h.ProposeWorkflowDraft = workflowRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	ok := map[string]any{"condition": "always", "message": "hi", "channels": "#finance"}
	for name, tc := range map[string]struct {
		branches any
		want     string
	}{
		"bad enum":       {[]any{ok, map[string]any{"condition": "sometimes", "message": "hi", "channels": "#a"}}, "branches[1].condition"},
		"unknown field":  {[]any{map[string]any{"message": "hi", "channels": "#a", "colour": "red"}}, "branches[0].colour"},
		"no message":     {[]any{map[string]any{"channels": "#a"}}, "branches[0].message"},
		"not an object":  {[]any{"always"}, "branches[0] must be an object"},
		"101":            {manyBranches(ok, 101), "at most 100"},
		"empty":          {[]any{}, "branches"},
		"keep on draft":  {[]any{map[string]any{"keep": 1, "message": "hi", "channels": "#a"}}, "branches[0].keep"},
		"not an array":   {map[string]any{"message": "hi"}, "branches"},
		"bad json array": {"[{", "branches"},
	} {
		args := map[string]any{}
		for k, v := range validArgs["plan_workflow"] {
			args[k] = v
		}
		args["branches"] = tc.branches
		res := call(t, r, "plan_workflow", args)
		if !isError(res) || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s: want a refusal naming %q, got %q", name, tc.want, resultText(res))
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused call reached the store: %+v", got)
	}

	// Six conditions are past the old cap of three plus an otherwise; the
	// schema lets them through to the store's own check.
	args := map[string]any{}
	for k, v := range validArgs["plan_workflow"] {
		args[k] = v
	}
	args["branches"] = manyBranches(ok, 6)
	if res := call(t, r, "plan_workflow", args); isError(res) || len(got) != 1 || len(got[0].Branches) != 6 {
		t.Fatalf("six conditions refused: %q (%d drafts)", resultText(res), len(got))
	}
}

func manyBranches(b map[string]any, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// The model repeats its tools' words to the person, who reads a branch as a
// condition and never sees the cap: no workflow tool's title or description
// (an argument's or an item's included) says branch or states a limit.
func TestWorkflowTools_SayConditionsNotBranches(t *testing.T) {
	limit := regexp.MustCompile(`(?i)\bup to \d|\bat most \d|\b(three|3) (branches|conditions)`)
	check := func(where, text string) {
		if strings.Contains(strings.ToLower(text), "branch") {
			t.Errorf("%s says branch: %q", where, text)
		}
		if limit.MatchString(text) {
			t.Errorf("%s states a limit: %q", where, text)
		}
	}
	var walk func(where string, args []Arg)
	walk = func(where string, args []Arg) {
		for _, a := range args {
			check(where+"."+a.Name, a.Description)
			walk(where+"."+a.Name, a.Items)
		}
	}
	tools := workflowTools(Handlers{})
	if len(tools) != 3 {
		t.Fatalf("%d workflow tools; this test reads each", len(tools))
	}
	for _, tool := range tools {
		check(tool.Name+" title", tool.Title)
		check(tool.Name, tool.Description)
		walk(tool.Name, tool.Args)
	}
	check("server instructions", instructions)
}

// The draft the agent gets back carries no SQL, no message and nothing a
// query could have filled; the store's refusal reaches the agent as text.
func TestPlanWorkflow_OutputAndRefusal(t *testing.T) {
	h := leakyHandlers()
	r, _ := testServer(t, h, adminActor)
	res := call(t, r, "plan_workflow", validArgs["plan_workflow"])
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), canary) || strings.Contains(string(raw), "SELECT") {
		t.Fatalf("the draft leaked its SQL or message: %s", raw)
	}

	if d := data(t, res); d["check"] != "The query ran once and returns a column for every slot." {
		t.Fatalf("check = %v", d["check"])
	}
	h.ProposeWorkflowDraft = func(_ *gin.Context, _ services.Actor, in handlers.WorkflowDraftInput) (*handlers.Workflow, error) {
		w := leakyWorkflow(in)
		w.DraftUnchecked = true
		return w, nil
	}
	r, _ = testServer(t, h, adminActor)
	if d := data(t, call(t, r, "plan_workflow", validArgs["plan_workflow"])); !strings.Contains(d["check"].(string), "click Test") {
		t.Fatalf("an unchecked draft must say so: check = %v", d["check"])
	}

	h.ProposeWorkflowDraft = func(*gin.Context, services.Actor, handlers.WorkflowDraftInput) (*handlers.Workflow, error) {
		return nil, &handlers.PlanRefusal{Code: "channel_not_found", Status: http.StatusNotFound,
			Message: "There is no delivery channel named \"#fin\". This workspace's delivery channels are: #finance (slack)."}
	}
	r, audit := testServer(t, h, adminActor)
	res = call(t, r, "plan_workflow", validArgs["plan_workflow"])
	if !isError(res) || !strings.Contains(resultText(res), "#finance (slack)") {
		t.Fatalf("want the refusal text, got %q", resultText(res))
	}
	if (*audit)[0] != (auditRow{"plan_workflow", "workflow", "", "channel_not_found", http.StatusNotFound}) {
		t.Fatalf("audit = %+v", *audit)
	}

	h.ProposeWorkflowDraft = nil
	r, _ = testServer(t, h, adminActor)
	if res := call(t, r, "plan_workflow", validArgs["plan_workflow"]); !isError(res) || !strings.Contains(resultText(res), "not available") {
		t.Fatalf("a missing store: got %v", res)
	}
}

// The assistant drafts for an admin, and Run hands back the workflow id so the
// page can show the draft as a card; a member is refused.
func TestPlanWorkflow_AssistantRunReturnsTheDraft(t *testing.T) {
	s, c, rows := inProcess(t, assistantAdmin)
	args, _ := json.Marshal(validArgs["plan_workflow"])
	r := s.Run(c, "plan_workflow", args)
	text, isErr, made := r.Text, r.IsError, r.ResourceID
	if isErr || made != workflowID || !strings.Contains(text, "card") {
		t.Fatalf("Run = (%q, %v, %q)", text, isErr, made)
	}
	if len(*rows) != 1 || (*rows)[0] != (auditRow{"plan_workflow", "workflow", workflowID, "ok", 200}) {
		t.Fatalf("audit = %+v", *rows)
	}

	member := assistantAdmin
	member.Role = security.WSMember
	s, c, _ = inProcess(t, member)
	if r := s.Run(c, "plan_workflow", args); !r.IsError || r.ResourceID != "" || !strings.Contains(r.Text, "admin") {
		t.Fatalf("member: Run = %+v", r)
	}
}

// No tool turns a workflow on, runs it, tests it or applies a change: those
// are REST calls with the person's session. Proposing is the most a tool does.
func TestNoToolTurnsOnOrRunsAWorkflow(t *testing.T) {
	s, _, _ := inProcess(t, assistantAdmin)
	allowed := map[string]bool{"plan_workflow": true, "list_workflows": true, "plan_workflow_change": true}
	for _, tool := range s.Tools() {
		if strings.Contains(tool.Name, "workflow") && !allowed[tool.Name] {
			t.Fatalf("unexpected workflow tool %s", tool.Name)
		}
	}
}

// list_workflows is open to a viewer token with metadata:read. Each workflow
// reads as its trigger and its branches, each with when it sends and its
// channels; never its SQL, messages, paused reason or last run.
func TestListWorkflows_ShapeAndNoLeak(t *testing.T) {
	viewer := adminActor
	viewer.Role = security.WSViewer
	viewer.Scopes = []string{services.ScopeMetadataRead}
	r, audit := testServer(t, leakyHandlers(), viewer)
	res := call(t, r, "list_workflows", map[string]any{})
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), canary) || strings.Contains(string(raw), fmt.Sprint(canaryNum)) ||
		strings.Contains(string(raw), "SELECT") {
		t.Fatalf("list_workflows leaked a stored value: %s", raw)
	}
	d := data(t, res)
	list := d["workflows"].([]any)
	if len(list) != 1 || d["truncated"] != false {
		t.Fatalf("workflows = %v", d)
	}
	w := list[0].(map[string]any)
	if w["id"] != workflowID || w["trigger"] != "every hour (UTC)" || w["status"] != "active" {
		t.Fatalf("workflow = %v", w)
	}
	branches, _ := json.Marshal(w["branches"])
	want := `[{"channels":["#ops"],"number":1,"when":"send when failed is above 100"},` +
		`{"channels":["Finance team"],"number":2,"when":"otherwise send"}]`
	if string(branches) != want {
		t.Fatalf("branches = %s\nwant %s", branches, want)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"list_workflows", "workflow", "", "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}
}

// More workflows than a model is shown are cut, and the output says so.
func TestListWorkflows_Truncates(t *testing.T) {
	h := leakyHandlers()
	h.ListWorkflows = func(*gin.Context, services.Actor) ([]*handlers.Workflow, error) {
		out := make([]*handlers.Workflow, handlers.WorkflowsForModelMax+1)
		for i := range out {
			out[i] = leakyAlert()
		}
		return out, nil
	}
	r, _ := testServer(t, h, adminActor)
	d := data(t, call(t, r, "list_workflows", map[string]any{}))
	if len(d["workflows"].([]any)) != handlers.WorkflowsForModelMax || d["truncated"] != true {
		t.Fatalf("got %d workflows, truncated=%v", len(d["workflows"].([]any)), d["truncated"])
	}
}

// plan_workflow_change needs the workflows:write scope and an admin. Each item
// reaches the store with keep as the 1-based number the model sent; the audit
// row and the output name the change, not the workflow, and the output shows
// the branches after the change without their messages.
func TestPlanWorkflowChange(t *testing.T) {
	type proposal struct {
		id       string
		branches []handlers.WorkflowBranchInput
		summary  string
	}
	var got []proposal
	h := leakyHandlers()
	inner := h.ProposeWorkflowChange
	h.ProposeWorkflowChange = func(c *gin.Context, a services.Actor, id string, in []handlers.WorkflowBranchInput,
		summary string) (*handlers.WorkflowChange, error) {
		got = append(got, proposal{id, in, summary})
		return inner(c, a, id, in, summary)
	}

	member := adminActor
	member.Role = security.WSMember
	r, audit := testServer(t, h, member)
	if res := call(t, r, "plan_workflow_change", validArgs["plan_workflow_change"]); !isError(res) ||
		!strings.Contains(resultText(res), "admin") {
		t.Fatalf("member: want a refusal, got %v", res)
	}
	if len(got) != 0 || (*audit)[0] != (auditRow{"plan_workflow_change", "workflow_change", "", "forbidden", 403}) {
		t.Fatalf("a member reached the store: %+v, audit %+v", got, *audit)
	}

	r, audit = testServer(t, h, adminActor)
	args := map[string]any{"workflow_id": workflowID, "summary": "Raises the Slack alert to 500.", "branches": []any{
		map[string]any{"keep": 1, "threshold": "500"},
		map[string]any{"condition": "else", "message": "{{failed}} failed", "channels": "Finance team, #ops"},
	}}
	res := call(t, r, "plan_workflow_change", args)
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), canary) || strings.Contains(string(raw), "SELECT") {
		t.Fatalf("the change leaked a stored value: %s", raw)
	}
	d := data(t, res)
	if d["change_id"] != changeID || d["workflow_id"] != workflowID || d["summary"] != "Raises the Slack alert to 500." ||
		!strings.Contains(d["url"].(string), "change="+changeID) || !strings.Contains(d["next"].(string), "url") {
		t.Fatalf("change = %v", d)
	}
	branches, _ := json.Marshal(d["branches"])
	if string(branches) != `[{"channels":["Finance team"],"number":1,"when":"always send"}]` {
		t.Fatalf("branches = %s", branches)
	}
	want := proposal{workflowID, []handlers.WorkflowBranchInput{
		{Keep: 1, Threshold: "500"},
		{Condition: "always", Message: "{{failed}} failed", Channels: []string{"Finance team", "#ops"}},
	}, "Raises the Slack alert to 500."}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("store got %+v\nwant %+v", got, want)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_workflow_change", "workflow_change", changeID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}

	for name, branches := range map[string][]any{
		"keep past the last": {map[string]any{"keep": 101}},
		"keep is a number":   {map[string]any{"keep": "two"}},
	} {
		args := map[string]any{"workflow_id": workflowID, "branches": branches}
		if res := call(t, r, "plan_workflow_change", args); !isError(res) || !strings.Contains(resultText(res), "branches[0].keep") {
			t.Errorf("%s: want a refusal naming branches[0].keep, got %q", name, resultText(res))
		}
	}

	h.ProposeWorkflowChange = func(*gin.Context, services.Actor, string, []handlers.WorkflowBranchInput, string) (*handlers.WorkflowChange, error) {
		return nil, &handlers.PlanRefusal{Code: "branch_not_found", Status: http.StatusUnprocessableEntity, Branch: 2,
			Message: "Condition 2: keep 3 names no condition; the workflow has 2."}
	}
	r, audit = testServer(t, h, adminActor)
	res = call(t, r, "plan_workflow_change", validArgs["plan_workflow_change"])
	if !isError(res) || !strings.Contains(resultText(res), "keep 3 names no condition") {
		t.Fatalf("want the refusal text, got %q", resultText(res))
	}
	if (*audit)[0] != (auditRow{"plan_workflow_change", "workflow_change", "", "branch_not_found", http.StatusUnprocessableEntity}) {
		t.Fatalf("audit = %+v", *audit)
	}
}

// The assistant's Run hands back the change id, which the page shows as a
// card comparing the branches; an unchecked query says Apply checks it again.
func TestPlanWorkflowChange_AssistantRunReturnsTheChange(t *testing.T) {
	s, c, rows := inProcess(t, assistantAdmin)
	args, _ := json.Marshal(validArgs["plan_workflow_change"])
	r := s.Run(c, "plan_workflow_change", args)
	text, isErr, made := r.Text, r.IsError, r.ResourceID
	if isErr || made != changeID || !strings.Contains(text, "card") || !strings.Contains(text, "Apply") {
		t.Fatalf("Run = (%q, %v, %q)", text, isErr, made)
	}
	if len(*rows) != 1 || (*rows)[0] != (auditRow{"plan_workflow_change", "workflow_change", changeID, "ok", 200}) {
		t.Fatalf("audit = %+v", *rows)
	}
}

// leakyStepsWorkflow is a workflow that runs a pipeline, refreshes a model
// after it, and, with no query, sends one message with a chart and a dashboard
// when the model ends. Its message carries the canary; the deleted pipeline of a second
// run_pipeline step has no step ref.
func leakyStepsWorkflow() *handlers.Workflow {
	w := &handlers.Workflow{
		ID: workflowID, Name: "Nightly build", Status: "draft", PausedReason: canary, ProposedBy: "assistant",
		TriggerDescription: "every day at 02:00 (UTC)",
		Channels:           []handlers.WorkflowChannel{{ID: destID, Name: "#data", Kind: "slack", NodeID: "send_1"}},
		StepRefs: map[string]handlers.WorkflowRef{
			"load":   {ID: pipeID, Name: "orders load"},
			"build":  {ID: modelID, Name: "revenue model"},
			"orders": {ID: chartID, Name: "orders by day"},
			"board":  {ID: dashID, Name: "weekly board"},
		},
		URL: "https://app.rsync.example/workflows/" + workflowID,
	}
	m := workflow.Message{Subject: canary, Body: canary}
	w.Definition = workflow.Definition{Version: 3, Steps: []workflow.Step{
		{ID: "load", Type: workflow.StepRunPipeline, PipelineID: pipeID, TimeoutMinutes: 720, Retries: 2},
		{ID: "gone", Type: workflow.StepRunPipeline, PipelineID: runID, TimeoutMinutes: 60},
		{ID: "build", Type: workflow.StepRefreshModel, After: []string{"load", "gone"}, ModelID: modelID,
			TimeoutMinutes: 720},
		{ID: "cool", Type: workflow.StepWait, After: []string{"build"}, Rule: workflow.RuleAllDone, Minutes: 15},
		{ID: "orders", Type: workflow.StepChart, After: []string{"cool"}, ChartID: chartID},
		{ID: "board", Type: workflow.StepDashboard, After: []string{"cool"}, DashboardID: dashID},
		{ID: "send_1", Type: workflow.StepSend, After: []string{"orders", "board"}, Rule: workflow.RuleAnyFailed, Message: &m,
			Required: true},
	}}
	return w
}

// plan_workflow's steps reach the store as the handler's step input: a model's
// words for a type or a rule become the enum value, after splits on commas,
// and steps_rule passes through. With steps, connection_id and sql may be left
// out. Control: the same call without steps reaches the store with none.
func TestPlanWorkflow_Steps(t *testing.T) {
	var got []handlers.WorkflowDraftInput
	h := leakyHandlers()
	h.ProposeWorkflowDraft = workflowRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	args := map[string]any{"name": "Nightly build", "trigger": "manual", "steps_rule": "on_failure",
		"branches": []any{map[string]any{"message": "The build failed.", "channels": "#data"}},
		"steps": []any{
			map[string]any{"id": "load", "type": "pipeline", "pipeline_id": pipeID, "retries": 2},
			map[string]any{"id": "build", "type": "build_model", "model_id": modelID, "after": " load ,, ",
				"timeout_minutes": 90},
			map[string]any{"id": "cool", "type": "sleep", "minutes": 15, "after": "load,build", "rule": "done"},
			map[string]any{"id": "orders", "type": "graph", "chart_id": chartID, "after": "cool"},
			map[string]any{"id": "board", "type": "board", "dashboard_id": dashID, "after": "cool"},
		}}
	data(t, call(t, r, "plan_workflow", args))
	want := []handlers.WorkflowStepInput{
		{ID: "load", Type: "run_pipeline", PipelineID: pipeID, Retries: 2},
		{ID: "build", Type: "refresh_model", ModelID: modelID, After: []string{"load"}, TimeoutMinutes: 90},
		{ID: "cool", Type: "wait", Minutes: 15, After: []string{"load", "build"}, Rule: "all_done"},
		{ID: "orders", Type: "chart", ChartID: chartID, After: []string{"cool"}},
		{ID: "board", Type: "dashboard", DashboardID: dashID, After: []string{"cool"}},
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Steps, want) || got[0].StepsRule != "any_failed" ||
		got[0].ConnectionID != "" || got[0].SQL != "" {
		t.Fatalf("store got %+v\nwant steps %+v", got, want)
	}

	got = nil
	data(t, call(t, r, "plan_workflow", validArgs["plan_workflow"]))
	if len(got) != 1 || got[0].Steps != nil || got[0].StepsRule != "" {
		t.Fatalf("no steps: store got %+v", got)
	}
}

// A step is checked like a branch, and its errors name it by its place; a
// refused step never reaches the store.
func TestPlanWorkflow_StepArgumentErrors(t *testing.T) {
	var got []handlers.WorkflowDraftInput
	h := leakyHandlers()
	h.ProposeWorkflowDraft = workflowRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	ok := map[string]any{"id": "load", "type": "run_pipeline", "pipeline_id": pipeID}
	for name, tc := range map[string]struct {
		steps any
		want  string
	}{
		"bad type":       {[]any{ok, map[string]any{"id": "q", "type": "query"}}, "steps[1].type"},
		"no id":          {[]any{map[string]any{"type": "wait", "minutes": 5}}, "steps[0].id"},
		"no type":        {[]any{map[string]any{"id": "w"}}, "steps[0].type"},
		"bad rule":       {[]any{map[string]any{"id": "w", "type": "wait", "minutes": 5, "rule": "sometimes"}}, "steps[0].rule"},
		"four retries":   {[]any{map[string]any{"id": "l", "type": "run_pipeline", "pipeline_id": pipeID, "retries": 4}}, "steps[0].retries"},
		"a day and more": {[]any{map[string]any{"id": "w", "type": "wait", "minutes": 1441}}, "steps[0].minutes"},
		"pipeline id":    {[]any{map[string]any{"id": "l", "type": "run_pipeline", "pipeline_id": "orders"}}, "steps[0].pipeline_id"},
		"chart id":       {[]any{map[string]any{"id": "c", "type": "chart", "chart_id": "orders by day"}}, "steps[0].chart_id"},
		"dashboard id":   {[]any{map[string]any{"id": "d", "type": "dashboard", "dashboard_id": "weekly"}}, "steps[0].dashboard_id"},
		"unknown field":  {[]any{map[string]any{"id": "l", "type": "run_pipeline", "sql": "SELECT 1"}}, "steps[0].sql"},
		"too many":       {manyBranches(ok, maxWorkflowToolSteps+1), "at most 20"},
	} {
		args := map[string]any{}
		for k, v := range validArgs["plan_workflow"] {
			args[k] = v
		}
		args["steps"] = tc.steps
		res := call(t, r, "plan_workflow", args)
		if !isError(res) || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s: want a refusal naming %q, got %q", name, tc.want, resultText(res))
		}
	}
	args := map[string]any{}
	for k, v := range validArgs["plan_workflow"] {
		args[k] = v
	}
	args["steps_rule"] = "whenever"
	if res := call(t, r, "plan_workflow", args); !isError(res) || !strings.Contains(resultText(res), "steps_rule") {
		t.Errorf("bad steps_rule: got %q", resultText(res))
	}
	if len(got) != 0 {
		t.Fatalf("a refused call reached the store: %+v", got)
	}
}

// A workflow's steps reach the model as their ids, types, parents and rules,
// with the pipeline, model, chart or dashboard each runs or draws by name and id (a step
// whose pipeline is gone shows its id with no name), and the rule its one
// message runs by, after drawing steps alone; never the message. A draft with no query says so instead of claiming its
// query ran, and has no connection.
func TestWorkflowTools_ShowSteps(t *testing.T) {
	wantSteps := `[{"after":[],"id":"load","pipeline":{"id":"` + pipeID + `","name":"orders load"},` +
		`"retries":2,"timeout_minutes":720,"type":"run_pipeline"},` +
		`{"after":[],"id":"gone","pipeline":{"id":"` + runID + `","name":""},"timeout_minutes":60,"type":"run_pipeline"},` +
		`{"after":["load","gone"],"id":"build","model":{"id":"` + modelID + `","name":"revenue model"},` +
		`"rule":"all_success","timeout_minutes":720,"type":"refresh_model"},` +
		`{"after":["build"],"id":"cool","minutes":15,"rule":"all_done","type":"wait"},` +
		`{"after":["cool"],"chart":{"id":"` + chartID + `","name":"orders by day"},"id":"orders",` +
		`"rule":"all_success","type":"chart"},` +
		`{"after":["cool"],"dashboard":{"id":"` + dashID + `","name":"weekly board"},"id":"board",` +
		`"rule":"all_success","type":"dashboard"}]`
	check := func(where string, w map[string]any, raw []byte) {
		t.Helper()
		if strings.Contains(string(raw), canary) {
			t.Fatalf("%s leaked a stored value: %s", where, raw)
		}
		steps, _ := json.Marshal(w["steps"])
		if string(steps) != wantSteps || w["steps_rule"] != "any_failed" {
			t.Fatalf("%s steps = %s, steps_rule = %v\nwant %s", where, steps, w["steps_rule"], wantSteps)
		}
		branches, _ := json.Marshal(w["branches"])
		if string(branches) != `[{"channels":["#data"],"number":1,"when":"send when its steps finish"}]` {
			t.Fatalf("%s branches = %s", where, branches)
		}
		if _, ok := w["connection"]; ok {
			t.Fatalf("%s has a connection with no query: %v", where, w["connection"])
		}
	}

	h := leakyHandlers()
	h.ListWorkflows = func(*gin.Context, services.Actor) ([]*handlers.Workflow, error) {
		return []*handlers.Workflow{leakyStepsWorkflow()}, nil
	}
	h.ProposeWorkflowDraft = func(*gin.Context, services.Actor, handlers.WorkflowDraftInput) (*handlers.Workflow, error) {
		return leakyStepsWorkflow(), nil
	}
	r, _ := testServer(t, h, adminActor)
	res := call(t, r, "list_workflows", map[string]any{})
	raw, _ := json.Marshal(res)
	check("list_workflows", data(t, res)["workflows"].([]any)[0].(map[string]any), raw)

	res = call(t, r, "plan_workflow", validArgs["plan_workflow"])
	raw, _ = json.Marshal(res)
	d := data(t, res)
	check("plan_workflow", d, raw)
	if !strings.Contains(d["check"].(string), "no query") {
		t.Fatalf("check = %v", d["check"])
	}

	// Control: a workflow with no steps shows an empty list and no rule, and
	// its draft says the query ran.
	h.ProposeWorkflowDraft = workflowRecorder(&[]handlers.WorkflowDraftInput{})
	r, _ = testServer(t, h, adminActor)
	d = data(t, call(t, r, "plan_workflow", validArgs["plan_workflow"]))
	if steps, _ := json.Marshal(d["steps"]); string(steps) != "[]" || d["steps_rule"] != nil ||
		d["check"] != "The query ran once and returns a column for every slot." || d["connection"] == nil {
		t.Fatalf("no steps: %v", d)
	}
}
