package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPStepperSendsTheStepWithTheInternalSecret(t *testing.T) {
	var got struct {
		method, path, secret string
		body                 map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.secret = r.Method, r.URL.Path, r.Header.Get("X-Internal-Secret")
		raw, _ := io.ReadAll(r.Body)
		got.body = nil
		_ = json.Unmarshal(raw, &got.body)
		_, _ = w.Write([]byte(`{"content":"","tool_calls":[{"id":"c1","name":"list_pipelines","arguments":"{}"}],"finish_reason":"tool_calls","entry":"claude","label":"Claude","model":"claude-sonnet-5-5","source":"panel","cost_cents":3}`))
	}))
	defer srv.Close()
	t.Setenv("LLM_SERVICE_URL", srv.URL+"/")
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-internal-secret")

	resp, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{
		Entry: "claude", APIKey: secret("unit-test-claude-key"), Messages: []Message{{Role: "user", Content: "q"}}, ToolChoice: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/v1/assistant/step" || got.secret != "unit-test-internal-secret" {
		t.Fatalf("request = %s %s secret %q", got.method, got.path, got.secret)
	}
	if got.body["api_key"] != "unit-test-claude-key" || got.body["tool_choice"] != "none" || got.body["entry"] != "claude" {
		t.Fatalf("body = %v", got.body)
	}
	if msgs, _ := got.body["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "q" {
		t.Fatalf("messages = %v", got.body["messages"])
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "list_pipelines" || resp.Source != "panel" || resp.CostCents != 3 {
		t.Fatalf("response = %+v", resp)
	}

	// No panel key: the field is left out, so llm-service uses its environment key.
	if _, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{Entry: "claude", Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatal(err)
	}
	if _, present := got.body["api_key"]; present {
		t.Fatalf("an empty key is sent: %v", got.body)
	}
}

func TestHTTPStepperProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/assistant/providers" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"entries":{"gemini":{"label":"Gemini","model":"gemini-2.5-flash"}},"known":["claude","chatgpt","gemini"],"panel_defaults":{"claude":"claude-sonnet-5-5"}}`))
	}))
	defer srv.Close()
	t.Setenv("LLM_SERVICE_URL", srv.URL)
	p, err := (&HTTPStepper{}).Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries["gemini"].Model != "gemini-2.5-flash" || p.PanelDefaults["claude"] != "claude-sonnet-5-5" || len(p.Known) != 3 {
		t.Fatalf("providers = %+v", p)
	}
}

func TestHTTPStepperMapsRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   StepError
	}{
		{"no key", 409, `{"error":"model_not_configured","reason":"x"}`, StepError{409, "model_not_configured", "this model has no key; an admin can add one in Admin > Settings > AI models"}},
		{"provider", 502, `{"error":"provider_error","reason":"the provider rejected the key (401)"}`, StepError{502, "provider_error", "the provider rejected the key (401)"}},
		{"guard refused", 422, `{"error":"refused","reason":"a field is not allowed"}`, StepError{422, "refused", "the question could not be sent to the model: a field is not allowed"}},
		{"bad request", 422, `{"detail":[]}`, StepError{422, "bad_request", "the model service refused the request"}},
		{"secret mismatch", 401, `{}`, StepError{502, "llm_service_error", "api-gateway and llm-service do not share INTERNAL_SERVICE_SECRET"}},
		{"other", 500, `boom`, StepError{502, "llm_service_error", "the assistant's model service returned HTTP 500"}},
		{"unreadable ok", 200, `not json`, StepError{502, "llm_service_error", "the assistant's model service returned an unreadable answer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("LLM_SERVICE_URL", srv.URL)
			_, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{Entry: "claude"})
			var se *StepError
			if !errors.As(err, &se) || *se != tc.want {
				t.Fatalf("err = %#v, want %#v", err, tc.want)
			}
		})
	}
}

func TestHTTPStepperUnreachableAndSlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	t.Setenv("LLM_SERVICE_URL", url)
	var se *StepError
	if _, err := (&HTTPStepper{}).Step(context.Background(), StepRequest{}); !errors.As(err, &se) || se.Code != "llm_service_unreachable" || se.Status != http.StatusBadGateway {
		t.Fatalf("unreachable: %v", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	t.Setenv("LLM_SERVICE_URL", slow.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (&HTTPStepper{}).Step(ctx, StepRequest{}); !errors.As(err, &se) || se.Code != "timeout" || se.Status != http.StatusGatewayTimeout {
		t.Fatalf("slow: %v", err)
	}
}

// A cancelled step (Stop, or the asker went away) is not an outage: the
// stepper returns the context's error, so it is logged and answered as a
// cancellation, never as "the model service is unreachable".
func TestHTTPStepperCancelledIsNotAnOutage(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	t.Setenv("LLM_SERVICE_URL", slow.URL)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := (&HTTPStepper{}).Step(ctx, StepRequest{})
	var se *StepError
	if !errors.Is(err, context.Canceled) || errors.As(err, &se) {
		t.Fatalf("cancelled step: %v", err)
	}
}
