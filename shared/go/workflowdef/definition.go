// Package workflowdef holds the parts of a workflow that need no database: its
// definition and the checks on it, the rules that decide which step runs next,
// the condition, and the message template. The api-gateway stores and checks
// workflows with it; the temporal-adapter runs them with it, so the two can
// never disagree about what a valid workflow is or which step comes next.
//
// A workflow is a trigger and a graph of steps (definition version 3): load a
// pipeline, refresh a model, wait, run one read-only query, decide on its
// result, send a message. Each step names the steps it runs after and a rule
// over them. Versions 1 and 2 were one query, one condition list and a message
// per condition; they read as a query step, a condition step and one send step
// per condition, so stored workflows and the copies their runs keep need no
// rewrite. The person reads a condition's case as a "condition" (and each step
// as a node on the canvas). A workflow never moves data between connections,
// and never orders model after model: that is the model's own after_upstream.
package workflowdef

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron"
)

// Trigger kinds.
const (
	TriggerSchedule      = "schedule"
	TriggerAfterPipeline = "after_pipeline"
	TriggerManual        = "manual"
)

// Condition kinds.
const (
	ConditionAlways    = "always"
	ConditionRowsExist = "rows_exist"
	ConditionNoRows    = "no_rows"
	ConditionValue     = "value"
)

// Limits.
const (
	MaxNameLen      = 200
	MaxSQLLen       = 8000
	MaxSubjectLen   = 200
	MaxBodyLen      = 4000
	MaxQuietMinutes = 7 * 24 * 60
	MaxChannels     = 5
	// MaxBranches caps the branches, an otherwise included. It is a ceiling
	// against a runaway request, not a product limit: every branch reads the
	// same query's result, so one costs nothing more to run, and the screen
	// never states it. Only a request past it is told the number.
	MaxBranches = 100
)

// Trigger is when a workflow runs. Manual workflows run only from Run now.
type Trigger struct {
	Type       string `json:"type"`
	Cron       string `json:"cron,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
	PipelineID string `json:"pipeline_id,omitempty"`
}

// Query is the one read-only SQL statement a run executes.
type Query struct {
	ConnectionID string `json:"connection_id"`
	SQL          string `json:"sql"`
}

// Condition decides whether a run sends its message. A value condition
// compares one number from the first row; the others look only at whether
// rows came back. Anything smarter belongs in the SQL.
type Condition struct {
	Type      string  `json:"type"`
	Column    string  `json:"column,omitempty"`
	Op        string  `json:"op,omitempty"`
	Threshold float64 `json:"threshold"`
}

// Message is what a run sends. Subject and body are templates (render.go).
type Message struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Branch is one "if this, send that" path on the query's result: one of the
// condition step's conditions and the send step for that case. A run sends the
// first branch whose condition holds, top to bottom; an always branch, allowed
// only last, is the otherwise. Its channels are stored with the workflow, keyed
// on the send step's id (workflow_channels.node_id), not here.
type Branch struct {
	Condition Condition `json:"condition"`
	Message   Message   `json:"message"`
}

// Step types. query, condition and send are slice 1's; run_pipeline,
// refresh_model and wait are build step 5's; chart and dashboard are
// dashboards D3's.
const (
	StepQuery        = "query"
	StepCondition    = "condition"
	StepSend         = "send"
	StepRunPipeline  = "run_pipeline"
	StepRefreshModel = "refresh_model"
	StepWait         = "wait"
	StepChart        = "chart"
	StepDashboard    = "dashboard"
)

// Rules: when a step with parents runs, as in Airflow. A skipped parent is
// not succeeded for all_success and done for all_done.
const (
	RuleAllSuccess = "all_success"
	RuleAllDone    = "all_done"
	RuleAnyFailed  = "any_failed"
)

// Graph limits.
const (
	// MaxSteps is a ceiling against a runaway request: 100 conditions and
	// their sends fit under it with room for the steps around them.
	MaxSteps = 250
	// DefaultStepTimeoutMinutes is how long a run_pipeline or refresh_model
	// step waits for its run when the definition does not say (12 hours).
	DefaultStepTimeoutMinutes = 12 * 60
	MaxStepTimeoutMinutes     = 7 * 24 * 60
	// MaxRetries is how often a failed run_pipeline step starts the pipeline
	// again. Each try is a new pipeline run.
	MaxRetries     = 3
	MinWaitMinutes = 1
	MaxWaitMinutes = 24 * 60
)

// Step is one node of a workflow. Only the fields of its type are kept; the
// others are cleared by Normalize.
type Step struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// After names the steps this one runs after; a step with none is a root
	// and starts when the run starts.
	After []string `json:"after,omitempty"`
	// Rule decides, from After's outcomes, whether this step runs. "" on a
	// step with parents is all_success.
	Rule string `json:"rule,omitempty"`

	// query
	ConnectionID string `json:"connection_id,omitempty"`
	SQL          string `json:"sql,omitempty"`
	// condition: the first that holds is the case the run takes.
	Conditions []Condition `json:"conditions,omitempty"`
	// send. Case, 0-based, makes it the send for that condition (it runs only
	// when the condition step took that case); a send with no case runs
	// whenever its rule says. Required: a failed send fails the run.
	Case     *int     `json:"case,omitempty"`
	Message  *Message `json:"message,omitempty"`
	Required bool     `json:"required,omitempty"`
	// run_pipeline
	PipelineID string `json:"pipeline_id,omitempty"`
	// refresh_model
	ModelID string `json:"model_id,omitempty"`
	// run_pipeline and refresh_model: how long the step waits for its run.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
	// run_pipeline: how often a failed run is started again.
	Retries int `json:"retries,omitempty"`
	// wait
	Minutes int `json:"minutes,omitempty"`
	// chart: the saved chart it draws. Each send after it carries the chart.
	ChartID string `json:"chart_id,omitempty"`
	// dashboard: the dashboard it draws, every tile. Each send after it
	// carries the dashboard.
	DashboardID string `json:"dashboard_id,omitempty"`
}

// IsDrawing reports whether the step draws something for the sends after it:
// a chart or a dashboard.
func (s Step) IsDrawing() bool {
	return s.Type == StepChart || s.Type == StepDashboard
}

// IsProcess reports whether the step works on the data platform (a pipeline,
// a model, a timer) rather than on a query's result.
func (s Step) IsProcess() bool {
	return s.Type == StepRunPipeline || s.Type == StepRefreshModel || s.Type == StepWait
}

// Definition is a workflow's stored shape, version 3: a trigger and a graph
// of steps. Versions 1 and 2 are read as their steps (UnmarshalJSON).
type Definition struct {
	Version int     `json:"version"`
	Trigger Trigger `json:"trigger"`
	Steps   []Step  `json:"steps"`
	// QuietMinutes stops a condition's send that keeps holding from sending
	// again until this long after it last sent. 0 sends every time.
	QuietMinutes int `json:"quiet_minutes,omitempty"`
}

// FromBranches is the step graph of slice 1's shape: a query, a condition
// step with one condition per branch, and one send per branch after it. Each
// send is required, as a failed delivery failed a run before step 5.
func FromBranches(t Trigger, q Query, bs []Branch, quiet int) Definition {
	d := Definition{Version: 3, Trigger: t, QuietMinutes: quiet}
	conds := make([]Condition, len(bs))
	for i, b := range bs {
		conds[i] = b.Condition
	}
	d.Steps = append(d.Steps,
		Step{ID: "query", Type: StepQuery, ConnectionID: q.ConnectionID, SQL: q.SQL},
		Step{ID: "condition", Type: StepCondition, After: []string{"query"}, Rule: RuleAllSuccess, Conditions: conds})
	for i, b := range bs {
		d.Steps = append(d.Steps, caseSend(fmt.Sprintf("send_%d", i+1), i, b.Message))
	}
	return d
}

func caseSend(id string, i int, m Message) Step {
	return Step{ID: id, Type: StepSend, After: []string{"condition"}, Rule: RuleAllSuccess, Case: &i,
		Message: &m, Required: true}
}

// UnmarshalJSON reads every version. Version 1 (one condition and one message
// at the top level) and version 2 (a query and a list of branches) become
// FromBranches' steps, so stored workflows and the copies their runs keep
// (migration 131) need no rewrite.
func (d *Definition) UnmarshalJSON(b []byte) error {
	type plain Definition
	var raw struct {
		plain
		Query     *Query     `json:"query"`
		Branches  []Branch   `json:"branches"`
		Condition *Condition `json:"condition"`
		Message   *Message   `json:"message"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*d = Definition(raw.plain)
	if len(d.Steps) > 0 || (raw.Query == nil && raw.Branches == nil && raw.Condition == nil && raw.Message == nil) {
		return nil
	}
	bs := raw.Branches
	if len(bs) == 0 && (raw.Condition != nil || raw.Message != nil) {
		var br Branch
		if raw.Condition != nil {
			br.Condition = *raw.Condition
		}
		if raw.Message != nil {
			br.Message = *raw.Message
		}
		bs = []Branch{br}
	}
	var q Query
	if raw.Query != nil {
		q = *raw.Query
	}
	d.Version, d.Steps = 3, FromBranches(d.Trigger, q, bs, d.QuietMinutes).Steps
	return nil
}

// Step is the step with this id, nil when there is none. The pointer is into
// d.Steps.
func (d Definition) Step(id string) *Step {
	for i := range d.Steps {
		if d.Steps[i].ID == id {
			return &d.Steps[i]
		}
	}
	return nil
}

func (d Definition) firstOfType(t string) *Step {
	for i := range d.Steps {
		if d.Steps[i].Type == t {
			return &d.Steps[i]
		}
	}
	return nil
}

// Clone is a deep copy of d: editing the copy's steps leaves d's alone.
func (d Definition) Clone() Definition {
	out := d
	out.Steps = make([]Step, len(d.Steps))
	for i, s := range d.Steps {
		s.After = append([]string(nil), s.After...)
		s.Conditions = append([]Condition(nil), s.Conditions...)
		if s.Case != nil {
			c := *s.Case
			s.Case = &c
		}
		if s.Message != nil {
			m := *s.Message
			s.Message = &m
		}
		out.Steps[i] = s
	}
	return out
}

// QueryStep is the workflow's query step, nil when it has none.
func (d Definition) QueryStep() *Step { return d.firstOfType(StepQuery) }

// ConditionStep is the workflow's condition step, nil when it has none.
func (d Definition) ConditionStep() *Step { return d.firstOfType(StepCondition) }

// CaseSend is the send for condition i (0-based), nil when there is none.
func (d Definition) CaseSend(i int) *Step {
	for k := range d.Steps {
		s := &d.Steps[k]
		if s.Type == StepSend && s.Case != nil && *s.Case == i {
			return s
		}
	}
	return nil
}

// SendSteps lists the send steps in definition order.
func (d Definition) SendSteps() []*Step {
	var out []*Step
	for i := range d.Steps {
		if d.Steps[i].Type == StepSend {
			out = append(out, &d.Steps[i])
		}
	}
	return out
}

// DrawingsBefore lists the chart and dashboard steps send runs after, in its
// After order: the charts and dashboards its message carries.
func (d Definition) DrawingsBefore(send *Step) []*Step {
	var out []*Step
	for _, p := range send.After {
		if s := d.Step(p); s != nil && s.IsDrawing() {
			out = append(out, s)
		}
	}
	return out
}

// ChartSteps lists the chart steps in definition order.
func (d Definition) ChartSteps() []*Step {
	var out []*Step
	for i := range d.Steps {
		if d.Steps[i].Type == StepChart {
			out = append(out, &d.Steps[i])
		}
	}
	return out
}

// DashboardSteps lists the dashboard steps in definition order.
func (d Definition) DashboardSteps() []*Step {
	var out []*Step
	for i := range d.Steps {
		if d.Steps[i].Type == StepDashboard {
			out = append(out, &d.Steps[i])
		}
	}
	return out
}

// ProcessSteps lists the run_pipeline, refresh_model and wait steps.
func (d Definition) ProcessSteps() []*Step {
	var out []*Step
	for i := range d.Steps {
		if d.Steps[i].IsProcess() {
			out = append(out, &d.Steps[i])
		}
	}
	return out
}

// Query is the query step's connection and SQL; empty without one.
func (d Definition) Query() Query {
	if s := d.QueryStep(); s != nil {
		return Query{ConnectionID: s.ConnectionID, SQL: s.SQL}
	}
	return Query{}
}

// Branches is each condition with its case's message, top to bottom.
func (d Definition) Branches() []Branch {
	c := d.ConditionStep()
	if c == nil {
		return nil
	}
	out := make([]Branch, len(c.Conditions))
	for i, cond := range c.Conditions {
		out[i].Condition = cond
		if s := d.CaseSend(i); s != nil && s.Message != nil {
			out[i].Message = *s.Message
		}
	}
	return out
}

// IsLegacyShape reports whether d is slice 1's shape and nothing more: a
// query that starts the run, a condition after it, and one send per case
// after that. Such a run can still be served by a temporal-adapter from
// before step 5.
func (d Definition) IsLegacyShape() bool {
	q, c := d.QueryStep(), d.ConditionStep()
	if q == nil || c == nil || len(q.After) > 0 || len(c.After) != 1 || c.After[0] != q.ID {
		return false
	}
	if len(d.Steps) != 2+len(c.Conditions) {
		return false
	}
	for i := range c.Conditions {
		s := d.CaseSend(i)
		if s == nil || len(s.After) != 1 || s.After[0] != c.ID || (s.Rule != "" && s.Rule != RuleAllSuccess) {
			return false
		}
	}
	return true
}

// SetQuery sets the query step's connection and SQL, adding a root query step
// named "query" when there is none.
func (d *Definition) SetQuery(q Query) {
	if s := d.QueryStep(); s != nil {
		s.ConnectionID, s.SQL = q.ConnectionID, q.SQL
		return
	}
	d.Steps = append([]Step{{ID: "query", Type: StepQuery, ConnectionID: q.ConnectionID, SQL: q.SQL}}, d.Steps...)
}

// ReplaceBranches sets the conditions and their sends. Case i keeps the id
// (and so the channels) of the send that was case i; a new case gets the next
// free send_<k>; a case dropped drops its send. A definition with a query and
// no condition step gets one after the query.
func (d *Definition) ReplaceBranches(bs []Branch) {
	c := d.ConditionStep()
	if c == nil {
		q := d.QueryStep()
		if q == nil {
			return
		}
		d.Steps = append(d.Steps, Step{ID: d.freeID("condition"), Type: StepCondition, After: []string{q.ID},
			Rule: RuleAllSuccess})
		c = &d.Steps[len(d.Steps)-1]
	}
	condID := c.ID
	conds := make([]Condition, len(bs))
	for i, b := range bs {
		conds[i] = b.Condition
	}
	c.Conditions = conds

	ids := map[int]string{}
	kept := d.Steps[:0:0]
	for _, s := range d.Steps {
		if s.Type == StepSend && s.Case != nil {
			if *s.Case >= len(bs) {
				continue
			}
			ids[*s.Case] = s.ID
		}
		kept = append(kept, s)
	}
	d.Steps = kept
	for i, b := range bs {
		if id, ok := ids[i]; ok {
			s := d.Step(id)
			m := b.Message
			s.Message = &m
			continue
		}
		s := caseSend(d.freeID("send_"), i, b.Message)
		s.After = []string{condID}
		d.Steps = append(d.Steps, s)
	}
}

// freeID is prefix, or prefix<k> for the first k from 1 not taken (prefix
// ending in "_" always takes a number).
func (d Definition) freeID(prefix string) string {
	if !strings.HasSuffix(prefix, "_") && d.Step(prefix) == nil {
		return prefix
	}
	for k := 1; ; k++ {
		if id := prefix + strconv.Itoa(k); d.Step(id) == nil {
			return id
		}
	}
}

// Invalid is a definition rsync.ai refuses, with a sentence for the person.
type Invalid struct {
	Field   string
	Message string
	// Branch is the 1-based condition the refusal is about, 0 for none.
	Branch int
	// Step is the id of the step the refusal is about, "" for none.
	Step string
}

func (e *Invalid) Error() string { return e.Message }

func invalid(field, format string, a ...any) *Invalid {
	return &Invalid{Field: field, Message: fmt.Sprintf(format, a...)}
}

func invalidStep(step, field, format string, a ...any) *Invalid {
	inv := invalid(field, format, a...)
	inv.Step = step
	return inv
}

var opSynonyms = map[string]string{
	">": ">", "gt": ">", "greater_than": ">", "above": ">",
	">=": ">=", "gte": ">=", "at_least": ">=",
	"<": "<", "lt": "<", "less_than": "<", "below": "<",
	"<=": "<=", "lte": "<=", "at_most": "<=",
	"=": "=", "==": "=", "eq": "=", "equals": "=",
	"!=": "!=", "<>": "!=", "ne": "!=", "not_equal": "!=",
}

// cronParser is the five-field parser pipeline schedules use.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var stepIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// sendOnlyPlaceholders are the slots a send that is not a condition's case
// may use: it has no query result to fill the others from.
var sendOnlyPlaceholders = []string{"workflow_name", "run_time", "link"}

// Normalize trims every field, fills defaults and checks the definition's
// shape: the trigger, each step and the graph they make. It does not look
// anything up: the connection, the pipelines, the models and the SQL's
// statement class are checked by the caller, which has the database.
func Normalize(d *Definition) error {
	d.Version = 3
	if err := normalizeTrigger(&d.Trigger); err != nil {
		return err
	}
	if len(d.Steps) > MaxSteps {
		return invalid("steps", "A workflow has at most %d steps; this one has %d.", MaxSteps, len(d.Steps))
	}
	if err := normalizeGraph(d); err != nil {
		return err
	}

	q, c := d.QueryStep(), d.ConditionStep()
	if q != nil {
		q.ConnectionID = strings.TrimSpace(q.ConnectionID)
		q.SQL = strings.TrimSpace(q.SQL)
		if q.ConnectionID == "" {
			return invalidStep(q.ID, "connection_id", "A workflow needs the connection its query runs on.")
		}
		if q.SQL == "" {
			return invalidStep(q.ID, "sql", "A workflow needs a SQL query.")
		}
		if len(q.SQL) > MaxSQLLen {
			return invalidStep(q.ID, "sql", "The SQL is %d characters; a workflow's query may be at most %d.",
				len(q.SQL), MaxSQLLen)
		}
	}
	if c != nil {
		if len(c.Conditions) == 0 {
			return invalidStep(c.ID, "message", "A workflow needs a message to send.")
		}
		if len(c.Conditions) > MaxBranches {
			return invalidStep(c.ID, "branches", "A workflow has at most %d conditions; this one has %d.",
				MaxBranches, len(c.Conditions))
		}
		for i := range c.Conditions {
			if err := normalizeBranch(d, c, q, i); err != nil {
				return err
			}
		}
	}

	sends := 0
	for i := range d.Steps {
		s := &d.Steps[i]
		switch s.Type {
		case StepSend:
			sends++
			if s.Case == nil {
				if err := normalizeMessage(s, nil, "", sendOnlyPlaceholders); err != nil {
					return err
				}
			}
		case StepRunPipeline:
			s.PipelineID = strings.TrimSpace(s.PipelineID)
			if s.PipelineID == "" {
				return invalidStep(s.ID, "pipeline_id", "Step %s needs the pipeline it runs.", s.ID)
			}
			if d.Trigger.Type == TriggerAfterPipeline && s.PipelineID == d.Trigger.PipelineID {
				return invalidStep(s.ID, "pipeline_id", "Step %s runs the pipeline this workflow runs after, so each "+
					"run would start the next one, forever. Run another pipeline, or trigger the workflow another way.", s.ID)
			}
			if err := normalizeTimeout(s); err != nil {
				return err
			}
			if s.Retries < 0 || s.Retries > MaxRetries {
				return invalidStep(s.ID, "retries", "Step %s may retry 0 to %d times.", s.ID, MaxRetries)
			}
		case StepRefreshModel:
			s.ModelID = strings.TrimSpace(s.ModelID)
			if s.ModelID == "" {
				return invalidStep(s.ID, "model_id", "Step %s needs the model it refreshes.", s.ID)
			}
			if err := normalizeTimeout(s); err != nil {
				return err
			}
			if s.Retries != 0 {
				return invalidStep(s.ID, "retries", "Step %s refreshes a model, which is not retried: a retry would "+
					"build the model again over a run its checks blocked. Set retries to 0.", s.ID)
			}
		case StepWait:
			if s.Minutes < MinWaitMinutes || s.Minutes > MaxWaitMinutes {
				return invalidStep(s.ID, "minutes", "Step %s waits %d to %d minutes (24 hours).", s.ID,
					MinWaitMinutes, MaxWaitMinutes)
			}
		case StepChart:
			s.ChartID = strings.TrimSpace(s.ChartID)
			if s.ChartID == "" {
				return invalidStep(s.ID, "chart_id", "Step %s needs the chart it draws.", s.ID)
			}
			if err := checkDrawingSends(d, s, "chart"); err != nil {
				return err
			}
		case StepDashboard:
			s.DashboardID = strings.TrimSpace(s.DashboardID)
			if s.DashboardID == "" {
				return invalidStep(s.ID, "dashboard_id", "Step %s needs the dashboard it draws.", s.ID)
			}
			if err := checkDrawingSends(d, s, "dashboard"); err != nil {
				return err
			}
		}
	}
	if sends == 0 {
		return invalid("message", "A workflow needs a message to send.")
	}

	if d.QuietMinutes < 0 || d.QuietMinutes > MaxQuietMinutes {
		return invalid("quiet_minutes", "quiet_minutes must be between 0 and %d (a week).", MaxQuietMinutes)
	}
	return nil
}

func normalizeTrigger(t *Trigger) error {
	t.Type = strings.ToLower(strings.TrimSpace(t.Type))
	t.Cron = strings.Join(strings.Fields(t.Cron), " ")
	t.Timezone = strings.TrimSpace(t.Timezone)
	t.PipelineID = strings.TrimSpace(t.PipelineID)
	switch t.Type {
	case TriggerSchedule:
		if t.Cron == "" {
			return invalid("cron", "A scheduled workflow needs a five-field cron expression, such as 0 9 * * 1 for Mondays at 09:00.")
		}
		if _, err := cronParser.Parse(t.Cron); err != nil {
			return invalid("cron", "%q is not a five-field cron expression: %v.", t.Cron, err)
		}
		if t.Timezone == "" {
			t.Timezone = "UTC"
		}
		if _, err := loadLocation(t.Timezone); err != nil {
			return invalid("timezone", "%q is not a time zone. Use an IANA name such as Europe/Berlin.", t.Timezone)
		}
		t.PipelineID = ""
	case TriggerAfterPipeline:
		if t.PipelineID == "" {
			return invalid("pipeline_id", "A workflow that runs after a pipeline needs that pipeline's id.")
		}
		t.Cron, t.Timezone = "", ""
	case TriggerManual:
		t.Cron, t.Timezone, t.PipelineID = "", "", ""
	default:
		return invalid("trigger", "trigger %q is not one of schedule, after_pipeline or manual.", t.Type)
	}
	return nil
}

// checkDrawingSends refuses a chart or dashboard step (what names it) with no
// send after it, or with a step after it that is not a send: what it draws
// goes into the message of each send after it. A condition's sends run after
// the condition alone (checkDecisionShape), so a drawing is never in a
// condition's case.
func checkDrawingSends(d *Definition, c *Step, what string) *Invalid {
	sends := 0
	for i := range d.Steps {
		s := &d.Steps[i]
		for _, p := range s.After {
			if p != c.ID {
				continue
			}
			if s.Type != StepSend {
				return invalidStep(s.ID, "after", "Step %s runs after the %s %s, and only a send may: the %s "+
					"goes into the message of each send after it.", s.ID, what, c.ID, what)
			}
			sends++
		}
	}
	if sends == 0 {
		return invalidStep(c.ID, "after", "Step %s draws a %s for a message, so it needs a send after it.", c.ID, what)
	}
	return nil
}

func normalizeTimeout(s *Step) *Invalid {
	if s.TimeoutMinutes == 0 {
		s.TimeoutMinutes = DefaultStepTimeoutMinutes
	}
	if s.TimeoutMinutes < 1 || s.TimeoutMinutes > MaxStepTimeoutMinutes {
		return invalidStep(s.ID, "timeout_minutes", "Step %s may wait 1 to %d minutes (a week) for its run.",
			s.ID, MaxStepTimeoutMinutes)
	}
	return nil
}

// normalizeGraph checks each step's id, type, parents and rule, clears the
// fields its type does not use, and refuses a cycle and a query, condition
// and case sends that are not wired as one decision.
func normalizeGraph(d *Definition) error {
	seen := map[string]bool{}
	for i := range d.Steps {
		s := &d.Steps[i]
		s.ID = strings.TrimSpace(s.ID)
		s.Type = strings.ToLower(strings.TrimSpace(s.Type))
		if !stepIDRe.MatchString(s.ID) {
			return invalidStep(s.ID, "id", "%q is not a step id: use lower-case letters, digits and _, starting with "+
				"a letter, at most 40 characters.", s.ID)
		}
		if seen[s.ID] {
			return invalidStep(s.ID, "id", "Two steps are named %s; each step needs its own id.", s.ID)
		}
		seen[s.ID] = true
		if s.Type != StepChart {
			s.ChartID = ""
		}
		if s.Type != StepDashboard {
			s.DashboardID = ""
		}
		switch s.Type {
		case StepQuery:
			s.Conditions, s.Case, s.Message, s.Required = nil, nil, nil, false
			s.PipelineID, s.ModelID, s.TimeoutMinutes, s.Retries, s.Minutes = "", "", 0, 0, 0
		case StepCondition:
			s.ConnectionID, s.SQL, s.Case, s.Message, s.Required = "", "", nil, nil, false
			s.PipelineID, s.ModelID, s.TimeoutMinutes, s.Retries, s.Minutes = "", "", 0, 0, 0
		case StepSend:
			s.ConnectionID, s.SQL, s.Conditions = "", "", nil
			s.PipelineID, s.ModelID, s.TimeoutMinutes, s.Retries, s.Minutes = "", "", 0, 0, 0
		case StepRunPipeline:
			s.ConnectionID, s.SQL, s.Conditions, s.Case, s.Message, s.Required = "", "", nil, nil, nil, false
			s.ModelID, s.Minutes = "", 0
		case StepRefreshModel:
			s.ConnectionID, s.SQL, s.Conditions, s.Case, s.Message, s.Required = "", "", nil, nil, nil, false
			s.PipelineID, s.Minutes = "", 0
		case StepWait:
			s.ConnectionID, s.SQL, s.Conditions, s.Case, s.Message, s.Required = "", "", nil, nil, nil, false
			s.PipelineID, s.ModelID, s.TimeoutMinutes, s.Retries = "", "", 0, 0
		case StepChart, StepDashboard:
			s.ConnectionID, s.SQL, s.Conditions, s.Case, s.Message, s.Required = "", "", nil, nil, nil, false
			s.PipelineID, s.ModelID, s.TimeoutMinutes, s.Retries, s.Minutes = "", "", 0, 0, 0
		default:
			return invalidStep(s.ID, "type", "Step %s: %q is not one of query, condition, send, run_pipeline, "+
				"refresh_model, wait, chart or dashboard.", s.ID, s.Type)
		}
	}
	for i := range d.Steps {
		s := &d.Steps[i]
		parents := map[string]bool{}
		for k, p := range s.After {
			p = strings.TrimSpace(p)
			s.After[k] = p
			switch {
			case p == s.ID:
				return invalidStep(s.ID, "after", "Step %s cannot run after itself.", s.ID)
			case !seen[p]:
				return invalidStep(s.ID, "after", "Step %s runs after %q, which is not a step of this workflow.", s.ID, p)
			case parents[p]:
				return invalidStep(s.ID, "after", "Step %s names %s twice in after.", s.ID, p)
			}
			parents[p] = true
		}
		if len(s.After) == 0 {
			s.After, s.Rule = nil, ""
			continue
		}
		s.Rule = strings.ToLower(strings.TrimSpace(s.Rule))
		switch s.Rule {
		case "":
			s.Rule = RuleAllSuccess
		case RuleAllSuccess, RuleAllDone, RuleAnyFailed:
		default:
			return invalidStep(s.ID, "rule", "Step %s: rule %q is not one of all_success, all_done or any_failed.",
				s.ID, s.Rule)
		}
	}
	if cyc := cycle(d.Steps); len(cyc) > 0 {
		return invalidStep(cyc[0], "after", "The steps %s run after each other in a circle, so none of them could "+
			"start.", strings.Join(cyc, ", "))
	}
	return checkDecisionShape(d)
}

// cycle lists the steps on or behind a cycle, in definition order; empty
// when the steps make none (Kahn's algorithm).
func cycle(steps []Step) []string {
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, s := range steps {
		indeg[s.ID] += 0
		for _, p := range s.After {
			indeg[s.ID]++
			children[p] = append(children[p], s.ID)
		}
	}
	var queue []string
	for _, s := range steps {
		if indeg[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}
	done := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		done++
		for _, c := range children[id] {
			if indeg[c]--; indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if done == len(steps) {
		return nil
	}
	var left []string
	for _, s := range steps {
		if indeg[s.ID] > 0 {
			left = append(left, s.ID)
		}
	}
	return left
}

// checkDecisionShape refuses a query, condition and case sends that do not
// make one decision: a query with a condition after it and nothing else, a
// condition after only that query, and after the condition only its cases'
// sends, one per case.
func checkDecisionShape(d *Definition) error {
	var q, c *Step
	for i := range d.Steps {
		s := &d.Steps[i]
		switch s.Type {
		case StepQuery:
			if q != nil {
				return invalidStep(s.ID, "steps", "A workflow has at most one query step.")
			}
			q = s
		case StepCondition:
			if c != nil {
				return invalidStep(s.ID, "steps", "A workflow has at most one condition step.")
			}
			c = s
		}
	}
	switch {
	case q == nil && c == nil:
		for _, s := range d.Steps {
			if s.Type == StepSend && s.Case != nil {
				return invalidStep(s.ID, "case", "Step %s sends for a condition, but the workflow has no condition step.", s.ID)
			}
		}
		return nil
	case c == nil:
		return invalidStep(q.ID, "steps", "Step %s queries, but no condition step decides on its result: add one "+
			"after it, with the sends it chooses between.", q.ID)
	case q == nil:
		return invalidStep(c.ID, "steps", "Step %s decides on a query's result, but the workflow has no query step.", c.ID)
	}
	if len(c.After) != 1 || c.After[0] != q.ID || c.Rule != RuleAllSuccess {
		return invalidStep(c.ID, "after", "Step %s must run after the query step %s alone, with rule all_success.",
			c.ID, q.ID)
	}
	cases := map[int]string{}
	for i := range d.Steps {
		s := &d.Steps[i]
		afterQ, afterC := false, false
		for _, p := range s.After {
			afterQ = afterQ || p == q.ID
			afterC = afterC || p == c.ID
		}
		if afterQ && s.ID != c.ID {
			return invalidStep(s.ID, "after", "Step %s runs after the query %s; only the condition step may: it is "+
				"what decides on the query's result.", s.ID, q.ID)
		}
		isCase := s.Type == StepSend && s.Case != nil
		if afterC && !isCase {
			return invalidStep(s.ID, "after", "Step %s runs after the condition %s but is not one of its sends. "+
				"Run it after a send instead.", s.ID, c.ID)
		}
		if !isCase {
			continue
		}
		k := *s.Case
		if len(s.After) != 1 || !afterC || s.Rule != RuleAllSuccess {
			return invalidStep(s.ID, "after", "Step %s sends for a condition, so it runs after the condition step %s "+
				"alone, with rule all_success.", s.ID, c.ID)
		}
		if k < 0 || k >= len(c.Conditions) {
			return invalidStep(s.ID, "case", "Step %s sends for condition %d, but %s has %d.", s.ID, k+1, c.ID,
				len(c.Conditions))
		}
		if other, dup := cases[k]; dup {
			return invalidStep(s.ID, "case", "Steps %s and %s both send for condition %d; each condition has one send.",
				other, s.ID, k+1)
		}
		cases[k] = s.ID
	}
	for k := range c.Conditions {
		if _, ok := cases[k]; !ok {
			inv := invalidStep(c.ID, "message", "A workflow needs a message to send.")
			if len(c.Conditions) > 1 {
				inv.Message = fmt.Sprintf("Condition %d: %s", k+1, inv.Message)
			}
			inv.Branch = k + 1
			return inv
		}
	}
	return nil
}

// normalizeBranch checks condition i and its send's message, with the slots
// that message may use. A refusal names the condition when there is more than
// one.
func normalizeBranch(d *Definition, cs, q *Step, i int) *Invalid {
	n := len(cs.Conditions)
	fail := func(inv *Invalid) *Invalid {
		inv.Branch = i + 1
		if inv.Step == "" {
			inv.Step = cs.ID
		}
		if n > 1 {
			inv.Message = fmt.Sprintf("Condition %d: %s", i+1, inv.Message)
		}
		return inv
	}

	c := &cs.Conditions[i]
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.Column = strings.TrimSpace(c.Column)
	if c.Type == "" {
		c.Type = ConditionAlways
	}
	switch c.Type {
	case ConditionAlways, ConditionRowsExist, ConditionNoRows:
		c.Column, c.Op, c.Threshold = "", "", 0
	case ConditionValue:
		op, ok := opSynonyms[strings.ToLower(strings.TrimSpace(c.Op))]
		if !ok {
			return fail(invalid("condition_op", "condition_op %q is not one of >, >=, <, <=, = or !=.", c.Op))
		}
		c.Op = op
	default:
		return fail(invalid("condition", "condition %q is not one of always, rows_exist, no_rows or value.", c.Type))
	}
	if c.Type == ConditionAlways && i < n-1 {
		return fail(invalid("condition", "Only the last condition may be always: it is the otherwise, sent when "+
			"no condition above it is true, so a condition below it would never be checked. Give this one a test."))
	}
	send := d.CaseSend(i)
	if send == nil {
		return fail(invalid("message", "A workflow needs a message to send."))
	}
	sqlText := ""
	if q != nil {
		sqlText = q.SQL
	}
	if inv := normalizeMessage(send, c, sqlText, nil); inv != nil {
		return fail(inv)
	}
	return nil
}

// normalizeMessage checks a send's message. cond is its case's condition,
// nil for a send that is no case; allowed, when set, lists the only slots it
// may use.
func normalizeMessage(s *Step, cond *Condition, sqlText string, allowed []string) *Invalid {
	if s.Message == nil {
		s.Message = &Message{}
	}
	m := s.Message
	m.Subject = strings.TrimSpace(m.Subject)
	m.Body = strings.TrimSpace(m.Body)
	if m.Body == "" {
		return invalidStep(s.ID, "message", "A workflow needs a message to send.")
	}
	if n := len([]rune(m.Subject)); n > MaxSubjectLen {
		return invalidStep(s.ID, "subject", "The subject is %d characters; it may be at most %d.", n, MaxSubjectLen)
	}
	if n := len([]rune(m.Body)); n > MaxBodyLen {
		return invalidStep(s.ID, "message", "The message is %d characters; it may be at most %d.", n, MaxBodyLen)
	}
	for _, field := range []struct{ name, text string }{{"subject", m.Subject}, {"message", m.Body}} {
		if bad := badPlaceholders(field.text); len(bad) > 0 {
			return invalidStep(s.ID, field.name, "%s are not placeholders rsync.ai can fill. Use {{name}} with a "+
				"column name or one of %s.", strings.Join(bad, ", "), strings.Join(fixedPlaceholders, ", "))
		}
		if allowed != nil {
			if extra := slotsOutside(field.text, allowed); len(extra) > 0 {
				return invalidStep(s.ID, field.name, "Step %s does not send a query's result, so %s cannot be filled: "+
					"it may use only %s.", s.ID, strings.Join(extra, ", "), braced(allowed))
			}
			continue
		}
		if err := slotsMisstateTheData(field.name, field.text, *cond, sqlText); err != nil {
			err.Step = s.ID
			return err
		}
	}
	return nil
}

// slotsOutside lists each {{name}} in text not in allowed, once each.
func slotsOutside(text string, allowed []string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if !ok[m[1]] && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, "{{"+m[1]+"}}")
		}
	}
	return out
}

func braced(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{{" + n + "}}"
	}
	return strings.Join(out, ", ")
}

// limitedRe matches a query whose own result is cut to a fixed number of rows:
// a LIMIT or FETCH FIRST at its end, or a leading SELECT TOP. A LIMIT inside a
// subquery is followed by its closing parenthesis, so it does not match.
var limitedRe = regexp.MustCompile(`(?is)(\blimit\s+\d+(\s+offset\s+\d+)?|\bfetch\s+(first|next)\s+\d+\s+rows?\s+only)\s*;?\s*$|^\s*select\s+(distinct\s+)?top\s*\(?\s*\d+`)

// slotsMisstateTheData refuses the two slots that describe the query's result,
// not the data, where a draft uses them as a number from the data. Drafts on
// prod wrote "Total users: {{row_count}}" under a one-row COUNT (it said 1)
// and "the total is {{row_count}}" under LIMIT 10 (it said 10). The fix is
// always the same: name the number in the SQL and write its column.
func slotsMisstateTheData(field, text string, c Condition, sqlText string) *Invalid {
	used := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		used[m[1]] = true
	}
	if used["value"] && c.Type != ConditionValue {
		return invalid(field, "{{value}} is only the number a value condition compares. To show a number the "+
			"query computes, name it in the SQL (for example COUNT(*) AS total_users) and write {{total_users}}.")
	}
	if used["row_count"] && limitedRe.MatchString(sqlText) {
		return invalid(field, "{{row_count}} counts the rows this query returns, so with a LIMIT it is at most the "+
			"limit, never a total in the data. For a total next to a list, add COUNT(*) OVER () AS total to the "+
			"SQL and write {{total}}.")
	}
	return nil
}

// ParseThreshold reads a threshold an agent sent as text ("50", "0.05", "1,000").
func ParseThreshold(s string) (float64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	s = strings.TrimSuffix(s, "%")
	if s == "" {
		return 0, fmt.Errorf("no threshold given")
	}
	return strconv.ParseFloat(s, 64)
}

// NextRun is the first time after now a scheduled trigger fires.
func NextRun(t Trigger, now time.Time) (time.Time, error) {
	if t.Type != TriggerSchedule {
		return time.Time{}, fmt.Errorf("trigger %q has no schedule", t.Type)
	}
	sched, err := cronParser.Parse(t.Cron)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := loadLocation(t.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(now.In(loc)).UTC(), nil
}

// DescribeTrigger renders a trigger for the person: "every Monday at 09:00 (UTC)".
func DescribeTrigger(t Trigger, pipelineName string) string {
	switch t.Type {
	case TriggerSchedule:
		tz := t.Timezone
		if tz == "" {
			tz = "UTC"
		}
		return describeCron(t.Cron) + " (" + tz + ")"
	case TriggerAfterPipeline:
		if pipelineName == "" {
			pipelineName = "a pipeline that was deleted"
		}
		return fmt.Sprintf("after %s finishes", pipelineName)
	default:
		return "only when someone clicks Run now"
	}
}

// DescribeBranch renders branch i of n for the person: its condition, or
// "otherwise send" for the last always branch of several.
func DescribeBranch(b Branch, i, n int) string {
	if n > 1 && i == n-1 && (b.Condition.Type == ConditionAlways || b.Condition.Type == "") {
		return "otherwise send"
	}
	return DescribeCondition(b.Condition)
}

// DescribeBranches renders every branch as one sentence: "send when failed is
// above 100; else send when failed is above 10; otherwise send".
func DescribeBranches(bs []Branch) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = DescribeBranch(b, i, len(bs))
		if i > 0 && !strings.HasPrefix(parts[i], "otherwise") {
			parts[i] = "else " + parts[i]
		}
	}
	return strings.Join(parts, "; ")
}

// opWords spells a value condition's operator out for the person.
var opWords = map[string]string{
	">": "is above", ">=": "is at least", "<": "is below", "<=": "is at most", "=": "is", "!=": "is not",
}

// DescribeCondition renders a condition for the person: "send when count is
// above 5,000". The editor's describeCondition (useWorkflowEdit.ts) writes the
// same words; a test on each side pins them.
func DescribeCondition(c Condition) string {
	switch c.Type {
	case ConditionRowsExist:
		return "send when the query returns rows"
	case ConditionNoRows:
		return "send when the query returns no rows"
	case ConditionValue:
		col := c.Column
		if col == "" {
			col = "the first column"
		}
		op, ok := opWords[c.Op]
		if !ok {
			op = c.Op
		}
		return fmt.Sprintf("send when %s %s %s", col, op, groupThousands(strconv.FormatFloat(c.Threshold, 'f', -1, 64)))
	default:
		return "always send"
	}
}

// groupThousands puts a comma between each three digits of a number's whole
// part: "-1234567.5" is "-1,234,567.5".
func groupThousands(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, dot := strings.Cut(s, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if dot {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}

func loadLocation(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "UTC") {
		return time.UTC, nil
	}
	if name == "Local" {
		return nil, fmt.Errorf("Local is not a time zone")
	}
	return time.LoadLocation(name)
}

var weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// everyHour says an hourly schedule as the editor's cron note does
// (frontend cronSentence.ts describeTimes): ":00" read as a clock time no one
// reads, and the note beside the cron said "on the hour".
func everyHour(m int) string {
	switch m {
	case 0:
		return "every hour on the hour"
	case 1:
		return "every hour at 1 minute past"
	default:
		return fmt.Sprintf("every hour at %d minutes past", m)
	}
}

// everyNHours says "*/n" hours at minute m as cronSentence.ts does: two times
// are listed, a step that divides the day runs on from its first time, and any
// other step names its first and last time.
func everyNHours(n, m int) string {
	if n == 1 {
		return everyHour(m)
	}
	last := (23 / n) * n
	if last/n+1 <= 2 {
		return fmt.Sprintf("every day at 00:%02d and %02d:%02d", m, last, m)
	}
	if 24%n == 0 {
		return fmt.Sprintf("every %d hours from 00:%02d", n, m)
	}
	return fmt.Sprintf("every %d hours from 00:%02d to %02d:%02d", n, m, last, m)
}

// describeCron says the common five-field shapes in words and falls back to the
// expression itself, never guessing at a shape it does not know.
func describeCron(expr string) string {
	f := strings.Fields(expr)
	raw := "on cron " + expr
	if len(f) != 5 {
		return raw
	}
	min, hour, dom, mon, dow := f[0], f[1], f[2], f[3], f[4]
	num := func(s string, max int) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 0 && n <= max
	}
	m, minOK := num(min, 59)
	h, hourOK := num(hour, 23)
	at := fmt.Sprintf("%02d:%02d", h, m)
	if mon != "*" {
		return raw
	}
	switch {
	case strings.HasPrefix(min, "*/") && hour == "*" && dom == "*" && dow == "*":
		if n, ok := num(min[2:], 59); ok && n > 0 {
			return fmt.Sprintf("every %d minutes", n)
		}
	case minOK && hour == "*" && dom == "*" && dow == "*":
		return everyHour(m)
	case minOK && strings.HasPrefix(hour, "*/") && dom == "*" && dow == "*":
		if n, ok := num(hour[2:], 23); ok && n > 0 {
			return everyNHours(n, m)
		}
	case minOK && hourOK && dom == "*" && dow == "*":
		return "every day at " + at
	case minOK && hourOK && dom == "*" && dow == "1-5":
		return "every weekday at " + at
	case minOK && hourOK && dom == "*":
		if d, ok := num(dow, 7); ok {
			return fmt.Sprintf("every %s at %s", weekdays[d%7], at)
		}
	case minOK && hourOK && dow == "*":
		if d, ok := num(dom, 31); ok && d > 0 {
			return fmt.Sprintf("on day %d of every month at %s", d, at)
		}
	}
	return raw
}
