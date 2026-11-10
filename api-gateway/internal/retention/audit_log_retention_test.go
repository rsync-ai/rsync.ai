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
// arguments; it does not execute the age predicate. retention_pg_test.go
// (-tags integration_pg) runs it on a real PostgreSQL.

func TestAuditLogRetention_DefaultsToOneYear(t *testing.T) {
	t.Setenv("RSYNC_AUDIT_LOG_RETENTION_DAYS", "") // restores the caller's value afterwards
	_ = os.Unsetenv("RSYNC_AUDIT_LOG_RETENTION_DAYS")
	if got := LoadAuditLogRetentionConfig().RetentionDays; got != 365 {
		t.Fatalf("default retention = %d days, want 365", got)
	}
}

func TestAuditLogRetention_ConfigFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"730", 730},
		{" 30 ", 30},
		{"0", 0},    // keep every row
		{"-1", 365}, // not a number of days: keep the default
		{"1y", 365}, // not a number of days: keep the default
		{"", 365},   // set but empty: keep the default
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("RSYNC_AUDIT_LOG_RETENTION_DAYS", c.val)
			if got := LoadAuditLogRetentionConfig().RetentionDays; got != c.want {
				t.Fatalf("RSYNC_AUDIT_LOG_RETENTION_DAYS=%q -> %d days, want %d", c.val, got, c.want)
			}
		})
	}
}

func auditCfg() AuditLogRetentionConfig {
	return AuditLogRetentionConfig{RetentionDays: 365, Chunk: 2, Budget: time.Minute}
}

func expectAuditPrune(mock sqlmock.Sqlmock, query string, n int64) {
	mock.ExpectExec(regexp.QuoteMeta(query)).
		WithArgs(365, 2).
		WillReturnResult(sqlmock.NewResult(0, n))
}

func TestAuditLogRetention_ChunksBothTablesUntilAShortChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectAuditPrune(mock, pruneAuditLogsQuery, 2) // full chunk: go on
	expectAuditPrune(mock, pruneAuditLogsQuery, 1) // short chunk: audit_logs done
	expectAuditPrune(mock, pruneConnectionAccessLogsQuery, 2)
	expectAuditPrune(mock, pruneConnectionAccessLogsQuery, 0)

	res := RunAuditLogRetentionOnce(context.Background(), db, auditCfg())
	if res.Err != nil || res.AuditRows != 3 || res.AccessRows != 2 {
		t.Fatalf("got audit=%d access=%d err=%v, want 3, 2, nil", res.AuditRows, res.AccessRows, res.Err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogRetention_ZeroDaysTouchesNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := auditCfg()
	cfg.RetentionDays = 0

	res := RunAuditLogRetentionOnce(context.Background(), db, cfg)
	if res.AuditRows != 0 || res.AccessRows != 0 || res.Err != nil {
		t.Fatalf("RetentionDays=0 did something: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogRetention_AnErrorEndsTheTick(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(pruneAuditLogsQuery)).
		WithArgs(365, 2).
		WillReturnError(errors.New("boom"))

	res := RunAuditLogRetentionOnce(context.Background(), db, auditCfg())
	if res.Err == nil {
		t.Fatal("want the error reported")
	}
	// connection_access_logs is not touched after audit_logs failed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogRetention_StopsAtTheBudget(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := auditCfg()
	cfg.Budget = 0 // already spent after the first chunk

	expectAuditPrune(mock, pruneAuditLogsQuery, 2) // full chunk, but no time left

	res := RunAuditLogRetentionOnce(context.Background(), db, cfg)
	if !res.BudgetExhausted || res.AuditRows != 2 || res.AccessRows != 0 {
		t.Fatalf("got %+v, want budget exhausted after one chunk of audit_logs", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
