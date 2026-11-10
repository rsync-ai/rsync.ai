package main

// Tests for the per-flush CDC tally (cdc_flush_tally.go). The end-to-end counts through
// both flush paths — replays, restarts, per-row recovery — are pinned by
// cdc_counter_ledger_test.go, which ran per row before this and runs per flush now. These
// pin the tally itself: what goes per table, what goes process-wide, and that a flush
// creates no counter it has nothing to add to.

import (
	"sync"
	"testing"
)

func TestTallyLandedCDC_SplitsCountedRowsFromReplays(t *testing.T) {
	row := func(table, op string, id int) *SinkMessage {
		return &SinkMessage{Table: table, CDCOp: op, After: map[string]interface{}{"id": id, "name": "n"}}
	}
	sms := []*SinkMessage{
		row("public.orders", "c", 1),
		row("public.orders", "c", 2),
		row("public.orders", "U", 3), // replay: process-wide only; op case is ignored
		nil,                          // skipped, as the per-row loops skipped it
		row("public.items", "r", 4),
		row("public.items", "d", 5),
		row("public.orders", " u ", 6),
		row("public.orders", "x", 7), // an op the counters do not keep
	}
	counted := []bool{true, true, false, true, true, true, true, true}

	tally := tallyLandedCDC(sms, counted)

	if tally.inserts != 2 || tally.updates != 2 || tally.reads != 1 || tally.deletes != 1 {
		t.Fatalf("process-wide: inserts=%d updates=%d reads=%d deletes=%d, want 2/2/1/1 (replays included)",
			tally.inserts, tally.updates, tally.reads, tally.deletes)
	}
	if len(tally.tables) != 2 || tally.tables[0].table != "public.orders" || tally.tables[1].table != "public.items" {
		t.Fatalf("tables = %+v, want orders then items in first-seen order", tally.tables)
	}
	orders, items := tally.tables[0], tally.tables[1]
	if orders.inserts != 2 || orders.updates != 1 || orders.reads != 0 || orders.deletes != 0 {
		t.Fatalf("orders: %+v, want 2 inserts and 1 update (the replayed update is not counted)", orders)
	}
	if items.reads != 1 || items.deletes != 1 || items.inserts != 0 || items.updates != 0 {
		t.Fatalf("items: %+v, want 1 read and 1 delete", items)
	}

	// Bytes are the counted rows' bytes, the unknown op included (the per-row loops
	// metered every counted row), the replay excluded.
	var wantOrders int64
	for _, i := range []int{0, 1, 6, 7} {
		wantOrders += cdcRowBytes(sms[i])
	}
	if wantOrders == 0 {
		t.Fatal("cdcRowBytes metered nothing; the byte assertion below would be vacuous")
	}
	if orders.bytes != wantOrders {
		t.Fatalf("orders bytes = %d, want %d", orders.bytes, wantOrders)
	}
}

func TestCDCFlushTally_ApplyAddsOncePerTableAndCreatesNoZeroCounter(t *testing.T) {
	m := &Metrics{}
	inserts, updates, deletes, bytes := &sync.Map{}, &sync.Map{}, &sync.Map{}, &sync.Map{}
	incrementCounter(inserts, "public.orders", 10) // what earlier flushes left

	sms := []*SinkMessage{
		{Table: "public.orders", CDCOp: "c", After: map[string]interface{}{"id": 1}},
		{Table: "public.orders", CDCOp: "c", After: map[string]interface{}{"id": 2}},
		{Table: "public.orders", CDCOp: "r", After: map[string]interface{}{"id": 3}},
	}
	tallyLandedCDC(sms, []bool{true, true, true}).apply(m, inserts, updates, deletes, bytes)

	if got := loadCounter(inserts, "public.orders"); got != 12 {
		t.Fatalf("inserts = %d, want 12", got)
	}
	if got := loadCounter(&m.cdcSnapshotRowsByTable, "public.orders"); got != 1 {
		t.Fatalf("snapshot rows = %d, want 1", got)
	}
	if loadCounter(bytes, "public.orders") <= 0 {
		t.Fatal("bytes were not added")
	}
	if m.cdcInserts != 2 || m.cdcReads != 1 || m.cdcUpdates != 0 || m.cdcDeletes != 0 {
		t.Fatalf("metrics: inserts=%d reads=%d updates=%d deletes=%d", m.cdcInserts, m.cdcReads, m.cdcUpdates, m.cdcDeletes)
	}
	// A batch of inserts must not leave a 0 update or delete counter behind: the table
	// view lists every key it finds.
	for name, c := range map[string]*sync.Map{"updates": updates, "deletes": deletes} {
		if _, ok := c.Load("public.orders"); ok {
			t.Errorf("%s gained a zero counter for a table the flush had none of", name)
		}
	}
}
