package handlers

import (
	"context"
	"database/sql"
	"time"

	"github.com/rsync-ai/shared/sqlexec"
)

// openExplorerDB opens a direct-driver read's connection (a table sample, Test checks).
// A test swaps it for a driver that records what the read sent (read_only_reads_test.go).
var openExplorerDB = sql.Open

// readOnlyCheckReader runs each data check in its own read-only transaction on one
// connection (Test checks, and a model run's warning checks once its plan is done). Its
// own transaction per check means a failed check ends nothing but itself, as on a plain
// connection.
type readOnlyCheckReader struct {
	conn    *sql.Conn
	dialect string
	timeout time.Duration
}

func (r readOnlyCheckReader) inReadTx(ctx context.Context, fn func(driverCheckReader) error) error {
	tx, err := sqlexec.BeginReadOnly(ctx, r.conn, r.dialect, r.timeout)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(driverCheckReader{q: tx, dialect: r.dialect})
}

func (r readOnlyCheckReader) counts(ctx context.Context, query string, n int) ([]*int64, error) {
	var out []*int64
	err := r.inReadTx(ctx, func(d driverCheckReader) error {
		var err error
		out, err = d.counts(ctx, query, n)
		return err
	})
	return out, err
}

func (r readOnlyCheckReader) columns(ctx context.Context, table string) ([]string, bool, error) {
	var cols []string
	known := true
	err := r.inReadTx(ctx, func(d driverCheckReader) error {
		var err error
		cols, known, err = d.columns(ctx, table)
		return err
	})
	return cols, known, err
}

func (r readOnlyCheckReader) abortsOnError() bool { return false }
