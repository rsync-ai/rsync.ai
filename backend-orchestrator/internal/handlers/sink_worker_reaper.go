package handlers

// sink_worker_reaper.go — stops the kafka-mcp-sink workers nothing else will stop.
//
// Two kinds of worker outlive their purpose:
//
//   - The orphan (KI-CDC-SINK-WORKER-NO-REAPER). A pipeline Stop or Delete whose
//     stop_sink failed reports it and moves on: the row is deleted or flipped to
//     'stopped', and the worker keeps consuming and writing.
//   - The finished batch run's worker. A batch run starts a worker for its consumer
//     group and nothing ever stops it: the run completes and the worker holds a
//     process, a metrics port and a broker session until the sink service restarts.
//
// Every interval the reaper lists the workers the sink service holds (list_sinks) and
// asks the database about each worker's pipeline. It stops a worker only on a POSITIVE
// answer:
//
//   - no row for the pipeline id (a delete is a hard DELETE), or
//   - status 'stopped', last changed more than a grace period ago, or
//   - a batch worker (consumer group "…-batch") that names the run it serves, whose
//     pipeline has read 'completed' or 'failed' for longer than the grace period, with
//     no run of the pipeline in flight.
//
// Everything else keeps the worker: a failed list, a worker with no pipeline UUID, a
// failed query, any other status. A worker must qualify on two ticks in a row, and the
// database is asked again right before the stop, so a pipeline started between two
// looks is never touched. A batch worker's stop also names the run the reaper saw
// (only_if_execution_id): if a newer run reused the worker in between, the sink service
// refuses the stop and the worker is kept. stop_sink keeps the consumer group's
// committed offsets, and nothing here touches a CDC slot, publication or connector: a
// stopped pipeline resumes where it left off.
//
// Once the reaper has stopped the last worker of a DELETED pipeline, it runs that
// pipeline's topic and consumer-group cleanup again (KI-CDC-ORPHAN-TOPICS-RECREATED).
// The delete ran it while the orphan was still reading, and a live reader's metadata
// requests re-create each deleted topic, empty, under the broker's auto-create default.
//
// SINK_WORKER_REAPER_MODE: unset or "enforce" stops workers; "dry_run" logs what it
// would stop; "off" starts nothing.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

type sinkReaperMode int

const (
	sinkReaperOff sinkReaperMode = iota
	sinkReaperDryRun
	sinkReaperEnforce
)

func (m sinkReaperMode) String() string {
	switch m {
	case sinkReaperDryRun:
		return "dry_run"
	case sinkReaperEnforce:
		return "enforce"
	default:
		return "off"
	}
}

// parseSinkReaperMode reads SINK_WORKER_REAPER_MODE. Unset is enforce: without the
// reaper every batch run leaves a worker behind. A value it does not recognise is off,
// with a warning: a typo must not arm a loop that stops workers.
func parseSinkReaperMode(v string) sinkReaperMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "enforce":
		return sinkReaperEnforce
	case "dry_run", "dry-run", "dryrun":
		return sinkReaperDryRun
	case "off", "false", "0", "disabled", "none":
		return sinkReaperOff
	default:
		log.WithField("SINK_WORKER_REAPER_MODE", v).
			Warn("sink worker reaper: unrecognised mode; the reaper is off (use enforce, dry_run or off)")
		return sinkReaperOff
	}
}

const (
	defaultSinkReaperInterval     = 5 * time.Minute
	defaultSinkReaperStoppedGrace = 10 * time.Minute
	// sinkReaperSightings is how many ticks in a row a worker must look orphaned before
	// it is stopped.
	sinkReaperSightings   = 2
	sinkReaperCallTimeout = 45 * time.Second
	// sinkReaperCleanupAttempts bounds how many ticks a deleted pipeline's failing topic
	// cleanup is tried before the reaper gives up on it (with a warning).
	sinkReaperCleanupAttempts = 3
)

// Why the reaper stops a worker.
const (
	sinkReapPipelineDeleted = "pipeline deleted"
	sinkReapPipelineStopped = "pipeline stopped"
	sinkReapBatchFinished   = "batch run finished"
)

// The stop_sink contract with the sink service (kafka-mcp-sink connector.py). The key
// makes a stop conditional on the run that owns the worker; the error starts with the
// refusal text when a different run owns it. TestSinkStopMatchesTheTextItsSourcesWrite
// holds both to the connector's source.
const (
	sinkStopOnlyIfExecutionKey = "only_if_execution_id"
	sinkWorkerNotOwner         = "Worker not owned by that execution"
)

// sinkWorkerListing is one worker the sink service reported.
type sinkWorkerListing struct {
	consumerGroup string
	pipelineID    string
	// executionID is the run the worker serves now; "" from an older sink image.
	executionID string
}

// isBatchSinkGroup: batch runs (and a hybrid CDC pipeline's backfill) read their
// pipeline.* topics in the consumer group sink-<id8>-batch.
func isBatchSinkGroup(group string) bool {
	return strings.HasSuffix(group, "-batch")
}

// listSinkWorkers asks the sink service which workers it holds. ok is false on any
// failure, including an older sink image without list_sinks: no listing, no action.
func listSinkWorkers(ctx context.Context, sinks sinkStopExecutor) (workers []sinkWorkerListing, ok bool) {
	if sinks == nil {
		return nil, false
	}
	resp, err := sinks.ExecuteWithContext(ctx, mcp.ExecuteRequest{
		Connector: "kafka-mcp-sink",
		Operation: "list_sinks",
		Config:    map[string]string{},
		Params:    map[string]interface{}{"config": map[string]interface{}{}},
	})
	if err != nil || resp == nil || !resp.Success || resp.Result == nil {
		return nil, false
	}
	raw, isList := resp.Result["workers"].([]interface{})
	if !isList {
		return nil, false
	}
	for _, item := range raw {
		w, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		group, _ := w["consumer_group"].(string)
		pid, _ := w["pipeline_id"].(string)
		execID, _ := w["execution_id"].(string)
		group, pid = strings.TrimSpace(group), strings.TrimSpace(pid)
		if group == "" {
			continue
		}
		workers = append(workers, sinkWorkerListing{
			consumerGroup: group, pipelineID: pid, executionID: strings.TrimSpace(execID),
		})
	}
	return workers, true
}

// sinkWorkerOwnerGone asks the database whether a worker's purpose is over. It returns
// a reason only on a positive answer; a worker with no pipeline UUID, a failed query or
// any live status returns "".
func sinkWorkerOwnerGone(ctx context.Context, db *sql.DB, w sinkWorkerListing, grace time.Duration) string {
	if db == nil {
		return ""
	}
	if _, err := uuid.Parse(w.pipelineID); err != nil {
		return ""
	}
	var status string
	var settled bool
	err := db.QueryRowContext(ctx,
		`SELECT status, updated_at < NOW() - make_interval(secs => $2) FROM pipelines WHERE id = $1::uuid`,
		w.pipelineID, grace.Seconds()).Scan(&status, &settled)
	status = strings.ToLower(strings.TrimSpace(status))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sinkReapPipelineDeleted
	case err != nil:
		log.WithError(err).WithField("pipeline_id", w.pipelineID).
			Debug("sink worker reaper: pipeline lookup failed; keeping the worker")
		return ""
	case !settled:
		return ""
	case status == "stopped":
		return sinkReapPipelineStopped
	case (status == "completed" || status == "failed") && isBatchSinkGroup(w.consumerGroup) && w.executionID != "":
		// Only with the run named: the stop is then conditional on it, so the sink
		// service refuses it if a newer run reused the worker in between.
		if pipelineRunInFlight(ctx, db, w.pipelineID) {
			return ""
		}
		return sinkReapBatchFinished
	default:
		return ""
	}
}

// pipelineRunInFlight reports whether the pipeline has a run pending or running. A
// failed lookup counts as in flight: it keeps the worker.
func pipelineRunInFlight(ctx context.Context, db *sql.DB, pipelineID string) bool {
	var active bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM executions WHERE pipeline_id = $1::uuid AND status IN ('running', 'pending'))`,
		pipelineID).Scan(&active); err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).
			Debug("sink worker reaper: execution lookup failed; keeping the worker")
		return true
	}
	return active
}

// sinkReapAction is what one tick decided for one worker, returned for tests and logs.
type sinkReapAction struct {
	ConsumerGroup string
	PipelineID    string
	Reason        string
	Stopped       bool   // enforce mode: stop_sink succeeded
	Failure       string // enforce mode: why the stop failed
	// Superseded: the sink service refused the stop because a newer run owns the
	// worker now. The worker is kept; it is not a failure.
	Superseded bool
}

// SinkWorkerReaper stops kafka-mcp-sink workers whose pipeline is gone or whose batch
// run is over.
type SinkWorkerReaper struct {
	db       *sql.DB
	sinks    sinkStopExecutor
	mode     sinkReaperMode
	interval time.Duration
	grace    time.Duration
	// suspects counts consecutive ticks each group has qualified for a stop.
	suspects map[string]int
	// cleanupKafka deletes a pipeline's topics and consumer groups; nil skips it.
	cleanupKafka func(ctx context.Context, pipelineID string) []string
	// pendingCleanup: deleted pipelines whose worker the reaper stopped, waiting for
	// their last reader to go, with the cleanup attempts made so far.
	pendingCleanup map[string]int
}

func newSinkWorkerReaper(db *sql.DB, sinks sinkStopExecutor, mode sinkReaperMode, interval, grace time.Duration) *SinkWorkerReaper {
	return &SinkWorkerReaper{
		db: db, sinks: sinks, mode: mode, interval: interval, grace: grace,
		suspects:       map[string]int{},
		pendingCleanup: map[string]int{},
	}
}

// WithKafkaCleanup has the reaper run fn for a deleted pipeline once it has stopped
// the pipeline's last worker. nil-safe on both sides.
func (r *SinkWorkerReaper) WithKafkaCleanup(fn func(ctx context.Context, pipelineID string) []string) *SinkWorkerReaper {
	if r != nil {
		r.cleanupKafka = fn
	}
	return r
}

// PipelineKafkaCleanup is the topic and consumer-group cleanup a pipeline delete runs,
// for the reaper to run again (WithKafkaCleanup). nil without a topology manager.
func PipelineKafkaCleanup(db *sql.DB, tm *kafka.TopologyManager) func(ctx context.Context, pipelineID string) []string {
	if tm == nil {
		return nil
	}
	return func(ctx context.Context, pipelineID string) []string {
		return cleanupPipelineKafkaResources(ctx, db, tm, pipelineID)
	}
}

func envSeconds(name string, def time.Duration) time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return def
}

// NewSinkWorkerReaperFromEnv returns nil when SINK_WORKER_REAPER_MODE turns it off or
// there is no database or MCP manager to use.
func NewSinkWorkerReaperFromEnv(db *sql.DB, mcpManager *mcp.ServerManager) *SinkWorkerReaper {
	mode := parseSinkReaperMode(os.Getenv("SINK_WORKER_REAPER_MODE"))
	sinks := newSinkStopExecutor(mcpManager)
	if mode == sinkReaperOff || db == nil || sinks == nil {
		return nil
	}
	return newSinkWorkerReaper(db, sinks, mode,
		envSeconds("SINK_WORKER_REAPER_INTERVAL_SECONDS", defaultSinkReaperInterval),
		envSeconds("SINK_WORKER_REAPER_STOPPED_GRACE_SECONDS", defaultSinkReaperStoppedGrace))
}

// Mode names the reaper's mode for the startup log.
func (r *SinkWorkerReaper) Mode() string { return r.mode.String() }

// Run ticks until ctx ends.
func (r *SinkWorkerReaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tickCtx, cancel := context.WithTimeout(ctx, sinkReaperCallTimeout)
			r.Tick(tickCtx)
			cancel()
		}
	}
}

// Tick runs one sweep. A failed listing forgets every suspect, so the two sightings a
// stop needs are always two successful looks in a row.
func (r *SinkWorkerReaper) Tick(ctx context.Context) []sinkReapAction {
	if r == nil || r.mode == sinkReaperOff {
		return nil
	}
	workers, ok := listSinkWorkers(ctx, r.sinks)
	if !ok {
		r.suspects = map[string]int{}
		log.Debug("sink worker reaper: the sink service did not list its workers; nothing done")
		return nil
	}
	next := map[string]int{}
	stoppedNow := map[string]bool{}
	var actions []sinkReapAction
	for _, w := range workers {
		reason := sinkWorkerOwnerGone(ctx, r.db, w, r.grace)
		if reason == "" {
			continue
		}
		seen := r.suspects[w.consumerGroup] + 1
		next[w.consumerGroup] = seen
		fields := log.Fields{
			"consumer_group": w.consumerGroup, "pipeline_id": w.pipelineID, "execution_id": w.executionID,
			"reason": reason, "sightings": seen, "mode": r.mode.String(),
		}
		if seen < sinkReaperSightings {
			log.WithFields(fields).Info("sink worker reaper: worker qualifies for a stop; waiting for a second look")
			continue
		}
		action := sinkReapAction{ConsumerGroup: w.consumerGroup, PipelineID: w.pipelineID, Reason: reason}
		if r.mode == sinkReaperDryRun {
			log.WithFields(fields).Warn("sink worker reaper (dry run): would stop this sink worker")
			actions = append(actions, action)
			continue
		}
		// Ask again right before acting: the pipeline may have been started since.
		if again := sinkWorkerOwnerGone(ctx, r.db, w, r.grace); again == "" {
			delete(next, w.consumerGroup)
			continue
		}
		failure := stopSinkWorkerOwnedBy(ctx, r.sinks, w.consumerGroup, w.executionID)
		switch {
		case failure == "":
			action.Stopped = true
			stoppedNow[w.consumerGroup] = true
			delete(next, w.consumerGroup)
			if reason == sinkReapPipelineDeleted {
				if _, pending := r.pendingCleanup[w.pipelineID]; !pending {
					r.pendingCleanup[w.pipelineID] = 0
				}
			}
			log.WithFields(fields).Warn("sink worker reaper: stopped a sink worker")
		case strings.Contains(failure, sinkWorkerNotOwner):
			// A newer run reused the worker after the reaper looked. Keep it, and start
			// its sightings over.
			action.Superseded = true
			delete(next, w.consumerGroup)
			log.WithFields(fields).WithField("answer", failure).
				Info("sink worker reaper: a newer run owns the worker now; keeping it")
		default:
			action.Failure = failure
			log.WithFields(fields).WithField("failure", failure).
				Warn("sink worker reaper: could not stop the sink worker; will try again next tick")
		}
		actions = append(actions, action)
	}
	r.suspects = next
	r.cleanUpDeletedPipelines(ctx, workers, stoppedNow)
	return actions
}

// cleanUpDeletedPipelines runs the topic and consumer-group cleanup of each deleted
// pipeline whose worker the reaper stopped, once none of its workers is still listed
// (other than those stopped this tick): a reader left up would re-create the topics.
func (r *SinkWorkerReaper) cleanUpDeletedPipelines(ctx context.Context, listed []sinkWorkerListing, stoppedNow map[string]bool) {
	if r.cleanupKafka == nil || len(r.pendingCleanup) == 0 {
		return
	}
	reading := map[string]bool{}
	for _, w := range listed {
		if !stoppedNow[w.consumerGroup] {
			reading[w.pipelineID] = true
		}
	}
	for pid, attempts := range r.pendingCleanup {
		if reading[pid] {
			continue
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultKafkaTeardownBudgets.cleanup)
		errs := r.cleanupKafka(cctx, pid)
		cancel()
		fields := log.Fields{"pipeline_id": pid, "attempt": attempts + 1}
		switch {
		case len(errs) == 0:
			delete(r.pendingCleanup, pid)
			log.WithFields(fields).Info("sink worker reaper: cleaned up the deleted pipeline's topics and consumer groups")
		case attempts+1 >= sinkReaperCleanupAttempts:
			delete(r.pendingCleanup, pid)
			log.WithFields(fields).WithField("errors", errs).
				Warn("sink worker reaper: giving up on the deleted pipeline's topic cleanup; its topics may be left behind")
		default:
			r.pendingCleanup[pid] = attempts + 1
			log.WithFields(fields).WithField("errors", errs).
				Warn("sink worker reaper: the deleted pipeline's topic cleanup failed; will try again next tick")
		}
	}
}
