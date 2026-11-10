package sqlexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// executeDirectWrite runs an authorized write / DDL / destructive statement against a
// database/sql engine (PostgreSQL/Redshift, MySQL, SQL Server) via ExecContext and
// returns the affected-row count. Authorization (the workspace-role gate) and
// single-statement validation already ran in validators.ValidateExplorerStatement;
// this path applies no LIMIT and scans no rows. Writes get a longer deadline than the
// read preview since DDL and bulk DML can outlast a SELECT sample.
func executeDirectWrite(ctx context.Context, driver, dsn, sqlText string) (*Result, error) {
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(0)
	conn.SetConnMaxLifetime(35 * time.Second)

	if err := conn.PingContext(execCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	start := time.Now()
	res, err := conn.ExecContext(execCtx, sqlText)
	if err != nil {
		return nil, fmt.Errorf("statement failed: %w", err)
	}

	resp := &Result{
		Columns:         []string{},
		Rows:            []map[string]interface{}{},
		ExecutionTimeMs: time.Since(start).Milliseconds(),
	}
	if reportsRowsAffected(stmt.ClassifyStatementSQL(sqlText)) {
		if n, err := res.RowsAffected(); err == nil {
			resp.RowsAffected = &n
		}
	}
	return resp, nil
}

// reportsRowsAffected answers whether a statement class has a meaningful affected-row
// count to show the user. Only DML does.
//
// DDL and DROP/TRUNCATE return 0 from every driver's RowsAffected() because they affect
// no rows by definition — but the UI rendered that as "DROP succeeded — 0 rows affected",
// which reads like nothing happened at the exact moment a table was destroyed. Leaving
// the field nil makes the UI fall back to "The statement completed successfully."
//
// ClassUnknown is excluded on purpose: if we can't classify the statement we can't
// vouch for the number either.
func reportsRowsAffected(class stmt.StatementClass) bool {
	return class == stmt.ClassDMLWrite
}

// executeDatabricksWrite runs an authorized write statement through the Databricks SQL
// Statement Execution API (the same endpoint the read path uses), without a LIMIT
// clamp. Databricks does not surface a portable affected-row count on this endpoint,
// so RowsAffected is left nil.
func executeDatabricksWrite(ctx context.Context, newClient func(time.Duration) *http.Client, config map[string]interface{}, sqlText string) (*Result, error) {
	host := normalizeDatabricksHost(getStringConfig(config, "host", "server_hostname", "workspace_host"))
	token := getStringConfig(config, "access_token", "token", "pat")
	warehouseID := getStringConfig(config, "warehouse_id", "warehouseId", "sql_warehouse_id")
	httpPath := getStringConfig(config, "http_path")
	if warehouseID == "" {
		warehouseID = parseWarehouseIDFromHTTPPath(httpPath)
	}
	catalog := getStringConfig(config, "catalog", "database")
	schemaName := getStringConfig(config, "schema", "schema_name")

	execCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := databricksExecuteStatement(execCtx, newClient, host, token, warehouseID, sqlText, catalog, schemaName)
	if err != nil {
		return nil, err
	}
	if resp.Status.State != "" && strings.ToUpper(resp.Status.State) != "SUCCEEDED" {
		msg := strings.TrimSpace(resp.Status.Error.Message)
		if msg == "" {
			msg = fmt.Sprintf("statement status=%s", resp.Status.State)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return &Result{
		Columns:         []string{},
		Rows:            []map[string]interface{}{},
		ExecutionTimeMs: time.Since(start).Milliseconds(),
	}, nil
}
