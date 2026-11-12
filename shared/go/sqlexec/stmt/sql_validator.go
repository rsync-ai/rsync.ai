package stmt

import (
	"fmt"
	"regexp"
	"strings"
)

// SQLValidationResult contains the result of SQL validation
type SQLValidationResult struct {
	Valid            bool     `json:"valid"`
	StatementType    string   `json:"statement_type"`
	ErrorCode        string   `json:"error_code,omitempty"`
	ErrorMessage     string   `json:"error_message,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	HasLimit         bool     `json:"has_limit"`
	LimitValue       int      `json:"limit_value,omitempty"`
	ReferencedTables []string `json:"referenced_tables,omitempty"`
}

// SQLValidationError codes
const (
	ErrCodeNotSelectOnly     = "NOT_SELECT_ONLY"
	ErrCodeMultipleStatements = "MULTIPLE_STATEMENTS"
	ErrCodeDangerousKeyword  = "DANGEROUS_KEYWORD"
	ErrCodeEmptyQuery        = "EMPTY_QUERY"
	ErrCodeSQLInjection      = "SQL_INJECTION"
)

// ValidateExplorerSQL validates SQL for Explorer query execution
// Returns detailed validation result with error classification
func ValidateExplorerSQL(sql string) *SQLValidationResult {
	result := &SQLValidationResult{
		Valid:    true,
		Warnings: []string{},
	}

	// Step 1: Basic cleanup
	sql = strings.TrimSpace(sql)
	if sql == "" {
		result.Valid = false
		result.ErrorCode = ErrCodeEmptyQuery
		result.ErrorMessage = "Query cannot be empty"
		return result
	}

	rawSQL := sql

	// Step 2: Remove comments (single-line and multi-line)
	sql = removeComments(sql)
	sql = strings.TrimSpace(sql)

	if sql == "" {
		result.Valid = false
		result.ErrorCode = ErrCodeEmptyQuery
		result.ErrorMessage = "Query cannot be empty (only comments)"
		return result
	}

	// Step 3: Check for multiple statements
	if hasMultipleStatements(sql) {
		result.Valid = false
		result.ErrorCode = ErrCodeMultipleStatements
		result.ErrorMessage = "Multiple SQL statements are not allowed"
		return result
	}

	// Step 4: Identify statement type
	stmtType := identifyStatementType(sql)
	result.StatementType = stmtType

	// Step 5: Only allow SELECT and WITH (for CTEs)
	if stmtType != "SELECT" && stmtType != "WITH" {
		result.Valid = false
		result.ErrorCode = ErrCodeNotSelectOnly
		result.ErrorMessage = fmt.Sprintf("Only SELECT queries are allowed. Found: %s", stmtType)
		return result
	}

	// Step 6: Check for dangerous keywords even within SELECT
	if dangerous, keyword := hasDangerousKeyword(sql); dangerous {
		result.Valid = false
		result.ErrorCode = ErrCodeDangerousKeyword
		result.ErrorMessage = fmt.Sprintf("Query contains dangerous keyword: %s", keyword)
		return result
	}

	// Step 6b: Refuse a read that calls a function acting on the server. Scans the
	// text as sent (rawSQL), not the comment-stripped form; see callsServerFunction.
	if calls, fn := callsServerFunction(rawSQL); calls {
		result.Valid = false
		result.ErrorCode = ErrCodeDangerousKeyword
		result.ErrorMessage = fmt.Sprintf("A read query cannot call %s: it acts on the database server, not on table data", fn)
		return result
	}

	// Step 7: Check for SQL injection patterns
	if hasInjectionPattern(sql) {
		result.Valid = false
		result.ErrorCode = ErrCodeSQLInjection
		result.ErrorMessage = "Query contains suspicious patterns"
		return result
	}

	// Step 8: Extract LIMIT information
	result.HasLimit, result.LimitValue = extractLimit(sql)

	// Step 9: Extract referenced tables
	result.ReferencedTables = extractTables(sql)

	// Step 10: Add warnings
	if !result.HasLimit {
		result.Warnings = append(result.Warnings, "Query has no LIMIT clause - results will be capped")
	}

	if len(result.ReferencedTables) > 5 {
		result.Warnings = append(result.Warnings, "Query references many tables - may be slow")
	}

	return result
}

// removeComments removes SQL comments
func removeComments(sql string) string {
	// SQL-aware comment stripping: remove `-- ...` and `/* ... */` only when not inside
	// single-quoted strings or double-quoted identifiers.
	var out strings.Builder
	out.Grow(len(sql))

	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	inExecComment := false

	for i := 0; i < len(sql); i++ {
		ch := sql[i]

		// End line comment at newline (preserve newline to keep statement boundaries reasonable)
		if inLineComment {
			if ch == '\n' {
				inLineComment = false
				out.WriteByte(ch)
			}
			continue
		}

		// End block comment at */
		if inBlockComment {
			if ch == '*' && i+1 < len(sql) && sql[i+1] == '/' {
				inBlockComment = false
				i++ // consume '/'
			}
			continue
		}

		// Inside single-quoted literal
		if inSingle {
			out.WriteByte(ch)
			if ch == '\'' {
				// SQL escaping: '' inside a string is an escaped quote
				if i+1 < len(sql) && sql[i+1] == '\'' {
					out.WriteByte(sql[i+1])
					i++
				} else {
					inSingle = false
				}
			}
			continue
		}

		// Inside double-quoted identifier
		if inDouble {
			out.WriteByte(ch)
			if ch == '"' {
				// Escaped double-quote inside identifier: ""
				if i+1 < len(sql) && sql[i+1] == '"' {
					out.WriteByte(sql[i+1])
					i++
				} else {
					inDouble = false
				}
			}
			continue
		}

		// Not inside any string/comment: detect comment starts
		if ch == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
			i++ // consume second '-'
			continue
		}
		if ch == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			// `/*!` is a MySQL/MariaDB executable comment, optionally version-gated
			// (`/*!40101 …`). Those engines RUN its contents — it is code wearing a
			// comment's syntax. Stripping it hid real statements from every check
			// built on this function: `SELECT 1; /*!40101 DROP TABLE customers */`
			// stripped down to `SELECT 1;`, so the single-statement rule saw one
			// statement and the classifier saw a read, and it validated for a viewer.
			// Keep the body and drop only the delimiters, so what we classify is what
			// the engine executes. PostgreSQL treats `/*!` as an ordinary comment, so
			// retaining the body there can only over-classify, which is the safe way
			// to be wrong.
			if i+2 < len(sql) && sql[i+2] == '!' {
				i += 2 // consume '*' and '!'
				for i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
					i++ // consume the optional version gate
				}
				inExecComment = true
				continue
			}
			inBlockComment = true
			i++ // consume '*'
			continue
		}
		// Closing delimiter of an executable comment: the body was already kept.
		if inExecComment && ch == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inExecComment = false
			i++ // consume '/'
			continue
		}

		// Detect string starts
		if ch == '\'' {
			inSingle = true
			out.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inDouble = true
			out.WriteByte(ch)
			continue
		}

		out.WriteByte(ch)
	}

	return out.String()
}

// hasMultipleStatements reports whether sql contains more than one statement
// (a stacked-statement / SQL-injection guard). A ';' only separates statements
// when it is OUTSIDE a string literal or quoted identifier.
//
// SECURITY: string-boundary tracking MUST match the destination engines'
// standard SQL semantics, NOT C-style backslash escaping. Under PostgreSQL
// (standard_conforming_strings=on, the default since 9.1), Redshift, and
// SQL Server, a backslash is an ORDINARY character and a literal quote is
// written by DOUBLING it (''). A guard that treats \' as an escaped quote
// diverges from the DB: it reads `… WHERE x='a\'; DROP TABLE t; SELECT '1'`
// as one string-laden statement while the DB closes the string at the quote
// and runs the stacked DROP — the exact bypass that let a workspace admin
// (gated to DML/DDL) execute owner-only DROP/TRUNCATE via PostgreSQL's
// multi-statement simple-query protocol. So reuse stripStringLiterals, which masks
// literal/identifier contents with the correct ''-doubling (no backslash
// special) walker; any ';' that survives is a real separator.
func hasMultipleStatements(sql string) bool {
	masked := stripStringLiterals(sql)
	// A single trailing ';' is a terminator, not a second statement.
	masked = strings.TrimSuffix(strings.TrimSpace(masked), ";")
	return strings.Contains(masked, ";")
}

// identifyStatementType identifies the SQL statement type
func identifyStatementType(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	// Common statement type prefixes
	types := []struct {
		prefix string
		name   string
	}{
		{"SELECT", "SELECT"},
		{"WITH", "WITH"},
		{"INSERT", "INSERT"},
		{"UPDATE", "UPDATE"},
		{"DELETE", "DELETE"},
		{"MERGE", "MERGE"},
		{"DROP", "DROP"},
		{"CREATE", "CREATE"},
		{"ALTER", "ALTER"},
		{"TRUNCATE", "TRUNCATE"},
		{"GRANT", "GRANT"},
		{"REVOKE", "REVOKE"},
		{"EXEC", "EXEC"},
		{"EXECUTE", "EXECUTE"},
		{"CALL", "CALL"},
		{"SET", "SET"},
		{"COPY", "COPY"},
		{"VACUUM", "VACUUM"},
		{"ANALYZE", "ANALYZE"},
		{"EXPLAIN", "EXPLAIN"},
		// ClassifyStatement blocks these; without them here the full-statement
		// classifier saw UNKNOWN, which is owner-runnable, not blocked.
		{"SHOW", "SHOW"},
		{"DESCRIBE", "DESCRIBE"},
		{"DESC", "DESC"},
	}

	for _, t := range types {
		if strings.HasPrefix(upper, t.prefix) {
			// Verify it's a word boundary
			if len(upper) == len(t.prefix) || !isAlphanumeric(rune(upper[len(t.prefix)])) {
				return t.name
			}
		}
	}

	return "UNKNOWN"
}

// stripStringLiterals replaces the contents of single-quoted strings,
// double-quoted identifiers, and backtick-quoted identifiers with
// equal-length spaces. Used by hasDangerousKeyword + hasInjectionPattern
// before the regex scan so SQL keywords that appear inside literals
// don't trigger false positives.
//
// Without this, queries like:
//   SELECT 'How do I drop table foo?' AS faq
//   SELECT title FROM articles WHERE body LIKE '%insert into%'
//   SELECT id FROM commits WHERE message ILIKE 'truncate %'
// were being rejected as injection attempts even though the dangerous
// keyword lives inside a string literal — perfectly safe SQL.
//
// Mirrors the SQL-aware walking pattern in removeComments above.
// Preserves source length so any positional / line-column-based
// downstream logic stays accurate.
func stripStringLiterals(sql string) string {
	var out strings.Builder
	out.Grow(len(sql))

	inSingle := false
	inDouble := false
	inBacktick := false

	for i := 0; i < len(sql); i++ {
		ch := sql[i]

		// Inside single-quoted literal
		if inSingle {
			out.WriteByte(' ')
			if ch == '\'' {
				// SQL escape: '' is a literal quote, not end-of-string.
				if i+1 < len(sql) && sql[i+1] == '\'' {
					out.WriteByte(' ')
					i++
				} else {
					inSingle = false
				}
			}
			continue
		}

		// Inside double-quoted identifier
		if inDouble {
			out.WriteByte(' ')
			if ch == '"' {
				if i+1 < len(sql) && sql[i+1] == '"' {
					out.WriteByte(' ')
					i++
				} else {
					inDouble = false
				}
			}
			continue
		}

		// Inside backtick-quoted identifier (MySQL)
		if inBacktick {
			out.WriteByte(' ')
			if ch == '`' {
				inBacktick = false
			}
			continue
		}

		// Detect quote starts. Keep the opening quote so subsequent
		// dangerous-keyword detection can still match queries that
		// genuinely START with a keyword (the literal content is
		// what we're masking, not the quote markers themselves).
		switch ch {
		case '\'':
			inSingle = true
			out.WriteByte(ch)
		case '"':
			inDouble = true
			out.WriteByte(ch)
		case '`':
			inBacktick = true
			out.WriteByte(ch)
		default:
			out.WriteByte(ch)
		}
	}

	return out.String()
}

// hasDangerousKeyword checks for dangerous keywords in the query.
//
// D3 fix: scans the string-literal-stripped form so queries like
// `SELECT 'how do I drop table foo?' AS faq` aren't rejected for
// the word "drop" appearing inside a literal.
func hasDangerousKeyword(sql string) (bool, string) {
	upper := strings.ToUpper(stripStringLiterals(sql))

	// Dangerous keywords that should never appear (even in SELECT)
	dangerous := []string{
		"INSERT INTO",
		"UPDATE ",
		"DELETE FROM",
		// MERGE INTO belongs beside its three siblings above and its absence was
		// load-bearing: a CTE-prefixed MERGE classified as a read, reached this list,
		// and found nothing to stop it. ClassifyStatementSQL now classifies that
		// statement as dml_write so it never gets here — this is the backstop for the
		// next time something reclassifies a write as a read.
		"MERGE INTO",
		"DROP TABLE",
		"DROP DATABASE",
		"DROP SCHEMA",
		"DROP INDEX",
		"DROP VIEW",
		"CREATE TABLE",
		"CREATE DATABASE",
		"ALTER TABLE",
		"TRUNCATE ",
		"GRANT ",
		"REVOKE ",
		"EXEC ",
		"EXECUTE ",
		"CALL ",
		"COPY ",
		"\\COPY",
		"SET ROLE",
		"SET SESSION",
		"IMPORT ",
		"EXPORT ",
		"BACKUP ",
		"RESTORE ",
		"LOAD DATA",
		"INTO OUTFILE",
		"INTO DUMPFILE",
	}

	for _, kw := range dangerous {
		pattern := `\b` + regexp.QuoteMeta(kw)
		if matched, _ := regexp.MatchString(pattern, upper); matched {
			return true, strings.TrimSpace(kw)
		}
	}

	if hasSelectInto(upper) {
		return true, "SELECT INTO"
	}

	return false, ""
}

// selectIntoRe finds INTO anywhere in a read's code (literals and quoted identifiers
// already masked). Every form of it writes: Postgres `SELECT … INTO [TEMP|UNLOGGED]
// name` and SQL Server `SELECT … INTO name` create a table, MySQL `INTO @var` and
// `INTO OUTFILE` assign or write a file. No read needs it, so INTO is refused in any
// form. It used to match only `INTO [TEMP] TABLE`, which let `SELECT * INTO newtbl`
// through as a read (KI-SQL-READ-GATE-LETS-WRITES-THROUGH).
var selectIntoRe = regexp.MustCompile(`(?i)\bINTO\b`)

// hasSelectInto reports whether a read's masked code contains INTO.
func hasSelectInto(maskedSQL string) bool {
	return selectIntoRe.MatchString(maskedSQL)
}

// serverFunctionRe names the functions a read may not call. Each one acts on the
// server instead of reading table data, and a read-only transaction stops none of
// them: it ends sessions, reads or writes server files, opens a second connection
// (which is not read-only), changes a setting (statement_timeout included), takes a
// lock, consumes or drops a CDC replication slot, or runs a query string the gate
// never saw. The names are whole words and case-insensitive. dblink, crosstab,
// advisory locks and stat resets are families, so they match by prefix.
//
// The list stops the known names early with a clear message. It cannot name every
// function (an extension or user-defined function can do anything), which is why
// the read executors also run each read read-only under a server-side timeout.
var serverFunctionRe = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
	// Postgres: server files.
	`pg_read_file`, `pg_read_binary_file`, `pg_write_file`, `pg_stat_file`,
	`pg_ls_dir`, `pg_ls_logdir`, `pg_ls_waldir`, `pg_ls_tmpdir`, `pg_ls_archive_statusdir`,
	`pg_ls_logicalsnapdir`, `pg_ls_logicalmapdir`, `pg_ls_replslotdir`, `pg_logdir_ls`,
	`pg_file_write`, `pg_file_rename`, `pg_file_unlink`, `pg_file_sync`,
	`lo_import`, `lo_export`, `lo_unlink`, `lo_create`, `lo_creat`, `lo_put`,
	`lo_from_bytea`, `lo_truncate`, `lo_truncate64`, `lowrite`,
	// Postgres: sessions and the server process.
	`pg_terminate_backend`, `pg_cancel_backend`, `pg_reload_conf`, `pg_rotate_logfile`,
	`pg_switch_wal`, `pg_switch_xlog`, `pg_promote`, `pg_create_restore_point`,
	`pg_start_backup`, `pg_stop_backup`, `pg_backup_start`, `pg_backup_stop`,
	`pg_wal_replay_pause`, `pg_wal_replay_resume`, `pg_log_backend_memory_contexts`,
	`pg_stat_reset\w*`, `pg_stat_statements_reset`,
	// Postgres: a second connection, or a query string run as SQL.
	`dblink\w*`, `query_to_xml`, `query_to_xmlschema`, `query_to_xml_and_xmlschema`,
	`cursor_to_xml`, `cursor_to_xmlschema`, `ts_stat`, `crosstab\d*`,
	// Postgres: settings, locks, notifications, sequences.
	`set_config`, `pg_(?:try_)?advisory_\w+`, `pg_notify`, `nextval`, `setval`,
	// Postgres: replication slots and origins. Consuming the CDC slot loses data.
	`pg_create_logical_replication_slot`, `pg_create_physical_replication_slot`,
	`pg_copy_logical_replication_slot`, `pg_copy_physical_replication_slot`,
	`pg_drop_replication_slot`, `pg_replication_slot_advance`,
	`pg_logical_slot_get_changes`, `pg_logical_slot_get_binary_changes`,
	`pg_logical_slot_peek_changes`, `pg_logical_slot_peek_binary_changes`,
	`pg_logical_emit_message`, `pg_replication_origin_create`, `pg_replication_origin_drop`,
	`pg_replication_origin_advance`, `pg_replication_origin_session_setup`,
	`pg_replication_origin_session_reset`, `pg_replication_origin_xact_setup`,
	`pg_replication_origin_xact_reset`,
	// MySQL: server files and shell UDFs.
	`load_file`, `sys_exec`, `sys_eval`,
	// SQL Server: another server or data source.
	`openrowset`, `opendatasource`, `openquery`,
	// Oracle: network, files, and a query string run as SQL.
	`utl_http`, `utl_file`, `utl_tcp`, `utl_smtp`, `utl_mail`, `utl_inaddr`,
	`dbms_xmlgen`, `dbms_xmlquery`, `dbms_sql`, `dbms_pipe`, `dbms_lock`,
	`dbms_scheduler`, `dbms_job`, `dbms_java`,
	// BigQuery: a query string run on another database.
	`external_query`,
}, "|") + `)\b` +
	// Snowflake: end other queries or sessions.
	`|(?i)\bsystem\$(?:cancel|abort)\w*` +
	// ClickHouse table functions that reach a URL, a server file, another server or a
	// shell script. These names are ordinary words, so only a call (name + "(") counts.
	`|(?i)\b(?:url|urlCluster|s3|s3Cluster|gcs|file|fileCluster|remote|remoteSecure|` +
	`cluster|clusterAllReplicas|mysql|postgresql|jdbc|odbc|hdfs|hdfsCluster|` +
	`executable|azureBlobStorage|mongodb|redis|sqlite)\s*\(` +
	// A Postgres Unicode-escaped identifier (U&"…\005f…") spells any name, including
	// the ones above, without its letters.
	`|(?i)\bU&"`)

// callsServerFunction reports the first server-acting function a read names.
//
// It scans the statement exactly as the client sent it, comments and literals
// included. A masker has to decide where each string and comment ends, and an
// engine's quoting rules can decide differently, so a call the masker took for string
// content or a comment can still run. A name inside a real literal is refused too
// (`WHERE query LIKE '%dblink%'`); that rare over-match is the safe side.
func callsServerFunction(rawSQL string) (bool, string) {
	m := serverFunctionRe.FindString(rawSQL)
	if m == "" {
		return false, ""
	}
	return true, strings.TrimRight(strings.TrimSpace(m), "( \t\r\n")
}

// hasInjectionPattern checks for common SQL injection patterns.
//
// D3 fix:
//   - keyword/structural patterns scan the string-literal-stripped
//     form so analyst queries like
//     `SELECT title FROM articles WHERE body LIKE '%insert into%'`
//     or `WHERE message ILIKE 'truncate %'` aren't rejected for
//     keywords appearing inside literals.
//   - dropped `UNION\s+(ALL\s+)?SELECT` — legitimate analytic SQL
//     (`SELECT ... UNION ALL SELECT ...`) is common in Explorer.
//   - dropped `concat\s*\(` — `CONCAT(first_name, ' ', last_name)` is
//     standard string building. Real concat-based injection still
//     requires a tautology or stacked statement, caught below.
//
// Tautology patterns intentionally scan the ORIGINAL SQL: in a
// classic `' OR '1'='1` injection the payload IS a string literal,
// so stripping literals would mask the very thing we want to find.
// The tradeoff: a literal that happens to contain `OR 1=1` text
// (a contrived FAQ example) is still blocked. Acceptable.
func hasInjectionPattern(sql string) bool {
	stripped := stripStringLiterals(sql)

	// Patterns scanned against the string-literal-stripped form.
	// These catch attempts to insert structural SQL (DDL/DML, hex,
	// char-encoding) anywhere in the query. Stripping makes sure a
	// keyword inside a quoted literal doesn't trigger them.
	strippedPatterns := []string{
		`;\s*(SELECT|INSERT|UPDATE|DELETE|DROP|CREATE|ALTER)`, // Chained statements
		`--\s*$`,             // Trailing comment
		`;\s*--`,             // Statement + comment
		`\b0x[0-9a-fA-F]+\b`, // Hex-encoding bypass
		`\bchar\s*\(\s*\d+`,  // CHAR(NN) encoding bypass
	}
	for _, pattern := range strippedPatterns {
		if matched, _ := regexp.MatchString("(?i)"+pattern, stripped); matched {
			return true
		}
	}

	// Tautology patterns scanned against the ORIGINAL SQL — the
	// injection payload lives INSIDE the literal, so stripping
	// would mask the signal.
	tautologyPatterns := []string{
		`'\s*OR\s+'?1'?\s*=\s*'?1'?\s*'?`, // ' OR '1'='1'
		`\bOR\s+'?1'?\s*=\s*'?1'?\b`,      // OR '1'='1' / OR 1=1
		`\bOR\s+1\s*=\s*1\b`,              // OR 1=1
		`'\s*OR\s+'?true'?\b`,             // ' OR true
		`\bOR\s+true\b`,                   // OR true
	}
	for _, pattern := range tautologyPatterns {
		if matched, _ := regexp.MatchString("(?i)"+pattern, sql); matched {
			return true
		}
	}

	return false
}

// extractLimit extracts the LIMIT value from the query
func extractLimit(sql string) (bool, int) {
	// Match LIMIT N or LIMIT N OFFSET M
	pattern := regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)
	matches := pattern.FindStringSubmatch(sql)

	if len(matches) > 1 {
		var limit int
		fmt.Sscanf(matches[1], "%d", &limit)
		return true, limit
	}

	return false, 0
}

// extractTables extracts table names from the query
func extractTables(sql string) []string {
	tables := make(map[string]bool)

	// Match FROM table and JOIN table patterns
	patterns := []string{
		`(?i)\bFROM\s+([a-zA-Z_][a-zA-Z0-9_]*(?:\.[a-zA-Z_][a-zA-Z0-9_]*)?)`,
		`(?i)\bJOIN\s+([a-zA-Z_][a-zA-Z0-9_]*(?:\.[a-zA-Z_][a-zA-Z0-9_]*)?)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringSubmatch(sql, -1)
		for _, match := range matches {
			if len(match) > 1 {
				tableName := strings.ToLower(match[1])
				// Skip common SQL keywords that might be matched
				if tableName != "select" && tableName != "where" && tableName != "and" {
					tables[tableName] = true
				}
			}
		}
	}

	result := make([]string, 0, len(tables))
	for t := range tables {
		result = append(result, t)
	}

	return result
}

// isAlphanumeric checks if a character is alphanumeric or underscore
func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// ClampLimit returns SQL with LIMIT clamped to maxLimit
func ClampLimit(sql string, maxLimit int) string {
	hasLimit, existingLimit := extractLimit(sql)

	if !hasLimit {
		// Add LIMIT
		sql = strings.TrimSuffix(strings.TrimSpace(sql), ";")
		return sql + fmt.Sprintf(" LIMIT %d", maxLimit)
	}

	if existingLimit > maxLimit {
		// Replace with clamped value
		pattern := regexp.MustCompile(`(?i)\bLIMIT\s+\d+`)
		return pattern.ReplaceAllString(sql, fmt.Sprintf("LIMIT %d", maxLimit))
	}

	return sql
}

// ClassifyExecutionError classifies database execution errors for better user feedback
func ClassifyExecutionError(err error) (errorType string, suggestion string) {
	errStr := strings.ToLower(err.Error())

	switch {
	// A read runs in a read-only transaction: Postgres "cannot execute … in a read-only
	// transaction" (25006), MySQL 1792 "Cannot execute statement in a READ ONLY
	// transaction". A warehouse connector that refuses a non-read says "runs read-only"
	// (BigQuery: warehouse_adapters.py _read_query).
	case strings.Contains(errStr, "read-only transaction") ||
		strings.Contains(errStr, "read only transaction") ||
		strings.Contains(errStr, "runs read-only"):
		return "permission_denied", "A read runs read-only and cannot change data or the database."

	case strings.Contains(errStr, "does not exist") ||
		strings.Contains(errStr, "relation") && strings.Contains(errStr, "does not exist"):
		return "missing_table_or_column", "The referenced table or column may have been renamed or deleted. Try refreshing the schema."

	case strings.Contains(errStr, "column") && strings.Contains(errStr, "does not exist"):
		return "missing_table_or_column", "The referenced column does not exist. Try refreshing the schema."

	// The server's own timeouts: Postgres "canceling statement due to statement
	// timeout", MySQL 3024 "maximum statement execution time exceeded", MariaDB 1969
	// "max_statement_time exceeded".
	case strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "cancel") ||
		strings.Contains(errStr, "deadline") ||
		strings.Contains(errStr, "execution time exceeded") ||
		strings.Contains(errStr, "max_statement_time exceeded"):
		return "timeout", "Query took too long. Try adding filters or reducing the time range."

	case strings.Contains(errStr, "permission denied") ||
		strings.Contains(errStr, "access denied"):
		return "permission_denied", "You don't have permission to access this data. Contact your administrator."

	case strings.Contains(errStr, "connection") ||
		strings.Contains(errStr, "network") ||
		strings.Contains(errStr, "refused"):
		return "network_or_unavailable", "Database is unavailable. Please try again in a moment."

	// Match both Postgres ("syntax error at or near") and MySQL 1064
	// ("you have an error in your SQL syntax").
	case strings.Contains(errStr, "syntax"):
		return "syntax_error", "The SQL query has a syntax error. Please check the query."

	default:
		return "unknown", "An unexpected error occurred. Please try again."
	}
}
