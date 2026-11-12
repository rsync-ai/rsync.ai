package handlers

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/sqlexec/sqlexectest"
)

// Bug class: a read path that trusts a keyword gate
// (KI-SQL-READ-GATE-LETS-WRITES-THROUGH). The engines' reads are pinned in
// shared/go/sqlexec (readonly_test.go); these pin the two the gateway still runs itself
// until query-service does (query-service-design.md § 9, Q3 and Q4): a table sample,
// Test checks and a model run's warning checks. Each runs inside a transaction the database holds read-only (on SQL
// Server, one that is always rolled back), after a server-side statement timeout, and
// commits nothing.

// A table sample (the connection page, the transforms preview, Ask rsync.ai's
// sample_rows) reads as the Explorer does: read-only, under a server-side timeout,
// rolled back. A table whose read writes is refused by the database.
func TestTableSamplesRunReadOnly(t *testing.T) {
	config := map[string]interface{}{"host": "db.internal", "port": 5432, "user": "u", "password": "p", "database": "d"}
	for _, tc := range []struct {
		connectorType string
		driver        string
		timeoutRe     *regexp.Regexp
	}{
		{"postgres", "postgres", sqlexectest.PGTimeoutRe},
		{"postgresql", "postgres", sqlexectest.PGTimeoutRe},
		{"mysql", "mysql", sqlexectest.MySQLTimeoutRe},
	} {
		t.Run(tc.connectorType, func(t *testing.T) {
			// Each caller gives a sample 5 seconds.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			rec, _ := sqlexectest.Record(t, &openExplorerDB)
			if _, err := sampleDataFromConnection(ctx, tc.connectorType, config, "rsync_writes", 10); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
				t.Errorf("a sample whose read writes was not refused: err %v, events %q", err, rec.Events())
			}
			for _, e := range rec.Events() {
				if e == "WRITE" {
					t.Errorf("the sample wrote: %q", rec.Events())
				}
			}

			rec, opened := sqlexectest.Record(t, &openExplorerDB)
			res, err := sampleDataFromConnection(ctx, tc.connectorType, config, "rsync_probe", 10)
			if err != nil {
				t.Fatalf("sample failed: %v (events %q)", err, rec.Events())
			}
			if len(*opened) != 1 || (*opened)[0] != tc.driver {
				t.Fatalf("opened %q, want one %q connection", *opened, tc.driver)
			}
			if res.RowCount != 1 {
				t.Errorf("got %d rows, want the probe's 1", res.RowCount)
			}
			sqlexectest.AssertReadOnlyRead(t, rec.Events(), true, tc.timeoutRe, 5_000)
			if tc.driver == "mysql" {
				sqlexectest.AssertMySQLSessionReadOnly(t, rec.Events())
			}
		})
	}
}

// Test checks reads a model's table on the model's own connection: each check runs in
// its own read-only transaction, so a failed check ends nothing but itself.
func TestTestChecksReadOnly(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"host":"db.internal","user":"u","password":"p","database":"d"}`)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}

	for _, tc := range []struct {
		dialect      string
		wantReadOnly bool
		timeoutRe    *regexp.Regexp
	}{
		{"postgres", true, sqlexectest.PGTimeoutRe},
		{"mysql", true, sqlexectest.MySQLTimeoutRe},
		{"sqlserver", false, nil},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			rec, _ := sqlexectest.Record(t, &openExplorerDB)
			m := &savedQueryModel{ConnectionID: "c1", ConnectorType: tc.dialect, ConfigEncrypted: enc}
			rd, closeReader, refusal := openCheckReader(context.Background(), m, tc.dialect)
			if refusal != "" {
				t.Fatalf("refused: %s", refusal)
			}
			defer closeReader()
			if rd.abortsOnError() {
				t.Errorf("a failed check would end the checks after it")
			}
			for i := 0; i < 2; i++ {
				got, err := rd.counts(context.Background(), "SELECT 5 AS rsync_probe", 1)
				if err != nil || len(got) != 1 || got[0] == nil || *got[0] != 5 {
					t.Fatalf("check %d: got %v, %v", i, got, err)
				}
			}
			events := rec.Events()
			sqlexectest.AssertReadOnlyRead(t, events, tc.wantReadOnly, tc.timeoutRe, modelRunTimeout.Milliseconds())
			if tc.dialect == "mysql" {
				sqlexectest.AssertMySQLSessionReadOnly(t, events)
			}
			begins, rollbacks := 0, 0
			for _, e := range events {
				switch {
				case strings.HasPrefix(e, "begin "):
					begins++
				case e == "rollback":
					rollbacks++
				}
			}
			if begins != 2 || rollbacks != 2 {
				t.Errorf("want each of the 2 checks in its own transaction, got %d begins and %d rollbacks: %q", begins, rollbacks, events)
			}
		})
	}
}

// A model run's warning checks read the table the run has just published, once its plan
// is done: each runs in its own read-only transaction on the run's connection, as Test
// checks do (KI-WAREHOUSE-READS-RELY-ON-THE-GATE). They used to read on that connection
// with nothing around them, so a check's SQL the gate missed could write.
func TestModelRunWarningChecksReadOnly(t *testing.T) {
	for _, tc := range []struct {
		dialect      string
		wantReadOnly bool
		timeoutRe    *regexp.Regexp
	}{
		{"postgres", true, sqlexectest.PGTimeoutRe},
		{"mysql", true, sqlexectest.MySQLTimeoutRe},
		{"sqlserver", false, nil},
	} {
		// run publishes s.<table> with one warning check on it, and returns the check's
		// results and what was sent after the plan's last statement (or its commit).
		run := func(t *testing.T, table string) ([]modelCheckResult, []string) {
			ctx, cancel := context.WithTimeout(context.Background(), modelRunTimeout)
			defer cancel()
			rec := &sqlexectest.Driver{}
			database := sql.OpenDB(rec)
			defer database.Close()
			database.SetMaxOpenConns(1) // as executeModelPlan
			checks := driverPlanChecks(tc.dialect, "s", table, []modelCheck{
				{Name: "has rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityWarn},
			})
			if _, err := runModelPlan(ctx, database, tc.dialect, ownedPlan(tc.dialect, "s", table), "", checks); err != nil {
				t.Fatalf("run failed: %v (events %q)", err, rec.Events())
			}
			stmts := buildMaterializationPlan(tc.dialect, "s", table, "SELECT id FROM src", true)
			events, done := rec.Events(), -1
			for i, e := range events {
				if e == "commit" || strings.HasSuffix(e, ": "+stmts[len(stmts)-1]) {
					done = i
				}
			}
			if done < 0 {
				t.Fatalf("the plan did not run: %q", events)
			}
			return checks.run.resultsOrNil(), events[done+1:]
		}

		t.Run(tc.dialect+"/reads in a read-only transaction", func(t *testing.T) {
			res, after := run(t, "rsync_probe")
			sqlexectest.AssertReadOnlyRead(t, after, tc.wantReadOnly, tc.timeoutRe, modelRunTimeout.Milliseconds())
			if tc.dialect == "mysql" {
				sqlexectest.AssertMySQLSessionReadOnly(t, after)
			}
			begins, rollbacks := 0, 0
			for _, e := range after {
				switch {
				case strings.HasPrefix(e, "begin "):
					begins++
				case e == "rollback":
					rollbacks++
				}
			}
			if begins != 2 || rollbacks != 2 {
				t.Errorf("want the check's 2 reads each in its own transaction, got %d begins and %d rollbacks: %q", begins, rollbacks, after)
			}
			if len(res) != 1 || res[0].Status != checkStatusPass || res[0].Observed == nil || *res[0].Observed != 5 {
				t.Errorf("results %+v, want the check passed on the probe's count of 5", res)
			}
		})

		// A check whose read writes: Postgres and MySQL refuse it, and on SQL Server it
		// runs only inside the transaction that is rolled back, which undoes it.
		t.Run(tc.dialect+"/a check that writes", func(t *testing.T) {
			res, after := run(t, "rsync_writes")
			inTx := false
			for _, e := range after {
				switch {
				case strings.HasPrefix(e, "begin "):
					inTx = true
				case e == "rollback":
					inTx = false
				case e == "commit":
					t.Errorf("a warning check committed: %q", after)
				case e == "WRITE" && (tc.wantReadOnly || !inTx):
					t.Errorf("a warning check wrote: %q", after)
				}
			}
			if tc.wantReadOnly && (len(res) != 1 || res[0].Status != checkStatusError || !strings.Contains(res[0].Message, "read-only transaction")) {
				t.Errorf("results %+v, want the check refused by the read-only transaction", res)
			}
		})
	}

	// The checks cannot reach the table at all: each is recorded as not run, not left
	// out of the run's results.
	t.Run("no connection for the checks", func(t *testing.T) {
		database := sql.OpenDB(&sqlexectest.Driver{})
		_ = database.Close()
		checks := driverPlanChecks("postgres", "s", "orders", []modelCheck{
			{Name: "has rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityWarn},
		})
		checks.report(context.Background(), database, "postgres")
		res := checks.run.resultsOrNil()
		if len(res) != 1 || res[0].Status != checkStatusError || !strings.Contains(res[0].Message, "database is closed") {
			t.Errorf("results %+v, want the check recorded as not run, with the reason", res)
		}
	})
}
