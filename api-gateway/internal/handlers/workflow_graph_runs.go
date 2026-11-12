package handlers

// A workflow run's graph (docs/internal/workflows-design.md §§ 3.1, 4 and 5, build step
// 5). temporal-adapter's runWorkflowGraph asks /graph for the run's steps, decides each
// step's turn itself (workflowdef.Gate), and drives each step through
// /steps/:step/:action, then records the run with /complete. Every step's outcome is a
// workflow_step_runs row (migration 140), one per attempt.
//
// Only the query's result moves between steps: it waits in Redis, encrypted, for the
// condition and the sends to read, and never reaches Postgres or Temporal's history.
// A run_pipeline step starts the pipeline's own run and waits on its execution row; a
// refresh_model step asks the model's refresh workflow for a run (the adapter sends the
// signal) and waits on the model's run row. A person's Stop is recorded on the run and
// honoured at the next step call, whatever the adapter heard.
//
// Each endpoint is safe to retry: a step's row says what it already did, a pipeline run
// has an id derived from (run, step, attempt), and a send claims its ledger row.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/security"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	// workflowRunStopSignal is the Stop button's signal to WorkflowRunWorkflow.
	workflowRunStopSignal = "workflow_run_stop"
	// workflowStepAliveMinutes is how far each step call moves the run's
	// alive_until ahead: past the adapter's longest gap between two calls.
	workflowStepAliveMinutes = 15

	workflowStepEndedError    = "The run ended before this step finished."
	workflowStepStoppedNote   = "Someone stopped the run."
	workflowResultGone        = "The query's result was no longer held (rsync.ai keeps it only while the run lasts)."
	workflowStepPipelineGone  = "The pipeline was deleted."
	workflowStepModelGone     = "The model was deleted."
	workflowStepPipelineBusy  = "Waiting for the pipeline's current run to finish."
	workflowStepModelLeftNote = "The model run in flight was left to finish."
)

// Seams for tests: starting and stopping a step's pipeline run, the checks a
// pipeline run passes before it starts, Stop's signal, and where the query's
// result waits.
var (
	startWorkflowStepPipeline   = startPipelineRun
	stopWorkflowStepPipelineRun = stopWorkflowStepPipeline
	workflowStepPipelineGate    = workflowPipelineGate
	signalWorkflowRunStop       = temporalSignalWorkflowRunStop
)

var workflowResults workflowResultStore = redisWorkflowResults{}

// ---------------------------------------------------------------------------
// The query's result, between steps
// ---------------------------------------------------------------------------

// workflowResultStore holds a query step's result for the steps after it.
type workflowResultStore interface {
	Put(ctx context.Context, key string, r workflow.Result, ttl time.Duration) error
	Get(ctx context.Context, key string) (workflow.Result, bool, error)
	Delete(ctx context.Context, key string) error
}

func workflowResultKey(runID, stepID string) string { return runID + "/" + stepID }

// workflowStepMessageKey is where a send step's message waits for its sends.
func workflowStepMessageKey(runID, stepID string) string { return runID + "/" + stepID }

// storedResult is workflow.Result as Redis holds it.
type storedResult struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"row_count"`
	Truncated bool             `json:"truncated"`
}

// storableCell is a cell JSON keeps as the condition and the message read it:
// a time as a message shows it, bytes as text, and a float JSON cannot hold
// (NaN, ±Inf) as its text, which no condition reads as a number.
func storableCell(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int, int32, int64, uint64, json.Number:
		return t
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return strconv.FormatFloat(t, 'f', -1, 64)
		}
		return t
	case float32:
		if math.IsNaN(float64(t)) || math.IsInf(float64(t), 0) {
			return strconv.FormatFloat(float64(t), 'f', -1, 32)
		}
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	if _, err := json.Marshal(v); err == nil {
		return v
	}
	return fmt.Sprint(v)
}

func encodeWorkflowResult(r workflow.Result) ([]byte, error) {
	s := storedResult{Columns: r.Columns, RowCount: r.RowCount, Truncated: r.Truncated,
		Rows: make([]map[string]any, 0, len(r.Rows))}
	for _, row := range r.Rows {
		m := make(map[string]any, len(row))
		for k, v := range row {
			m[k] = storableCell(v)
		}
		s.Rows = append(s.Rows, m)
	}
	return json.Marshal(s)
}

// decodeWorkflowResult reads numbers as json.Number, so a BIGINT keeps every
// digit and a condition reads it as a number (workflowdef.asNumber).
func decodeWorkflowResult(raw []byte) (workflow.Result, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var s storedResult
	if err := dec.Decode(&s); err != nil {
		return workflow.Result{}, err
	}
	return workflow.Result{Columns: s.Columns, Rows: s.Rows, RowCount: s.RowCount, Truncated: s.Truncated}, nil
}

// redisWorkflowResults keeps each result encrypted, under run/step.
type redisWorkflowResults struct{}

func workflowResultRedisKey(key string) string { return "workflow-run-result:" + key }

func (redisWorkflowResults) client() (*redis.Client, error) {
	if schemaCache == nil {
		return nil, errors.New("Redis is not available")
	}
	return schemaCache.GetClient(), nil
}

func (s redisWorkflowResults) Put(ctx context.Context, key string, r workflow.Result, ttl time.Duration) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	raw, err := encodeWorkflowResult(r)
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(raw)
	if err != nil {
		return err
	}
	return rc.Set(ctx, workflowResultRedisKey(key), enc, ttl).Err()
}

func (s redisWorkflowResults) Get(ctx context.Context, key string) (workflow.Result, bool, error) {
	rc, err := s.client()
	if err != nil {
		return workflow.Result{}, false, err
	}
	enc, err := rc.Get(ctx, workflowResultRedisKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return workflow.Result{}, false, nil
	}
	if err != nil {
		return workflow.Result{}, false, err
	}
	raw, err := crypto.Decrypt(enc)
	if err != nil {
		return workflow.Result{}, false, err
	}
	r, err := decodeWorkflowResult(raw)
	if err != nil {
		return workflow.Result{}, false, err
	}
	return r, true, nil
}

func (s redisWorkflowResults) Delete(ctx context.Context, key string) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	return rc.Del(ctx, workflowResultRedisKey(key)).Err()
}

// workflowStepTimeout is a process step's timeout in minutes.
func workflowStepTimeout(s workflow.Step) int {
	if s.TimeoutMinutes > 0 {
		return s.TimeoutMinutes
	}
	return workflow.DefaultStepTimeoutMinutes
}

// workflowResultTTL is the longest a run of d can last, and an hour more: the
// result must outlast every step between the query and its last reader. The
// run deletes it when it ends.
func workflowResultTTL(d workflow.Definition) time.Duration {
	mins := 60
	for _, s := range d.ProcessSteps() {
		if s.Type == workflow.StepWait {
			mins += s.Minutes
			continue
		}
		mins += workflowStepTimeout(*s) * (1 + s.Retries)
	}
	return time.Duration(mins) * time.Minute
}

// ---------------------------------------------------------------------------
// /graph
// ---------------------------------------------------------------------------

// graphStep is a step as the adapter runs it: its place in the graph and its
// limits, never its SQL or message.
type graphStep struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	After          []string `json:"after,omitempty"`
	Rule           string   `json:"rule,omitempty"`
	Case           *int     `json:"case,omitempty"`
	Required       bool     `json:"required,omitempty"`
	TimeoutMinutes int      `json:"timeout_minutes,omitempty"`
	Retries        int      `json:"retries,omitempty"`
	Minutes        int      `json:"minutes,omitempty"`
}

func graphSteps(d workflow.Definition) []graphStep {
	out := make([]graphStep, 0, len(d.Steps))
	for _, s := range d.Steps {
		g := graphStep{ID: s.ID, Type: s.Type, After: s.After, Rule: s.Rule, Case: s.Case, Required: s.Required}
		switch s.Type {
		case workflow.StepRunPipeline:
			g.TimeoutMinutes, g.Retries = workflowStepTimeout(s), s.Retries
		case workflow.StepRefreshModel:
			g.TimeoutMinutes = workflowStepTimeout(s)
		case workflow.StepWait:
			g.Minutes = s.Minutes
		}
		out = append(out, g)
	}
	return out
}

// GraphWorkflowRunInternal gives the adapter a run's steps.
// POST /api/v1/internal/workflow-runs/:id/graph
//
// action "run" carries the steps; "none" means the run has ended (or was
// deleted) and there is nothing to run.
func GraphWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"action": "none"})
		return
	}
	if err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] graph: could not load the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load the run"})
		return
	}
	if run.RunStatus != "running" {
		c.JSON(http.StatusOK, gin.H{"action": "none"})
		return
	}
	if err := bumpWorkflowRunAlive(ctx, database, runID, workflowStepAliveMinutes); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] graph: could not keep the run alive")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record the run"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"action": "run", "steps": graphSteps(run.Definition)})
}

// bumpWorkflowRunAlive moves the run's alive_until at least minutes ahead.
func bumpWorkflowRunAlive(ctx context.Context, database *sql.DB, runID string, minutes int) error {
	_, err := database.ExecContext(ctx, `
		UPDATE workflow_runs
		SET alive_until = GREATEST(COALESCE(alive_until, NOW()), NOW() + make_interval(mins => $2))
		WHERE id = $1 AND status = 'running'
	`, runID, minutes)
	return err
}

// ---------------------------------------------------------------------------
// /steps/:step/:action
// ---------------------------------------------------------------------------

type graphStepRequest struct {
	Attempt   int    `json:"attempt"`
	ChannelID string `json:"channel_id"`
	// Reason is a stop's: "stop" (someone stopped the run) or "timeout".
	Reason string `json:"reason"`
	// Status and Note are a record's.
	Status string `json:"status"`
	Note   string `json:"note"`
	// Resignal is a refresh_model request after the model said it was busy;
	// SkippedRunID is that busy answer's run row.
	Resignal     bool   `json:"resignal"`
	SkippedRunID string `json:"skipped_run_id"`
}

// modelRefreshSignal is what the adapter signals the model's refresh workflow
// with (SignalModelRefreshActivity).
type modelRefreshSignal struct {
	SavedQueryID string `json:"saved_query_id"`
	UpstreamKind string `json:"upstream_kind"`
	UpstreamID   string `json:"upstream_id"`
	ExecutionID  string `json:"execution_id"`
}

// graphStepAnswer is a step call's answer. Status is the step's (a send's own
// status for action send). Halt: the run ended, run nothing more. Again: the
// step waits; poll again. Refused: the step failed for a reason no retry
// changes. Stop: someone stopped the run.
type graphStepAnswer struct {
	Status       string              `json:"status"`
	Case         *int                `json:"case,omitempty"`
	Halt         bool                `json:"halt,omitempty"`
	Channels     []string            `json:"channels,omitempty"`
	Again        bool                `json:"again,omitempty"`
	Refused      bool                `json:"refused,omitempty"`
	Resignal     bool                `json:"resignal,omitempty"`
	SkippedRunID string              `json:"skipped_run_id,omitempty"`
	ExecutionID  string              `json:"execution_id,omitempty"`
	Stop         bool                `json:"stop,omitempty"`
	Signal       *modelRefreshSignal `json:"signal,omitempty"`
}

// stepRow is a step's workflow_step_runs row, one attempt.
type stepRow struct {
	status      string
	executionID string
	modelRunID  string
	errText     string
	note        string
	rowCount    sql.NullInt64
	caseIndex   sql.NullInt64
	requestedAt sql.NullTime
}

func stepFinal(status string) bool {
	switch status {
	case workflow.StatusSucceeded, workflow.StatusWarned, workflow.StatusFailed, workflow.StatusSkipped,
		workflow.StatusStopped:
		return true
	}
	return false
}

func (r stepRow) answer() graphStepAnswer {
	a := graphStepAnswer{Status: r.status, ExecutionID: r.executionID}
	if r.caseIndex.Valid {
		i := int(r.caseIndex.Int64)
		a.Case = &i
	}
	return a
}

// graphStepActions is which actions each step type takes; every type also
// takes stop and record.
var graphStepActions = map[string][]string{
	workflow.StepQuery:        {"run"},
	workflow.StepCondition:    {"run"},
	workflow.StepChart:        {"run"},
	workflow.StepDashboard:    {"run"},
	workflow.StepSend:         {"plan", "send", "finish"},
	workflow.StepRunPipeline:  {"start", "poll"},
	workflow.StepRefreshModel: {"request", "poll"},
	workflow.StepWait:         {},
}

func graphStepTakes(stepType, action string) bool {
	if action == "stop" || action == "record" {
		return true
	}
	for _, a := range graphStepActions[stepType] {
		if a == action {
			return true
		}
	}
	return false
}

// graphStepCall is one call's context.
type graphStepCall struct {
	c        *gin.Context
	ctx      context.Context
	db       *sql.DB
	run      *workflowRunRow
	step     workflow.Step
	attempt  int
	req      graphStepRequest
	row      stepRow
	unavail  func(err error, what string)
	answered bool
}

func (s *graphStepCall) reply(a graphStepAnswer) {
	s.answered = true
	s.c.JSON(http.StatusOK, a)
}

func (s *graphStepCall) fields() log.Fields {
	return log.Fields{"run_id": s.run.RunID, "step_id": s.step.ID, "attempt": s.attempt}
}

// StepWorkflowRunInternal runs one action of one step of a run.
// POST /api/v1/internal/workflow-runs/:id/steps/:step/:action
//
// 400 is a call no retry fixes; 5xx is retried by the adapter.
func StepWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	var req graphStepRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	s := &graphStepCall{c: c, ctx: ctx, db: database, req: req}
	s.unavail = func(err error, what string) {
		log.WithError(err).WithField("run_id", runID).Warn("[workflows] step: " + what)
		s.answered = true
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": what})
	}

	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, graphStepAnswer{Halt: true})
		return
	}
	if err != nil {
		s.unavail(err, "could not load the run")
		return
	}
	if run.RunStatus != "running" {
		c.JSON(http.StatusOK, graphStepAnswer{Status: run.RunStatus, Halt: true})
		return
	}
	s.run = run
	step := run.Definition.Step(c.Param("step"))
	if step == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the run has no such step"})
		return
	}
	s.step = *step
	action := c.Param("action")
	if !graphStepTakes(step.Type, action) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("a %s step does not take %q", step.Type, action)})
		return
	}
	s.attempt = req.Attempt
	if s.attempt == 0 {
		s.attempt = 1
	}
	maxAttempt := 1
	if step.Type == workflow.StepRunPipeline {
		maxAttempt = 1 + step.Retries
	}
	if s.attempt < 1 || s.attempt > maxAttempt {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("attempt must be 1 to %d", maxAttempt)})
		return
	}
	if action == "send" {
		if _, err := uuid.Parse(req.ChannelID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel_id"})
			return
		}
	}
	if action == "record" {
		if msg := s.recordRefusal(); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
	}
	if action == "stop" && req.Reason != "stop" && req.Reason != "timeout" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason must be stop or timeout"})
		return
	}

	alive := workflowStepAliveMinutes
	if step.Type == workflow.StepWait {
		alive += step.Minutes
	}
	if err := bumpWorkflowRunAlive(ctx, database, runID, alive); err != nil {
		s.unavail(err, "could not keep the run alive")
		return
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO workflow_step_runs (run_id, step_id, attempt, workspace_id, step_type)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING
	`, runID, step.ID, s.attempt, run.WorkspaceID, step.Type); err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	if !s.loadRow() {
		return
	}
	if run.StopRequested && action != "stop" {
		s.stop("stop")
		if !s.answered {
			s.reply(graphStepAnswer{Status: workflow.StatusStopped, Stop: true})
		}
		return
	}
	if stepFinal(s.row.status) && action != "send" {
		s.reply(s.row.answer())
		return
	}

	switch action {
	case "stop":
		s.stop(req.Reason)
		if !s.answered && s.loadRow() {
			a := s.row.answer()
			a.Stop = req.Reason == "stop"
			s.reply(a)
		}
	case "record":
		s.record()
	case "run":
		switch step.Type {
		case workflow.StepQuery:
			s.runQuery()
		case workflow.StepChart:
			s.runChart()
		case workflow.StepDashboard:
			s.runDashboard()
		default:
			s.runCondition()
		}
	case "plan":
		s.planSends()
	case "send":
		sendWorkflowChannel(c, database, runID, step.ID, req.ChannelID, workflowStepMessageKey(runID, step.ID))
		s.answered = true
	case "finish":
		s.finishSends()
	case "start":
		s.startPipeline()
	case "poll":
		if step.Type == workflow.StepRunPipeline {
			s.pollPipeline()
		} else {
			s.pollModel()
		}
	case "request":
		s.requestModel()
	}
	if !s.answered {
		log.WithFields(s.fields()).WithField("action", action).Error("[workflows] step: no answer")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "the step gave no answer"})
	}
}

func (s *graphStepCall) loadRow() bool {
	var r stepRow
	var execID, modelRunID, errText, note sql.NullString
	err := s.db.QueryRowContext(s.ctx, `
		SELECT status, execution_id, saved_query_run_id::text, error, note, row_count, case_index, requested_at
		FROM workflow_step_runs WHERE run_id = $1 AND step_id = $2 AND attempt = $3
	`, s.run.RunID, s.step.ID, s.attempt).Scan(&r.status, &execID, &modelRunID, &errText, &note, &r.rowCount,
		&r.caseIndex, &r.requestedAt)
	if err != nil {
		s.unavail(err, "could not read the step")
		return false
	}
	r.executionID, r.modelRunID, r.errText, r.note = execID.String, modelRunID.String, errText.String, note.String
	s.row = r
	return true
}

// settle records the step's outcome while it has none. It reports false
// when it could not write (the call has answered 503).
func (s *graphStepCall) settle(status, errText, note string) bool {
	var e, n interface{} = sql.NullString{}, sql.NullString{}
	if errText != "" {
		e = workflow.CapRunes(errText, workflowErrorMax)
	}
	if note != "" {
		n = workflow.CapRunes(note, workflowErrorMax)
	}
	finished := "NULL"
	if stepFinal(status) {
		finished = "NOW()"
	}
	if _, err := s.db.ExecContext(context.WithoutCancel(s.ctx), `
		UPDATE workflow_step_runs SET status = $4, error = COALESCE($5, error), note = COALESCE($6, note),
		       finished_at = `+finished+`
		WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND status IN ('running', 'waiting')
	`, s.run.RunID, s.step.ID, s.attempt, status, e, n); err != nil {
		s.unavail(err, "could not record the step")
		return false
	}
	return true
}

// finish records a final outcome and answers with the step's row as it now
// stands (another attempt may have recorded first).
func (s *graphStepCall) finish(status, errText, note string, a graphStepAnswer) {
	if !s.settle(status, errText, note) || !s.loadRow() {
		return
	}
	out := s.row.answer()
	out.Refused, out.Channels = a.Refused, a.Channels
	s.reply(out)
}

// refuse fails the step for a reason no retry changes, and pauses the
// workflow when the reason will hold next time too.
func (s *graphStepCall) refuse(msg string, pause bool) {
	if pause {
		pauseWorkflowForRun(s.ctx, s.db, s.run, msg)
	}
	s.finish(workflow.StatusFailed, msg, "", graphStepAnswer{Refused: true})
}

// actorRefused checks the run's actor before the step acts as them. true:
// the call has answered.
func (s *graphStepCall) actorRefused() bool {
	refusal, err := workflowActorRefusal(s.ctx, s.db, s.run)
	if err != nil {
		s.unavail(err, "could not check the run-as user's role")
		return true
	}
	if refusal != "" {
		s.refuse(refusal, true)
		return true
	}
	return false
}

// ---- stop and record ------------------------------------------------------

// stop ends a step that is still running: "stop" when someone stopped the
// run, "timeout" when the step outlived its timeout. A pipeline run the step
// started is stopped too; a model run is left to finish (it is the model's,
// and other steps or schedules may be waiting on it).
func (s *graphStepCall) stop(reason string) {
	if stepFinal(s.row.status) {
		return
	}
	status, errText, note := workflow.StatusStopped, "", workflowStepStoppedNote
	if reason == "timeout" {
		status, note = workflow.StatusFailed, ""
		errText = fmt.Sprintf("The step did not finish within %d minutes.", workflowStepTimeout(s.step))
		if s.step.Type == workflow.StepWait {
			errText = fmt.Sprintf("The wait did not end within %d minutes.", s.step.Minutes)
		}
	}
	switch s.step.Type {
	case workflow.StepRunPipeline:
		if s.row.executionID != "" {
			why := "Stopped by its workflow run."
			if reason == "timeout" {
				why = errText
			}
			stopWorkflowStepPipelineRun(context.WithoutCancel(s.ctx), s.db, s.step.PipelineID, s.row.executionID, why)
		}
	case workflow.StepRefreshModel:
		if s.row.status == workflow.StatusWaiting {
			note = strings.TrimSpace(note + " " + workflowStepModelLeftNote)
		}
	case workflow.StepSend:
		if _, err := s.db.ExecContext(context.WithoutCancel(s.ctx), `
			UPDATE workflow_sends SET status = 'skipped', error = $3, finished_at = NOW()
			WHERE run_id = $1 AND step_id = $2 AND status = 'pending'
		`, s.run.RunID, s.step.ID, workflowSendSkipError); err != nil {
			s.unavail(err, "could not record the sends")
			return
		}
	}
	s.settle(status, errText, note)
}

// stopWorkflowStepPipeline stops a pipeline run a step started, as the Stop
// button does a batch run: cancel its Temporal workflow, then mark the run
// cancelled. A run that already ended is left as it is.
func stopWorkflowStepPipeline(ctx context.Context, database *sql.DB, pipelineID, execID, why string) {
	var status string
	if err := database.QueryRowContext(ctx, `SELECT status FROM executions WHERE id = $1`, execID).Scan(&status); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.WithError(err).WithField("execution_id", execID).Warn("[workflows] stop: could not read the pipeline run")
		}
		return
	}
	if status != "running" && status != "pending" {
		return
	}
	payload := map[string]interface{}{
		"pipeline_id":  pipelineID,
		"execution_id": execID,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"action":       "stop",
	}
	if getTemporalClient() == nil {
		log.WithField("execution_id", execID).Warn("[workflows] stop: no Temporal client to signal the pipeline run")
	} else if err := signalPipelineWorkflowWithFallback(ctx, pipelineID, execID, "cancel", payload); err != nil {
		log.WithError(err).WithField("execution_id", execID).Warn("[workflows] stop: could not signal the pipeline run")
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE executions SET status = 'cancelled', end_time = NOW(), error_message = COALESCE(error_message, $2)
		WHERE id = $1 AND status IN ('running', 'pending')
	`, execID, why); err != nil {
		log.WithError(err).WithField("execution_id", execID).Warn("[workflows] stop: could not mark the pipeline run cancelled")
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE pipelines SET status = 'stopped', updated_at = NOW() WHERE id = $1 AND status = 'running'
	`, pipelineID); err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Warn("[workflows] stop: could not mark the pipeline stopped")
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE pipeline_progress SET status = 'cancelled', message = $3, updated_at = NOW()
		WHERE pipeline_id = $1 AND execution_id = $2
	`, pipelineID, execID, why); err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Warn("[workflows] stop: could not mark the progress cancelled")
	}
	stopBatchSinkWorkersCtx(ctx, pipelineID, "")
}

// recordRefusal says why a record call is not one this step may make.
func (s *graphStepCall) recordRefusal() string {
	switch s.req.Status {
	case workflow.StatusSkipped, workflow.StatusFailed:
		return ""
	case workflow.StatusRunning, workflow.StatusSucceeded:
		if s.step.Type == workflow.StepWait {
			return ""
		}
	}
	return fmt.Sprintf("a %s step cannot record %q", s.step.Type, s.req.Status)
}

// record writes what the adapter decided: a step its parents skipped, a step
// that failed in the adapter, and a wait step's start and end.
func (s *graphStepCall) record() {
	switch s.req.Status {
	case workflow.StatusRunning:
		if s.settle(workflow.StatusWaiting, "", s.req.Note) && s.loadRow() {
			s.reply(s.row.answer())
		}
	case workflow.StatusFailed:
		msg := s.req.Note
		if msg == "" {
			msg = workflowStepFailedError
		}
		s.finish(workflow.StatusFailed, msg, "", graphStepAnswer{})
	default:
		s.finish(s.req.Status, "", s.req.Note, graphStepAnswer{})
	}
}

// ---- query and condition --------------------------------------------------

// runQuery runs the query as the run's actor and holds its result for the
// steps after it. Only the row count is recorded.
func (s *graphStepCall) runQuery() {
	actor := workflowRunActor(s.run)
	if actor == "" {
		s.refuse("The workflow has no run-as user. An owner or admin should turn it on again.", true)
		return
	}
	role, err := resolveWorkspaceRoleForUser(s.ctx, s.db, s.run.WorkspaceID, actor)
	if err != nil {
		s.unavail(err, "could not check the run-as user's role")
		return
	}
	if refusal := workflowRoleRefusal(role); refusal != "" {
		s.refuse(refusal, true)
		return
	}
	if s.run.ConnectionID == "" {
		s.refuse("The workflow's connection was deleted.", true)
		return
	}
	if reason := workflowSQLRefusal(s.step.SQL, role); reason != "" {
		s.refuse(reason, true)
		return
	}
	res, _, err := workflowQuery(s.ctx, s.db, s.run.WorkspaceID, s.run.ConnectionID, s.step.SQL, role)
	if err != nil {
		s.finish(workflow.StatusFailed, err.Error(), "", graphStepAnswer{})
		return
	}
	if err := workflowResults.Put(s.ctx, workflowResultKey(s.run.RunID, s.step.ID), res,
		workflowResultTTL(s.run.Definition)); err != nil {
		s.unavail(err, "could not hold the query's result")
		return
	}
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE workflow_step_runs SET row_count = $4
		WHERE run_id = $1 AND step_id = $2 AND attempt = $3
	`, s.run.RunID, s.step.ID, s.attempt, res.RowCount); err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	if _, err := s.db.ExecContext(s.ctx, `UPDATE workflow_runs SET row_count = $2 WHERE id = $1 AND status = 'running'`,
		s.run.RunID, res.RowCount); err != nil {
		s.unavail(err, "could not record the run")
		return
	}
	s.finish(workflow.StatusSucceeded, "", "", graphStepAnswer{})
}

// queryResult reads the query step's held result.
func (s *graphStepCall) queryResult() (workflow.Result, bool, error) {
	q := s.run.Definition.QueryStep()
	if q == nil {
		return workflow.Result{}, false, nil
	}
	return workflowResults.Get(s.ctx, workflowResultKey(s.run.RunID, q.ID))
}

func conditionBranches(st workflow.Step) []workflow.Branch {
	bs := make([]workflow.Branch, 0, len(st.Conditions))
	for _, c := range st.Conditions {
		bs = append(bs, workflow.Branch{Condition: c})
	}
	return bs
}

// runCondition decides which case the run takes: its case_index, -1 for
// none. The run's condition_met, branch and notes say the same, as a run
// before step 5 recorded them.
func (s *graphStepCall) runCondition() {
	res, held, err := s.queryResult()
	if err != nil {
		s.unavail(err, "could not read the query's result")
		return
	}
	if !held {
		s.finish(workflow.StatusFailed, workflowResultGone, "", graphStepAnswer{})
		return
	}
	dec, err := workflow.Decide(conditionBranches(s.step), res)
	if err != nil {
		s.finish(workflow.StatusFailed, "The condition could not be decided: "+err.Error()+".", "", graphStepAnswer{})
		return
	}
	met := dec.Branch >= 0
	var branch interface{} = sql.NullInt64{}
	if met {
		branch = dec.Branch
	}
	notes := dec.Notes
	if notes == nil {
		notes = []string{}
	}
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE workflow_step_runs SET case_index = $4
		WHERE run_id = $1 AND step_id = $2 AND attempt = $3
	`, s.run.RunID, s.step.ID, s.attempt, dec.Branch); err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE workflow_runs SET condition_met = $2, branch = $3, notes = $4 WHERE id = $1 AND status = 'running'
	`, s.run.RunID, met, branch, pgdriver.StringArray(notes)); err != nil {
		s.unavail(err, "could not record the run")
		return
	}
	s.finish(workflow.StatusSucceeded, "", strings.Join(dec.Notes, " "), graphStepAnswer{})
}

// ---- send -----------------------------------------------------------------

// planSends renders the step's message, holds it for the sends, and writes
// the step's ledger: the channels the adapter then sends to one by one.
func (s *graphStepCall) planSends() {
	channels, err := s.ledger()
	if err != nil {
		s.unavail(err, "could not read the sends")
		return
	}
	if len(channels) > 0 {
		s.reply(graphStepAnswer{Status: s.row.status, Channels: channels})
		return
	}

	st := s.step
	var (
		res workflow.Result
		out workflow.Outcome
	)
	if st.Case != nil {
		cond := s.run.Definition.ConditionStep()
		if cond == nil || *st.Case < 0 || *st.Case >= len(cond.Conditions) {
			s.finish(workflow.StatusFailed, "The condition this message answers is not in the workflow.", "",
				graphStepAnswer{Refused: true})
			return
		}
		if q := s.run.Definition.QuietMinutes; q > 0 {
			recent, err := workflowSentRecently(s.ctx, s.db, s.run.WorkflowID, s.run.RunID, st.ID, q)
			if err != nil {
				s.unavail(err, "could not read the recent sends")
				return
			}
			if recent {
				held := "The condition was true"
				if len(cond.Conditions) > 1 {
					held = fmt.Sprintf("Condition %d was true", *st.Case+1)
				}
				s.finish(workflow.StatusSkipped, "", fmt.Sprintf(
					"%s, but it sent a message less than %d minutes ago, so this one was not.", held, q), graphStepAnswer{})
				return
			}
		}
		r, held, err := s.queryResult()
		if err != nil {
			s.unavail(err, "could not read the query's result")
			return
		}
		if !held {
			s.finish(workflow.StatusFailed, workflowResultGone, "", graphStepAnswer{})
			return
		}
		o, err := workflow.Evaluate(cond.Conditions[*st.Case], r)
		if err != nil {
			s.finish(workflow.StatusFailed, "The condition could not be decided: "+err.Error()+".", "", graphStepAnswer{})
			return
		}
		res, out = r, o
	}

	chs, err := loadWorkflowSendChannels(s.ctx, s.db, s.run.WorkspaceID, s.run.WorkflowID, st.ID)
	if err != nil {
		s.unavail(err, "could not load the delivery channels")
		return
	}
	if len(chs) == 0 {
		s.finish(workflow.StatusFailed, "This step has no delivery channel to send to.", "", graphStepAnswer{Refused: true})
		return
	}
	var m workflow.Message
	if st.Message != nil {
		m = *st.Message
	}
	drawings, drawingNotes, err := s.sendDrawings()
	if err != nil {
		s.unavail(err, "could not read the charts and dashboards")
		return
	}
	msg, notes := renderWorkflowMessageWithDrawings(s.run.Name, m, res, out, workflowURL(s.run.WorkflowID), drawings, time.Now())
	notes = append(drawingNotes, notes...)

	ids, err := s.writeLedger(chs, msg)
	if errors.Is(err, errWorkflowMessageNotHeld) {
		s.unavail(err, "the message could not be held for sending")
		return
	}
	if err != nil {
		s.unavail(err, "could not plan the sends")
		return
	}
	if ids == nil {
		// The run was stopped or ended while this step planned.
		s.reply(graphStepAnswer{Status: workflow.StatusStopped, Stop: true})
		return
	}
	if len(notes) > 0 {
		if _, err := s.db.ExecContext(s.ctx, `
			UPDATE workflow_step_runs SET note = $4 WHERE run_id = $1 AND step_id = $2 AND attempt = $3
		`, s.run.RunID, st.ID, s.attempt, workflow.CapRunes(strings.Join(notes, " "), workflowErrorMax)); err != nil {
			log.WithError(err).WithFields(s.fields()).Warn("[workflows] plan: could not record the message's notes")
		}
	}
	s.reply(graphStepAnswer{Status: workflow.StatusRunning, Channels: ids})
}

// ledger lists the step's planned sends, in send order.
func (s *graphStepCall) ledger() ([]string, error) {
	rows, err := s.db.QueryContext(s.ctx, `
		SELECT channel_id FROM workflow_sends WHERE run_id = $1 AND step_id = $2 ORDER BY position
	`, s.run.RunID, s.step.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// writeLedger holds the message and writes the step's sends, with the run
// locked so a Stop cannot land between the check and the write. nil ids: the
// run is no longer running, or someone stopped it.
func (s *graphStepCall) writeLedger(chs []*sendChannel, msg renderedMessage) ([]string, error) {
	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var (
		status  string
		stopped bool
	)
	if err := tx.QueryRowContext(s.ctx, `
		SELECT status, stop_requested_at IS NOT NULL FROM workflow_runs WHERE id = $1 FOR UPDATE
	`, s.run.RunID).Scan(&status, &stopped); err != nil {
		return nil, err
	}
	if status != "running" || stopped {
		return nil, nil
	}
	if err := workflowMessages.Put(s.ctx, workflowStepMessageKey(s.run.RunID, s.step.ID), msg); err != nil {
		return nil, fmt.Errorf("%w: %v", errWorkflowMessageNotHeld, err)
	}
	ids := make([]string, 0, len(chs))
	for i, ch := range chs {
		if _, err := tx.ExecContext(s.ctx, `
			INSERT INTO workflow_sends (run_id, step_id, channel_id, position, name, kind)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING
		`, s.run.RunID, s.step.ID, ch.ID, i, ch.Name, ch.Kind); err != nil {
			return nil, err
		}
		ids = append(ids, ch.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// finishSends closes the step's ledger: a send not made is skipped, one cut
// off mid-post failed; the step fails when any channel failed.
func (s *graphStepCall) finishSends() {
	ctx := context.WithoutCancel(s.ctx)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflow_sends SET status = 'skipped', error = $3, finished_at = NOW()
		WHERE run_id = $1 AND step_id = $2 AND status = 'pending'
	`, s.run.RunID, s.step.ID, workflowSendSkipError); err != nil {
		s.unavail(err, "could not record the sends")
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflow_sends SET status = 'failed', error = $3, finished_at = NOW()
		WHERE run_id = $1 AND step_id = $2 AND status = 'sending'
	`, s.run.RunID, s.step.ID, workflowSendCutOffError); err != nil {
		s.unavail(err, "could not record the sends")
		return
	}
	var failed, total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE status = 'failed'), COUNT(*)
		FROM workflow_sends WHERE run_id = $1 AND step_id = $2
	`, s.run.RunID, s.step.ID).Scan(&failed, &total); err != nil {
		s.unavail(err, "could not read the sends")
		return
	}
	if err := workflowMessages.Delete(ctx, workflowStepMessageKey(s.run.RunID, s.step.ID)); err != nil {
		log.WithError(err).WithFields(s.fields()).Warn("[workflows] finish: could not delete the message; it expires on its own")
	}
	if failed > 0 {
		s.finish(workflow.StatusFailed, fmt.Sprintf("%d of %d delivery channels failed.", failed, total), "", graphStepAnswer{})
		return
	}
	s.finish(workflow.StatusSucceeded, "", "", graphStepAnswer{})
}

// ---- run_pipeline ---------------------------------------------------------

// workflowStepExecutionID is the pipeline run one attempt of a step starts:
// the same id however often the start is retried, so it starts once.
func workflowStepExecutionID(runID, stepID string, attempt int) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s_%s_%d", runID, stepID, attempt))).String()
}

// workflowPipelineGate is the checks the Run button makes before a run
// starts, as the run's actor. refusal fails the step; wait parks it.
func workflowPipelineGate(ctx context.Context, database *sql.DB, ws, actor, pipelineID string) (refusal, wait string) {
	if ref := runQuotaRefusal(ctx, database, ws, actor); ref != nil {
		return refusalMessage(ref.body, "The workspace has used its data allowance."), ""
	}
	cooldown := 10
	if v := strings.TrimSpace(os.Getenv("PIPELINE_RUN_COOLDOWN_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cooldown = n
		}
	}
	if !checkPipelineRunCooldown(ctx, database, pipelineID, cooldown) {
		return "", "Waiting: another run of the pipeline started in the last " + strconv.Itoa(cooldown) + " seconds."
	}
	return "", ""
}

// refusalMessage is a refusal body's sentence.
func refusalMessage(body gin.H, fallback string) string {
	for _, k := range []string{"message", "error"} {
		if v, ok := body[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

// startPipeline starts the step's pipeline run as the run's actor. A busy
// pipeline (a run in progress, or the cooldown) parks the step: the adapter
// asks again, and the step starts its own run once the pipeline is free.
func (s *graphStepCall) startPipeline() {
	execID := workflowStepExecutionID(s.run.RunID, s.step.ID, s.attempt)
	var existing string
	err := s.db.QueryRowContext(s.ctx, `SELECT status FROM executions WHERE id = $1`, execID).Scan(&existing)
	if err == nil {
		s.markStarted(execID)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.unavail(err, "could not read the pipeline run")
		return
	}
	if s.actorRefused() {
		return
	}
	name, mode, err := workflowStepPipeline(s.ctx, s.db, s.run.WorkspaceID, s.step.PipelineID)
	if errors.Is(err, sql.ErrNoRows) {
		s.refuse(workflowStepPipelineGone, true)
		return
	}
	if err != nil {
		s.unavail(err, "could not read the pipeline")
		return
	}
	if mode != "batch" {
		s.refuse(fmt.Sprintf("%s is now a %s pipeline, which keeps running until it is stopped. "+
			"A workflow runs batch pipelines only.", name, workflowPipelineModeName(mode)), true)
		return
	}
	actor := workflowRunActor(s.run)
	refusal, wait := workflowStepPipelineGate(s.ctx, s.db, s.run.WorkspaceID, actor, s.step.PipelineID)
	if refusal != "" {
		s.refuse(refusal, false)
		return
	}
	if wait != "" {
		s.park(wait)
		return
	}
	_, ref := startWorkflowStepPipeline(s.ctx, s.db, s.step.PipelineID, s.run.WorkspaceID, actor, RunPipelineRequest{},
		pipelineRunOpts{ExecID: execID, WorkflowRunID: s.run.RunID, DraftGate: true, DraftWorkspaceID: s.run.WorkspaceID})
	switch {
	case ref == nil:
		s.markStarted(execID)
	case ref.status == http.StatusConflict:
		s.park(workflowStepPipelineBusy)
	case ref.status >= 500:
		s.unavail(errors.New(refusalMessage(ref.body, "")), "could not start the pipeline run")
	default:
		// A 4xx refusal (the draft gate included) comes before the run is written.
		s.refuse(refusalMessage(ref.body, "The pipeline run could not start."), false)
	}
}

// park records the step waiting and asks the adapter to ask again.
func (s *graphStepCall) park(note string) {
	if !s.settle(workflow.StatusWaiting, "", note) || !s.loadRow() {
		return
	}
	a := s.row.answer()
	a.Again = true
	s.reply(a)
}

// markStarted records the step's pipeline run.
func (s *graphStepCall) markStarted(execID string) {
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE workflow_step_runs SET status = 'running', execution_id = $4, note = NULL
		WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND status IN ('running', 'waiting')
	`, s.run.RunID, s.step.ID, s.attempt, execID); err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	if !s.loadRow() {
		return
	}
	s.reply(s.row.answer())
}

// pollPipeline reads the step's pipeline run once; the adapter polls.
func (s *graphStepCall) pollPipeline() {
	if s.row.executionID == "" {
		s.c.JSON(http.StatusBadRequest, gin.H{"error": "the step has not started a pipeline run"})
		s.answered = true
		return
	}
	var status, errMsg, progress string
	err := s.db.QueryRowContext(s.ctx, `
		SELECT e.status, COALESCE(e.error_message, ''), COALESCE(pp.status, '')
		FROM executions e
		LEFT JOIN pipeline_progress pp ON pp.pipeline_id = e.pipeline_id AND pp.execution_id::text = e.id::text
		WHERE e.id = $1
	`, s.row.executionID).Scan(&status, &errMsg, &progress)
	if errors.Is(err, sql.ErrNoRows) {
		s.finish(workflow.StatusFailed, "The pipeline run was deleted.", "", graphStepAnswer{})
		return
	}
	if err != nil {
		s.unavail(err, "could not read the pipeline run")
		return
	}
	switch status {
	case "completed":
		s.finish(workflow.StatusSucceeded, "", "", graphStepAnswer{})
	case "failed", "cancelled":
		if errMsg == "" {
			errMsg = "The pipeline run " + status + "."
		}
		s.finish(workflow.StatusFailed, errMsg, "", graphStepAnswer{})
	default:
		if progress == "waiting_for_user" {
			s.park("The pipeline run is waiting for someone to answer its question.")
			return
		}
		if s.row.status == workflow.StatusWaiting && !s.settle(workflow.StatusRunning, "", "") {
			return
		}
		if !s.loadRow() {
			return
		}
		a := s.row.answer()
		a.Again = true
		s.reply(a)
	}
}

// ---- refresh_model --------------------------------------------------------

// requestModel records when the step asked for its model's run and gives the
// adapter the signal to send. The step waits for the first run of the model
// that starts after it asked. A resignal follows a run the model skipped
// because it was busy: the step asks again, once per such skip.
func (s *graphStepCall) requestModel() {
	var one int
	err := s.db.QueryRowContext(s.ctx, `SELECT 1 FROM saved_queries WHERE id = $1 AND workspace_id = $2`,
		s.step.ModelID, s.run.WorkspaceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		s.refuse(workflowStepModelGone, true)
		return
	}
	if err != nil {
		s.unavail(err, "could not read the model")
		return
	}
	if s.actorRefused() {
		return
	}
	if s.req.Resignal {
		if _, err := uuid.Parse(s.req.SkippedRunID); err != nil {
			s.c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skipped_run_id"})
			s.answered = true
			return
		}
		_, err = s.db.ExecContext(s.ctx, `
			UPDATE workflow_step_runs SET status = 'waiting', requested_at = NOW(), saved_query_run_id = NULL
			WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND status IN ('running', 'waiting')
			  AND saved_query_run_id = $4::uuid
		`, s.run.RunID, s.step.ID, s.attempt, s.req.SkippedRunID)
	} else {
		_, err = s.db.ExecContext(s.ctx, `
			UPDATE workflow_step_runs SET status = 'waiting', requested_at = COALESCE(requested_at, NOW())
			WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND status IN ('running', 'waiting')
		`, s.run.RunID, s.step.ID, s.attempt)
	}
	if err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	if !s.loadRow() {
		return
	}
	a := s.row.answer()
	a.Signal = &modelRefreshSignal{
		SavedQueryID: s.step.ModelID,
		UpstreamKind: upstreamKindWorkflow,
		UpstreamID:   s.run.WorkflowID,
		ExecutionID:  s.run.RunID,
	}
	s.reply(a)
}

// pollModel reads the first run of the model since the step asked: its
// outcome is the step's, and a run whose checks warned is "warned".
func (s *graphStepCall) pollModel() {
	if !s.row.requestedAt.Valid {
		s.c.JSON(http.StatusBadRequest, gin.H{"error": "the step has not asked for a model run"})
		s.answered = true
		return
	}
	var (
		runID, status, errMsg string
		rows                  sql.NullInt64
		warned                bool
	)
	err := s.db.QueryRowContext(s.ctx, `
		SELECT r.run_id::text, r.status, COALESCE(r.error, ''), r.rows_affected,
		       EXISTS (SELECT 1 FROM saved_query_check_results c
		               WHERE c.run_id = r.run_id AND c.status IN ('fail', 'error'))
		FROM saved_query_runs r
		WHERE r.saved_query_id = $1 AND r.started_at >= $2
		ORDER BY r.started_at
		LIMIT 1
	`, s.step.ModelID, s.row.requestedAt.Time).Scan(&runID, &status, &errMsg, &rows, &warned)
	if errors.Is(err, sql.ErrNoRows) {
		a := s.row.answer()
		a.Again = true
		s.reply(a)
		return
	}
	if err != nil {
		s.unavail(err, "could not read the model's runs")
		return
	}
	if _, err := s.db.ExecContext(s.ctx, `
		UPDATE workflow_step_runs SET saved_query_run_id = $4::uuid, row_count = $5
		WHERE run_id = $1 AND step_id = $2 AND attempt = $3 AND status IN ('running', 'waiting')
	`, s.run.RunID, s.step.ID, s.attempt, runID, rowCountParam(rows)); err != nil {
		s.unavail(err, "could not record the step")
		return
	}
	switch {
	case status == "succeeded" && warned:
		s.finish(workflow.StatusWarned, "", "A data check on the model's run warned.", graphStepAnswer{})
	case status == "succeeded":
		s.finish(workflow.StatusSucceeded, "", "", graphStepAnswer{})
	case status == "skipped" && errMsg == modelRunInProgressError:
		if !s.loadRow() {
			return
		}
		a := s.row.answer()
		a.Resignal, a.SkippedRunID = true, runID
		s.reply(a)
	default:
		if errMsg == "" {
			errMsg = "The model's run " + status + "."
		}
		s.finish(workflow.StatusFailed, errMsg, "", graphStepAnswer{})
	}
}

func rowCountParam(n sql.NullInt64) interface{} {
	if !n.Valid || n.Int64 > math.MaxInt32 || n.Int64 < math.MinInt32 {
		return sql.NullInt64{}
	}
	return n.Int64
}

// ---------------------------------------------------------------------------
// /complete
// ---------------------------------------------------------------------------

// CompleteWorkflowRunInternal records a graph run from its steps, once.
// POST /api/v1/internal/workflow-runs/:id/complete
//
// abort: the adapter could not run the graph to its end (an internal error);
// the run fails. A step still running is recorded stopped when someone
// stopped the run, else failed.
func CompleteWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	var req struct {
		Abort bool `json:"abort"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, gin.H{"status": "none"})
		return
	}
	if err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] complete: could not load the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not load the run"})
		return
	}
	o, finished, status, err := completeWorkflowRun(ctx, database, run, req.Abort)
	if err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] complete: could not record the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record the run"})
		return
	}
	if finished {
		auditWorkflowRun(ctx, database, run, o)
		dropWorkflowRunHeld(ctx, run)
		notifyWorkflowRunFailed(ctx, database, run, o)
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// dropWorkflowRunHeld deletes what a run held in Redis: its query's result,
// each chart and dashboard step's drawing and each send step's message. Each
// also expires on its own.
func dropWorkflowRunHeld(ctx context.Context, run *workflowRunRow) {
	if q := run.Definition.QueryStep(); q != nil {
		if err := workflowResults.Delete(ctx, workflowResultKey(run.RunID, q.ID)); err != nil {
			log.WithError(err).WithField("run_id", run.RunID).Warn("[workflows] could not delete the query's result; it expires on its own")
		}
	}
	for _, s := range run.Definition.Steps {
		switch s.Type {
		case workflow.StepChart:
			if err := workflowCharts.Delete(ctx, workflowResultKey(run.RunID, s.ID)); err != nil {
				log.WithError(err).WithField("run_id", run.RunID).Warn("[workflows] could not delete a chart; it expires on its own")
			}
		case workflow.StepDashboard:
			if err := workflowDashboards.Delete(ctx, workflowResultKey(run.RunID, s.ID)); err != nil {
				log.WithError(err).WithField("run_id", run.RunID).Warn("[workflows] could not delete a dashboard; it expires on its own")
			}
		}
	}
	for _, s := range run.Definition.SendSteps() {
		if err := workflowMessages.Delete(ctx, workflowStepMessageKey(run.RunID, s.ID)); err != nil {
			log.WithError(err).WithField("run_id", run.RunID).Warn("[workflows] could not delete a message; it expires on its own")
		}
	}
}

// completeWorkflowRun closes a run's sends and steps and records the run from
// its steps' last attempts, with the run locked. finished says whether this
// call recorded it.
func completeWorkflowRun(ctx context.Context, database *sql.DB, run *workflowRunRow, abort bool) (runOutcome, bool, string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return runOutcome{}, false, "", err
	}
	defer func() { _ = tx.Rollback() }()
	var (
		status   string
		rowCount sql.NullInt64
		stopped  bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, row_count, stop_requested_at IS NOT NULL FROM workflow_runs WHERE id = $1 FOR UPDATE
	`, run.RunID).Scan(&status, &rowCount, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return runOutcome{}, false, "none", nil
	}
	if err != nil {
		return runOutcome{}, false, "", err
	}
	if status != "running" {
		return runOutcome{}, false, status, nil
	}
	for _, q := range []struct{ sql, msg string }{
		{`UPDATE workflow_sends SET status = 'skipped', error = $2, finished_at = NOW()
		  WHERE run_id = $1 AND status = 'pending'`, workflowSendSkipError},
		{`UPDATE workflow_sends SET status = 'failed', error = $2, finished_at = NOW()
		  WHERE run_id = $1 AND status = 'sending'`, workflowSendCutOffError},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, run.RunID, q.msg); err != nil {
			return runOutcome{}, false, "", err
		}
	}
	leftover, leftErr, leftNote := workflow.StatusFailed, workflowStepEndedError, ""
	if stopped {
		leftover, leftErr, leftNote = workflow.StatusStopped, "", workflowStepStoppedNote
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_step_runs SET status = $2, error = COALESCE(NULLIF($3, ''), error),
		       note = COALESCE(NULLIF($4, ''), note), finished_at = NOW()
		WHERE run_id = $1 AND status IN ('running', 'waiting')
	`, run.RunID, leftover, leftErr, leftNote); err != nil {
		return runOutcome{}, false, "", err
	}

	statuses, errs := map[string]string{}, map[string]string{}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (step_id) step_id, status, COALESCE(error, '')
		FROM workflow_step_runs WHERE run_id = $1
		ORDER BY step_id, attempt DESC
	`, run.RunID)
	if err != nil {
		return runOutcome{}, false, "", err
	}
	for rows.Next() {
		var id, st, e string
		if err := rows.Scan(&id, &st, &e); err != nil {
			rows.Close()
			return runOutcome{}, false, "", err
		}
		statuses[id], errs[id] = st, e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return runOutcome{}, false, "", err
	}

	o := runOutcome{deliveries: []DeliveryOutcome{}}
	drows, err := tx.QueryContext(ctx, `
		SELECT step_id, channel_id, name, kind, status, sent, COALESCE(error, '')
		FROM workflow_sends WHERE run_id = $1 ORDER BY step_id, position
	`, run.RunID)
	if err != nil {
		return runOutcome{}, false, "", err
	}
	for drows.Next() {
		var d DeliveryOutcome
		if err := drows.Scan(&d.StepID, &d.ChannelID, &d.Name, &d.Kind, &d.Status, &d.Sent, &d.Error); err != nil {
			drows.Close()
			return runOutcome{}, false, "", err
		}
		o.deliveries = append(o.deliveries, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return runOutcome{}, false, "", err
	}
	if rowCount.Valid {
		n := int(rowCount.Int64)
		o.rowCount = &n
	}

	o.status = workflow.RunStatus(run.Definition, statuses, stopped)
	switch {
	case o.status == workflow.StatusStopped:
	case abort:
		o.status, o.err = workflow.StatusFailed, workflowStepFailedError
	case o.status == workflow.StatusFailed:
		o.err = firstFailedStep(run.Definition, statuses, errs)
		o.failedSteps = failingSteps(run.Definition, statuses)
	}
	deliveries, _ := json.Marshal(o.deliveries)
	var errText interface{} = sql.NullString{}
	if o.err != "" {
		errText = workflow.CapRunes(o.err, workflowErrorMax)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = $2, deliveries = $3, error = $4, finished_at = NOW()
		WHERE id = $1 AND status = 'running'
	`, run.RunID, o.status, string(deliveries), errText)
	if err != nil {
		return runOutcome{}, false, "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return runOutcome{}, false, status, nil
	}
	if err := tx.Commit(); err != nil {
		return runOutcome{}, false, "", err
	}
	return o, true, o.status, nil
}

// firstFailedStep is the run's error: the first step, in the workflow's
// order, whose failure failed the run.
func firstFailedStep(d workflow.Definition, statuses, errs map[string]string) string {
	failed := failingSteps(d, statuses)
	if len(failed) == 0 {
		return workflowStepFailedError
	}
	if e := strings.TrimSpace(errs[failed[0]]); e != "" {
		return fmt.Sprintf("Step %s failed: %s", failed[0], e)
	}
	return fmt.Sprintf("Step %s failed.", failed[0])
}

// failingSteps are the steps, in the workflow's order, whose failure failed the
// run (RunStatus's rule); none for a run that failed for another reason. The
// failure notice reads them (workflow_run_alert.go).
func failingSteps(d workflow.Definition, statuses map[string]string) []string {
	var ids []string
	for _, s := range d.Steps {
		if statuses[s.ID] != workflow.StatusFailed || (s.Type == workflow.StepSend && !s.Required) {
			continue
		}
		ids = append(ids, s.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// The Stop button
// ---------------------------------------------------------------------------

// StopWorkflowRun asks a running run to stop. The run records the request and
// each step reads it at its next call, so the stop holds even when Temporal's
// signal is lost; the signal only makes it prompt.
// POST /api/v1/workflows/:id/runs/:runId/stop
func StopWorkflowRun(c *gin.Context) {
	userID, ws, _, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	id, runID := c.Param("id"), c.Param("runId")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if _, err := uuid.Parse(runID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	ctx := c.Request.Context()
	var temporalID sql.NullString
	err := database.QueryRowContext(ctx, `
		UPDATE workflow_runs SET stop_requested_at = NOW(), stop_requested_by = NULLIF($4, '')::uuid
		WHERE id = $1 AND workflow_id = $2 AND workspace_id = $3 AND status = 'running' AND stop_requested_at IS NULL
		RETURNING temporal_workflow_id
	`, runID, id, ws, userID).Scan(&temporalID)
	if errors.Is(err, sql.ErrNoRows) {
		var (
			status    string
			requested bool
		)
		err := database.QueryRowContext(ctx, `
			SELECT status, stop_requested_at IS NOT NULL FROM workflow_runs
			WHERE id = $1 AND workflow_id = $2 AND workspace_id = $3
		`, runID, id, ws).Scan(&status, &requested)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		case err != nil:
			log.WithError(err).Error("[workflows] stop: could not read the run")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to stop the run"})
		case status == "running" && requested:
			c.JSON(http.StatusConflict, gin.H{"error": "already_stopping",
				"message": "This run is already stopping."})
		default:
			c.JSON(http.StatusConflict, gin.H{"error": "not_running",
				"message": "This run has already finished."})
		}
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflows] stop failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to stop the run"})
		return
	}
	if temporalID.Valid && temporalID.String != "" {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := signalWorkflowRunStop(sctx, temporalID.String); err != nil {
			log.WithError(err).WithField("run_id", runID).Warn("[workflows] stop: could not signal the run; its next step stops it")
		}
		cancel()
	}
	logAudit(c, "stop_workflow_run", "workflow", id, map[string]interface{}{"run_id": runID})
	c.JSON(http.StatusAccepted, gin.H{"status": "stopping"})
}

func temporalSignalWorkflowRunStop(ctx context.Context, temporalID string) error {
	tc := getTemporalClient()
	if tc == nil {
		return errNoTemporalClient
	}
	return tc.SignalWorkflow(ctx, temporalID, "", workflowRunStopSignal, nil)
}
