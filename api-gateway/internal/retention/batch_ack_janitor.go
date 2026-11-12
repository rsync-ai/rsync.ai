package retention

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// The batch-ack janitor (KI-CDC-ACK-LEDGER-UNBOUNDED, migration 146).
//
// pipeline_batch_acks is the sink's delivery ledger. Two kinds of row made it grow
// with every event a pipeline ever carried, and nothing trimmed either: the table is
// cascaded away only when its pipeline is deleted.
//
//   - CDC rows, one per change event. The sink now keeps a running total per
//     (pipeline, execution, table, op) in cdc_counter_totals and the highest landed
//     offset per partition in cdc_offset_floors instead. The fold moves the per-event
//     rows written before that into those two tables and deletes them, in one
//     statement, so the sink's restart seed (totals + unfolded rows) never sees a row
//     on both sides or on neither.
//   - Batch-mode rows of finished runs. Every reader of a batch run's acks reads the
//     run in flight, or a run a resume checkpoint still points at
//     (batch_resume_heal.go). The prune deletes a run's rows once it ended
//     RetentionDays ago, and never a run a checkpoint names.
//
// Always on, in cloud and OSS alike. Safe with several api-gateway replicas: every
// chunk takes its rows FOR UPDATE SKIP LOCKED. Deleting rows does not shrink the
// table's file; it makes the space reusable, so the table stops growing.

const (
	batchAckJanitorInterval = time.Hour
	batchAckJanitorChunk    = 5000
	// batchAckJanitorBudget bounds each half of a tick (fold, then prune), so a large
	// backlog is worked off over several ticks instead of one long burst of writes,
	// and a fold backlog does not starve the prune.
	batchAckJanitorBudget = 5 * time.Minute
	// batchAckJanitorPause is the breather between chunks.
	batchAckJanitorPause = 100 * time.Millisecond
	// defaultBatchAckRetentionDays keeps a finished batch run's acks this long.
	defaultBatchAckRetentionDays = 30
)

type BatchAckJanitorConfig struct {
	// RetentionDays keeps a finished batch run's acks this many days after it
	// ended; 0 keeps them all (the CDC fold still runs).
	RetentionDays int
	Interval      time.Duration
	Chunk         int
	Budget        time.Duration
	Pause         time.Duration
}

func LoadBatchAckJanitorConfig() BatchAckJanitorConfig {
	cfg := BatchAckJanitorConfig{
		RetentionDays: defaultBatchAckRetentionDays,
		Interval:      batchAckJanitorInterval,
		Chunk:         batchAckJanitorChunk,
		Budget:        batchAckJanitorBudget,
		Pause:         batchAckJanitorPause,
	}
	if v, ok := os.LookupEnv("RSYNC_BATCH_ACK_RETENTION_DAYS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.RetentionDays = n
		} else {
			log.Warnf("batch-ack janitor: RSYNC_BATCH_ACK_RETENTION_DAYS=%q is not a whole number of days; keeping %d", v, cfg.RetentionDays)
		}
	}
	return cfg
}

func StartBatchAckJanitor(ctx context.Context, database *sql.DB) {
	if database == nil {
		log.Warn("batch-ack janitor: DB is nil; skipping")
		return
	}
	cfg := LoadBatchAckJanitorConfig()
	go func() {
		RunBatchAckJanitorOnce(ctx, database, cfg)

		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunBatchAckJanitorOnce(ctx, database, cfg)
			}
		}
	}()
}

// BatchAckJanitorResult is what one tick did.
type BatchAckJanitorResult struct {
	FoldedCDCRows   int64
	PrunedRuns      int
	PrunedBatchRows int64
	BudgetExhausted bool
	FoldErr         error
	PruneErr        error
}

// RunBatchAckJanitorOnce folds, then prunes, each within cfg.Budget. Best-effort:
// an error ends that half of the tick, and the next tick starts over.
func RunBatchAckJanitorOnce(ctx context.Context, database *sql.DB, cfg BatchAckJanitorConfig) BatchAckJanitorResult {
	var res BatchAckJanitorResult

	deadline := time.Now().Add(cfg.Budget)
	res.FoldedCDCRows, res.FoldErr = foldCDCAcks(ctx, database, cfg, deadline)
	if res.FoldErr != nil {
		log.WithError(res.FoldErr).Warn("batch-ack janitor: CDC fold stopped (the sink's seed still counts unfolded rows)")
	}
	res.BudgetExhausted = !time.Now().Before(deadline)

	if cfg.RetentionDays > 0 && ctx.Err() == nil {
		deadline = time.Now().Add(cfg.Budget)
		res.PrunedRuns, res.PrunedBatchRows, res.PruneErr = pruneBatchAcks(ctx, database, cfg, deadline)
		if res.PruneErr != nil {
			log.WithError(res.PruneErr).Warn("batch-ack janitor: batch prune stopped")
		}
		res.BudgetExhausted = res.BudgetExhausted || !time.Now().Before(deadline)
	}

	if res.FoldedCDCRows > 0 || res.PrunedBatchRows > 0 || res.BudgetExhausted {
		log.WithFields(log.Fields{
			"folded_cdc_rows":   res.FoldedCDCRows,
			"pruned_batch_runs": res.PrunedRuns,
			"pruned_batch_rows": res.PrunedBatchRows,
			"retention_days":    cfg.RetentionDays,
			"budget_exhausted":  res.BudgetExhausted,
		}).Info("batch-ack janitor tick complete")
	}
	return res
}

// foldCDCAcksQuery moves one chunk of per-event CDC rows into the counter tables.
// Every data-modifying CTE runs exactly once, and the two inserts read the deleted
// rows from moved's RETURNING, so a row is counted iff it is deleted. GROUP BY
// gives each key one row, so ON CONFLICT never meets the same key twice.
//
//   - Counted rows match the sink's seed: no last_error (a negative ack never
//     landed), and op c / r / u / d. The floors take every row with an offset —
//     a failed row was consumed too, and its offset must still read as a replay.
//   - Rows acked in the last hour are left alone. A sink from before migration 146
//     still writing per-event rows can re-write a row after a redelivery; one an
//     hour old was committed in Kafka long before.
//   - id > $2 resumes where the previous chunk stopped, so a tick does not rescan
//     the batch rows and dead tuples below it.
const foldCDCAcksQuery = `
WITH moved AS (
	DELETE FROM pipeline_batch_acks
	WHERE id IN (
		SELECT id FROM pipeline_batch_acks
		WHERE storage_type = 'cdc'
		  AND acked_at < NOW() - INTERVAL '1 hour'
		  AND id > $2
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	)
	RETURNING id, pipeline_id, execution_id, table_name, cdc_op,
	          kafka_topic, kafka_partition, kafka_offset, last_error
),
totals AS (
	INSERT INTO cdc_counter_totals (pipeline_id, execution_id, table_name, op, total)
	SELECT pipeline_id, execution_id, table_name, LOWER(TRIM(COALESCE(cdc_op, ''))), COUNT(*)
	FROM moved
	WHERE last_error IS NULL
	  AND LOWER(TRIM(COALESCE(cdc_op, ''))) IN ('c', 'r', 'u', 'd')
	GROUP BY 1, 2, 3, 4
	ON CONFLICT (pipeline_id, execution_id, table_name, op)
	DO UPDATE SET total = cdc_counter_totals.total + EXCLUDED.total, updated_at = NOW()
	RETURNING 1
),
floors AS (
	INSERT INTO cdc_offset_floors (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition, max_offset)
	SELECT pipeline_id, execution_id, table_name, kafka_topic, kafka_partition, MAX(kafka_offset)
	FROM moved
	WHERE kafka_topic IS NOT NULL AND kafka_partition IS NOT NULL AND kafka_offset IS NOT NULL
	GROUP BY 1, 2, 3, 4, 5
	ON CONFLICT (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition)
	DO UPDATE SET max_offset = GREATEST(cdc_offset_floors.max_offset, EXCLUDED.max_offset), updated_at = NOW()
	RETURNING 1
)
SELECT (SELECT COUNT(*) FROM moved), (SELECT COALESCE(MAX(id), 0) FROM moved)`

func foldCDCAcks(ctx context.Context, database *sql.DB, cfg BatchAckJanitorConfig, deadline time.Time) (int64, error) {
	var folded, lastID int64
	for {
		var n, maxID int64
		if err := database.QueryRowContext(ctx, foldCDCAcksQuery, cfg.Chunk, lastID).Scan(&n, &maxID); err != nil {
			return folded, err
		}
		folded += n
		if n < int64(cfg.Chunk) || !pauseUntil(ctx, cfg.Pause, deadline) {
			return folded, nil
		}
		lastID = maxID
	}
}

// prunableBatchRunsQuery lists finished batch runs that still hold acks, oldest
// first. Finished = a terminal status, the set the pipeline runs list treats as
// terminal (handlers/pipelines.go); a run that never stamped end_time is aged by
// its start. A CDC execution (id = pipeline_id) never ends and is the fold's; a run
// a resume checkpoint names is read by the resume heal however old it is.
const prunableBatchRunsQuery = `
SELECT e.id, e.pipeline_id
FROM executions e
WHERE e.id <> e.pipeline_id
  AND e.status IN ('completed', 'success', 'cancelled', 'failed', 'error',
                   'silent_drop_detected', 'silent_partial_drop_detected',
                   'credential_check_failed')
  AND COALESCE(e.end_time, e.start_time) < NOW() - make_interval(days => $1)
  AND EXISTS (
	SELECT 1 FROM pipeline_batch_acks a
	WHERE a.pipeline_id = e.pipeline_id AND a.execution_id = e.id
	  AND a.storage_type <> 'cdc'
  )
  AND NOT EXISTS (
	SELECT 1 FROM pipeline_checkpoints c
	WHERE c.pipeline_id = e.pipeline_id
	  AND c.position->>'execution_id' = e.id::text
  )
ORDER BY COALESCE(e.end_time, e.start_time)
LIMIT $2`

const pruneBatchRunAcksQuery = `
WITH del AS (
	SELECT id FROM pipeline_batch_acks
	WHERE pipeline_id = $1 AND execution_id = $2 AND storage_type <> 'cdc'
	LIMIT $3
	FOR UPDATE SKIP LOCKED
)
DELETE FROM pipeline_batch_acks WHERE id IN (SELECT id FROM del)`

// batchAckPruneRunsPerPass is how many runs one listing returns.
const batchAckPruneRunsPerPass = 100

func pruneBatchAcks(ctx context.Context, database *sql.DB, cfg BatchAckJanitorConfig, deadline time.Time) (int, int64, error) {
	var runs int
	var deleted int64
	for {
		ids, err := listPrunableBatchRuns(ctx, database, cfg.RetentionDays)
		if err != nil {
			return runs, deleted, err
		}
		for _, r := range ids {
			for {
				res, err := database.ExecContext(ctx, pruneBatchRunAcksQuery, r.pipelineID, r.executionID, cfg.Chunk)
				if err != nil {
					return runs, deleted, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return runs, deleted, err
				}
				deleted += n
				if n < int64(cfg.Chunk) {
					break
				}
				if !pauseUntil(ctx, cfg.Pause, deadline) {
					return runs, deleted, nil
				}
			}
			runs++
			if !pauseUntil(ctx, 0, deadline) {
				return runs, deleted, nil
			}
		}
		if len(ids) < batchAckPruneRunsPerPass {
			return runs, deleted, nil
		}
	}
}

type batchRun struct{ executionID, pipelineID string }

func listPrunableBatchRuns(ctx context.Context, database *sql.DB, retentionDays int) ([]batchRun, error) {
	rows, err := database.QueryContext(ctx, prunableBatchRunsQuery, retentionDays, batchAckPruneRunsPerPass)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchRun
	for rows.Next() {
		var r batchRun
		if err := rows.Scan(&r.executionID, &r.pipelineID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// pauseUntil waits d, and reports whether there is time and context left to go on.
func pauseUntil(ctx context.Context, d time.Duration, deadline time.Time) bool {
	if d > 0 {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
	return ctx.Err() == nil && time.Now().Before(deadline)
}
