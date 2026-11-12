package sqlexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	log "github.com/sirupsen/logrus"
)

// A read the gateway runs on a direct driver (Explorer preview and export, a workflow's
// query step, Test checks) runs inside a transaction the database itself holds to
// reads, under a timeout the server enforces. The statement gate
// (validators.ValidateExplorerStatement) refuses writes by reading the text; these two
// are what still hold when the text fools it (KI-SQL-READ-GATE-LETS-WRITES-THROUGH).
//
// Per driver:
//   - postgres (and Redshift): BEGIN READ ONLY, then SET LOCAL statement_timeout.
//   - mysql (and MariaDB): the session held read-only, then START TRANSACTION READ
//     ONLY, and a session max_execution_time (MySQL) or max_statement_time (MariaDB).
//   - sqlserver: go-mssqldb has no read-only transaction, so the read runs in an
//     ordinary one that is always rolled back; SQL Server undoes DDL with it. There is
//     no session statement timeout either: on the context's deadline go-mssqldb sends
//     an attention, which stops the statement on the server.

// openExplorerDB opens a direct-driver read's connection. A test swaps it for a driver
// that records what the read sent (readonly_test.go, through sqlexectest).
var openExplorerDB = sql.Open

// beginReadOnly opens the transaction one read runs in, with a server-side timeout of d
// on each statement, so a read stops on the server and not only in the gateway: pgx
// does not cancel a Postgres query when its context ends, and go-sql-driver/mysql only
// closes its connection. The caller always rolls the transaction back: a read has
// nothing to commit, and on SQL Server the rollback is what undoes a write.
func beginReadOnly(ctx context.Context, conn *sql.Conn, driver string, d time.Duration) (*sql.Tx, error) {
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if driver == "mysql" {
		// MySQL commits the open transaction before a DDL statement and runs the
		// statement in a transaction of its own, so START TRANSACTION READ ONLY alone
		// lets a CREATE or DROP through. That transaction takes the session's access
		// mode, so holding the session read-only refuses it too.
		if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
			return nil, fmt.Errorf("could not make the session read-only: %w", err)
		}
		if err := setMySQLReadTimeout(ctx, conn, ms); err != nil {
			return nil, err
		}
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: driver != "sqlserver"})
	if err != nil {
		return nil, fmt.Errorf("could not open a read-only transaction: %w", err)
	}
	if driver == "postgres" {
		// SET LOCAL ends with the transaction, so a pooler in transaction mode
		// (PgBouncer, Supabase, Neon) never hands the setting to another client.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("could not set a statement timeout: %w", err)
		}
	}
	return tx, nil
}

// mysqlUnknownSystemVariable is ER_UNKNOWN_SYSTEM_VARIABLE: MariaDB has no
// max_execution_time, MySQL no max_statement_time.
const mysqlUnknownSystemVariable = 1193

// setMySQLReadTimeout bounds each later statement on conn's session by ms. A session
// setting is safe here because each read opens its own connection and closes it.
func setMySQLReadTimeout(ctx context.Context, conn *sql.Conn, ms int64) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION max_execution_time = %d", ms))
	if !isMySQLError(err, mysqlUnknownSystemVariable) {
		if err != nil {
			return fmt.Errorf("could not set a statement timeout: %w", err)
		}
		return nil
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf("SET SESSION max_statement_time = %.3f", float64(ms)/1000))
	if isMySQLError(err, mysqlUnknownSystemVariable) {
		// A server older than MySQL 5.7.8 or MariaDB 10.1 has neither variable. The
		// read still runs read-only; only the gateway's deadline bounds it.
		log.Warn("explorer: the MySQL server has no statement timeout variable; the read is bounded by the gateway's deadline only")
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not set a statement timeout: %w", err)
	}
	return nil
}

func isMySQLError(err error, number uint16) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == number
}

// timeUntilDeadline is what is left of ctx's deadline, or fallback when it has none.
func timeUntilDeadline(ctx context.Context, fallback time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return fallback
}
