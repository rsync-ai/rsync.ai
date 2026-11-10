package assistant

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"api-gateway/internal/handlers"

	"github.com/gin-gonic/gin"
)

// fakeSQL is generate_sql's writer in memory.
type fakeSQL struct {
	mu     sync.Mutex
	writes []sqlArgs
	models []handlers.AskModel
	sql    string
	used   []string
	err    error
}

func sqlOption(f *fakeSQL) SQLWriter {
	if f == nil {
		return nil
	}
	return f
}

func (f *fakeSQL) Write(_ *gin.Context, connectionID, question string, tables []string, m handlers.AskModel) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, sqlArgs{ConnectionID: connectionID, Question: question, Tables: tables})
	f.models = append(f.models, m)
	if f.err != nil {
		return "", nil, f.err
	}
	return f.sql, f.used, nil
}

const draftSQL = "SELECT status, COUNT(*) AS orders FROM public.orders GROUP BY status"

func writesSQL() *fakeSQL {
	return &fakeSQL{sql: draftSQL, used: []string{"public.orders"}}
}

func sqlCall(id string) ToolCall {
	return ToolCall{ID: id, Name: GenerateSQLTool, Arguments: `{"connection_id":"` + sampleConn + `","question":"orders by status"}`}
}

func TestGenerateSQLOfferedOnlyWhenWired(t *testing.T) {
	h := newHarnessSQL(t, &memStore{}, nil, writesSQL(), Limits{}, say("ok"))
	h.ask(t, "u1", map[string]any{"question": "q"})
	if !offers(h.llm.reqs[0], GenerateSQLTool) {
		t.Fatal("generate_sql not offered with Options.SQL set")
	}
	h = newHarness(t, Limits{}, say("ok"))
	h.ask(t, "u1", map[string]any{"question": "q"})
	if offers(h.llm.reqs[0], GenerateSQLTool) {
		t.Fatal("generate_sql offered with Options.SQL nil")
	}
}

// The SQL goes to the panel only: the model reads which connection and tables,
// never the statement. The call is audited without the question or the SQL,
// and not metered here: the Explorer's own step counts it.
// The SQL is written by the model the question is answered with: the entry,
// its saved model and its saved key reach the writer, not the server's
// Explorer model.
func TestGenerateSQLIsWrittenByTheAskModel(t *testing.T) {
	f := writesSQL()
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{}, callTools(sqlCall("c1")), say("Here is the query."))
	h.keys.rows["claude"] = savedKey{key: "unit-test-claude-key", model: "claude-sonnet-5-5"}
	if w, _ := h.ask(t, "u1", map[string]any{"question": "how many orders in each status?"}); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := handlers.AskModel{Entry: "claude", Model: "claude-sonnet-5-5", Key: "unit-test-claude-key"}
	if h.llm.reqs[0].Entry != "claude" {
		t.Fatalf("control: the question was answered with %q", h.llm.reqs[0].Entry)
	}
	if len(f.models) != 1 || f.models[0] != want {
		t.Fatalf("the SQL was written with %+v, want %+v", f.models, want)
	}
}

func TestGenerateSQLGoesToThePersonNotTheModel(t *testing.T) {
	f := writesSQL()
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{}, callTools(sqlCall("c1")), say("Here is the query."))
	w, out := h.ask(t, "u1", map[string]any{"question": "how many orders in each status?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.writes) != 1 || f.writes[0].ConnectionID != sampleConn || f.writes[0].Question != "orders by status" || f.writes[0].Tables != nil {
		t.Fatalf("writes = %+v", f.writes)
	}

	msgs := h.llm.reqs[1].Messages
	result := msgs[len(msgs)-1]
	if result.Role != "tool" || result.Data != "" {
		t.Fatalf("generate_sql result = %+v, want an unmarked tool message", result)
	}
	if strings.Contains(result.Content, "SELECT") || strings.Contains(result.Content, "COUNT") {
		t.Fatalf("the model read the SQL: %s", result.Content)
	}
	var got sqlResult
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
		t.Fatalf("result is not the fixed shape: %v: %s", err, result.Content)
	}
	if got.ConnectionID != sampleConn || strings.Join(got.Tables, ",") != "public.orders" || got.Note != sqlNote {
		t.Fatalf("result = %+v", got)
	}

	if len(out.LookedAt) != 1 || !out.LookedAt[0].OK || out.LookedAt[0].SQL == nil {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
	if d := *out.LookedAt[0].SQL; d.SQL != draftSQL || d.ConnectionID != sampleConn || strings.Join(d.Tables, ",") != "public.orders" {
		t.Fatalf("draft = %+v", d)
	}

	var rows []auditRow
	for _, r := range h.audit.rows {
		if r.action == "assistant_generate_sql" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || rows[0].resourceType != "connection" || rows[0].resourceID != sampleConn || rows[0].details["outcome"] != "ok" || rows[0].details["tables"] != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	raw, _ := json.Marshal(rows[0].details)
	if strings.Contains(string(raw), "SELECT") || strings.Contains(string(raw), "orders by status") {
		t.Fatalf("audit row carries the SQL or the question: %s", raw)
	}
	if len(h.metered) != 0 {
		t.Fatalf("metered %q: GenerateSQL counts the query itself", h.metered)
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", out.ConversationID)]
	if len(turns) != 1 || strings.Contains(turns[0].Answer, "SELECT") {
		t.Fatalf("stored turns = %+v", turns)
	}
}

func TestGenerateSQLWriterErrorIsTheModelsText(t *testing.T) {
	f := writesSQL()
	f.err = errors.New("Not run: no AI model is set up for writing SQL. An admin can set one up.")
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{}, callTools(sqlCall("c1")), say("ok"))
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	msgs := h.llm.reqs[1].Messages
	result := msgs[len(msgs)-1]
	if result.Content != `{"error":"Not run: no AI model is set up for writing SQL. An admin can set one up."}` {
		t.Fatalf("error result = %s", result.Content)
	}
	if len(out.LookedAt) != 1 || out.LookedAt[0].OK || out.LookedAt[0].SQL != nil {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
	last := h.audit.rows[len(h.audit.rows)-1]
	if last.action != "assistant_generate_sql" || last.details["outcome"] != "error" {
		t.Fatalf("audit = %+v", last)
	}
}

// The question goes to the Explorer's steps, which take names only. Once a
// value may be in what the model read, no SQL is written in that conversation.
func TestGenerateSQLRefusedOnceSampleValuesAreInTheConversation(t *testing.T) {
	t.Run("sampled earlier in this question", func(t *testing.T) {
		f := writesSQL()
		h := newHarnessSQL(t, &memStore{}, openSamples(), f, Limits{},
			callTools(sampleCall("c1")), callTools(sqlCall("c2")), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		assertSQLRefused(t, h, f, out)
	})
	t.Run("sampled in the same turn", func(t *testing.T) {
		f := writesSQL()
		h := newHarnessSQL(t, &memStore{}, openSamples(), f, Limits{},
			callTools(sampleCall("c1"), sqlCall("c2")), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		assertSQLRefused(t, h, f, out)
	})
	t.Run("a sampled earlier answer is sent", func(t *testing.T) {
		f := writesSQL()
		store := &memStore{convs: map[string][]Turn{
			conversationKey("u1", "ws-1", "conv-1"): {{Question: "q1", Answer: "status is shipped", Samples: "claude"}},
		}}
		h := newHarnessSQL(t, store, openSamples(), f, Limits{}, callTools(sqlCall("c1")), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q", "conversation_id": "conv-1"})
		assertSQLRefused(t, h, f, out)
	})
	t.Run("a sampled earlier answer is withheld", func(t *testing.T) {
		f := writesSQL()
		store := &memStore{convs: map[string][]Turn{
			conversationKey("u1", "ws-1", "conv-1"): {{Question: "q1", Answer: "status is shipped", Samples: "claude"}},
		}}
		// Sample rows are off now, so the earlier answer is not sent: nothing
		// the model reads holds a value.
		h := newHarnessSQL(t, store, nil, f, Limits{}, callTools(sqlCall("c1")), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q", "conversation_id": "conv-1"})
		if len(f.writes) != 1 || len(out.LookedAt) != 1 || out.LookedAt[0].SQL == nil {
			t.Fatalf("writes = %+v, looked_at = %+v", f.writes, out.LookedAt)
		}
	})
}

func assertSQLRefused(t *testing.T, h *harness, f *fakeSQL, out Answer) {
	t.Helper()
	if len(f.writes) != 0 {
		t.Fatalf("Write ran %d times with sample values in the conversation", len(f.writes))
	}
	var sqlResults []Message
	for _, req := range h.llm.reqs {
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "SQL cannot be written") {
				sqlResults = append(sqlResults, m)
			}
		}
	}
	if len(sqlResults) == 0 || !strings.HasPrefix(sqlResults[0].Content, `{"error":"Not run: sample values`) {
		t.Fatalf("no refusal reached the model: %+v", sqlResults)
	}
	for _, l := range out.LookedAt {
		if l.SQL != nil {
			t.Fatalf("looked_at carries SQL: %+v", out.LookedAt)
		}
	}
	for _, r := range h.audit.rows {
		if r.action == "assistant_generate_sql" {
			t.Fatal("a refused call wrote an audit row")
		}
	}
}

func TestGenerateSQLBadArgumentsNotRun(t *testing.T) {
	long := strings.Repeat("a", maxSQLQuestion+1)
	many := `["a","b","c","d","e","f","g","h","i","j","k"]`
	cases := map[string]string{
		"not json":       `{"connection_id":`,
		"unknown field":  `{"connection_id":"` + sampleConn + `","question":"q","sql":"DROP TABLE x"}`,
		"bad id":         `{"connection_id":"orders","question":"q"}`,
		"no question":    `{"connection_id":"` + sampleConn + `"}`,
		"long question":  `{"connection_id":"` + sampleConn + `","question":"` + long + `"}`,
		"control char":   `{"connection_id":"` + sampleConn + `","question":"a\u0007b"}`,
		"too many":       `{"connection_id":"` + sampleConn + `","question":"q","tables":` + many + `}`,
		"blank table":    `{"connection_id":"` + sampleConn + `","question":"q","tables":[" "]}`,
		"table with tab": `{"connection_id":"` + sampleConn + `","question":"q","tables":["a\tb"]}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			f := writesSQL()
			h := newHarnessSQL(t, &memStore{}, nil, f, Limits{},
				callTools(ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: args}), say("ok"))
			_, out := h.ask(t, "u1", map[string]any{"question": "q"})
			if len(f.writes) != 0 {
				t.Fatalf("Write ran on %s", args)
			}
			msgs := h.llm.reqs[1].Messages
			if got := msgs[len(msgs)-1].Content; !strings.HasPrefix(got, `{"error":"Not run: `) {
				t.Fatalf("result = %s", got)
			}
			if len(out.LookedAt) == 1 && (out.LookedAt[0].OK || out.LookedAt[0].SQL != nil) {
				t.Fatalf("looked_at = %+v", out.LookedAt)
			}
		})
	}
}

func TestGenerateSQLPassesNamedTablesAndKeepsLineBreaks(t *testing.T) {
	f := writesSQL()
	args := `{"connection_id":"` + sampleConn + `","question":"orders\nby status","tables":[" public.orders ","customers"]}`
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{},
		callTools(ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: args}), say("ok"))
	h.ask(t, "u1", map[string]any{"question": "q"})
	if len(f.writes) != 1 || f.writes[0].Question != "orders\nby status" || strings.Join(f.writes[0].Tables, "|") != "public.orders|customers" {
		t.Fatalf("writes = %+v", f.writes)
	}
}
