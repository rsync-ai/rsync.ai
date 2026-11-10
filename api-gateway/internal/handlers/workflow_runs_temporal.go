package handlers

// Workflow runs on Temporal (docs/internal/workflows-design.md § 4.1, build
// step 4). A run is a WorkflowRunWorkflow on temporal-adapter's `workflow-runs`
// queue; each of its steps is an activity that calls one endpoint here:
//
//	begin    — which run this is: a scheduled run inserts its row here, keyed
//	           on the Temporal workflow id, so a retried step finds it again.
//	evaluate — the checks, the query and the condition. A run that sends
//	           writes its sends ledger (workflow_sends) and returns the channels.
//	send     — one channel, once: the ledger row is claimed before the post and
//	           settled after, so a retry after an accepted send sends nothing.
//	finish   — the run's status and deliveries, from the ledger, once.
//
// What starts a run: a Temporal schedule per scheduled workflow (created on
// activate and edit, deleted on pause and delete, and reconciled when the
// gateway starts), an after-pipeline fire, or Run now. The gateway's ticker
// and run goroutines are gone; the query and the sends still run here, where
// the executors and the transports are.
//
// The message carries values from the rows, so it never enters Temporal's
// history: evaluate keeps it in Redis, encrypted, for workflowMessageTTL, and
// finish deletes it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/db"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

const (
	workflowRunsTaskQueue   = "workflow-runs"
	workflowRunWorkflowType = "WorkflowRunWorkflow"

	// workflowAbandonedAfter fails a run still running this long after it
	// started, or after the alive_until its steps last set: its Temporal
	// workflow is gone. A run without steps ends inside its 10 minute
	// deadline, and a scheduled run waits for a busy one for longer than
	// this, so the wait outlasts an abandoned run; a run with steps holds
	// alive_until 15 minutes ahead while it runs, so a scheduled run that
	// waits on one skips its tick when the adapter's wait gives up.
	workflowAbandonedAfter = 15 * time.Minute
	// workflowMessageTTL is how long a rendered message waits for its sends.
	workflowMessageTTL = 30 * time.Minute
	// workflowSendTimeout bounds one channel's post.
	workflowSendTimeout = 2 * time.Minute
	// workflowSendStaleAfter is when a send still marked sending was cut off:
	// past its own timeout, so no gateway is still posting it.
	workflowSendStaleAfter = workflowSendTimeout + 30*time.Second

	workflowAbandonedError  = "This run stopped without finishing; rsync.ai marked it failed."
	workflowNotStartedError = "rsync.ai could not start this run: its scheduler (Temporal) was not reachable."
	workflowSendCutOffError = "rsync.ai stopped while sending to this channel; the message may not have arrived."
	workflowSendSkipError   = "The run stopped before it sent to this channel."
	workflowMessageGone     = "The message was no longer held for sending (it waits 30 minutes), so it was not sent."
	workflowChannelGone     = "The delivery channel was deleted before this run sent to it."
	workflowDeadlineError   = "The run did not finish within 10 minutes, so rsync.ai stopped it."
	workflowStepFailedError = "rsync.ai hit an internal error running this workflow."
)

// errWorkflowRunNotStarted is a run inserted that Temporal did not take; the
// run is recorded failed.
var errWorkflowRunNotStarted = errors.New("workflow run not started")

// workflowRunInput is WorkflowRunWorkflow's argument. A scheduled run has no
// run_id: its first step inserts the run.
type workflowRunInput struct {
	RunID      string `json:"run_id,omitempty"`
	WorkflowID string `json:"workflow_id"`
	Trigger    string `json:"trigger"`
}

// Seams for tests: Temporal, and where a message waits for its sends.
var (
	startWorkflowRunExecution = temporalStartWorkflowRun
	upsertWorkflowSchedule    = temporalUpsertWorkflowSchedule
	deleteWorkflowSchedule    = temporalDeleteWorkflowSchedule
)

var workflowMessages workflowMessageStore = redisWorkflowMessages{}

// ---------------------------------------------------------------------------
// Temporal
// ---------------------------------------------------------------------------

// temporalStartWorkflowRun starts one run. A start Temporal already has is
// done, not failed: the id names the run. FAILED_ONLY lets a run whose
// workflow failed be started again under the same name.
func temporalStartWorkflowRun(ctx context.Context, temporalID string, in workflowRunInput) error {
	tc := getTemporalClient()
	if tc == nil {
		return errNoTemporalClient
	}
	_, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    temporalID,
		TaskQueue:             workflowRunsTaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, workflowRunWorkflowType, in)
	if isFanOutAlreadyStarted(err) {
		return nil
	}
	return err
}

func workflowScheduleID(workflowID string) string { return "workflow-schedule-" + workflowID }

// temporalCronExpressions is a workflow's cron as Temporal reads it. Both
// read five fields alike but one: when day-of-month and day-of-week are both
// set, the cron the workflow was validated with (robfig) runs on either and
// Temporal only on both. Two expressions, one per day field, are the "either".
func temporalCronExpressions(expr string) []string {
	f := strings.Fields(expr)
	if len(f) != 5 || cronFieldIsStar(f[2]) || cronFieldIsStar(f[4]) {
		return []string{expr}
	}
	return []string{
		strings.Join([]string{f[0], f[1], f[2], f[3], "*"}, " "),
		strings.Join([]string{f[0], f[1], "*", f[3], f[4]}, " "),
	}
}

// cronFieldIsStar is robfig v1's star: any part that starts with * or ?,
// stepped or not ("*/2" too, unlike robfig v3).
func cronFieldIsStar(field string) bool {
	for _, part := range strings.Split(field, ",") {
		if strings.HasPrefix(part, "*") || strings.HasPrefix(part, "?") {
			return true
		}
	}
	return false
}

// temporalUpsertWorkflowSchedule creates a scheduled workflow's Temporal
// schedule, or brings an existing one to this trigger and unpauses it. SKIP:
// a run never starts while the last scheduled one is still running. The
// default catch-up window: a time missed while Temporal was down runs once
// when it is back (SKIP drops the rest), as the gateway's ticker did.
func temporalUpsertWorkflowSchedule(ctx context.Context, workflowID string, t workflow.Trigger) error {
	tc := getTemporalClient()
	if tc == nil {
		return errNoTemporalClient
	}
	id := workflowScheduleID(workflowID)
	spec := client.ScheduleSpec{CronExpressions: temporalCronExpressions(t.Cron), TimeZoneName: t.Timezone}
	action := &client.ScheduleWorkflowAction{
		// Temporal adds each start's time, so a run's id names its time.
		ID:        "workflow-run:" + workflowID + ":schedule",
		Workflow:  workflowRunWorkflowType,
		TaskQueue: workflowRunsTaskQueue,
		Args:      []interface{}{workflowRunInput{WorkflowID: workflowID, Trigger: workflow.TriggerSchedule}},
	}
	_, err := tc.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:      id,
		Spec:    spec,
		Action:  action,
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		Note:    "rsync.ai workflow " + workflowID,
	})
	if !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
		return err
	}
	return tc.ScheduleClient().GetHandle(ctx, id).Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			s := in.Description.Schedule
			s.Spec = &spec
			s.Action = action
			if s.Policy == nil {
				s.Policy = &client.SchedulePolicies{}
			}
			s.Policy.Overlap = enumspb.SCHEDULE_OVERLAP_POLICY_SKIP
			if s.State == nil {
				s.State = &client.ScheduleState{}
			}
			s.State.Paused = false
			return &client.ScheduleUpdate{Schedule: &s}, nil
		},
	})
}

// temporalDeleteWorkflowSchedule deletes a workflow's schedule; one already
// gone is deleted.
func temporalDeleteWorkflowSchedule(ctx context.Context, workflowID string) error {
	tc := getTemporalClient()
	if tc == nil {
		return errNoTemporalClient
	}
	err := tc.ScheduleClient().GetHandle(ctx, workflowScheduleID(workflowID)).Delete(ctx)
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// dropWorkflowSchedule deletes a schedule best effort. One left behind costs
// one empty run: its next start finds the workflow not on a schedule, ends,
// and deletes it (beginScheduledWorkflowRun).
func dropWorkflowSchedule(ctx context.Context, workflowID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := deleteWorkflowSchedule(ctx, workflowID); err != nil {
		log.WithError(err).WithField("workflow_id", workflowID).
			Warn("[workflows] could not delete the Temporal schedule; its runs will find the workflow not active")
	}
}

// isScheduleRefused is Temporal refusing a schedule's spec (a cron or a time
// zone it cannot read), as opposed to not answering.
func isScheduleRefused(err error) bool {
	var bad *serviceerror.InvalidArgument
	return errors.As(err, &bad)
}

const workflowSchedulerUnreachable = "rsync.ai could not reach its scheduler (Temporal) to set this workflow's schedule. Try again in a minute."

// applyWorkflowSchedule creates or updates a workflow's schedule for a request
// that turns it on or edits it, and answers the request when that fails. The
// caller holds the workflow's row lock and rolls back on false.
func applyWorkflowSchedule(c *gin.Context, workflowID string, t workflow.Trigger) bool {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	err := upsertWorkflowSchedule(ctx, workflowID, t)
	switch {
	case err == nil:
		return true
	case isScheduleRefused(err):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "The schedule could not be read: " + err.Error(),
			"code": "invalid_cron"})
	default:
		log.WithError(err).WithField("workflow_id", workflowID).Warn("[workflows] could not set the Temporal schedule")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": workflowSchedulerUnreachable})
	}
	return false
}

var workflowReconcileOnce sync.Once

// ReconcileWorkflowSchedules makes sure every active scheduled workflow has its
// Temporal schedule: the ones turned on before step 4, and any whose create
// was lost. It retries until Temporal answers for all of them. A schedule
// Temporal refuses pauses its workflow with the reason.
func ReconcileWorkflowSchedules(ctx context.Context) {
	workflowReconcileOnce.Do(func() {
		go func() {
			for pause := 5 * time.Second; ; pause *= 2 {
				if pause > 2*time.Minute {
					pause = 2 * time.Minute
				}
				done, err := reconcileWorkflowSchedules(ctx)
				if done {
					return
				}
				log.WithError(err).Warn("[workflows] could not reconcile schedules with Temporal; retrying")
				if !waitOrDone(ctx, pause) {
					return
				}
			}
		}()
	})
}

func reconcileWorkflowSchedules(ctx context.Context) (bool, error) {
	database := db.GetDB()
	if database == nil {
		return false, errors.New("database not available")
	}
	rows, err := database.QueryContext(ctx, `
		SELECT id FROM workflows WHERE status = 'active' AND trigger_kind = 'schedule'`)
	if err != nil {
		return isUndefinedTable(err), err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	var lastErr error
	for _, id := range ids {
		if err := reconcileWorkflowSchedule(ctx, database, id); err != nil {
			lastErr = err
		}
	}
	if len(ids) > 0 && lastErr == nil {
		log.WithField("workflows", len(ids)).Info("[workflows] Temporal schedules reconciled")
	}
	return lastErr == nil, lastErr
}

// reconcileWorkflowSchedule sets one workflow's schedule from its definition
// as it is now, under the row's lock like every other schedule change, so an
// edit or a pause since the list was read cannot be overwritten.
func reconcileWorkflowSchedule(ctx context.Context, database *sql.DB, workflowID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	err = tx.QueryRowContext(ctx, `
		SELECT definition FROM workflows WHERE id = $1 AND status = 'active' AND trigger_kind = 'schedule'
		FOR UPDATE`, workflowID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var def workflow.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		log.WithError(err).WithField("workflow_id", workflowID).Warn("[workflows] unreadable definition")
		return nil
	}
	upsertCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = upsertWorkflowSchedule(upsertCtx, workflowID, def.Trigger)
	cancel()
	switch {
	case err == nil:
	case isScheduleRefused(err):
		reason := workflow.CapRunes("Its schedule could not be read: "+err.Error(), workflowErrorMax)
		if _, perr := tx.ExecContext(ctx, `
			UPDATE workflows SET status = 'paused', paused_reason = $2, next_run_at = NULL, updated_at = NOW()
			WHERE id = $1
		`, workflowID, reason); perr != nil {
			return perr
		}
	default:
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Where a message waits for its sends
// ---------------------------------------------------------------------------

type workflowMessageStore interface {
	Put(ctx context.Context, runID string, m renderedMessage) error
	Get(ctx context.Context, runID string) (renderedMessage, bool, error)
	Delete(ctx context.Context, runID string) error
}

// redisWorkflowMessages keeps each message encrypted, under the run's id, for
// workflowMessageTTL.
type redisWorkflowMessages struct{}

func workflowMessageKey(runID string) string { return "workflow-run-message:" + runID }

func (redisWorkflowMessages) client() (*redis.Client, error) {
	if schemaCache == nil {
		return nil, errors.New("Redis is not available")
	}
	return schemaCache.GetClient(), nil
}

func (s redisWorkflowMessages) Put(ctx context.Context, runID string, m renderedMessage) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(raw)
	if err != nil {
		return err
	}
	return rc.Set(ctx, workflowMessageKey(runID), enc, workflowMessageTTL).Err()
}

func (s redisWorkflowMessages) Get(ctx context.Context, runID string) (renderedMessage, bool, error) {
	rc, err := s.client()
	if err != nil {
		return renderedMessage{}, false, err
	}
	enc, err := rc.Get(ctx, workflowMessageKey(runID)).Result()
	if errors.Is(err, redis.Nil) {
		return renderedMessage{}, false, nil
	}
	if err != nil {
		return renderedMessage{}, false, err
	}
	raw, err := crypto.Decrypt(enc)
	if err != nil {
		return renderedMessage{}, false, err
	}
	var m renderedMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return renderedMessage{}, false, err
	}
	return m, true, nil
}

func (s redisWorkflowMessages) Delete(ctx context.Context, runID string) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	return rc.Del(ctx, workflowMessageKey(runID)).Err()
}

// ---------------------------------------------------------------------------
// Starting runs
// ---------------------------------------------------------------------------

// failAbandonedWorkflowRuns fails one workflow's runs that outlived any live
// run, so a lost one does not block the next for good. A run with steps
// (migration 140) keeps alive_until ahead of now while it runs, since its
// steps may take hours; a run without one is abandoned 15 minutes after it
// started. Its steps still running fail with it.
func failAbandonedWorkflowRuns(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}, workflowID string) error {
	_, err := q.ExecContext(ctx, `
		WITH gone AS (
			UPDATE workflow_runs SET status = 'failed', finished_at = NOW(), error = $2
			WHERE workflow_id = $1 AND status = 'running'
			  AND COALESCE(alive_until, started_at + make_interval(secs => $3)) < NOW()
			RETURNING id
		)
		UPDATE workflow_step_runs s SET status = 'failed', error = COALESCE(s.error, $4), finished_at = NOW()
		FROM gone WHERE s.run_id = gone.id AND s.status IN ('running', 'waiting')
	`, workflowID, workflowAbandonedError, int(workflowAbandonedAfter.Seconds()), workflowStepEndedError)
	return err
}

// markWorkflowRunNotStarted records a run Temporal never took.
func markWorkflowRunNotStarted(ctx context.Context, database *sql.DB, runID string) {
	if _, err := database.ExecContext(ctx, `
		UPDATE workflow_runs SET status = 'failed', finished_at = NOW(), error = $2
		WHERE id = $1 AND status = 'running'
	`, runID, workflowNotStartedError); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] could not record a run that did not start")
	}
}

// startManualWorkflowRun inserts a Run now run and hands it to Temporal, once:
// the person who clicked is waiting. A run Temporal did not take is recorded
// failed and errWorkflowRunNotStarted returned.
func startManualWorkflowRun(ctx context.Context, database *sql.DB, workflowID, workspaceID, startedBy string) (string, error) {
	runID := uuid.NewString()
	temporalID := "workflow-run-" + runID
	var by interface{} = sql.NullString{}
	if startedBy != "" {
		by = startedBy
	}
	res, err := database.ExecContext(ctx, `
		INSERT INTO workflow_runs (id, workflow_id, workspace_id, trigger, started_by, definition, temporal_workflow_id)
		SELECT $1, $2, $3, 'manual', $4, w.definition, $5 FROM workflows w WHERE w.id = $2 AND w.workspace_id = $3
	`, runID, workflowID, workspaceID, by, temporalID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", sql.ErrNoRows
	}
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := startWorkflowRunExecution(startCtx, temporalID,
		workflowRunInput{RunID: runID, WorkflowID: workflowID, Trigger: workflow.TriggerManual}); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] could not hand a run to Temporal")
		markWorkflowRunNotStarted(context.WithoutCancel(ctx), database, runID)
		return runID, errWorkflowRunNotStarted
	}
	return runID, nil
}

// pendingWorkflowRun is an after-pipeline run to hand to Temporal.
type pendingWorkflowRun struct {
	RunID      string
	WorkflowID string
	TemporalID string
}

// FireWorkflowsAfterPipeline starts every active workflow that runs after
// pipelineID. It is called where FireModelsAfterPipeline is (a pipeline's
// completion), returns at once, and fires each workflow once per executionID
// however often the event repeats. A run Temporal does not take within the
// fan-out's retry budget is recorded failed. A CDC pipeline's window fires the
// same runs synchronously, from fireAfterCDCWindow (cdc_trailing_edge.go).
func FireWorkflowsAfterPipeline(_ context.Context, pipelineID, executionID string) {
	database := db.GetDB()
	if database == nil || pipelineID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		runs, err := fireWorkflowsAfterPipeline(ctx, database, pipelineID, executionID)
		if err != nil {
			if !isUndefinedTable(err) {
				log.WithError(err).WithField("pipeline_id", pipelineID).Warn("[workflows] after-pipeline fire failed")
			}
			return
		}
		for _, r := range startWorkflowRuns(ctx, fanOutTemporalBreaker, runs) {
			markWorkflowRunNotStarted(ctx, database, r.RunID)
		}
	}()
}

// fireWorkflowsAfterPipeline inserts one run per active workflow that follows
// pipelineID, in the pipeline's own workspace, and returns the runs to start.
// A re-delivered event inserts nothing: the partial unique index on
// (workflow_id, trigger_ref) holds the first. It also returns this event's
// runs still running, so a fire repeated after Temporal did not take them
// (the CDC sweep's retry) starts them; one Temporal has is not started twice.
// The workflows' abandoned runs are failed first.
func fireWorkflowsAfterPipeline(ctx context.Context, database *sql.DB, pipelineID, executionID string) ([]pendingWorkflowRun, error) {
	var ref interface{} = sql.NullString{}
	if executionID != "" {
		ref = executionID
	}
	if _, err := database.ExecContext(ctx, `
		WITH gone AS (
			UPDATE workflow_runs r SET status = 'failed', finished_at = NOW(), error = $2
			FROM workflows w
			WHERE w.id = r.workflow_id AND w.trigger_pipeline_id = $1
			  AND r.status = 'running' AND COALESCE(r.alive_until, r.started_at + make_interval(secs => $3)) < NOW()
			RETURNING r.id
		)
		UPDATE workflow_step_runs s SET status = 'failed', error = COALESCE(s.error, $4), finished_at = NOW()
		FROM gone WHERE s.run_id = gone.id AND s.status IN ('running', 'waiting')
	`, pipelineID, workflowAbandonedError, int(workflowAbandonedAfter.Seconds()), workflowStepEndedError); err != nil {
		return nil, err
	}
	rows, err := database.QueryContext(ctx, `
		WITH fired AS (
			INSERT INTO workflow_runs (id, workflow_id, workspace_id, trigger, trigger_ref, definition, temporal_workflow_id)
			SELECT f.run_id, f.id, f.workspace_id, 'after_pipeline', $2::text, f.definition,
			       COALESCE('workflow-run:' || f.id::text || ':' || $2::text, 'workflow-run-' || f.run_id::text)
			FROM (
				SELECT uuid_generate_v4() AS run_id, w.id, w.workspace_id, w.definition
				FROM workflows w
				JOIN pipelines p ON p.id = w.trigger_pipeline_id AND p.workspace_id = w.workspace_id
				WHERE w.status = 'active' AND w.trigger_kind = 'after_pipeline' AND w.trigger_pipeline_id = $1
			) f
			ON CONFLICT (workflow_id, trigger_ref) WHERE trigger = 'after_pipeline' DO NOTHING
			RETURNING id, workflow_id, temporal_workflow_id
		)
		SELECT id, workflow_id, temporal_workflow_id FROM fired
		UNION ALL
		SELECT r.id, r.workflow_id, r.temporal_workflow_id
		FROM workflow_runs r JOIN workflows w ON w.id = r.workflow_id
		WHERE $2::text IS NOT NULL AND r.trigger = 'after_pipeline' AND r.trigger_ref = $2::text
		  AND r.status = 'running' AND r.temporal_workflow_id IS NOT NULL AND w.trigger_pipeline_id = $1
	`, pipelineID, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []pendingWorkflowRun
	for rows.Next() {
		var r pendingWorkflowRun
		if err := rows.Scan(&r.RunID, &r.WorkflowID, &r.TemporalID); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// startWorkflowRuns hands after-pipeline runs to Temporal, retrying inside the
// fan-out's budget behind the fan-out's breaker, and returns the runs it could
// not start.
func startWorkflowRuns(ctx context.Context, b *fanOutBreaker, runs []pendingWorkflowRun) []pendingWorkflowRun {
	if len(runs) == 0 {
		return nil
	}
	if !b.acquire() {
		return runs
	}
	ctx, cancel := context.WithTimeout(ctx, fanOutRetryBudget)
	defer cancel()
	pending := runs
	var lastErr error
	for pause := fanOutRetryFirstPause; ; pause *= 2 {
		var next []pendingWorkflowRun
		lastErr = nil
		for _, r := range pending {
			err := startWorkflowRunExecution(ctx, r.TemporalID,
				workflowRunInput{RunID: r.RunID, WorkflowID: r.WorkflowID, Trigger: workflow.TriggerAfterPipeline})
			if err != nil {
				lastErr = err
				next = append(next, r)
			}
		}
		pending = next
		if len(pending) == 0 {
			break
		}
		log.WithError(lastErr).WithField("runs", len(pending)).Warn("[workflows] could not reach Temporal to start runs")
		if !waitOrDone(ctx, pause) {
			break
		}
	}
	b.observe(lastErr)
	return pending
}

// ---------------------------------------------------------------------------
// The run's steps (internal endpoints, called by temporal-adapter)
// ---------------------------------------------------------------------------

func validRunPathID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return "", false
	}
	return id, true
}

func workflowRunsDB(c *gin.Context) *sql.DB {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
	}
	return database
}

// BeginWorkflowRunInternal says which run a WorkflowRunWorkflow is.
// POST /api/v1/internal/workflows/:id/runs/begin
//
// action "run" carries the run id; "busy" means wait and ask again (a scheduled
// run, while another run of the workflow is running); "none" means end.
func BeginWorkflowRunInternal(c *gin.Context) {
	workflowID, ok := validRunPathID(c)
	if !ok {
		return
	}
	var req struct {
		RunID              string `json:"run_id"`
		Trigger            string `json:"trigger"`
		TemporalWorkflowID string `json:"temporal_workflow_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	none := func() { c.JSON(http.StatusOK, gin.H{"action": "none"}) }

	if req.RunID != "" {
		if _, err := uuid.Parse(req.RunID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run_id"})
			return
		}
		var status string
		err := database.QueryRowContext(ctx, `SELECT status FROM workflow_runs WHERE id = $1 AND workflow_id = $2`,
			req.RunID, workflowID).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			none()
		case err != nil:
			log.WithError(err).WithField("run_id", req.RunID).Error("[workflows] begin: could not read the run")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read the run"})
		case status == "running":
			c.JSON(http.StatusOK, gin.H{"action": "run", "run_id": req.RunID})
		default:
			none()
		}
		return
	}
	if req.Trigger != workflow.TriggerSchedule || req.TemporalWorkflowID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a run without run_id must be a scheduled one with its temporal_workflow_id"})
		return
	}
	action, runID, err := beginScheduledWorkflowRun(ctx, database, workflowID, req.TemporalWorkflowID)
	if err != nil {
		log.WithError(err).WithField("workflow_id", workflowID).Error("[workflows] begin: could not start a scheduled run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not begin the run"})
		return
	}
	if action == "run" {
		c.JSON(http.StatusOK, gin.H{"action": action, "run_id": runID})
		return
	}
	c.JSON(http.StatusOK, gin.H{"action": action})
}

// beginScheduledWorkflowRun inserts a scheduled run, once per Temporal
// workflow, with the workflow row locked so two starts cannot both insert.
func beginScheduledWorkflowRun(ctx context.Context, database *sql.DB, workflowID, temporalID string) (string, string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	var (
		ws, status, kind string
		raw              []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT workspace_id, status, trigger_kind, definition FROM workflows WHERE id = $1 FOR UPDATE
	`, workflowID).Scan(&ws, &status, &kind, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		// The workflow was deleted: its schedule is the only thing left of it.
		dropWorkflowSchedule(ctx, workflowID)
		return "none", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var existing, existingStatus string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM workflow_runs WHERE temporal_workflow_id = $1`,
		temporalID).Scan(&existing, &existingStatus)
	switch {
	case err == nil:
		if existingStatus == "running" {
			return "run", existing, nil
		}
		return "none", "", nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", err
	}
	// Paused, or no longer on a schedule: a schedule a lost delete left behind.
	// Every schedule change is made holding this row's lock (turn on, edit,
	// pause, delete, an automatic pause), so deleting it here cannot undo a
	// turn-on: one made before this lock is seen as active, and one after
	// creates the schedule again.
	if status != "active" || kind != workflow.TriggerSchedule {
		dropWorkflowSchedule(ctx, workflowID)
		return "none", "", nil
	}
	if err := failAbandonedWorkflowRuns(ctx, tx, workflowID); err != nil {
		return "", "", err
	}
	var running bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM workflow_runs WHERE workflow_id = $1 AND status = 'running')
	`, workflowID).Scan(&running); err != nil {
		return "", "", err
	}
	if running {
		return "busy", "", nil
	}
	var def workflow.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		return "", "", fmt.Errorf("read the workflow definition: %w", err)
	}
	// next_run_at is what the workflows page shows; Temporal keeps the time.
	next, nerr := workflow.NextRun(def.Trigger, time.Now())
	if nerr != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflows SET status = 'paused', next_run_at = NULL, paused_reason = $2, updated_at = NOW()
			WHERE id = $1
		`, workflowID, workflow.CapRunes("Its schedule could not be read: "+nerr.Error(), workflowErrorMax)); err != nil {
			return "", "", err
		}
		dropWorkflowSchedule(ctx, workflowID)
		return "none", "", tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflows SET next_run_at = $2 WHERE id = $1`, workflowID, next); err != nil {
		return "", "", err
	}
	var runID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO workflow_runs (workflow_id, workspace_id, trigger, definition, temporal_workflow_id)
		VALUES ($1, $2, 'schedule', $3, $4) RETURNING id
	`, workflowID, ws, string(raw), temporalID).Scan(&runID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return "run", runID, nil
}

// workflowLedgerChannels lists a run's planned sends, in send order.
func workflowLedgerChannels(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}, runID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT channel_id FROM workflow_sends WHERE run_id = $1 ORDER BY position`, runID)
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

// EvaluateWorkflowRunInternal runs a run's checks, query and condition.
// POST /api/v1/internal/workflow-runs/:id/evaluate
//
// outcome "finished": the run is recorded (nothing to send, or it failed);
// "send": channels lists the ledger's channels, in order. A retry after the
// plan was written returns the same plan without running the query again.
func EvaluateWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	finished := func() { c.JSON(http.StatusOK, gin.H{"outcome": "finished"}) }
	send := func(ids []string) { c.JSON(http.StatusOK, gin.H{"outcome": "send", "channels": ids}) }
	internalErr := func(err error, what string) {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] evaluate: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": what})
	}

	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		finished()
		return
	}
	if err != nil {
		// A definition that cannot be read now never will be.
		recorded, rerr := recordWorkflowRun(ctx, database, runID, runOutcome{
			status: "failed", err: "The workflow could not be loaded."})
		if rerr != nil {
			internalErr(rerr, "could not load the run")
			return
		}
		log.WithError(err).WithField("run_id", runID).Error("[workflows] could not load run")
		if recorded {
			notifyUnloadedWorkflowRunFailed(ctx, database, runID)
		}
		finished()
		return
	}
	if run.RunStatus != "running" {
		finished()
		return
	}
	if ids, err := workflowLedgerChannels(ctx, database, runID); err != nil {
		internalErr(err, "could not read the sends")
		return
	} else if len(ids) > 0 {
		send(ids)
		return
	}

	o, plan := evaluateWorkflowRun(ctx, database, run)
	if plan == nil {
		recorded, err := recordWorkflowRun(ctx, database, runID, o)
		if err != nil {
			internalErr(err, "could not record the run")
			return
		}
		if recorded {
			pauseWorkflowForRun(ctx, database, run, o.pause)
			auditWorkflowRun(ctx, database, run, o)
			notifyWorkflowRunFailed(ctx, database, run, o)
			finished()
			return
		}
		// Another attempt got here first.
		if ids, err := workflowLedgerChannels(ctx, database, runID); err != nil {
			internalErr(err, "could not read the sends")
		} else if len(ids) > 0 {
			send(ids)
		} else {
			finished()
		}
		return
	}

	ids, err := planWorkflowSends(ctx, database, run, o, plan)
	switch {
	case errors.Is(err, errWorkflowMessageNotHeld):
		log.WithError(err).WithField("run_id", runID).Warn("[workflows] evaluate: could not hold the message")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the message could not be held for sending"})
	case err != nil:
		internalErr(err, "could not plan the sends")
	case ids == nil:
		finished()
	default:
		send(ids)
	}
}

var errWorkflowMessageNotHeld = errors.New("the message could not be held for sending")

// planWorkflowSends writes a run's sends ledger and what it decided, and holds
// its message for the sends. nil ids: the run is no longer running.
func planWorkflowSends(ctx context.Context, database *sql.DB, run *workflowRunRow, o runOutcome, plan *sendPlan) ([]string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM workflow_runs WHERE id = $1 FOR UPDATE`, run.RunID).
		Scan(&status); err != nil {
		return nil, err
	}
	if status != "running" {
		return nil, nil
	}
	if ids, err := workflowLedgerChannels(ctx, tx, run.RunID); err != nil || len(ids) > 0 {
		return ids, err
	}
	if err := workflowMessages.Put(ctx, run.RunID, plan.msg); err != nil {
		return nil, fmt.Errorf("%w: %v", errWorkflowMessageNotHeld, err)
	}
	ids := make([]string, 0, len(plan.channels))
	for i, ch := range plan.channels {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_sends (run_id, step_id, channel_id, position, name, kind)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING
		`, run.RunID, plan.stepID, ch.ID, i, ch.Name, ch.Kind); err != nil {
			return nil, err
		}
		ids = append(ids, ch.ID)
	}
	notes := o.notes
	if notes == nil {
		notes = []string{}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs SET condition_met = $2, branch = $3, row_count = $4, notes = $5
		WHERE id = $1 AND status = 'running'
	`, run.RunID, o.conditionMet, o.branch, o.rowCount, pgdriver.StringArray(notes)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// SendWorkflowRunInternal sends a run's message to one channel, once.
// POST /api/v1/internal/workflow-runs/:id/send
//
// It serves a run of a temporal-adapter from before step 5; a graph run sends
// from its send step (workflow_graph_runs.go). status is the send's: sent,
// failed or skipped. 409 means another attempt holds the send; ask again.
func SendWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	var req struct {
		ChannelID string `json:"channel_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if _, err := uuid.Parse(req.ChannelID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel_id"})
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	sendWorkflowChannel(c, database, runID, "", req.ChannelID, runID)
}

// sendWorkflowChannel sends one channel of a run's sends ledger, once. stepID
// names the send step; "" is a legacy run's one send step, read from its
// ledger. msgKey is where the message waits: the run's id for a legacy run,
// run/step for a send step. A run that ended or that someone stopped sends
// nothing more.
func sendWorkflowChannel(c *gin.Context, database *sql.DB, runID, stepID, channelID, msgKey string) {
	ctx := c.Request.Context()
	reply := func(status string) { c.JSON(http.StatusOK, gin.H{"status": status}) }
	unavailable := func(err error, what string) {
		log.WithError(err).WithField("run_id", runID).Warn("[workflows] send: " + what)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": what})
	}

	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		reply(deliveryStatusSkipped)
		return
	}
	if err != nil {
		unavailable(err, "could not read the run")
		return
	}
	var (
		status    string
		startedAt sql.NullTime
	)
	err = database.QueryRowContext(ctx, `
		SELECT step_id, status, started_at FROM workflow_sends
		WHERE run_id = $1 AND channel_id = $2 AND ($3::text = '' OR step_id = $3::text)
		ORDER BY position LIMIT 1
	`, runID, channelID, stepID).Scan(&stepID, &status, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "this run does not send to that channel"})
		return
	}
	if err != nil {
		unavailable(err, "could not read the send")
		return
	}
	settle := func(from, to, msg string) {
		var errText interface{} = sql.NullString{}
		if msg != "" {
			errText = workflow.CapRunes(msg, 500)
		}
		if _, err := database.ExecContext(context.WithoutCancel(ctx), `
			UPDATE workflow_sends SET status = $5, error = $6, finished_at = NOW()
			WHERE run_id = $1 AND step_id = $2 AND channel_id = $3 AND status = $4
		`, runID, stepID, channelID, from, to, errText); err != nil {
			log.WithError(err).WithField("run_id", runID).Error("[workflows] send: could not record the send")
		}
	}
	current := func() {
		var s string
		if err := database.QueryRowContext(ctx, `
			SELECT status FROM workflow_sends WHERE run_id = $1 AND step_id = $2 AND channel_id = $3
		`, runID, stepID, channelID).Scan(&s); err != nil {
			unavailable(err, "could not read the send")
			return
		}
		reply(s)
	}

	switch status {
	case deliveryStatusSent, deliveryStatusFailed, deliveryStatusSkipped:
		reply(status)
		return
	case "sending":
		if startedAt.Valid && time.Since(startedAt.Time) < workflowSendStaleAfter {
			c.JSON(http.StatusConflict, gin.H{"error": "this send is in progress"})
			return
		}
		settle("sending", deliveryStatusFailed, workflowSendCutOffError)
		current()
		return
	}
	if run.RunStatus != "running" || run.StopRequested {
		settle("pending", deliveryStatusSkipped, workflowSendSkipError)
		current()
		return
	}

	// Checked again at the send: the run-as user may have lost the role since.
	refusal, err := workflowActorRefusal(ctx, database, run)
	if err != nil {
		unavailable(err, "could not check the run-as user's role")
		return
	}
	if refusal != "" {
		settle("pending", deliveryStatusFailed, refusal)
		pauseWorkflowForRun(ctx, database, run, refusal)
		current()
		return
	}
	msg, held, err := workflowMessages.Get(ctx, msgKey)
	if err != nil {
		unavailable(err, "could not read the message")
		return
	}
	if !held {
		settle("pending", deliveryStatusFailed, workflowMessageGone)
		current()
		return
	}
	ch, err := loadChannelForSend(ctx, database, run.WorkspaceID, channelID)
	if errors.Is(err, sql.ErrNoRows) {
		settle("pending", deliveryStatusFailed, workflowChannelGone)
		current()
		return
	}
	if err != nil {
		unavailable(err, "could not load the channel")
		return
	}

	res, err := database.ExecContext(ctx, `
		UPDATE workflow_sends s SET status = 'sending', started_at = NOW()
		WHERE s.run_id = $1 AND s.step_id = $2 AND s.channel_id = $3 AND s.status = 'pending'
		  AND EXISTS (SELECT 1 FROM workflow_runs r
		              WHERE r.id = s.run_id AND r.status = 'running' AND r.stop_requested_at IS NULL)
	`, runID, stepID, channelID)
	if err != nil {
		unavailable(err, "could not claim the send")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "the send changed under this attempt"})
		return
	}
	// Claimed: from here the post and its record finish even if the caller
	// gives up, so the row never stays 'sending' for a send that went out.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workflowSendTimeout)
	defer cancel()
	d := deliverToChannel(sendCtx, database, ch, msg)
	var errText interface{} = sql.NullString{}
	if d.Error != "" {
		errText = d.Error
	}
	if _, err := database.ExecContext(context.WithoutCancel(ctx), `
		UPDATE workflow_sends SET status = $4, sent = $5, error = $6, finished_at = NOW()
		WHERE run_id = $1 AND step_id = $2 AND channel_id = $3 AND status = 'sending'
	`, runID, stepID, channelID, d.Status, d.Sent, errText); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] send: could not record the send")
	}
	reply(d.Status)
}

// FinishWorkflowRunInternal records a run that planned sends, once.
// POST /api/v1/internal/workflow-runs/:id/finish
//
// abort "" is a run whose sends are done; "deadline" one stopped at its 10
// minutes; "step_failed" one whose step failed for good. Sends not made are
// recorded skipped, and one cut off mid-post failed. A run someone stopped
// (migration 140) is recorded stopped.
func FinishWorkflowRunInternal(c *gin.Context) {
	runID, ok := validRunPathID(c)
	if !ok {
		return
	}
	var req struct {
		Abort string `json:"abort"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	switch req.Abort {
	case "", "deadline", "step_failed":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "abort must be empty, deadline or step_failed"})
		return
	}
	database := workflowRunsDB(c)
	if database == nil {
		return
	}
	ctx := c.Request.Context()
	o, finished, status, err := finishWorkflowRun(ctx, database, runID, req.Abort)
	if err != nil {
		log.WithError(err).WithField("run_id", runID).Error("[workflows] finish: could not record the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record the run"})
		return
	}
	if finished {
		if run, err := loadWorkflowRun(ctx, database, runID); err == nil {
			auditWorkflowRun(ctx, database, run, o)
			notifyWorkflowRunFailed(ctx, database, run, o)
		}
		if err := workflowMessages.Delete(ctx, runID); err != nil {
			log.WithError(err).WithField("run_id", runID).Warn("[workflows] finish: could not delete the message; it expires on its own")
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// finishWorkflowRun closes a run's ledger and records the run from it, with
// the run locked. finished says whether this call recorded it.
func finishWorkflowRun(ctx context.Context, database *sql.DB, runID, abort string) (runOutcome, bool, string, error) {
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
	`, runID).Scan(&status, &rowCount, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return runOutcome{}, false, "", nil
	}
	if err != nil {
		return runOutcome{}, false, "", err
	}
	if status != "running" {
		return runOutcome{}, false, status, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_sends SET status = 'skipped', error = $2, finished_at = NOW()
		WHERE run_id = $1 AND status = 'pending'
	`, runID, workflowSendSkipError); err != nil {
		return runOutcome{}, false, "", err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_sends SET status = 'failed', error = $2, finished_at = NOW()
		WHERE run_id = $1 AND status = 'sending'
	`, runID, workflowSendCutOffError); err != nil {
		return runOutcome{}, false, "", err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT step_id, channel_id, name, kind, status, sent, COALESCE(error, '')
		FROM workflow_sends WHERE run_id = $1 ORDER BY position
	`, runID)
	if err != nil {
		return runOutcome{}, false, "", err
	}
	o := runOutcome{status: "succeeded", deliveries: []DeliveryOutcome{}}
	failedCount := 0
	for rows.Next() {
		var d DeliveryOutcome
		if err := rows.Scan(&d.StepID, &d.ChannelID, &d.Name, &d.Kind, &d.Status, &d.Sent, &d.Error); err != nil {
			rows.Close()
			return runOutcome{}, false, "", err
		}
		if d.Status == deliveryStatusFailed {
			failedCount++
		}
		o.deliveries = append(o.deliveries, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return runOutcome{}, false, "", err
	}
	if rowCount.Valid {
		n := int(rowCount.Int64)
		o.rowCount = &n
	}
	switch {
	case stopped:
		// Someone stopped it: the sends it had not made were skipped above.
		o.status = workflow.StatusStopped
	case abort == "deadline":
		o.status, o.err = "failed", workflowDeadlineError
	case abort == "step_failed":
		o.status, o.err = "failed", workflowStepFailedError
	case failedCount > 0:
		o.status, o.err = "failed", fmt.Sprintf("%d of %d delivery channels failed.", failedCount, len(o.deliveries))
	}
	deliveries, _ := json.Marshal(o.deliveries)
	var errText interface{} = sql.NullString{}
	if o.err != "" {
		errText = o.err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_runs SET status = $2, deliveries = $3, error = $4, finished_at = NOW()
		WHERE id = $1 AND status = 'running'
	`, runID, o.status, string(deliveries), errText)
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
