package sqlexec

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// postgresExplorerDSN builds the PostgreSQL DSN for an Explorer connection. Shared by the
// read (executePostgresQueryWithRedact) and write (executeDirectWrite) paths so the
// two never drift on TLS mode, port coercion, or connect timeout.
func postgresExplorerDSN(config map[string]interface{}) string {
	host, _ := config["host"].(string)
	port := config["port"]
	user, _ := config["user"].(string)
	password, _ := config["password"].(string)
	database, _ := config["database"].(string)
	sslMode := resolvePostgresSSLMode(config, host)

	portInt := 5432
	switch v := port.(type) {
	case float64:
		portInt = int(v)
	case int:
		portInt = v
	case string:
		fmt.Sscanf(v, "%d", &portInt)
	}

	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10",
		host, portInt, user, password, database, sslMode)
}

// executePostgresQueryWithRedact is the shared implementation. When
// redact is true, PII columns (by name match) are masked before the
// rows leave this function. When false, raw values are returned.
func executePostgresQueryWithRedact(ctx context.Context, config map[string]interface{}, sqlQuery string, limit int, redact bool, args ...interface{}) (*Result, error) {
	dsn := postgresExplorerDSN(config)

	// Open connection with timeout
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := openExplorerDB("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	// Set connection limits
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(0)
	conn.SetConnMaxLifetime(15 * time.Second)

	// Use a single session so SET search_path affects the query.
	dbc, err := conn.Conn(queryCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire db connection: %w", err)
	}
	defer dbc.Close()

	// Test connection
	if err := dbc.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	warnings := []string{}
	// Best-effort: set search_path so unqualified tables work (e.g. `digits` vs `logistics_db.digits`).
	// We infer schemas from referenced tables in the SQL and the current database's information_schema.
	if schemas := inferPostgresSearchPathSchemas(queryCtx, dbc, sqlQuery); len(schemas) > 0 {
		parts := make([]string, 0, len(schemas)+1)
		for _, s := range schemas {
			parts = append(parts, quotePGIdent(s))
		}
		// Always include public as fallback.
		parts = append(parts, quotePGIdent("public"))
		stmt := fmt.Sprintf("SET search_path TO %s", strings.Join(parts, ", "))
		if _, err := dbc.ExecContext(queryCtx, stmt); err != nil {
			// Non-fatal; query might still succeed if fully-qualified.
			warnings = append(warnings, fmt.Sprintf("Failed to set search_path (continuing): %v", err))
		}
	}

	// Clamp query LIMIT using validator
	wrappedSQL := stmt.ClampLimit(sqlQuery, limit+1) // +1 row so the Truncated flag below is accurate (ClampLimit caps the DB-side LIMIT)

	startTime := time.Now()

	// Execute query, read-only (readonly.go)
	tx, err := beginReadOnly(queryCtx, dbc, "postgres", timeUntilDeadline(queryCtx, 10*time.Second))
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(queryCtx, wrappedSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}
	types := declaredTypes(rows)

	// Scan rows
	result := []map[string]interface{}{}
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

			// Convert []byte to string
			if b, ok := val.([]byte); ok {
				val = string(b)
			}

			// Redact PII + secrets in preview mode (but avoid masking
			// generic ids). Skipped for the export path (T2-7): the
			// data owner expects to see their own raw values in CSV
			// downloads of their own connection.
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

type sqlQueryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func inferPostgresSearchPathSchemas(ctx context.Context, q sqlQueryer, sqlQuery string) []string {
	if ctx == nil || q == nil {
		return nil
	}
	v := stmt.ValidateExplorerSQL(sqlQuery)
	if v == nil || len(v.ReferencedTables) == 0 {
		return nil
	}

	// Collect schema-qualified refs directly from SQL, and unqualified table names for lookup.
	explicitSchemas := map[string]struct{}{}
	tableNames := map[string]struct{}{}
	for _, t := range v.ReferencedTables {
		raw := strings.TrimSpace(t)
		if raw == "" {
			continue
		}
		// Remove surrounding quotes for simple parsing.
		raw = strings.Trim(raw, `"'`)
		if strings.Contains(raw, ".") {
			parts := strings.SplitN(raw, ".", 2)
			s := strings.TrimSpace(strings.Trim(parts[0], `"'`))
			if isSafeSchemaName(s) && !isSystemSchema(s) {
				explicitSchemas[s] = struct{}{}
			}
			continue
		}
		// Table-only reference
		table := strings.TrimSpace(strings.Trim(raw, `"'`))
		if table != "" {
			tableNames[table] = struct{}{}
		}
	}

	out := make([]string, 0, 4)
	seen := map[string]struct{}{}
	for s := range explicitSchemas {
		seen[s] = struct{}{}
		out = append(out, s)
	}

	// If SQL already uses explicit schemas, we still may want to include them in search_path,
	// but it's not required. For unqualified tables, try to find a single non-public schema.
	for table := range tableNames {
		// Prefer non-public schema if public doesn't contain the table.
		// If the table exists in multiple non-system schemas, don't guess.
		rows, err := q.QueryContext(ctx, `
			SELECT table_schema
			FROM information_schema.tables
			WHERE table_name = $1
			  AND table_schema NOT IN ('pg_catalog', 'information_schema')
			ORDER BY CASE WHEN table_schema = 'public' THEN 1 ELSE 0 END, table_schema
		`, table)
		if err != nil {
			continue
		}
		schemas, err := func() ([]string, error) {
			defer rows.Close()
			out := []string{}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					return nil, err
				}
				s = strings.TrimSpace(s)
				if s != "" && isSafeSchemaName(s) && !isSystemSchema(s) {
					out = append(out, s)
				}
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			return out, nil
		}()
		if err != nil {
			continue
		}
		if len(schemas) == 1 {
			s := schemas[0]
			if s != "public" {
				if _, ok := seen[s]; !ok {
					seen[s] = struct{}{}
					out = append(out, s)
				}
			}
		}
	}

	// Keep deterministic order to avoid surprising behavior.
	sort.Strings(out)
	return out
}

func isSystemSchema(s string) bool {
	ss := strings.ToLower(strings.TrimSpace(s))
	return ss == "pg_catalog" || ss == "information_schema" || strings.HasPrefix(ss, "pg_")
}

func isSafeSchemaName(s string) bool {
	// Conservative: schema names we inject into SET search_path must be simple.
	// (We still quote them, but keep this guard to avoid oddities.)
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(strings.TrimSpace(s))
}

func quotePGIdent(s string) string {
	// PostgreSQL identifier quoting: double quotes, with "" escaping.
	v := strings.ReplaceAll(strings.TrimSpace(s), `"`, `""`)
	return `"` + v + `"`
}
