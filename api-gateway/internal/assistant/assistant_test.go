package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// ---- fakes ----------------------------------------------------------------

type stepFunc func(StepRequest) (*StepResponse, error)

// fakeStepper plays a script of model turns and records every request.
type fakeStepper struct {
	mu        sync.Mutex
	script    []stepFunc
	reqs      []StepRequest
	providers *Providers
	provErr   error
	// onStep, when set, runs before step i is played, with the step's
	// context; an error it returns is the step's error.
	onStep func(ctx context.Context, i int) error
}

func (f *fakeStepper) Step(ctx context.Context, req StepRequest) (*StepResponse, error) {
	f.mu.Lock()
	req.Messages = append([]Message(nil), req.Messages...)
	f.reqs = append(f.reqs, req)
	i := len(f.reqs) - 1
	onStep := f.onStep
	f.mu.Unlock()
	if onStep != nil {
		if err := onStep(ctx, i); err != nil {
			return nil, err
		}
	}
	if i >= len(f.script) {
		return say("done")(req)
	}
	return f.script[i](req)
}

func (f *fakeStepper) Providers(context.Context) (*Providers, error) {
	if f.provErr != nil {
		return nil, f.provErr
	}
	if f.providers == nil {
		return &Providers{PanelDefaults: map[string]string{"claude": "claude-sonnet-5-5", "chatgpt": "gpt-4o-mini", "gemini": "gemini-2.5-flash"}}, nil
	}
	return f.providers, nil
}

func say(content string) stepFunc {
	return func(req StepRequest) (*StepResponse, error) {
		return &StepResponse{Content: content, Entry: req.Entry, Label: labels[req.Entry], Model: "m-" + req.Entry, Source: "env", CostCents: 1}, nil
	}
}

func callTools(calls ...ToolCall) stepFunc {
	return func(req StepRequest) (*StepResponse, error) {
		return &StepResponse{ToolCalls: calls, Entry: req.Entry, Label: labels[req.Entry], Model: "m-" + req.Entry, Source: "env", CostCents: 1}, nil
	}
}

type toolCall struct {
	name string
	args string
}

// fakeTools offers list_pipelines and get_pipeline.
type fakeTools struct {
	mu      sync.Mutex
	calls   []toolCall
	results map[string]string
	errs    map[string]bool
	// made is the resource id a tool returns, by tool name.
	made map[string]string
	// onCall, when set, runs as each tool is called.
	onCall func(name string)
	// extra are offered after the two above.
	extra []string
	// audited overrides a tool's audit outcome and status, by tool name.
	audited map[string]agentgw.Audited
	// guides are the guide each tool names, by tool name.
	guides map[string]string
	// specs, when set, are the tools offered in place of all the above.
	specs []agentgw.ToolSpec
}

func (f *fakeTools) ToolSpecs() []agentgw.ToolSpec {
	if f.specs != nil {
		return f.specs
	}
	specs := []agentgw.ToolSpec{
		{Name: "list_pipelines", Description: "List pipelines", InputSchema: map[string]any{"type": "object"}},
		{Name: "get_pipeline", Description: "One pipeline", InputSchema: map[string]any{"type": "object"}},
	}
	for _, name := range f.extra {
		specs = append(specs, agentgw.ToolSpec{Name: name, Description: name, InputSchema: map[string]any{"type": "object"}})
	}
	for i := range specs {
		specs[i].Guide = f.guides[specs[i].Name]
	}
	return specs
}

func (f *fakeTools) Run(_ *gin.Context, name string, arguments json.RawMessage) agentgw.Result {
	if f.onCall != nil {
		f.onCall(name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, toolCall{name, string(arguments)})
	out := agentgw.Result{Text: `{"items":[]}`, ResourceID: f.made[name], Audited: agentgw.Audited{Outcome: "ok", Status: 200}}
	if r, ok := f.results[name]; ok {
		out.Text, out.IsError = r, f.errs[name]
		if out.IsError {
			out.Audited = agentgw.Audited{Outcome: "error", Status: 500}
		}
	}
	if a, ok := f.audited[name]; ok {
		out.Audited = a
	}
	return out
}

type savedKey struct{ key, model, adminID string }

// fakeKeys is the panel's key table in memory.
type fakeKeys struct {
	mu     sync.Mutex
	rows   map[string]savedKey
	getErr error
	gets   int
	lists  int
}

func (f *fakeKeys) Get(_ context.Context, entry string) (string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return "", "", false, f.getErr
	}
	r, ok := f.rows[entry]
	return r.key, r.model, ok, nil
}

func (f *fakeKeys) List(context.Context) ([]KeyRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	var out []KeyRow
	for _, e := range Entries {
		if r, ok := f.rows[e]; ok {
			out = append(out, KeyRow{Entry: e, Model: r.model, UpdatedBy: "admin@example.com", UpdatedAt: time.Unix(1700000000, 0).UTC()})
		}
	}
	return out, nil
}

func (f *fakeKeys) SetKey(_ context.Context, entry, key, model, adminID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]savedKey{}
	}
	f.rows[entry] = savedKey{key, model, adminID}
	return nil
}

func (f *fakeKeys) SetModel(_ context.Context, entry, model, adminID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[entry]
	if !ok {
		return false, nil
	}
	r.model, r.adminID = model, adminID
	f.rows[entry] = r
	return true, nil
}

func (f *fakeKeys) Remove(_ context.Context, entry string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[entry]; !ok {
		return false, nil
	}
	delete(f.rows, entry)
	return true, nil
}

type memStore struct {
	mu      sync.Mutex
	convs   map[string][]Turn
	stops   map[string]bool
	markErr error
}

func (m *memStore) Load(_ context.Context, key string) ([]Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Turn(nil), m.convs[key]...), nil
}

func (m *memStore) Save(_ context.Context, key string, turns []Turn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.convs == nil {
		m.convs = map[string][]Turn{}
	}
	m.convs[key] = append([]Turn(nil), turns...)
	return nil
}

func (m *memStore) MarkStopped(_ context.Context, key string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.markErr != nil {
		return m.markErr
	}
	if m.stops == nil {
		m.stops = map[string]bool{}
	}
	m.stops[key] = true
	return nil
}

func (m *memStore) Stopped(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stops[key], nil
}

type auditRow struct {
	action, resourceType, resourceID string
	details                          map[string]interface{}
}

type auditLog struct {
	mu   sync.Mutex
	rows []auditRow
}

func (a *auditLog) record(_ *gin.Context, action, resourceType, resourceID string, details map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, auditRow{action, resourceType, resourceID, details})
}

// ---- harness --------------------------------------------------------------

type harness struct {
	llm    *fakeStepper
	tools  *fakeTools
	keys   *fakeKeys
	store  *memStore
	audit  *auditLog
	quota  bool
	router *gin.Engine
	// metered are the connection ids Options.MeterSQL was called with.
	metered []string
}

// newHarness mounts the assistant behind a stand-in for the session
// middleware: X-Test-User and X-Test-Workspace name the signed-in user.
func newHarness(t *testing.T, limits Limits, script ...stepFunc) *harness {
	t.Helper()
	return newHarnessOn(t, &memStore{}, limits, script...)
}

// newHarnessOn is newHarness on a given store: two harnesses on one store are
// two api-gateway replicas sharing one Redis.
func newHarnessOn(t *testing.T, store *memStore, limits Limits, script ...stepFunc) *harness {
	t.Helper()
	return newHarnessWith(t, store, nil, limits, script...)
}

// newHarnessWith is newHarnessOn with sample rows wired; samples nil leaves
// Options.Samples nil.
func newHarnessWith(t *testing.T, store *memStore, samples *fakeSamples, limits Limits, script ...stepFunc) *harness {
	t.Helper()
	return newHarnessSQL(t, store, samples, nil, limits, script...)
}

// newHarnessSQL is newHarnessWith with generate_sql's writer wired (nil: not).
func newHarnessSQL(t *testing.T, store *memStore, samples *fakeSamples, sql *fakeSQL, limits Limits, script ...stepFunc) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &harness{
		llm:   &fakeStepper{script: script},
		tools: &fakeTools{results: map[string]string{}, errs: map[string]bool{}},
		keys:  &fakeKeys{rows: map[string]savedKey{}},
		store: store,
		audit: &auditLog{},
		quota: true,
	}
	r := gin.New()
	api := r.Group("/api/v1", func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
			c.Set("admin_user_id", u)
		}
		c.Next()
	})
	admin := api.Group("/admin")
	_, err := Mount(api, admin, Options{
		Tools: h.tools,
		LLM:   h.llm,
		Keys:  h.keys,
		Store: h.store,
		Actor: func(c *gin.Context) services.Actor {
			return services.Actor{UserID: c.GetString("user_id"), WorkspaceID: c.GetHeader("X-Test-Workspace"), Channel: services.ChannelAssistant}
		},
		QuotaOK: func(context.Context, string) bool { return h.quota },
		Audit:   h.audit.record,
		Samples: samplesOption(samples),
		SQL:     sqlOption(sql),
		MeterSQL: func(_ *gin.Context, connectionID string) {
			h.metered = append(h.metered, connectionID)
		},
		Limits: limits,
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	h.router = r
	return h
}

func (h *harness) do(t *testing.T, method, path, user string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
		req.Header.Set("X-Test-Workspace", "ws-1")
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func (h *harness) ask(t *testing.T, user string, body map[string]any) (*httptest.ResponseRecorder, Answer) {
	t.Helper()
	if _, ok := body["model"]; !ok {
		body["model"] = "claude"
	}
	w := h.do(t, http.MethodPost, "/api/v1/assistant/messages", user, body)
	var out Answer
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("answer is not JSON: %v: %s", err, w.Body.String())
		}
	}
	return w, out
}

func roles(msgs []Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.Role
	}
	return strings.Join(parts, ",")
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error
}

// ---- the loop -------------------------------------------------------------

func TestAnswerWithoutTools(t *testing.T) {
	h := newHarness(t, Limits{}, say("You have two pipelines."))
	w, out := h.ask(t, "u1", map[string]any{"question": "  how many pipelines?  "})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if out.Answer != "You have two pipelines." || out.Stopped != "" || out.CostCents != 1 {
		t.Fatalf("answer = %+v", out)
	}
	if out.Model != (ModelInfo{Entry: "claude", Label: "Claude", Model: "m-claude"}) {
		t.Fatalf("model = %+v", out.Model)
	}
	if !strings.Contains(w.Body.String(), `"looked_at":[]`) {
		t.Fatalf("looked_at should be an empty list, not null: %s", w.Body.String())
	}
	if !conversationIDPattern.MatchString(out.ConversationID) {
		t.Fatalf("conversation id %q", out.ConversationID)
	}
	if len(h.llm.reqs) != 1 {
		t.Fatalf("steps = %d, want 1", len(h.llm.reqs))
	}
	req := h.llm.reqs[0]
	if req.ToolChoice != "auto" || len(req.Tools) != 2 || req.UserID != "u1" || req.MaxTokens != 1024 {
		t.Fatalf("step request = %+v", req)
	}
	if req.APIKey != "" {
		t.Fatalf("no panel key is set, so none is sent; got one")
	}
	if roles(req.Messages) != "user" || req.Messages[0].Content != "how many pipelines?" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", out.ConversationID)]
	if len(turns) != 1 || !reflect.DeepEqual(turns[0], Turn{Question: "how many pipelines?", Answer: "You have two pipelines."}) {
		t.Fatalf("stored turns = %+v", turns)
	}
}

// The id a successful call made reaches looked_at, so the page can show a
// plan_pipeline's plan as a card; a failed call carries none.
func TestLookedAtCarriesTheMadeResource(t *testing.T) {
	h := newHarness(t, Limits{},
		callTools(
			ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{}`},
			ToolCall{ID: "c2", Name: "get_pipeline", Arguments: `{"pipeline_id":"p9"}`},
		),
		say("done"),
	)
	h.tools.made = map[string]string{"list_pipelines": "plan-1", "get_pipeline": "p9"}
	h.tools.results["get_pipeline"] = "pipeline not found"
	h.tools.errs["get_pipeline"] = true

	_, out := h.ask(t, "u1", map[string]any{"question": "plan it"})
	if len(out.LookedAt) != 2 || out.LookedAt[0].ResourceID != "plan-1" || out.LookedAt[1].ResourceID != "" {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
}

func TestEmptyModelAnswerIsReplaced(t *testing.T) {
	h := newHarness(t, Limits{}, say("   "), say(""))
	_, out := h.ask(t, "u1", map[string]any{"question": "hi"})
	if out.Answer != "The model returned an empty answer. Try asking again." {
		t.Fatalf("answer = %q", out.Answer)
	}
}

// Found by the accuracy eval (s05, 2026-10-11): Gemini ended its first turn
// with no words and no tool call, and the person read "The model returned an
// empty answer. Try asking again." Asked again, it answers. One empty turn a
// question is now asked again, with the same request, and both calls are paid.
func TestAnEmptyTurnIsAskedAgainOnce(t *testing.T) {
	h := newHarness(t, Limits{}, say(""), say("You have 3 pipelines."))
	_, out := h.ask(t, "u1", map[string]any{"question": "how many pipelines"})
	if out.Answer != "You have 3 pipelines." {
		t.Fatalf("answer = %q", out.Answer)
	}
	if len(h.llm.reqs) != 2 {
		t.Fatalf("steps = %d, want 2", len(h.llm.reqs))
	}
	if !reflect.DeepEqual(h.llm.reqs[0].Messages, h.llm.reqs[1].Messages) || h.llm.reqs[1].ToolChoice != h.llm.reqs[0].ToolChoice {
		t.Fatalf("the second ask is not the same request:\n%+v\n%+v", h.llm.reqs[0], h.llm.reqs[1])
	}
	if out.CostCents != 2 || len(out.Usage.Steps) != 2 {
		t.Fatalf("cost = %d, usage steps = %d; want both calls counted", out.CostCents, len(out.Usage.Steps))
	}

	// Once a question: a second empty turn is the fallback.
	h = newHarness(t, Limits{}, say(""), callTools(ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{}`}), say(""), say("late"))
	_, out = h.ask(t, "u1", map[string]any{"question": "hi"})
	if out.Answer != "The model returned an empty answer. Try asking again." || len(h.llm.reqs) != 3 {
		t.Fatalf("answer = %q after %d steps", out.Answer, len(h.llm.reqs))
	}

	// Cut off or filtered is not empty by choice: not asked again.
	for _, reason := range []string{"length", "content_filter"} {
		h = newHarness(t, Limits{}, finishWith("", reason), say("late"))
		h.ask(t, "u1", map[string]any{"question": "hi"})
		if len(h.llm.reqs) != 1 {
			t.Fatalf("%s: steps = %d, want 1", reason, len(h.llm.reqs))
		}
	}
}

func finishWith(content, reason string) stepFunc {
	return func(req StepRequest) (*StepResponse, error) {
		r, _ := say(content)(req)
		r.FinishReason = reason
		return r, nil
	}
}

// Found asking Ask rsync.ai about a BigQuery table (2026-10-09): generate_sql
// failed twice, the model's last turn was empty, and the person read "The
// model returned an empty answer. Try asking again." The answer now says what
// failed.
func TestAnEmptyAnswerAfterFailedCallsSaysWhatFailed(t *testing.T) {
	f := writesSQL()
	f.err = errors.New("Unknown column: merchant_name")
	h := newHarnessSQL(t, &memStore{}, nil, f, Limits{},
		callTools(sqlCall("c1"), ToolCall{ID: "c2", Name: "get_pipeline", Arguments: `{"pipeline_id":"p9"}`}),
		callTools(sqlCall("c3")),
		say(""), say(""),
	)
	h.tools.results["get_pipeline"] = "pipeline not found"
	h.tools.errs["get_pipeline"] = true

	_, out := h.ask(t, "u1", map[string]any{"question": "top merchant by confirmed fraud"})
	for _, want := range []string{"generate_sql: Unknown column: merchant_name", "get_pipeline: pipeline not found"} {
		if !strings.Contains(out.Answer, want) {
			t.Fatalf("the answer does not name the failed call %q:\n%s", want, out.Answer)
		}
	}
	if strings.Count(out.Answer, "merchant_name") != 1 {
		t.Fatalf("the same failure is listed once:\n%s", out.Answer)
	}
	if strings.Contains(out.Answer, "{") {
		t.Fatalf("the error is the message, not its JSON:\n%s", out.Answer)
	}
}

// A call that succeeded is not a failure: control for the test above.
func TestAnEmptyAnswerAfterGoodCallsKeepsItsText(t *testing.T) {
	h := newHarness(t, Limits{}, callTools(ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{}`}), say(""), say(""))
	_, out := h.ask(t, "u1", map[string]any{"question": "hi"})
	if out.Answer != "The model returned an empty answer. Try asking again." {
		t.Fatalf("answer = %q", out.Answer)
	}
}

// The provider's stop reason was never read, so an answer cut off at the
// length limit read as finished, and an empty one as the model's choice.
func TestTheStopReasonIsReported(t *testing.T) {
	cases := []struct {
		content, reason, want string
	}{
		{"Alerts by severity: low 8,822, medium", "length", "Alerts by severity: low 8,822, medium\n\n(The answer was cut off: the model reached its length limit.)"},
		{"", "length", "The model ran out of room before it wrote an answer. Ask a narrower question."},
		{"", "content_filter", "The model provider declined to answer. Try asking again."},
		{"All done.", "stop", "All done."},
	}
	for _, tc := range cases {
		h := newHarness(t, Limits{}, finishWith(tc.content, tc.reason))
		_, out := h.ask(t, "u1", map[string]any{"question": "hi"})
		if out.Answer != tc.want {
			t.Errorf("%q, %s: answer = %q, want %q", tc.content, tc.reason, out.Answer, tc.want)
		}
	}
}

func TestToolRoundTrip(t *testing.T) {
	h := newHarness(t, Limits{},
		callTools(
			ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{"limit":5}`},
			ToolCall{ID: "c2", Name: "get_pipeline", Arguments: `{"pipeline_id":"p9"}`},
		),
		say("p9 does not exist; you have no pipelines."),
	)
	h.tools.results["list_pipelines"] = `{"items":[]}`
	h.tools.results["get_pipeline"] = "pipeline not found"
	h.tools.errs["get_pipeline"] = true

	w, out := h.ask(t, "u1", map[string]any{"question": "what about p9?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(h.tools.calls) != 2 || h.tools.calls[0] != (toolCall{"list_pipelines", `{"limit":5}`}) || h.tools.calls[1] != (toolCall{"get_pipeline", `{"pipeline_id":"p9"}`}) {
		t.Fatalf("tool calls = %+v", h.tools.calls)
	}
	if len(out.LookedAt) != 2 || !out.LookedAt[0].OK || out.LookedAt[1].OK || out.LookedAt[1].Tool != "get_pipeline" {
		t.Fatalf("looked_at = %+v", out.LookedAt)
	}
	if out.CostCents != 2 {
		t.Fatalf("cost = %d, want both steps summed", out.CostCents)
	}
	second := h.llm.reqs[1].Messages
	if roles(second) != "user,assistant,tool,tool" {
		t.Fatalf("second step roles = %s", roles(second))
	}
	if ids := []string{second[1].ToolCalls[0].ID, second[1].ToolCalls[1].ID, second[2].ToolCallID, second[3].ToolCallID}; strings.Join(ids, ",") != "c1,c2,c1,c2" {
		t.Fatalf("call ids = %v", ids)
	}
	if second[3].Content != "pipeline not found" {
		t.Fatalf("a tool error goes back to the model as its result; got %q", second[3].Content)
	}
}

// Bug class: a token the provider hands out on one turn and checks on the
// next, dropped between them. Gemini 3 signs the first call of each turn and
// refuses the next step when the call comes back unsigned (2026-11-03).
func TestASignatureGoesBackWithItsCallAndNowhereElse(t *testing.T) {
	h := newHarness(t, Limits{},
		callTools(
			ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{}`, Signature: "Q2lTaWduZWQ="},
			ToolCall{ID: "c2", Name: "get_pipeline", Arguments: `{"pipeline_id":"p9"}`},
		),
		say("one pipeline"),
	)
	w, _ := h.ask(t, "u1", map[string]any{"question": "what is it called?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	calls := h.llm.reqs[1].Messages[1].ToolCalls
	if len(calls) != 2 || calls[0].Signature != "Q2lTaWduZWQ=" || calls[1].Signature != "" {
		t.Fatalf("second step's calls = %+v, want the first one signed", calls)
	}
	if strings.Contains(w.Body.String(), "Q2lTaWduZWQ=") {
		t.Fatalf("the signature reached the browser: %s", w.Body.String())
	}
}

func TestStepCapEndsWithToolChoiceNone(t *testing.T) {
	loop := callTools(ToolCall{ID: "c", Name: "list_pipelines", Arguments: `{}`})
	h := newHarness(t, Limits{Steps: 3}, loop, loop, loop)
	w, out := h.ask(t, "u1", map[string]any{"question": "loop forever"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var choices []string
	for _, r := range h.llm.reqs {
		choices = append(choices, r.ToolChoice)
	}
	if strings.Join(choices, ",") != "auto,auto,none" {
		t.Fatalf("tool_choice per step = %v", choices)
	}
	if out.Stopped != "limit" || !strings.Contains(out.Answer, "I looked at 2 things") {
		t.Fatalf("answer = %+v", out)
	}
	if len(h.tools.calls) != 2 {
		t.Fatalf("tools run = %d, want 2 (the last step's calls are not run)", len(h.tools.calls))
	}
	last := h.llm.reqs[2].Messages
	if roles(last) != "user,assistant,tool,assistant,tool" {
		t.Fatalf("roles = %s", roles(last))
	}
	// The model reused id "c": the second use is renamed so ids stay unique.
	if last[1].ToolCalls[0].ID != "c" || last[3].ToolCalls[0].ID != "call_2_0" || last[4].ToolCallID != "call_2_0" {
		t.Fatalf("ids = %q %q %q", last[1].ToolCalls[0].ID, last[3].ToolCalls[0].ID, last[4].ToolCallID)
	}
}

func TestLastStepThatAnswersIsNotALimit(t *testing.T) {
	h := newHarness(t, Limits{Steps: 2}, callTools(ToolCall{ID: "c", Name: "list_pipelines"}), say("none"))
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if out.Stopped != "" || out.Answer != "none" || h.llm.reqs[1].ToolChoice != "none" {
		t.Fatalf("answer = %+v, choice = %q", out, h.llm.reqs[1].ToolChoice)
	}
}

func TestToolCallCap(t *testing.T) {
	h := newHarness(t, Limits{ToolCalls: 2},
		callTools(
			ToolCall{ID: "a", Name: "list_pipelines", Arguments: `{}`},
			ToolCall{ID: "b", Name: "list_pipelines", Arguments: `{}`},
			ToolCall{ID: "c", Name: "list_pipelines", Arguments: `{}`},
		),
		say("answered from two"),
	)
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if len(h.tools.calls) != 2 {
		t.Fatalf("tools run = %d, want 2", len(h.tools.calls))
	}
	second := h.llm.reqs[1]
	if second.ToolChoice != "none" {
		t.Fatalf("after the cap the next step must forbid tools; got %q", second.ToolChoice)
	}
	if got := second.Messages[4].Content; !strings.HasPrefix(got, "Not run: the limit of tool calls") || second.Messages[4].ToolCallID != "c" {
		t.Fatalf("third call's result = %+v", second.Messages[4])
	}
	if out.Answer != "answered from two" || len(out.LookedAt) != 2 {
		t.Fatalf("answer = %+v", out)
	}
}

func TestMalformedCallsAreRepairedNotRun(t *testing.T) {
	h := newHarness(t, Limits{},
		callTools(
			ToolCall{ID: "bad id!", Name: "list_pipelines", Arguments: `{"a":1}`},
			ToolCall{ID: "", Name: "list_pipelines", Arguments: `not json`},
			ToolCall{ID: "x", Name: "drop_database", Arguments: `{}`},
			ToolCall{ID: "y", Name: "list_pipelines", Arguments: `{"s":"` + strings.Repeat("a", maxArgumentChars) + `"}`},
			ToolCall{ID: "z", Name: "list_pipelines", Arguments: `[1,2]`},
		),
		say("ok"),
	)
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if len(h.tools.calls) != 1 || h.tools.calls[0].args != `{"a":1}` {
		t.Fatalf("only the valid call runs; ran %+v", h.tools.calls)
	}
	msgs := h.llm.reqs[1].Messages
	if roles(msgs) != "user,assistant,tool,tool,tool,tool" {
		t.Fatalf("roles = %s (the unknown tool must be dropped, not answered)", roles(msgs))
	}
	turn := msgs[1]
	wantIDs := []string{"call_1_0", "call_1_1", "y", "z"}
	for i, tc := range turn.ToolCalls {
		if tc.ID != wantIDs[i] || msgs[2+i].ToolCallID != wantIDs[i] {
			t.Fatalf("call %d id = %q / result id %q, want %q", i, tc.ID, msgs[2+i].ToolCallID, wantIDs[i])
		}
		if tc.Name == "drop_database" {
			t.Fatalf("an unknown tool reached the next step")
		}
		if i > 0 && tc.Arguments != "{}" {
			t.Fatalf("call %d arguments = %q, want {}", i, tc.Arguments)
		}
		if i > 0 && !strings.HasPrefix(msgs[2+i].Content, "Not run: the arguments must be a JSON object") {
			t.Fatalf("call %d result = %q", i, msgs[2+i].Content)
		}
	}
	var dropped bool
	for _, l := range out.LookedAt {
		if l.Tool == "drop_database" && !l.OK {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("looked_at should name the unknown tool as not ok: %+v", out.LookedAt)
	}
}

// Bug class: a turn whose every call named a tool that does not exist ended
// the question. On prod (2026-10-10) Gemini called a tool rsync.ai does not
// have and the user read "The model asked for a tool rsync.ai does not have"
// in place of an answer. The model is told which tools exist and asked again;
// the made-up call never enters the conversation, which llm-service refuses.
func TestUnknownToolIsCorrectedAndTheModelAsksAgain(t *testing.T) {
	h := newHarness(t, Limits{},
		func(req StepRequest) (*StepResponse, error) {
			r, _ := callTools(ToolCall{ID: "x", Name: "PlanWorkflowSteps", Arguments: `{}`})(req)
			r.Content = "Let me plan that."
			return r, nil
		},
		callTools(ToolCall{ID: "y", Name: "list_pipelines", Arguments: `{}`}),
		say("two pipelines"),
	)
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if len(h.tools.calls) != 1 || h.tools.calls[0].name != "list_pipelines" || out.Answer != "two pipelines" {
		t.Fatalf("tools=%+v answer=%q", h.tools.calls, out.Answer)
	}
	if len(h.llm.reqs) != 3 {
		t.Fatalf("steps = %d, want 3", len(h.llm.reqs))
	}
	second := h.llm.reqs[1].Messages
	if roles(second) != "user,assistant,user" || second[1].Content != "Let me plan that." || len(second[1].ToolCalls) != 0 {
		t.Fatalf("second step = %s %+v", roles(second), second)
	}
	want := "Not run: PlanWorkflowSteps is not a tool here. The tools are: get_pipeline, list_pipelines. Call one of them by its exact name, or answer."
	if second[2].Content != want {
		t.Fatalf("note = %q, want %q", second[2].Content, want)
	}
	for i, req := range h.llm.reqs {
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				if tc.Name == "PlanWorkflowSteps" {
					t.Fatalf("step %d carries the made-up call", i+1)
				}
			}
		}
	}
}

// After sample_rows a made-up name may quote a value, and the note is an
// unmarked user message: it names the real tools only.
func TestUnknownToolNoteDropsTheNameOnceValuesWereRead(t *testing.T) {
	tools := []Tool{{Name: "list_pipelines"}, {Name: SampleRowsTool}}
	got := unknownToolNote([]string{"acme_corp"}, tools, true)
	if strings.Contains(got, "acme_corp") || !strings.HasPrefix(got, "Not run: that tool does not exist here. The tools are: list_pipelines, sample_rows.") {
		t.Fatalf("note = %q", got)
	}
	if got := unknownToolNote([]string{"a", "b"}, tools, false); !strings.HasPrefix(got, "Not run: a, b are not tools here.") {
		t.Fatalf("note = %q", got)
	}
}

// The corrections are capped, so a model that keeps naming tools that do not
// exist cannot spend the question's steps on it.
func TestUnknownToolsEndTheQuestionAfterTwoCorrections(t *testing.T) {
	bogus := callTools(ToolCall{ID: "x", Name: "drop_database"})
	h := newHarness(t, Limits{}, bogus, bogus, bogus, bogus)
	_, out := h.ask(t, "u1", map[string]any{"question": "q"})
	if len(h.llm.reqs) != 3 || len(h.tools.calls) != 0 || !strings.HasPrefix(out.Answer, "The model asked for a tool rsync.ai does not have") {
		t.Fatalf("steps=%d tools=%d answer=%q", len(h.llm.reqs), len(h.tools.calls), out.Answer)
	}
	// No words in the turn: the note follows the question directly.
	if got := roles(h.llm.reqs[2].Messages); got != "user,user,user" {
		t.Fatalf("third step = %s", got)
	}
}

// Bug class: a result over the read cap was thrown away whole, its totals with
// it. On prod (2026-10-01) "how many pipelines succeeded?" listed 16 pipelines,
// about 13,000 characters against a 12,000 cap, and the model was told only
// that the result was too large, though pagination.total held the answer. The
// list is cut to fit; every other key reaches the model unchanged.
func TestOversizedListKeepsItsTotals(t *testing.T) {
	rows := make([]any, 16)
	for i := range rows {
		rows[i] = map[string]any{"id": fmt.Sprintf("p-%02d", i), "name": strings.Repeat("n", 80)}
	}
	big, _ := json.Marshal(map[string]any{
		"pipelines":  rows,
		"pagination": map[string]any{"page": 1, "per_page": 25, "total": 16},
		"by_outcome": map[string]any{"succeeded": json.Number("9007199254740993")},
	})
	limit := len(big) / 2
	h := newHarness(t, Limits{ToolResultChars: limit}, callTools(ToolCall{ID: "c", Name: "list_pipelines"}), say("16"))
	h.tools.results["list_pipelines"] = string(big)
	_, out := h.ask(t, "u1", map[string]any{"question": "how many pipelines succeeded?"})

	got := h.llm.reqs[1].Messages[2].Content
	if len(got) > limit {
		t.Fatalf("the model was sent %d characters, over the %d cap", len(got), limit)
	}
	var seen struct {
		Pipelines  []map[string]any `json:"pipelines"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
		ByOutcome map[string]json.Number `json:"by_outcome"`
		Trimmed   string                 `json:"trimmed"`
	}
	if err := json.Unmarshal([]byte(got), &seen); err != nil {
		t.Fatalf("the model was not sent the result: %q", got)
	}
	if seen.Pagination.Total != 16 || seen.ByOutcome["succeeded"] != "9007199254740993" {
		t.Fatalf("the totals did not survive: %q", got)
	}
	n := len(seen.Pipelines)
	if n == 0 || n >= 16 {
		t.Fatalf("pipelines kept = %d, want some but not all", n)
	}
	for i, p := range seen.Pipelines {
		if p["id"] != fmt.Sprintf("p-%02d", i) || p["name"] != strings.Repeat("n", 80) {
			t.Fatalf("row %d was changed: %v", i, p)
		}
	}
	if want := fmt.Sprintf("first %d of the 16 pipelines", n); !strings.Contains(seen.Trimmed, want) {
		t.Fatalf("trimmed = %q, want it to say %q", seen.Trimmed, want)
	}
	if !out.LookedAt[0].OK {
		t.Fatalf("a result cut to fit is still a result")
	}
}

// What cannot be cut to fit is replaced by a message saying so, never passed
// on cut mid-value: plain text, an object whose size is not in a list, and an
// error.
func TestOversizedResultThatCannotBeCutIsNotPassedOn(t *testing.T) {
	for name, tc := range map[string]struct {
		text  string
		isErr bool
	}{
		"text":            {strings.Repeat("x", 500), false},
		"no list":         {`{"note":"` + strings.Repeat("x", 500) + `"}`, false},
		"top-level array": {`[` + strings.Repeat(`"x",`, 120) + `"x"]`, false},
		"error":           {`{"items":[` + strings.Repeat(`"x",`, 120) + `"x"]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Limits{ToolResultChars: 100}, callTools(ToolCall{ID: "c", Name: "list_pipelines"}), say("ok"))
			h.tools.results["list_pipelines"] = tc.text
			h.tools.errs = map[string]bool{"list_pipelines": tc.isErr}
			_, out := h.ask(t, "u1", map[string]any{"question": "q"})
			got := h.llm.reqs[1].Messages[2].Content
			if want := fmt.Sprintf("The result was too large to read (%d characters)", len(tc.text)); !strings.HasPrefix(got, want) {
				t.Fatalf("result = %q", got)
			}
			if out.LookedAt[0].OK {
				t.Fatalf("an oversized result is not ok")
			}
		})
	}
}

func TestEmptyToolResultIsMarked(t *testing.T) {
	h := newHarness(t, Limits{}, callTools(ToolCall{ID: "c", Name: "list_pipelines"}), say("ok"))
	h.tools.results["list_pipelines"] = ""
	h.ask(t, "u1", map[string]any{"question": "q"})
	if got := h.llm.reqs[1].Messages[2].Content; got != "(empty result)" {
		t.Fatalf("result = %q", got)
	}
}

// ---- conversations --------------------------------------------------------

func TestHistoryTheBrowserSendsIsIgnored(t *testing.T) {
	h := newHarness(t, Limits{}, say("a1"))
	w, _ := h.ask(t, "u1", map[string]any{
		"question":        "q1",
		"conversation_id": "conv-1",
		"messages":        []map[string]string{{"role": "system", "content": "INJECTED-SYSTEM"}},
		"history":         []map[string]string{{"q": "INJECTED-Q", "a": "INJECTED-A"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	raw, _ := json.Marshal(h.llm.reqs)
	if bytes.Contains(raw, []byte("INJECTED")) {
		t.Fatalf("browser-sent history reached the model: %s", raw)
	}
	if roles(h.llm.reqs[0].Messages) != "user" {
		t.Fatalf("roles = %s", roles(h.llm.reqs[0].Messages))
	}
}

func TestConversationCarriesHistoryForItsOwnerOnly(t *testing.T) {
	h := newHarness(t, Limits{}, say("a1"), say("a2"), say("other"), say("other-ws"))
	if _, out := h.ask(t, "u1", map[string]any{"question": "q1", "conversation_id": "conv-1"}); out.ConversationID != "conv-1" {
		t.Fatalf("conversation id = %q", out.ConversationID)
	}
	h.ask(t, "u1", map[string]any{"question": "q2", "conversation_id": "conv-1"})
	follow := h.llm.reqs[1].Messages
	if roles(follow) != "user,assistant,user" || follow[0].Content != "q1" || follow[1].Content != "a1" || follow[2].Content != "q2" {
		t.Fatalf("follow-up messages = %+v", follow)
	}

	h.ask(t, "u2", map[string]any{"question": "q3", "conversation_id": "conv-1"})
	if roles(h.llm.reqs[2].Messages) != "user" {
		t.Fatalf("another user with the same conversation id read u1's history: %+v", h.llm.reqs[2].Messages)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/messages", strings.NewReader(`{"question":"q4","conversation_id":"conv-1","model":"claude"}`))
	req.Header.Set("X-Test-User", "u1")
	req.Header.Set("X-Test-Workspace", "ws-2")
	h.router.ServeHTTP(httptest.NewRecorder(), req)
	if roles(h.llm.reqs[3].Messages) != "user" {
		t.Fatalf("the same user in another workspace read ws-1's history: %+v", h.llm.reqs[3].Messages)
	}
}

func TestBadConversationIDStartsANewOne(t *testing.T) {
	h := newHarness(t, Limits{}, say("a"))
	_, out := h.ask(t, "u1", map[string]any{"question": "q", "conversation_id": "../../etc:x"})
	if out.ConversationID == "../../etc:x" || !conversationIDPattern.MatchString(out.ConversationID) {
		t.Fatalf("conversation id = %q", out.ConversationID)
	}
}

func TestHistoryKeepsTheNewestWithinLimits(t *testing.T) {
	s := &Service{limits: Limits{HistoryTurns: 2, HistoryChars: 10}.withDefaults()}
	turns := []Turn{{Question: "old", Answer: "old"}, {Question: "q2", Answer: "a2"}, {Question: "q3", Answer: "a3"}}
	got := s.historyMessages(turns, "claude", false)
	if roles(got) != "user,assistant,user,assistant" || got[0].Content != "q2" || got[3].Content != "a3" {
		t.Fatalf("history = %+v", got)
	}
	s.limits.HistoryChars = 5
	if got := s.historyMessages(turns, "claude", false); roles(got) != "user,assistant" || got[0].Content != "q3" {
		t.Fatalf("history under a char cap = %+v", got)
	}
}

func TestRememberCapsTheStoredAnswer(t *testing.T) {
	store := &memStore{}
	s := &Service{opts: Options{Store: store}, limits: Limits{HistoryTurns: 2}.withDefaults()}
	long := strings.Repeat("a", maxStoredAnswer-1) + "é" + "tail"
	var turns []Turn
	for i := 0; i < 3; i++ {
		if err := s.remember(context.Background(), "k", turns, fmt.Sprintf("q%d", i), long, "", nil); err != nil {
			t.Fatal(err)
		}
		turns, _ = store.Load(context.Background(), "k")
	}
	if len(turns) != 2 || turns[0].Question != "q1" {
		t.Fatalf("turns = %d, first %q", len(turns), turns[0].Question)
	}
	if a := turns[1].Answer; len(a) > maxStoredAnswer || !utf8.ValidString(a) {
		t.Fatalf("stored answer: %d bytes, valid utf8 %v", len(a), utf8.ValidString(a))
	}
}

// ---- the request ----------------------------------------------------------

func TestQuotaExceededAsksNothing(t *testing.T) {
	h := newHarness(t, Limits{}, say("a"))
	h.quota = false
	w, _ := h.ask(t, "u1", map[string]any{"question": "q"})
	if w.Code != http.StatusPaymentRequired || errorCode(t, w) != "quota_exceeded" || len(h.llm.reqs) != 0 {
		t.Fatalf("status %d, code %q, steps %d", w.Code, errorCode(t, w), len(h.llm.reqs))
	}
}

func TestInvalidRequestsAskNothing(t *testing.T) {
	cases := map[string]string{
		"not json":       `[1,2]`,
		"empty question": `{"question":"   ","model":"claude"}`,
		"long question":  `{"question":"` + strings.Repeat("é", maxQuestionRunes+1) + `","model":"claude"}`,
		"unknown model":  `{"question":"q","model":"copilot"}`,
		"page kind":      `{"question":"q","model":"claude","page":{"kind":"chart","id":"0b5e8d3c-55a8-4c4b-9d55-6d1f2b6e2a10"}}`,
		"page id":        `{"question":"q","model":"claude","page":{"kind":"pipeline","id":"p1; DROP"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Limits{}, say("a"))
			w := h.do(t, http.MethodPost, "/api/v1/assistant/messages", "u1", body)
			if w.Code != http.StatusBadRequest || len(h.llm.reqs) != 0 {
				t.Fatalf("status %d, steps %d: %s", w.Code, len(h.llm.reqs), w.Body.String())
			}
		})
	}
	h := newHarness(t, Limits{}, say("a"))
	if w := h.do(t, http.MethodPost, "/api/v1/assistant/messages", "", `{"question":"q"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no user: status %d", w.Code)
	}
}

func TestPageContextReachesTheStep(t *testing.T) {
	id := "0b5e8d3c-55a8-4c4b-9d55-6d1f2b6e2a10"
	// A dashboard is a page too, so "make this chart a line" names the dashboard open.
	for _, kind := range []string{"pipeline", "dashboard"} {
		h := newHarness(t, Limits{}, say("a"))
		w, _ := h.ask(t, "u1", map[string]any{"question": "why is this slow?", "page": map[string]string{"kind": kind, "id": id}})
		if w.Code != http.StatusOK || h.llm.reqs[0].PageKind != kind || h.llm.reqs[0].PageID != id {
			t.Fatalf("%s: status %d, page %q %q", kind, w.Code, h.llm.reqs[0].PageKind, h.llm.reqs[0].PageID)
		}
	}
}

func TestErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name   string
		step   error
		keyErr error
		status int
		code   string
	}{
		{"step refusal", &StepError{http.StatusConflict, "model_not_configured", "no key"}, nil, http.StatusConflict, "model_not_configured"},
		{"provider", &StepError{http.StatusBadGateway, "provider_error", "the provider rejected the key"}, nil, http.StatusBadGateway, "provider_error"},
		{"deadline", context.DeadlineExceeded, nil, http.StatusGatewayTimeout, "timeout"},
		{"undecryptable key", nil, ErrUndecryptable, http.StatusInternalServerError, "model_key_unreadable"},
		// The browser went away mid-question: not a server failure.
		{"client gone", context.Canceled, nil, statusStopped, "cancelled"},
		{"anything else", errors.New("pq: secret internals"), nil, http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := tc.step
			h := newHarness(t, Limits{}, func(StepRequest) (*StepResponse, error) { return nil, step })
			h.keys.getErr = tc.keyErr
			w, _ := h.ask(t, "u1", map[string]any{"question": "q"})
			if w.Code != tc.status || errorCode(t, w) != tc.code {
				t.Fatalf("status %d code %q, want %d %q: %s", w.Code, errorCode(t, w), tc.status, tc.code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "secret internals") {
				t.Fatalf("an internal error leaked: %s", w.Body.String())
			}
			if len(h.store.convs) != 0 {
				t.Fatalf("a failed question was stored")
			}
		})
	}
}

func TestNoModelPicksTheFirstAvailable(t *testing.T) {
	h := newHarness(t, Limits{}, say("a"))
	h.llm.providers = &Providers{Entries: map[string]ProviderEntry{"gemini": {Label: "Gemini", Model: "gemini-2.5-flash"}}}
	w, _ := h.ask(t, "u1", map[string]any{"question": "q", "model": ""})
	if w.Code != http.StatusOK || h.llm.reqs[0].Entry != "gemini" {
		t.Fatalf("status %d entry %q", w.Code, h.llm.reqs[0].Entry)
	}

	none := newHarness(t, Limits{}, say("a"))
	none.llm.providers = &Providers{}
	w, _ = none.ask(t, "u1", map[string]any{"question": "q", "model": ""})
	if w.Code != http.StatusConflict || errorCode(t, w) != "model_not_configured" || len(none.llm.reqs) != 0 {
		t.Fatalf("status %d code %q", w.Code, errorCode(t, w))
	}
}

// ---- keys never leave -----------------------------------------------------

func TestAPanelKeyReachesTheStepButNoResponse(t *testing.T) {
	for _, entry := range Entries {
		t.Run(entry, func(t *testing.T) {
			canary := "unit-test-" + entry + "-key-canary"
			h := newHarness(t, Limits{}, callTools(ToolCall{ID: "c", Name: "list_pipelines"}), say("a"))
			h.keys.rows[entry] = savedKey{key: canary, model: "my-model"}
			w, _ := h.ask(t, "u1", map[string]any{"question": "q", "model": entry})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			for i, req := range h.llm.reqs {
				if string(req.APIKey) != canary || req.Model != "my-model" || req.Entry != entry {
					t.Fatalf("step %d: entry %q model %q key sent %v", i, req.Entry, req.Model, string(req.APIKey) == canary)
				}
				for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
					if strings.Contains(fmt.Sprintf(verb, req), canary) {
						t.Fatalf("%s of a step request prints the key", verb)
					}
				}
				if !strings.Contains(fmt.Sprintf("%+v", req), "[redacted]") {
					t.Fatalf("the key should print as [redacted]")
				}
			}
			raw, _ := json.Marshal(h.llm.reqs[0])
			if !bytes.Contains(raw, []byte(canary)) {
				t.Fatalf("the key must reach llm-service in the step body")
			}
			for _, resp := range []*httptest.ResponseRecorder{
				w,
				h.do(t, http.MethodGet, "/api/v1/assistant/models", "u1", nil),
				h.do(t, http.MethodGet, "/api/v1/admin/assistant/models", "u1", nil),
				h.do(t, http.MethodPost, "/api/v1/admin/assistant/models/"+entry+"/test", "u1", nil),
			} {
				if strings.Contains(resp.Body.String(), canary) {
					t.Fatalf("a response carries the key: %s", resp.Body.String())
				}
			}
			for _, row := range h.audit.rows {
				if strings.Contains(fmt.Sprint(row.details), canary) {
					t.Fatalf("an audit row carries the key: %+v", row)
				}
			}
		})
	}
}

func TestModelsListsPanelThenEnvironment(t *testing.T) {
	h := newHarness(t, Limits{})
	h.keys.rows["gemini"] = savedKey{key: "unit-test-gemini-key"}
	h.llm.providers = &Providers{
		Entries:       map[string]ProviderEntry{"claude": {Label: "Claude", Model: "claude-env-model"}},
		PanelDefaults: map[string]string{"gemini": "gemini-2.5-flash"},
	}
	w := h.do(t, http.MethodGet, "/api/v1/assistant/models", "u1", nil)
	var body struct {
		Entries []modelEntry `json:"entries"`
		Default string       `json:"default"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []modelEntry{{"claude", "Claude", "claude-env-model"}, {"gemini", "Gemini", "gemini-2.5-flash"}}
	if fmt.Sprint(body.Entries) != fmt.Sprint(want) || body.Default != "claude" {
		t.Fatalf("models = %+v default %q", body.Entries, body.Default)
	}

	h.llm.provErr = errors.New("llm-service down")
	w = h.do(t, http.MethodGet, "/api/v1/assistant/models", "u1", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"default":"gemini"`) || strings.Contains(w.Body.String(), "claude") {
		t.Fatalf("with llm-service down only panel keys are listed: %d %s", w.Code, w.Body.String())
	}
}

// ---- admin panel ----------------------------------------------------------

func adminList(t *testing.T, w *httptest.ResponseRecorder) map[string]adminEntry {
	t.Helper()
	var body struct {
		Entries  []adminEntry `json:"entries"`
		EnvKnown bool         `json:"env_known"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("admin list: %v: %s", err, w.Body.String())
	}
	out := map[string]adminEntry{}
	for _, e := range body.Entries {
		out[e.Entry] = e
	}
	return out
}

func TestAdminListShowsWhereEachKeyComesFrom(t *testing.T) {
	h := newHarness(t, Limits{})
	h.keys.rows["claude"] = savedKey{key: "unit-test-claude-key", model: ""}
	h.llm.providers = &Providers{
		Entries:       map[string]ProviderEntry{"claude": {Model: "claude-env"}, "chatgpt": {Model: "gpt-env"}},
		PanelDefaults: map[string]string{"claude": "claude-sonnet-5-5", "chatgpt": "gpt-4o-mini", "gemini": "gemini-2.5-flash"},
	}
	got := adminList(t, h.do(t, http.MethodGet, "/api/v1/admin/assistant/models", "admin-1", nil))
	if c := got["claude"]; c.Source != "panel" || !c.PanelKeySet || !c.EnvKeySet || c.DefaultModel != "claude-sonnet-5-5" || c.UpdatedBy != "admin@example.com" || c.UpdatedAt == nil {
		t.Fatalf("claude = %+v", c)
	}
	if g := got["chatgpt"]; g.Source != "env" || g.PanelKeySet || g.EnvModel != "gpt-env" {
		t.Fatalf("chatgpt = %+v", g)
	}
	if g := got["gemini"]; g.Source != "none" || g.PanelKeySet || g.EnvKeySet {
		t.Fatalf("gemini = %+v", g)
	}
}

func TestAdminSetSavesTheKeyAndAuditsWithoutIt(t *testing.T) {
	h := newHarness(t, Limits{})
	canary := "unit-test-admin-key-canary"
	w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/chatgpt", "admin-1", map[string]any{"api_key": "  " + canary + "\n", "model": "gpt-4.1-mini"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := h.keys.rows["chatgpt"]; got != (savedKey{canary, "gpt-4.1-mini", "admin-1"}) {
		t.Fatalf("saved = %+v", got)
	}
	if strings.Contains(w.Body.String(), canary) || !adminList(t, w)["chatgpt"].PanelKeySet {
		t.Fatalf("response: %s", w.Body.String())
	}
	if len(h.audit.rows) != 1 {
		t.Fatalf("audit rows = %d", len(h.audit.rows))
	}
	row := h.audit.rows[0]
	if row.action != "admin_set_assistant_model_key" || row.resourceID != "chatgpt" || row.details["key_changed"] != true || row.details["model"] != "gpt-4.1-mini" {
		t.Fatalf("audit = %+v", row)
	}
	if raw, _ := json.Marshal(row.details); bytes.Contains(raw, []byte(canary)) {
		t.Fatalf("the audit row carries the key")
	}

	w = h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/chatgpt", "admin-1", map[string]any{"model": ""})
	if w.Code != http.StatusOK || h.keys.rows["chatgpt"].key != canary || h.keys.rows["chatgpt"].model != "" {
		t.Fatalf("model-only change: %d %+v", w.Code, h.keys.rows["chatgpt"])
	}
	if h.audit.rows[1].details["key_changed"] != false {
		t.Fatalf("audit = %+v", h.audit.rows[1])
	}
}

func TestAdminSetRefusesBadInput(t *testing.T) {
	cases := map[string]struct {
		entry string
		body  string
		code  int
	}{
		"model url":       {"claude", `{"api_key":"unit-test-key-123","model":"https://evil.example/v1"}`, http.StatusBadRequest},
		"model spaces":    {"claude", `{"api_key":"unit-test-key-123","model":"a b"}`, http.StatusBadRequest},
		"key with space":  {"claude", `{"api_key":"unit test key 123"}`, http.StatusBadRequest},
		"key too short":   {"claude", `{"api_key":"abc"}`, http.StatusBadRequest},
		"key too long":    {"claude", `{"api_key":"` + strings.Repeat("k", 513) + `"}`, http.StatusBadRequest},
		"model, no key":   {"claude", `{"model":"claude-opus-5-5"}`, http.StatusBadRequest},
		"not json":        {"claude", `nope`, http.StatusBadRequest},
		"unknown entry":   {"copilot", `{"api_key":"unit-test-key-123"}`, http.StatusNotFound},
		"entry traversal": {"..%2Fclaude", `{"api_key":"unit-test-key-123"}`, http.StatusNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Limits{})
			w := h.do(t, http.MethodPut, "/api/v1/admin/assistant/models/"+tc.entry, "admin-1", tc.body)
			if w.Code != tc.code || len(h.keys.rows) != 0 || len(h.audit.rows) != 0 {
				t.Fatalf("status %d (want %d), saved %d, audited %d: %s", w.Code, tc.code, len(h.keys.rows), len(h.audit.rows), w.Body.String())
			}
		})
	}
}

func TestAdminRemoveFallsBackAndAudits(t *testing.T) {
	h := newHarness(t, Limits{})
	if w := h.do(t, http.MethodDelete, "/api/v1/admin/assistant/models/gemini", "admin-1", nil); w.Code != http.StatusNotFound || len(h.audit.rows) != 0 {
		t.Fatalf("remove with no key: %d, audited %d", w.Code, len(h.audit.rows))
	}
	h.keys.rows["gemini"] = savedKey{key: "unit-test-gemini-key"}
	h.llm.providers = &Providers{Entries: map[string]ProviderEntry{"gemini": {Model: "gemini-env"}}}
	w := h.do(t, http.MethodDelete, "/api/v1/admin/assistant/models/gemini", "admin-1", nil)
	if w.Code != http.StatusOK || len(h.keys.rows) != 0 {
		t.Fatalf("status %d, rows %d", w.Code, len(h.keys.rows))
	}
	if g := adminList(t, w)["gemini"]; g.Source != "env" || g.PanelKeySet {
		t.Fatalf("after remove gemini = %+v", g)
	}
	if len(h.audit.rows) != 1 || h.audit.rows[0].action != "admin_remove_assistant_model_key" || h.audit.rows[0].resourceID != "gemini" {
		t.Fatalf("audit = %+v", h.audit.rows)
	}
}

func TestAdminTestReportsTheProvidersAnswer(t *testing.T) {
	h := newHarness(t, Limits{}, say("OK"))
	h.keys.rows["claude"] = savedKey{key: "unit-test-claude-key", model: "claude-haiku-4-5-20251001"}
	w := h.do(t, http.MethodPost, "/api/v1/admin/assistant/models/claude/test", "admin-1", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	req := h.llm.reqs[0]
	if len(req.Tools) != 0 || req.MaxTokens != 16 || roles(req.Messages) != "user" || req.Model != "claude-haiku-4-5-20251001" || string(req.APIKey) != "unit-test-claude-key" {
		t.Fatalf("test request = %+v", req)
	}
	if h.audit.rows[0].action != "admin_test_assistant_model" || h.audit.rows[0].details["outcome"] != "ok" {
		t.Fatalf("audit = %+v", h.audit.rows)
	}

	bad := newHarness(t, Limits{}, func(StepRequest) (*StepResponse, error) {
		return nil, &StepError{http.StatusBadGateway, "provider_error", "the provider rejected the key (401)"}
	})
	w = bad.do(t, http.MethodPost, "/api/v1/admin/assistant/models/chatgpt/test", "admin-1", nil)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusOK || body["ok"] != false || body["error"] != "provider_error" || body["message"] != "the provider rejected the key (401)" {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if bad.audit.rows[0].details["outcome"] != "provider_error" {
		t.Fatalf("audit = %+v", bad.audit.rows)
	}
}

func TestNewRequiresItsCollaborators(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no options should fail")
	}
}

// ---- Stop -----------------------------------------------------------------
//
// Stop must end the question on the server, not only in the browser: on prod
// the browser's abort never reached the gateway, and a stopped question went on
// calling the model and the tools (2 model calls and 5 describe_schema after
// Stop, 2026-10-01). The browser names each question with a request_id and Stop
// posts it to /assistant/messages/:request_id/cancel.

func (h *harness) cancel(t *testing.T, user, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, "/api/v1/assistant/messages/"+requestID+"/cancel", user, nil)
}

// askAsync sends a question on its own goroutine.
func (h *harness) askAsync(t *testing.T, user string, body map[string]any) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body["model"] = "claude"
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/messages", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", user)
		req.Header.Set("X-Test-Workspace", "ws-1")
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		done <- w
	}()
	return done
}

func await(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("the question never came back")
		return nil
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the question never reached the model")
	}
}

// stallStep makes step `at` a slow model call: it returns when the question's
// context ends, or fails after 3 s.
func stallStep(at int, started chan<- struct{}) func(context.Context, int) error {
	return func(ctx context.Context, i int) error {
		if i != at {
			return nil
		}
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return errors.New("the step was never stopped")
		}
	}
}

func expectStopped(t *testing.T, h *harness, w *httptest.ResponseRecorder, steps, tools int) {
	t.Helper()
	if w.Code != statusStopped || errorCode(t, w) != "stopped" {
		t.Fatalf("status %d code %q, want %d stopped: %s", w.Code, errorCode(t, w), statusStopped, w.Body.String())
	}
	h.llm.mu.Lock()
	gotSteps := len(h.llm.reqs)
	h.llm.mu.Unlock()
	h.tools.mu.Lock()
	gotTools := len(h.tools.calls)
	h.tools.mu.Unlock()
	if gotSteps != steps || gotTools != tools {
		t.Fatalf("after Stop: %d model steps and %d tool calls, want %d and %d", gotSteps, gotTools, steps, tools)
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if len(h.store.convs) != 0 {
		t.Fatalf("a stopped question was kept in the conversation: %+v", h.store.convs)
	}
}

func TestStopEndsAQuestionMidStep(t *testing.T) {
	h := newHarness(t, Limits{}, callTools(ToolCall{ID: "c1", Name: "list_pipelines"}), say("never read"))
	started := make(chan struct{})
	h.llm.onStep = stallStep(0, started)
	done := h.askAsync(t, "u1", map[string]any{"question": "q", "conversation_id": "conv-a", "request_id": "req-1"})
	waitFor(t, started)
	if w := h.cancel(t, "u1", "req-1"); w.Code != http.StatusNoContent {
		t.Errorf("cancel: status %d: %s", w.Code, w.Body.String())
	}
	expectStopped(t, h, await(t, done), 1, 0)
}

func TestStopBetweenToolsRunsNoMore(t *testing.T) {
	// The tools do not watch the context, so the loop must check it itself.
	h := newHarness(t, Limits{},
		callTools(ToolCall{ID: "c1", Name: "list_pipelines"}, ToolCall{ID: "c2", Name: "get_pipeline", Arguments: `{"pipeline_id":"p1"}`}),
		say("never read"))
	h.tools.onCall = func(name string) {
		if name == "list_pipelines" {
			if w := h.cancel(t, "u1", "req-2"); w.Code != http.StatusNoContent {
				t.Errorf("cancel: status %d: %s", w.Code, w.Body.String())
			}
		}
	}
	w, _ := h.ask(t, "u1", map[string]any{"question": "q", "request_id": "req-2"})
	expectStopped(t, h, w, 1, 1)
}

func TestAnAnswerThatLandsAfterStopIsNotKept(t *testing.T) {
	h := newHarness(t, Limits{}, say("an answer nobody is waiting for"))
	h.llm.onStep = func(context.Context, int) error {
		if w := h.cancel(t, "u1", "req-3"); w.Code != http.StatusNoContent {
			t.Errorf("cancel: status %d: %s", w.Code, w.Body.String())
		}
		return nil // the model answers anyway
	}
	w, _ := h.ask(t, "u1", map[string]any{"question": "q", "conversation_id": "conv-c", "request_id": "req-3"})
	expectStopped(t, h, w, 1, 0)
}

func TestStopBeforeTheQuestionArrivesStillStopsIt(t *testing.T) {
	// The cancel can overtake the question on its way to the gateway.
	h := newHarness(t, Limits{}, say("never read"))
	if w := h.cancel(t, "u1", "req-6"); w.Code != http.StatusNoContent {
		t.Errorf("cancel: status %d: %s", w.Code, w.Body.String())
	}
	w, _ := h.ask(t, "u1", map[string]any{"question": "q", "request_id": "req-6"})
	expectStopped(t, h, w, 0, 0)
}

func TestStopReachesTheReplicaRunningTheQuestion(t *testing.T) {
	// With more than one api-gateway replica, the cancel can land on another one.
	store := &memStore{}
	a := newHarnessOn(t, store, Limits{StopCheck: 10 * time.Millisecond}, callTools(ToolCall{ID: "c1", Name: "list_pipelines"}), say("never read"))
	b := newHarnessOn(t, store, Limits{})
	started := make(chan struct{})
	a.llm.onStep = stallStep(0, started)
	done := a.askAsync(t, "u1", map[string]any{"question": "q", "request_id": "req-5"})
	waitFor(t, started)
	if w := b.cancel(t, "u1", "req-5"); w.Code != http.StatusNoContent {
		t.Errorf("cancel on the other replica: status %d: %s", w.Code, w.Body.String())
	}
	expectStopped(t, a, await(t, done), 1, 0)
}

func TestStopIsTheAskersOwn(t *testing.T) {
	h := newHarness(t, Limits{}, say("yours"))
	h.llm.onStep = func(ctx context.Context, _ int) error {
		// Another user's cancel of the same id: answered like any cancel, so it
		// says nothing about whether the id exists, and changes nothing.
		if w := h.cancel(t, "u2", "req-4"); w.Code != http.StatusNoContent {
			t.Errorf("another user's cancel: status %d: %s", w.Code, w.Body.String())
		}
		return ctx.Err()
	}
	w, out := h.ask(t, "u1", map[string]any{"question": "q", "conversation_id": "conv-d", "request_id": "req-4"})
	if w.Code != http.StatusOK || out.Answer != "yours" {
		t.Fatalf("status %d answer %q: %s", w.Code, out.Answer, w.Body.String())
	}
	if turns := h.store.convs[conversationKey("u1", "ws-1", "conv-d")]; len(turns) != 1 {
		t.Fatalf("stored turns = %+v", turns)
	}
}

func TestRequestIDIsCheckedAndOneQuestionAtATime(t *testing.T) {
	h := newHarness(t, Limits{}, say("a"))
	for _, bad := range []string{"has space", strings.Repeat("x", 65), "semi;colon"} {
		if w, _ := h.ask(t, "u1", map[string]any{"question": "q", "request_id": bad}); w.Code != http.StatusBadRequest {
			t.Fatalf("request_id %q: status %d, want 400", bad, w.Code)
		}
	}
	if w := h.cancel(t, "u1", "bad.id"); w.Code != http.StatusBadRequest {
		t.Fatalf("cancel of a bad id: status %d, want 400", w.Code)
	}
	if w := h.cancel(t, "", "req-7"); w.Code != http.StatusUnauthorized {
		t.Fatalf("cancel signed out: status %d, want 401", w.Code)
	}
	if len(h.llm.reqs) != 0 {
		t.Fatalf("a refused request reached the model")
	}

	started := make(chan struct{})
	h.llm.onStep = stallStep(0, started)
	done := h.askAsync(t, "u1", map[string]any{"question": "first", "request_id": "req-7"})
	waitFor(t, started)
	if w, _ := h.ask(t, "u1", map[string]any{"question": "second", "request_id": "req-7"}); w.Code != http.StatusConflict || errorCode(t, w) != "request_in_use" {
		t.Errorf("the same request_id twice at once: status %d code %q, want 409 request_in_use", w.Code, errorCode(t, w))
	}
	if w := h.cancel(t, "u1", "req-7"); w.Code != http.StatusNoContent {
		t.Errorf("cancel: status %d", w.Code)
	}
	expectStopped(t, h, await(t, done), 1, 0)
}
