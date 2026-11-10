// Package sqlexectest records what a direct-driver read sent, so a test can check it ran
// read-only (KI-SQL-READ-GATE-LETS-WRITES-THROUGH): the user's statement executes inside
// a transaction the database holds read-only (on SQL Server, one that is always rolled
// back), after a server-side statement timeout, and nothing is ever committed.
//
// sqlexec's own tests use it on the engines' reads; the gateway's on the reads it still
// runs itself (a table sample, Test checks). It imports nothing of sqlexec, so either
// can import it.
package sqlexectest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Driver logs every statement, transaction boundary and its options.
type Driver struct {
	mu     sync.Mutex
	events []string
	// ExecErr, when set, answers an Exec whose text contains the key.
	ExecErr map[string]error
}

func (d *Driver) log(e string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
}

// Events is what the reads sent, in order.
func (d *Driver) Events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func (d *Driver) Connect(context.Context) (driver.Conn, error) {
	return &recordingConn{d: d}, nil
}
func (d *Driver) Driver() driver.Driver { return nil }

type recordingConn struct {
	d        *Driver
	inTx     bool
	readOnly bool
}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recordingConn: Prepare is not used")
}
func (c *recordingConn) Close() error { return nil }
func (c *recordingConn) Begin() (driver.Tx, error) {
	return nil, errors.New("recordingConn: use BeginTx")
}

func (c *recordingConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.d.log(fmt.Sprintf("begin read_only=%v", opts.ReadOnly))
	c.inTx, c.readOnly = true, opts.ReadOnly
	return &recordingTx{c: c}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	where := "exec"
	if c.inTx {
		where = "exec(tx)"
	}
	c.d.log(where + ": " + q)
	for key, err := range c.d.ExecErr {
		if strings.Contains(q, key) {
			return nil, err
		}
	}
	return driver.RowsAffected(0), nil
}

// QueryContext answers the probe statement with one row (5), anything else (the
// executors' own catalog lookups) with none. A read of rsync_writes writes, as a view
// over a function that inserts does: the database refuses it in a read-only
// transaction and does it anywhere else.
func (c *recordingConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	where := "query"
	if c.inTx {
		where = "query(tx)"
	}
	c.d.log(where + ": " + q)
	if strings.Contains(q, "rsync_probe") {
		return &recordingRows{cols: []string{"rsync_c0"}, left: 1}, nil
	}
	if strings.Contains(q, "rsync_writes") {
		if c.inTx && c.readOnly {
			return nil, errors.New("cannot execute INSERT in a read-only transaction")
		}
		c.d.log("WRITE")
		return &recordingRows{cols: []string{"rsync_c0"}, left: 1}, nil
	}
	return &recordingRows{cols: []string{"x"}}, nil
}

type recordingTx struct{ c *recordingConn }

func (t *recordingTx) Commit() error   { t.c.d.log("commit"); t.c.inTx = false; return nil }
func (t *recordingTx) Rollback() error { t.c.d.log("rollback"); t.c.inTx = false; return nil }

type recordingRows struct {
	cols []string
	left int
}

func (r *recordingRows) Columns() []string { return r.cols }
func (r *recordingRows) Close() error      { return nil }
func (r *recordingRows) Next(dest []driver.Value) error {
	if r.left == 0 {
		return io.EOF
	}
	r.left--
	for i := range dest {
		dest[i] = int64(5)
	}
	return nil
}

// ColumnTypeDatabaseTypeName declares each column as the int64 Next writes into it.
func (r *recordingRows) ColumnTypeDatabaseTypeName(int) string { return "INT8" }

// Record routes *open (a package's openExplorerDB) to a Driver for the test's duration
// and records which driver name each read asked for.
func Record(t testing.TB, open *func(driverName, dsn string) (*sql.DB, error)) (*Driver, *[]string) {
	t.Helper()
	rec := &Driver{}
	var opened []string
	orig := *open
	*open = func(name, _ string) (*sql.DB, error) {
		opened = append(opened, name)
		return sql.OpenDB(rec), nil
	}
	t.Cleanup(func() { *open = orig })
	return rec, &opened
}

// AssertReadOnlyRead checks one read's events: the timeout (when the engine has one)
// is set before the transaction, the probe runs inside it, and the transaction is
// rolled back and never committed.
func AssertReadOnlyRead(t testing.TB, events []string, wantReadOnly bool, timeoutRe *regexp.Regexp, maxTimeoutMS int64) {
	t.Helper()
	idx := func(pred func(string) bool) int {
		for i, e := range events {
			if pred(e) {
				return i
			}
		}
		return -1
	}
	begin := idx(func(e string) bool { return strings.HasPrefix(e, "begin ") })
	probe := idx(func(e string) bool { return strings.Contains(e, "rsync_probe") })
	rollback := idx(func(e string) bool { return e == "rollback" })

	if begin < 0 {
		t.Fatalf("the read opened no transaction: %q", events)
	}
	if got := events[begin]; got != fmt.Sprintf("begin read_only=%v", wantReadOnly) {
		t.Errorf("transaction opened as %q, want read_only=%v", got, wantReadOnly)
	}
	if probe < 0 || !strings.HasPrefix(events[probe], "query(tx): ") {
		t.Errorf("the statement did not run inside the transaction: %q", events)
	}
	if rollback < probe {
		t.Errorf("the transaction was not rolled back after the statement: %q", events)
	}
	for _, e := range events {
		if e == "commit" {
			t.Errorf("a read committed: %q", events)
		}
	}
	if timeoutRe == nil {
		return
	}
	at := idx(func(e string) bool { return timeoutRe.MatchString(e) })
	if at < 0 || at > probe {
		t.Fatalf("no server-side timeout set before the statement: %q", events)
	}
	ms, err := parseTimeoutMS(timeoutRe.FindStringSubmatch(events[at])[1])
	if err != nil || ms <= 0 || ms > maxTimeoutMS {
		t.Errorf("server-side timeout %q is not within (0, %dms]", events[at], maxTimeoutMS)
	}
}

// AssertMySQLSessionReadOnly fails unless the session was made read-only before the
// read's transaction opened. START TRANSACTION READ ONLY alone lets DDL through on MySQL:
// a CREATE commits that transaction and runs in one of its own (checked on MySQL 8.4).
func AssertMySQLSessionReadOnly(t testing.TB, events []string) {
	t.Helper()
	for _, e := range events {
		if e == "exec: SET SESSION TRANSACTION READ ONLY" {
			return
		}
		if strings.HasPrefix(e, "begin ") {
			break
		}
	}
	t.Errorf("the MySQL session was not held read-only before the transaction: %q", events)
}

func parseTimeoutMS(s string) (int64, error) {
	if strings.Contains(s, ".") { // MariaDB takes seconds
		f, err := strconv.ParseFloat(s, 64)
		return int64(f * 1000), err
	}
	return strconv.ParseInt(s, 10, 64)
}

// The server-side timeout each driver's read sets, in milliseconds.
var (
	PGTimeoutRe    = regexp.MustCompile(`^exec\(tx\): SET LOCAL statement_timeout = (\d+)$`)
	MySQLTimeoutRe = regexp.MustCompile(`^exec: SET SESSION max_execution_time = (\d+)$`)
)
