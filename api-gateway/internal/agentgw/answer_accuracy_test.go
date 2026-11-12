package agentgw

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// capture answers with body and records the query the handler saw.
func capture(seen *url.Values, body any) gin.HandlerFunc {
	return func(c *gin.Context) {
		*seen = c.Request.URL.Query()
		c.JSON(200, body)
	}
}

// Bug class: a filter compared by exact string. The model passes the asker's
// word ("successful", "Sources", "in progress"), the handler matches no row,
// and the answer is a confident "none". Every filter with a fixed set of
// values now maps the word onto one of them, or refuses it naming the set;
// none reaches the handler as a value it does not know.
func TestFilterWords_MapToTheValueTheHandlerKnows(t *testing.T) {
	cases := []struct {
		tool  string
		args  map[string]any
		param string
		want  string
	}{
		{"list_pipelines", map[string]any{"status": "successful"}, "status", "passed"},
		{"list_pipelines", map[string]any{"status": "Completed"}, "status", "passed"},
		{"list_pipelines", map[string]any{"status": "FAILED"}, "status", "failed"},
		{"list_pipelines", map[string]any{"status": "error"}, "status", "failed"},
		{"list_pipelines", map[string]any{"status": "streaming"}, "status", "running"},
		{"list_pipelines", map[string]any{"status": "in progress"}, "status", "running"},
		{"list_pipelines", map[string]any{"status": "cancelled"}, "status", "stopped"},
		{"list_pipelines", map[string]any{"status": "waiting-for-user"}, "status", "waiting_for_user"},
		{"list_connections", map[string]any{"type": "Sources"}, "type", "source"},
		{"list_connections", map[string]any{"type": "target"}, "type", "destination"},
		{"list_runs", map[string]any{"status": "successful"}, "outcome", "succeeded"},
		{"list_runs", map[string]any{"status": "stopped"}, "outcome", "cancelled"},
		{"list_runs", map[string]any{"status": "error"}, "outcome", "failed"},
		{"list_runs", map[string]any{"status": "failed"}, "outcome", "failed"},
	}
	for _, tc := range cases {
		var seen url.Values
		b := leakyBodies()
		h := leakyHandlers()
		h.ListPipelines = capture(&seen, b["list_pipelines"])
		h.ListConnections = capture(&seen, b["list_connections"])
		h.ListRuns = capture(&seen, b["list_runs"])
		r, _ := testServer(t, h, readerActor)
		res := call(t, r, tc.tool, tc.args)
		if isError(res) {
			t.Fatalf("%s %v: %s", tc.tool, tc.args, resultText(res))
		}
		if got := seen.Get(tc.param); got != tc.want {
			t.Errorf("%s %v: the handler saw %s=%q, want %q", tc.tool, tc.args, tc.param, got, tc.want)
		}
	}
}

func TestFilterWords_UnknownWordIsRefusedWithTheSet(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"list_pipelines", map[string]any{"status": "active"}, "one of running, passed, failed"},
		{"list_connections", map[string]any{"type": "warehouse"}, "one of source, destination"},
		{"list_runs", map[string]any{"status": "everything"}, "one of succeeded, failed, cancelled"},
		{"list_connectors", map[string]any{"supports": "reverse_etl"}, "one of source, destination, cdc"},
	}
	for _, tc := range cases {
		reached := false
		h := leakyHandlers()
		h.ListPipelines = func(*gin.Context) { reached = true }
		h.ListConnections, h.ListRuns, h.ListConnectors = h.ListPipelines, h.ListPipelines, h.ListPipelines
		r, audit := testServer(t, h, readerActor)
		res := call(t, r, tc.tool, tc.args)
		if !isError(res) || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s %v: want an error containing %q, got %q", tc.tool, tc.args, tc.want, resultText(res))
		}
		if reached || len(*audit) != 1 || (*audit)[0].outcome != "invalid_arguments" {
			t.Errorf("%s %v: reached=%v audit=%+v", tc.tool, tc.args, reached, *audit)
		}
	}
}

// A synonym must name a value of its own enum, or a word would map onto a
// value the handler refuses.
func TestCheckTool_SynonymOutsideEnum(t *testing.T) {
	tool := &Tool{Name: "t", Title: "T", Description: "d " + guideSuffix("models"), Guide: "models", ResourceType: "r", Output: runOut{},
		run:  func(*toolCall) (any, error) { return runOut{}, nil },
		Args: []Arg{{Name: "a", Type: "string", Description: "d", Enum: []string{"x"}, Synonyms: map[string]string{"y": "x"}}}}
	if err := checkTool(tool); err != nil {
		t.Fatalf("control: %v", err)
	}
	tool.Args[0].Synonyms["z"] = "w"
	if checkTool(tool) == nil {
		t.Fatal("a synonym outside the enum was accepted")
	}
}

func connectionsBody(types ...string) gin.H {
	var conns []any
	for i, typ := range types {
		conns = append(conns, gin.H{"id": connID[:len(connID)-1] + string(rune('1'+i)), "name": typ + " conn",
			"type": "destination", "connector_type": typ, "status": "active", "is_connected": true})
	}
	return gin.H{"connections": conns}
}

// connector_type is matched the way the NL planner resolves a connector, so
// the display name the asker reads in the UI finds the connection; the
// handler's own exact-match filter never sees it. A filter that matches
// nothing says which connector types the workspace does have.
func TestListConnections_ConnectorTypeByAnyName(t *testing.T) {
	var seen url.Values
	h := leakyHandlers()
	h.ListConnections = capture(&seen, connectionsBody("gcs", "postgresql", "bigquery", "aws-s3"))
	r, _ := testServer(t, h, readerActor)

	for arg, want := range map[string]string{
		"Google Cloud Storage (GCS)": "gcs",
		"google cloud storage":       "gcs",
		"GCS":                        "gcs",
		"postgres":                   "postgresql",
		"Amazon S3":                  "aws-s3",
		"BigQuery":                   "bigquery",
	} {
		d := data(t, call(t, r, "list_connections", map[string]any{"connector_type": arg}))
		conns, _ := d["connections"].([]any)
		if len(conns) != 1 || conns[0].(map[string]any)["connector_type"] != want || d["total"] != float64(1) {
			t.Errorf("connector_type %q: %v", arg, d)
		}
		if seen.Has("connector_type") {
			t.Errorf("connector_type %q reached the handler's exact-match filter: %v", arg, seen)
		}
	}

	d := data(t, call(t, r, "list_connections", map[string]any{"connector_type": "snowflake"}))
	note, _ := d["note"].(string)
	if conns, _ := d["connections"].([]any); len(conns) != 0 || d["total"] != float64(0) {
		t.Fatalf("snowflake matched: %v", d)
	}
	for _, typ := range []string{"aws-s3", "bigquery", "gcs", "postgresql"} {
		if !strings.Contains(note, typ) {
			t.Errorf("the empty answer's note does not name %q: %q", typ, note)
		}
	}

	// Control: no filter returns every connection, with no note.
	d = data(t, call(t, r, "list_connections", map[string]any{}))
	if conns, _ := d["connections"].([]any); len(conns) != 4 || d["total"] != float64(4) || d["note"] != nil {
		t.Fatalf("unfiltered: %v", d)
	}
}

// Found asking Ask rsync.ai about MongoDB data (2026-10-09): the model sent
// generate_sql to the MongoDB connection, which has no SQL, and could not tell
// a warehouse's SQL from PostgreSQL's. Each connection says the SQL dialect it
// is queried in, the one generate_sql writes for it; a connection with none
// is not queried with SQL.
func TestListConnections_NamesEachConnectionsSQLDialect(t *testing.T) {
	h := leakyHandlers()
	h.ListConnections = jsonHandler(200, connectionsBody("bigquery", "postgresql", "mongodb", "gcs", "snowflake"))
	r, _ := testServer(t, h, readerActor)

	d := data(t, call(t, r, "list_connections", map[string]any{}))
	conns, _ := d["connections"].([]any)
	if len(conns) != 5 {
		t.Fatalf("connections = %v", d)
	}
	for _, c := range conns {
		cn := c.(map[string]any)
		ct := cn["connector_type"].(string)
		want := handlers.SQLDialect(ct)
		got, has := cn["sql_dialect"]
		switch {
		case want == "" && has:
			t.Errorf("%s has no SQL, but list_connections says sql_dialect %v", ct, got)
		case want != "" && got != want:
			t.Errorf("%s: sql_dialect = %v, want %q", ct, got, want)
		}
	}
	if handlers.SQLDialect("bigquery") != "bigquery" || handlers.SQLDialect("mongodb") != "" {
		t.Fatal("control: the dialect table changed under this test")
	}
}

// Bug class: a history question answered from the one last run a pipeline
// list carries ("has it ever failed?" → "no"). list_runs reads every run in
// the workspace: the counts by outcome, then the newest runs.
func TestListRuns_CountsAndRuns(t *testing.T) {
	var seen url.Values
	h := leakyHandlers()
	h.ListRuns = capture(&seen, leakyBodies()["list_runs"])
	r, audit := testServer(t, h, readerActor)
	d := data(t, call(t, r, "list_runs", map[string]any{"pipeline_id": pipeID, "limit": 10}))
	if seen.Get("pipeline_id") != pipeID || seen.Get("limit") != "10" || seen.Has("outcome") {
		t.Fatalf("handler query = %v", seen)
	}
	if d["total"] != float64(87) {
		t.Errorf("total = %v", d["total"])
	}
	by, _ := d["by_outcome"].(map[string]any)
	if by["succeeded"] != float64(76) || by["failed"] != float64(1) || by["cancelled"] != float64(4) || by["other"] != float64(2) {
		t.Errorf("by_outcome = %v", by)
	}
	runs, _ := d["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	run := runs[0].(map[string]any)
	if run["outcome"] != "failed" || run["error_class"] != "auth" || run["started_at"] != stamp || run["pipeline_name"] != "orders sync" {
		t.Errorf("run = %v", run)
	}
	if len(*audit) != 1 || (*audit)[0].tool != "list_runs" || (*audit)[0].resourceType != "execution" || (*audit)[0].outcome != "ok" {
		t.Errorf("audit = %+v", *audit)
	}
}

// Bug class: "what can rsync.ai connect to?" answered from the workspace's own
// connections. list_connectors reads the connector catalog, never a
// connector's configuration schema or container details.
func TestListConnectors_Catalog(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	d := data(t, call(t, r, "list_connectors", map[string]any{}))
	conns, _ := d["connectors"].([]any)
	if len(conns) != 2 || d["total"] != float64(2) {
		t.Fatalf("catalog = %v", d)
	}
	gcs := conns[0].(map[string]any)
	if gcs["name"] != "gcs" || gcs["display_name"] != "Google Cloud Storage" || gcs["supports_destination"] != true || gcs["origin"] != "built_in" {
		t.Errorf("gcs = %v", gcs)
	}

	for args, want := range map[string]string{`{"supports":"cdc"}`: "postgresql", `{"name_contains":"cloud stor"}`: "gcs", `{"supports":"Change Data Capture"}`: "postgresql"} {
		var a map[string]any
		_ = json.Unmarshal([]byte(args), &a)
		d := data(t, call(t, r, "list_connectors", a))
		conns, _ := d["connectors"].([]any)
		if len(conns) != 1 || conns[0].(map[string]any)["name"] != want {
			t.Errorf("%s: %v", args, d)
		}
	}
}

// diagnose_run says when rsync.ai marked the error as not retryable, so the
// answer does not tell the asker to run it again unchanged.
func TestDiagnoseRun_NotRetryable(t *testing.T) {
	for msg, want := range map[string]bool{
		"activity error (type: ExecutorActivityV2): [DETERMINISTIC:EXECUTION_FAILED] tried to convert to int64": false,
		"dial tcp: connection refused (type: V2_ACTIVITY_ERROR, retryable: false)":                              false,
		"dial tcp: connection refused (type: V2_ACTIVITY_ERROR, retryable: true)":                               true,
	} {
		h := leakyHandlers()
		h.GetExecution = jsonHandler(200, gin.H{"id": runID, "pipeline_id": pipeID, "status": "failed", "error_message": msg})
		r, _ := testServer(t, h, readerActor)
		d := data(t, call(t, r, "diagnose_run", validArgs["diagnose_run"]))
		hint, _ := d["hint"].(string)
		if want {
			if _, has := d["retryable"]; has || strings.Contains(hint, "not retryable") {
				t.Errorf("%q: %v", msg, d)
			}
			continue
		}
		if d["retryable"] != false || !strings.Contains(hint, "not retryable") {
			t.Errorf("%q: %v", msg, d)
		}
	}
}

// Bug class: a Stop that lands while a handler is running. The handler's
// query fails on the cancelled context, and the call was audited as an
// error (500) instead of a cancel.
func TestCall_CancelledDuringTheHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := leakyHandlers()
	h.GetExecution = func(c *gin.Context) {
		cancel()
		c.JSON(500, gin.H{"error": "context canceled"})
	}
	var rows []auditRow
	s, err := New(Options{
		Handlers: h,
		Actor:    func(*gin.Context) services.Actor { return assistantActor },
		Audit: func(_ *gin.Context, tool, rt, rid, outcome string, status int) {
			rows = append(rows, auditRow{tool, rt, rid, outcome, status})
		},
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, c, _ := inProcess(t, assistantActor)
	c.Request = c.Request.WithContext(ctx)
	text, isErr := s.Call(c, "get_run_status", json.RawMessage(`{"run_id":"`+runID+`"}`))
	if !isErr || !strings.Contains(text, "cancelled") {
		t.Fatalf("isError=%v text=%q", isErr, text)
	}
	if len(rows) != 1 || rows[0].outcome != "cancelled" || rows[0].status != statusCancelled {
		t.Fatalf("audit = %+v", rows)
	}

	// Control: a handler error on a live context is still an error.
	h.GetExecution = jsonHandler(500, gin.H{"error": "boom"})
	rows = nil
	s, _ = New(Options{Handlers: h, Actor: func(*gin.Context) services.Actor { return assistantActor },
		Audit: func(_ *gin.Context, tool, rt, rid, outcome string, status int) {
			rows = append(rows, auditRow{tool, rt, rid, outcome, status})
		}, Version: "test"})
	c.Request = c.Request.WithContext(context.Background())
	if _, isErr := s.Call(c, "get_run_status", json.RawMessage(`{"run_id":"`+runID+`"}`)); !isErr || len(rows) != 1 || rows[0].outcome != "error" {
		t.Fatalf("live error: isError=%v audit=%+v", isErr, rows)
	}
}
