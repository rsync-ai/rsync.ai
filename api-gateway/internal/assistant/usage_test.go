package assistant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/agentgw"
)

// An answer's usage is what scripts/assistant_eval.py measures a prompt change
// by: tokens per question and per turn, the characters each turn sent by part,
// and how each tool call ended.

func withUsage(f stepFunc, tokens StepTokens, chars SentChars) stepFunc {
	return func(req StepRequest) (*StepResponse, error) {
		resp, err := f(req)
		if resp != nil {
			resp.Usage, resp.Chars = tokens, chars
		}
		return resp, err
	}
}

// llm-service's route builds the usage and chars keys the golden lists
// (tests/test_assistant_step_usage_golden.py); a key renamed on either side
// would decode as 0 here while both sides' own tests passed.
func TestStepResponseReadsTheUsageGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "shared", "assistant_step_usage_golden.json"))
	if err != nil {
		t.Fatalf("reading shared/assistant_step_usage_golden.json: %v", err)
	}
	var golden struct {
		StepUsage map[string]int `json:"step_usage"`
		StepChars map[string]int `json:"step_chars"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parsing shared/assistant_step_usage_golden.json: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"content": "ok", "usage": golden.StepUsage, "chars": golden.StepChars})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	t.Setenv("LLM_SERVICE_URL", srv.URL+"/")
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-internal-secret")

	resp, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{Entry: "chatgpt", Messages: []Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Back to a map: a field the golden lacks shows up as an extra key, a
	// golden key no field reads as a missing one.
	for name, pair := range map[string]struct {
		got  any
		want map[string]int
	}{"usage": {resp.Usage, golden.StepUsage}, "chars": {resp.Chars, golden.StepChars}} {
		b, _ := json.Marshal(pair.got)
		var got map[string]int
		_ = json.Unmarshal(b, &got)
		if !reflect.DeepEqual(got, pair.want) {
			t.Fatalf("%s decoded as %v, want %v", name, got, pair.want)
		}
		for k, v := range pair.want {
			if v == 0 {
				t.Fatalf("golden %s.%s is 0, so a lost key would still match", name, k)
			}
		}
	}
}

func TestAnswerSumsEachTurnsUsage(t *testing.T) {
	h := newHarness(t, Limits{},
		withUsage(callTools(ToolCall{ID: "c", Name: "list_pipelines", Arguments: `{}`}),
			StepTokens{PromptTokens: 13000, CachedTokens: 0, CompletionTokens: 20},
			SentChars{System: 18000, Tools: 28000, User: 10}),
		withUsage(say("none"),
			StepTokens{PromptTokens: 13100, CachedTokens: 12800, CompletionTokens: 40, ReasoningTokens: 8},
			SentChars{System: 18000, Tools: 28000, User: 10, Assistant: 16, Tool: 12}),
	)
	w, out := h.ask(t, "u1", map[string]any{"question": "any pipelines?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	u := out.Usage
	if u.InputTokens != 26100 || u.CachedTokens != 12800 || u.OutputTokens != 60 || u.ReasoningTokens != 8 {
		t.Fatalf("totals = %+v", u)
	}
	if len(u.Steps) != 2 || u.Steps[0].InputTokens != 13000 || u.Steps[1].CachedTokens != 12800 || u.Steps[1].ReasoningTokens != 8 {
		t.Fatalf("steps = %+v", u.Steps)
	}
	if u.Steps[0].Chars != (SentChars{System: 18000, Tools: 28000, User: 10}) || u.Steps[1].Chars.Tool != 12 {
		t.Fatalf("chars = %+v / %+v", u.Steps[0].Chars, u.Steps[1].Chars)
	}
	// A turn with no calls lists none, rather than null, so the eval reads
	// every turn the same way.
	if !strings.Contains(w.Body.String(), `"calls":[]`) {
		t.Fatalf("the answering turn's calls are not an empty list: %s", w.Body.String())
	}
}

// Bug class: a question that "worked" while its tool calls failed. The eval
// counts calls refused for their arguments, for a missing resource and for the
// role, so each call's audit outcome reaches the answer.
func TestEachCallRecordsHowItEnded(t *testing.T) {
	h := newHarness(t, Limits{},
		callTools(
			ToolCall{ID: "a", Name: "list_pipelines", Arguments: `{}`},
			ToolCall{ID: "b", Name: "get_pipeline", Arguments: `{"pipeline_id":"p9"}`},
			ToolCall{ID: "c", Name: "drop_database", Arguments: `{}`},
			ToolCall{ID: "d", Name: "list_pipelines", Arguments: `not json`},
			ToolCall{ID: "e", Name: "create_model", Arguments: `{}`},
		),
		say("p9 does not exist"),
	)
	h.tools.extra = []string{"create_model"}
	h.tools.results["get_pipeline"] = "pipeline not found"
	h.tools.errs["get_pipeline"] = true
	h.tools.audited = map[string]agentgw.Audited{
		"get_pipeline": {Outcome: "pipeline_not_found", Status: http.StatusNotFound},
		"create_model": {Outcome: "forbidden", Status: http.StatusForbidden},
	}
	_, out := h.ask(t, "u1", map[string]any{"question": "what about p9?"})
	want := []CallOutcome{
		{Tool: "list_pipelines", Outcome: "ok", Status: 200},
		{Tool: "get_pipeline", Outcome: "pipeline_not_found", Status: 404},
		{Tool: "drop_database", Outcome: "unknown_tool"},
		{Tool: "list_pipelines", Outcome: "invalid_arguments", Status: 400},
		{Tool: "create_model", Outcome: "forbidden", Status: 403},
	}
	if len(out.Usage.Steps) != 2 || !reflect.DeepEqual(out.Usage.Steps[0].Calls, want) {
		t.Fatalf("calls = %+v, want %+v", out.Usage.Steps, want)
	}
	if len(out.Usage.Steps[1].Calls) != 0 {
		t.Fatalf("the answering turn made no call: %+v", out.Usage.Steps[1].Calls)
	}
}

// Calls the loop itself does not pass on are named, so a cap that bites shows
// in the eval instead of reading as fewer calls.
func TestCallsTheLoopDidNotRunAreNamed(t *testing.T) {
	call := ToolCall{ID: "c", Name: "list_pipelines", Arguments: `{}`}
	outcomes := func(s StepUsage) string {
		var o []string
		for _, c := range s.Calls {
			o = append(o, c.Outcome)
		}
		return strings.Join(o, ",")
	}

	t.Run("tool call cap", func(t *testing.T) {
		h := newHarness(t, Limits{ToolCalls: 1}, callTools(call, call), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		if got := outcomes(out.Usage.Steps[0]); got != "ok,limit" {
			t.Fatalf("outcomes = %s", got)
		}
	})
	t.Run("last turn", func(t *testing.T) {
		h := newHarness(t, Limits{Steps: 2}, callTools(call), callTools(call))
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		if got := outcomes(out.Usage.Steps[1]); out.Stopped != "limit" || got != "limit" {
			t.Fatalf("stopped = %q, last turn's outcomes = %s", out.Stopped, got)
		}
	})
	t.Run("past the calls one turn may make", func(t *testing.T) {
		calls := make([]ToolCall, maxCallsPerTurn+1)
		for i := range calls {
			calls[i] = call
		}
		h := newHarness(t, Limits{}, callTools(calls...), say("ok"))
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		got := out.Usage.Steps[0].Calls
		if len(got) != maxCallsPerTurn+1 || got[maxCallsPerTurn-1].Outcome != "ok" || got[maxCallsPerTurn].Outcome != "dropped" {
			t.Fatalf("calls = %+v", got)
		}
	})
	t.Run("result too large", func(t *testing.T) {
		h := newHarness(t, Limits{ToolResultChars: 100}, callTools(call), say("ok"))
		h.tools.results["list_pipelines"] = strings.Repeat("x", 500)
		_, out := h.ask(t, "u1", map[string]any{"question": "q"})
		if got := outcomes(out.Usage.Steps[0]); got != "too_large" {
			t.Fatalf("outcomes = %s", got)
		}
	})
}
