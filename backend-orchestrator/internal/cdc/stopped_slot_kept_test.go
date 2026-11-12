package cdc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// CDC Stop keeps the replication slot and publication: they are the position
// Start resumes from. Every reaper — the periodic one and the WAL watchdog's
// named one — must therefore select only rows of a DELETED pipeline, and never
// by pipeline status: WAL pressure starts a stopped pipeline, it does not drop
// its slot.
func TestReapableQueriesKeepStoppedPipelines(t *testing.T) {
	for _, rt := range []string{"replication_slot", "publication"} {
		q := reapableResourceQuery(rt)
		if strings.Contains(q, "p.status") {
			t.Errorf("%s: reaper selects on pipeline status, so an existing pipeline can lose its position:\n%s", rt, q)
		}
		if !strings.Contains(q, "cr.pipeline_id IS NULL OR p.id IS NULL") {
			t.Errorf("%s: reaper no longer selects deleted pipelines:\n%s", rt, q)
		}
		if !strings.Contains(q, "cr.resource_type = '"+rt+"'") {
			t.Errorf("%s: wrong resource type:\n%s", rt, q)
		}
	}
}

func TestReapNamedOrphanedSlotsTouchesOnlyNamedSlotsOfDeletedPipelines(t *testing.T) {
	var seen string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		seen = actual
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()
	cols := []string{
		"id", "pipeline_id", "connection_id", "source_table",
		"resource_type", "resource_name", "status",
		"database_type", "metadata", "created_at", "deleted_at", "last_verified_at",
	}
	mock.ExpectQuery(`cr.pipeline_id IS NULL OR p.id IS NULL`).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("r1", nil, "conn1", nil, "replication_slot", "debezium_other_slot", "active",
				"postgresql", []byte(`{}`), time.Unix(1700000000, 0), nil, nil))

	// The only reapable row is not one the watchdog measured as CRITICAL, so
	// nothing is dropped and no connection config is ever read.
	n, err := NewPostgreSQLManager(mockDB).ReapNamedOrphanedSlots(context.Background(), []string{"debezium_critical_slot"})
	if err != nil || n != 0 {
		t.Fatalf("dropped=%d err=%v, want 0 <nil>", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "p.status") {
		t.Errorf("named reap selects on pipeline status:\n%s", seen)
	}

	// No names → no query at all.
	if n, err := NewPostgreSQLManager(mockDB).ReapNamedOrphanedSlots(context.Background(), nil); err != nil || n != 0 {
		t.Fatalf("dropped=%d err=%v", n, err)
	}
}

// The exported getters are what the reapers call, so pin THEIR SQL, not only
// the builder's.
func TestReapableGettersSQL(t *testing.T) {
	var seen string
	matcher := sqlmock.QueryMatcherFunc(func(_, actual string) error { seen = actual; return nil })
	for _, c := range []struct {
		name string
		call func(ctx context.Context, m *PostgreSQLManager) error
	}{
		{"GetReapableSlots", func(ctx context.Context, m *PostgreSQLManager) error {
			_, err := GetReapableSlots(ctx, m.db)
			return err
		}},
		{"GetReapablePublications", func(ctx context.Context, m *PostgreSQLManager) error {
			_, err := GetReapablePublications(ctx, m.db)
			return err
		}},
		{"ReapNamedOrphanedSlots", func(ctx context.Context, m *PostgreSQLManager) error {
			_, err := m.ReapNamedOrphanedSlots(ctx, []string{"debezium_slot"})
			return err
		}},
	} {
		mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectQuery("").WillReturnRows(sqlmock.NewRows([]string{"id"}))
		seen = ""
		_ = c.call(context.Background(), NewPostgreSQLManager(mockDB))
		if seen == "" {
			t.Errorf("%s: ran no query, so this check proved nothing", c.name)
		}
		if strings.Contains(seen, "p.status") {
			t.Errorf("%s: selects on pipeline status:\n%s", c.name, seen)
		}
		mockDB.Close()
	}
}
