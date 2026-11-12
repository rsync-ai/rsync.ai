package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"api-gateway/internal/cache"
	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	log "github.com/sirupsen/logrus"
)

// ============================================================================
// Silver models: the deterministic clean copy of a bronze table
// ============================================================================
// Bronze is what a pipeline writes, and two of its shapes are not yet a table anyone
// should query:
//
//   - history (CDC append mode): every insert, update and delete is its own row,
//     tagged _rsync_cdc_op and ordered by _rsync_cdc_seq — fixed width, so the
//     greatest value per key is that key's latest change (kafka-sink-worker cdcSeq).
//   - soft delete (warehouse current tables): one row per key, but a deleted key
//     keeps its row with _rsync_deleted = true.
//
// Silver answers "what is there now": the latest version of each key, deleted keys
// gone, rsync.ai's own _rsync_ columns dropped. Once the shape and the key are known that
// query is mechanical, so it is generated here from the catalog — names and types,
// never a row, and never by a language model. A current-state table with nothing to
// clean gets a plain select of its columns, so every bronze table gets a silver model
// the same way.
//
// This file only writes the query. It becomes a model through the ordinary routes
// (CreateSavedQuery, then PUT …/materialization with layer silver), so every check a
// model gets — the admin gate, the rule against taking over a table rsync.ai did not
// create — applies to it unchanged.

const (
	silverShapeHistory    = "history"
	silverShapeSoftDelete = "soft_delete"
	silverShapeCurrent    = "current"

	bronzeColumnPrefix  = "_rsync_"
	bronzeSeqColumn     = "_rsync_cdc_seq"
	bronzeOpColumn      = "_rsync_cdc_op"
	bronzeDeletedColumn = "_rsync_deleted"

	// silverDocumentDialect stands in for a dialect on a document store, whose silver
	// model is an aggregation pipeline rather than SQL.
	silverDocumentDialect = "document"

	// silverRowNumber is the window column the history query adds and then filters on.
	// A source column of the same name would collide with it inside the subquery.
	silverRowNumber = "rsync_rn"

	// silverTargetSuffix names the suggested silver table after its bronze one.
	silverTargetSuffix = "_silver"

	// silverCatalogRowLimit bounds a warehouse catalog read: one row per column.
	silverCatalogRowLimit = 2000

	silverMaxTableArg = 300
	silverMaxKeys     = 32
	silverMaxKeyLen   = 128
)

// silverColumn is one column as the source catalog spells it.
type silverColumn struct {
	Name string
	Type string
}

// silverSource is one bronze table as its catalog describes it. Schema is the
// catalog's own spelling ("" when the engine has none); on Databricks it is
// catalog.schema, on a document store the database.
type silverSource struct {
	Schema  string
	Table   string
	Columns []silverColumn
}

// silverPlan is the generated model, as GetSilverSQL returns it.
type silverPlan struct {
	Shape       string   `json:"shape"`
	Keys        []string `json:"keys,omitempty"`
	Columns     []string `json:"columns"`
	SQLText     string   `json:"sql_text"`
	TargetTable string   `json:"target_table,omitempty"`
}

// silverRefusal is a request the generator cannot serve. Message names columns, types
// and arguments by position — never a value, and never text the caller typed.
type silverRefusal struct {
	Status  int
	Code    string
	Message string
}

func (r *silverRefusal) Error() string { return r.Message }

func refuseSilver(status int, code, message string) *silverRefusal {
	return &silverRefusal{Status: status, Code: code, Message: message}
}

// buildSilverPlan decides the shape from the columns and writes the query. dialect is
// a modelDialect value or silverDocumentDialect. keys is used only on a history table,
// which has no constraint to read its business key from: the append table stores every
// version of a row, so any key it carries is per change, not per row.
func buildSilverPlan(dialect string, src *silverSource, keys []string) (*silverPlan, *silverRefusal) {
	byLower := make(map[string]silverColumn, len(src.Columns))
	var kept []silverColumn
	var bookkeeping []string
	for _, c := range src.Columns {
		lower := strings.ToLower(c.Name)
		if _, dup := byLower[lower]; dup {
			return nil, refuseSilver(http.StatusUnprocessableEntity, "ambiguous_column",
				fmt.Sprintf("the table has two columns named %s apart from letter case, which a generated query cannot tell apart", c.Name))
		}
		byLower[lower] = c
		if strings.HasPrefix(lower, bronzeColumnPrefix) {
			bookkeeping = append(bookkeeping, c.Name)
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil, refuseSilver(http.StatusUnprocessableEntity, "no_columns",
			"the table has no columns besides rsync.ai's own _rsync_ bookkeeping")
	}

	seq, hasSeq := byLower[bronzeSeqColumn]
	op, hasOp := byLower[bronzeOpColumn]
	deleted, hasDeleted := byLower[bronzeDeletedColumn]
	shape := silverShapeCurrent
	switch {
	case hasSeq && hasOp:
		shape = silverShapeHistory
	case hasDeleted:
		shape = silverShapeSoftDelete
	}

	var keyCols []silverColumn
	if shape == silverShapeHistory {
		if len(keys) == 0 {
			return nil, refuseSilver(http.StatusUnprocessableEntity, "keys_required",
				"this is a CDC history table — every change is its own row — so silver needs the business key "+
					"to keep each key's latest change: pass keys, the source table's primary key "+
					"(describe_schema on the pipeline's source connection shows it)")
		}
		seen := map[string]bool{}
		for i, k := range keys {
			lower := strings.ToLower(strings.TrimSpace(k))
			c, ok := byLower[lower]
			if !ok || strings.HasPrefix(lower, bronzeColumnPrefix) {
				return nil, refuseSilver(http.StatusUnprocessableEntity, "unknown_key",
					fmt.Sprintf("keys[%d] is not one of the table's own columns", i))
			}
			if seen[lower] {
				return nil, refuseSilver(http.StatusUnprocessableEntity, "duplicate_key",
					fmt.Sprintf("keys[%d] repeats an earlier key", i))
			}
			seen[lower] = true
			keyCols = append(keyCols, c)
		}
	}

	plan := &silverPlan{Shape: shape}
	for _, c := range kept {
		plan.Columns = append(plan.Columns, c.Name)
	}
	for _, c := range keyCols {
		plan.Keys = append(plan.Keys, c.Name)
	}

	var err *silverRefusal
	if dialect == silverDocumentDialect {
		plan.SQLText, err = silverDocumentQuery(src, shape, keyCols, seq, op, deleted, bookkeeping)
	} else {
		plan.SQLText, err = silverSQL(dialect, src, shape, kept, keyCols, seq, op, deleted)
	}
	if err != nil {
		return nil, err
	}
	plan.TargetTable = suggestSilverTarget(dialect, src)
	return plan, nil
}

// silverSQL writes the query for one SQL dialect. Table aliases carry no AS, which
// Oracle does not accept there; every name is quoted as the catalog spells it, so a
// mixed-case column on Snowflake or Postgres is the column that exists.
func silverSQL(dialect string, src *silverSource, shape string, kept, keys []silverColumn, seq, op, deleted silverColumn) (string, *silverRefusal) {
	from, err := silverSourceRef(dialect, src)
	if err != nil {
		return "", err
	}
	cols := make([]string, 0, len(kept))
	for _, c := range kept {
		if strings.EqualFold(c.Name, silverRowNumber) && shape == silverShapeHistory {
			return "", refuseSilver(http.StatusUnprocessableEntity, "reserved_column",
				"the table has a column named "+silverRowNumber+", the name the generated query uses for its row number")
		}
		q, err := quoteSilverIdent(dialect, c.Name)
		if err != nil {
			return "", err
		}
		cols = append(cols, q)
	}
	selectList := strings.Join(cols, ", ")

	switch shape {
	case silverShapeHistory:
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			q, err := quoteSilverIdent(dialect, k.Name)
			if err != nil {
				return "", err
			}
			parts = append(parts, q)
		}
		seqQ, err := quoteSilverIdent(dialect, seq.Name)
		if err != nil {
			return "", err
		}
		opQ, err := quoteSilverIdent(dialect, op.Name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SELECT %s\nFROM (\n  SELECT t.*, ROW_NUMBER() OVER (PARTITION BY %s ORDER BY %s DESC) AS %s\n  FROM %s t\n) rsync_latest\n"+
			"WHERE rsync_latest.%s = 1\n  AND (rsync_latest.%s IS NULL OR rsync_latest.%s <> 'D')",
			selectList, strings.Join(parts, ", "), seqQ, silverRowNumber, from,
			silverRowNumber, opQ, opQ), nil

	case silverShapeSoftDelete:
		delQ, err := quoteSilverIdent(dialect, deleted.Name)
		if err != nil {
			return "", err
		}
		var notDeleted string
		switch silverTypeClass(deleted.Type) {
		case "bool":
			notDeleted = "COALESCE(" + delQ + ", FALSE) = FALSE"
		case "number":
			notDeleted = "COALESCE(" + delQ + ", 0) = 0"
		default:
			return "", refuseSilver(http.StatusUnprocessableEntity, "unsupported_delete_flag",
				fmt.Sprintf("%s is a %s column; silver reads it only as a boolean or a 0/1 number", deleted.Name, silverTypeLabel(deleted.Type)))
		}
		return fmt.Sprintf("SELECT %s\nFROM %s\nWHERE %s", selectList, from, notDeleted), nil

	default:
		return fmt.Sprintf("SELECT %s\nFROM %s", selectList, from), nil
	}
}

// silverDocumentQuery writes a document store's silver model: the stored
// documentModelQuery shape, holding an aggregation pipeline.
func silverDocumentQuery(src *silverSource, shape string, keys []silverColumn, seq, op, deleted silverColumn, bookkeeping []string) (string, *silverRefusal) {
	var stages []map[string]interface{}
	switch shape {
	case silverShapeHistory:
		group := map[string]interface{}{}
		for i, k := range keys {
			if strings.HasPrefix(k.Name, "$") {
				return "", refuseSilver(http.StatusUnprocessableEntity, "unsupported_field",
					fmt.Sprintf("keys[%d] starts with $, which a pipeline cannot reference", i))
			}
			group[fmt.Sprintf("k%d", i)] = "$" + k.Name
		}
		stages = append(stages,
			map[string]interface{}{"$sort": map[string]interface{}{seq.Name: -1}},
			map[string]interface{}{"$group": map[string]interface{}{
				"_id":          group,
				"rsync_latest": map[string]interface{}{"$first": "$$ROOT"},
			}},
			map[string]interface{}{"$replaceRoot": map[string]interface{}{"newRoot": "$rsync_latest"}},
			map[string]interface{}{"$match": map[string]interface{}{op.Name: map[string]interface{}{"$ne": "D"}}},
		)
	case silverShapeSoftDelete:
		stages = append(stages, map[string]interface{}{"$match": map[string]interface{}{deleted.Name: map[string]interface{}{"$ne": true}}})
	}
	// Only top-level fields: a dotted name in the sample is a path inside a document.
	var unset []string
	for _, name := range bookkeeping {
		if !strings.Contains(name, ".") {
			unset = append(unset, name)
		}
	}
	if len(unset) > 0 {
		stages = append(stages, map[string]interface{}{"$unset": unset})
	}
	if len(stages) == 0 {
		stages = append(stages, map[string]interface{}{"$match": map[string]interface{}{}})
	}
	pipeline, err := json.Marshal(stages)
	if err != nil {
		return "", refuseSilver(http.StatusInternalServerError, "internal", "failed to write the pipeline")
	}
	text, err := json.Marshal(documentModelQuery{Database: src.Schema, Collection: src.Table, Pipeline: pipeline})
	if err != nil {
		return "", refuseSilver(http.StatusInternalServerError, "internal", "failed to write the pipeline")
	}
	// The same validation a saved pipeline gets when it runs, so a generated one that
	// the model runner would refuse never leaves here.
	if _, ferr := parseDocumentModelQuery(string(text)); ferr != nil {
		return "", refuseSilver(http.StatusUnprocessableEntity, "unsupported_pipeline", ferr.Message)
	}
	return string(text), nil
}

// quoteSilverIdent quotes one catalog name for a dialect, exactly as spelled: unlike
// quoteModelIdent, which names a table rsync.ai is about to create, this names one that
// already exists, so folding its case would name a different object. A name holding
// the quote character, a backslash or a control character is refused rather than
// escaped — catalog names that need either are rare enough to be worth a clear "no".
func quoteSilverIdent(dialect, name string) (string, *silverRefusal) {
	open, close := `"`, `"`
	switch dialect {
	case "mysql", "bigquery", "databricks", "clickhouse":
		open, close = "`", "`"
	case "sqlserver":
		open, close = "[", "]"
	}
	bad := name == "" || strings.ContainsAny(name, `\`+open+close) ||
		strings.IndexFunc(name, unicode.IsControl) >= 0
	if bad {
		return "", refuseSilver(http.StatusUnprocessableEntity, "unsupported_name",
			"a table or column name holds a character the generated query cannot quote")
	}
	return open + name + close, nil
}

// silverSourceRef renders the source table, qualified. Databricks (catalog.schema) and
// BigQuery (project.dataset) carry a dotted schema; every other engine's schema is one
// name, which a dot inside it does not split.
func silverSourceRef(dialect string, src *silverSource) (string, *silverRefusal) {
	var parts []string
	if src.Schema != "" {
		if dialect == "databricks" || dialect == "bigquery" {
			parts = strings.Split(src.Schema, ".")
		} else {
			parts = []string{src.Schema}
		}
	}
	parts = append(parts, src.Table)
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		q, err := quoteSilverIdent(dialect, p)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, q)
	}
	return strings.Join(quoted, "."), nil
}

// silverTypeClass reads a catalog type as "bool", "number" or "" (anything else). It
// compares the base name only — "tinyint(1)", "Nullable(UInt8)", "NUMBER(1,0)" — and
// never a prefix, which would read INTERVAL as an int.
func silverTypeClass(t string) string {
	base := strings.ToLower(strings.TrimSpace(t))
	base = strings.TrimPrefix(base, "nullable(")
	if end := strings.IndexFunc(base, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}); end >= 0 {
		base = base[:end]
	}
	switch base {
	case "bool", "boolean":
		return "bool"
	case "bit", "tinyint", "smallint", "mediumint", "int", "integer", "bigint",
		"int2", "int4", "int8", "int16", "int32", "int64",
		"uint8", "uint16", "uint32", "uint64", "number", "numeric", "decimal":
		return "number"
	}
	return ""
}

// silverTypeLabel is a catalog type safe to put in a message: the type's own letters
// and digits, capped. A type is metadata, but it is still text a catalog returned.
func silverTypeLabel(t string) string {
	var b strings.Builder
	for _, r := range t {
		if b.Len() >= 40 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '(' || r == ')' || r == ',' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "differently typed"
	}
	return b.String()
}

// suggestSilverTarget names the silver table <schema>.<table>_silver beside its bronze
// one, or "" when that is not a target validateModelTarget accepts (a dotted Databricks
// schema, a name too long for the suffix) — the caller then names one.
func suggestSilverTarget(dialect string, src *silverSource) string {
	target := src.Table + silverTargetSuffix
	if src.Schema != "" {
		target = src.Schema + "." + target
	}
	if _, _, err := validateModelTarget(target); err != nil {
		return ""
	}
	return target
}

// ---------------------------------------------------------------------------
// Finding the table's columns
// ---------------------------------------------------------------------------

// silverSchemaIndex reads the Explorer's schema index — the engines it covers, and
// document stores — from the cache, or builds and caches it. A package var so a test
// can stand in for the database.
var silverSchemaIndex = func(ctx context.Context, connectionID, workspaceID, connectorType, configEncrypted string, refresh bool) (*cache.ExplorerSchemaIndex, error) {
	cacheID := schemaIndexCacheID(connectionID, false)
	if !refresh && explorerCache != nil {
		if cached, err := explorerCache.GetSchemaIndex(ctx, cacheID); err == nil && cached != nil {
			return cached, nil
		}
	}
	idx, err := buildSchemaIndex(ctx, connectionID, workspaceID, connectorType, configEncrypted, false)
	if err != nil {
		return nil, err
	}
	if explorerCache != nil {
		cp := *idx
		cp.ConnectionID = cacheID
		if err := explorerCache.SetSchemaIndex(ctx, &cp); err != nil {
			log.Warnf("[silver] failed to cache schema index: %v", err)
		}
	}
	return idx, nil
}

// silverCatalogQuery runs a catalog read on a warehouse the schema index does not
// cover. A package var so a test can stand in for the orchestrator.
var silverCatalogQuery = func(ctx context.Context, connectorType string, config map[string]interface{}, query string) ([]map[string]interface{}, error) {
	resp, err := explorerOrchestrator().ExplorerQuery(ctx, connectorType, config, query, silverCatalogRowLimit, false, false)
	if err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// silverUsesIndex reports whether the schema index lists this dialect's tables with
// their columns (buildSchemaIndex); the other warehouses are read from their catalog.
func silverUsesIndex(dialect string) bool {
	switch dialect {
	case "postgres", "mysql", "sqlserver", "databricks", silverDocumentDialect:
		return true
	}
	return false
}

// silverTableMatches reports whether an index entry is the table the caller named, as
// "table", "schema.table" or, on Databricks, "schema.table" without the catalog.
// Compared whole, so a dot inside a collection name is part of the name.
func silverTableMatches(t cache.ExplorerTableIndex, want string, exact bool) bool {
	eq := strings.EqualFold
	if exact {
		eq = func(a, b string) bool { return a == b }
	}
	if eq(t.Name, want) {
		return true
	}
	if t.Schema == "" {
		return false
	}
	if eq(t.Schema+"."+t.Name, want) {
		return true
	}
	if dot := strings.LastIndex(t.Schema, "."); dot >= 0 && eq(t.Schema[dot+1:]+"."+t.Name, want) {
		return true
	}
	return false
}

// findSilverTable picks the caller's table from an index. A name that matches more
// than one table only apart from letter case resolves to the exact spelling, if one is.
func findSilverTable(tables []cache.ExplorerTableIndex, want string) (*cache.ExplorerTableIndex, *silverRefusal) {
	var matches []int
	for i := range tables {
		if silverTableMatches(tables[i], want, false) {
			matches = append(matches, i)
		}
	}
	if len(matches) > 1 {
		var exact []int
		for _, i := range matches {
			if silverTableMatches(tables[i], want, true) {
				exact = append(exact, i)
			}
		}
		matches = exact
		if len(matches) != 1 {
			return nil, refuseSilver(http.StatusConflict, "ambiguous_table",
				"more than one table on this connection has that name; name it as schema.table")
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &tables[matches[0]], nil
}

// silverCatalogName admits a schema or table name that may be written into a catalog
// query as a literal: no quote can appear in it.
var silverCatalogName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// silverCatalogSQL returns the column read for one warehouse. Each row is schema,
// table, column, type. The names were checked against silverCatalogName first, which
// is what makes interpolating them as literals safe (the oracleModelCatalogState
// pattern); a name is matched without regard to case, and the catalog's own spelling
// comes back in the row.
func silverCatalogSQL(dialect, schemaName, tableName string) (string, *silverRefusal) {
	if !silverCatalogName.MatchString(tableName) || (schemaName != "" && !silverCatalogName.MatchString(schemaName)) {
		return "", refuseSilver(http.StatusUnprocessableEntity, "unsupported_name",
			"on this warehouse silver reads tables whose schema and name are letters, digits and underscores")
	}
	lowerT, upperT := strings.ToLower(tableName), strings.ToUpper(tableName)
	switch dialect {
	case "bigquery":
		if schemaName == "" {
			return "", refuseSilver(http.StatusBadRequest, "dataset_required", "name the table as dataset.table")
		}
		return fmt.Sprintf("SELECT table_schema, table_name, column_name, data_type FROM `%s`.INFORMATION_SCHEMA.COLUMNS "+
			"WHERE LOWER(table_name) = '%s' ORDER BY table_name, ordinal_position", schemaName, lowerT), nil
	case "snowflake":
		where := fmt.Sprintf("UPPER(TABLE_NAME) = '%s' AND TABLE_SCHEMA <> 'INFORMATION_SCHEMA'", upperT)
		if schemaName != "" {
			where += fmt.Sprintf(" AND UPPER(TABLE_SCHEMA) = '%s'", strings.ToUpper(schemaName))
		}
		return "SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE " + where +
			" ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION", nil
	case "clickhouse":
		db := "database = currentDatabase()"
		if schemaName != "" {
			db = fmt.Sprintf("lower(database) = '%s'", strings.ToLower(schemaName))
		}
		return fmt.Sprintf("SELECT database, table, name, type FROM system.columns WHERE lower(table) = '%s' AND %s "+
			"ORDER BY database, table, position", lowerT, db), nil
	case "oracle":
		owner := "OWNER = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')"
		if schemaName != "" {
			owner = fmt.Sprintf("UPPER(OWNER) = '%s'", strings.ToUpper(schemaName))
		}
		return fmt.Sprintf("SELECT OWNER, TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM ALL_TAB_COLUMNS WHERE UPPER(TABLE_NAME) = '%s' AND %s "+
			"ORDER BY OWNER, TABLE_NAME, COLUMN_ID", upperT, owner), nil
	}
	return "", refuseSilver(http.StatusBadRequest, "unsupported_connector", "silver models are not supported on this connector")
}

// silverRowField reads one field of a catalog row whatever case the engine gave its
// column name (Oracle and Snowflake answer in upper case).
func silverRowField(row map[string]interface{}, names ...string) string {
	for k, v := range row {
		for _, n := range names {
			if strings.EqualFold(k, n) && v != nil {
				return fmt.Sprint(v)
			}
		}
	}
	return ""
}

// silverSourceFromCatalog groups catalog rows into the one table they describe.
func silverSourceFromCatalog(rows []map[string]interface{}) (*silverSource, *silverRefusal) {
	var src *silverSource
	for _, row := range rows {
		schemaName := silverRowField(row, "table_schema", "database", "owner")
		tableName := silverRowField(row, "table_name", "table")
		col := silverRowField(row, "column_name", "name")
		if tableName == "" || col == "" {
			continue
		}
		if src == nil {
			src = &silverSource{Schema: schemaName, Table: tableName}
		} else if src.Schema != schemaName || src.Table != tableName {
			return nil, refuseSilver(http.StatusConflict, "ambiguous_table",
				"more than one table on this connection has that name; name it as schema.table")
		}
		src.Columns = append(src.Columns, silverColumn{Name: col, Type: silverRowField(row, "data_type", "type")})
	}
	return src, nil
}

// splitSilverTableArg splits a warehouse table argument on its last dot.
func splitSilverTableArg(arg string) (schemaName, tableName string) {
	if dot := strings.LastIndex(arg, "."); dot >= 0 {
		return arg[:dot], arg[dot+1:]
	}
	return "", arg
}

// silverConnection is what GetSilverSQL loads about the connection.
type silverConnection struct {
	ID              string
	WorkspaceID     string
	ConnectorType   string
	ConfigEncrypted string
}

// loadSilverSource finds the named table and its columns.
func loadSilverSource(ctx context.Context, conn silverConnection, dialect, table string) (*silverSource, *silverRefusal) {
	notFound := refuseSilver(http.StatusNotFound, "table_not_found",
		"no table by that name on this connection (describe_schema lists them)")
	readFailed := refuseSilver(http.StatusBadGateway, "catalog_unavailable",
		"could not read the connection's tables; check that it is reachable and try again")

	if silverUsesIndex(dialect) {
		// The cached index can predate the table; a miss is checked once against a
		// fresh read before it is called a miss.
		for _, refresh := range []bool{false, true} {
			idx, err := silverSchemaIndex(ctx, conn.ID, conn.WorkspaceID, conn.ConnectorType, conn.ConfigEncrypted, refresh)
			if err != nil {
				log.Errorf("[silver] schema index for connection %s failed: %v", conn.ID, err)
				return nil, readFailed
			}
			t, refusal := findSilverTable(idx.Tables, table)
			if refusal != nil {
				return nil, refusal
			}
			if t != nil {
				src := &silverSource{Schema: t.Schema, Table: t.Name}
				for _, c := range t.Columns {
					src.Columns = append(src.Columns, silverColumn{Name: c.Name, Type: c.Type})
				}
				return src, nil
			}
		}
		return nil, notFound
	}

	schemaName, tableName := splitSilverTableArg(table)
	query, refusal := silverCatalogSQL(dialect, schemaName, tableName)
	if refusal != nil {
		return nil, refusal
	}
	configJSON, err := crypto.DecryptString(conn.ConfigEncrypted)
	if err != nil {
		log.Errorf("[silver] decrypt config for connection %s failed", conn.ID)
		return nil, refuseSilver(http.StatusInternalServerError, "internal", "failed to load the connection")
	}
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		log.Errorf("[silver] parse config for connection %s failed", conn.ID)
		return nil, refuseSilver(http.StatusInternalServerError, "internal", "failed to load the connection")
	}
	rows, err := silverCatalogQuery(ctx, conn.ConnectorType, config, query)
	if err != nil {
		log.Errorf("[silver] catalog read for connection %s failed: %v", conn.ID, err)
		return nil, readFailed
	}
	src, refusal := silverSourceFromCatalog(rows)
	if refusal != nil {
		return nil, refusal
	}
	if src == nil {
		return nil, notFound
	}
	return src, nil
}

type silverSQLRequest struct {
	Table string   `json:"table"`
	Keys  []string `json:"keys"`
}

// GetSilverSQL handles POST /api/v1/explorer/connections/:id/silver-sql: the silver
// model for one bronze table — its shape, key, kept columns, the query and a suggested
// target. It writes nothing; viewer-level, like the schema index it reads, because all
// it returns is generated from names and types.
func GetSilverSQL(c *gin.Context) {
	connectionID, ok := requireUUIDParam(c, "id", "invalid_connection_id", "Invalid connection ID format")
	if !ok {
		return
	}
	if _, ok := resolveUserID(c); !ok {
		return
	}
	var req silverSQLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the body must be a JSON object with table and optionally keys", "code": "invalid_request"})
		return
	}
	req.Table = strings.TrimSpace(req.Table)
	if req.Table == "" || len(req.Table) > silverMaxTableArg {
		c.JSON(http.StatusBadRequest, gin.H{"error": "table is required: the bronze table's name, as table or schema.table", "code": "invalid_request"})
		return
	}
	if len(req.Keys) > silverMaxKeys {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("at most %d keys", silverMaxKeys), "code": "invalid_request"})
		return
	}
	for i, k := range req.Keys {
		if strings.TrimSpace(k) == "" || len(k) > silverMaxKeyLen {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("keys[%d] must be a column name", i), "code": "invalid_request"})
			return
		}
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}
	conn := silverConnection{ID: connectionID, WorkspaceID: activeWorkspaceID(c)}
	err := database.QueryRowContext(c.Request.Context(), `
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, connectionID, conn.WorkspaceID).Scan(&conn.ConnectorType, &conn.ConfigEncrypted)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		log.Errorf("[silver] load connection %s failed: %v", connectionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	dialect := modelDialect(conn.ConnectorType)
	if dialect == "" && isDocumentConnector(conn.ConnectorType) {
		dialect = silverDocumentDialect
	}
	if dialect == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "models are not supported on this connector", "code": "unsupported_connector"})
		return
	}

	src, refusal := loadSilverSource(c.Request.Context(), conn, dialect, req.Table)
	if refusal == nil {
		var plan *silverPlan
		plan, refusal = buildSilverPlan(dialect, src, req.Keys)
		if refusal == nil {
			source := src.Table
			if src.Schema != "" {
				source = src.Schema + "." + src.Table
			}
			c.JSON(http.StatusOK, gin.H{
				"connection_id": connectionID,
				"dialect":       dialect,
				"source_table":  source,
				"shape":         plan.Shape,
				"keys":          plan.Keys,
				"columns":       plan.Columns,
				"sql_text":      plan.SQLText,
				"target_table":  plan.TargetTable,
			})
			return
		}
	}
	c.JSON(refusal.Status, gin.H{"error": refusal.Message, "code": refusal.Code})
}
