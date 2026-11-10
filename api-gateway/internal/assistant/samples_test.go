package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// fakeSamples is the two sample-rows switches and a sampler in memory.
type fakeSamples struct {
	mu          sync.Mutex
	mode        string
	modeErr     error
	approved    map[string]bool
	approvedErr error
	setErr      error
	reads       []sampleArgs
	readErr     error
	cols        []string
	rows        []map[string]interface{}
	withheld    []string
}

func samplesOption(f *fakeSamples) Samples {
	if f == nil {
		return nil
	}
	return f
}

func (f *fakeSamples) Approved(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.approvedErr != nil {
		return nil, f.approvedErr
	}
	out := map[string]bool{}
	for k, v := range f.approved {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSamples) SetApproved(_ context.Context, entry string, allowed bool, _ string) (map[string]bool, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	f.mu.Lock()
	if f.approved == nil {
		f.approved = map[string]bool{}
	}
	if allowed {
		f.approved[entry] = true
	} else {
		delete(f.approved, entry)
	}
	f.mu.Unlock()
	return f.Approved(context.Background())
}

func (f *fakeSamples) Mode(_ context.Context, workspaceID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if workspaceID != "ws-1" {
		return "", errors.New("unknown workspace")
	}
	return f.mode, f.modeErr
}

func (f *fakeSamples) Read(_ *gin.Context, connectionID, table string, columns []string) ([]string, []map[string]interface{}, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, sampleArgs{ConnectionID: connectionID, Table: table, Columns: columns})
	if f.readErr != nil {
		return nil, nil, nil, f.readErr
	}
	return f.cols, f.rows, f.withheld, nil
}

const sampleConn = "6f1c3a52-6b1f-4c39-9d36-0c6f1f2b7a10"

func openSamples() *fakeSamples {
	return &fakeSamples{
		mode:     "sample_rows",
		approved: map[string]bool{"claude": true},
		cols:     []string{"id", "status"},
		rows: []map[string]interface{}{
			{"id": 1, "status": "shipped"},
			{"id": 2, "status": "pending"},
		},
		withheld: []string{"email"},
	}
}

func sampleCall(id string) ToolCall {
	return ToolCall{ID: id, Name: SampleRowsTool, Arguments: `{"connection_id":"` + sampleConn + `","table":"public.orders"}`}
}

func offers(req StepRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// The tool is offered only while the owner turned sample rows on AND an admin
// approved the picked entry; a read that fails counts as off.
func TestSampleRowsOfferedOnlyWhenBothSwitchesOn(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*fakeSamples)
		model string
		want  bool
	}{
		{"both on", func(*fakeSamples) {}, "claude", true},
		{"workspace names_only", func(f *fakeSamples) { f.mode = "names_only" }, "claude", false},
		{"entry not approved", func(*fakeSamples) {}, "chatgpt", false},
		{"no entry approved", func(f *fakeSamples) { f.approved = nil }, "claude", false},
		{"mode unreadable", func(f *fakeSamples) { f.modeErr = errors.New("db down") }, "claude", false},
		{"approvals unreadable", func(f *fakeSamples) { f.approvedErr = errors.New("db down") }, "claude", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := openSamples()
			tc.edit(f)
			h := newHarnessWith(t, &memStore{}, f, Limits{}, say("ok"))
			w, _ := h.ask(t, "u1", map[string]any{"question": "what do statuses look like?", "model": tc.model})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if got := offers(h.llm.reqs[0], SampleRowsTool); got != tc.want {
				t.Fatalf("sample_rows offered = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("not wired", func(t *testing.T) {
		h := newHarness(t, Limits{}, say("ok"))
		h.ask(t, "u1", map[string]any{"question": "q"})
		if offers(h.llm.reqs[0], SampleRowsTool) {
			t.Fatal("sample_rows offered with Options.Samples nil")
		}
	})
}

// A call while the tool is closed is dropped, never run: a model cannot read
// values by naming a tool it was not given.
func TestSampleRowsNotRunWhenClosed(t *testing.T) {
	f := openSamples()
	f.mode = "names_only"
	h := newHarnessWith(t, &memStore{}, f, Limits{}, callTools(sampleCall("c1")), say("ok"))
	h.ask(t, "u1", map[string]any{"question": "q"})
	if len(f.reads) != 0 {
		t.Fatalf("Read ran %d times while closed", len(f.reads))
	}
	for _, r := range h.audit.rows {
		if r.action == "assistant_sample_rows" {
			t.Fatal("a closed tool wrote a sample-rows audit row")
		}
	}
}

func TestSampleRowsCallIsMarkedAuditedAndRemembered(t *testing.T) {
	f := openSamples()
	h := newHarnessWith(t, &memStore{}, f, Limits{},
		callTools(sampleCall("c1")),
		callTools(ToolCall{ID: "c2", Name: "list_pipelines", Arguments: `{}`}),
		say("Statuses look like shipped and pending."),
	)
	w, out := h.ask(t, "u1", map[string]any{"question": "what do statuses look like?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.reads) != 1 || f.reads[0].ConnectionID != sampleConn || f.reads[0].Table != "public.orders" {
		t.Fatalf("reads = %+v", f.reads)
	}

	// The result is marked, fixed-shape, and carries no withheld value.
	msgs := h.llm.reqs[1].Messages
	result := msgs[len(msgs)-1]
	if result.Role != "tool" || result.Data != DataSampleRows {
		t.Fatalf("sample_rows result = %+v, want a tool message marked %q", result, DataSampleRows)
	}
	var got sampleResult
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
		t.Fatalf("result is not the fixed shape: %v: %s", err, result.Content)
	}
	if len(got.Rows) != 2 || got.Note != sampleNote || strings.Join(got.WithheldColumns, ",") != "email" {
		t.Fatalf("result = %+v", got)
	}

	// The turn the model wrote after reading values is marked; the one before is not.
	third := h.llm.reqs[2].Messages
	var assistants []Message
	for _, m := range third {
		if m.Role == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 2 || assistants[0].Data != "" || assistants[1].Data != DataSampleRows {
		t.Fatalf("assistant turns = %+v", assistants)
	}

	// One audit row, counts only.
	var rows []auditRow
	for _, r := range h.audit.rows {
		if r.action == "assistant_sample_rows" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || rows[0].resourceType != "connection" || rows[0].resourceID != sampleConn {
		t.Fatalf("audit rows = %+v", rows)
	}
	d := rows[0].details
	if d["table"] != "public.orders" || d["entry"] != "claude" || d["rows"] != 2 || d["withheld_columns"] != 1 || d["outcome"] != "ok" {
		t.Fatalf("audit details = %+v", d)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "shipped") {
		t.Fatalf("audit row carries a value: %s", raw)
	}

	// The panel shows counts, and the answer is remembered as sampled.
	if len(out.LookedAt) != 2 || out.LookedAt[0].Sample == nil || *out.LookedAt[0].Sample != (SampleSummary{Rows: 2, WithheldColumns: 1}) {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", out.ConversationID)]
	if len(turns) != 1 || turns[0].Samples != "claude" {
		t.Fatalf("stored turns = %+v", turns)
	}
}

// An error result is marked too: the guard refuses any unmarked result of a
// sample_rows call.
func TestSampleRowsErrorResultIsMarked(t *testing.T) {
	f := openSamples()
	f.readErr = errors.New("connection not found in this workspace")
	h := newHarnessWith(t, &memStore{}, f, Limits{}, callTools(sampleCall("c1")), say("ok"))
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	msgs := h.llm.reqs[1].Messages
	result := msgs[len(msgs)-1]
	if result.Data != DataSampleRows || !strings.HasPrefix(result.Content, `{"error":`) {
		t.Fatalf("error result = %+v", result)
	}
	if len(out.LookedAt) != 1 || out.LookedAt[0].OK || out.LookedAt[0].Sample != nil {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", out.ConversationID)]
	if len(turns) != 1 || turns[0].Samples != "" {
		t.Fatalf("an answer that read no value is remembered as sampled: %+v", turns)
	}
}

func TestSampleRowsCappedAtThreePerQuestion(t *testing.T) {
	f := openSamples()
	h := newHarnessWith(t, &memStore{}, f, Limits{},
		callTools(sampleCall("c1"), sampleCall("c2"), sampleCall("c3"), sampleCall("c4")),
		say("ok"),
	)
	h.ask(t, "u1", map[string]any{"question": "q"})
	if len(f.reads) != maxSampleCalls {
		t.Fatalf("Read ran %d times, want %d", len(f.reads), maxSampleCalls)
	}
	msgs := h.llm.reqs[1].Messages
	last := msgs[len(msgs)-1]
	if last.Data != DataSampleRows || !strings.Contains(last.Content, "at most 3 times") {
		t.Fatalf("fourth result = %+v", last)
	}
}

func TestSampleRowsRefusesBadArguments(t *testing.T) {
	for name, args := range map[string]string{
		"no connection":    `{"table":"t"}`,
		"connection name":  `{"connection_id":"prod","table":"t"}`,
		"no table":         `{"connection_id":"` + sampleConn + `"}`,
		"control in table": `{"connection_id":"` + sampleConn + `","table":"a\nb"}`,
		"extra field":      `{"connection_id":"` + sampleConn + `","table":"t","limit":500}`,
		"too many columns": `{"connection_id":"` + sampleConn + `","table":"t","columns":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := openSamples()
			h := newHarnessWith(t, &memStore{}, f, Limits{},
				callTools(ToolCall{ID: "c1", Name: SampleRowsTool, Arguments: args}), say("ok"))
			h.ask(t, "u1", map[string]any{"question": "q"})
			if len(f.reads) != 0 {
				t.Fatalf("Read ran on %s", args)
			}
			msgs := h.llm.reqs[1].Messages
			if last := msgs[len(msgs)-1]; last.Data != DataSampleRows || !strings.Contains(last.Content, "Not run") {
				t.Fatalf("result = %+v", last)
			}
		})
	}
}

// A result too large for one tool result loses columns from the end, never
// its shape.
func TestSampleRowsFitsOneToolResult(t *testing.T) {
	f := openSamples()
	f.cols = nil
	row := map[string]interface{}{}
	for i := 0; i < 20; i++ {
		col := "c" + string(rune('a'+i))
		f.cols = append(f.cols, col)
		row[col] = strings.Repeat("x", 64)
	}
	f.rows = []map[string]interface{}{row, row, row, row, row}
	h := newHarnessWith(t, &memStore{}, f, Limits{ToolResultChars: 2000}, callTools(sampleCall("c1")), say("ok"))
	h.ask(t, "u1", map[string]any{"question": "q"})
	msgs := h.llm.reqs[1].Messages
	content := msgs[len(msgs)-1].Content
	var got sampleResult
	if err := json.Unmarshal([]byte(content), &got); err != nil || len(content) > 2000 {
		t.Fatalf("result %d chars, err %v", len(content), err)
	}
	if got.ColumnsNotShown == 0 || len(got.Columns)+got.ColumnsNotShown != 20 {
		t.Fatalf("columns %d shown, %d not shown", len(got.Columns), got.ColumnsNotShown)
	}
	for _, r := range got.Rows {
		if len(r) != len(got.Columns) {
			t.Fatalf("a row keeps a column the result does not list: %v", r)
		}
	}
}

// What the egress guard reports for a result reaches the panel, and an answer
// whose values the guard did not send is not remembered as sampled.
func TestSampleRowsGuardOutcomeReachesThePanel(t *testing.T) {
	f := openSamples()
	h := newHarnessWith(t, &memStore{}, f, Limits{},
		callTools(sampleCall("c1")),
		func(req StepRequest) (*StepResponse, error) {
			resp, _ := say("ok")(req)
			resp.DataResults = []DataResult{{ToolCallID: "c1", Sent: false, WithheldColumns: 2}}
			return resp, nil
		},
	)
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if s := out.LookedAt[0].Sample; s == nil || !s.NotSent || s.WithheldColumns != 3 {
		t.Fatalf("sample summary = %+v", s)
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", out.ConversationID)]
	if turns[0].Samples != "" {
		t.Fatalf("turn remembered as sampled though nothing was sent: %+v", turns[0])
	}
}

// An answer formed from sample rows is sent again only to the entry that read
// them, and only while sample rows are on.
func TestHistoryWithholdsASampledAnswer(t *testing.T) {
	s := &Service{limits: Limits{}.withDefaults()}
	turns := []Turn{{Question: "q1", Answer: "plain"}, {Question: "q2", Answer: "status is shipped", Samples: "claude"}}

	open := s.historyMessages(turns, "claude", true)
	if open[3].Content != "status is shipped" || open[3].Data != DataSampleRows || open[1].Data != "" {
		t.Fatalf("open, same entry: %+v", open)
	}
	for name, got := range map[string][]Message{
		"closed":      s.historyMessages(turns, "claude", false),
		"other entry": s.historyMessages(turns, "chatgpt", true),
	} {
		if got[3].Content != withheldAnswer || got[3].Data != "" {
			t.Fatalf("%s: sampled answer = %+v", name, got[3])
		}
		if got[1].Content != "plain" {
			t.Fatalf("%s: an unsampled answer was withheld: %+v", name, got[1])
		}
	}
}

func TestAdminSetsSampleRowsApproval(t *testing.T) {
	f := openSamples()
	f.approved = nil
	h := newHarnessWith(t, &memStore{}, f, Limits{})
	w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/gemini/sample-rows", "admin-1", map[string]any{"allowed": true})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var view struct {
		Entries []adminEntry `json:"entries"`
		Known   bool         `json:"sample_rows_known"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range view.Entries {
		got[e.Entry] = e.SampleRows
	}
	if !view.Known || !got["gemini"] || got["claude"] || got["chatgpt"] {
		t.Fatalf("view = %+v", view)
	}
	last := h.audit.rows[len(h.audit.rows)-1]
	if last.action != "admin_set_assistant_sample_rows" || last.resourceID != "gemini" || last.details["allowed"] != true {
		t.Fatalf("audit = %+v", last)
	}

	for body, want := range map[string]int{`{}`: http.StatusBadRequest, `{"allowed":"yes"}`: http.StatusBadRequest} {
		if w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/gemini/sample-rows", "admin-1", body); w.Code != want {
			t.Fatalf("%s: status %d, want %d", body, w.Code, want)
		}
	}
	if w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/llama/sample-rows", "admin-1", map[string]any{"allowed": true}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown entry: status %d", w.Code)
	}

	// Not wired: no route, and the list says the switch is unknown.
	plain := newHarness(t, Limits{})
	if w := plain.do(t, http.MethodPut, "/api/v1/admin/assistant/models/gemini/sample-rows", "admin-1", map[string]any{"allowed": true}); w.Code != http.StatusNotFound {
		t.Fatalf("route exists without Samples: status %d", w.Code)
	}
	if w := plain.do(t, http.MethodGet, "/api/v1/admin/assistant/models", "admin-1", nil); !strings.Contains(w.Body.String(), `"sample_rows_known":false`) {
		t.Fatalf("list without Samples: %s", w.Body.String())
	}
}

// An install that does not allow sample rows refuses the approval with 403
// and a code the UI can tell apart from a failed save; nothing is audited.
func TestAdminSampleRowsRefusedOnThisInstall(t *testing.T) {
	f := openSamples()
	f.setErr = handlers.ErrSampleRowsDisabled
	h := newHarnessWith(t, &memStore{}, f, Limits{})
	before := len(h.audit.rows)
	w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/gemini/sample-rows", "admin-1", map[string]any{"allowed": true})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"sample_rows_disabled"`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(h.audit.rows) != before {
		t.Fatalf("audited a refused approval: %+v", h.audit.rows[before:])
	}
}

// The rsync.ai MCP server stays names-only: no agent gateway tool is
// sample_rows, so an agent token can never read a row value through it.
func TestAgentGatewayHasNoSampleRowsTool(t *testing.T) {
	gw, err := agentgw.New(agentgw.Options{
		Handlers: agentgw.DefaultHandlers(),
		Actor:    func(*gin.Context) services.Actor { return services.Actor{} },
		Audit:    func(*gin.Context, string, string, string, string, int) {},
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	specs := gw.ToolSpecs()
	if len(specs) == 0 {
		t.Fatal("the agent gateway lists no tools; this test would pass on nothing")
	}
	for _, spec := range specs {
		if spec.Name == SampleRowsTool || strings.Contains(spec.Name, "sample") {
			t.Fatalf("agent gateway tool %q reads sample rows", spec.Name)
		}
	}
}
