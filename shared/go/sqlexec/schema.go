package sqlexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/schemaindex"
	log "github.com/sirupsen/logrus"
)

// fetchMySQLForeignKeys fetches FK relationships from MySQL/MariaDB.
func fetchMySQLForeignKeys(ctx context.Context, config map[string]interface{}) ([]schemaindex.ForeignKey, error) {
	host, _ := config["host"].(string)
	port, _ := config["port"]
	user, _ := config["user"].(string)
	password, _ := config["password"].(string)
	database, _ := config["database"].(string)
	database = strings.TrimSpace(database)

	portInt := 3306
	switch v := port.(type) {
	case float64:
		portInt = int(v)
	case int:
		portInt = v
	case string:
		fmt.Sscanf(v, "%d", &portInt)
	}

	tlsMode := resolveMySQLTLSMode(config, host)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&timeout=10s&readTimeout=10s&writeTimeout=10s&tls=%s",
		user, password, host, portInt, database, tlsMode)

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetConnMaxLifetime(30 * time.Second)
	if err := conn.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	// FK relationships from information_schema.key_column_usage
	query := `
		SELECT
			k.TABLE_SCHEMA,
			k.TABLE_NAME,
			k.COLUMN_NAME,
			k.REFERENCED_TABLE_SCHEMA,
			k.REFERENCED_TABLE_NAME,
			k.REFERENCED_COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE k
		WHERE k.REFERENCED_TABLE_NAME IS NOT NULL
		  AND k.TABLE_SCHEMA NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
		  AND k.REFERENCED_TABLE_SCHEMA NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
		  AND (? = '' OR (k.TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_SCHEMA = ?))
		ORDER BY k.TABLE_SCHEMA, k.TABLE_NAME, k.COLUMN_NAME
	`

	rows, err := conn.QueryContext(queryCtx, query, database, database, database)
	if err != nil {
		return nil, fmt.Errorf("failed to query foreign keys: %w", err)
	}
	defer rows.Close()

	var fks []schemaindex.ForeignKey
	for rows.Next() {
		var fromSchema, fromTable, fromCol, toSchema, toTable, toCol string
		if err := rows.Scan(&fromSchema, &fromTable, &fromCol, &toSchema, &toTable, &toCol); err != nil {
			log.Warnf("Failed to scan FK row: %v", err)
			continue
		}
		fks = append(fks, schemaindex.ForeignKey{
			FromSchema: fromSchema,
			FromTable:  fromTable,
			FromColumn: fromCol,
			ToSchema:   toSchema,
			ToTable:    toTable,
			ToColumn:   toCol,
			Confidence: 1.0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}
	return fks, nil
}

// fetchDatabricksSchemaIndex builds a schema index using Databricks SQL over HTTP.
// This is a best-effort MVP:
// - Lists tables via SHOW TABLES
// - Gets columns via DESCRIBE
// - Does not fetch row counts or foreign keys (Databricks generally doesn't expose FKs)
func fetchDatabricksSchemaIndex(ctx context.Context, newClient func(time.Duration) *http.Client, config map[string]interface{}) ([]schemaindex.Table, error) {
	host := normalizeDatabricksHost(getStringConfig(config, "host", "server_hostname", "workspace_host"))
	token := getStringConfig(config, "access_token", "token", "pat")
	warehouseID := getStringConfig(config, "warehouse_id", "warehouseId", "sql_warehouse_id")
	httpPath := getStringConfig(config, "http_path")
	if warehouseID == "" {
		warehouseID = parseWarehouseIDFromHTTPPath(httpPath)
	}
	catalog := strings.TrimSpace(getStringConfig(config, "catalog", "database"))
	schemaName := strings.TrimSpace(getStringConfig(config, "schema", "schema_name"))
	if schemaName == "" {
		schemaName = "default"
	}

	queryCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	qualifiedSchema := schemaName
	if catalog != "" {
		qualifiedSchema = fmt.Sprintf("%s.%s", catalog, schemaName)
	}

	// List tables
	showSQL := fmt.Sprintf("SHOW TABLES IN %s", qualifiedSchema)
	showResp, err := databricksExecuteStatement(queryCtx, newClient, host, token, warehouseID, showSQL, catalog, schemaName)
	if err != nil {
		return nil, err
	}
	if showResp.Result == nil {
		return nil, fmt.Errorf("databricks SHOW TABLES returned no result")
	}

	// Identify table name column
	colIdx := -1
	for i, c := range showResp.Manifest.Schema.Columns {
		n := strings.ToLower(strings.TrimSpace(c.Name))
		if n == "tablename" || n == "table_name" || n == "table" || n == "name" {
			colIdx = i
			break
		}
		if n == "tableName" {
			colIdx = i
			break
		}
	}
	if colIdx < 0 && len(showResp.Manifest.Schema.Columns) > 0 {
		// Spark usually returns database, tableName, isTemporary
		for i, c := range showResp.Manifest.Schema.Columns {
			n := strings.ToLower(strings.TrimSpace(c.Name))
			if strings.Contains(n, "table") && strings.Contains(n, "name") {
				colIdx = i
				break
			}
		}
	}
	if colIdx < 0 {
		return nil, fmt.Errorf("could not locate table name column in SHOW TABLES response")
	}

	tableNames := []string{}
	for _, r := range showResp.Result.DataArray {
		if colIdx < len(r) {
			if s, ok := r[colIdx].(string); ok && strings.TrimSpace(s) != "" {
				tableNames = append(tableNames, strings.TrimSpace(s))
			}
		}
	}

	// Describe each table (cap to avoid massive scans)
	maxTables := 200
	if len(tableNames) > maxTables {
		tableNames = tableNames[:maxTables]
	}

	out := []schemaindex.Table{}
	for _, t := range tableNames {
		fullName := t
		if catalog != "" {
			fullName = fmt.Sprintf("%s.%s.%s", catalog, schemaName, t)
		} else {
			fullName = fmt.Sprintf("%s.%s", schemaName, t)
		}

		descSQL := fmt.Sprintf("DESCRIBE %s", fullName)
		descResp, err := databricksExecuteStatement(queryCtx, newClient, host, token, warehouseID, descSQL, catalog, schemaName)
		if err != nil || descResp.Result == nil {
			// Skip tables we can't describe (permissions etc.)
			continue
		}

		// Identify describe columns: col_name + data_type (Spark typically: col_name, data_type, comment)
		nameIdx := -1
		typeIdx := -1
		for i, c := range descResp.Manifest.Schema.Columns {
			n := strings.ToLower(strings.TrimSpace(c.Name))
			if n == "col_name" || n == "column_name" || n == "name" {
				nameIdx = i
			}
			if n == "data_type" || n == "type" || strings.Contains(n, "data") && strings.Contains(n, "type") {
				typeIdx = i
			}
		}
		if nameIdx < 0 && len(descResp.Manifest.Schema.Columns) > 0 {
			nameIdx = 0
		}
		if typeIdx < 0 && len(descResp.Manifest.Schema.Columns) > 1 {
			typeIdx = 1
		}

		cols := []schemaindex.Column{}
		for _, r := range descResp.Result.DataArray {
			if nameIdx >= len(r) {
				continue
			}
			cn, _ := r[nameIdx].(string)
			cn = strings.TrimSpace(cn)
			if cn == "" || strings.HasPrefix(cn, "#") {
				// Spark DESCRIBE includes section headers like '# Partition Information'
				continue
			}
			ct := ""
			if typeIdx >= 0 && typeIdx < len(r) {
				if s, ok := r[typeIdx].(string); ok {
					ct = strings.TrimSpace(s)
				}
			}
			cols = append(cols, schemaindex.Column{
				Name:       cn,
				Type:       ct,
				IsNullable: true,
			})
		}

		tIdx := schemaindex.Table{
			// Prefer a qualified schema when catalog is present (Unity Catalog).
			// This yields identifiers like <catalog>.<schema>.<table> in the UI/LLM context.
			Schema:   qualifiedSchema,
			Name:     t,
			RowCount: 0,
			Columns:  cols,
		}
		tIdx.SearchTokens = schemaindex.SearchTokens(tIdx)
		out = append(out, tIdx)
	}

	return out, nil
}

// fetchPostgresForeignKeys fetches FK relationships from Postgres.
func fetchPostgresForeignKeys(ctx context.Context, config map[string]interface{}) ([]schemaindex.ForeignKey, error) {
	host, _ := config["host"].(string)
	port, _ := config["port"]
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

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10",
		host, portInt, user, password, database, sslMode)

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetConnMaxLifetime(30 * time.Second)
	if err := conn.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	// pg_catalog, not the information_schema constraint views: those show only
	// tables the user owns (constraint_column_usage) or holds a non-SELECT
	// privilege on, so a read-only user saw no relationships. The privilege
	// filter keeps what the views did right: no table the user cannot read.
	// Columns pair by key position, so a composite key is not cross-joined.
	query := `
		SELECT
			n.nspname,
			c.relname,
			a.attname,
			rn.nspname AS foreign_table_schema,
			rc.relname AS foreign_table_name,
			ra.attname AS foreign_column_name
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_class rc ON rc.oid = con.confrelid
		JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
		CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		JOIN pg_catalog.pg_attribute ra ON ra.attrelid = con.confrelid AND ra.attnum = con.confkey[k.ord]
		WHERE con.contype = 'f'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		  AND rn.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		  AND has_table_privilege(con.conrelid, 'SELECT')
		  AND has_table_privilege(con.confrelid, 'SELECT')
		ORDER BY n.nspname, c.relname, a.attname
	`

	rows, err := conn.QueryContext(queryCtx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query foreign keys: %w", err)
	}
	defer rows.Close()

	var fks []schemaindex.ForeignKey
	for rows.Next() {
		var fromSchema, fromTable, fromCol, toSchema, toTable, toCol string
		if err := rows.Scan(&fromSchema, &fromTable, &fromCol, &toSchema, &toTable, &toCol); err != nil {
			log.Warnf("Failed to scan FK row: %v", err)
			continue
		}
		fks = append(fks, schemaindex.ForeignKey{
			FromSchema: fromSchema,
			FromTable:  fromTable,
			FromColumn: fromCol,
			ToSchema:   toSchema,
			ToTable:    toTable,
			ToColumn:   toCol,
			Confidence: 1.0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	return fks, nil
}

// fetchMySQLSchemaIndex fetches schema metadata from MySQL/MariaDB
func fetchMySQLSchemaIndex(ctx context.Context, config map[string]interface{}) ([]schemaindex.Table, error) {
	host, _ := config["host"].(string)
	port, _ := config["port"]
	user, _ := config["user"].(string)
	password, _ := config["password"].(string)
	database, _ := config["database"].(string)
	database = strings.TrimSpace(database)

	portInt := 3306
	switch v := port.(type) {
	case float64:
		portInt = int(v)
	case int:
		portInt = v
	case string:
		fmt.Sscanf(v, "%d", &portInt)
	}

	tlsMode := resolveMySQLTLSMode(config, host)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&timeout=10s&readTimeout=10s&writeTimeout=10s&tls=%s",
		user, password, host, portInt, database, tlsMode)

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetConnMaxLifetime(30 * time.Second)
	if err := conn.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	// Query tables and columns; filter out system schemas.
	query := `
		SELECT
			t.TABLE_SCHEMA,
			t.TABLE_NAME,
			COALESCE(t.TABLE_ROWS, 0) AS row_count,
			c.COLUMN_NAME,
			c.DATA_TYPE,
			c.IS_NULLABLE,
			CASE WHEN k.COLUMN_NAME IS NOT NULL THEN TRUE ELSE FALSE END AS is_pk,
			c.ORDINAL_POSITION
		FROM information_schema.TABLES t
		LEFT JOIN information_schema.COLUMNS c
			ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		LEFT JOIN information_schema.KEY_COLUMN_USAGE k
			ON c.TABLE_SCHEMA = k.TABLE_SCHEMA AND c.TABLE_NAME = k.TABLE_NAME AND c.COLUMN_NAME = k.COLUMN_NAME
		LEFT JOIN information_schema.TABLE_CONSTRAINTS tc
			ON tc.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA AND tc.CONSTRAINT_NAME = k.CONSTRAINT_NAME
			AND tc.TABLE_SCHEMA = k.TABLE_SCHEMA AND tc.TABLE_NAME = k.TABLE_NAME
			AND tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.TABLE_SCHEMA NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
		  AND (? = '' OR t.TABLE_SCHEMA = ?)
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME, c.ORDINAL_POSITION
	`

	rows, err := conn.QueryContext(queryCtx, query, database, database)
	if err != nil {
		return nil, fmt.Errorf("failed to query schema: %w", err)
	}
	defer rows.Close()

	tableMap := make(map[string]*schemaindex.Table)
	tableOrder := []string{}

	for rows.Next() {
		var schema, tableName string
		var rowCount int64
		var colName, colType sql.NullString
		var isNullable sql.NullString
		var isPK bool
		var ordPos sql.NullInt64

		if err := rows.Scan(&schema, &tableName, &rowCount, &colName, &colType, &isNullable, &isPK, &ordPos); err != nil {
			log.Warnf("Failed to scan row: %v", err)
			continue
		}

		key := schema + "." + tableName
		if _, exists := tableMap[key]; !exists {
			tableMap[key] = &schemaindex.Table{
				Name:     tableName,
				Schema:   schema,
				RowCount: rowCount,
				Columns:  []schemaindex.Column{},
			}
			tableOrder = append(tableOrder, key)
		}

		if colName.Valid {
			tableMap[key].Columns = append(tableMap[key].Columns, schemaindex.Column{
				Name:         colName.String,
				Type:         colType.String,
				IsPrimaryKey: isPK,
				IsNullable:   strings.EqualFold(isNullable.String, "YES"),
			})
			if isPK {
				tableMap[key].PrimaryKey = append(tableMap[key].PrimaryKey, colName.String)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	tables := make([]schemaindex.Table, 0, len(tableMap))
	for _, key := range tableOrder {
		t := tableMap[key]
		t.SearchTokens = schemaindex.SearchTokens(*t)
		tables = append(tables, *t)
	}

	return tables, nil
}

// fetchSQLServerSchemaIndex fetches schema metadata from SQL Server / Azure SQL.
// SQL Server's INFORMATION_SCHEMA.TABLE_SCHEMA is the SCHEMA (dbo, ...), not the
// database — the connection is already scoped to one database, so we list every
// user schema (excluding sys / INFORMATION_SCHEMA). Row counts are reported as 0
// (INFORMATION_SCHEMA has no row-count column; not load-bearing for the index).
func fetchSQLServerSchemaIndex(ctx context.Context, config map[string]interface{}) ([]schemaindex.Table, error) {
	dsn := sqlServerExplorerDSN(config)

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetConnMaxLifetime(30 * time.Second)
	if err := conn.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	query := `
		SELECT
			t.TABLE_SCHEMA,
			t.TABLE_NAME,
			CAST(0 AS BIGINT) AS row_count,
			c.COLUMN_NAME,
			c.DATA_TYPE,
			c.IS_NULLABLE,
			CAST(CASE WHEN k.COLUMN_NAME IS NOT NULL THEN 1 ELSE 0 END AS BIT) AS is_pk,
			c.ORDINAL_POSITION
		FROM INFORMATION_SCHEMA.TABLES t
		LEFT JOIN INFORMATION_SCHEMA.COLUMNS c
			ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		LEFT JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
			ON tc.TABLE_SCHEMA = c.TABLE_SCHEMA AND tc.TABLE_NAME = c.TABLE_NAME
			AND tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE k
			ON k.CONSTRAINT_NAME = tc.CONSTRAINT_NAME AND k.TABLE_SCHEMA = c.TABLE_SCHEMA
			AND k.TABLE_NAME = c.TABLE_NAME AND k.COLUMN_NAME = c.COLUMN_NAME
		WHERE t.TABLE_TYPE = 'BASE TABLE'
		  AND t.TABLE_SCHEMA NOT IN ('sys', 'INFORMATION_SCHEMA')
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME, c.ORDINAL_POSITION
	`

	rows, err := conn.QueryContext(queryCtx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query schema: %w", err)
	}
	defer rows.Close()

	tableMap := make(map[string]*schemaindex.Table)
	tableOrder := []string{}

	for rows.Next() {
		var schema, tableName string
		var rowCount int64
		var colName, colType sql.NullString
		var isNullable sql.NullString
		var isPK bool
		var ordPos sql.NullInt64

		if err := rows.Scan(&schema, &tableName, &rowCount, &colName, &colType, &isNullable, &isPK, &ordPos); err != nil {
			log.Warnf("Failed to scan row: %v", err)
			continue
		}

		key := schema + "." + tableName
		if _, exists := tableMap[key]; !exists {
			tableMap[key] = &schemaindex.Table{
				Name:     tableName,
				Schema:   schema,
				RowCount: rowCount,
				Columns:  []schemaindex.Column{},
			}
			tableOrder = append(tableOrder, key)
		}

		if colName.Valid {
			tableMap[key].Columns = append(tableMap[key].Columns, schemaindex.Column{
				Name:         colName.String,
				Type:         colType.String,
				IsPrimaryKey: isPK,
				IsNullable:   strings.EqualFold(isNullable.String, "YES"),
			})
			if isPK {
				tableMap[key].PrimaryKey = append(tableMap[key].PrimaryKey, colName.String)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	tables := make([]schemaindex.Table, 0, len(tableMap))
	for _, key := range tableOrder {
		t := tableMap[key]
		t.SearchTokens = schemaindex.SearchTokens(*t)
		tables = append(tables, *t)
	}

	return tables, nil
}

// fetchPostgresSchemaIndex fetches schema metadata from PostgreSQL
func fetchPostgresSchemaIndex(ctx context.Context, config map[string]interface{}) ([]schemaindex.Table, error) {
	// Build DSN
	host, _ := config["host"].(string)
	port, _ := config["port"]
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

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10",
		host, portInt, user, password, database, sslMode)

	// Open connection
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	conn.SetConnMaxLifetime(30 * time.Second)

	if err := conn.PingContext(queryCtx); err != nil {
		return nil, fmt.Errorf("connection ping failed: %w", err)
	}

	// Query tables with columns
	query := `
		WITH table_info AS (
			SELECT 
				t.table_schema,
				t.table_name,
				pg_catalog.obj_description(
					(quote_ident(t.table_schema) || '.' || quote_ident(t.table_name))::regclass, 'pg_class'
				) as table_comment
			FROM information_schema.tables t
			WHERE t.table_schema NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
			  AND t.table_type = 'BASE TABLE'
		),
		column_info AS (
			SELECT 
				c.table_schema,
				c.table_name,
				c.column_name,
				c.data_type,
				c.is_nullable,
				c.ordinal_position,
				CASE WHEN pk.column_name IS NOT NULL THEN true ELSE false END as is_pk
			FROM information_schema.columns c
			-- pg_catalog, not information_schema.table_constraints: that view
			-- hides tables the user neither owns nor holds a non-SELECT
			-- privilege on, so a read-only user saw no primary keys.
			LEFT JOIN (
				SELECT n.nspname AS table_schema, cl.relname AS table_name, a.attname AS column_name
				FROM pg_catalog.pg_constraint con
				JOIN pg_catalog.pg_class cl ON cl.oid = con.conrelid
				JOIN pg_catalog.pg_namespace n ON n.oid = cl.relnamespace
				JOIN pg_catalog.pg_attribute a
					ON a.attrelid = con.conrelid AND a.attnum = ANY(con.conkey)
				WHERE con.contype = 'p'
			) pk ON c.table_schema = pk.table_schema
				AND c.table_name = pk.table_name 
				AND c.column_name = pk.column_name
			WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		),
		row_counts AS (
			SELECT 
				schemaname as table_schema,
				relname as table_name,
				n_live_tup as row_count
			FROM pg_stat_user_tables
		)
		SELECT 
			ti.table_schema,
			ti.table_name,
			COALESCE(rc.row_count, 0) as row_count,
			ci.column_name,
			ci.data_type,
			ci.is_nullable,
			ci.is_pk,
			ci.ordinal_position
		FROM table_info ti
		LEFT JOIN column_info ci ON ti.table_schema = ci.table_schema AND ti.table_name = ci.table_name
		LEFT JOIN row_counts rc ON ti.table_schema = rc.table_schema AND ti.table_name = rc.table_name
		ORDER BY ti.table_schema, ti.table_name, ci.ordinal_position
	`

	rows, err := conn.QueryContext(queryCtx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query schema: %w", err)
	}
	defer rows.Close()

	// Build table index
	tableMap := make(map[string]*schemaindex.Table)
	tableOrder := []string{}

	for rows.Next() {
		var schema, tableName string
		var rowCount int64
		var colName, colType sql.NullString
		var isNullable string
		var isPK bool
		var ordPos int

		if err := rows.Scan(&schema, &tableName, &rowCount, &colName, &colType, &isNullable, &isPK, &ordPos); err != nil {
			log.Warnf("Failed to scan row: %v", err)
			continue
		}

		key := schema + "." + tableName
		if _, exists := tableMap[key]; !exists {
			tableMap[key] = &schemaindex.Table{
				Name:     tableName,
				Schema:   schema,
				RowCount: rowCount,
				Columns:  []schemaindex.Column{},
			}
			tableOrder = append(tableOrder, key)
		}

		if colName.Valid {
			tableMap[key].Columns = append(tableMap[key].Columns, schemaindex.Column{
				Name:         colName.String,
				Type:         colType.String,
				IsPrimaryKey: isPK,
				IsNullable:   isNullable == "YES",
			})

			if isPK {
				tableMap[key].PrimaryKey = append(tableMap[key].PrimaryKey, colName.String)
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	// Convert to slice and build search tokens
	tables := make([]schemaindex.Table, 0, len(tableMap))
	for _, key := range tableOrder {
		t := tableMap[key]
		t.SearchTokens = schemaindex.SearchTokens(*t)
		tables = append(tables, *t)
	}

	return tables, nil
}
