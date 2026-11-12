package sqlexec

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/rsync-ai/shared/sqlexec/sqlexectest"
)

// Bug class: a read path that trusts a keyword gate
// (KI-SQL-READ-GATE-LETS-WRITES-THROUGH). The gate reads SQL text; these tests pin the
// second line behind it, on every direct engine's read: the user's statement executes
// inside a transaction the database holds read-only (on SQL Server, one that is always
// rolled back), after a server-side statement timeout, and nothing is ever committed.
// They drive the engines through a driver that records what each one sent.

func TestExplorerReadsRunReadOnly(t *testing.T) {
	const probe = "SELECT 5 AS rsync_probe"
	config := map[string]interface{}{"host": "db.internal", "port": 5432, "user": "u", "password": "p", "database": "d"}

	cases := []struct {
		name          string
		connectorType string
		mask          bool
		driver        string
		wantReadOnly  bool
		timeoutRe     *regexp.Regexp
	}{
		{"postgres preview", "postgres", true, "postgres", true, sqlexectest.PGTimeoutRe},
		{"postgres export", "postgres", false, "postgres", true, sqlexectest.PGTimeoutRe},
		{"redshift preview", "redshift", true, "postgres", true, sqlexectest.PGTimeoutRe},
		{"mysql preview", "mysql", true, "mysql", true, sqlexectest.MySQLTimeoutRe},
		{"mysql export", "mysql", false, "mysql", true, sqlexectest.MySQLTimeoutRe},
		{"mariadb preview", "mariadb", true, "mysql", true, sqlexectest.MySQLTimeoutRe},
		// go-mssqldb has no read-only transaction and SQL Server no session statement
		// timeout: the read runs in a transaction that is always rolled back.
		{"sqlserver preview", "sqlserver", true, "sqlserver", false, nil},
		{"sqlserver export", "sqlserver", false, "sqlserver", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, opened := sqlexectest.Record(t, &openExplorerDB)
			eng, err := For(tc.connectorType, Config{Settings: config})
			if err != nil {
				t.Fatalf("For(%q): %v", tc.connectorType, err)
			}
			res, err := eng.Query(context.Background(), probe, nil, Limits{Rows: 100, Mask: tc.mask})
			if err != nil {
				t.Fatalf("read failed: %v (events %q)", err, rec.Events())
			}
			if len(*opened) != 1 || (*opened)[0] != tc.driver {
				t.Fatalf("opened %q, want one %q connection", *opened, tc.driver)
			}
			if res.RowCount != 1 {
				t.Errorf("got %d rows, want the probe's 1", res.RowCount)
			}
			// Each column's declared type comes back with it: a value alone cannot tell
			// a NUMERIC, which Postgres and MySQL return as text, from text.
			if !reflect.DeepEqual(res.ColumnTypes, []string{"INT8"}) {
				t.Errorf("column types %q, want the driver's [INT8]", res.ColumnTypes)
			}
			// The engines bound a read by 10s; the server's bound must not exceed it.
			sqlexectest.AssertReadOnlyRead(t, rec.Events(), tc.wantReadOnly, tc.timeoutRe, 10_000)
			if tc.driver == "mysql" {
				sqlexectest.AssertMySQLSessionReadOnly(t, rec.Events())
			}
		})
	}
}

// The server's timeout follows the caller's deadline when that is shorter: a workflow
// step's or a request's budget, not a fixed 10s.
func TestExplorerReadTimeoutFollowsTheCallersDeadline(t *testing.T) {
	rec, _ := sqlexectest.Record(t, &openExplorerDB)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	eng := postgresEngine{Config{Settings: map[string]interface{}{"host": "db.internal"}}}
	if _, err := eng.Query(ctx, "SELECT 5 AS rsync_probe", nil, Limits{Rows: 10, Mask: true}); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	sqlexectest.AssertReadOnlyRead(t, rec.Events(), true, sqlexectest.PGTimeoutRe, 3_000)
}

// MariaDB has no max_execution_time; the read falls back to its max_statement_time
// (seconds). A server with neither still reads, read-only, under the caller's deadline.
func TestMySQLReadTimeoutFallsBackForMariaDB(t *testing.T) {
	unknown := &mysql.MySQLError{Number: mysqlUnknownSystemVariable, Message: "Unknown system variable"}
	eng := mysqlEngine{Config{Settings: map[string]interface{}{"host": "db.internal"}}}
	read := func() error {
		_, err := eng.Query(context.Background(), "SELECT 5 AS rsync_probe", nil, Limits{Rows: 10, Mask: true})
		return err
	}

	t.Run("mariadb", func(t *testing.T) {
		rec, _ := sqlexectest.Record(t, &openExplorerDB)
		rec.ExecErr = map[string]error{"max_execution_time": unknown}
		if err := read(); err != nil {
			t.Fatalf("read failed: %v", err)
		}
		sqlexectest.AssertReadOnlyRead(t, rec.Events(), true, regexp.MustCompile(`^exec: SET SESSION max_statement_time = (\d+\.\d+)$`), 10_000)
		sqlexectest.AssertMySQLSessionReadOnly(t, rec.Events())
	})

	t.Run("neither variable", func(t *testing.T) {
		rec, _ := sqlexectest.Record(t, &openExplorerDB)
		rec.ExecErr = map[string]error{"max_execution_time": unknown, "max_statement_time": unknown}
		if err := read(); err != nil {
			t.Fatalf("read failed: %v", err)
		}
		sqlexectest.AssertReadOnlyRead(t, rec.Events(), true, nil, 0)
		sqlexectest.AssertMySQLSessionReadOnly(t, rec.Events())
	})

	t.Run("any other error stops the read", func(t *testing.T) {
		rec, _ := sqlexectest.Record(t, &openExplorerDB)
		rec.ExecErr = map[string]error{"max_execution_time": &mysql.MySQLError{Number: 1227, Message: "Access denied"}}
		if err := read(); err == nil {
			t.Fatalf("read ran without its timeout: %q", rec.Events())
		}
		for _, e := range rec.Events() {
			if strings.Contains(e, "rsync_probe") {
				t.Errorf("the statement ran after the timeout could not be set: %q", rec.Events())
			}
		}
	})
}
