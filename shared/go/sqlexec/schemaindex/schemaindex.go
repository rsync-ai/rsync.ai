// Package schemaindex holds the shape of a connection's schema index: the tables, columns
// and foreign keys the schema readers return. It imports no driver, so the gateway's cache
// can hold an index without linking the engines that read one.
package schemaindex

import "strings"

// Index is what a schema reader returns: the tables, and the foreign keys it could read.
type Index struct {
	Tables      []Table
	ForeignKeys []ForeignKey
}

// ForeignKey represents a foreign key relationship in the schema index.
type ForeignKey struct {
	FromSchema string `json:"from_schema,omitempty"`
	FromTable  string `json:"from_table"`
	FromColumn string `json:"from_column"`
	ToSchema   string `json:"to_schema,omitempty"`
	ToTable    string `json:"to_table"`
	ToColumn   string `json:"to_column"`
	// Confidence is 1.0 for explicit FK constraints; <1.0 for inferred relationships (Phase 3.5).
	Confidence float64 `json:"confidence,omitempty"`
}

// Table represents a table in the schema index
type Table struct {
	Name       string   `json:"name"`
	Schema     string   `json:"schema,omitempty"`
	RowCount   int64    `json:"row_count,omitempty"`
	Columns    []Column `json:"columns"`
	PrimaryKey []string `json:"primary_key,omitempty"`
	// Compact searchable tokens for retrieval
	SearchTokens string `json:"search_tokens"`
}

// Column represents a column in the table index
type Column struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	IsPrimaryKey bool   `json:"is_primary_key,omitempty"`
	IsNullable   bool   `json:"is_nullable,omitempty"`
}

// SearchTokens creates searchable tokens for a table (for retrieval)
func SearchTokens(table Table) string {
	var tokens []string

	// Table name tokens
	tokens = append(tokens, strings.ToLower(table.Name))
	// Split by underscore and camelCase
	tokens = append(tokens, splitToTokens(table.Name)...)

	// Column name tokens
	for _, col := range table.Columns {
		tokens = append(tokens, strings.ToLower(col.Name))
		tokens = append(tokens, splitToTokens(col.Name)...)
	}

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}

	return strings.Join(unique, " ")
}

// splitToTokens splits a name into searchable tokens
func splitToTokens(name string) []string {
	var tokens []string

	// Split by underscore
	parts := strings.Split(strings.ToLower(name), "_")
	tokens = append(tokens, parts...)

	// Only snake_case is split. Identifiers reaching this cache are snake_case by
	// convention across the warehouses it serves, so camelCase splitting has never
	// been needed; add it here if a source ever ships camelCase identifiers.

	return tokens
}
