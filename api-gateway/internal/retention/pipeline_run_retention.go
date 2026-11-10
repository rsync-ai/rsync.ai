package retention

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Run-history retention. pipeline_run_events (every run's log, plus a metric tick every
// few seconds while a run or a CDC stream is live) and pipeline_run_metrics only ever
// grew. They are now kept PIPELINE_RUN_RETENTION_DAYS (default 90) and trimmed once a
// day; ENABLE_PIPELINE_RUN_RETENTION=false keeps every row.
//
// What goes, and what is kept on purpose:
//
//   - A finished batch run older than the window goes WHOLE: every event of it at once.
//     The Trends page derives a run's start, end and counts from its events
//     (pipeline_compare.go getExecutionSummary), so half a run would read as a
//     different run. The executions row stays, so the run list keeps it.
//   - Each pipeline's newest run, and the run holding its newest PIPELINE_COMPLETED, are
//     never touched. A model's upstream gate reads that completion
//     (saved_query_schedules.go unsatisfiedUpstreams); without it a batch upstream idle
//     for 90 days would read as never completed.
//   - Metric ticks (DATA_PLANE_METRICS, TABLE_STATS, PROGRESS, stage heartbeats) that
//     belong to no batch run — a CDC stream's, or rows with no run — go by age. A CDC
//     pipeline's lifecycle events (stage changes, schema changes, alerts) and the
//     healer's rows are kept: the run flow folds them all (pipeline_flow.go), and they
//     are few.
//   - pipeline_run_metrics goes by age.
//   - pipeline_run_table_stats is kept unless PIPELINE_RUN_RETENTION_TABLE_STATS=true:
//     it holds the usage totals and the lineage run history, and the projector bills a
//     CDC table's committed bytes as the growth over its stored row
//     (projector/event_projector.go oldBytes), so deleting a live CDC row would bill
//     its bytes again. Opted in, only finished batch runs' rows go.
//
// Safe with several api-gateway replicas: every chunk takes its rows FOR UPDATE SKIP
// LOCKED. Deleting rows does not shrink the table's file; it makes the space reusable,
// so the table stops growing.

const (
	defaultPipelineRunRetentionDays = 90
	// minPipelineRunRetentionDays: the projector consumes the run-event topics from
	// their first offset (FirstOffset) and skips an event whose (pipeline_id, event_id)
	// row is already stored (event_projector.go ON CONFLICT DO NOTHING); only a stored
	// PIPELINE_COMPLETED fires the model and workflow hooks. The topics keep 7 days
	// (backend-orchestrator kafka/topology.go), so a row deleted younger than that could
	// be replayed and fire its hooks again. Twice the topic retention leaves margin.
	minPipelineRunRetentionDays = 14
	pipelineRunRetentionChunk   = 10_000
	pipelineRunRetentionEvery   = 24 * time.Hour
	// pipelineRunRetentionBudget bounds one tick, so a first run over months of rows is
	// worked off over several days instead of one long burst of writes.
	pipelineRunRetentionBudget = 5 * time.Minute
	pipelineRunRetentionPause  = 100 * time.Millisecond
)

type PipelineRunRetentionConfig struct {
	Enabled bool
	// RetentionDays keeps a row at least this many days; never below
	// minPipelineRunRetentionDays.
	RetentionDays int
	BatchSize     int
	Interval      time.Duration
	Budget        time.Duration
	Pause         time.Duration
	// PruneTableStats also trims finished batch runs' pipeline_run_table_stats rows.
	PruneTableStats bool
}

// LoadPipelineRunRetentionConfig reads the environment. A value it cannot use logs a
// warning and keeps the default.
func LoadPipelineRunRetentionConfig() PipelineRunRetentionConfig {
	cfg := PipelineRunRetentionConfig{
		Enabled:       true,
		RetentionDays: defaultPipelineRunRetentionDays,
		BatchSize:     pipelineRunRetentionChunk,
		Interval:      pipelineRunRetentionEvery,
		Budget:        pipelineRunRetentionBudget,
		Pause:         pipelineRunRetentionPause,
	}
	cfg.Enabled = envBool("ENABLE_PIPELINE_RUN_RETENTION", cfg.Enabled)
	cfg.PruneTableStats = envBool("PIPELINE_RUN_RETENTION_TABLE_STATS", cfg.PruneTableStats)
	if v := envPositiveInt("PIPELINE_RUN_RETENTION_DAYS", cfg.RetentionDays); v < minPipelineRunRetentionDays {
		log.Warnf("pipeline run retention: PIPELINE_RUN_RETENTION_DAYS=%d is under the %d-day floor (the run-event topics replay 7 days); using %d",
			v, minPipelineRunRetentionDays, minPipelineRunRetentionDays)
		cfg.RetentionDays = minPipelineRunRetentionDays
	} else {
		cfg.RetentionDays = v
	}
	cfg.BatchSize = envPositiveInt("PIPELINE_RUN_RETENTION_BATCH_SIZE", cfg.BatchSize)
	cfg.Interval = time.Duration(envPositiveInt("PIPELINE_RUN_RETENTION_INTERVAL_HOURS", int(cfg.Interval/time.Hour))) * time.Hour
	return cfg
}

var (
	currentRunRetentionOnce sync.Once
	currentRunRetention     PipelineRunRetentionConfig
)

// CurrentPipelineRunRetentionConfig is LoadPipelineRunRetentionConfig read once per
// process, so a request handler can ask without logging the same warning per request.
func CurrentPipelineRunRetentionConfig() PipelineRunRetentionConfig {
	currentRunRetentionOnce.Do(func() { currentRunRetention = LoadPipelineRunRetentionConfig() })
	return currentRunRetention
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		log.Warnf("pipeline run retention: %s=%q is not true or false; using %v", name, v, def)
		return def
	}
	return b
}

func envPositiveInt(name string, def int) int {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		log.Warnf("pipeline run retention: %s=%q is not a positive whole number; using %d", name, v, def)
		return def
	}
	return n
}

func StartPipelineRunRetention(ctx context.Context, database *sql.DB) {
	cfg := CurrentPipelineRunRetentionConfig()
	if !cfg.Enabled {
		log.Info("pipeline run retention: ENABLE_PIPELINE_RUN_RETENTION=false, keeping every run row")
		return
	}
	if database == nil {
		log.Warn("pipeline run retention: DB is nil; skipping")
		return
	}

	go func() {
		RunPipelineRunRetentionOnce(ctx, database, cfg)

		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunPipelineRunRetentionOnce(ctx, database, cfg)
			}
		}
	}()
}

// The SQL fragments below read executions x joined to pipelines p.
const (
	// runIsBatchSQL: x is a run of its own — not the CDC stream's synthetic id (events of
	// a live stream carry execution_id = pipeline_id), not a CDC run, not a CDC
	// pipeline's run. The CDC test is handlers.pipelineRowIsCDCSQL.
	runIsBatchSQL = `x.id <> x.pipeline_id
	  AND x.trigger_source <> 'cdc'
	  AND NOT (LOWER(COALESCE(TRIM(p.sync_mode), '')) = 'cdc' OR (COALESCE(TRIM(p.sync_mode), '') = '' AND p.cdc_mode IS NOT NULL))`

	// runIsPastWindowSQL: x finished ($1 + 1) days ago. The extra day keeps a run whole:
	// an event stored a little after its run ended is still inside $1 days of age.
	// The status list is the executions list's terminal set (handlers/pipelines.go),
	// plus 'stopped'.
	runIsPastWindowSQL = `x.status IN ('completed', 'success', 'cancelled', 'failed', 'error',
	                   'silent_drop_detected', 'silent_partial_drop_detected',
	                   'credential_check_failed', 'stopped')
	  AND COALESCE(x.end_time, x.start_time) < NOW() - make_interval(days => $1 + 1)`

	// runIsKeptSQL: x is its pipeline's newest run, or holds its newest
	// PIPELINE_COMPLETED (idx_pipeline_run_events_completed_latest, migration 155).
	// IS NOT DISTINCT FROM, not =: a pipeline with no completion (or one stamped with no
	// run) makes the subquery NULL, and NOT (... = NULL) would keep every one of its runs.
	runIsKeptSQL = `(x.id IS NOT DISTINCT FROM (SELECT l.id FROM executions l
	           WHERE l.pipeline_id = x.pipeline_id AND l.start_time IS NOT NULL
	           ORDER BY l.start_time DESC LIMIT 1)
	   OR x.id IS NOT DISTINCT FROM (SELECT c.execution_id FROM pipeline_run_events c
	           WHERE c.pipeline_id = x.pipeline_id AND c.event_type = 'PIPELINE_COMPLETED'
	           ORDER BY c.received_at DESC LIMIT 1))`
)

// Every query takes ($1 retention days, $2 chunk size). The age scans start from
// idx_pipeline_run_events_created_at and idx_pipeline_run_metrics_ts (migration 155).
const (
	pruneOldBatchRunEventsQuery = `
WITH del AS (
	SELECT e.id FROM pipeline_run_events e
	JOIN executions x ON x.id = e.execution_id
	JOIN pipelines p ON p.id = x.pipeline_id
	WHERE e.created_at < NOW() - make_interval(days => $1)
	  AND ` + runIsBatchSQL + `
	  AND ` + runIsPastWindowSQL + `
	  AND NOT ` + runIsKeptSQL + `
	LIMIT $2
	FOR UPDATE OF e SKIP LOCKED
)
DELETE FROM pipeline_run_events WHERE id IN (SELECT id FROM del)`

	// The routine set is handlers.routineEventTypes plus PROGRESS (the API-polling
	// worker's per-page tick) and stage heartbeats; a warning or an error is never
	// routine — the same rule as the run log's "hide routine events" filter
	// (handlers/pipeline_events.go ExcludeRoutine).
	pruneRoutineEventsQuery = `
WITH del AS (
	SELECT e.id FROM pipeline_run_events e
	WHERE e.created_at < NOW() - make_interval(days => $1)
	  AND LOWER(COALESCE(e.severity, '')) NOT IN ('warn', 'warning', 'error', 'critical', 'fatal')
	  AND (e.event_type IN ('DATA_PLANE_METRICS', 'TABLE_STATS', 'PROGRESS')
	       OR COALESCE(e.payload->'metadata'->>'heartbeat', '') = 'true')
	  AND NOT EXISTS (
	      SELECT 1 FROM executions x JOIN pipelines p ON p.id = x.pipeline_id
	      WHERE x.id = e.execution_id AND ` + runIsBatchSQL + `)
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
DELETE FROM pipeline_run_events WHERE id IN (SELECT id FROM del)`

	pruneRunMetricsQuery = `
WITH del AS (
	SELECT id FROM pipeline_run_metrics
	WHERE ts < NOW() - make_interval(days => $1)
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
DELETE FROM pipeline_run_metrics WHERE id IN (SELECT id FROM del)`

	// Besides the kept runs above, a pipeline's newest table-stats run that delivered
	// anything stays: the CDC first-data check reads it (handlers/pipeline_runtime.go
	// loadCDCFirstDataWait) and the sticky table selection falls back to the newest
	// table-stats run (handlers/pipelines.go inferSelectedTablesFromLastRun).
	pruneBatchTableStatsQuery = `
WITH del AS (
	SELECT s.id FROM pipeline_run_table_stats s
	JOIN executions x ON x.id = s.execution_id
	JOIN pipelines p ON p.id = x.pipeline_id
	WHERE s.mode = 'batch'
	  AND ` + runIsBatchSQL + `
	  AND ` + runIsPastWindowSQL + `
	  AND NOT ` + runIsKeptSQL + `
	  AND x.id IS DISTINCT FROM (SELECT d.execution_id FROM pipeline_run_table_stats d
	           WHERE d.pipeline_id = s.pipeline_id
	             AND (d.last_applied_ts IS NOT NULL
	                  OR COALESCE(d.inserted_rows, 0) > 0
	                  OR COALESCE(d.applied_total_events, 0) > 0)
	           ORDER BY d.updated_at DESC LIMIT 1)
	LIMIT $2
	FOR UPDATE OF s SKIP LOCKED
)
DELETE FROM pipeline_run_table_stats WHERE id IN (SELECT id FROM del)`
)

// PipelineRunRetentionResult is what one tick did.
type PipelineRunRetentionResult struct {
	RunEvents       int64
	RoutineEvents   int64
	Metrics         int64
	TableStats      int64
	BudgetExhausted bool
	Err             error
}

// RunPipelineRunRetentionOnce trims the run tables within cfg.Budget, one table after
// the other. Best-effort: an error or a spent budget ends the tick, and the next tick
// starts over.
func RunPipelineRunRetentionOnce(ctx context.Context, database *sql.DB, cfg PipelineRunRetentionConfig) PipelineRunRetentionResult {
	var res PipelineRunRetentionResult
	if !cfg.Enabled || cfg.RetentionDays < minPipelineRunRetentionDays || cfg.BatchSize <= 0 {
		return res
	}

	steps := []struct {
		query string
		count *int64
	}{
		{pruneOldBatchRunEventsQuery, &res.RunEvents},
		{pruneRoutineEventsQuery, &res.RoutineEvents},
		{pruneRunMetricsQuery, &res.Metrics},
	}
	if cfg.PruneTableStats {
		steps = append(steps, struct {
			query string
			count *int64
		}{pruneBatchTableStatsQuery, &res.TableStats})
	}

	deadline := time.Now().Add(cfg.Budget)
	for _, s := range steps {
		n, done, err := pruneRunChunks(ctx, database, s.query, cfg, deadline)
		*s.count = n
		if err != nil {
			res.Err = err
			break
		}
		if !done {
			break
		}
	}
	res.BudgetExhausted = !time.Now().Before(deadline)
	if res.Err != nil {
		log.WithError(res.Err).Warn("pipeline run retention: prune stopped")
	}

	if res.RunEvents > 0 || res.RoutineEvents > 0 || res.Metrics > 0 || res.TableStats > 0 || res.BudgetExhausted {
		log.WithFields(log.Fields{
			"deleted_run_events":     res.RunEvents,
			"deleted_routine_events": res.RoutineEvents,
			"deleted_metrics":        res.Metrics,
			"deleted_table_stats":    res.TableStats,
			"retention_days":         cfg.RetentionDays,
			"budget_exhausted":       res.BudgetExhausted,
		}).Info("pipeline run retention tick complete")
	}
	return res
}

// pruneRunChunks runs query in chunks until a short chunk (done) or the budget or
// context runs out.
func pruneRunChunks(ctx context.Context, database *sql.DB, query string, cfg PipelineRunRetentionConfig, deadline time.Time) (int64, bool, error) {
	var deleted int64
	for {
		r, err := database.ExecContext(ctx, query, cfg.RetentionDays, cfg.BatchSize)
		if err != nil {
			return deleted, false, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return deleted, false, err
		}
		deleted += n
		if n < int64(cfg.BatchSize) {
			return deleted, true, nil
		}
		if !pauseUntil(ctx, cfg.Pause, deadline) {
			return deleted, false, nil
		}
	}
}
