package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"api-gateway/internal/cache"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/validators"

	"github.com/gin-gonic/gin"
)

// Ask rsync.ai's generate_sql (internal/assistant/sql.go) writes SQL with the
// Explorer's own two steps, run in-process as the asking user:
// ResolveExplorerTables picks the tables and GenerateSQL writes the SQL. So the
// same LLM privacy guard, plan quota and Usage-page count apply as they did in
// the Explorer (docs/internal/dashboards-design.md § 7.5).

// assistantSQLTables is how many tables the SQL is written over: the Explorer
// took the top three candidates.
const assistantSQLTables = 3

// The two steps, package vars so a test can stand in for llm-service.
var (
	assistantResolveTables gin.HandlerFunc = ResolveExplorerTables
	assistantGenerateSQL   gin.HandlerFunc = GenerateSQL
)

// runAsCallerEngine is made once: gin.New logs a warning each time in debug mode.
var runAsCallerEngine = sync.OnceValue(func() *gin.Engine { return gin.New() })

// The texts the model reads when no SQL was written. Fixed: no handler's own
// message reaches the model.
const (
	sqlNotRunConnection = "Not run: no connection with that id in this workspace. list_connections gives the ids."
	sqlNotRunDocuments  = "Not run: this connection holds documents, not tables, so rsync.ai cannot write SQL for it."
	sqlNotRunSchema     = "Not run: rsync.ai could not read this connection's tables. Check the connection on the Connections page."
	sqlNotRunNoTables   = "Not run: rsync.ai could not tell which tables the question is about. Ask again naming the tables in tables (describe_schema lists them)."
	sqlNotRunTrial      = "Not run: this workspace's free trial has ended, so rsync.ai cannot write SQL. An owner can upgrade on the Billing page."
	sqlNotRunQuota      = "Not run: this workspace has used its NL→SQL queries for this month. An owner can upgrade on the Billing page."
	sqlNotRunNoLLM      = "Not run: no AI model is set up for writing SQL. An admin can set one up."
	sqlNotRunNotRead    = "Not run: the SQL written was not a read-only query, so it is not shown. Ask again."
	sqlNotRunFailed     = "Not run: rsync.ai could not write the SQL right now. Try again later."
)

// AskModel is the model an Ask rsync.ai question is answered with. The SQL
// generate_sql writes comes from it too, not from the server's Explorer model.
// Key is the key an admin saved in the panel, empty when llm-service's own
// environment key serves the entry; Model is empty for the entry's default.
type AskModel struct {
	Entry string
	Model string
	Key   AskKey
}

// AskKey is a model key on its way to llm-service. It prints as [redacted].
type AskKey string

func (AskKey) String() string   { return "[redacted]" }
func (AskKey) GoString() string { return "[redacted]" }

// ctxAskModel carries an AskModel to GenerateSQL. Only Write sets it, on the
// generate step's own context: no request body or header can, so the public
// /api/v1/sql/generate never picks a model or sends a key.
const ctxAskModel = "assistant_ask_model"

// AssistantSQL is the assistant package's SQLWriter.
type AssistantSQL struct{}

// Write returns a read-only statement answering question on connectionID and
// the tables it was written over, written by m when m names an entry.
// tables, when given, are the tables to use, as name or schema.name; else the
// table-linking step picks them. An error's text is fixed, or names only the
// tables the caller gave.
func (AssistantSQL) Write(c *gin.Context, connectionID, question string, tables []string, m AskModel) (string, []string, error) {
	wsID := activeWorkspaceID(c)
	database := db.GetDB()
	if wsID == "" || database == nil {
		return "", nil, errors.New(sqlNotRunFailed)
	}
	ctx := c.Request.Context()
	var connectorType, configEncrypted string
	err := database.QueryRowContext(ctx,
		`SELECT connector_type, config FROM connections WHERE id = $1 AND workspace_id = $2`,
		connectionID, wsID,
	).Scan(&connectorType, &configEncrypted)
	if err == sql.ErrNoRows {
		return "", nil, errors.New(sqlNotRunConnection)
	}
	if err != nil {
		return "", nil, errors.New(sqlNotRunFailed)
	}
	if ResolveExplorerCapability(connectorType).QueryLanguage == langDocument {
		return "", nil, errors.New(sqlNotRunDocuments)
	}
	index, err := silverSchemaIndex(ctx, connectionID, wsID, connectorType, configEncrypted, false)
	if err != nil || index == nil {
		return "", nil, errors.New(sqlNotRunSchema)
	}

	var picked []cache.ExplorerTableIndex
	if len(tables) > 0 {
		var missing []string
		picked, missing = namedTables(index.Tables, tables)
		if len(missing) > 0 {
			return "", nil, errors.New("Not run: no table " + strings.Join(missing, ", ") +
				" on this connection, or the name is in more than one schema (write schema.table). describe_schema lists the tables.")
		}
	} else {
		var resolved ResolveTablesResponse
		status, body := runAsCaller(c, assistantResolveTables, ResolveTablesRequest{ConnectionID: connectionID, Question: question}, nil)
		if status != http.StatusOK {
			return "", nil, sqlStepError(status, body)
		}
		if err := json.Unmarshal(body, &resolved); err != nil {
			return "", nil, errors.New(sqlNotRunFailed)
		}
		picked = candidateTables(index.Tables, resolved.Candidates, assistantSQLTables)
	}
	if len(picked) == 0 {
		return "", nil, errors.New(sqlNotRunNoTables)
	}

	var own map[string]any
	if m.Entry != "" {
		own = map[string]any{ctxAskModel: m}
	}
	status, body := runAsCaller(c, assistantGenerateSQL, generateSQLBody(connectionID, question, picked, index.ForeignKeys), own)
	if status != http.StatusOK {
		return "", nil, sqlStepError(status, body)
	}
	var out SQLGenerateResponse
	if err := json.Unmarshal(body, &out); err != nil || strings.TrimSpace(out.SQL) == "" {
		return "", nil, errors.New(sqlNotRunFailed)
	}
	// One statement, a read: the gate a custom SQL check passes
	// (customCheckSQLRefusal). Classifying alone reads `SELECT 1; DROP …` as a read.
	if v := validators.ValidateExplorerStatement(out.SQL, security.WSViewer); v == nil || !v.Valid ||
		validators.ClassifyStatementSQL(out.SQL) != validators.ClassRead {
		return "", nil, errors.New(sqlNotRunNotRead)
	}
	used := make([]string, len(picked))
	for i, t := range picked {
		used[i] = explorerTableKey(t)
	}
	return out.SQL, used, nil
}

// generateSQLBody is GenerateSQL's request as the Explorer built it: the
// tables' names, columns and types (no row count, no search tokens), the
// schema line, and the foreign keys among the tables.
func generateSQLBody(connectionID, question string, picked []cache.ExplorerTableIndex, fks []cache.ExplorerForeignKeyIndex) gin.H {
	typed := make([]cache.ExplorerTableIndex, len(picked))
	keys := make([]string, len(picked))
	lines := make([]string, len(picked))
	inSet := make(map[string]bool, len(picked))
	for i, t := range picked {
		typed[i] = cache.ExplorerTableIndex{Name: t.Name, Schema: t.Schema, Columns: t.Columns, PrimaryKey: t.PrimaryKey}
		keys[i] = explorerTableKey(t)
		inSet[strings.ToLower(keys[i])] = true
		cols := make([]string, len(t.Columns))
		for j, col := range t.Columns {
			cols[j] = col.Name
		}
		lines[i] = keys[i] + "(" + strings.Join(cols, ", ") + ")"
	}
	among := []cache.ExplorerForeignKeyIndex{}
	for _, fk := range fks {
		from := strings.ToLower(qualifiedName(fk.FromSchema, fk.FromTable))
		to := strings.ToLower(qualifiedName(fk.ToSchema, fk.ToTable))
		if inSet[from] && inSet[to] {
			among = append(among, fk)
		}
	}
	return gin.H{
		"question":      question,
		"connection_id": connectionID,
		"schema":        strings.Join(lines, "; "),
		"tables":        strings.Join(keys, ","),
		"tables_typed":  typed,
		"foreign_keys":  among,
	}
}

func qualifiedName(schema, name string) string {
	if schema = strings.TrimSpace(schema); schema != "" {
		return schema + "." + name
	}
	return name
}

func explorerTableKey(t cache.ExplorerTableIndex) string {
	return qualifiedName(t.Schema, t.Name)
}

// findTable returns the one table ref names, as schema.name or a name only in
// one schema.
func findTable(all []cache.ExplorerTableIndex, ref string) (cache.ExplorerTableIndex, bool) {
	ref = strings.TrimSpace(ref)
	schema, name := "", ref
	if i := strings.LastIndex(ref, "."); i >= 0 {
		schema, name = ref[:i], ref[i+1:]
	}
	var found []cache.ExplorerTableIndex
	for _, t := range all {
		if !strings.EqualFold(t.Name, name) {
			continue
		}
		if schema != "" && !strings.EqualFold(t.Schema, schema) {
			continue
		}
		found = append(found, t)
	}
	if len(found) != 1 {
		return cache.ExplorerTableIndex{}, false
	}
	return found[0], true
}

// namedTables finds each named table once; missing are the names that match
// no table, or more than one.
func namedTables(all []cache.ExplorerTableIndex, names []string) (picked []cache.ExplorerTableIndex, missing []string) {
	seen := map[string]bool{}
	for _, n := range names {
		t, ok := findTable(all, n)
		if !ok {
			missing = append(missing, n)
			continue
		}
		if k := strings.ToLower(explorerTableKey(t)); !seen[k] {
			seen[k] = true
			picked = append(picked, t)
		}
	}
	return picked, missing
}

// candidateTables is the first max table-linking candidates that are tables
// of the index, in the order given.
func candidateTables(all []cache.ExplorerTableIndex, candidates []TableCandidate, max int) []cache.ExplorerTableIndex {
	var picked []cache.ExplorerTableIndex
	seen := map[string]bool{}
	for _, cand := range candidates {
		if len(picked) >= max {
			break
		}
		ref := strings.TrimSpace(cand.Table)
		if !strings.Contains(ref, ".") {
			ref = qualifiedName(cand.SchemaName, ref)
		}
		t, ok := findTable(all, ref)
		if !ok {
			continue
		}
		if k := strings.ToLower(explorerTableKey(t)); !seen[k] {
			seen[k] = true
			picked = append(picked, t)
		}
	}
	return picked
}

// sqlStepError is the fixed text for a step's non-2xx answer.
func sqlStepError(status int, body []byte) error {
	var e struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &e)
	switch {
	case status == http.StatusPaymentRequired && e.Error == "trial_expired":
		return errors.New(sqlNotRunTrial)
	case status == http.StatusPaymentRequired:
		return errors.New(sqlNotRunQuota)
	case status == http.StatusBadRequest && e.ErrorCode == "document_connection":
		return errors.New(sqlNotRunDocuments)
	case status == http.StatusNotFound:
		return errors.New(sqlNotRunConnection)
	case status == http.StatusServiceUnavailable && e.Error == llmNotConfiguredCode:
		return errors.New(sqlNotRunNoLLM)
	default:
		return errors.New(sqlNotRunFailed)
	}
}

// runAsCaller runs h with body as its JSON request, the way the agent
// gateway's tools call a REST handler (agentgw/call.go): the caller's context
// keys and request context, marked as an assistant call, and no header of the
// real request. own are keys set on h's context only, after the caller's. It
// returns the status and body h wrote.
func runAsCaller(c *gin.Context, h gin.HandlerFunc, body any, own map[string]any) (int, []byte) {
	raw, err := json.Marshal(body)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "/assistant/generate-sql", bytes.NewReader(raw))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set("Content-Type", "application/json")
	// The caller's trusted client IP, not RemoteAddr (the proxy in front of
	// the gateway): h's own audit row records sc.ClientIP().
	req.RemoteAddr = net.JoinHostPort(c.ClientIP(), "0")
	w := httptest.NewRecorder()
	sc := gin.CreateTestContextOnly(w, runAsCallerEngine())
	sc.Request = req
	for k, v := range c.Keys {
		sc.Set(k, v)
	}
	for k, v := range own {
		sc.Set(k, v)
	}
	MarkAssistantCall(sc)
	h(sc)
	return sc.Writer.Status(), w.Body.Bytes()
}
