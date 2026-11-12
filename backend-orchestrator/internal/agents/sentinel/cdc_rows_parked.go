package sentinel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// cdc_rows_parked.go — the alarm for CDC changes that were consumed but never written.
//
// When the destination refuses a change, or the sink cannot convert it, the sink parks it
// in its dead-letter queue, commits its Kafka offset and moves on (per-row isolation,
// kafka-sink-worker main.go flushBatchPerRow). That is the right runtime behaviour -- one
// bad row must not stop the stream -- but it makes the change invisible to every other
// alarm here. Kafka lag reads 0, because the change was consumed. The drain check sees a
// sink committing steadily. The source is current. Everything is green, and the
// destination is missing a row that will never arrive on its own.
//
// The sink does count it: migration 080 added pipeline_run_table_stats.dlq_rows for
// exactly this, and the pipeline's table view shows it. Nothing told anybody. This reads
// that count every source-lag tick and raises a critical issue -- pushed to the owner as
// CDC_ROWS_PARKED -- when it grows.
//
// It alerts on GROWTH, not on a non-zero count, because a parked row stays parked: the
// count is cumulative (migration 134 sums it across sink restarts) and nothing in rsync.ai
// replays the DLQ. Alerting on "> 0" would re-notify every hour (the gateway's dedup
// window) for the life of the pipeline about rows the owner already knows of. So the issue
// row carries the count it last reported (metadata.dlq_rows), the alert fires only when the
// count passes it, and the row stays on the pipeline page for as long as rows are parked --
// it is still true. The reported count is read back from that row after a restart, so a
// redeploy does not re-announce old rows.

// rowsParkedTablesShown is how many tables the alert names; the rest are a count.
const rowsParkedTablesShown = 3

// rowsParkedIssueID keys the parked-rows issue. Its own prefix, so neither lag resolver
// can clear it: a sink that is draining perfectly is exactly the shape this alarm is for.
func rowsParkedIssueID(pipelineID string) string {
	return fmt.Sprintf("cdc-rows-parked-%s", pipelineID)
}

// parkedTable is one table's parked-row count.
type parkedTable struct {
	name string
	rows int64
}

// loadParkedRows returns a CDC pipeline's parked-row count per table, largest first, and
// their total. dlq_rows is per (pipeline, execution, table) and cumulative within each, so
// the per-table SUM over executions is the pipeline's lifetime count. The WHERE matches the
// partial index idx_pipeline_run_table_stats_dlq (migration 080), so a pipeline with nothing
// parked costs an index probe that finds nothing.
func (s *CDCSentinel) loadParkedRows(ctx context.Context, pipelineID string) ([]parkedTable, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT qualified_name, SUM(dlq_rows)::bigint
		FROM pipeline_run_table_stats
		WHERE pipeline_id = $1::uuid AND mode = 'cdc' AND dlq_rows > 0
		GROUP BY qualified_name
		ORDER BY 2 DESC, 1
	`, pipelineID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var tables []parkedTable
	var total int64
	for rows.Next() {
		var t parkedTable
		if err := rows.Scan(&t.name, &t.rows); err != nil {
			return nil, 0, err
		}
		tables = append(tables, t)
		total += t.rows
	}
	return tables, total, rows.Err()
}

// reportedParkedRows is the parked-row count this pipeline's alert last reported: from
// memory once this process has seen the pipeline, else from the issue row a previous
// process left (0 when there is none). ok is false when the row could not be read, and
// the caller then does nothing this tick -- re-announcing every parked row after one
// failed read is the false alarm this watermark exists to prevent.
func (s *CDCSentinel) reportedParkedRows(ctx context.Context, pipelineID string) (int64, bool) {
	s.mu.Lock()
	n, seen := s.rowsParked[pipelineID]
	s.mu.Unlock()
	if seen {
		return n, true
	}

	var stored sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT (metadata->>'dlq_rows')::bigint
		FROM sentinel_active_issues
		WHERE id = $1 AND resolved_at IS NULL
	`, rowsParkedIssueID(pipelineID)).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, true
	case err != nil:
		log.WithError(err).WithField("pipeline_id", pipelineID).Debug("🛡️ rows parked: could not read the last reported count")
		return 0, false
	}
	return stored.Int64, true
}

func (s *CDCSentinel) rememberParkedRows(pipelineID string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rowsParked == nil {
		s.rowsParked = make(map[string]int64)
	}
	s.rowsParked[pipelineID] = n
}

// rowsParkedReport is the description and metadata for the issue: names and counts only,
// never row content. newRows is how many were parked since the last report.
func rowsParkedReport(tables []parkedTable, total, newRows int64) (string, map[string]interface{}) {
	shown := tables
	if len(shown) > rowsParkedTablesShown {
		shown = shown[:rowsParkedTablesShown]
	}
	names := make([]string, 0, len(shown))
	parts := make([]string, 0, len(shown))
	byTable := make(map[string]int64, len(tables))
	for _, t := range tables {
		byTable[t.name] = t.rows
	}
	for _, t := range shown {
		names = append(names, t.name)
		parts = append(parts, fmt.Sprintf("%s (%d)", t.name, t.rows))
	}
	more := len(tables) - len(shown)

	desc := fmt.Sprintf("CDC sink parked %d change(s) in its dead-letter queue instead of writing them", total)
	if newRows > 0 && newRows != total {
		desc += fmt.Sprintf(" (%d new)", newRows)
	}
	if len(parts) > 0 {
		desc += " — " + strings.Join(parts, ", ")
		if more > 0 {
			desc += fmt.Sprintf(" and %d more table(s)", more)
		}
	}
	desc += ". They are NOT in the destination: the sink committed past them, so Kafka lag and the drain check read healthy."

	return desc, map[string]interface{}{
		"dlq_rows":     total,
		"new_dlq_rows": newRows,
		"tables":       names,
		"more_tables":  more,
		"by_table":     byTable,
	}
}

// checkRowsParked raises, refreshes or clears a CDC pipeline's parked-rows issue. Called
// from checkSinkConsumerLag, so it runs for every running CDC pipeline each source-lag tick.
//
//   - more parked than last reported: a critical issue, pushed to the owner, naming the
//     tables and how many are new.
//   - the same count: nothing. The issue row stays; the rows are still missing.
//   - fewer (the counts were reset): the row is rewritten with the new count, silently, so
//     the next real increase is measured from it.
//   - none: the issue is removed.
//
// A failed read changes nothing; the next tick tries again.
func (s *CDCSentinel) checkRowsParked(ctx context.Context, pipelineID, pipelineName, dbType string) {
	if s.db == nil {
		return
	}
	tables, total, err := s.loadParkedRows(ctx, pipelineID)
	if err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Debug("🛡️ rows parked: could not read dlq_rows")
		return
	}
	issueID := rowsParkedIssueID(pipelineID)

	if total == 0 {
		// One DELETE per process, not per tick: after it, memory says 0 until rows park.
		s.mu.Lock()
		n, seen := s.rowsParked[pipelineID]
		s.mu.Unlock()
		if seen && n == 0 {
			return
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sentinel_active_issues WHERE id = $1`, issueID); err != nil {
			log.WithError(err).WithField("pipeline_id", pipelineID).Debug("🛡️ rows parked: could not clear the issue")
			return
		}
		s.rememberParkedRows(pipelineID, 0)
		return
	}

	reported, ok := s.reportedParkedRows(ctx, pipelineID)
	if !ok {
		return
	}

	switch {
	case total > reported:
		desc, meta := rowsParkedReport(tables, total, total-reported)
		s.emitCDCIssue(ctx, issueID, IssueTypeRowsParked, IssueSeverityCritical, "cdc_rows_parked",
			pipelineID, pipelineName, dbType, desc, meta)
		s.rememberParkedRows(pipelineID, total)
	case total < reported:
		desc, meta := rowsParkedReport(tables, total, 0)
		metaJSON, _ := json.Marshal(meta)
		if _, err := s.db.ExecContext(ctx,
			`UPDATE sentinel_active_issues SET description = $2, metadata = $3 WHERE id = $1`,
			issueID, desc, metaJSON); err != nil {
			log.WithError(err).WithField("pipeline_id", pipelineID).Debug("🛡️ rows parked: could not lower the reported count")
			return
		}
		s.rememberParkedRows(pipelineID, total)
	default:
		s.rememberParkedRows(pipelineID, total)
	}
}
