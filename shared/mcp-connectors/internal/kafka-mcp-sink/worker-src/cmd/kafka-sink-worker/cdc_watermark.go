package main

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The CDC watermark (api-gateway migration 137, cdc_table_watermarks).
//
// A CDC pipeline never completes, so nothing told a model or a workflow how far a
// table had landed. After each counter write — every caller runs it once the
// destination write has landed — the sink upserts, per (pipeline, table), the newest source time
// it landed and stamps landed_at with the database's now(). The api-gateway reads it
// three ways: the trailing-edge sweep (landed_at), an upstream_policy='all' fan-in
// (source_time), and each model run's record of what it was built from.
//
// Only messages that are not a replay (cdcReplay, decided at consume time against
// the stored offset floors) move it. A replay after a restart re-lands rows that
// already landed; counting those would bump landed_at and fire a trailing edge for
// data downstream has already seen.
//
// Best-effort, like the counter write itself: a failed upsert is logged and the flush
// goes on. It is a statement of its own, not part of the counter upsert, so a sink running
// against a database without migration 137 loses the watermark and nothing else.

// cdcWatermark is one table's newest landed source time within a flush.
type cdcWatermark struct {
	pipelineID  string
	table       string // source-side qualified name, as TABLE_STATS reports it
	destination string // destination qualified name, "" when the sink cannot name one
	sourceTSMs  int64  // Debezium source.ts_ms (≤ 0 when unknown)
}

// cdcWatermarkCols is the bind arguments per row of the upsert.
const cdcWatermarkCols = 4

// buildCDCWatermarks reduces the landed messages of a flush to one watermark per
// (pipeline, table), keeping the newest source time. landed[i] says whether sms[i]
// landed and is not a replay; a message without a landed entry is skipped. Rows without a
// pipeline UUID or a table name are skipped too: the upsert casts the id to uuid,
// and one bad row would fail the statement for every table in the flush. Sorted by
// pipeline then table, so two concurrent flushes lock rows in the same order. Pure.
func buildCDCWatermarks(sms []*SinkMessage, landed []bool) []cdcWatermark {
	type key struct{ pipeline, table string }
	byKey := map[key]*cdcWatermark{}
	for i, sm := range sms {
		if sm == nil || i >= len(landed) || !landed[i] {
			continue
		}
		pipelineID := strings.TrimSpace(sm.PipelineID)
		if !looksLikeUUID(pipelineID) {
			continue
		}
		identity, _ := tableIdentityForStats(sm)
		table, _ := identity["qualified_name"].(string)
		if strings.TrimSpace(table) == "" {
			continue
		}
		destination, _ := identity["destination_qualified_name"].(string)
		k := key{pipelineID, table}
		w := byKey[k]
		if w == nil {
			w = &cdcWatermark{pipelineID: pipelineID, table: table}
			byKey[k] = w
		}
		if destination != "" {
			w.destination = destination
		}
		if sm.SourceTS > w.sourceTSMs {
			w.sourceTSMs = sm.SourceTS
		}
	}
	out := make([]cdcWatermark, 0, len(byKey))
	for _, w := range byKey {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pipelineID != out[j].pipelineID {
			return out[i].pipelineID < out[j].pipelineID
		}
		return out[i].table < out[j].table
	})
	return out
}

// buildCDCWatermarkUpsert renders the watermarks as one multi-row upsert. source_time
// only moves forward (GREATEST ignores a NULL), so an older message landing late
// cannot pull it back; landed_at is always the database's now(). Pure.
func buildCDCWatermarkUpsert(ws []cdcWatermark) (string, []interface{}) {
	if len(ws) == 0 {
		return "", nil
	}
	tuples := make([]string, 0, len(ws))
	args := make([]interface{}, 0, len(ws)*cdcWatermarkCols)
	for _, w := range ws {
		b := len(args)
		p := func(n int) string { return "$" + strconv.Itoa(b+n) }
		tuples = append(tuples, "("+p(1)+"::uuid,"+p(2)+",NULLIF("+p(3)+",''),"+p(4)+"::timestamptz,now())")
		sourceTime := sql.NullTime{}
		if w.sourceTSMs > 0 {
			sourceTime = sql.NullTime{Time: time.UnixMilli(w.sourceTSMs).UTC(), Valid: true}
		}
		args = append(args, w.pipelineID, w.table, w.destination, sourceTime)
	}
	query := `INSERT INTO cdc_table_watermarks (pipeline_id, table_name, destination_qualified_name, source_time, landed_at) VALUES ` +
		strings.Join(tuples, ",") +
		` ON CONFLICT (pipeline_id, table_name) DO UPDATE SET` +
		` source_time = GREATEST(cdc_table_watermarks.source_time, EXCLUDED.source_time),` +
		` destination_qualified_name = COALESCE(EXCLUDED.destination_qualified_name, cdc_table_watermarks.destination_qualified_name),` +
		` landed_at = EXCLUDED.landed_at`
	return query, args
}

// recordCDCWatermarks upserts the watermarks of a flush's newly landed messages.
// Non-fatal: an error is logged and dropped.
func recordCDCWatermarks(ctx context.Context, db *sql.DB, sms []*SinkMessage, landed []bool) {
	if db == nil {
		return
	}
	query, args := buildCDCWatermarkUpsert(buildCDCWatermarks(sms, landed))
	if query == "" {
		return
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		logf("warning", "cdc watermark upsert failed (non-fatal; the run-after-pipeline trailing edge and model provenance miss this flush): %v", err)
	}
}
