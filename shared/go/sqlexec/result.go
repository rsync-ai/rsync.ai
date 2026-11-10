package sqlexec

import "database/sql"

// Result represents the query execution response
type Result struct {
	Columns         []string                 `json:"columns"`
	Rows            []map[string]interface{} `json:"rows"`
	RowCount        int                      `json:"row_count"`
	ExecutionTimeMs int64                    `json:"execution_time_ms"`
	Truncated       bool                     `json:"truncated"`
	Warnings        []string                 `json:"warnings,omitempty"`
	// RowsAffected is set only for write statements (INSERT/UPDATE/DELETE/DDL) and
	// carries the number of rows the statement changed. nil for reads and for engines
	// that don't report an affected-row count. Pointer so reads omit it entirely.
	RowsAffected *int64 `json:"rows_affected,omitempty"`
	// StatementType echoes the classified statement (SELECT, INSERT, DROP, …) so the
	// UI can render a write result ("42 rows affected") vs a result grid.
	StatementType string `json:"statement_type,omitempty"`
	// ColumnTypes is each column's type as the database declared it, by Columns' index
	// (VARCHAR, NUMERIC, INT8): a value alone does not say it, since Postgres and MySQL
	// return a NUMERIC as text. Empty for an engine whose connector reports none (a
	// delegated read); never sent on.
	ColumnTypes []string `json:"-"`
}

// declaredTypes is each of a read's columns' declared type, by index; nil when the
// driver reports none.
func declaredTypes(rows *sql.Rows) []string {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil
	}
	out := make([]string, len(cts))
	for i, ct := range cts {
		out[i] = ct.DatabaseTypeName()
	}
	return out
}
