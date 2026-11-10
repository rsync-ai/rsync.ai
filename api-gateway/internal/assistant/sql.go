package assistant

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"api-gateway/internal/handlers"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Writing SQL (docs/internal/dashboards-design.md § 7.5). Ask rsync.ai's
// generate_sql writes a read-only query with the Explorer's own two steps
// (handlers.AssistantSQL), so the same LLM privacy guard and plan quota apply,
// and it counts on the Usage page as an Explorer generation did. The SQL goes
// to the person only, on the answer's "Looked at", with Copy and Open in
// Explorer; the model reads which connection and tables it was written over,
// never the SQL. Nothing runs. The tool is the assistant's own: the agent
// gateway's tools may not return query text.

// GenerateSQLTool is the tool's name. No agent gateway tool may take it.
const GenerateSQLTool = "generate_sql"

const (
	maxSQLQuestion = 1000
	maxSQLTables   = 10
	sqlNote        = "The SQL is shown to the person with Copy and Open in Explorer. You are not shown it, and it has not run."
)

// SQLWriter writes the SQL (handlers.AssistantSQL).
type SQLWriter interface {
	// Write returns a read-only statement answering question on one of the
	// active workspace's connections, as the signed-in user, and the tables it
	// uses. tables, when given, are the tables to use. m is the model the
	// question is answered with, which writes the SQL too. An error's message
	// is one a model may read.
	Write(c *gin.Context, connectionID, question string, tables []string, m handlers.AskModel) (sql string, used []string, err error)
}

// SQLDraft is a generate_sql call's SQL, for the panel only.
type SQLDraft struct {
	SQL          string   `json:"sql"`
	ConnectionID string   `json:"connection_id"`
	Tables       []string `json:"tables"`
}

var generateSQLSpec = Tool{
	Name: GenerateSQLTool,
	Description: "Writes a read-only SQL query that answers a question about the data in one of this workspace's " +
		"connections, from names and types only, as the Explorer does. The person sees the SQL with Copy and Open in " +
		"Explorer; you are not shown it and nothing runs.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"connection_id": map[string]any{"type": "string", "format": "uuid", "description": "The connection's id, from list_connections: one with a sql_dialect, which the SQL is written in."},
			"question": map[string]any{
				"type": "string", "maxLength": maxSQLQuestion,
				"description": "What the query must answer, in the person's words, with table and column names where you know them. Never a value read from sample_rows.",
			},
			"tables": map[string]any{
				"type": "array", "items": map[string]any{"type": "string", "maxLength": maxSampleTableLen}, "maxItems": maxSQLTables,
				"description": "The tables to use, as describe_schema names them, such as public.orders. Leave it out to let rsync.ai pick them.",
			},
		},
		"required":             []string{"connection_id", "question"},
		"additionalProperties": false,
	},
}

type sqlArgs struct {
	ConnectionID string   `json:"connection_id"`
	Question     string   `json:"question"`
	Tables       []string `json:"tables"`
}

func parseSQLArgs(raw string) (sqlArgs, string) {
	var a sqlArgs
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, "the arguments must be {connection_id, question, tables?}"
	}
	if _, err := uuid.Parse(a.ConnectionID); err != nil {
		return a, "connection_id must be a connection's id, from list_connections"
	}
	a.Question = strings.TrimSpace(a.Question)
	if a.Question == "" || utf8.RuneCountInString(a.Question) > maxSQLQuestion || strings.IndexFunc(a.Question, notTextRune) >= 0 {
		return a, "question must be text of at most 1000 characters"
	}
	if len(a.Tables) > maxSQLTables {
		return a, "tables may name at most 10 tables"
	}
	for i, t := range a.Tables {
		a.Tables[i] = strings.TrimSpace(t)
		if a.Tables[i] == "" || len(t) > maxSampleTableLen || strings.IndexFunc(t, unicode.IsControl) >= 0 {
			return a, "each table must be a table name of at most 256 characters"
		}
	}
	return a, ""
}

// notTextRune is a control character other than a line break or tab.
func notTextRune(r rune) bool {
	return unicode.IsControl(r) && r != '\n' && r != '\t'
}

// sqlResult is what the model reads: never the SQL.
type sqlResult struct {
	ConnectionID string   `json:"connection_id"`
	Tables       []string `json:"tables"`
	Note         string   `json:"note"`
}

// runGenerateSQL runs one generate_sql call, audits it, and returns the result
// the model reads and the SQL for the panel (nil when none was written).
func (s *Service) runGenerateSQL(c *gin.Context, q question, args string) (string, bool, *SQLDraft) {
	a, problem := parseSQLArgs(args)
	if problem != "" {
		return sampleError("Not run: " + problem + "."), true, nil
	}
	audit := map[string]interface{}{"entry": q.entry, "tables_named": len(a.Tables)}
	sql, used, err := s.opts.SQL.Write(c, a.ConnectionID, a.Question, a.Tables, q.model)
	if err != nil {
		audit["outcome"] = "error"
		s.opts.Audit(c, "assistant_generate_sql", "connection", a.ConnectionID, audit)
		return sampleError(err.Error()), true, nil
	}
	audit["outcome"] = "ok"
	audit["tables"] = len(used)
	s.opts.Audit(c, "assistant_generate_sql", "connection", a.ConnectionID, audit)
	raw, _ := json.Marshal(sqlResult{ConnectionID: a.ConnectionID, Tables: used, Note: sqlNote})
	return string(raw), false, &SQLDraft{SQL: sql, ConnectionID: a.ConnectionID, Tables: used}
}

// valuesInConversation reports whether a message the model will read may hold
// a row value: a sample_rows result, or an answer or turn written after one.
func valuesInConversation(msgs []Message) bool {
	for _, m := range msgs {
		if m.Data == DataSampleRows {
			return true
		}
	}
	return false
}
