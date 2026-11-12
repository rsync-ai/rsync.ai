package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"api-gateway/internal/agentgw"
)

// Bug class: **a tool given a connection the workspace does not have ends the
// answer.** Found in the 2026-10-11 Ask eval: with no connection named, the
// model skipped list_connections, called generate_sql with "rsync-ai-warehouse"
// and answered "the connection ID is not valid". An error that only pointed at
// list_connections did not make it look the id up, so every tool that takes a
// connection_id is now told the workspace's connections and their ids, and the
// model calls again in the same answer.

const (
	warehouseConn = "0b7e4d1a-2f3c-4e5d-8a9b-1c2d3e4f5a6b"
	restConn      = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"
	madeUpConn    = "rsync-ai-warehouse"
)

// Two connections: one queried with SQL, one not.
const twoConnections = `{"connections":[` +
	`{"id":"` + warehouseConn + `","name":"Fintech Warehouse","connector_type":"postgresql","sql_dialect":"postgresql"},` +
	`{"id":"` + restConn + `","name":"Orders API","connector_type":"rest"}` +
	`],"total":2,"truncated":false}`

// describeSchemaSpec is an agent gateway tool that must name a connection.
var describeSchemaSpec = agentgw.ToolSpec{
	Name: "describe_schema", Description: "Tables of a connection",
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"connection_id": map[string]any{"type": "string"}},
		"required":   []any{"connection_id"},
	},
}

func withConnections(h *harness, listed string) {
	h.tools.specs = []agentgw.ToolSpec{
		{Name: listConnectionsTool, Description: "List connections", InputSchema: map[string]any{"type": "object"}},
		describeSchemaSpec,
	}
	h.tools.results[listConnectionsTool] = listed
}

// lastResult is the tool result the model read before its last turn.
func lastResult(t *testing.T, h *harness) string {
	t.Helper()
	msgs := h.llm.reqs[len(h.llm.reqs)-1].Messages
	return msgs[len(msgs)-1].Content
}

func listCalls(h *harness) int {
	n := 0
	for _, c := range h.tools.calls {
		if c.name == listConnectionsTool {
			n++
		}
	}
	return n
}

func TestACallNamingAnUnknownConnectionIsToldTheWorkspacesConnections(t *testing.T) {
	cases := []struct {
		name string
		call ToolCall
		// sqlOnly: generate_sql lists only the connections queried with SQL.
		sqlOnly bool
		// writerErr is the writer's answer to an id that is not the workspace's.
		writerErr error
		sampleErr error
	}{
		{name: "generate_sql, a made-up id", sqlOnly: true,
			call: ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + madeUpConn + `","question":"declined card transactions by merchant"}`}},
		{name: "generate_sql, an id of no connection here", sqlOnly: true,
			writerErr: errors.New("Not run: no connection with that id in this workspace. list_connections gives the ids."),
			call:      ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + sampleConn + `","question":"declined card transactions by merchant"}`}},
		{name: "generate_sql, no id", sqlOnly: true,
			call: ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"question":"declined card transactions by merchant"}`}},
		{name: "sample_rows, a made-up id",
			call: ToolCall{ID: "c1", Name: SampleRowsTool, Arguments: `{"connection_id":"` + madeUpConn + `","table":"fintech_silver.transactions"}`}},
		{name: "sample_rows, an id of no connection here",
			sampleErr: errors.New("connection not found in this workspace"),
			call:      ToolCall{ID: "c1", Name: SampleRowsTool, Arguments: `{"connection_id":"` + sampleConn + `","table":"fintech_silver.transactions"}`}},
		{name: "an agent gateway tool, a made-up id",
			call: ToolCall{ID: "c1", Name: "describe_schema", Arguments: `{"connection_id":"` + madeUpConn + `"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := writesSQL()
			f.err = tc.writerErr
			samples := openSamples()
			samples.readErr = tc.sampleErr
			h := newHarnessSQL(t, &memStore{}, samples, f, Limits{}, callTools(tc.call), say("ok"))
			withConnections(h, twoConnections)
			h.tools.results["describe_schema"] = "Invalid arguments: connection_id must be an id (a uuid), not a name."
			h.tools.errs["describe_schema"] = true

			_, out := h.ask(t, "u1", map[string]any{"question": "Which merchants have the most declined card transactions?"})
			got := lastResult(t, h)
			for _, want := range []string{"Fintech Warehouse", warehouseConn, "Call " + tc.call.Name + " again"} {
				if !strings.Contains(got, want) {
					t.Fatalf("the error does not name %q: %s", want, got)
				}
			}
			if tc.sqlOnly && strings.Contains(got, restConn) {
				t.Fatalf("generate_sql's error lists a connection with no SQL: %s", got)
			}
			if !tc.sqlOnly && !strings.Contains(got, restConn) {
				t.Fatalf("the error leaves out a connection: %s", got)
			}
			var wrote struct {
				ConnectionID string `json:"connection_id"`
			}
			_ = json.Unmarshal([]byte(tc.call.Arguments), &wrote)
			if wrote.ConnectionID != "" && strings.Contains(got, wrote.ConnectionID) {
				t.Fatalf("the error repeats the id the model wrote: %s", got)
			}
			if tc.call.Name != "describe_schema" && !json.Valid([]byte(got)) {
				t.Fatalf("the error is not {\"error\": ...}: %s", got)
			}
			if listCalls(h) != 1 {
				t.Fatalf("list_connections ran %d times, want 1", listCalls(h))
			}
			// The lookup is the gateway's own: the model's calls are what it made.
			if len(out.LookedAt) != 1 || out.LookedAt[0].OK {
				t.Fatalf("looked_at = %+v", out.LookedAt)
			}
		})
	}
}

// The model, told the connections, calls again with the right id and the
// SQL is written in the same answer.
func TestTheModelCallsAgainWithTheRightConnection(t *testing.T) {
	f := writesSQL()
	made := ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + madeUpConn + `","question":"declined card transactions by merchant"}`}
	again := ToolCall{ID: "c2", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + warehouseConn + `","question":"declined card transactions by merchant"}`}
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{}, callTools(made), callTools(again), say("ok"))
	withConnections(h, twoConnections)
	_, out := h.ask(t, "u1", map[string]any{"question": "Which merchants have the most declined card transactions?"})
	if len(f.writes) != 1 || f.writes[0].ConnectionID != warehouseConn {
		t.Fatalf("writes = %+v", f.writes)
	}
	if len(out.LookedAt) != 2 || out.LookedAt[1].SQL == nil {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
}

// A call that named one of the workspace's connections failed for another
// reason: its error is left as it was.
func TestAKnownConnectionsErrorIsUnchanged(t *testing.T) {
	f := writesSQL()
	f.err = errors.New("Not run: rsync.ai could not read this connection's tables. Check the connection on the Connections page.")
	call := ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + strings.ToUpper(warehouseConn) + `","question":"q"}`}
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{}, callTools(call), say("ok"))
	withConnections(h, twoConnections)
	h.ask(t, "u1", map[string]any{"question": "q"})
	if got := lastResult(t, h); got != sampleError(f.err.Error()) {
		t.Fatalf("result = %s", got)
	}
}

// A tool whose connection_id is optional, called without one, failed for
// another reason.
func TestAnOptionalConnectionLeftOutGetsNoList(t *testing.T) {
	h := newHarnessSQL(t, &memStore{}, nil, nil, Limits{},
		callTools(ToolCall{ID: "c1", Name: "list_models", Arguments: `{}`}), say("ok"))
	withConnections(h, twoConnections)
	h.tools.specs = append(h.tools.specs, agentgw.ToolSpec{Name: "list_models", Description: "Models", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"connection_id": map[string]any{"type": "string"}},
	}})
	h.tools.results["list_models"] = "rsync.ai could not run this tool."
	h.tools.errs["list_models"] = true
	h.ask(t, "u1", map[string]any{"question": "q"})
	if got := lastResult(t, h); got != "rsync.ai could not run this tool." {
		t.Fatalf("result = %s", got)
	}
	if listCalls(h) != 0 {
		t.Fatal("list_connections ran for a call that named no connection")
	}
}

// When the connections cannot be read, the error is left as it was.
func TestUnreadableConnectionsLeaveTheError(t *testing.T) {
	call := ToolCall{ID: "c1", Name: GenerateSQLTool, Arguments: `{"connection_id":"` + madeUpConn + `","question":"q"}`}
	h := newHarnessSQL(t, &memStore{}, nil, writesSQL(), Limits{}, callTools(call), say("ok"))
	withConnections(h, "rsync.ai could not run this tool.")
	h.tools.errs[listConnectionsTool] = true
	h.ask(t, "u1", map[string]any{"question": "q"})
	want := sampleError("Not run: connection_id must be a connection's id, from list_connections.")
	if got := lastResult(t, h); got != want {
		t.Fatalf("result = %s", got)
	}
}

// Every failed call of one question reads the connections once.
func TestTheConnectionsAreReadOnceAQuestion(t *testing.T) {
	call := func(id string) ToolCall {
		return ToolCall{ID: id, Name: GenerateSQLTool, Arguments: `{"connection_id":"` + madeUpConn + `","question":"q"}`}
	}
	h := newHarnessSQL(t, &memStore{}, nil, writesSQL(), Limits{}, callTools(call("c1")), callTools(call("c2")), say("ok"))
	withConnections(h, twoConnections)
	h.ask(t, "u1", map[string]any{"question": "q"})
	if listCalls(h) != 1 {
		t.Fatalf("list_connections ran %d times, want 1", listCalls(h))
	}
}

// llm-service sends a sample_rows error of at most 500 characters, and none
// longer: however many connections there are, the list fits.
func TestASampleRowsErrorFitsTheGuardsCap(t *testing.T) {
	var conns []string
	for i := 0; i < 60; i++ {
		conns = append(conns, fmt.Sprintf(`{"id":"%08d-0000-4000-8000-000000000000","name":"Regional warehouse number %d","sql_dialect":"postgresql"}`, i, i))
	}
	call := ToolCall{ID: "c1", Name: SampleRowsTool, Arguments: `{"connection_id":"` + madeUpConn + `","table":"t"}`}
	h := newHarnessSQL(t, &memStore{}, openSamples(), nil, Limits{}, callTools(call), say("ok"))
	withConnections(h, `{"connections":[`+strings.Join(conns, ",")+`],"total":60}`)
	h.ask(t, "u1", map[string]any{"question": "q"})
	var e struct {
		Error string `json:"error"`
	}
	got := lastResult(t, h)
	if err := json.Unmarshal([]byte(got), &e); err != nil {
		t.Fatalf("not {\"error\": ...}: %s", got)
	}
	if n := utf8.RuneCountInString(e.Error); n > maxSampleErrorChars {
		t.Fatalf("the error is %d characters, over %d: %s", n, maxSampleErrorChars, e.Error)
	}
	if !strings.Contains(e.Error, "Regional warehouse number 0 (id") || !strings.Contains(e.Error, "list_connections lists them all") {
		t.Fatalf("error = %s", e.Error)
	}
}
