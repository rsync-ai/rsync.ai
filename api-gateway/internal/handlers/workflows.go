package handlers

// Workflows, slice 1 (migration 130, docs/internal/workflows-design.md § 0): a
// trigger, one read-only SQL query on a workspace connection, a condition on
// its result and a message to one or more delivery channels. A workflow never
// moves data between connections; that is a pipeline's job.
//
// Ask rsync.ai or an agent token drafts one (ProposeWorkflowDraft, the
// plan_workflow tool). Drafting runs the query once, to refuse SQL that does
// not run and slots its result has no column for; only the scrubbed error or
// the column names go back to the model, never a row. A draft sends nothing.
// The person opens it in rsync.ai, tests it (the query runs and the message is
// shown to them, not sent, and not to any model) and turns it on. Turning on, running,
// pausing and deleting are REST calls with the person's session, checked as
// workflow.write (owner or admin).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	// workflowDraftsMax caps one person's drafts in a workspace, so a looping
	// agent cannot fill the table.
	workflowDraftsMax = 20
	// workflowPreviewRows is how many rows a test shows the person.
	workflowPreviewRows = 20
)

// WorkflowDraftInput is what plan_workflow asks for.
type WorkflowDraftInput struct {
	Name         string
	ConnectionID string
	SQL          string
	Trigger      string
	Cron         string
	Timezone     string
	PipelineID   string
	// Branches are checked top to bottom on the query's result; the first
	// whose condition holds sends.
	Branches     []WorkflowBranchInput
	QuietMinutes int
	// Steps are process steps (run_pipeline, refresh_model, wait) that run
	// before the query, and with no query chart steps too. The query, or with
	// no query the one send, runs after the steps no other step runs after, by
	// StepsRule; that send carries each chart among them.
	Steps     []WorkflowStepInput
	StepsRule string
}

// WorkflowStepInput is a step as plan_workflow asks for it: a pipeline to run,
// a model to refresh, a wait or a chart to draw. Its fields are workflow.Step's.
type WorkflowStepInput struct {
	ID             string
	Type           string
	After          []string
	Rule           string
	PipelineID     string
	ModelID        string
	Minutes        int
	TimeoutMinutes int
	Retries        int
	ChartID        string
	DashboardID    string
}

// WorkflowBranchInput is one branch as plan_workflow and plan_workflow_change
// ask for it: a condition, a message and the channels it sends to.
type WorkflowBranchInput struct {
	// Keep, when above 0, is the 1-based number of one of the workflow's
	// branches to start from: its condition, message and channels. Only
	// plan_workflow_change sets it. A field also set replaces that part of
	// the kept branch, so a model can change a threshold without the
	// message, which it is never shown.
	Keep            int
	Condition       string
	ConditionColumn string
	ConditionOp     string
	Threshold       string
	Subject         string
	Message         string
	// Channels are delivery channel names, matched without regard to case.
	Channels []string
}

// WorkflowRef names a connection or pipeline a workflow points at.
type WorkflowRef struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ConnectorType string `json:"connector_type,omitempty"`
}

// WorkflowChannel is a delivery channel a workflow sends to.
type WorkflowChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// NodeID is the id of the send step that sends to it.
	NodeID string `json:"node_id"`
}

// Workflow is a workflow as the API shows it. It holds names, ids and the SQL
// the person approved, never a row value.
type Workflow struct {
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	Status               string              `json:"status"`
	PausedReason         string              `json:"paused_reason,omitempty"`
	Definition           workflow.Definition `json:"definition"`
	TriggerDescription   string              `json:"trigger_description"`
	ConditionDescription string              `json:"condition_description"`
	// BranchDescriptions describe each branch's condition, top to bottom:
	// "send when failed > 100", ..., "otherwise send".
	BranchDescriptions []string          `json:"branch_descriptions"`
	Connection         *WorkflowRef      `json:"connection"`
	Pipeline           *WorkflowRef      `json:"pipeline,omitempty"`
	Channels           []WorkflowChannel `json:"channels"`
	// StepRefs names each run_pipeline step's pipeline, refresh_model step's
	// model and chart step's chart, by step id; a step whose pipeline, model
	// or chart was deleted has none.
	StepRefs   map[string]WorkflowRef `json:"step_refs,omitempty"`
	NextRunAt  *time.Time             `json:"next_run_at,omitempty"`
	ProposedBy string                 `json:"proposed_by"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	LastRun    *WorkflowRun           `json:"last_run,omitempty"`
	URL        string                 `json:"url"`
	// DraftUnchecked is set only on the draft ProposeWorkflowDraft just
	// stored, when its query was still running at the check's deadline.
	DraftUnchecked bool `json:"-"`
}

// WorkflowRun is one run as the API shows it: outcomes, never a row value.
type WorkflowRun struct {
	ID           string `json:"id"`
	Trigger      string `json:"trigger"`
	Status       string `json:"status"`
	ConditionMet *bool  `json:"condition_met,omitempty"`
	// Branch is the 0-based branch that sent; absent when none held.
	Branch     *int              `json:"branch,omitempty"`
	RowCount   *int              `json:"row_count,omitempty"`
	Deliveries []DeliveryOutcome `json:"deliveries"`
	Notes      []string          `json:"notes"`
	Error      string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	// EarlierVersion is set on a run whose definition (the copy it started
	// with, migration 131) is not the workflow's now: it ran before an edit.
	// Definition is that copy, sent only then. Set by ListWorkflowRuns only.
	EarlierVersion bool                 `json:"earlier_version,omitempty"`
	Definition     *workflow.Definition `json:"definition,omitempty"`
	// StopRequested: someone pressed Stop and the run is ending.
	StopRequested bool `json:"stop_requested,omitempty"`
	// Steps is each step's last attempt, for a run of the steps graph (build
	// step 5); a step not in it has not started. Set on the runs list and on
	// a workflow's last_run.
	Steps []WorkflowStepRun `json:"steps,omitempty"`
}

// WorkflowStepRun is how one step of a run went: outcomes and links, never a
// row value.
type WorkflowStepRun struct {
	StepID  string `json:"step_id"`
	Type    string `json:"type"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// ExecutionID is a run_pipeline step's pipeline run.
	ExecutionID string `json:"execution_id,omitempty"`
	PipelineID  string `json:"pipeline_id,omitempty"`
	// ModelRunID is a refresh_model step's model run (saved_query_runs).
	ModelRunID string `json:"model_run_id,omitempty"`
	ModelID    string `json:"model_id,omitempty"`
	// RowCount is a query step's.
	RowCount *int `json:"row_count,omitempty"`
	// Case is the condition a condition step took, 0-based, -1 for none.
	Case       *int       `json:"case,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// workflowForbidden says why the caller may not draft a workflow.
func workflowForbidden(actor services.Actor) *PlanRefusal {
	if !actor.Role.Meets(security.WSAdmin) {
		return refusePlan("forbidden", http.StatusForbidden,
			"Only a workspace owner or admin can create workflows, because a workflow runs a query unattended and "+
				"sends its result out of rsync.ai. Ask an owner or admin.")
	}
	return refusePlan("forbidden", http.StatusForbidden,
		"This token lacks the %s scope. An owner or admin can create a token that has it.", services.ScopeWorkflowsWrite)
}

// ProposeWorkflowDraft checks a workflow Ask rsync.ai or an agent drafted and
// stores it as a draft for actor's user. Its query runs once, to check it
// (checkWorkflowDraft); a draft sends nothing until the person turns it on.
func ProposeWorkflowDraft(c *gin.Context, actor services.Actor, in WorkflowDraftInput) (*Workflow, error) {
	if actor.Channel != services.ChannelAssistant && actor.Channel != services.ChannelAgent {
		return nil, refusePlan("forbidden", http.StatusForbidden, "Only Ask rsync.ai or an agent token drafts a workflow.")
	}
	if err := services.Authorize(actor, services.ActionWorkflowWrite); err != nil {
		return nil, workflowForbidden(actor)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	ws, userID := actor.WorkspaceID, actor.UserID

	name, refusal := normalizeWorkflowName(in.Name)
	if refusal != nil {
		return nil, refusal
	}

	branches, refusal := draftBranches(in.Branches, nil)
	if refusal != nil {
		return nil, refusal
	}
	def, refusal := draftDefinition(in, branches)
	if refusal != nil {
		return nil, refusal
	}
	conn, pipeline, refusal, err := validateWorkflowDefinition(ctx, database, ws, userID, &def, actor.Role, true)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}

	channels, refusal, err := resolveBranchChannels(ctx, database, ws, in.Branches, def, nil)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	checked, refusal, err := checkWorkflowDraft(ctx, database, ws, def, actor.Role)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}

	draft := workflowDraft{name: name, def: def, conn: conn, pipeline: pipeline, channels: channels,
		proposedBy: actorKindFor(actor.Channel)}
	id, refusal, err := storeWorkflowDraft(ctx, database, ws, userID, draft)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	audit := workflowDraftAudit(draft, checked)
	audit["channel"] = string(actor.Channel)
	logAudit(c, "propose_workflow", "workflow", id, audit)
	w, err := loadWorkflow(ctx, database, ws, id)
	if err != nil {
		return nil, err
	}
	w.DraftUnchecked = !checked
	return w, nil
}

// workflowDraft is a checked workflow storeWorkflowDraft stores as a draft.
type workflowDraft struct {
	name           string
	def            workflow.Definition
	conn, pipeline *WorkflowRef
	channels       []WorkflowChannel
	// proposedBy is who drafted it: actorKindAssistant, actorKindAgent, or
	// workflowMadeByAPerson for a workflow built in the editor.
	proposedBy string
}

// workflowMadeByAPerson is the proposed_by of a workflow a person built in the
// editor (CreateWorkflow), as for a chart or a dashboard made by hand
// (migrations 143, 150).
const workflowMadeByAPerson = "none"

// storeWorkflowDraft stores d as userID's draft, with its channels, in one
// transaction, and returns its id. A person with workflowDraftsMax drafts is
// refused, whoever drafted this one.
func storeWorkflowDraft(ctx context.Context, database *sql.DB, ws, userID string, d workflowDraft) (string, *PlanRefusal, error) {
	var drafts int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflows WHERE workspace_id = $1 AND created_by = $2 AND status = 'draft'
	`, ws, userID).Scan(&drafts); err != nil {
		return "", nil, fmt.Errorf("count drafts: %w", err)
	}
	if drafts >= workflowDraftsMax {
		return "", refusePlan("too_many_drafts", http.StatusTooManyRequests,
			"You have %d workflow drafts. Turn some on or delete them before drafting more.", drafts), nil
	}

	raw, err := json.Marshal(d.def)
	if err != nil {
		return "", nil, fmt.Errorf("marshal workflow: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO workflows (workspace_id, name, status, definition, trigger_kind, connection_id,
		                       trigger_pipeline_id, created_by, proposed_by)
		VALUES ($1, $2, 'draft', $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, ws, d.name, string(raw), d.def.Trigger.Type, refID(d.conn), refID(d.pipeline), userID,
		d.proposedBy).Scan(&id); err != nil {
		return "", nil, fmt.Errorf("store workflow: %w", err)
	}
	if err := insertWorkflowChannels(ctx, tx, id, d.channels); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return id, nil, nil
}

// workflowDraftAudit is the audit row's detail for a new draft: its shape and
// whether its query was checked, never its SQL or its messages.
func workflowDraftAudit(d workflowDraft, checked bool) map[string]interface{} {
	bs := d.def.Branches()
	conditions := make([]string, len(bs))
	for i, b := range bs {
		conditions[i] = b.Condition.Type
	}
	return map[string]interface{}{
		"connection_id": refID(d.conn),
		"trigger":       d.def.Trigger.Type,
		"conditions":    conditions,
		"steps":         draftStepTypes(d.def),
		"channels":      countChannels(d.channels),
		"query_checked": checked,
	}
}

// normalizeWorkflowName collapses a workflow name's whitespace and checks its
// length.
func normalizeWorkflowName(name string) (string, *PlanRefusal) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", refusePlan("invalid_name", http.StatusBadRequest, "A workflow needs a short name, such as Weekly revenue digest.")
	}
	if len([]rune(name)) > workflow.MaxNameLen {
		return "", refusePlan("invalid_name", http.StatusBadRequest, "A workflow name may be at most %d characters.", workflow.MaxNameLen)
	}
	return name, nil
}

// draftDefinition is the definition plan_workflow asked for: the query, its
// conditions and their sends (workflow.FromBranches), with in.Steps before
// them. With steps and no query there is nothing to decide on, so the one
// branch, which must be always, becomes a send that runs when the steps end.
// The steps no other step runs after are the query's (or that send's) parents.
// A chart step goes only before that one send: a condition's sends run after
// the condition alone, so a chart in a condition's message is not drafted.
func draftDefinition(in WorkflowDraftInput, branches []workflow.Branch) (workflow.Definition, *PlanRefusal) {
	t := workflow.Trigger{Type: in.Trigger, Cron: in.Cron, Timezone: in.Timezone, PipelineID: in.PipelineID}
	noQuery := len(in.Steps) > 0 && strings.TrimSpace(in.ConnectionID) == "" && strings.TrimSpace(in.SQL) == ""
	var def workflow.Definition
	if noQuery {
		if len(branches) != 1 || (branches[0].Condition.Type != "" &&
			!strings.EqualFold(strings.TrimSpace(branches[0].Condition.Type), workflow.ConditionAlways)) {
			return def, refusePlan("invalid_branches", http.StatusBadRequest,
				"A workflow with no query has nothing to decide on, so it sends one message when its steps finish: "+
					"give exactly one branch, with condition always. To decide on data, give connection_id and sql.")
		}
		m := branches[0].Message
		def = workflow.Definition{Version: 3, Trigger: t, QuietMinutes: in.QuietMinutes, Steps: []workflow.Step{
			{ID: "send_1", Type: workflow.StepSend, Message: &m, Required: true}}}
	} else {
		def = workflow.FromBranches(t, workflow.Query{ConnectionID: in.ConnectionID, SQL: in.SQL}, branches,
			in.QuietMinutes)
	}
	if len(in.Steps) == 0 {
		return def, nil
	}
	parents := map[string]bool{}
	for _, s := range in.Steps {
		for _, p := range s.After {
			parents[strings.TrimSpace(p)] = true
		}
	}
	steps := make([]workflow.Step, 0, len(in.Steps)+len(def.Steps))
	var last []string
	for _, s := range in.Steps {
		typ := strings.ToLower(strings.TrimSpace(s.Type))
		id := strings.TrimSpace(s.ID)
		if typ != workflow.StepRunPipeline && typ != workflow.StepRefreshModel && typ != workflow.StepWait &&
			typ != workflow.StepChart && typ != workflow.StepDashboard {
			r := refusePlan("invalid_type", http.StatusBadRequest, "Step %s: steps holds only run_pipeline, "+
				"refresh_model, wait, chart and dashboard steps; the query, its conditions and the messages are "+
				"the workflow's own fields.", id)
			r.Step = id
			return def, r
		}
		if (typ == workflow.StepChart || typ == workflow.StepDashboard) && !noQuery {
			r := refusePlan("invalid_steps", http.StatusBadRequest, "Step %s: a %s goes into the one message "+
				"of a workflow with no query; with connection_id and sql, each condition sends its own message, "+
				"which carries no %s. Leave out connection_id and sql, or the %s step.", id, typ, typ, typ)
			r.Step = id
			return def, r
		}
		steps = append(steps, workflow.Step{ID: id, Type: typ, After: s.After, Rule: s.Rule, PipelineID: s.PipelineID,
			ModelID: s.ModelID, Minutes: s.Minutes, TimeoutMinutes: s.TimeoutMinutes, Retries: s.Retries,
			ChartID: s.ChartID, DashboardID: s.DashboardID})
		if !parents[id] {
			last = append(last, id)
		}
	}
	def.Steps[0].After, def.Steps[0].Rule = last, in.StepsRule
	def.Steps = append(steps, def.Steps...)
	return def, nil
}

// refID is r's id, or nil (SQL NULL) for no ref.
func refID(r *WorkflowRef) any {
	if r == nil {
		return nil
	}
	return r.ID
}

// draftStepTypes lists d's run_pipeline, refresh_model, wait, chart and
// dashboard steps' types, for the audit log.
func draftStepTypes(d workflow.Definition) []string {
	out := []string{}
	for _, s := range d.Steps {
		if s.IsProcess() || s.IsDrawing() {
			out = append(out, s.Type)
		}
	}
	return out
}

// BranchSendID is the id of the send step for branch i (0-based): condition
// i's send, or a workflow's one plain send when it has no condition. "" when
// there is none.
func BranchSendID(d workflow.Definition, i int) string {
	if s := d.CaseSend(i); s != nil {
		return s.ID
	}
	if d.ConditionStep() == nil && i == 0 {
		if ss := d.SendSteps(); len(ss) == 1 {
			return ss[0].ID
		}
	}
	return ""
}

// draftBranches builds a definition's branches from what the model sent. A
// Keep item starts from branch Keep of existing (plan_workflow_change), with
// the parts the item sets replaced.
func draftBranches(in []WorkflowBranchInput, existing *Workflow) ([]workflow.Branch, *PlanRefusal) {
	var have []workflow.Branch
	if existing != nil {
		have = existing.Definition.Branches()
	}
	out := make([]workflow.Branch, 0, len(in))
	for i, b := range in {
		var br workflow.Branch
		if b.Keep > 0 {
			if b.Keep > len(have) {
				return nil, branchRefusal(refusePlan("invalid_branches", http.StatusBadRequest,
					"keep %d names no condition: the workflow has %d. list_workflows gives each workflow's conditions.",
					b.Keep, len(have)), i, len(in))
			}
			br = have[b.Keep-1]
		}
		cond, refusal := branchCondition(b, br.Condition)
		if refusal != nil {
			return nil, branchRefusal(refusal, i, len(in))
		}
		br.Condition = cond
		if b.Keep == 0 || strings.TrimSpace(b.Subject) != "" {
			br.Message.Subject = b.Subject
		}
		if b.Keep == 0 || strings.TrimSpace(b.Message) != "" {
			br.Message.Body = b.Message
		}
		out = append(out, br)
	}
	return out, nil
}

// branchCondition is b's condition over base, the kept branch's (or none):
// a new condition type starts afresh, and a column, comparison or threshold
// b sets replaces base's. A value condition needs a threshold unless base was
// one already.
func branchCondition(b WorkflowBranchInput, base workflow.Condition) (workflow.Condition, *PlanRefusal) {
	c := base
	if t := strings.TrimSpace(b.Condition); t != "" && !strings.EqualFold(t, base.Type) {
		c = workflow.Condition{Type: t}
	}
	if col := strings.TrimSpace(b.ConditionColumn); col != "" {
		c.Column = col
	}
	if op := strings.TrimSpace(b.ConditionOp); op != "" {
		c.Op = op
	}
	isValue := strings.EqualFold(c.Type, workflow.ConditionValue)
	if isValue && (strings.TrimSpace(b.Threshold) != "" || !strings.EqualFold(base.Type, workflow.ConditionValue)) {
		t, err := workflow.ParseThreshold(b.Threshold)
		if err != nil {
			return c, refusePlan("invalid_threshold", http.StatusBadRequest,
				"A value condition needs a number to compare with; %q is not one.", b.Threshold)
		}
		c.Threshold = t
	}
	return c, nil
}

// resolveBranchChannels resolves each branch's channel names
// (resolveWorkflowChannels). A Keep item that names none keeps that branch's
// channels of existing. The result holds every branch's channels, each marked
// with the id of its branch's send step in def.
func resolveBranchChannels(ctx context.Context, database *sql.DB, ws string, in []WorkflowBranchInput,
	def workflow.Definition, existing *Workflow) ([]WorkflowChannel, *PlanRefusal, error) {
	var out []WorkflowChannel
	for i, b := range in {
		var chans []WorkflowChannel
		if b.Keep > 0 && existing != nil && len(b.Channels) == 0 {
			kept := BranchSendID(existing.Definition, b.Keep-1)
			for _, ch := range existing.Channels {
				if kept != "" && ch.NodeID == kept {
					chans = append(chans, ch)
				}
			}
			if len(chans) == 0 {
				return nil, branchRefusal(refusePlan("no_channels", http.StatusBadRequest,
					"Condition %d of the workflow has no delivery channel left, so it cannot be kept as it is: "+
						"give it channels.", b.Keep), i, len(in)), nil
			}
		} else {
			var refusal *PlanRefusal
			var err error
			chans, refusal, err = resolveWorkflowChannels(ctx, database, ws, b.Channels)
			if err != nil || refusal != nil {
				return nil, branchRefusal(refusal, i, len(in)), err
			}
		}
		node := BranchSendID(def, i)
		for _, ch := range chans {
			ch.NodeID = node
			out = append(out, ch)
		}
	}
	return out, nil, nil
}

// branchRefusal marks r as about branch i of n, naming it in the message when
// there are several: the person reads a branch as a condition.
func branchRefusal(r *PlanRefusal, i, n int) *PlanRefusal {
	if r == nil {
		return nil
	}
	r.Branch = i + 1
	if n > 1 {
		r.Message = fmt.Sprintf("Condition %d: %s", i+1, r.Message)
	}
	return r
}

// insertWorkflowChannels stores a workflow's channels, each under its send
// step, in the order given.
func insertWorkflowChannels(ctx context.Context, tx *sql.Tx, workflowID string, channels []WorkflowChannel) error {
	pos := map[string]int{}
	for _, ch := range channels {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_channels (workflow_id, node_id, channel_id, position) VALUES ($1, $2, $3, $4)
		`, workflowID, ch.NodeID, ch.ID, pos[ch.NodeID]); err != nil {
			return fmt.Errorf("store workflow channels: %w", err)
		}
		pos[ch.NodeID]++
	}
	return nil
}

// countChannels counts the distinct channels a workflow sends to, across its
// branches.
func countChannels(channels []WorkflowChannel) int {
	seen := map[string]bool{}
	for _, ch := range channels {
		seen[ch.ID] = true
	}
	return len(seen)
}

// validateWorkflowDefinition makes the checks a draft and an edit both need
// before the query runs: def is normalized in place, its connection is a SQL
// connection in ws, its SQL is one read statement role may run, each chart a
// step draws is one userID (the person saving) sees, and an after_pipeline
// trigger names a pipeline in ws. A refusal for the model (forModel) names the
// tool that lists valid ids; one for a person editing in rsync.ai names the
// field they pick from.
func validateWorkflowDefinition(ctx context.Context, database *sql.DB, ws, userID string, def *workflow.Definition,
	role security.WorkspaceRole, forModel bool) (conn, pipeline *WorkflowRef, refusal *PlanRefusal, err error) {
	hint := func(tool, field string) string {
		if forModel {
			return tool + " gives each " + field + "'s id."
		}
		return "Pick another " + field + "."
	}
	if err := workflow.Normalize(def); err != nil {
		var inv *workflow.Invalid
		if errors.As(err, &inv) {
			r := refusePlan("invalid_"+inv.Field, http.StatusBadRequest, "%s", inv.Message)
			r.Branch, r.Step = inv.Branch, inv.Step
			return nil, nil, r, nil
		}
		return nil, nil, nil, err
	}

	if q := def.QueryStep(); q != nil {
		onStep := func(r *PlanRefusal) *PlanRefusal {
			r.Step = q.ID
			return r
		}
		if _, err := uuid.Parse(q.ConnectionID); err != nil {
			return nil, nil, onStep(refusePlan("invalid_connection", http.StatusBadRequest,
				"%q is not a connection id. %s", q.ConnectionID, hint("list_connections", "connection"))), nil
		}
		conn = &WorkflowRef{ID: q.ConnectionID}
		err = database.QueryRowContext(ctx, `SELECT name, connector_type FROM connections WHERE id = $1 AND workspace_id = $2`,
			conn.ID, ws).Scan(&conn.Name, &conn.ConnectorType)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, onStep(refusePlan("connection_not_found", http.StatusNotFound,
				"No connection %s in this workspace. %s", conn.ID, hint("list_connections", "connection"))), nil
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load connection: %w", err)
		}
		if reason := workflowConnectionRefusal(conn.ConnectorType); reason != "" {
			return nil, nil, onStep(refusePlan("connection_not_sql", http.StatusUnprocessableEntity, "%s", reason)), nil
		}
		if reason := workflowSQLRefusal(q.SQL, role); reason != "" {
			return nil, nil, onStep(refusePlan("invalid_sql", http.StatusBadRequest, "%s", reason)), nil
		}
	}
	for _, s := range def.ProcessSteps() {
		if refusal, err := validateProcessStep(ctx, database, ws, *s, hint); err != nil || refusal != nil {
			return nil, nil, refusal, err
		}
	}
	for _, s := range def.ChartSteps() {
		if refusal, err := validateChartStep(ctx, database, ws, userID, *s, hint); err != nil || refusal != nil {
			return nil, nil, refusal, err
		}
	}
	for _, s := range def.DashboardSteps() {
		if refusal, err := validateDashboardStep(ctx, database, ws, userID, *s, hint); err != nil || refusal != nil {
			return nil, nil, refusal, err
		}
	}

	if def.Trigger.Type == workflow.TriggerAfterPipeline {
		if _, err := uuid.Parse(def.Trigger.PipelineID); err != nil {
			return nil, nil, refusePlan("invalid_pipeline", http.StatusBadRequest,
				"%q is not a pipeline id. %s", def.Trigger.PipelineID, hint("list_pipelines", "pipeline")), nil
		}
		pipeline = &WorkflowRef{ID: def.Trigger.PipelineID}
		err := database.QueryRowContext(ctx, `SELECT name FROM pipelines WHERE id = $1 AND workspace_id = $2`,
			pipeline.ID, ws).Scan(&pipeline.Name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, refusePlan("pipeline_not_found", http.StatusNotFound,
				"No pipeline %s in this workspace. %s", pipeline.ID, hint("list_pipelines", "pipeline")), nil
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load pipeline: %w", err)
		}
	}
	if refusal, err := validateModelOwnership(ctx, database, ws, def); err != nil || refusal != nil {
		return nil, nil, refusal, err
	}
	return conn, pipeline, nil, nil
}

// modelUpstreamsSQL lists what a model ($1) rebuilds after: the pipelines and
// models of its active after_upstream trigger, and theirs in turn, $3 deep
// (maxTriggerChainDepth, the fire path's bound), each with its name, the
// fewest triggers between, and the model's own name. Every model on the way
// is one of ws ($2), as the fire path requires of each hop
// (downstreamModelLookup).
const modelUpstreamsSQL = `
	WITH RECURSIVE up(kind, id, depth) AS (
		SELECT u.upstream_kind, COALESCE(u.upstream_pipeline_id, u.upstream_saved_query_id), 1
		FROM saved_query_schedules s
		JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
		WHERE s.saved_query_id = $1::uuid AND s.schedule_type = 'after_upstream' AND s.status = 'active'
		UNION
		SELECT u.upstream_kind, COALESCE(u.upstream_pipeline_id, u.upstream_saved_query_id), up.depth + 1
		FROM up
		JOIN saved_query_schedules s ON s.saved_query_id = up.id
		JOIN saved_queries sq ON sq.id = s.saved_query_id AND sq.workspace_id = $2::uuid
		JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
		WHERE up.kind = 'model' AND up.depth < $3 AND s.schedule_type = 'after_upstream' AND s.status = 'active'
	)
	SELECT a.kind, a.id::text, COALESCE(p.name, m.name, ''), a.depth,
		COALESCE((SELECT name FROM saved_queries WHERE id = $1::uuid), '')
	FROM (SELECT kind, id, MIN(depth) AS depth FROM up GROUP BY kind, id) a
	LEFT JOIN pipelines p ON a.kind = 'pipeline' AND p.id = a.id
	LEFT JOIN saved_queries m ON a.kind = 'model' AND m.id = a.id
`

// validateModelOwnership keeps a model's place after a pipeline or another
// model with the model, as workflows-design.md § 2 has it: its after_upstream
// trigger owns that edge, and a workflow never draws model → model. It
// refuses a refresh_model step straight after another, directly or through
// wait steps, and one whose model already rebuilds, through its trigger or
// those of the models above it, after a pipeline or model this workflow runs:
// the pipeline it runs after, a run_pipeline step's or another refresh_model
// step's. Each rebuilt the model twice a run.
func validateModelOwnership(ctx context.Context, database *sql.DB, ws string, def *workflow.Definition) (*PlanRefusal, error) {
	type source struct{ step, kind, id, does string }
	var refreshes []*workflow.Step
	var sources []source
	if def.Trigger.Type == workflow.TriggerAfterPipeline {
		sources = append(sources, source{"", upstreamKindPipeline, def.Trigger.PipelineID, "this workflow runs after it"})
	}
	for _, s := range def.ProcessSteps() {
		switch s.Type {
		case workflow.StepRunPipeline:
			sources = append(sources, source{s.ID, upstreamKindPipeline, s.PipelineID, "step " + s.ID + " runs it"})
		case workflow.StepRefreshModel:
			refreshes = append(refreshes, s)
			sources = append(sources, source{s.ID, upstreamKindModel, s.ModelID, "step " + s.ID + " refreshes it"})
		}
	}
	for _, s := range refreshes {
		if prev := refreshBefore(*def, *s); prev != "" {
			r := refusePlan("model_after_model", http.StatusUnprocessableEntity,
				"Step %s refreshes a model right after step %s refreshes one. A workflow does not order one model "+
					"after another: the model does. Set the second model's schedule to rebuild after the first, and "+
					"remove step %s.", s.ID, prev, s.ID)
			r.Step = s.ID
			return r, nil
		}
	}
	for _, s := range refreshes {
		var others []source
		for _, src := range sources {
			if src.step != s.ID {
				others = append(others, src)
			}
		}
		if len(others) == 0 {
			continue
		}
		type upstream struct {
			name  string
			depth int
		}
		ups := map[string]upstream{}
		var model string
		rows, err := database.QueryContext(ctx, modelUpstreamsSQL, s.ModelID, ws, maxTriggerChainDepth)
		if err != nil {
			return nil, fmt.Errorf("load model upstreams: %w", err)
		}
		for rows.Next() {
			var kind, id string
			var u upstream
			if err := rows.Scan(&kind, &id, &u.name, &u.depth, &model); err != nil {
				rows.Close()
				return nil, fmt.Errorf("load model upstreams: %w", err)
			}
			ups[kind+":"+id] = u
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("load model upstreams: %w", err)
		}
		rows.Close()
		for _, src := range others {
			u, ok := ups[src.kind+":"+src.id]
			if !ok {
				continue
			}
			through := "through its own trigger"
			if u.depth > 1 {
				through = "through the triggers of the models between them"
			}
			r := refusePlan("model_already_follows", http.StatusUnprocessableEntity,
				"Step %s: model %q already rebuilds after the %s %q finishes, %s, and %s, so it would rebuild "+
					"twice. Remove step %s, or change the model's trigger.", s.ID, model, src.kind, u.name, through,
				src.does, s.ID)
			r.Step = s.ID
			return r, nil
		}
	}
	return nil, nil
}

// refreshBefore is the refresh_model step s runs straight after, directly or
// through wait steps, or "". A query, condition or send between them is the
// workflow deciding something, which is its own.
func refreshBefore(def workflow.Definition, s workflow.Step) string {
	seen := map[string]bool{}
	queue := append([]string(nil), s.After...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		p := def.Step(id)
		switch {
		case p == nil:
		case p.Type == workflow.StepRefreshModel:
			return p.ID
		case p.Type == workflow.StepWait:
			queue = append(queue, p.After...)
		}
	}
	return ""
}

// validateProcessStep checks what a process step points at: a run_pipeline
// step's pipeline is a batch pipeline in ws (a CDC or polling pipeline runs
// until it is stopped, so a step would never see it finish), and a
// refresh_model step's model is a model (a table or statement query) in ws.
func validateProcessStep(ctx context.Context, database *sql.DB, ws string, s workflow.Step,
	hint func(tool, field string) string) (*PlanRefusal, error) {
	onStep := func(r *PlanRefusal) *PlanRefusal {
		r.Step = s.ID
		return r
	}
	switch s.Type {
	case workflow.StepRunPipeline:
		if _, err := uuid.Parse(s.PipelineID); err != nil {
			return onStep(refusePlan("invalid_pipeline", http.StatusBadRequest,
				"Step %s: %q is not a pipeline id. %s", s.ID, s.PipelineID, hint("list_pipelines", "pipeline"))), nil
		}
		name, mode, err := workflowStepPipeline(ctx, database, ws, s.PipelineID)
		if errors.Is(err, sql.ErrNoRows) {
			return onStep(refusePlan("pipeline_not_found", http.StatusNotFound,
				"Step %s: no pipeline %s in this workspace. %s", s.ID, s.PipelineID, hint("list_pipelines", "pipeline"))), nil
		}
		if err != nil {
			return nil, fmt.Errorf("load pipeline: %w", err)
		}
		if mode != "batch" {
			return onStep(refusePlan("pipeline_not_batch", http.StatusUnprocessableEntity,
				"Step %s: %s is a %s pipeline, which keeps running until it is stopped, so a workflow step would wait "+
					"for it forever. A workflow runs batch pipelines only; to act after a streaming pipeline, query its "+
					"destination instead.", s.ID, name, workflowPipelineModeName(mode))), nil
		}
	case workflow.StepRefreshModel:
		if _, err := uuid.Parse(s.ModelID); err != nil {
			return onStep(refusePlan("invalid_model", http.StatusBadRequest,
				"Step %s: %q is not a model id. %s", s.ID, s.ModelID, hint("list_models", "model"))), nil
		}
		var one int
		err := database.QueryRowContext(ctx, `
			SELECT 1 FROM saved_queries
			WHERE id = $1 AND workspace_id = $2 AND materialization IN ('table', 'statement')
		`, s.ModelID, ws).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return onStep(refusePlan("model_not_found", http.StatusNotFound,
				"Step %s: no model %s in this workspace. %s", s.ID, s.ModelID, hint("list_models", "model"))), nil
		}
		if err != nil {
			return nil, fmt.Errorf("load model: %w", err)
		}
	}
	return nil, nil
}

// validateChartStep checks a chart step's chart is a chart of ws that userID
// sees. A run draws it as the person the workflow runs as (runChart), who is
// the person saving an active workflow or turning one on (saveWorkflowEdit,
// ActivateWorkflow); a private chart is its owner's alone.
func validateChartStep(ctx context.Context, database *sql.DB, ws, userID string, s workflow.Step,
	hint func(tool, field string) string) (*PlanRefusal, error) {
	onStep := func(r *PlanRefusal) *PlanRefusal {
		r.Step = s.ID
		return r
	}
	if _, err := uuid.Parse(s.ChartID); err != nil {
		return onStep(refusePlan("invalid_chart", http.StatusBadRequest,
			"Step %s: %q is not a chart id. %s", s.ID, s.ChartID, hint("get_dashboard", "chart"))), nil
	}
	_, err := loadVisibleChart(ctx, database, ws, userID, s.ChartID)
	if errors.Is(err, sql.ErrNoRows) {
		return onStep(refusePlan("chart_not_found", http.StatusNotFound,
			"Step %s: no chart %s you can see in this workspace. %s", s.ID, s.ChartID,
			hint("get_dashboard", "chart"))), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}
	return nil, nil
}

// validateDashboardStep checks a dashboard step's board is a dashboard of ws
// that userID sees, as validateChartStep does a chart. A run draws each tile
// as the person the workflow runs as (runDashboard); a tile whose chart that
// person cannot see is sent as "not drawn", not refused here.
func validateDashboardStep(ctx context.Context, database *sql.DB, ws, userID string, s workflow.Step,
	hint func(tool, field string) string) (*PlanRefusal, error) {
	onStep := func(r *PlanRefusal) *PlanRefusal {
		r.Step = s.ID
		return r
	}
	if _, err := uuid.Parse(s.DashboardID); err != nil {
		return onStep(refusePlan("invalid_dashboard", http.StatusBadRequest,
			"Step %s: %q is not a dashboard id. %s", s.ID, s.DashboardID, hint("list_dashboards", "dashboard"))), nil
	}
	_, err := scanDashboard(database.QueryRowContext(ctx, dashboardSelect+` AND d.id = $3`, ws, userID,
		s.DashboardID).Scan, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return onStep(refusePlan("dashboard_not_found", http.StatusNotFound,
			"Step %s: no dashboard %s you can see in this workspace. %s", s.ID, s.DashboardID,
			hint("list_dashboards", "dashboard"))), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load dashboard: %w", err)
	}
	return nil, nil
}

// workflowStepPipeline loads a pipeline of ws: its name and its effective
// sync mode (resolveEffectiveSyncMode: batch, cdc or api_polling).
func workflowStepPipeline(ctx context.Context, database *sql.DB, ws, id string) (name, mode string, err error) {
	var src sql.NullString
	var syncMode, cdcMode *string
	if err := database.QueryRowContext(ctx, `
		SELECT name, source_connection_id::text, sync_mode, cdc_mode FROM pipelines WHERE id = $1 AND workspace_id = $2
	`, id, ws).Scan(&name, &src, &syncMode, &cdcMode); err != nil {
		return "", "", err
	}
	return name, resolveEffectiveSyncMode(database, id, src.String, syncMode, cdcMode), nil
}

// workflowPipelineModeName is a sync mode as a person reads it.
func workflowPipelineModeName(mode string) string {
	switch mode {
	case "cdc":
		return "CDC (streaming)"
	case "api_polling":
		return "polling"
	}
	return mode
}

// workflowDraftCheckTimeout bounds the one run a draft gets. It must end well
// inside Ask rsync.ai's answer deadline (2 minutes), leaving the model time to
// fix the draft and send it again. A var so a test can shorten it.
var workflowDraftCheckTimeout = 30 * time.Second

// runWorkflowCheck runs def's query once, as Test does, within
// workflowDraftCheckTimeout, and checks def against the result
// (workflow.CheckResult). The rows stay in this function: qerr is the
// database's error, inv what CheckResult refused. A query still running at
// the deadline fails nothing: checked is false, and Test runs it in full. A
// workflow with no query has nothing to run, so it is checked at once.
func runWorkflowCheck(ctx context.Context, database *sql.DB, ws string, def workflow.Definition,
	role security.WorkspaceRole) (checked bool, qerr error, inv *workflow.Invalid, err error) {
	q := def.QueryStep()
	if q == nil {
		return true, nil, nil, nil
	}
	qctx, cancel := context.WithTimeout(ctx, workflowDraftCheckTimeout)
	defer cancel()
	res, _, qerr := workflowQuery(qctx, database, ws, q.ConnectionID, q.SQL, role)
	if qerr != nil {
		if ctx.Err() != nil {
			return false, nil, nil, ctx.Err()
		}
		if errors.Is(qctx.Err(), context.DeadlineExceeded) {
			return false, nil, nil, nil
		}
		return false, qerr, nil, nil
	}
	if inv := workflow.CheckResult(def, res); inv != nil {
		return false, nil, inv, nil
	}
	return true, nil, nil, nil
}

// checkWorkflowDraft runs a draft's query once (runWorkflowCheck). Drafts on
// prod were stored with SQL that did not compile and slots no column filled,
// and failed only when the person clicked Test, after the model had moved on.
//
// A refusal goes to a model, so it carries the database's error scrubbed of
// literals (llmscrub.Scrub) or the result's column names, never a value.
func checkWorkflowDraft(ctx context.Context, database *sql.DB, ws string, def workflow.Definition,
	role security.WorkspaceRole) (checked bool, refusal *PlanRefusal, err error) {
	checked, qerr, inv, err := runWorkflowCheck(ctx, database, ws, def, role)
	if err != nil {
		return false, nil, err
	}
	if qerr != nil {
		return false, refusePlan("query_failed", http.StatusUnprocessableEntity,
			"rsync.ai ran the query once to check the draft and it failed, so nothing was stored: %s. If the "+
				"error is in the SQL, fix it (describe_schema gives the tables and columns) and draft again; "+
				"otherwise tell the person.", llmscrub.ScrubMax(qerr.Error(), workflowErrorMax)), nil
	}
	if inv != nil {
		r := refusePlan("invalid_"+inv.Field, http.StatusUnprocessableEntity, "%s", inv.Message)
		r.Branch, r.Step = inv.Branch, inv.Step
		return false, r, nil
	}
	return checked, nil, nil
}

// checkWorkflowForPerson runs def's query once (runWorkflowCheck) for a
// person in rsync.ai: they may see the database's error unscrubbed, as Test
// shows it. outcome finishes the sentence: "nothing was saved".
func checkWorkflowForPerson(ctx context.Context, database *sql.DB, ws string, def workflow.Definition,
	role security.WorkspaceRole, outcome string) (checked bool, refusal *PlanRefusal, err error) {
	checked, qerr, inv, err := runWorkflowCheck(ctx, database, ws, def, role)
	if err != nil {
		return false, nil, err
	}
	if qerr != nil {
		return false, refusePlan("query_failed", http.StatusUnprocessableEntity,
			"rsync.ai ran the query to check it and it failed, so %s: %s", outcome,
			workflow.CapRunes(qerr.Error(), workflowErrorMax)), nil
	}
	if inv != nil {
		r := refusePlan("invalid_"+inv.Field, http.StatusUnprocessableEntity, "%s Because of that, %s.",
			inv.Message, outcome)
		r.Branch, r.Step = inv.Branch, inv.Step
		return false, r, nil
	}
	return checked, nil, nil
}

// writeWorkflowRefusal answers a REST call a check refused. code names the
// field at fault, branch (1-based) the branch and step the step's id, so the
// editor can open that step.
func writeWorkflowRefusal(c *gin.Context, r *PlanRefusal) {
	body := gin.H{"error": r.Message, "code": r.Code}
	if r.Branch > 0 {
		body["branch"] = r.Branch
	}
	if r.Step != "" {
		body["step"] = r.Step
	}
	c.JSON(r.Status, body)
}

// resolveWorkflowChannels matches channel names to the workspace's delivery
// channels. An unknown name is refused with the names that exist, so the
// agent can pick one; it never creates a channel.
func resolveWorkflowChannels(ctx context.Context, database *sql.DB, ws string, names []string) ([]WorkflowChannel, *PlanRefusal, error) {
	names = uniqueTrimmed(names)
	rows, err := database.QueryContext(ctx,
		`SELECT id, name, kind FROM delivery_channels WHERE workspace_id = $1 ORDER BY lower(name)`, ws)
	if err != nil {
		return nil, nil, fmt.Errorf("list delivery channels: %w", err)
	}
	defer rows.Close()
	var all []WorkflowChannel
	for rows.Next() {
		var ch WorkflowChannel
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Kind); err != nil {
			return nil, nil, err
		}
		all = append(all, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	available := func() string {
		if len(all) == 0 {
			return "This workspace has no delivery channels yet. An owner or admin adds one (a Slack channel or an " +
				"email list) in Workspace settings > Delivery channels; then ask again."
		}
		list := make([]string, len(all))
		for i, ch := range all {
			list[i] = fmt.Sprintf("%s (%s)", ch.Name, ch.Kind)
		}
		return "This workspace's delivery channels are: " + strings.Join(list, ", ") + "."
	}
	if len(names) == 0 {
		return nil, refusePlan("no_channels", http.StatusBadRequest,
			"A workflow needs at least one delivery channel to send to. %s", available()), nil
	}
	if len(names) > workflow.MaxChannels {
		return nil, refusePlan("too_many_channels", http.StatusBadRequest,
			"A workflow can send to at most %d channels.", workflow.MaxChannels), nil
	}
	var out []WorkflowChannel
	seen := map[string]bool{}
	for _, n := range names {
		var match *WorkflowChannel
		for i := range all {
			if strings.EqualFold(all[i].Name, n) || strings.EqualFold(strings.TrimPrefix(all[i].Name, "#"), strings.TrimPrefix(n, "#")) {
				match = &all[i]
				break
			}
		}
		if match == nil {
			return nil, refusePlan("channel_not_found", http.StatusNotFound,
				"There is no delivery channel named %q. %s", n, available()), nil
		}
		if !seen[match.ID] {
			seen[match.ID] = true
			out = append(out, *match)
		}
	}
	return out, nil, nil
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

const workflowSelect = `
	SELECT w.id, w.name, w.status, COALESCE(w.paused_reason, ''), w.definition, w.next_run_at, w.proposed_by,
	       w.created_at, w.updated_at,
	       w.connection_id, COALESCE(c.name, ''), COALESCE(c.connector_type, ''),
	       w.trigger_pipeline_id, COALESCE(p.name, '')
	FROM workflows w
	LEFT JOIN connections c ON c.id = w.connection_id AND c.workspace_id = w.workspace_id
	LEFT JOIN pipelines p ON p.id = w.trigger_pipeline_id AND p.workspace_id = w.workspace_id
`

func scanWorkflow(scan func(...any) error) (*Workflow, error) {
	var (
		w                  Workflow
		raw                []byte
		next               sql.NullTime
		connID, pipelineID sql.NullString
		connName, connType string
		pipelineName       string
	)
	if err := scan(&w.ID, &w.Name, &w.Status, &w.PausedReason, &raw, &next, &w.ProposedBy, &w.CreatedAt, &w.UpdatedAt,
		&connID, &connName, &connType, &pipelineID, &pipelineName); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &w.Definition); err != nil {
		return nil, fmt.Errorf("read workflow definition: %w", err)
	}
	if next.Valid && w.Status == "active" {
		t := next.Time
		w.NextRunAt = &t
	}
	if connID.Valid {
		w.Connection = &WorkflowRef{ID: connID.String, Name: connName, ConnectorType: connType}
	}
	if w.Definition.Trigger.Type == workflow.TriggerAfterPipeline {
		w.Pipeline = &WorkflowRef{ID: pipelineID.String, Name: pipelineName}
		if !pipelineID.Valid {
			w.Pipeline = nil
		}
	}
	w.TriggerDescription = workflow.DescribeTrigger(w.Definition.Trigger, pipelineName)
	w.describeBranches()
	w.Channels = []WorkflowChannel{}
	w.URL = workflowURL(w.ID)
	return &w, nil
}

// describeBranches fills the descriptions of w's branches from its definition.
func (w *Workflow) describeBranches() {
	bs := w.Definition.Branches()
	w.ConditionDescription = workflow.DescribeBranches(bs)
	w.BranchDescriptions = make([]string, len(bs))
	for i, b := range bs {
		w.BranchDescriptions[i] = workflow.DescribeBranch(b, i, len(bs))
	}
	if w.Definition.ConditionStep() == nil && len(w.Definition.SendSteps()) > 0 {
		w.ConditionDescription = "send when its steps finish"
	}
}

func loadWorkflow(ctx context.Context, database *sql.DB, ws, id string) (*Workflow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, sql.ErrNoRows
	}
	w, err := scanWorkflow(database.QueryRowContext(ctx, workflowSelect+` WHERE w.id = $1 AND w.workspace_id = $2`, id, ws).Scan)
	if err != nil {
		return nil, err
	}
	if err := attachWorkflowExtras(ctx, database, ws, []*Workflow{w}); err != nil {
		return nil, err
	}
	return w, nil
}

// attachWorkflowExtras fills each workflow's channels, step refs and last run (with its steps).
func attachWorkflowExtras(ctx context.Context, database *sql.DB, ws string, list []*Workflow) error {
	if len(list) == 0 {
		return nil
	}
	byID := map[string]*Workflow{}
	ids := make([]string, 0, len(list))
	for _, w := range list {
		byID[w.ID] = w
		ids = append(ids, w.ID)
	}
	joined := strings.Join(ids, ",")
	rows, err := database.QueryContext(ctx, `
		SELECT wc.workflow_id, dc.id, dc.name, dc.kind, wc.node_id
		FROM workflow_channels wc JOIN delivery_channels dc ON dc.id = wc.channel_id AND dc.workspace_id = $1
		WHERE wc.workflow_id = ANY(string_to_array($2, ',')::uuid[])
		ORDER BY wc.node_id, wc.position, lower(dc.name)
	`, ws, joined)
	if err != nil {
		return err
	}
	for rows.Next() {
		var wid string
		var ch WorkflowChannel
		if err := rows.Scan(&wid, &ch.ID, &ch.Name, &ch.Kind, &ch.NodeID); err != nil {
			rows.Close()
			return err
		}
		if w := byID[wid]; w != nil {
			w.Channels = append(w.Channels, ch)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := attachStepRefs(ctx, database, ws, list); err != nil {
		return err
	}
	runs, err := database.QueryContext(ctx, `
		SELECT DISTINCT ON (r.workflow_id) r.workflow_id, `+workflowRunColumns+`
		FROM workflow_runs r
		WHERE r.workspace_id = $1 AND r.workflow_id = ANY(string_to_array($2, ',')::uuid[])
		ORDER BY r.workflow_id, r.started_at DESC
	`, ws, joined)
	if err != nil {
		return err
	}
	defer runs.Close()
	var last []*WorkflowRun
	for runs.Next() {
		var wid string
		run, err := scanWorkflowRun(func(dest ...any) error { return runs.Scan(append([]any{&wid}, dest...)...) })
		if err != nil {
			return err
		}
		if w := byID[wid]; w != nil {
			w.LastRun = run
			last = append(last, run)
		}
	}
	if err := runs.Err(); err != nil {
		return err
	}
	// The canvas marks each step of the last run, so it needs its steps too.
	return attachWorkflowStepRuns(ctx, database, ws, last)
}

// attachStepRefs names the pipelines and models each workflow's process
// steps run, and the charts and dashboards its drawing steps draw, from ws only.
func attachStepRefs(ctx context.Context, database *sql.DB, ws string, list []*Workflow) error {
	var pipelineIDs, modelIDs, chartIDs, dashboardIDs []string
	for _, w := range list {
		for _, s := range w.Definition.Steps {
			switch {
			case s.Type == workflow.StepRunPipeline && validUUID(s.PipelineID):
				pipelineIDs = append(pipelineIDs, s.PipelineID)
			case s.Type == workflow.StepRefreshModel && validUUID(s.ModelID):
				modelIDs = append(modelIDs, s.ModelID)
			case s.Type == workflow.StepChart && validUUID(s.ChartID):
				chartIDs = append(chartIDs, s.ChartID)
			case s.Type == workflow.StepDashboard && validUUID(s.DashboardID):
				dashboardIDs = append(dashboardIDs, s.DashboardID)
			}
		}
	}
	if len(pipelineIDs)+len(modelIDs)+len(chartIDs)+len(dashboardIDs) == 0 {
		return nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT 'pipeline', id::text, name FROM pipelines WHERE workspace_id = $1 AND id = ANY($2::uuid[])
		UNION ALL
		SELECT 'model', id::text, name FROM saved_queries WHERE workspace_id = $1 AND id = ANY($3::uuid[])
		UNION ALL
		SELECT 'chart', id::text, name FROM charts WHERE workspace_id = $1 AND id = ANY($4::uuid[])
		UNION ALL
		SELECT 'dashboard', id::text, name FROM dashboards WHERE workspace_id = $1 AND id = ANY($5::uuid[])
	`, ws, pgdriver.StringArray(pipelineIDs), pgdriver.StringArray(modelIDs), pgdriver.StringArray(chartIDs),
		pgdriver.StringArray(dashboardIDs))
	if err != nil {
		return err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var kind, id, name string
		if err := rows.Scan(&kind, &id, &name); err != nil {
			return err
		}
		names[kind+":"+id] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range list {
		for _, s := range w.Definition.Steps {
			var key, id string
			switch s.Type {
			case workflow.StepRunPipeline:
				key, id = "pipeline:"+s.PipelineID, s.PipelineID
			case workflow.StepRefreshModel:
				key, id = "model:"+s.ModelID, s.ModelID
			case workflow.StepChart:
				key, id = "chart:"+s.ChartID, s.ChartID
			case workflow.StepDashboard:
				key, id = "dashboard:"+s.DashboardID, s.DashboardID
			default:
				continue
			}
			name, ok := names[key]
			if !ok {
				continue
			}
			if w.StepRefs == nil {
				w.StepRefs = map[string]WorkflowRef{}
			}
			w.StepRefs[s.ID] = WorkflowRef{ID: id, Name: name}
		}
	}
	return nil
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

const workflowRunColumns = `r.id, r.trigger, r.status, r.condition_met, r.row_count, r.deliveries,
	array_to_string(r.notes, E'\x1f'), COALESCE(r.error, ''), r.started_at, r.finished_at, r.branch,
	r.stop_requested_at IS NOT NULL`

func scanWorkflowRun(scan func(...any) error) (*WorkflowRun, error) {
	var (
		r          WorkflowRun
		met        sql.NullBool
		rowCount   sql.NullInt64
		deliveries []byte
		notes      string
		finished   sql.NullTime
		branch     sql.NullInt64
	)
	if err := scan(&r.ID, &r.Trigger, &r.Status, &met, &rowCount, &deliveries, &notes, &r.Error, &r.StartedAt, &finished,
		&branch, &r.StopRequested); err != nil {
		return nil, err
	}
	if branch.Valid {
		b := int(branch.Int64)
		r.Branch = &b
	}
	if met.Valid {
		m := met.Bool
		r.ConditionMet = &m
	}
	if rowCount.Valid {
		n := int(rowCount.Int64)
		r.RowCount = &n
	}
	if finished.Valid {
		t := finished.Time
		r.FinishedAt = &t
	}
	r.Deliveries = []DeliveryOutcome{}
	if len(deliveries) > 0 {
		_ = json.Unmarshal(deliveries, &r.Deliveries)
	}
	r.Notes = splitUnit(notes)
	return &r, nil
}

// ---------------------------------------------------------------------------
// REST
// ---------------------------------------------------------------------------

// workflowRequest resolves the caller and the workspace and checks the role.
func workflowRequest(c *gin.Context, min security.WorkspaceRole) (userID, ws string, role security.WorkspaceRole,
	database *sql.DB, ok bool) {
	if userID, ok = resolveUserID(c); !ok {
		return
	}
	if ws, ok = resolveActiveWorkspace(c); !ok {
		return
	}
	if role, ok = requireWorkspaceRole(c, min); !ok {
		return
	}
	if database = db.GetDB(); database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		ok = false
	}
	return
}

func writeWorkflowLoadError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	log.WithError(err).Error("[workflows] load failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the workflow"})
}

// ListWorkflows lists the workspace's workflows.
// GET /api/v1/workflows
func ListWorkflows(c *gin.Context) {
	_, ws, role, database, ok := workflowRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	rows, err := database.QueryContext(ctx, workflowSelect+` WHERE w.workspace_id = $1 ORDER BY w.created_at DESC LIMIT 200`, ws)
	if err != nil {
		if isUndefinedTable(err) {
			c.JSON(http.StatusOK, gin.H{"workflows": []Workflow{}, "can_manage": role.Meets(security.WSAdmin)})
			return
		}
		log.WithError(err).Error("[workflows] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workflows"})
		return
	}
	var list []*Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows.Scan)
		if err != nil {
			rows.Close()
			log.WithError(err).Error("[workflows] scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workflows"})
			return
		}
		list = append(list, w)
	}
	rows.Close()
	if err := attachWorkflowExtras(ctx, database, ws, list); err != nil {
		log.WithError(err).Error("[workflows] extras failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workflows"})
		return
	}
	out := make([]Workflow, 0, len(list))
	for _, w := range list {
		out = append(out, *w)
	}
	c.JSON(http.StatusOK, gin.H{"workflows": out, "can_manage": role.Meets(security.WSAdmin)})
}

// GetWorkflow returns one workflow.
// GET /api/v1/workflows/:id
func GetWorkflow(c *gin.Context) {
	_, ws, role, database, ok := workflowRequest(c, security.WSViewer)
	if !ok {
		return
	}
	w, err := loadWorkflow(c.Request.Context(), database, ws, c.Param("id"))
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workflow": w, "can_manage": role.Meets(security.WSAdmin)})
}

// workflowUpdateRequest is a whole workflow as the editor saves it.
type workflowUpdateRequest struct {
	Name       string              `json:"name"`
	Definition workflow.Definition `json:"definition"`
	// NodeChannelIDs are each send step's delivery channel ids, keyed by the
	// step's id, in the order the editor shows them.
	NodeChannelIDs map[string][]string `json:"node_channel_ids"`
	// BranchChannelIDs are each branch's delivery channel ids, one list per
	// branch, as the editor sent them before step 5. Ignored when
	// NodeChannelIDs is set.
	BranchChannelIDs [][]string `json:"branch_channel_ids"`
	// ChannelIDs is the one list of a workflow with one branch, as the editor
	// sent it before branches. Ignored when either of the above is set.
	ChannelIDs []string `json:"channel_ids"`
	// UpdatedAt is the workflow's updated_at when the editor loaded it. A
	// workflow changed since is not overwritten.
	UpdatedAt time.Time `json:"updated_at"`
}

// UpdateWorkflow saves a person's edit of a workflow: its name, definition
// and channels, replaced whole. It makes every check a draft gets, the query
// run included (validateWorkflowDefinition, checkWorkflowForPerson), so an
// edit cannot store what drafting would refuse. A workflow that is on stays
// on, with its next run recomputed, and runs as the editor from now on: they
// approved the query it runs. Runs started before the edit keep the
// definition they started with (workflow_runs.definition).
// PUT /api/v1/workflows/:id
func UpdateWorkflow(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	var req workflowUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "The request is not a workflow: " + err.Error()})
		return
	}
	if req.UpdatedAt.IsZero() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "updated_at is required: it is the version of the workflow you edited."})
		return
	}
	ctx := c.Request.Context()
	w, err := loadWorkflow(ctx, database, ws, c.Param("id"))
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	if !w.UpdatedAt.Equal(req.UpdatedAt) {
		c.JSON(http.StatusConflict, gin.H{"error": workflowChangedSinceLoaded})
		return
	}

	name, refusal := normalizeWorkflowName(req.Name)
	if refusal != nil {
		writeWorkflowRefusal(c, refusal)
		return
	}
	def := req.Definition
	// The shape is checked first, on a normalized copy, so an edit that
	// changes the steps is refused before any lookup. A definition that does
	// not normalize is refused below, naming its fault.
	if shaped := def.Clone(); workflow.Normalize(&shaped) == nil {
		if refusal := workflowEditShapeRefusal(w.Definition, shaped); refusal != nil {
			writeWorkflowRefusal(c, refusal)
			return
		}
	}
	conn, pipeline, refusal, err := validateWorkflowDefinition(ctx, database, ws, userID, &def, role, false)
	if err == nil && refusal == nil {
		var channels []WorkflowChannel
		channels, refusal, err = sendChannelsByID(ctx, database, ws, def, req.NodeChannelIDs, req.BranchChannelIDs,
			req.ChannelIDs)
		if err == nil && refusal == nil {
			var checked bool
			checked, refusal, err = checkWorkflowForPerson(ctx, database, ws, def, role, "nothing was saved")
			if err == nil && refusal == nil {
				saveWorkflowEdit(c, database, ws, w, userID, workflowEdit{name: name, def: def, conn: conn,
					pipeline: pipeline, channels: channels, checked: checked})
				return
			}
		}
	}
	if err != nil {
		log.WithError(err).Error("[workflows] update check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the workflow"})
		return
	}
	writeWorkflowRefusal(c, refusal)
}

// workflowCreateRequest is a workflow a person built in the editor.
type workflowCreateRequest struct {
	Name       string              `json:"name"`
	Definition workflow.Definition `json:"definition"`
	// NodeChannelIDs are each send step's delivery channel ids, keyed by the
	// step's id, in the order the editor shows them.
	NodeChannelIDs map[string][]string `json:"node_channel_ids"`
}

// CreateWorkflow stores a workflow a person built in the editor, with no
// model, as their draft (workflows-design.md § 0.8). It is a query, its
// conditions and one message for each (workflowBuiltShapeRefusal); a
// pipeline, model, wait, chart or dashboard step is drafted by Ask rsync.ai.
// It makes every check an edit makes, the query run included, and is held to
// the same drafts cap as a drafted workflow. It sends nothing until the person
// turns it on.
// POST /api/v1/workflows
func CreateWorkflow(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	var req workflowCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "The request is not a workflow: " + err.Error()})
		return
	}
	ctx := c.Request.Context()
	name, refusal := normalizeWorkflowName(req.Name)
	if refusal != nil {
		writeWorkflowRefusal(c, refusal)
		return
	}
	def := req.Definition
	// As in UpdateWorkflow, the shape is checked on a normalized copy before
	// any lookup; a definition that does not normalize is refused below.
	if shaped := def.Clone(); workflow.Normalize(&shaped) == nil {
		if refusal := workflowBuiltShapeRefusal(shaped); refusal != nil {
			writeWorkflowRefusal(c, refusal)
			return
		}
	}
	// A message with no channels is refused naming its step, whatever came.
	byNode := req.NodeChannelIDs
	if byNode == nil {
		byNode = map[string][]string{}
	}
	conn, pipeline, refusal, err := validateWorkflowDefinition(ctx, database, ws, userID, &def, role, false)
	if err == nil && refusal == nil {
		var channels []WorkflowChannel
		channels, refusal, err = sendChannelsByID(ctx, database, ws, def, byNode, nil, nil)
		if err == nil && refusal == nil {
			var checked bool
			checked, refusal, err = checkWorkflowForPerson(ctx, database, ws, def, role, "nothing was saved")
			if err == nil && refusal == nil {
				draft := workflowDraft{name: name, def: def, conn: conn, pipeline: pipeline, channels: channels,
					proposedBy: workflowMadeByAPerson}
				var id string
				id, refusal, err = storeWorkflowDraft(ctx, database, ws, userID, draft)
				if err == nil && refusal == nil {
					logAudit(c, "create_workflow", "workflow", id, workflowDraftAudit(draft, checked))
					w, err := loadWorkflow(ctx, database, ws, id)
					if err != nil {
						writeWorkflowLoadError(c, err)
						return
					}
					w.DraftUnchecked = !checked
					c.JSON(http.StatusCreated, gin.H{"workflow": w, "can_manage": true, "query_checked": checked})
					return
				}
			}
		}
	}
	if err != nil {
		log.WithError(err).Error("[workflows] create failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create the workflow"})
		return
	}
	writeWorkflowRefusal(c, refusal)
}

// workflowBuiltShapeRefusal refuses a workflow built in the editor that is
// more than a query, a condition step after it and one required send per
// condition after that (IsLegacyShape), the steps FromBranches makes. got is
// normalized.
func workflowBuiltShapeRefusal(got workflow.Definition) *PlanRefusal {
	if !got.IsLegacyShape() {
		return refusePlan("invalid_steps", http.StatusBadRequest,
			"A workflow built here is a query, its conditions and a message for each. A pipeline, model, wait, "+
				"chart or dashboard step is drafted by Ask rsync.ai.")
	}
	for _, s := range got.SendSteps() {
		if !s.Required {
			return sendNotRequiredRefusal(s.ID)
		}
	}
	return nil
}

// sendNotRequiredRefusal refuses a condition's message whose failed delivery
// would not fail the run. Every message a model or a person adds is required,
// as FromBranches and ReplaceBranches make it.
func sendNotRequiredRefusal(id string) *PlanRefusal {
	r := refusePlan("invalid_steps", http.StatusBadRequest,
		"Step %s's message must fail the run when it cannot be delivered, as every workflow's message does.", id)
	r.Step = id
	return r
}

// workflowEditShapeRefusal refuses an edit that changes the workflow's steps
// rather than what they hold. An edit in rsync.ai changes the trigger, the
// query, the conditions' tests, the messages and the channels; which steps
// there are, how they are wired, how many conditions there are, a process
// step's settings and a chart step's chart stay as they are. The one
// exception is a workflow that is a query, its conditions and a message for
// each, before and after: its edit may add, remove or reorder conditions,
// each with its message (workflowConditionsEditRefusal), as the editor builds
// one (CreateWorkflow). Any other workflow's conditions change through a
// proposed change (workflow_changes.go), applied through saveWorkflowEdit,
// not through UpdateWorkflow. Both definitions are normalized.
func workflowEditShapeRefusal(have, got workflow.Definition) *PlanRefusal {
	if have.IsLegacyShape() && got.IsLegacyShape() {
		return workflowConditionsEditRefusal(have, got)
	}
	if h, g := len(have.Branches()), len(got.Branches()); h != g {
		return refusePlan("invalid_branches", http.StatusBadRequest,
			"An edit keeps the workflow's %d conditions, and %d came. To add or remove a condition, ask rsync.ai.", h, g)
	}
	if h, g := len(have.Steps), len(got.Steps); h != g {
		return refusePlan("invalid_steps", http.StatusBadRequest,
			"An edit keeps the workflow's %d steps, and %d came. To add or remove a step, ask rsync.ai to draft "+
				"the workflow again.", h, g)
	}
	for i := range have.Steps {
		h, g := have.Steps[i], got.Steps[i]
		same := h.ID == g.ID && h.Type == g.Type && h.Rule == g.Rule && slices.Equal(h.After, g.After) &&
			h.Required == g.Required && ((h.Case == nil) == (g.Case == nil)) &&
			(h.Case == nil || *h.Case == *g.Case) && len(h.Conditions) == len(g.Conditions) && h.ChartID == g.ChartID &&
			h.DashboardID == g.DashboardID
		if h.IsProcess() {
			same = same && h.PipelineID == g.PipelineID && h.ModelID == g.ModelID && h.Minutes == g.Minutes &&
				h.TimeoutMinutes == g.TimeoutMinutes && h.Retries == g.Retries
		}
		if !same {
			r := refusePlan("invalid_steps", http.StatusBadRequest,
				"An edit changes what the steps hold, not the steps, and step %s came changed. To change the steps, "+
					"ask rsync.ai to draft the workflow again.", h.ID)
			r.Step = h.ID
			return r
		}
	}
	return nil
}

// workflowConditionsEditRefusal refuses an edit of a workflow that is a
// query, its conditions and a message for each (IsLegacyShape, before and
// after) unless the query and condition steps keep their ids, each message
// kept keeps whether its failed delivery fails the run, and each message added
// fails it, as ReplaceBranches makes it. Conditions may be added, removed or
// reordered; each message follows its condition by its case. Both definitions
// are normalized.
func workflowConditionsEditRefusal(have, got workflow.Definition) *PlanRefusal {
	for _, pair := range [][2]*workflow.Step{{have.QueryStep(), got.QueryStep()},
		{have.ConditionStep(), got.ConditionStep()}} {
		if h, g := pair[0], pair[1]; h.ID != g.ID {
			r := refusePlan("invalid_steps", http.StatusBadRequest,
				"An edit keeps step %s, and it came as %s.", h.ID, g.ID)
			r.Step = h.ID
			return r
		}
	}
	for _, g := range got.SendSteps() {
		if h := have.Step(g.ID); h != nil && h.Required != g.Required {
			r := refusePlan("invalid_steps", http.StatusBadRequest,
				"An edit changes what the steps hold, not the steps, and step %s came changed. To change the steps, "+
					"ask rsync.ai to draft the workflow again.", g.ID)
			r.Step = g.ID
			return r
		} else if h == nil && !g.Required {
			return sendNotRequiredRefusal(g.ID)
		}
	}
	return nil
}

// workflowChangedSinceLoaded is the answer to an edit of a stale copy.
const workflowChangedSinceLoaded = "This workflow was changed since you opened it, so your edit was not saved. " +
	"Reload it to see the change, then edit again."

// workflowEdit is a checked edit of a workflow, whole, for saveWorkflowEdit.
type workflowEdit struct {
	name           string
	def            workflow.Definition
	conn, pipeline *WorkflowRef
	channels       []WorkflowChannel
	checked        bool
	// changeID, when set, is the proposed change (workflow_changes) this edit
	// applies. It is claimed in the save's transaction, so a change is applied
	// once and only together with the edit it makes.
	changeID string
}

// saveWorkflowEdit stores a checked edit in one transaction, only if the
// workflow is still the version w was loaded at, and answers the request.
func saveWorkflowEdit(c *gin.Context, database *sql.DB, ws string, w *Workflow, userID string, e workflowEdit) {
	ctx := c.Request.Context()
	name, def, conn, pipeline, channels, checked := e.name, e.def, e.conn, e.pipeline, e.channels, e.checked
	raw, err := json.Marshal(def)
	if err != nil {
		log.WithError(err).Error("[workflows] marshal failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}
	var pipelineID interface{} = sql.NullString{}
	if pipeline != nil {
		pipelineID = pipeline.ID
	}
	// A workflow that is on stays on: its next run follows the new trigger,
	// and the editor becomes its run-as user. A draft or a paused one has no
	// next run and keeps its run-as user until it is turned on.
	var next interface{} = sql.NullTime{}
	var runAs interface{} = sql.NullString{}
	if w.Status == "active" {
		runAs = userID
		if def.Trigger.Type == workflow.TriggerSchedule {
			t, err := workflow.NextRun(def.Trigger, time.Now())
			if err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "The schedule could not be read: " + err.Error(),
					"code": "invalid_cron"})
				return
			}
			next = t
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("[workflows] update begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE workflows SET name = $3, definition = $4, trigger_kind = $5, connection_id = $6,
		       trigger_pipeline_id = $7, next_run_at = $8, run_as_user_id = COALESCE($9::uuid, run_as_user_id),
		       updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND updated_at = $10
	`, w.ID, ws, name, string(raw), def.Trigger.Type, refID(conn), pipelineID, next, runAs, w.UpdatedAt)
	if err != nil {
		log.WithError(err).Error("[workflows] update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": workflowChangedSinceLoaded})
		return
	}
	if e.changeID != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE workflow_changes SET status = 'applied', applied_at = NOW()
			WHERE id = $1 AND workspace_id = $2 AND created_by = $3 AND workflow_id = $4
			  AND status = 'proposed' AND expires_at > NOW()
		`, e.changeID, ws, userID, w.ID)
		if err != nil {
			log.WithError(err).Error("[workflows] claim change failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply the change"})
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "This change was applied or expired while it was being applied. Reload it.",
				"code": "change_changed"})
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_channels WHERE workflow_id = $1`, w.ID); err != nil {
		log.WithError(err).Error("[workflows] update channels failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}
	if err := insertWorkflowChannels(ctx, tx, w.ID, channels); err != nil {
		log.WithError(err).Error("[workflows] update channels failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}
	// The Temporal schedule follows the edit, while the UPDATE above holds the
	// row's lock (see beginScheduledWorkflowRun).
	if w.Status == "active" {
		old, now := w.Definition.Trigger, def.Trigger
		switch {
		case now.Type == workflow.TriggerSchedule && now != old:
			if !applyWorkflowSchedule(c, w.ID, now) {
				return
			}
		case old.Type == workflow.TriggerSchedule && now.Type != workflow.TriggerSchedule:
			dropWorkflowSchedule(ctx, w.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workflows] update commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the workflow"})
		return
	}

	action, detail := "update_workflow", map[string]interface{}{
		"changed":       workflowEditChanges(w, name, def, channels),
		"status":        w.Status,
		"query_checked": checked,
	}
	if e.changeID != "" {
		action, detail["change_id"] = "apply_workflow_change", e.changeID
	}
	logAudit(c, action, "workflow", w.ID, detail)
	updated, err := loadWorkflow(ctx, database, ws, w.ID)
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workflow": updated, "can_manage": true, "query_checked": checked})
}

// workflowEditChanges names the parts of w an edit changed, for the audit
// row: names of parts only, never the SQL or the message.
func workflowEditChanges(w *Workflow, name string, def workflow.Definition, channels []WorkflowChannel) []string {
	changed := []string{}
	same := func(a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	if name != w.Name {
		changed = append(changed, "name")
	}
	for _, part := range []struct {
		name string
		a, b any
	}{
		{"trigger", w.Definition.Trigger, def.Trigger},
		{"query", w.Definition.Query(), def.Query()},
		{"branches", w.Definition.Branches(), def.Branches()},
		{"quiet_minutes", w.Definition.QuietMinutes, def.QuietMinutes},
	} {
		if !same(part.a, part.b) {
			changed = append(changed, part.name)
		}
	}
	before := make([]string, len(w.Channels))
	for i, ch := range w.Channels {
		before[i] = ch.NodeID + ":" + ch.ID
	}
	after := make([]string, len(channels))
	for i, ch := range channels {
		after[i] = ch.NodeID + ":" + ch.ID
	}
	if strings.Join(before, ",") != strings.Join(after, ",") {
		changed = append(changed, "channels")
	}
	return changed
}

// sendChannelsByID resolves the delivery channels an editor picked for each
// send step of def, which Normalize has checked (workflowChannelsByID).
// byNode maps a send step's id to its channel ids. An editor from before
// step 5 sent perBranch, one list per condition, or legacy, the one list of a
// workflow with one condition; each list is its condition's send's.
func sendChannelsByID(ctx context.Context, database *sql.DB, ws string, def workflow.Definition,
	byNode map[string][]string, perBranch [][]string, legacy []string) ([]WorkflowChannel, *PlanRefusal, error) {
	sends := def.SendSteps()
	n := len(def.Branches())
	if def.ConditionStep() == nil {
		n = len(sends)
	}
	if byNode == nil {
		if perBranch == nil && n == 1 {
			perBranch = [][]string{legacy}
		}
		if len(perBranch) != n {
			return nil, refusePlan("invalid_channels", http.StatusBadRequest,
				"Send one list of delivery channels per condition: the workflow has %d conditions and %d lists came.",
				n, len(perBranch)), nil
		}
		byNode = map[string][]string{}
		for i, ids := range perBranch {
			id := BranchSendID(def, i)
			if id == "" {
				return nil, refusePlan("invalid_channels", http.StatusBadRequest,
					"This workflow has several messages: send node_channel_ids, each message's channels by its "+
						"step id."), nil
			}
			byNode[id] = ids
		}
	}
	for id := range byNode {
		if s := def.Step(id); s == nil || s.Type != workflow.StepSend {
			r := refusePlan("invalid_channels", http.StatusBadRequest,
				"node_channel_ids names %q, which is not a message step of this workflow.", id)
			r.Step = id
			return nil, r, nil
		}
	}
	var out []WorkflowChannel
	for _, s := range sends {
		chans, refusal, err := workflowChannelsByID(ctx, database, ws, byNode[s.ID])
		if err != nil {
			return nil, nil, err
		}
		if refusal != nil {
			refusal.Step = s.ID
			if s.Case != nil {
				refusal = branchRefusal(refusal, *s.Case, n)
			}
			return nil, refusal, nil
		}
		for _, ch := range chans {
			ch.NodeID = s.ID
			out = append(out, ch)
		}
	}
	return out, nil, nil
}

// workflowChannelsByID resolves the delivery channels an editor picked, by
// id, in the order picked. Each must be one of the workspace's channels.
func workflowChannelsByID(ctx context.Context, database *sql.DB, ws string, ids []string) ([]WorkflowChannel, *PlanRefusal, error) {
	ids = uniqueTrimmed(ids)
	if len(ids) == 0 {
		return nil, refusePlan("no_channels", http.StatusBadRequest, "A workflow needs at least one delivery channel to send to."), nil
	}
	if len(ids) > workflow.MaxChannels {
		return nil, refusePlan("too_many_channels", http.StatusBadRequest,
			"A workflow can send to at most %d channels.", workflow.MaxChannels), nil
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return nil, refusePlan("channel_not_found", http.StatusBadRequest, "%q is not a delivery channel id.", id), nil
		}
	}
	rows, err := database.QueryContext(ctx, `
		SELECT id, name, kind FROM delivery_channels
		WHERE workspace_id = $1 AND id = ANY(string_to_array($2, ',')::uuid[])
	`, ws, strings.Join(ids, ","))
	if err != nil {
		return nil, nil, fmt.Errorf("load delivery channels: %w", err)
	}
	defer rows.Close()
	byID := map[string]WorkflowChannel{}
	for rows.Next() {
		var ch WorkflowChannel
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Kind); err != nil {
			return nil, nil, err
		}
		byID[strings.ToLower(ch.ID)] = ch
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := make([]WorkflowChannel, 0, len(ids))
	for _, id := range ids {
		ch, ok := byID[strings.ToLower(id)]
		if !ok {
			return nil, refusePlan("channel_not_found", http.StatusNotFound,
				"A delivery channel you picked is not in this workspace any more. Pick another."), nil
		}
		out = append(out, ch)
	}
	return out, nil, nil
}

// ListWorkflowRuns returns a workflow's last 50 runs.
// GET /api/v1/workflows/:id/runs
func ListWorkflowRuns(c *gin.Context) {
	_, ws, _, database, ok := workflowRequest(c, security.WSViewer)
	if !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	rows, err := database.QueryContext(c.Request.Context(), `
		SELECT `+workflowRunColumns+`,
		       CASE WHEN r.definition IS DISTINCT FROM w.definition THEN r.definition END
		FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id AND w.workspace_id = r.workspace_id
		WHERE r.workflow_id = $1 AND r.workspace_id = $2
		ORDER BY r.started_at DESC
		LIMIT 50
	`, id, ws)
	if err != nil {
		log.WithError(err).Error("[workflows] runs failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	defer rows.Close()
	out := []WorkflowRun{}
	for rows.Next() {
		// earlier is the run's own copy when it differs from the workflow's
		// definition now; NULL when they match or the run predates the copy.
		var earlier []byte
		r, err := scanWorkflowRun(func(dest ...any) error { return rows.Scan(append(dest, &earlier)...) })
		if err != nil {
			log.WithError(err).Error("[workflows] run scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
			return
		}
		if len(earlier) > 0 {
			var def workflow.Definition
			if json.Unmarshal(earlier, &def) == nil {
				r.EarlierVersion, r.Definition = true, &def
			}
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("[workflows] runs failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	ptrs := make([]*WorkflowRun, len(out))
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := attachWorkflowStepRuns(c.Request.Context(), database, ws, ptrs); err != nil {
		log.WithError(err).Error("[workflows] run steps failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": out})
}

// attachWorkflowStepRuns fills each run's steps: each step's last attempt,
// with the pipeline or model the run's own definition named for it.
func attachWorkflowStepRuns(ctx context.Context, database *sql.DB, ws string, runs []*WorkflowRun) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(runs))
	byID := make(map[string]*WorkflowRun, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
		byID[r.ID] = r
	}
	rows, err := database.QueryContext(ctx, `
		SELECT DISTINCT ON (s.run_id, s.step_id)
		       s.run_id::text, s.step_id, s.step_type, s.attempt, s.status, COALESCE(s.execution_id, ''),
		       COALESCE(s.saved_query_run_id::text, ''), s.row_count, s.case_index, s.started_at, s.finished_at,
		       COALESCE(s.error, ''), COALESCE(s.note, ''),
		       COALESCE(st.d->>'pipeline_id', ''), COALESCE(st.d->>'model_id', '')
		FROM workflow_step_runs s
		JOIN workflow_runs r ON r.id = s.run_id
		LEFT JOIN LATERAL (
			SELECT e AS d
			FROM jsonb_array_elements(CASE WHEN jsonb_typeof(r.definition->'steps') = 'array'
			                               THEN r.definition->'steps' ELSE '[]'::jsonb END) e
			WHERE e->>'id' = s.step_id
			LIMIT 1
		) st ON TRUE
		WHERE s.run_id = ANY($1::uuid[]) AND s.workspace_id = $2
		ORDER BY s.run_id, s.step_id, s.attempt DESC
	`, pgdriver.StringArray(ids), ws)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			runID             string
			st                WorkflowStepRun
			rowCount, caseIdx sql.NullInt64
			finished          sql.NullTime
			pipelineID, model string
		)
		if err := rows.Scan(&runID, &st.StepID, &st.Type, &st.Attempt, &st.Status, &st.ExecutionID, &st.ModelRunID,
			&rowCount, &caseIdx, &st.StartedAt, &finished, &st.Error, &st.Note, &pipelineID, &model); err != nil {
			return err
		}
		if rowCount.Valid {
			n := int(rowCount.Int64)
			st.RowCount = &n
		}
		if caseIdx.Valid {
			n := int(caseIdx.Int64)
			st.Case = &n
		}
		if finished.Valid {
			t := finished.Time
			st.FinishedAt = &t
		}
		switch st.Type {
		case workflow.StepRunPipeline:
			st.PipelineID = pipelineID
		case workflow.StepRefreshModel:
			st.ModelID = model
		}
		if r := byID[runID]; r != nil {
			r.Steps = append(r.Steps, st)
		}
	}
	return rows.Err()
}

// workflowTestNotes says what a test leaves out of a run: it runs no process
// step, and it draws no chart, so a message it shows lacks the charts a run puts
// in it.
func workflowTestNotes(d workflow.Definition) []string {
	notes := []string{}
	if len(d.ProcessSteps()) > 0 {
		n := "A test runs no step: no pipeline runs, no model is refreshed and nothing waits."
		if d.QueryStep() != nil {
			n += " The query ran on the data as it is now."
		}
		notes = append(notes, n)
	}
	for _, s := range d.Steps {
		if s.Type == workflow.StepChart {
			notes = append(notes, "A test draws no chart: a run puts each chart step's chart in the messages after it, and the messages here leave it out.")
			break
		}
	}
	for _, s := range d.Steps {
		if s.Type == workflow.StepDashboard {
			notes = append(notes, "A test draws no dashboard: a run puts each dashboard step's tiles and PDF in the messages after it, and the messages here leave them out.")
			break
		}
	}
	return notes
}

// WorkflowPreviewMessage is one channel's message as a test renders it.
type WorkflowPreviewMessage struct {
	Channel string `json:"channel"`
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
	// Step is the id of the send step whose message this is.
	Step string `json:"step"`
	// Branch is the 0-based condition whose message this is; absent for a
	// send that follows no condition.
	Branch *int `json:"branch,omitempty"`
}

// TestWorkflow runs the workflow's query and shows the person what a run
// would send. Nothing is sent and nothing is recorded but an audit row; the
// rows go to this person's browser only, never to a model. Every send step's
// message is rendered, so the person sees each; branch names the condition a
// run would send for now (absent when none holds). A test runs no step: no
// pipeline runs, no model is refreshed, nothing waits and no chart is drawn.
// POST /api/v1/workflows/:id/test
func TestWorkflow(c *gin.Context) {
	_, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	w, err := loadWorkflow(ctx, database, ws, c.Param("id"))
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	notes := workflowTestNotes(w.Definition)
	var res workflow.Result
	var ms int64
	resp := gin.H{}
	if q := w.Definition.QueryStep(); q != nil {
		if w.Connection == nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "The workflow's connection was deleted."})
			return
		}
		res, ms, err = workflowQuery(ctx, database, ws, w.Connection.ID, q.SQL, role)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		preview := res.Rows
		if len(preview) > workflowPreviewRows {
			preview = preview[:workflowPreviewRows]
		}
		resp["columns"], resp["row_count"], resp["truncated"] = res.Columns, res.RowCount, res.Truncated
		resp["execution_time_ms"], resp["rows"] = ms, preview
	}
	branches := w.Definition.Branches()
	hasCondition := w.Definition.ConditionStep() != nil
	dec := workflow.Decision{Branch: -1}
	var condErr error
	if hasCondition {
		dec, condErr = workflow.Decide(branches, res)
	}
	resp["condition_met"] = condErr == nil && (dec.Branch >= 0 || !hasCondition)
	if condErr != nil {
		resp["condition_error"] = "The condition could not be decided: " + condErr.Error() + "."
	} else {
		notes = append(notes, dec.Notes...)
		if dec.Branch >= 0 {
			resp["branch"] = dec.Branch
			if dec.Outcome.Value != nil {
				resp["value"] = *dec.Outcome.Value
			}
		}
	}
	messages := []WorkflowPreviewMessage{}
	seen := map[string]bool{}
	for _, s := range w.Definition.SendSteps() {
		if s.Message == nil {
			continue
		}
		var out workflow.Outcome
		prefix := ""
		if s.Case != nil {
			i := *s.Case
			// A condition below the one that held was not evaluated by Decide;
			// its own outcome fills {{value}} in its preview.
			out = dec.Outcome
			if i != dec.Branch && i < len(branches) {
				out, _ = workflow.Evaluate(branches[i].Condition, res)
			}
			if len(branches) > 1 {
				prefix = fmt.Sprintf("Condition %d: ", i+1)
			}
		} else if len(w.Definition.SendSteps()) > 1 {
			prefix = fmt.Sprintf("Message step %s: ", s.ID)
		}
		msg, renderNotes := renderWorkflowMessage(w.Name, *s.Message, res, out, w.URL, time.Now())
		for _, n := range renderNotes {
			n = prefix + n
			if !seen[n] {
				seen[n] = true
				notes = append(notes, n)
			}
		}
		for _, ch := range w.Channels {
			if ch.NodeID != s.ID {
				continue
			}
			m := WorkflowPreviewMessage{Channel: ch.Name, Kind: ch.Kind, Step: s.ID, Branch: s.Case}
			if ch.Kind == deliveryKindSlack {
				m.Body = msg.Slack
			} else {
				m.Subject, m.Body = msg.Subject, msg.Text
			}
			messages = append(messages, m)
		}
	}
	resp["messages"] = messages
	resp["notes"] = notes
	logAudit(c, "test_workflow", "workflow", w.ID, map[string]interface{}{"row_count": res.RowCount})
	c.JSON(http.StatusOK, resp)
}

// ActivateWorkflow turns a draft or paused workflow on, as the caller: they
// become its run-as user. Every check a draft passed is made again, the query
// run included: a draft stored unchecked, or whose tables changed since, is
// refused here rather than at its first unattended run.
// POST /api/v1/workflows/:id/activate
func ActivateWorkflow(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	w, err := loadWorkflow(ctx, database, ws, c.Param("id"))
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	def := w.Definition
	if err := workflow.Normalize(&def); err != nil {
		msg := err.Error()
		var inv *workflow.Invalid
		if errors.As(err, &inv) {
			msg = inv.Message
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": msg + " Edit the workflow to fix it, then turn it on."})
		return
	}
	if refusal := workflowCannotRun(w, role); refusal != "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": refusal})
		return
	}
	// A pipeline or model a step names may have been deleted, or a pipeline
	// turned to streaming, since the workflow was saved; a chart or dashboard
	// deleted or made private. The workflow runs as the person turning it on,
	// so each chart and dashboard is one they see.
	pick := func(_, field string) string {
		return "Edit the workflow to pick another " + field + ", then turn it on."
	}
	for _, s := range def.Steps {
		var refusal *PlanRefusal
		var err error
		switch {
		case s.IsProcess():
			refusal, err = validateProcessStep(ctx, database, ws, s, pick)
		case s.Type == workflow.StepChart:
			refusal, err = validateChartStep(ctx, database, ws, userID, s, pick)
		case s.Type == workflow.StepDashboard:
			refusal, err = validateDashboardStep(ctx, database, ws, userID, s, pick)
		}
		if err != nil {
			log.WithError(err).Error("[workflows] activate step check failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the workflow"})
			return
		}
		if refusal != nil {
			writeWorkflowRefusal(c, refusal)
			return
		}
	}
	if _, refusal, err := checkWorkflowForPerson(ctx, database, ws, def, role, "the workflow was not turned on"); err != nil {
		log.WithError(err).Error("[workflows] activate check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the workflow"})
		return
	} else if refusal != nil {
		writeWorkflowRefusal(c, refusal)
		return
	}
	var next interface{} = sql.NullTime{}
	if w.Definition.Trigger.Type == workflow.TriggerSchedule {
		t, err := workflow.NextRun(w.Definition.Trigger, time.Now())
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "The schedule could not be read: " + err.Error()})
			return
		}
		next = t
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("[workflows] activate begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to turn the workflow on"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE workflows SET status = 'active', paused_reason = NULL, next_run_at = $3, run_as_user_id = $4,
		       updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2
	`, w.ID, ws, next, userID)
	if err != nil {
		log.WithError(err).Error("[workflows] activate failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to turn the workflow on"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	// Its runs start from a Temporal schedule, made while the UPDATE holds the
	// row's lock (see beginScheduledWorkflowRun). No schedule, not turned on.
	if w.Definition.Trigger.Type == workflow.TriggerSchedule && !applyWorkflowSchedule(c, w.ID, w.Definition.Trigger) {
		return
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workflows] activate commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to turn the workflow on"})
		return
	}
	logAudit(c, "activate_workflow", "workflow", w.ID, map[string]interface{}{"trigger": w.Definition.Trigger.Type})
	w, err = loadWorkflow(ctx, database, ws, w.ID)
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workflow": w})
}

// workflowCannotRun says why w cannot be turned on or run by role.
func workflowCannotRun(w *Workflow, role security.WorkspaceRole) string {
	if q := w.Definition.QueryStep(); q != nil {
		if w.Connection == nil {
			return "The workflow's connection was deleted. Edit the workflow to pick another connection, or delete it."
		}
		if reason := workflowConnectionRefusal(w.Connection.ConnectorType); reason != "" {
			return reason
		}
		if reason := workflowSQLRefusal(q.SQL, role); reason != "" {
			return reason
		}
	}
	if w.Definition.Trigger.Type == workflow.TriggerAfterPipeline && w.Pipeline == nil {
		return "The pipeline this workflow runs after was deleted."
	}
	sends := w.Definition.SendSteps()
	nBranches := len(w.Definition.Branches())
	for _, s := range sends {
		has := false
		for _, ch := range w.Channels {
			has = has || ch.NodeID == s.ID
		}
		switch {
		case has:
		case s.Case != nil && nBranches > 1:
			return fmt.Sprintf("Condition %d has no delivery channel to send to. Edit the workflow to pick one.", *s.Case+1)
		case len(sends) > 1:
			return fmt.Sprintf("Message step %s has no delivery channel to send to. Edit the workflow to pick one.", s.ID)
		default:
			return "The workflow has no delivery channel to send to."
		}
	}
	return ""
}

// PauseWorkflow stops a workflow's triggers. Run now still works.
// POST /api/v1/workflows/:id/pause
func PauseWorkflow(c *gin.Context) {
	_, ws, _, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	ctx := c.Request.Context()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("[workflows] pause begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause the workflow"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	var kind string
	err = tx.QueryRowContext(ctx, `
		UPDATE workflows SET status = 'paused', paused_reason = NULL, next_run_at = NULL, updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'active'
		RETURNING trigger_kind
	`, id, ws).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only a workflow that is on can be paused."})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflows] pause failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause the workflow"})
		return
	}
	// Best effort, under the row's lock: a schedule left behind is deleted by
	// its next start (beginScheduledWorkflowRun).
	if kind == workflow.TriggerSchedule {
		dropWorkflowSchedule(ctx, id)
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workflows] pause commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause the workflow"})
		return
	}
	logAudit(c, "pause_workflow", "workflow", id, nil)
	c.JSON(http.StatusOK, gin.H{"status": "paused"})
}

// RunWorkflowNow runs a workflow once, as the caller, whatever its status.
// The run happens on Temporal; the response carries its id.
// POST /api/v1/workflows/:id/run
func RunWorkflowNow(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	w, err := loadWorkflow(ctx, database, ws, c.Param("id"))
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	if refusal := workflowCannotRun(w, role); refusal != "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": refusal})
		return
	}
	if err := failAbandonedWorkflowRuns(ctx, database, w.ID); err != nil {
		log.WithError(err).Warn("[workflows] could not fail abandoned runs")
	}
	var running bool
	if err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_runs WHERE workflow_id = $1 AND status = 'running')`,
		w.ID).Scan(&running); err == nil && running {
		c.JSON(http.StatusConflict, gin.H{"error": "This workflow is already running. Wait for that run to finish."})
		return
	}
	runID, err := startManualWorkflowRun(ctx, database, w.ID, ws, userID)
	if errors.Is(err, errWorkflowRunNotStarted) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": workflowNotStartedError + " Try again in a minute.", "run_id": runID})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflows] run start failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start the run"})
		return
	}
	logAudit(c, "run_workflow", "workflow", w.ID, map[string]interface{}{"run_id": runID})
	c.JSON(http.StatusAccepted, gin.H{"run_id": runID, "status": "running"})
}

// DeleteWorkflow deletes a workflow and its runs.
// DELETE /api/v1/workflows/:id
func DeleteWorkflow(c *gin.Context) {
	_, ws, _, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	ctx := c.Request.Context()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("[workflows] delete begin failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the workflow"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	var name, kind string
	err = tx.QueryRowContext(ctx,
		`DELETE FROM workflows WHERE id = $1 AND workspace_id = $2 RETURNING name, trigger_kind`, id, ws).Scan(&name, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflows] delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the workflow"})
		return
	}
	// Best effort: a schedule left behind is deleted by its next start.
	if kind == workflow.TriggerSchedule {
		dropWorkflowSchedule(ctx, id)
	}
	if err := tx.Commit(); err != nil {
		log.WithError(err).Error("[workflows] delete commit failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the workflow"})
		return
	}
	logAudit(c, "delete_workflow", "workflow", id, map[string]interface{}{"name": name})
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
