package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A question's cost is its steps' exact costs summed, then rounded up to a
// cent once. Each step rounded up on its own charged a question of three
// 0.0004¢ steps 3¢ (llm-service tests/test_llm_cost_is_rounded_only_as_a_total.py).

func costing(cents float64, f stepFunc) stepFunc {
	return func(req StepRequest) (*StepResponse, error) {
		r, err := f(req)
		if r != nil {
			r.CostCents = cents
		}
		return r, err
	}
}

func TestAQuestionsCostIsItsStepsSummedThenRounded(t *testing.T) {
	h := newHarness(t, Limits{},
		costing(0.0004, callTools(ToolCall{ID: "c1", Name: "list_pipelines", Arguments: `{}`})),
		costing(0.0004, callTools(ToolCall{ID: "c2", Name: "list_pipelines", Arguments: `{"limit":5}`})),
		costing(0.0004, say("You have no pipelines.")),
	)
	h.tools.results["list_pipelines"] = `{"items":[]}`

	w, out := h.ask(t, "u1", map[string]any{"question": "which pipelines do I have?"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(h.llm.reqs) != 3 {
		t.Fatalf("steps = %d, want 3", len(h.llm.reqs))
	}
	if out.CostCents != 1 {
		t.Fatalf("cost = %d¢, want 1¢: three 0.0004¢ steps are 0.0012¢, rounded up once", out.CostCents)
	}
}

func TestWholeCentsRoundsTheTotalUpWithoutFloatError(t *testing.T) {
	for _, tc := range []struct {
		steps []float64
		want  int
	}{
		{nil, 0},
		{[]float64{0}, 0},
		{[]float64{0.00045}, 1},
		{[]float64{0.4, 0.4, 0.4}, 2},
		// 0.2 + 0.4 + 0.3 + 0.1 is 1.0000000000000002 in float64: exactly 1¢.
		{[]float64{0.2, 0.4, 0.3, 0.1}, 1},
		{[]float64{1, 1}, 2},
		{[]float64{2.5}, 3},
	} {
		var sum float64
		for _, c := range tc.steps {
			sum += c
		}
		if got := wholeCents(sum); got != tc.want {
			t.Errorf("wholeCents(sum of %v = %v) = %d, want %d", tc.steps, sum, got, tc.want)
		}
	}
}

func TestAStepsFractionalCostDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":"hi","tool_calls":[],"finish_reason":"stop","entry":"chatgpt","label":"ChatGPT","model":"gpt-4o-mini","source":"env","cost_cents":0.00045}`))
	}))
	defer srv.Close()
	t.Setenv("LLM_SERVICE_URL", srv.URL+"/")
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-internal-secret")

	resp, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{
		Entry: "chatgpt", Messages: []Message{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("a sub-cent step cost must decode: %v", err)
	}
	if resp.CostCents != 0.00045 {
		t.Fatalf("cost = %v, want 0.00045", resp.CostCents)
	}
}
