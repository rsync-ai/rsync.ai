package agentgw

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/handlers"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"
)

// The workflow tools: draft a workflow the person then tests and turns on in
// rsync.ai, list the workflows, and propose a change to one's branches that
// the person applies. A workflow is a trigger, one read-only SQL query on a
// connection, and branches, each a condition on the query's result, a message
// and delivery channels the workspace already has; the last may send
// otherwise. The first branch whose condition is true sends. Before the query
// a workflow may run steps: run a batch pipeline, refresh a model or wait,
// each after the steps it names; with steps and no query it sends one message
// when they end. The person reads
// a branch as a condition, so every description here says condition: the
// model repeats its tools' words. Only the argument names say branches. A draft or a change sends nothing; no tool turns a workflow on,
// runs one, tests one or applies a change. The server checks a draft
// (handlers.ProposeWorkflowDraft) and a change (handlers.ProposeWorkflowChange):
// the SQL is one read statement, the connection is a SQL one in this
// workspace, the channels exist, each step's pipeline is a batch one and its
// model a model in this workspace, and the query, run once, compiles and
// returns a column for every slot and condition.
//
// The output is names, ids and descriptions: never a row value, nor a
// workflow's SQL or messages. The rows of that one run stay in the server; a
// refusal carries the scrubbed database error or the result's column names.

type workflowRefOut struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ConnectorType string `json:"connector_type,omitempty"`
}

// workflowBranchOut is one branch as a model sees it: when it sends and to
// which channels. Its message is not shown.
type workflowBranchOut struct {
	// Number is the branch's 1-based place, as plan_workflow_change's keep
	// names it.
	Number   int      `json:"number"`
	When     string   `json:"when"`
	Channels []string `json:"channels"`
}

// workflowStepOut is a step that runs before the query (or before the one
// message of a workflow with no query) as a model sees it: the pipeline it
// runs, the model it refreshes or the chart or dashboard it draws, by name and
// id, or how long it waits, and the steps it runs after.
type workflowStepOut struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`
	After []string `json:"after"`
	// Rule is when it runs after them: all_success, all_done or any_failed.
	// "" for a step that runs first.
	Rule      string          `json:"rule,omitempty"`
	Pipeline  *workflowRefOut `json:"pipeline,omitempty"`
	Model     *workflowRefOut `json:"model,omitempty"`
	Chart     *workflowRefOut `json:"chart,omitempty"`
	Dashboard *workflowRefOut `json:"dashboard,omitempty"`
	// Minutes is a wait step's; TimeoutMinutes and Retries a run_pipeline or
	// refresh_model step's.
	Minutes        int `json:"minutes,omitempty"`
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	Retries        int `json:"retries,omitempty"`
}

type workflowDraftOut struct {
	WorkflowID string              `json:"workflow_id"`
	Status     string              `json:"status"`
	Name       string              `json:"name"`
	Connection *workflowRefOut     `json:"connection,omitempty"`
	Pipeline   *workflowRefOut     `json:"pipeline,omitempty"`
	Trigger    string              `json:"trigger"`
	Steps      []workflowStepOut   `json:"steps"`
	StepsRule  string              `json:"steps_rule,omitempty"`
	Branches   []workflowBranchOut `json:"branches"`
	URL        string              `json:"url"`
	// Check is a fixed sentence: whether the query ran when it was drafted.
	Check string `json:"check"`
	// Next is a fixed sentence: what to tell the person.
	Next string `json:"next"`
}

// madeResourceID is the draft plan_workflow stored, for its audit row and for
// the assistant's workflow card.
func (w workflowDraftOut) madeResourceID() string { return w.WorkflowID }

type listWorkflowsOut struct {
	Workflows []workflowOut `json:"workflows"`
	// Truncated says the workspace has more than the newest
	// handlers.WorkflowsForModelMax shown.
	Truncated bool `json:"truncated"`
}

type workflowOut struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Status     string              `json:"status"`
	Connection *workflowRefOut     `json:"connection,omitempty"`
	Pipeline   *workflowRefOut     `json:"pipeline,omitempty"`
	Trigger    string              `json:"trigger"`
	Steps      []workflowStepOut   `json:"steps"`
	StepsRule  string              `json:"steps_rule,omitempty"`
	Branches   []workflowBranchOut `json:"branches"`
	NextRunAt  *time.Time          `json:"next_run_at,omitempty"`
	URL        string              `json:"url"`
}

type workflowChangeOut struct {
	ChangeID     string `json:"change_id"`
	WorkflowID   string `json:"workflow_id"`
	WorkflowName string `json:"workflow_name"`
	Summary      string `json:"summary,omitempty"`
	// Branches are the workflow's branches as the change would leave them.
	Branches []workflowBranchOut `json:"branches"`
	URL      string              `json:"url"`
	// Check and Next are fixed sentences, as on a draft.
	Check string `json:"check"`
	Next  string `json:"next"`
}

// madeResourceID is the change plan_workflow_change stored, for its audit row
// and for the assistant's change card.
func (w workflowChangeOut) madeResourceID() string { return w.ChangeID }

var (
	workflowTriggers        = []string{workflow.TriggerSchedule, workflow.TriggerAfterPipeline, workflow.TriggerManual}
	workflowTriggerSynonyms = map[string]string{
		"cron": "schedule", "scheduled": "schedule", "daily": "schedule", "weekly": "schedule", "hourly": "schedule",
		"after": "after_pipeline", "pipeline": "after_pipeline", "after_load": "after_pipeline",
		"pipeline_finished": "after_pipeline", "on_pipeline": "after_pipeline",
		"on_demand": "manual", "run_now": "manual", "none": "manual",
	}
	workflowConditions        = []string{workflow.ConditionAlways, workflow.ConditionRowsExist, workflow.ConditionNoRows, workflow.ConditionValue}
	workflowConditionSynonyms = map[string]string{
		"none": "always", "every_time": "always", "digest": "always", "report": "always",
		"otherwise": "always", "else": "always", "default": "always",
		"any_rows": "rows_exist", "has_rows": "rows_exist", "rows": "rows_exist", "not_empty": "rows_exist",
		"empty": "no_rows", "zero_rows": "no_rows", "nothing": "no_rows",
		"threshold": "value", "number": "value", "metric": "value",
	}
	workflowStepTypes = []string{workflow.StepRunPipeline, workflow.StepRefreshModel, workflow.StepWait,
		workflow.StepChart, workflow.StepDashboard}
	workflowStepTypeSynonyms = map[string]string{
		"pipeline": "run_pipeline", "run": "run_pipeline", "load": "run_pipeline", "sync": "run_pipeline",
		"start_pipeline": "run_pipeline", "model": "refresh_model", "refresh": "refresh_model",
		"build_model": "refresh_model", "run_model": "refresh_model", "rebuild_model": "refresh_model",
		"sleep": "wait", "delay": "wait", "pause": "wait",
		"draw_chart": "chart", "graph": "chart", "plot": "chart", "attach_chart": "chart",
		"board": "dashboard", "draw_dashboard": "dashboard", "attach_dashboard": "dashboard",
	}
	workflowRules        = []string{workflow.RuleAllSuccess, workflow.RuleAllDone, workflow.RuleAnyFailed}
	workflowRuleSynonyms = map[string]string{
		"success": "all_success", "succeeded": "all_success", "all_succeeded": "all_success", "on_success": "all_success",
		"done": "all_done", "always": "all_done", "finished": "all_done", "all_finished": "all_done",
		"failed": "any_failed", "on_failure": "any_failed", "any_failure": "any_failed", "one_failed": "any_failed",
	}
	workflowOps        = []string{">", ">=", "<", "<=", "=", "!="}
	workflowOpSynonyms = map[string]string{
		"gt": ">", "greater_than": ">", "above": ">", "more_than": ">",
		"gte": ">=", "at_least": ">=",
		"lt": "<", "less_than": "<", "below": "<", "fewer_than": "<",
		"lte": "<=", "at_most": "<=",
		"eq": "=", "==": "=", "equals": "=",
		"ne": "!=", "<>": "!=", "not_equal": "!=",
	}
)

// workflowBranchArgs are the fields of one branch. A change's branch may keep
// one of the workflow's branches and set only what differs; a draft's branch
// is new, so it needs a message and channels.
func workflowBranchArgs(change bool) []Arg {
	var args []Arg
	if change {
		args = append(args, Arg{Name: "keep", Type: "integer", Min: 0, Max: workflow.MaxBranches, Default: 0,
			Description: "The condition's number (from list_workflows) to start from: its test, message and channels; a " +
				"field you also set replaces that part. Out for a new one."})
	}
	return append(args,
		Arg{Name: "condition", Type: "string", Enum: workflowConditions, Synonyms: workflowConditionSynonyms,
			Description: "always (the last only: a digest, or the otherwise), rows_exist (an exception list), no_rows " +
				"(nothing arrived), value (a number vs threshold). Default always."},
		Arg{Name: "condition_column", Type: "string", MaxLength: 63,
			Description: "For value: the column to compare. Default the first column."},
		Arg{Name: "condition_op", Type: "string", Enum: workflowOps, Synonyms: workflowOpSynonyms,
			Description: "For value: the comparison."},
		Arg{Name: "threshold", Type: "string", MaxLength: 40,
			Description: "For value: the number to compare with, such as 50 or 0.2."},
		Arg{Name: "subject", Type: "string", MaxLength: workflow.MaxSubjectLen,
			Description: "The email subject and the Slack message's first line. May use {{name}} slots."},
		Arg{Name: "message", Type: "string", Required: !change, MaxLength: workflow.MaxBodyLen, Raw: true,
			Description: "The message, short plain text with {{name}} slots: {{total_users}} for a column of the " +
				"first row, {{table}} for the rows. It shows only what its slots name."},
		Arg{Name: "channels", Type: "string", Required: !change, MaxLength: 600,
			Description: "Comma-separated delivery channel names, such as #finance, Finance team. Only names the " +
				"workspace has; a wrong name is refused with the list."},
	)
}

// maxWorkflowToolSteps is how many steps plan_workflow takes before the query.
const maxWorkflowToolSteps = 20

// workflowStepArgs are the fields of one step plan_workflow runs before the
// query, or with no query before the message.
func workflowStepArgs() []Arg {
	return []Arg{
		{Name: "id", Type: "string", Required: true, MaxLength: 40,
			Description: "A short id for the step, lower-case letters, digits and _, such as load_orders. Another " +
				"step's after names it."},
		{Name: "type", Type: "string", Required: true, Enum: workflowStepTypes, Synonyms: workflowStepTypeSynonyms,
			Description: "run_pipeline runs a batch pipeline and waits; refresh_model builds a model, runs its checks; " +
				"wait pauses; chart or dashboard draws a saved one into the message."},
		{Name: "after", Type: "string", MaxLength: 600,
			Description: "Comma-separated ids of the steps this one runs after. Leave it out for a step that " +
				"runs first."},
		{Name: "rule", Type: "string", Enum: workflowRules, Synonyms: workflowRuleSynonyms,
			Description: "all_success (default): each step it is after succeeded; all_done: each ended either way; " +
				"any_failed: one failed, for a cleanup or a fallback."},
		{Name: "pipeline_id", Type: "string", Format: "uuid",
			Description: "For run_pipeline: the batch pipeline to run; from list_pipelines."},
		{Name: "model_id", Type: "string", Format: "uuid",
			Description: "For refresh_model: the model to build; from list_models."},
		{Name: "chart_id", Type: "string", Format: "uuid",
			Description: "For chart: the saved chart to draw, a chart tile's chart_id from get_dashboard. Drawn as the " +
				"person who turns the workflow on, so one they see."},
		{Name: "dashboard_id", Type: "string", Format: "uuid",
			Description: "For dashboard: the dashboard to draw; from list_dashboards. It is drawn as the person " +
				"who turns the workflow on, so it must be a dashboard they see."},
		{Name: "minutes", Type: "integer", Min: 0, Max: workflow.MaxWaitMinutes, Default: 0,
			Description: "For wait: how long to wait, 1 to 1440 minutes (24 hours)."},
		{Name: "timeout_minutes", Type: "integer", Min: 0, Max: workflow.MaxStepTimeoutMinutes, Default: 0,
			Description: "For run_pipeline and refresh_model: how long the step waits for its run, retries " +
				"included, before it stops the run and fails. Default 720 (12 hours)."},
		{Name: "retries", Type: "integer", Min: 0, Max: workflow.MaxRetries, Default: 0,
			Description: "For run_pipeline: how many times to start the pipeline again when its run fails, 0 to " +
				"3. Default 0. A model is never retried."},
	}
}

func workflowTools(h Handlers) []*Tool {
	return []*Tool{
		{
			Name:  "plan_workflow",
			Title: "Draft a workflow",
			Description: "Drafts a workflow for the person to test and turn on in rsync.ai: on a trigger, it runs one " +
				"read-only SQL query on a connection and sends a message to the workspace's Slack or email " +
				"channels when a condition on the result is true. Rules: read_guide('workflows').",
			Guide: "workflows",
			Args: []Arg{
				{Name: "name", Type: "string", Required: true, MaxLength: workflow.MaxNameLen,
					Description: "A short name, such as Weekly revenue digest."},
				{Name: "connection_id", Type: "string", Format: "uuid",
					Description: "The SQL connection the query runs on; from list_connections. Left out only " +
						"with steps and no query."},
				{Name: "sql", Type: "string", MaxLength: workflow.MaxSQLLen, Raw: true,
					Description: "One read-only SELECT (or WITH … SELECT) in the connection's dialect, naming as a column every " +
						"number the message states. Left out only with connection_id."},
				{Name: "trigger", Type: "string", Required: true, Enum: workflowTriggers, Synonyms: workflowTriggerSynonyms,
					Description: "schedule runs on a cron; after_pipeline runs each time a pipeline finishes; manual " +
						"runs only when the person clicks Run now."},
				{Name: "cron", Type: "string", MaxLength: 100,
					Description: "For schedule: five-field cron, such as 0 9 * * 1 for Mondays at 09:00."},
				{Name: "timezone", Type: "string", MaxLength: 64,
					Description: "For schedule: an IANA timezone such as Europe/Berlin. Default UTC."},
				{Name: "pipeline_id", Type: "string", Format: "uuid",
					Description: "For after_pipeline: the pipeline; from list_pipelines."},
				{Name: "branches", Type: "array", Required: true, MaxItems: workflow.MaxBranches,
					Items: workflowBranchArgs(false),
					Description: "The conditions, top to bottom; the first that is true sends. Most workflows " +
						"have one."},
				{Name: "quiet_minutes", Type: "integer", Min: 0, Max: workflow.MaxQuietMinutes, Default: 0,
					Description: "For an alert: after a condition sends, it does not send again for this many " +
						"minutes. 0 sends every time."},
				{Name: "steps", Type: "array", MaxItems: maxWorkflowToolSteps, Items: workflowStepArgs(),
					Description: "Steps that run before the query (or, with no query, before the message), each when the steps its " +
						"after names end, by its rule. Leave it out for none."},
				{Name: "steps_rule", Type: "string", Enum: workflowRules, Synonyms: workflowRuleSynonyms,
					Description: "With steps: when the query (or, with no query, the message) runs after the last steps: " +
						"all_success (default), all_done or any_failed, as for a step's rule."},
			},
			Action:       services.ActionWorkflowWrite,
			Write:        true,
			ResourceType: "workflow",
			Output:       workflowDraftOut{},
			run: func(call *toolCall) (any, error) {
				return planWorkflow(call, h)
			},
		},
		{
			Name:  "list_workflows",
			Title: "List workflows",
			Description: "Lists the workspace's workflows, newest first: each one's id, name, status, trigger, connection, " +
				"steps, and its conditions top to bottom with when each sends and where. Never shows a workflow's " +
				"SQL or messages. Rules: read_guide('workflows').",
			Guide:        "workflows",
			Action:       services.ActionWorkflowRead,
			ResourceType: "workflow",
			Output:       listWorkflowsOut{},
			run: func(call *toolCall) (any, error) {
				return listWorkflows(call, h)
			},
		},
		{
			Name:  "plan_workflow_change",
			Title: "Propose a change to a workflow's conditions",
			Description: "Proposes a change to an existing workflow's conditions, for the person to apply in rsync.ai: " +
				"add, remove or reorder conditions, or change one's test, threshold, message or channels. It " +
				"never edits the query, trigger, schedule or steps. Rules: read_guide('workflows').",
			Guide: "workflows",
			Args: []Arg{
				{Name: "workflow_id", Type: "string", Format: "uuid", Required: true,
					Description: "The workflow to change; from list_workflows."},
				{Name: "branches", Type: "array", Required: true, MaxItems: workflow.MaxBranches,
					Items:       workflowBranchArgs(true),
					Description: "Every condition the workflow should have after the change, top to bottom."},
				{Name: "summary", Type: "string", MaxLength: 300,
					Description: "One sentence for the person: what the change does, such as Adds a Slack alert " +
						"above 100 failures."},
			},
			Action:       services.ActionWorkflowWrite,
			Write:        true,
			ResourceType: "workflow_change",
			Output:       workflowChangeOut{},
			run: func(call *toolCall) (any, error) {
				return planWorkflowChange(call, h)
			},
		},
	}
}

// branchInputs turns a checked branches argument into the handler's input.
func branchInputs(items []map[string]any) []handlers.WorkflowBranchInput {
	out := make([]handlers.WorkflowBranchInput, 0, len(items))
	for _, it := range items {
		channels := splitNames(itemStr(it, "channels"))
		out = append(out, handlers.WorkflowBranchInput{
			Keep:            itemNum(it, "keep"),
			Condition:       itemStr(it, "condition"),
			ConditionColumn: itemStr(it, "condition_column"),
			ConditionOp:     itemStr(it, "condition_op"),
			Threshold:       itemStr(it, "threshold"),
			Subject:         itemStr(it, "subject"),
			Message:         itemStr(it, "message"),
			Channels:        channels,
		})
	}
	return out
}

// stepInputs turns a checked steps argument into the handler's input.
func stepInputs(items []map[string]any) []handlers.WorkflowStepInput {
	if len(items) == 0 {
		return nil
	}
	out := make([]handlers.WorkflowStepInput, 0, len(items))
	for _, it := range items {
		out = append(out, handlers.WorkflowStepInput{
			ID:             itemStr(it, "id"),
			Type:           itemStr(it, "type"),
			After:          splitNames(itemStr(it, "after")),
			Rule:           itemStr(it, "rule"),
			PipelineID:     itemStr(it, "pipeline_id"),
			ModelID:        itemStr(it, "model_id"),
			Minutes:        itemNum(it, "minutes"),
			TimeoutMinutes: itemNum(it, "timeout_minutes"),
			Retries:        itemNum(it, "retries"),
			ChartID:        itemStr(it, "chart_id"),
			DashboardID:    itemStr(it, "dashboard_id"),
		})
	}
	return out
}

// splitNames is a comma-separated list's non-empty names, trimmed.
func splitNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// workflowRefusal is a handler's typed refusal as the agent reads it.
func workflowRefusal(err error) error {
	var refusal *handlers.PlanRefusal
	if errors.As(err, &refusal) {
		return &toolError{status: refusal.Status, outcome: refusal.Code, message: cleanText(refusal.Message)}
	}
	return err
}

func planWorkflow(call *toolCall, h Handlers) (any, error) {
	if h.ProposeWorkflowDraft == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Drafting workflows is not available on this rsync.ai."}
	}
	actor := call.s.opts.Actor(call.c)
	w, err := h.ProposeWorkflowDraft(call.c, actor, handlers.WorkflowDraftInput{
		Name:         call.str("name"),
		ConnectionID: call.str("connection_id"),
		SQL:          call.str("sql"),
		Trigger:      call.str("trigger"),
		Cron:         call.str("cron"),
		Timezone:     call.str("timezone"),
		PipelineID:   call.str("pipeline_id"),
		Branches:     branchInputs(call.items("branches")),
		QuietMinutes: call.num("quiet_minutes"),
		Steps:        stepInputs(call.items("steps")),
		StepsRule:    call.str("steps_rule"),
	})
	if err != nil {
		return nil, workflowRefusal(err)
	}
	return shapeWorkflowDraft(*w, actor.Channel), nil
}

func listWorkflows(call *toolCall, h Handlers) (any, error) {
	if h.ListWorkflows == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Workflows are not available on this rsync.ai."}
	}
	list, err := h.ListWorkflows(call.c, call.s.opts.Actor(call.c))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := listWorkflowsOut{Workflows: []workflowOut{}}
	if len(list) > handlers.WorkflowsForModelMax {
		list, out.Truncated = list[:handlers.WorkflowsForModelMax], true
	}
	for _, w := range list {
		o := workflowOut{
			ID:        w.ID,
			Name:      w.Name,
			Status:    ident(w.Status),
			Trigger:   w.TriggerDescription,
			Branches:  workflowBranches(*w),
			NextRunAt: w.NextRunAt,
			URL:       w.URL,
		}
		o.Steps, o.StepsRule = workflowSteps(*w)
		if w.Connection != nil {
			o.Connection = &workflowRefOut{ID: w.Connection.ID, Name: w.Connection.Name, ConnectorType: ident(w.Connection.ConnectorType)}
		}
		if w.Pipeline != nil {
			o.Pipeline = &workflowRefOut{ID: w.Pipeline.ID, Name: w.Pipeline.Name}
		}
		out.Workflows = append(out.Workflows, o)
	}
	return out, nil
}

func planWorkflowChange(call *toolCall, h Handlers) (any, error) {
	if h.ProposeWorkflowChange == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Changing workflows is not available on this rsync.ai."}
	}
	actor := call.s.opts.Actor(call.c)
	ch, err := h.ProposeWorkflowChange(call.c, actor, call.str("workflow_id"),
		branchInputs(call.items("branches")), call.str("summary"))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := workflowChangeOut{
		ChangeID:     ch.ID,
		WorkflowID:   ch.WorkflowID,
		WorkflowName: ch.WorkflowName,
		Summary:      ch.Summary,
		Branches:     []workflowBranchOut{},
		URL:          ch.URL,
	}
	if ch.Proposed != nil {
		out.Branches = workflowBranches(*ch.Proposed)
	}
	if ch.QueryUnchecked {
		out.Check = "Unchecked: the query was still running when the check stopped waiting. rsync.ai checks it " +
			"again when the person clicks Apply."
	} else {
		out.Check = "The workflow's query ran once and returns a column for every slot and condition."
	}
	switch {
	case actor.Channel == services.ChannelAssistant:
		out.Next = "Nothing has changed yet. The person sees this change as a card under your answer, comparing the " +
			"conditions now with after, and clicks Apply within 24 hours. Tell them that; only they can apply it."
	default:
		out.Next = "Nothing has changed yet. Give the person the url: they open it in rsync.ai, compare the " +
			"conditions and click Apply within 24 hours. Only they can; no tool can."
	}
	return out, nil
}

// workflowSteps is w's process and drawing steps as a model sees them, and the
// rule the query (or the one send of a workflow with no query) runs after them
// by. A step whose pipeline, model, chart or dashboard is gone, or not in the workspace,
// shows its id with no name.
func workflowSteps(w handlers.Workflow) ([]workflowStepOut, string) {
	var steps []*workflow.Step
	for i := range w.Definition.Steps {
		if s := &w.Definition.Steps[i]; s.IsProcess() || s.IsDrawing() {
			steps = append(steps, s)
		}
	}
	out := make([]workflowStepOut, 0, len(steps))
	if len(steps) == 0 {
		return out, ""
	}
	isStep := map[string]bool{}
	for _, s := range steps {
		isStep[s.ID] = true
	}
	ref := func(id string, s *workflow.Step) *workflowRefOut {
		r := &workflowRefOut{ID: id}
		if known, ok := w.StepRefs[s.ID]; ok {
			r.Name = known.Name
		}
		return r
	}
	for _, s := range steps {
		o := workflowStepOut{ID: s.ID, Type: s.Type, After: append([]string{}, s.After...), Minutes: s.Minutes,
			TimeoutMinutes: s.TimeoutMinutes, Retries: s.Retries}
		if len(s.After) > 0 {
			o.Rule = stepRule(s.Rule)
		}
		switch s.Type {
		case workflow.StepRunPipeline:
			o.Pipeline = ref(s.PipelineID, s)
		case workflow.StepRefreshModel:
			o.Model = ref(s.ModelID, s)
		case workflow.StepChart:
			o.Chart = ref(s.ChartID, s)
		case workflow.StepDashboard:
			o.Dashboard = ref(s.DashboardID, s)
		}
		out = append(out, o)
	}
	next := w.Definition.QueryStep()
	if next == nil {
		if ss := w.Definition.SendSteps(); len(ss) == 1 {
			next = ss[0]
		}
	}
	rule := ""
	if next != nil {
		for _, p := range next.After {
			if isStep[p] {
				rule = stepRule(next.Rule)
				break
			}
		}
	}
	return out, rule
}

// stepRule is a stored rule as a model reads it: "" is all_success.
func stepRule(r string) string {
	if r == "" {
		return workflow.RuleAllSuccess
	}
	return r
}

// workflowBranches is w's branches as a model sees them: when each sends and
// to which channels, never its message. A workflow with steps and no query
// has no condition: its one send is one branch, sent when its steps finish.
func workflowBranches(w handlers.Workflow) []workflowBranchOut {
	branches := w.Definition.Branches()
	out := make([]workflowBranchOut, len(branches))
	byNode := map[string]int{}
	for i, b := range branches {
		when := workflow.DescribeBranch(b, i, len(branches))
		out[i] = workflowBranchOut{Number: i + 1, When: when, Channels: []string{}}
		byNode[handlers.BranchSendID(w.Definition, i)] = i
	}
	if w.Definition.ConditionStep() == nil {
		if id := handlers.BranchSendID(w.Definition, 0); id != "" {
			out = []workflowBranchOut{{Number: 1, When: "send when its steps finish", Channels: []string{}}}
			byNode[id] = 0
		}
	}
	for _, ch := range w.Channels {
		if i, ok := byNode[ch.NodeID]; ok {
			out[i].Channels = append(out[i].Channels, ch.Name)
		}
	}
	return out
}

func shapeWorkflowDraft(w handlers.Workflow, channel services.Channel) workflowDraftOut {
	out := workflowDraftOut{
		WorkflowID: w.ID,
		Status:     ident(w.Status),
		Name:       w.Name,
		Trigger:    w.TriggerDescription,
		Branches:   workflowBranches(w),
		URL:        w.URL,
	}
	out.Steps, out.StepsRule = workflowSteps(w)
	if w.Connection != nil {
		out.Connection = &workflowRefOut{ID: w.Connection.ID, Name: w.Connection.Name, ConnectorType: ident(w.Connection.ConnectorType)}
	}
	if w.Pipeline != nil {
		out.Pipeline = &workflowRefOut{ID: w.Pipeline.ID, Name: w.Pipeline.Name}
	}
	switch {
	case w.Definition.QueryStep() == nil:
		out.Check = "The workflow has no query, so there was nothing to run: its steps and message were checked."
	case w.DraftUnchecked:
		out.Check = "Unchecked: the query was still running when the check stopped waiting. Tell the person " +
			"to click Test, which runs it in full."
	default:
		out.Check = "The query ran once and returns a column for every slot."
	}
	switch {
	case channel == services.ChannelAssistant:
		out.Next = "Nothing runs or sends yet. The person sees this draft as a card under your answer, where they " +
			"click Test to see the query's result and the message, then Turn on. Tell them that; only they can turn it on."
	default:
		out.Next = "Nothing runs or sends yet. Give the person the url: they open it in rsync.ai, click Test to see " +
			"the result and the message, then Turn on. Only they can; no tool can."
	}
	return out
}
