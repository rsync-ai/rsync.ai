package sqlexec

import (
	"context"
	"fmt"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// mysqlExplorerDSN builds the go-sql-driver/mysql DSN for an Explorer connection.
// Shared by the read (executeMySQLQueryWithRedact) and write (executeDirectWrite)
// paths so the two never drift on TLS mode, port coercion, or timeouts.
func mysqlExplorerDSN(config map[string]interface{}) string {
	host, _ := config["host"].(string)
	port := config["port"]
	user, _ := config["user"].(string)
	password, _ := config["password"].(string)
	database, _ := config["database"].(string)

	portInt := 3306
	switch v := port.(type) {
	case float64:
		portInt = int(v)
	case int:
		portInt = v
	case string:
		fmt.Sscanf(v, "%d", &portInt)
	}

	// Conservative timeouts (match Postgres behavior)
	tlsMode := resolveMySQLTLSMode(config, host)
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&timeout=10s&readTimeout=10s&writeTimeout=10s&tls=%s",
		user, password, host, portInt, database, tlsMode)
}

func executeMySQLQueryWithRedact(ctx context.Context, config map[string]interface{}, sqlQuery string, limit int, redact bool, args ...interface{}) (*Result, error) {
	dsn := mysqlExplorerDSN(config)

	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := openExplorerDB("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(0)
	conn.SetConnMaxLifetime(15 * time.Second)

	// One session, so the timeout set below applies to the query.
	dbc, err := conn.Conn(queryCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire db connection: %w", err)
	}
	defer dbc.Close()

	if err := dbc.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	wrappedSQL := stmt.ClampLimit(sqlQuery, limit+1) // +1 row so the Truncated flag below is accurate (ClampLimit caps the DB-side LIMIT)
	startTime := time.Now()

	// Read-only (readonly.go)
	tx, err := beginReadOnly(queryCtx, dbc, "mysql", timeUntilDeadline(queryCtx, 10*time.Second))
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(queryCtx, wrappedSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}
	types := declaredTypes(rows)

	result := []map[string]interface{}{}
	warnings := []string{}
	truncated := false

	for rows.Next() {
		if len(result) >= limit {
			truncated = true
			break
		}

		columnValues := make([]interface{}, len(columns))
		columnPointers := make([]interface{}, len(columns))
		for i := range columnValues {
			columnPointers[i] = &columnValues[i]
		}

		if err := rows.Scan(columnPointers...); err != nil {
			warnings = append(warnings, fmt.Sprintf("Failed to scan row: %v", err))
			continue
		}

		rowMap := make(map[string]interface{})
		for i, colName := range columns {
			val := columnValues[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			// PII redaction bypassed for data-owner export (T2-7).
			if redact && mask.ShouldRedactColumnName(colName) {
				val = mask.RedactForPreview(val)
			}
			rowMap[colName] = val
		}

		result = append(result, rowMap)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	executionTime := time.Since(startTime).Milliseconds()
	return &Result{
		Columns:         columns,
		Rows:            result,
		RowCount:        len(result),
		ExecutionTimeMs: executionTime,
		Truncated:       truncated,
		Warnings:        warnings,
		ColumnTypes:     types,
	}, nil
}
