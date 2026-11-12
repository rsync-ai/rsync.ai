package main

import "testing"

// A batch run to GCS wrote one Parquet object per table on prod (2026-09-30), yet
// pipeline_run_table_stats.files_written read 0 on every run. The projector stores
// metadata.counts.files_written for a batch TABLE_STATS event, but no producer had
// sent it since the executor's own emitter was removed: the bug class is a counter
// the projector reads that nothing writes. The sink is the hop that knows how many
// objects landed, so its event must carry the count.

func TestBatchTableStatsCarryTheFilesWrittenToObjectStorage(t *testing.T) {
	sm := &SinkMessage{PipelineID: "p", ExecutionID: "e", Table: "datingapp.matches"}
	counts := buildTableStatsEvent(sm, "batch", "completed", 4000, 4000, 44727, 1)["metadata"].(map[string]interface{})["counts"].(map[string]interface{})
	if got, ok := counts["files_written"]; !ok || got != int64(1) {
		t.Fatalf("counts.files_written = %v (present=%v), want 1", got, ok)
	}
}

func TestBatchTableStatsOmitFilesWrittenForADestinationThatWritesNoFiles(t *testing.T) {
	sm := &SinkMessage{PipelineID: "p", ExecutionID: "e", Table: "public.users"}
	counts := buildTableStatsEvent(sm, "batch", "completed", 5, 5, 80, noFileCount)["metadata"].(map[string]interface{})["counts"].(map[string]interface{})
	if got, ok := counts["files_written"]; ok {
		t.Fatalf("counts.files_written = %v for a table destination, want it absent", got)
	}
}

// The count is the distinct objects written: a Resume that rewrites a landed
// object under the same key (recordWrites keeps one entry) is one file, not two.
func TestObjectFileCountCountsEachObjectOnce(t *testing.T) {
	st := &tableWriteState{}
	st.recordWrites([]string{"t/part-000000.parquet", "t/part-010000.parquet"}, []int64{10000, 2000})
	st.recordWrites([]string{"t/part-010000.parquet"}, []int64{2000})
	if got := objectFileCount(true, st.keys); got != 2 {
		t.Fatalf("objectFileCount = %d, want 2", got)
	}
	if got := objectFileCount(false, st.keys); got != noFileCount {
		t.Fatalf("objectFileCount for a non-object destination = %d, want noFileCount", got)
	}
}

// A batch table read "completed" on the run page while it was still copying: every
// per-flush "running" event carried completed_at, and since a flush has read ==
// written the projector's recovery branch took that marker as the end of the table.
// The bug class is a terminal marker on a non-terminal event, so only the statuses
// the sink emits at EOF may carry completed_at.
func TestBatchTableStatsCarryCompletedAtOnlyWhenTheTableIsDone(t *testing.T) {
	sm := &SinkMessage{PipelineID: "p", ExecutionID: "e", Table: "public.users"}
	for status, want := range map[string]bool{"running": false, "completed": true, "degraded": true, "failed": true} {
		meta := buildTableStatsEvent(sm, "batch", status, 500, 500, 8000, noFileCount)["metadata"].(map[string]interface{})
		if _, got := meta["completed_at"]; got != want {
			t.Errorf("status %q: completed_at present = %v, want %v", status, got, want)
		}
		if _, ok := meta["started_at"]; !ok {
			t.Errorf("status %q: started_at missing", status)
		}
	}
}
