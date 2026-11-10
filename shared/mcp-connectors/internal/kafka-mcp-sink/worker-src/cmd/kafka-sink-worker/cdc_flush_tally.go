package main

import (
	"strings"
	"sync"
	"sync/atomic"
)

// cdc_flush_tally.go — a landed CDC batch's counts, summed per flush rather than per row.
//
// Both CDC flush paths used to walk the batch and add 1 to a shared counter for every row:
// a compare-and-swap loop on a sync.Map key per row, per counter, plus an atomic add on the
// process-wide metric. The relational batcher runs those flushes on several lanes at once,
// so a snapshot of a few million rows was a few million contended CAS loops on the same few
// keys. The totals are the same if each flush adds its sums once, and that is all this does:
// the replay rule (a row consumed at or below its offset floor feeds the process-wide
// metrics but not the per-table counters) and the bytes figure (cdcRowBytes per counted
// row) are unchanged.

// cdcTableTally is one table's share of a landed batch. Only counted rows — not replays —
// are in it.
type cdcTableTally struct {
	table                                   string
	inserts, updates, deletes, reads, bytes int64
}

// cdcFlushTally is a landed batch's counts. tables has one entry for every batch this
// worker builds today — every batcher key carries its table — but a batch that held two
// would still be counted per table, in first-seen order.
type cdcFlushTally struct {
	tables []cdcTableTally
	// All rows, replays included, for the process-wide metrics.
	inserts, updates, deletes, reads uint64
}

// tallyLandedCDC sums sms by op. counted[i] is persistCDCCounts' verdict for sms[i]: false
// for a replay, which the per-table counters have already seen. An op the counters do not
// keep (a schema change, an unknown op) is skipped, as the per-row loops skipped it.
func tallyLandedCDC(sms []*SinkMessage, counted []bool) cdcFlushTally {
	var t cdcFlushTally
	var cur *cdcTableTally
	for i, sm := range sms {
		if sm == nil {
			continue
		}
		isCounted := i < len(counted) && counted[i]
		if isCounted && (cur == nil || cur.table != sm.Table) {
			cur = nil
			for j := range t.tables {
				if t.tables[j].table == sm.Table {
					cur = &t.tables[j]
					break
				}
			}
			if cur == nil {
				t.tables = append(t.tables, cdcTableTally{table: sm.Table})
				cur = &t.tables[len(t.tables)-1]
			}
		}
		switch strings.ToLower(strings.TrimSpace(sm.CDCOp)) {
		case "c":
			t.inserts++
			if isCounted {
				cur.inserts++
			}
		case "r":
			t.reads++
			if isCounted {
				cur.reads++
			}
		case "u":
			t.updates++
			if isCounted {
				cur.updates++
			}
		case "d":
			t.deletes++
			if isCounted {
				cur.deletes++
			}
		}
		if isCounted {
			cur.bytes += cdcRowBytes(sm)
		}
	}
	return t
}

// apply adds the tally to the per-table counters and the process-wide metrics: one add per
// table per counter, and one per metric.
func (t cdcFlushTally) apply(m *Metrics, inserts, updates, deletes, bytes *sync.Map) {
	for _, tt := range t.tables {
		addCounter(inserts, tt.table, tt.inserts)
		addCounter(updates, tt.table, tt.updates)
		addCounter(deletes, tt.table, tt.deletes)
		addCounter(&m.cdcSnapshotRowsByTable, tt.table, tt.reads)
		addCounter(bytes, tt.table, tt.bytes)
	}
	addMetric(&m.cdcInserts, t.inserts)
	addMetric(&m.cdcUpdates, t.updates)
	addMetric(&m.cdcDeletes, t.deletes)
	addMetric(&m.cdcReads, t.reads)
}

// addCounter is incrementCounter that skips a zero delta, so a batch of inserts does not
// create an update counter for its table that a per-row loop never would have.
func addCounter(m *sync.Map, table string, delta int64) {
	if m != nil && delta != 0 {
		incrementCounter(m, table, delta)
	}
}

func addMetric(p *uint64, n uint64) {
	if n != 0 {
		atomic.AddUint64(p, n)
	}
}
