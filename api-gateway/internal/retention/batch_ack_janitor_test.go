package retention

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The SQL semantics (what the fold counts, which runs the prune may touch) are
// proven against a real PostgreSQL in internal/db/migration_146_integration_test.go.
// These tests pin the loop: chunking, the keyset resume, the time budget, the
// retention switch and error handling.

func janitorCfg() BatchAckJanitorConfig {
	return BatchAckJanitorConfig{RetentionDays: 30, Chunk: 2, Budget: time.Minute}
}

func expectFold(mock sqlmock.Sqlmock, lastID int64, n, maxID int64) {
	mock.ExpectQuery(regexp.QuoteMeta(foldCDCAcksQuery)).
		WithArgs(2, lastID).
		WillReturnRows(sqlmock.NewRows([]string{"n", "max"}).AddRow(n, maxID))
}

func expectRuns(mock sqlmock.Sqlmock, runs ...[2]string) {
	rows := sqlmock.NewRows([]string{"id", "pipeline_id"})
	for _, r := range runs {
		rows.AddRow(r[0], r[1])
	}
	mock.ExpectQuery(regexp.QuoteMeta(prunableBatchRunsQuery)).
		WithArgs(30, batchAckPruneRunsPerPass).
		WillReturnRows(rows)
}

func expectPrune(mock sqlmock.Sqlmock, run [2]string, n int64) {
	mock.ExpectExec(regexp.QuoteMeta(pruneBatchRunAcksQuery)).
		WithArgs(run[1], run[0], 2).
		WillReturnResult(sqlmock.NewResult(0, n))
}

func newJanitorMock(t *testing.T) (sqlmock.Sqlmock, func() BatchAckJanitorResult, BatchAckJanitorConfig) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := janitorCfg()
	run := func() BatchAckJanitorResult {
		return RunBatchAckJanitorOnce(context.Background(), db, cfg)
	}
	return mock, run, cfg
}

func TestBatchAckJanitor_FoldResumesAfterTheLastIDUntilAShortChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := janitorCfg()
	cfg.RetentionDays = 0

	expectFold(mock, 0, 2, 41)  // full chunk: go on after id 41
	expectFold(mock, 41, 2, 97) // full chunk: go on after id 97
	expectFold(mock, 97, 1, 98) // short chunk: the backlog is done

	res := RunBatchAckJanitorOnce(context.Background(), db, cfg)
	if res.FoldErr != nil || res.FoldedCDCRows != 5 {
		t.Fatalf("folded %d (err %v), want 5", res.FoldedCDCRows, res.FoldErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_RetentionZeroKeepsBatchRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := janitorCfg()
	cfg.RetentionDays = 0

	expectFold(mock, 0, 0, 0)
	// No listing and no delete expected: sqlmock fails an unexpected statement.

	res := RunBatchAckJanitorOnce(context.Background(), db, cfg)
	if res.PrunedRuns != 0 || res.PruneErr != nil {
		t.Fatalf("pruned %d runs (err %v) with retention 0", res.PrunedRuns, res.PruneErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_PrunesEachListedRunInChunks(t *testing.T) {
	mock, run, _ := newJanitorMock(t)
	a := [2]string{"aaaaaaaa-0000-0000-0000-000000000001", "11111111-0000-0000-0000-000000000001"}
	b := [2]string{"bbbbbbbb-0000-0000-0000-000000000002", "22222222-0000-0000-0000-000000000002"}

	expectFold(mock, 0, 0, 0)
	expectRuns(mock, a, b)
	expectPrune(mock, a, 2) // full chunk: the run has more
	expectPrune(mock, a, 1) // short: run a is done
	expectPrune(mock, b, 0) // nothing left to take (another replica took it)

	res := run()
	if res.PruneErr != nil || res.PrunedRuns != 2 || res.PrunedBatchRows != 3 {
		t.Fatalf("got runs=%d rows=%d err=%v, want 2 runs, 3 rows", res.PrunedRuns, res.PrunedBatchRows, res.PruneErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_ListsAgainWhileAPassIsFull(t *testing.T) {
	mock, run, _ := newJanitorMock(t)
	expectFold(mock, 0, 0, 0)

	full := make([][2]string, batchAckPruneRunsPerPass)
	for i := range full {
		full[i] = [2]string{"e", "p"}
	}
	expectRuns(mock, full...)
	for range full {
		expectPrune(mock, [2]string{"e", "p"}, 1)
	}
	expectRuns(mock) // second pass: nothing left

	res := run()
	if res.PruneErr != nil || res.PrunedRuns != batchAckPruneRunsPerPass {
		t.Fatalf("pruned %d runs (err %v), want %d", res.PrunedRuns, res.PruneErr, batchAckPruneRunsPerPass)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_BudgetEndsEachHalfAfterOneChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := janitorCfg()
	cfg.Budget = 0 // spent before the first chunk returns

	run := [2]string{"e", "p"}
	expectFold(mock, 0, 2, 9) // a full chunk, but no time left: stop
	expectRuns(mock, run, run)
	expectPrune(mock, run, 2) // a full chunk, but no time left: stop

	res := RunBatchAckJanitorOnce(context.Background(), db, cfg)
	if !res.BudgetExhausted {
		t.Fatal("BudgetExhausted = false, want true")
	}
	if res.FoldedCDCRows != 2 || res.PrunedBatchRows != 2 || res.PrunedRuns != 0 {
		t.Fatalf("got folded=%d pruned rows=%d runs=%d, want 2, 2, 0", res.FoldedCDCRows, res.PrunedBatchRows, res.PrunedRuns)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_AFoldErrorStillPrunes(t *testing.T) {
	mock, run, _ := newJanitorMock(t)
	r := [2]string{"e", "p"}
	mock.ExpectQuery(regexp.QuoteMeta(foldCDCAcksQuery)).WillReturnError(errors.New(`relation "cdc_counter_totals" does not exist`))
	expectRuns(mock, r)
	expectPrune(mock, r, 1)

	res := run()
	if res.FoldErr == nil {
		t.Fatal("FoldErr = nil, want the fold's error")
	}
	if res.PruneErr != nil || res.PrunedRuns != 1 {
		t.Fatalf("pruned %d runs (err %v), want 1", res.PrunedRuns, res.PruneErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_APruneErrorEndsThePrune(t *testing.T) {
	mock, run, _ := newJanitorMock(t)
	a, b := [2]string{"a", "p"}, [2]string{"b", "p"}
	expectFold(mock, 0, 0, 0)
	expectRuns(mock, a, b)
	mock.ExpectExec(regexp.QuoteMeta(pruneBatchRunAcksQuery)).WillReturnError(errors.New("canceling statement due to lock timeout"))
	// run b is not touched

	res := run()
	if res.PruneErr == nil || res.PrunedRuns != 0 {
		t.Fatalf("got runs=%d err=%v, want 0 runs and the error", res.PrunedRuns, res.PruneErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAckJanitor_ACanceledContextStopsBetweenChunks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := janitorCfg()
	cfg.Pause = time.Hour // the cancel lands in the pause

	ctx, cancel := context.WithCancel(context.Background())
	expectFold(mock, 0, 2, 9)
	time.AfterFunc(20*time.Millisecond, cancel)

	done := make(chan BatchAckJanitorResult, 1)
	go func() { done <- RunBatchAckJanitorOnce(ctx, db, cfg) }()
	select {
	case res := <-done:
		if res.FoldedCDCRows != 2 || res.PrunedRuns != 0 {
			t.Fatalf("got folded=%d runs=%d, want 2 and no prune", res.FoldedCDCRows, res.PrunedRuns)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the janitor did not stop on cancel")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The clauses the fold's correctness rests on. Each is proven on a real database
// by the integration test; this keeps an edit from dropping one silently.
func TestBatchAckJanitor_FoldQueryKeepsItsGuards(t *testing.T) {
	for _, clause := range []string{
		"storage_type = 'cdc'",                 // only CDC rows are folded
		"acked_at < NOW() - INTERVAL '1 hour'", // a row a pre-146 sink may re-write is left
		"FOR UPDATE SKIP LOCKED",               // replicas never fold the same row
		"WHERE last_error IS NULL",             // a negative ack never landed
		"IN ('c', 'r', 'u', 'd')",              // the ops the sink counts
		"total = cdc_counter_totals.total + EXCLUDED.total",
		"GREATEST(cdc_offset_floors.max_offset, EXCLUDED.max_offset)",
		"FROM moved", // counts come from the deleted rows
	} {
		if !strings.Contains(foldCDCAcksQuery, clause) {
			t.Errorf("fold query lost %q", clause)
		}
	}
	for _, clause := range []string{
		"e.id <> e.pipeline_id",                    // never the CDC execution
		"c.position->>'execution_id' = e.id::text", // never a run a checkpoint names
		"a.storage_type <> 'cdc'",
		"'completed'", "'cancelled'", "'failed'", "'credential_check_failed'",
	} {
		if !strings.Contains(prunableBatchRunsQuery, clause) {
			t.Errorf("prune listing lost %q", clause)
		}
	}
	if strings.Contains(prunableBatchRunsQuery, "'running'") {
		t.Error("prune listing must never take a running run")
	}
	if !strings.Contains(pruneBatchRunAcksQuery, "storage_type <> 'cdc'") {
		t.Error("prune delete lost its CDC exclusion")
	}
}

func TestLoadBatchAckJanitorConfig_RetentionDays(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		set         bool
		want        int
	}{
		{"unset", "", false, 30},
		{"zero keeps batch rows", "0", true, 0},
		{"days", "7", true, 7},
		{"spaces", " 14 ", true, 14},
		{"negative keeps the default", "-1", true, 30},
		{"junk keeps the default", "a month", true, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("RSYNC_BATCH_ACK_RETENTION_DAYS", tc.value)
			} else {
				t.Setenv("RSYNC_BATCH_ACK_RETENTION_DAYS", "")
				_ = os.Unsetenv("RSYNC_BATCH_ACK_RETENTION_DAYS")
			}
			cfg := LoadBatchAckJanitorConfig()
			if cfg.RetentionDays != tc.want {
				t.Fatalf("RetentionDays = %d, want %d", cfg.RetentionDays, tc.want)
			}
			if cfg.Chunk != batchAckJanitorChunk || cfg.Interval != batchAckJanitorInterval || cfg.Budget != batchAckJanitorBudget {
				t.Fatalf("defaults changed: %+v", cfg)
			}
		})
	}
}
