package main

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// Restart-safe per-table CDC counters, without a row per event.
//
// The sink reports a CDC table's inserts/updates/deletes as running totals kept in
// process memory, and the api-gateway projector stores them with GREATEST(stored,
// reported). So a restart has to start the totals where the last process stopped,
// and a replay (a restart before the Kafka commit) must not count the same offsets
// twice.
//
// Both used to come from pipeline_batch_acks: one row per CDC event, a lifetime
// COUNT(*) at start, and ON CONFLICT DO NOTHING to spot a replay. Nothing trimmed
// those rows, so the table grew with every change event (KI-CDC-ACK-LEDGER-UNBOUNDED).
// Two small tables now hold the same two facts:
//
//   - cdc_counter_totals: one row per (pipeline, execution, table, op) with the
//     running total. The startup seed reads it.
//   - cdc_offset_floors: one row per (pipeline, execution, table, topic, partition)
//     with the highest offset that landed. A message at or below its floor was
//     counted before.
//
// The replay test runs when a message is consumed, in Kafka order (markCDCReplay),
// not at flush. The object-storage batcher can flush a partition's batches out of
// order (its batch key carries partition and time path segments), and a test made
// at flush would take the earlier batch for a replay of the later one.
//
// Per-event rows written before this change still count: the seed adds their
// totals and max offsets until the api-gateway's ack janitor folds them into the two
// tables. The fold deletes and upserts in one statement, so a row counts in one
// place only.
//
// Not covered, as before: the bytes counter (nothing persists bytes), and the
// increments of a write that still failed after its retries. Those count live, but
// a restart cannot recover them. One edge is new. Suppose a later batch of a
// partition writes its floor and the process crashes before an earlier, out-of-order
// batch of the same partition writes its counts. When the earlier batch is
// redelivered, its offsets are below the floor, so its rows are not counted.

// cdcSeedTimeout bounds the startup counter read so a slow database delays the
// worker by at most this long; on timeout the counters start at zero as before.
const cdcSeedTimeout = 30 * time.Second

// cdcCountsAttempts bounds the tries of one flush's counter write. A deadlock or
// a dropped connection is retried; a longer outage leaves the increments live-only.
const cdcCountsAttempts = 3

// cdcStatsExecutionID is the execution_id CDC messages carry, and so the one their
// counters and TABLE_STATS are keyed by. In CDC mode, or when the worker has no
// execution id, it is the pipeline id: a stable bucket for a stream that never
// ends. parseCDCMessage and the startup seed both use it so they cannot disagree.
func cdcStatsExecutionID(cfg *WorkerConfig) string {
	pipelineID := strings.TrimSpace(cfg.PipelineID)
	executionID := strings.TrimSpace(cfg.ExecutionID)
	if strings.EqualFold(strings.TrimSpace(cfg.SinkMode), "cdc") || executionID == "" {
		return pipelineID
	}
	return executionID
}

// cdcFloorKey is one offset space within a worker's pipeline and execution.
type cdcFloorKey struct {
	table     string
	topic     string
	partition int64
}

// cdcFloorTracker holds, per offset space, the highest offset this process has
// claimed. It is seeded from cdc_offset_floors at start, so an offset an earlier
// process landed reads as a replay.
type cdcFloorTracker struct {
	mu     sync.Mutex
	floors map[cdcFloorKey]int64
}

func newCDCFloorTracker() *cdcFloorTracker {
	return &cdcFloorTracker{floors: map[cdcFloorKey]int64{}}
}

// raise lifts k's floor to offset; a lower offset leaves it as it is.
func (f *cdcFloorTracker) raise(k cdcFloorKey, offset int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.floors[k]; !ok || offset > cur {
		f.floors[k] = offset
	}
}

// claim reports whether offset lies above its space's floor, and if so raises the
// floor to it. A nil tracker claims everything.
func (f *cdcFloorTracker) claim(table, topic string, partition int, offset int64) bool {
	if f == nil {
		return true
	}
	k := cdcFloorKey{table: table, topic: topic, partition: int64(partition)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.floors[k]; ok && offset <= cur {
		return false
	}
	f.floors[k] = offset
	return true
}

// markCDCReplay decides, once per consumed CDC message and in consume order,
// whether landing it adds to the counters.
func markCDCReplay(f *cdcFloorTracker, sm *SinkMessage, msg kafka.Message) {
	if sm == nil {
		return
	}
	sm.cdcReplay = !f.claim(sm.Table, msg.Topic, msg.Partition, msg.Offset)
}

// cdcCountsFor reports, per message, whether it feeds the counters: every message
// except a replay.
func cdcCountsFor(sms []*SinkMessage) []bool {
	counted := make([]bool, len(sms))
	for i, sm := range sms {
		counted[i] = sm != nil && !sm.cdcReplay
	}
	return counted
}

// persistCDCCounts records a landed CDC flush: the counted messages' totals and
// every landed message's offset floor, in one statement. It returns which messages
// feed the counters, whatever the write does: the replay decision was made at
// consume time, so a failed write cannot change it. Non-fatal; the caller logs the
// error.
func persistCDCCounts(ctx context.Context, db *sql.DB, sms []*SinkMessage, messages []kafka.Message) ([]bool, error) {
	counted := cdcCountsFor(sms)
	if db == nil || len(sms) == 0 {
		return counted, nil
	}
	// Keep the synthetic executions row CDC's execution_id == pipeline_id needs
	// (transform logs and run events reference it). Once per flush, as before.
	if sms[0] != nil {
		ensureExecutionRowForCDCAudit(ctx, db, sms[0].PipelineID, sms[0].ExecutionID)
	}
	return counted, writeCDCCounts(ctx, db, sms, messages, counted)
}

// persistCDCCount is persistCDCCounts for one message. The single-event lanes can
// run before any flush (append-only mode never batches), so they anchor the
// executions row once per process instead of once per call.
func persistCDCCount(ctx context.Context, db *sql.DB, sm *SinkMessage, msg kafka.Message) (bool, error) {
	counted := cdcCountsFor([]*SinkMessage{sm})
	if db == nil || sm == nil {
		return counted[0], nil
	}
	ensureExecutionRowForCDCAuditOnce(ctx, db, sm.PipelineID, sm.ExecutionID)
	return counted[0], writeCDCCounts(ctx, db, []*SinkMessage{sm}, []kafka.Message{msg}, counted)
}

// writeCDCCounts runs the counter upsert, retrying a failure, then upserts the
// watermarks of the counted messages.
func writeCDCCounts(ctx context.Context, db *sql.DB, sms []*SinkMessage, messages []kafka.Message, counted []bool) error {
	var err error
	if q, args, ok := buildCDCCountsUpsert(sms, messages, counted); ok {
		for attempt := 1; ; attempt++ {
			if _, err = db.ExecContext(ctx, q, args...); err == nil {
				break
			}
			if attempt >= cdcCountsAttempts || isUndefinedTable(err) ||
				!ctxAwareSleep(ctx, time.Duration(attempt)*100*time.Millisecond) {
				break
			}
		}
	}
	recordCDCWatermarks(ctx, db, sms, counted)
	return err
}

// cdcTotalKey and cdcFloorRowKey identify one cdc_counter_totals and one
// cdc_offset_floors row.
type cdcTotalKey struct {
	pipelineID, executionID, table, op string
}

type cdcFloorRowKey struct {
	pipelineID, executionID string
	cdcFloorKey
}

// cdcCountedOp maps a CDC op to the one the counters keep, or "" for an op they
// do not count.
func cdcCountedOp(op string) string {
	switch op = strings.ToLower(strings.TrimSpace(op)); op {
	case "c", "r", "u", "d":
		return op
	}
	return ""
}

// buildCDCCountsUpsert renders a landed flush into one statement: the counted
// messages' totals added to cdc_counter_totals, and each offset space's highest
// landed offset raised in cdc_offset_floors. Rows are sorted so two lanes writing
// the same rows lock them in the same order. ok is false when there is nothing to
// write. Pure, for testability.
func buildCDCCountsUpsert(sms []*SinkMessage, messages []kafka.Message, counted []bool) (string, []interface{}, bool) {
	totals := map[cdcTotalKey]int64{}
	floors := map[cdcFloorRowKey]int64{}
	for i, sm := range sms {
		if sm == nil || i >= len(messages) {
			continue
		}
		pid, eid := strings.TrimSpace(sm.PipelineID), strings.TrimSpace(sm.ExecutionID)
		if !looksLikeUUID(pid) || !looksLikeUUID(eid) {
			continue
		}
		m := messages[i]
		fk := cdcFloorRowKey{pid, eid, cdcFloorKey{table: sm.Table, topic: m.Topic, partition: int64(m.Partition)}}
		if cur, ok := floors[fk]; !ok || m.Offset > cur {
			floors[fk] = m.Offset
		}
		if i < len(counted) && counted[i] {
			if op := cdcCountedOp(sm.CDCOp); op != "" {
				totals[cdcTotalKey{pid, eid, sm.Table, op}]++
			}
		}
	}
	if len(floors) == 0 {
		return "", nil, false
	}

	tkeys := make([]cdcTotalKey, 0, len(totals))
	for k := range totals {
		tkeys = append(tkeys, k)
	}
	sort.Slice(tkeys, func(i, j int) bool {
		a, b := tkeys[i], tkeys[j]
		if a.pipelineID != b.pipelineID {
			return a.pipelineID < b.pipelineID
		}
		if a.executionID != b.executionID {
			return a.executionID < b.executionID
		}
		if a.table != b.table {
			return a.table < b.table
		}
		return a.op < b.op
	})
	fkeys := make([]cdcFloorRowKey, 0, len(floors))
	for k := range floors {
		fkeys = append(fkeys, k)
	}
	sort.Slice(fkeys, func(i, j int) bool {
		a, b := fkeys[i], fkeys[j]
		if a.pipelineID != b.pipelineID {
			return a.pipelineID < b.pipelineID
		}
		if a.executionID != b.executionID {
			return a.executionID < b.executionID
		}
		if a.table != b.table {
			return a.table < b.table
		}
		if a.topic != b.topic {
			return a.topic < b.topic
		}
		return a.partition < b.partition
	})

	var args []interface{}
	tuple := func(vals ...interface{}) string {
		ph := make([]string, len(vals))
		for i, v := range vals {
			args = append(args, v)
			ph[i] = "$" + strconv.Itoa(len(args))
		}
		return "(" + strings.Join(ph, ",") + ")"
	}
	var b strings.Builder
	if len(tkeys) > 0 {
		b.WriteString("WITH totals AS (INSERT INTO cdc_counter_totals (pipeline_id, execution_id, table_name, op, total) VALUES ")
		for i, k := range tkeys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(tuple(k.pipelineID, k.executionID, k.table, k.op, totals[k]))
		}
		b.WriteString(" ON CONFLICT (pipeline_id, execution_id, table_name, op)" +
			" DO UPDATE SET total = cdc_counter_totals.total + EXCLUDED.total, updated_at = NOW()) ")
	}
	b.WriteString("INSERT INTO cdc_offset_floors (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition, max_offset) VALUES ")
	for i, k := range fkeys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(tuple(k.pipelineID, k.executionID, k.table, k.topic, k.partition, floors[k]))
	}
	b.WriteString(" ON CONFLICT (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition)" +
		" DO UPDATE SET max_offset = GREATEST(cdc_offset_floors.max_offset, EXCLUDED.max_offset), updated_at = NOW()")
	return b.String(), args, true
}

// isUndefinedTable reports a PostgreSQL 42P01: the counter tables are not there
// yet because the api-gateway has not run its migration.
func isUndefinedTable(err error) bool {
	var st interface{ SQLState() string }
	return errors.As(err, &st) && st.SQLState() == "42P01"
}

// The seed reads the counter tables plus any per-event rows the janitor has not
// folded yet. Each query is one statement, so it sees one snapshot: a fold that
// commits mid-seed moves a row from one side to the other, never into both or
// neither. Negative acks (last_error set) never landed, so they are excluded.
const (
	cdcCounterSeedQuery = `
	SELECT table_name, op, SUM(n)::bigint FROM (
		SELECT table_name, op, total AS n
		FROM cdc_counter_totals
		WHERE pipeline_id = $1 AND execution_id = $2
		UNION ALL
		SELECT table_name, LOWER(TRIM(COALESCE(cdc_op, ''))), COUNT(*)
		FROM pipeline_batch_acks
		WHERE pipeline_id = $1 AND execution_id = $2
		  AND storage_type = 'cdc' AND last_error IS NULL
		GROUP BY 1, 2
	) s GROUP BY 1, 2`

	cdcFloorSeedQuery = `
	SELECT table_name, kafka_topic, kafka_partition, MAX(o) FROM (
		SELECT table_name, kafka_topic, kafka_partition, max_offset AS o
		FROM cdc_offset_floors
		WHERE pipeline_id = $1 AND execution_id = $2
		UNION ALL
		SELECT table_name, kafka_topic, kafka_partition, MAX(kafka_offset)
		FROM pipeline_batch_acks
		WHERE pipeline_id = $1 AND execution_id = $2
		  AND storage_type = 'cdc' AND kafka_topic IS NOT NULL
		  AND kafka_partition IS NOT NULL AND kafka_offset IS NOT NULL
		GROUP BY 1, 2, 3
	) s GROUP BY 1, 2, 3`

	// The legacy-only reads, for a database the migration has not reached yet.
	cdcLegacyCounterSeedQuery = `
	SELECT table_name, LOWER(TRIM(COALESCE(cdc_op, ''))), COUNT(*)
	FROM pipeline_batch_acks
	WHERE pipeline_id = $1 AND execution_id = $2
	  AND storage_type = 'cdc' AND last_error IS NULL
	GROUP BY 1, 2`

	cdcLegacyFloorSeedQuery = `
	SELECT table_name, kafka_topic, kafka_partition, MAX(kafka_offset)
	FROM pipeline_batch_acks
	WHERE pipeline_id = $1 AND execution_id = $2
	  AND storage_type = 'cdc' AND kafka_topic IS NOT NULL
	  AND kafka_partition IS NOT NULL AND kafka_offset IS NOT NULL
	GROUP BY 1, 2, 3`
)

// queryCDCSeed runs q, or legacy when q's tables do not exist yet.
func queryCDCSeed(ctx context.Context, db *sql.DB, q, legacy string, args ...interface{}) (*sql.Rows, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil && isUndefinedTable(err) {
		return db.QueryContext(ctx, legacy, args...)
	}
	return rows, err
}

// seedCDCCounters loads the stored per-table totals into the empty counters (c into
// inserts, r — snapshot reads — into snapshotRows, u into updates, d into deletes)
// and the stored offset floors into floors. Best-effort: on any error it logs and
// leaves that half as it was.
func seedCDCCounters(ctx context.Context, db *sql.DB, pipelineID, executionID string, floors *cdcFloorTracker, inserts, updates, deletes, snapshotRows *sync.Map) {
	pipelineID = strings.TrimSpace(pipelineID)
	executionID = strings.TrimSpace(executionID)
	if db == nil || !looksLikeUUID(pipelineID) || !looksLikeUUID(executionID) {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, cdcSeedTimeout)
	defer cancel()
	seedCDCTotals(qctx, db, pipelineID, executionID, inserts, updates, deletes, snapshotRows)
	if floors != nil {
		seedCDCFloors(qctx, db, pipelineID, executionID, floors)
	}
}

func seedCDCTotals(ctx context.Context, db *sql.DB, pipelineID, executionID string, inserts, updates, deletes, snapshotRows *sync.Map) {
	rows, err := queryCDCSeed(ctx, db, cdcCounterSeedQuery, cdcLegacyCounterSeedQuery, pipelineID, executionID)
	if err != nil {
		logf("warning", "cdc counter seed skipped, counts start at zero (read failed: %v)", err)
		return
	}
	defer rows.Close()

	type seed struct {
		m     *sync.Map
		table string
		n     int64
	}
	var seeds []seed
	for rows.Next() {
		var table, op string
		var n int64
		if err := rows.Scan(&table, &op, &n); err != nil {
			logf("warning", "cdc counter seed skipped, counts start at zero (row unreadable: %v)", err)
			return
		}
		var m *sync.Map
		switch op {
		case "c":
			m = inserts
		case "r":
			m = snapshotRows
		case "u":
			m = updates
		case "d":
			m = deletes
		default:
			continue
		}
		seeds = append(seeds, seed{m: m, table: table, n: n})
	}
	if err := rows.Err(); err != nil {
		logf("warning", "cdc counter seed skipped, counts start at zero (read failed: %v)", err)
		return
	}
	// Apply only after the whole result was read, so a read that fails halfway
	// cannot leave some tables seeded and others not.
	tables := map[string]struct{}{}
	for _, s := range seeds {
		incrementCounter(s.m, s.table, s.n)
		tables[s.table] = struct{}{}
	}
	logEvent("info", "seeded cdc table counters", "tables", len(tables))
}

func seedCDCFloors(ctx context.Context, db *sql.DB, pipelineID, executionID string, floors *cdcFloorTracker) {
	rows, err := queryCDCSeed(ctx, db, cdcFloorSeedQuery, cdcLegacyFloorSeedQuery, pipelineID, executionID)
	if err != nil {
		logf("warning", "cdc offset floor seed skipped, a replay after this restart may be counted again (read failed: %v)", err)
		return
	}
	defer rows.Close()
	seeded := map[cdcFloorKey]int64{}
	for rows.Next() {
		var k cdcFloorKey
		var offset int64
		if err := rows.Scan(&k.table, &k.topic, &k.partition, &offset); err != nil {
			logf("warning", "cdc offset floor seed skipped, a replay after this restart may be counted again (row unreadable: %v)", err)
			return
		}
		seeded[k] = offset
	}
	if err := rows.Err(); err != nil {
		logf("warning", "cdc offset floor seed skipped, a replay after this restart may be counted again (read failed: %v)", err)
		return
	}
	for k, offset := range seeded {
		floors.raise(k, offset)
	}
	logEvent("info", "seeded cdc offset floors", "partitions", len(seeded))
}
