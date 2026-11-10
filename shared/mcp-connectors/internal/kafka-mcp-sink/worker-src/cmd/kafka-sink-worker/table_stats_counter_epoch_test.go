package main

// counts.bytes_committed and counts.dlq_rows are running totals this process keeps in
// memory: a restart reports them from zero. The api-gateway projector charged only the
// amount above the stored total, so after a restart it charged nothing until the new
// process passed the old total (0.04 GB of data transfer on prod, 2026-10-03, while a
// CDC pipeline streamed). Both TABLE_STATS builders now stamp counter_epoch_ms, the
// time this process started those totals, and the projector adds a new epoch's totals
// to the stored ones (api-gateway table_stats_counter_epoch_test.go).

import (
	"encoding/json"
	"testing"
	"time"
)

func statsEpoch(t *testing.T, ev map[string]interface{}) float64 {
	t.Helper()
	// Through JSON, as the projector reads it: a value past 2^53 would lose digits.
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Metadata map[string]interface{} `json:"metadata"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := back.Metadata["counter_epoch_ms"].(float64)
	if !ok {
		t.Fatalf("metadata.counter_epoch_ms missing: %#v", back.Metadata)
	}
	return v
}

func TestTableStatsCarryTheProcessCounterEpoch(t *testing.T) {
	sm := &SinkMessage{PipelineID: "p1", ExecutionID: "e1", Table: "public.orders"}
	cdc := statsEpoch(t, buildCDCTableStatsEvent(sm, 1, 0, 0, 100, 0, 0))
	batch := statsEpoch(t, buildTableStatsEvent(sm, "batch", "running", 1, 1, 100, noFileCount))

	if cdc != batch {
		t.Fatalf("CDC epoch %v != batch epoch %v; one process has one epoch", cdc, batch)
	}
	if cdc != float64(processCounterEpochMs) {
		t.Fatalf("epoch %v != processCounterEpochMs %d", cdc, processCounterEpochMs)
	}
	// Milliseconds, so it survives the projector's float64 decode and a later process
	// gets a larger value.
	now := time.Now().UnixMilli()
	if int64(cdc) <= 0 || int64(cdc) > now || now-int64(cdc) > int64(time.Hour/time.Millisecond) {
		t.Fatalf("epoch %v is not this process's start time in ms (now %d)", cdc, now)
	}
}

// The epoch is constant across a process's events: a new value on each event would
// read as a restart every time and add every running total again.
func TestTableStatsCounterEpochIsStable(t *testing.T) {
	sm := &SinkMessage{PipelineID: "p1", ExecutionID: "e1", Table: "public.orders"}
	first := statsEpoch(t, buildCDCTableStatsEvent(sm, 1, 0, 0, 100, 0, 0))
	time.Sleep(2 * time.Millisecond)
	second := statsEpoch(t, buildCDCTableStatsEvent(sm, 2, 0, 0, 200, 0, 0))
	if first != second {
		t.Fatalf("epoch changed between events of one process: %v → %v", first, second)
	}
}
