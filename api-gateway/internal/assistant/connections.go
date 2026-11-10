package assistant

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// A call that names a connection the workspace does not have (an id the model
// made up, or a connection's name where its id goes) is told the workspace's
// connections with their ids, so the model calls again in the same answer.
// Pointing it at list_connections was not enough: a model that skipped
// list_connections then ended on "the connection ID is not valid" (the
// 2026-10-11 Ask eval). The connections are list_connections' own, run as the
// person, so the hint names only what that tool would show them. The id the
// model wrote is never repeated, as unknownToolNote does not repeat a name.

const (
	listConnectionsTool = "list_connections"
	// connectionHintChars bounds a hint, in characters.
	connectionHintChars = 1800
	// maxSampleErrorChars is llm-service's cap on a sample_rows error
	// (src/egress/sample_rows.py _MAX_ERROR): a longer one is not sent.
	maxSampleErrorChars = 500
)

type connectionRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SQLDialect string `json:"sql_dialect"`
}

// workspaceConnections is list_connections' result, read at most once a question.
type workspaceConnections struct {
	read  bool
	ok    bool
	list  []connectionRef
	total int
}

func (w *workspaceConnections) load(c *gin.Context, tools Tools) bool {
	if w.read {
		return w.ok
	}
	w.read = true
	r := tools.Run(c, listConnectionsTool, json.RawMessage(`{}`))
	var body struct {
		Connections *[]connectionRef `json:"connections"`
		Total       int              `json:"total"`
	}
	if r.IsError || json.Unmarshal([]byte(r.Text), &body) != nil || body.Connections == nil {
		return false
	}
	w.ok, w.list, w.total = true, *body.Connections, body.Total
	return true
}

// connectionArg reports whether a tool's input schema takes a connection_id,
// and whether it must.
func connectionArg(schema map[string]any) (takes, required bool) {
	props, _ := schema["properties"].(map[string]any)
	if _, takes = props["connection_id"]; !takes {
		return false, false
	}
	switch req := schema["required"].(type) {
	case []string:
		for _, r := range req {
			required = required || r == "connection_id"
		}
	case []any:
		for _, r := range req {
			required = required || r == "connection_id"
		}
	}
	return true, required
}

// connectionHint is what a failed call of tool is told when its connection_id
// is not one of the workspace's connections: their names and ids, in at most
// limit characters. Empty when the call named one of them (it failed for
// another reason), named none it had to, or the connections could not be read.
func connectionHint(c *gin.Context, tools Tools, conns *workspaceConnections, tool string, required bool, args string, limit int) string {
	var a map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	raw, named := a["connection_id"]
	if !named && !required {
		return ""
	}
	var id string
	_ = json.Unmarshal(raw, &id) // a number or null names no connection
	id = strings.TrimSpace(id)
	if !conns.load(c, tools) {
		return ""
	}
	sqlOnly := tool == GenerateSQLTool
	var fit []connectionRef
	for _, cn := range conns.list {
		if id != "" && strings.EqualFold(cn.ID, id) {
			return ""
		}
		if !sqlOnly || cn.SQLDialect != "" {
			fit = append(fit, cn)
		}
	}
	what := "connections"
	if sqlOnly {
		what = "connections queried with SQL"
	}
	if len(fit) == 0 {
		return "This workspace has no " + what + "."
	}
	prefix := "This workspace's " + what + ": "
	more := " (and more: list_connections lists them all)"
	again := ". Call " + tool + " again, in this answer, with the id of the one the question is about; never ask the person for it."
	budget := limit - utf8.RuneCountInString(prefix+more+again)
	var list strings.Builder
	shown := 0
	for _, cn := range fit {
		entry := cn.Name + " (id " + cn.ID
		if sqlOnly {
			entry += ", " + cn.SQLDialect
		}
		entry += ")"
		if shown > 0 {
			entry = "; " + entry
		}
		if utf8.RuneCountInString(list.String()+entry) > budget {
			break
		}
		list.WriteString(entry)
		shown++
	}
	if shown == 0 {
		return "list_connections gives this workspace's " + what + " and their ids" + again
	}
	if shown == len(fit) && conns.total <= len(conns.list) {
		more = ""
	}
	return prefix + list.String() + more + again
}

// withHint adds hint to a tool's error text: inside {"error": ...}, which is
// all a sample_rows error may be, or after a plain one.
func withHint(text, hint string) string {
	var e map[string]string
	if json.Unmarshal([]byte(text), &e) == nil && len(e) == 1 && e["error"] != "" {
		return sampleError(strings.TrimRight(e["error"], " ") + " " + hint)
	}
	return strings.TrimRight(text, " ") + " " + hint
}

// hintLimit is how many characters a hint added to text, tool's error, may take.
func hintLimit(tool, text string) int {
	if tool != SampleRowsTool {
		return connectionHintChars
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(text), &e)
	return maxSampleErrorChars - utf8.RuneCountInString(e.Error) - 1
}
