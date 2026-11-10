package main

// Tests for the per-worker CDC buffer budget (cdc_buffer_budget.go). The estimate is only
// useful if it tracks what a buffered change really costs, so the first test measures
// that: it holds a few thousand parsed changes exactly as the batchers do and compares
// the heap they pin with what bufferedChangeBytes says.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// budgetEnvelope is one Debezium change: an id plus cols columns cycling through string,
// integer, double and boolean, with or without the Connect schema block.
func budgetEnvelope(i, cols, strLen int, withSchema bool, op string) []byte {
	row := map[string]interface{}{"id": i}
	fields := []interface{}{map[string]interface{}{"field": "id", "type": "int64", "optional": false}}
	for c := 0; c < cols; c++ {
		name := fmt.Sprintf("col_%d", c)
		switch c % 4 {
		case 0:
			row[name] = strings.Repeat("x", strLen)
			fields = append(fields, map[string]interface{}{"field": name, "type": "string", "optional": true})
		case 1:
			row[name] = 1000 + i*c
			fields = append(fields, map[string]interface{}{"field": name, "type": "int64", "optional": true})
		case 2:
			row[name] = float64(i) / 3.0
			fields = append(fields, map[string]interface{}{"field": name, "type": "double", "optional": true})
		case 3:
			row[name] = i%2 == 0
			fields = append(fields, map[string]interface{}{"field": name, "type": "boolean", "optional": true})
		}
	}
	payload := map[string]interface{}{
		"op": op, "before": nil, "after": row, "ts_ms": 1700000000000,
		"source": map[string]interface{}{"db": "shop", "schema": "public", "table": "orders", "lsn": 12345 + i,
			"ts_ms": 1700000000000, "snapshot": "false", "connector": "postgresql"},
	}
	if op == "u" {
		payload["before"] = row
	}
	env := payload
	if withSchema {
		env = map[string]interface{}{"schema": map[string]interface{}{"type": "struct", "fields": []interface{}{
			map[string]interface{}{"field": "before", "type": "struct", "fields": fields},
			map[string]interface{}{"field": "after", "type": "struct", "fields": fields},
		}}, "payload": payload}
	}
	b, _ := json.Marshal(env)
	return b
}

func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestBufferedChangeBytes_TracksMeasuredHeap: across narrow and wide rows, short and long
// strings, with and without the Connect schema, for object storage (jsonl and parquet
// bronze events) and for a relational destination (ColumnTypes, no bronze event), the
// estimate stays within [0.7, 1.5] of the heap the held changes really pin. Counting the
// bronze JSON size instead — what the batch's byte cap counts — reads as low as 1/6 of it
// for small-valued rows; this test is what keeps the budget from drifting back there.
func TestBufferedChangeBytes_TracksMeasuredHeap(t *testing.T) {
	type held struct {
		msg kafka.Message
		sm  *SinkMessage
		evt map[string]interface{}
	}
	for _, c := range []struct {
		cols, strLen int
		schema       bool
		op           string
		dest, format string
	}{
		{20, 20, true, "c", "gcs", "jsonl"},
		{20, 20, true, "c", "gcs", "parquet"},
		{20, 20, false, "c", "gcs", "jsonl"},
		{8, 200, true, "c", "aws-s3", "jsonl"},
		{8, 200, true, "u", "aws-s3", "parquet"},
		{60, 10, true, "c", "gcs", "parquet"},
		{4, 4000, true, "c", "gcs", "jsonl"},
		{20, 20, true, "c", "postgresql", ""},
		{8, 200, true, "u", "postgresql", ""},
		{60, 10, true, "c", "mysql", ""},
	} {
		name := fmt.Sprintf("%s/%s/cols=%d/str=%d/schema=%v/op=%s", c.dest, c.format, c.cols, c.strLen, c.schema, c.op)
		t.Run(name, func(t *testing.T) {
			cfg := &WorkerConfig{PipelineID: "p", SinkMode: "cdc", DestinationConnector: c.dest,
				DestinationConfig: map[string]interface{}{"file_format": c.format}}
			const n = 3000
			base := liveHeap()
			changes := make([]held, 0, n)
			var estimate int64
			for i := 0; i < n; i++ {
				msg := kafka.Message{Topic: "rsync.cdc.p.public.orders", Offset: int64(i),
					Key: []byte(fmt.Sprintf(`{"id":%d}`, i)), Value: budgetEnvelope(i, c.cols, c.strLen, c.schema, c.op)}
				sm, err := parseSinkMessage(cfg, msg)
				if err != nil {
					t.Fatal(err)
				}
				var evt map[string]interface{}
				if c.format != "" {
					if evt, _, err = buildBronzeCDCEvent(cfg, msg, sm, "upsert", c.format); err != nil {
						t.Fatal(err)
					}
				}
				changes = append(changes, held{msg, sm, evt})
				estimate += bufferedChangeBytes(msg, sm, evt)
			}
			actual := liveHeap() - base
			runtime.KeepAlive(changes)
			if actual <= 0 {
				t.Fatalf("measured no heap for %d held changes; the measurement is broken", n)
			}
			ratio := float64(estimate) / float64(actual)
			t.Logf("estimate %d B/change, measured %d B/change, ratio %.2f", estimate/n, actual/n, ratio)
			if ratio < 0.7 || ratio > 1.5 {
				t.Errorf("estimate/measured = %.2f, want within [0.7, 1.5]", ratio)
			}
		})
	}
}

// A map the change holds twice — After is also Data[0], and a jsonl bronze event's
// "after" — is counted once. Counting it three times would trip the budget at a third
// of what it allows.
func TestBufferedChangeBytes_CountsASharedMapOnce(t *testing.T) {
	row := map[string]interface{}{"id": int64(1), "name": strings.Repeat("n", 100)}
	alone := bufferedChangeBytes(kafka.Message{}, &SinkMessage{After: row}, nil)
	shared := bufferedChangeBytes(kafka.Message{}, &SinkMessage{After: row, Data: []map[string]interface{}{row}},
		map[string]interface{}{"after": row})
	// Data adds its one-element slice and the event its own map, not the row again.
	if extra := shared - alone; extra >= alone-perChangeOverheadBytes {
		t.Fatalf("sharing the row added %d bytes, as much as the row itself (%d): it was counted twice",
			extra, alone-perChangeOverheadBytes)
	}
}

func TestBufferBudget_NilIsNoBudget(t *testing.T) {
	var b *bufferBudget
	b.charge(1 << 40)
	if b.over() || b.inUse() != 0 {
		t.Fatal("a nil budget must never be over")
	}
	b.refund(1)
	b.noteEarlyFlush("t", 1)
	if newBufferBudget(0) != nil || newBufferBudget(-1) != nil {
		t.Fatal("a zero or negative limit must mean no budget")
	}
}

func TestBufferBudget_OverOnlyPastTheLimit(t *testing.T) {
	b := newBufferBudget(100)
	b.charge(100)
	if b.over() {
		t.Fatal("exactly at the limit is not over")
	}
	b.charge(1)
	if !b.over() {
		t.Fatal("past the limit must be over")
	}
	b.refund(1)
	if b.over() || b.inUse() != 100 {
		t.Fatalf("after the refund: over=%v inUse=%d", b.over(), b.inUse())
	}
}

func TestResolveBufferBudgetBytes(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int64
	}{
		{"", 128 << 20},
		{"256", 256 << 20},
		{"0", 0}, // the kill switch
		{" 64 ", 64 << 20},
		{"-5", 128 << 20},
		{"lots", 128 << 20},
		{"99999999", 128 << 20},
	} {
		t.Setenv(EnvSinkBufferBudgetMB, c.raw)
		if got := resolveBufferBudgetBytes(); got != c.want {
			t.Errorf("%s=%q: got %d, want %d", EnvSinkBufferBudgetMB, c.raw, got, c.want)
		}
	}
}

// budgetChange is one small CDC insert for shop.<table>, on that table's own topic.
func budgetChange(table string, offset int64) (kafka.Message, *SinkMessage) {
	return kafka.Message{Topic: "cdc.shop." + table, Offset: offset},
		&SinkMessage{PipelineID: ackFKPipelineID, Table: "shop." + table, CDCOp: "c", SourceTS: 1_700_000_000_000,
			KeyFields: []string{"id"}, PK: map[string]interface{}{"id": offset},
			After: map[string]interface{}{"id": offset, "name": "a name"}}
}

// feedChange adds one budgetChange to either batcher.
func feedChange(ctx context.Context, b any, table string, offset int64) {
	msg, sm := budgetChange(table, offset)
	switch b := b.(type) {
	case *cdcObjectBatcher:
		b.add(ctx, msg, sm)
	case *cdcDBBatcher:
		b.add(ctx, msg, sm)
	default:
		panic(fmt.Sprintf("feedChange: unsupported batcher %T", b))
	}
}

// A worker past its buffer budget writes its LARGEST open batch early — not the batch the
// change went to, not every batch — and what it held is refunded once the write lands.
// Before the budget nothing did this: each table's batch waited for its own cap or its
// interval, and the per-batch caps multiplied by the number of tables.
func TestObjectBatcher_OverBudgetWritesTheLargestBatchEarly(t *testing.T) {
	dest := &recordingDest{}
	b, hw := objectBatcherFor(t, map[string]interface{}{"bucket": "b", "file_format": "jsonl"}, nil, dest)
	dest.hw = hw
	b.budget = newBufferBudget(1 << 40) // measure first, then tighten
	ctx := t.Context()
	for i := int64(0); i < 3; i++ {
		feedChange(ctx, b, "orders", i)
	}
	feedChange(ctx, b, "items", 0)
	if got := dest.imports(); len(got) != 0 {
		t.Fatalf("wrote %v under a budget nothing is near", got)
	}
	var items, orders *cdcObjectBatch
	for _, batch := range b.batches {
		switch batch.table {
		case "shop.items":
			items = batch
		case "shop.orders":
			orders = batch
		}
	}
	if items == nil || orders == nil || b.budget.inUse() != items.heapBytes+orders.heapBytes {
		t.Fatalf("the budget holds %d, the open batches %d + %d", b.budget.inUse(), items.heapBytes, orders.heapBytes)
	}

	// One more orders change takes the worker past its budget.
	b.budget.limit = b.budget.inUse() + items.heapBytes/2
	feedChange(ctx, b, "orders", 3)

	got := dest.imports()
	if len(got) != 1 || !strings.Contains(got[0], "orders") {
		t.Fatalf("expected the orders batch (the largest) written early, got %v", got)
	}
	if !hw.seen("cdc.shop.orders", 0, 3) {
		t.Fatal("the early write did not mark its offsets written")
	}
	if len(b.batches) != 1 || onlyBatch(t, b) != items {
		t.Fatal("the smaller items batch must stay open; only the largest is written")
	}
	if b.budget.inUse() != items.heapBytes {
		t.Fatalf("after the write the budget holds %d, want the items batch's %d", b.budget.inUse(), items.heapBytes)
	}
	if b.budget.lastLog.IsZero() {
		t.Fatal("the early write was not reported")
	}

	b.drainForShutdown(ctx, time.Now())
	if b.budget.inUse() != 0 {
		t.Fatalf("every batch is written and the budget still holds %d bytes", b.budget.inUse())
	}
}

// The relational batcher writes on its flush lanes, so the early write is only
// submitted on the consume goroutine; it must also wait for it, or the next fetch runs
// while the memory is still held — which is the whole of what the budget is for.
func TestDBBatcher_OverBudgetSubmitsTheLargestBatchAndWaitsForIt(t *testing.T) {
	captureFailClosed(t)
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{"127.0.0.1:1"}, Topic: "cdc.shop.orders"})
	defer reader.Close()
	lanes := newFlushLanes(2)
	defer lanes.close()
	c := newCDCTestCounters()
	b := &cdcDBBatcher{
		cfg:          &WorkerConfig{PipelineID: ackFKPipelineID, SinkMode: "cdc"},
		destType:     "mongodb",
		destCfg:      map[string]interface{}{},
		params:       cdcBatchingParams{maxEvents: 2000, maxBytes: 24 << 20, flushInterval: time.Hour, backoff: time.Millisecond},
		gate:         newCommitGate(reader.CommitMessages, newHighWaterTracker()),
		httpClient:   &http.Client{Transport: dbDestination{}},
		ddl:          &DDLSupport{},
		eventsWriter: &kafka.Writer{},
		metrics:      c.metrics,
		cdcInserts:   c.inserts,
		cdcUpdates:   c.updates,
		cdcDeletes:   c.deletes,
		cdcBytes:     &sync.Map{},
		batches:      map[string]*cdcDBBatch{},
		lanes:        lanes,
		budget:       newBufferBudget(1 << 40),
	}
	ctx := t.Context()
	for i := int64(0); i < 3; i++ {
		feedChange(ctx, b, "orders", i)
	}
	feedChange(ctx, b, "items", 0)
	var items *cdcDBBatch
	for _, batch := range b.batches {
		if batch.table == "shop.items" {
			items = batch
		}
	}
	if len(b.batches) != 2 || items == nil {
		t.Fatalf("expected an orders and an items batch open, have %d", len(b.batches))
	}

	b.budget.limit = b.budget.inUse() + items.heapBytes/2
	feedChange(ctx, b, "orders", 3)

	// add has returned: the write must already have landed, not merely be queued.
	if got := loadCounter(c.inserts, "shop.orders"); got != 4 {
		t.Fatalf("orders inserts = %d when add returned, want 4: the early write was not waited for", got)
	}
	if got := loadCounter(c.inserts, "shop.items"); got != 0 {
		t.Fatalf("items inserts = %d, want 0: only the largest batch is written early", got)
	}
	if len(b.batches) != 1 || b.batches[b.batchKey(budgetChange("items", 0))] != items {
		t.Fatal("the smaller items batch must stay open")
	}
	if b.budget.inUse() != items.heapBytes {
		t.Fatalf("after the write the budget holds %d, want the items batch's %d", b.budget.inUse(), items.heapBytes)
	}
}

// Object-storage CDC files roll on bytes and time, with no row cap: 32 MB or 240 s by
// default. A setting still wins — cdc_batching over the default, the destination's
// max_file_* over both — and RSYNC_SINK_BUFFER_BUDGET_MB=0 turns the budget off.
func TestNewCDCObjectBatcher_FileDefaults(t *testing.T) {
	cases := []struct {
		name     string
		destCfg  map[string]interface{}
		batching *CDCBatchingConfig
		events   int
		bytes    int
		interval time.Duration
	}{
		{"defaults: no row cap, 32 MB, 240 s", map[string]interface{}{"bucket": "b"}, nil, 0, 32 << 20, 240 * time.Second},
		{"cdc_batching wins over the defaults", map[string]interface{}{"bucket": "b"},
			&CDCBatchingConfig{MaxEvents: 500, MaxBytes: 1 << 20, FlushIntervalSeconds: 10}, 500, 1 << 20, 10 * time.Second},
		{"max_file_* win over cdc_batching", map[string]interface{}{"bucket": "b", "max_file_rows": float64(50), "max_file_mb": float64(8)},
			&CDCBatchingConfig{MaxEvents: 500, MaxBytes: 1 << 20}, 50, 8 << 20, 240 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := objectBatcherFor(t, tc.destCfg, tc.batching, nil)
			if b.params.maxEvents != tc.events || b.params.maxBytes != tc.bytes || b.params.flushInterval != tc.interval {
				t.Fatalf("rows=%d bytes=%d interval=%v, want %d/%d/%v",
					b.params.maxEvents, b.params.maxBytes, b.params.flushInterval, tc.events, tc.bytes, tc.interval)
			}
			if b.budget == nil || b.budget.limit != defaultSinkBufferBudgetMB<<20 {
				t.Fatalf("budget = %+v, want the %d MiB default", b.budget, defaultSinkBufferBudgetMB)
			}
		})
	}

	t.Setenv(EnvSinkBufferBudgetMB, "0")
	if b, _ := objectBatcherFor(t, map[string]interface{}{"bucket": "b"}, nil, nil); b.budget != nil {
		t.Fatal("RSYNC_SINK_BUFFER_BUDGET_MB=0 must turn the budget off")
	}
}

// With no row cap, the 2000-row shared default no longer cuts an object file: 2500
// small changes stay one open batch until bytes or time decide.
func TestObjectBatcher_NoRowCapByDefault(t *testing.T) {
	dest := &recordingDest{}
	b, hw := objectBatcherFor(t, map[string]interface{}{"bucket": "b", "file_format": "jsonl"}, nil, dest)
	dest.hw = hw
	for i := int64(0); i < 2500; i++ {
		feedChange(t.Context(), b, "orders", i)
	}
	if got := dest.imports(); len(got) != 0 {
		t.Fatalf("2500 small changes were cut into %d file(s); only bytes or time should cut one", len(got))
	}
	if n := len(onlyBatch(t, b).events); n != 2500 {
		t.Fatalf("open batch holds %d changes, want 2500", n)
	}
}
