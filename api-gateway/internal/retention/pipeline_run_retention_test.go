package retention

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the config and the chunk loop. sqlmock matches the SQL text and its
// arguments; it does not execute the keep and age predicates. retention_pg_test.go
// (-tags integration_pg) runs them on a real PostgreSQL.

var runRetentionEnv = []string{
	"ENABLE_PIPELINE_RUN_RETENTION",
	"PIPELINE_RUN_RETENTION_DAYS",
	"PIPELINE_RUN_RETENTION_BATCH_SIZE",
	"PIPELINE_RUN_RETENTION_INTERVAL_HOURS",
	"PIPELINE_RUN_RETENTION_TABLE_STATS",
}

// clearRunRetentionEnv unsets every setting; t.Setenv restores the caller's values.
func clearRunRetentionEnv(t *testing.T) {
	t.Helper()
	for _, k := range runRetentionEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestPipelineRunRetention_OnByDefault(t *testing.T) {
	clearRunRetentionEnv(t)
	cfg := LoadPipelineRunRetentionConfig()
	if !cfg.Enabled || cfg.RetentionDays != 90 || cfg.PruneTableStats ||
		cfg.BatchSize != 10_000 || cfg.Interval != 24*time.Hour {
		t.Fatalf("defaults = %+v, want enabled, 90 days, table stats kept, 10000 rows, 24h", cfg)
	}
}

func TestPipelineRunRetention_Switches(t *testing.T) {
	// The same value goes to both switches: retention defaults to on, the table-stats
	// prune to off, so a value either one rejects reads as its own default.
	cases := []struct {
		val            string
		enabled, stats bool
	}{
		{"false", false, false},
		{"0", false, false},
		{" FALSE ", false, false},
		{"true", true, true},
		{"1", true, true},
		{"", true, false},    // set but empty: the defaults
		{"off", true, false}, // not a bool: the defaults
		{"yes", true, false}, // not a bool: the defaults
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			clearRunRetentionEnv(t)
			t.Setenv("ENABLE_PIPELINE_RUN_RETENTION", c.val)
			t.Setenv("PIPELINE_RUN_RETENTION_TABLE_STATS", c.val)
			cfg := LoadPipelineRunRetentionConfig()
			if cfg.Enabled != c.enabled {
				t.Errorf("ENABLE_PIPELINE_RUN_RETENTION=%q -> enabled=%v, want %v", c.val, cfg.Enabled, c.enabled)
			}
			if cfg.PruneTableStats != c.stats {
				t.Errorf("PIPELINE_RUN_RETENTION_TABLE_STATS=%q -> %v, want %v", c.val, cfg.PruneTableStats, c.stats)
			}
		})
	}
}

func TestPipelineRunRetention_DaysHaveAFloor(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"365", 365},
		{" 30 ", 30},
		{"14", 14},
		{"13", 14}, // under the floor: the floor
		{"1", 14},
		{"0", 90},   // not a positive number: the default
		{"-5", 90},  // not a positive number: the default
		{"90d", 90}, // not a number: the default
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			clearRunRetentionEnv(t)
			t.Setenv("PIPELINE_RUN_RETENTION_DAYS", c.val)
			if got := LoadPipelineRunRetentionConfig().RetentionDays; got != c.want {
				t.Fatalf("PIPELINE_RUN_RETENTION_DAYS=%q -> %d, want %d", c.val, got, c.want)
			}
		})
	}
}

func TestPipelineRunRetention_SizesFromEnv(t *testing.T) {
	clearRunRetentionEnv(t)
	t.Setenv("PIPELINE_RUN_RETENTION_BATCH_SIZE", "500")
	t.Setenv("PIPELINE_RUN_RETENTION_INTERVAL_HOURS", "6")
	cfg := LoadPipelineRunRetentionConfig()
	if cfg.BatchSize != 500 || cfg.Interval != 6*time.Hour {
		t.Fatalf("got batch=%d interval=%s, want 500 and 6h", cfg.BatchSize, cfg.Interval)
	}
	t.Setenv("PIPELINE_RUN_RETENTION_BATCH_SIZE", "lots")
	t.Setenv("PIPELINE_RUN_RETENTION_INTERVAL_HOURS", "0")
	cfg = LoadPipelineRunRetentionConfig()
	if cfg.BatchSize != 10_000 || cfg.Interval != 24*time.Hour {
		t.Fatalf("bad values: got batch=%d interval=%s, want the defaults", cfg.BatchSize, cfg.Interval)
	}
}

func runCfg() PipelineRunRetentionConfig {
	return PipelineRunRetentionConfig{Enabled: true, RetentionDays: 90, BatchSize: 2, Budget: time.Minute}
}

func expectRunPrune(mock sqlmock.Sqlmock, query string, n int64) {
	mock.ExpectExec(regexp.QuoteMeta(query)).
		WithArgs(90, 2).
		WillReturnResult(sqlmock.NewResult(0, n))
}

func newRunRetentionMock(t *testing.T) (sqlmock.Sqlmock, func(PipelineRunRetentionConfig) PipelineRunRetentionResult) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return mock, func(cfg PipelineRunRetentionConfig) PipelineRunRetentionResult {
		return RunPipelineRunRetentionOnce(context.Background(), db, cfg)
	}
}

func TestPipelineRunRetention_ChunksEachTableUntilAShortChunk(t *testing.T) {
	mock, run := newRunRetentionMock(t)
	expectRunPrune(mock, pruneOldBatchRunEventsQuery, 2) // full chunk: go on
	expectRunPrune(mock, pruneOldBatchRunEventsQuery, 1) // short chunk: done
	expectRunPrune(mock, pruneRoutineEventsQuery, 0)
	expectRunPrune(mock, pruneRunMetricsQuery, 2)
	expectRunPrune(mock, pruneRunMetricsQuery, 0)
	// pipeline_run_table_stats is not touched: PruneTableStats is off.

	res := run(runCfg())
	if res.Err != nil || res.RunEvents != 3 || res.RoutineEvents != 0 || res.Metrics != 2 || res.TableStats != 0 {
		t.Fatalf("got %+v, want 3 run events, 0 routine, 2 metrics, no table stats", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRunRetention_TableStatsOnlyWhenAskedFor(t *testing.T) {
	mock, run := newRunRetentionMock(t)
	expectRunPrune(mock, pruneOldBatchRunEventsQuery, 0)
	expectRunPrune(mock, pruneRoutineEventsQuery, 0)
	expectRunPrune(mock, pruneRunMetricsQuery, 0)
	expectRunPrune(mock, pruneBatchTableStatsQuery, 1)

	cfg := runCfg()
	cfg.PruneTableStats = true
	if res := run(cfg); res.Err != nil || res.TableStats != 1 {
		t.Fatalf("got %+v, want 1 table-stats row", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRunRetention_OffOrUnderTheFloorTouchesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*PipelineRunRetentionConfig){
		"disabled":        func(c *PipelineRunRetentionConfig) { c.Enabled = false },
		"under the floor": func(c *PipelineRunRetentionConfig) { c.RetentionDays = 7 },
	} {
		t.Run(name, func(t *testing.T) {
			mock, run := newRunRetentionMock(t)
			cfg := runCfg()
			mutate(&cfg)
			if res := run(cfg); res != (PipelineRunRetentionResult{}) {
				t.Fatalf("did something: %+v", res)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPipelineRunRetention_AnErrorEndsTheTick(t *testing.T) {
	mock, run := newRunRetentionMock(t)
	mock.ExpectExec(regexp.QuoteMeta(pruneOldBatchRunEventsQuery)).
		WithArgs(90, 2).
		WillReturnError(errors.New("boom"))

	if res := run(runCfg()); res.Err == nil {
		t.Fatal("want the error reported")
	}
	// Nothing after the failed step runs.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRunRetention_StopsAtTheBudget(t *testing.T) {
	mock, run := newRunRetentionMock(t)
	expectRunPrune(mock, pruneOldBatchRunEventsQuery, 2) // full chunk, but no time left

	cfg := runCfg()
	cfg.Budget = 0
	res := run(cfg)
	if !res.BudgetExhausted || res.RunEvents != 2 || res.Metrics != 0 {
		t.Fatalf("got %+v, want budget exhausted after one chunk of run events", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
