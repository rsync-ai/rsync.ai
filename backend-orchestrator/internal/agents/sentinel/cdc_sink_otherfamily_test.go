package sentinel

// Coverage for the per-source sink-drain gap. checkSourceDBLag first reached the
// family-agnostic sink-drain check (checkSinkConsumerLag → maybeAutoRestartSink) only from its
// PostgreSQL branch (via cdc_resources) and its MySQL branch (via connections); #687 added a
// third pass for a LIST of families (oracle/sqlserver/mongodb). A list still missed sources:
// MariaDB matches none of the branches, and a Postgres pipeline whose slot row is missing or
// not 'active' falls through both. Every one of them drains through the SAME shared sink
// worker, so each could stop reaching the destination with no alarm. The last pass is now a
// catch-all over every running CDC pipeline; these tests lock that, and that no pipeline is
// checked twice in one tick.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// countingMatcher is sqlmock's regexp matcher plus a count of the queries that read parked
// rows. checkRowsParked issues exactly one such read at the top of every sink check, so the
// count is the number of sink checks the tick ran.
type countingMatcher struct{ parkedReads int }

func (m *countingMatcher) Match(expectedSQL, actualSQL string) error {
	if strings.Contains(actualSQL, "FROM pipeline_run_table_stats") {
		m.parkedReads++
	}
	return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
}

// TestCheckSourceDBLag_ChecksEveryRunningCDCPipelineOnce: the catch-all must reach the
// pipelines no source-lag branch found (a MariaDB source, a pipeline whose source connection
// row is gone), and must not check again the ones a branch already did -- including a
// Postgres pipeline that has two slot rows. Red before the fix two ways: the family-list
// query never returns maria-1 or orphan-1, and without the checked set pg-1 is read three
// times (two slot rows + the catch-all).
//
// kafkaManager and mcpManager are nil, so each sink check is just its parked-rows read; that
// read returns an error here so the check stops there (no DELETE/INSERT to script).
func TestCheckSourceDBLag_ChecksEveryRunningCDCPipelineOnce(t *testing.T) {
	matcher := &countingMatcher{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	readFailed := errors.New("parked rows unreadable in this test")

	// 1) PostgreSQL slot discovery — pg-1 has TWO active slot rows. checkPGSlotLag's own
	//    connection lookup is not scripted, fails, and returns before touching a source.
	mock.ExpectQuery(`JOIN cdc_resources cr`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connection_id", "resource_name"}).
			AddRow("pg-1", "pg-cdc", "conn-pg", "slot_a").
			AddRow("pg-1", "pg-cdc", "conn-pg", "slot_b"))
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs("pg-1").WillReturnError(readFailed)

	// 2) MySQL connection discovery — none (its probe would need Kafka Connect).
	mock.ExpectQuery(`ILIKE 'mysql%'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "source_connection_id"}))

	// 3) The catch-all — every running CDC pipeline, pg-1 included.
	mock.ExpectQuery(`LEFT JOIN connections c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_type"}).
			AddRow("pg-1", "pg-cdc", "postgresql").
			AddRow("maria-1", "maria-cdc", "mariadb").
			AddRow("orphan-1", "orphan-cdc", ""))
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs("maria-1").WillReturnError(readFailed)
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs("orphan-1").WillReturnError(readFailed)

	s := &CDCSentinel{db: db}
	s.checkSourceDBLag(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a running CDC pipeline was not sink-checked: %v", err)
	}
	if matcher.parkedReads != 3 {
		t.Fatalf("ran %d sink checks for 3 running CDC pipelines, want exactly one each", matcher.parkedReads)
	}
}

// TestDiscoverCDCSinkTargets_NamesNoFamily: the catch-all query must not filter on a source
// family — a family list is how MariaDB was missed — and must keep a pipeline whose source
// connection row is gone (LEFT JOIN).
func TestDiscoverCDCSinkTargets_NamesNoFamily(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_, actual string) error {
			for _, family := range []string{"ILIKE", "connector_type =", "connector_type IN"} {
				if strings.Contains(actual, family) {
					return errors.New("discovery filters on the source family: " + family)
				}
			}
			if !strings.Contains(actual, "LEFT JOIN connections") {
				return errors.New("discovery drops pipelines without a source connection row")
			}
			return nil
		})))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`.`).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_type"}))

	s := &CDCSentinel{db: db}
	s.discoverCDCSinkTargets(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverCDCSinkTargets_NormalizesDBTypeLabel locks that the discovered targets carry a
// canonical dbType label — connector_type aliases are folded (oracledb→oracle, mssql→sqlserver)
// via cdc.NormalizeDBType so the emitted issue metadata is consistent regardless of how the
// source connection was named — and that a pipeline with no source connection is labelled
// "unknown" rather than with an empty string.
func TestDiscoverCDCSinkTargets_NormalizesDBTypeLabel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`LEFT JOIN connections c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_type"}).
			AddRow("p-ora", "ora-cdc", "oracledb"). // alias → oracle
			AddRow("p-mss", "mss-cdc", "mssql").    // alias → sqlserver
			AddRow("p-mongo", "mongo-cdc", "mongodb").
			AddRow("p-maria", "maria-cdc", "MariaDB").
			AddRow("p-gone", "gone-cdc", "")) // source connection row deleted

	s := &CDCSentinel{db: db}
	got := s.discoverCDCSinkTargets(context.Background())

	want := map[string]string{
		"p-ora": "oracle", "p-mss": "sqlserver", "p-mongo": "mongodb",
		"p-maria": "mariadb", "p-gone": "unknown",
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %d targets, want %d: %+v", len(got), len(want), got)
	}
	for _, tgt := range got {
		if w, ok := want[tgt.pipelineID]; !ok || tgt.dbType != w {
			t.Errorf("target %q: dbType = %q, want %q", tgt.pipelineID, tgt.dbType, w)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDiscoverCDCSinkTargets_QueryErrorReturnsNil locks fail-safe behaviour: a discovery
// query error must yield no targets (and no panic), so a transient control-DB hiccup can never
// fan out sink checks against bogus pipeline ids.
func TestDiscoverCDCSinkTargets_QueryErrorReturnsNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`LEFT JOIN connections c`).WillReturnError(errors.New("boom"))

	s := &CDCSentinel{db: db}
	if got := s.discoverCDCSinkTargets(context.Background()); got != nil {
		t.Errorf("on query error, want nil targets, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
