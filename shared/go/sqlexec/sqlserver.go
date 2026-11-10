package sqlexec

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
)

// =============================================================================
// SQL SERVER / AZURE SQL (go-mssqldb, read-only)
// =============================================================================

// sqlServerExplorerDSN builds a go-mssqldb URL DSN with host-aware TLS,
// mirroring the sqlserver connector + orchestrator resolveSQLServerEncrypt:
// Azure SQL (*.database.windows.net) presents a real CA cert → Encrypt=true +
// verify (TrustServerCertificate=false); a boxed/self-signed server encrypts
// in transit without CA verification. An explicit encrypt/sslmode/tls key wins.
func sqlServerExplorerDSN(config map[string]interface{}) string {
	host, _ := config["host"].(string)
	user, _ := config["user"].(string)
	password, _ := config["password"].(string)
	database, _ := config["database"].(string)
	host = strings.TrimSpace(host)

	portInt := 1433
	switch v := config["port"].(type) {
	case float64:
		portInt = int(v)
	case int:
		portInt = v
	case string:
		fmt.Sscanf(v, "%d", &portInt)
	}

	hl := strings.ToLower(host)
	isAzure := strings.HasSuffix(hl, ".database.windows.net")
	isLocal := isLocalDBHost(host) // address-aware (IP literals classified by address)

	raw := strings.ToLower(strings.TrimSpace(getStringConfig(config, "encrypt", "sslmode", "tls", "tls_mode")))
	encrypt, trust := "true", "false"
	switch {
	case raw == "disable" || raw == "disabled" || raw == "false" || raw == "no" || raw == "off" || raw == "0":
		encrypt, trust = "false", "true"
	case raw == "require" || raw == "required" || raw == "prefer" || raw == "preferred" || raw == "skip-verify" || raw == "skip_verify":
		encrypt, trust = "true", "true" // encrypt WITHOUT CA/hostname verify (opt-out for self-signed)
	case raw == "verify-ca" || raw == "verify_ca" || raw == "verify-full" || raw == "verify_full" || raw == "strict":
		encrypt, trust = "true", "false" // encrypt AND verify
	case isAzure:
		encrypt, trust = "true", "false" // real CA cert — verify it
	case isLocal:
		encrypt, trust = "true", "true" // local/boxed self-signed — trust
	default:
		encrypt, trust = "true", "false" // remote: encrypt AND verify by default (MITM-safe)
	}

	q := url.Values{}
	if strings.TrimSpace(database) != "" {
		q.Set("database", database)
	}
	q.Set("encrypt", encrypt)
	q.Set("TrustServerCertificate", trust)
	q.Set("dial timeout", "10")
	q.Set("connection timeout", "10")

	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(user, password),
		Host:     fmt.Sprintf("%s:%d", host, portInt),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// clampLimitSQLServer injects TOP (n) after the leading SELECT so the DB caps
// the returned rows (SQL Server has no LIMIT). CTE (WITH ...) or already-TOP'd
// queries are left unchanged; the executor's row-loop cap still bounds results.
func clampLimitSQLServer(sqlQuery string, maxLimit int) string {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sqlQuery), ";"))
	if regexp.MustCompile(`(?i)^SELECT\s+TOP\b`).MatchString(trimmed) {
		return trimmed
	}
	re := regexp.MustCompile(`(?i)^SELECT\s+(DISTINCT\s+)?`)
	loc := re.FindStringIndex(trimmed)
	if loc == nil || loc[0] != 0 {
		return trimmed // WITH/CTE or non-leading SELECT — rely on the row-loop cap
	}
	return trimmed[:loc[1]] + fmt.Sprintf("TOP (%d) ", maxLimit) + trimmed[loc[1]:]
}

func executeSQLServerQueryWithRedact(ctx context.Context, config map[string]interface{}, sqlQuery string, limit int, redact bool, args ...interface{}) (*Result, error) {
	dsn := sqlServerExplorerDSN(config)

	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := openExplorerDB("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(0)
	conn.SetConnMaxLifetime(15 * time.Second)

	dbc, err := conn.Conn(queryCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire db connection: %w", err)
	}
	defer dbc.Close()

	if err := dbc.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	wrappedSQL := clampLimitSQLServer(sqlQuery, limit+1) // +1 so Truncated is accurate
	startTime := time.Now()

	// In a transaction that is always rolled back (readonly.go)
	tx, err := beginReadOnly(queryCtx, dbc, "sqlserver", timeUntilDeadline(queryCtx, 10*time.Second))
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
