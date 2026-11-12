package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"api-gateway/internal/handlers"
	"api-gateway/internal/telemetry"
)

// The hop to llm-service: POST /v1/assistant/step once per model turn and GET
// /v1/assistant/providers for what its environment can serve. Every model call
// goes through llm-service's egress guard; this package never calls a
// provider itself.

// secret is a model key on its way to llm-service. It prints as [redacted], so
// a request logged with %v or %+v never shows it; JSON still carries it.
type secret string

func (secret) String() string   { return "[redacted]" }
func (secret) GoString() string { return "[redacted]" }

// Message is one turn of the conversation, in the provider-neutral shape the
// step route takes. Content is always sent, "" when a turn only calls tools.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Data is "sample_rows" on a message that may hold row values: a
	// sample_rows result, or an answer written after one. llm-service's
	// egress guard checks the sample-rows switches again before it sends one,
	// and runs a PII check on a result's values.
	Data string `json:"data,omitempty"`
}

// ToolCall is a tool the model asked for. Arguments is the JSON object text.
// Signature is Gemini's thought signature on the call, which the next step
// must send back with it; Gemini 3 refuses a turn that drops it. It lives for
// one question only and never leaves api-gateway any other way.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Signature string `json:"signature,omitempty"`
}

// Tool is one agent gateway tool as the model sees it.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// StepRequest is one model turn. APIKey is set only when an admin saved a key
// in the panel; empty, llm-service uses its environment key.
type StepRequest struct {
	Entry      string    `json:"entry"`
	APIKey     secret    `json:"api_key,omitempty"`
	Model      string    `json:"model,omitempty"`
	UserID     string    `json:"user_id,omitempty"`
	PageKind   string    `json:"page_kind,omitempty"`
	PageID     string    `json:"page_id,omitempty"`
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
	MaxTokens  int       `json:"max_tokens,omitempty"`
}

// StepResponse is what the model said, or which tools it asked for.
type StepResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls"`
	FinishReason string     `json:"finish_reason"`
	Entry        string     `json:"entry"`
	Label        string     `json:"label"`
	Model        string     `json:"model"`
	Source       string     `json:"source"`
	// CostCents is the turn's exact cost, a fraction of a cent for a small
	// turn; the question's total is rounded once (wholeCents).
	CostCents float64    `json:"cost_cents"`
	Usage     StepTokens `json:"usage"`
	Chars     SentChars  `json:"chars"`
	// DataResults is what the egress guard did with each sample_rows result
	// this turn sent.
	DataResults []DataResult `json:"data_results,omitempty"`
}

// StepTokens is the provider's own token count for one turn. CachedTokens is
// the part of PromptTokens the provider read from its prompt cache, and
// ReasoningTokens the part of CompletionTokens spent thinking; each is 0 when
// the provider does not report it.
type StepTokens struct {
	PromptTokens     int `json:"prompt_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// SentChars is how many characters of one turn's request, as the egress guard
// sent it, were the system prompt, the tool list, and each role's messages.
type SentChars struct {
	System    int `json:"system"`
	Tools     int `json:"tools"`
	User      int `json:"user"`
	Assistant int `json:"assistant"`
	Tool      int `json:"tool"`
}

// DataResult is one sample_rows result as the egress guard sent it: whether
// any value went, and how many more columns its PII check withheld.
type DataResult struct {
	ToolCallID      string `json:"tool_call_id"`
	Sent            bool   `json:"sent"`
	WithheldColumns int    `json:"withheld_columns"`
}

// ProviderEntry is one entry llm-service's environment can serve.
type ProviderEntry struct {
	Label string `json:"label"`
	Model string `json:"model"`
}

// Providers is llm-service's GET /v1/assistant/providers. Never a key.
type Providers struct {
	Entries       map[string]ProviderEntry `json:"entries"`
	Known         []string                 `json:"known"`
	PanelDefaults map[string]string        `json:"panel_defaults"`
}

// Stepper is llm-service as the loop sees it.
type Stepper interface {
	Step(ctx context.Context, req StepRequest) (*StepResponse, error)
	Providers(ctx context.Context) (*Providers, error)
}

// StepError is a refusal or failure the user is told about: an HTTP status
// for api-gateway's own response, a short code and a sentence. It never
// carries a provider's own error text (llm-service drops that already).
type StepError struct {
	Status int
	Code   string
	Reason string
}

func (e *StepError) Error() string { return e.Code + ": " + e.Reason }

// HTTPStepper calls llm-service over HTTP.
type HTTPStepper struct {
	Client *http.Client
}

// Step sends one model turn.
func (h *HTTPStepper) Step(ctx context.Context, req StepRequest) (*StepResponse, error) {
	var out StepResponse
	if err := h.callLLMService(ctx, http.MethodPost, "/v1/assistant/step", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Providers lists the entries llm-service's environment can serve.
func (h *HTTPStepper) Providers(ctx context.Context) (*Providers, error) {
	var out Providers
	if err := h.callLLMService(ctx, http.MethodGet, "/v1/assistant/providers", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// callLLMService is the one hop to llm-service: the caller's context (so the
// request names the workspace and keeps the trace), the internal secret, and
// the step route's refusals turned into StepErrors.
func (h *HTTPStepper) callLLMService(ctx context.Context, method, path string, body, out any) error {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("LLM_SERVICE_URL")), "/")
	if base == "" {
		base = "http://llm-service:5000"
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET")); s != "" {
		req.Header.Set("X-Internal-Secret", s)
	}
	handlers.StampLLMWorkspace(req)
	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		req.Header.Set(k, v)
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &StepError{http.StatusGatewayTimeout, "timeout", "the assistant took too long to answer"}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			// Stopped, or the asker went away: not an outage of the model service.
			return ctx.Err()
		}
		return &StepError{http.StatusBadGateway, "llm_service_unreachable", "the assistant's model service is unreachable"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			return &StepError{http.StatusBadGateway, "llm_service_error", "the assistant's model service returned an unreadable answer"}
		}
		return nil
	}
	var refusal struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &refusal)
	return stepRefusal(resp.StatusCode, refusal.Error, refusal.Reason)
}

// stepRefusal maps the step route's refusals onto what api-gateway answers.
func stepRefusal(status int, code, reason string) *StepError {
	switch {
	case status == http.StatusConflict && code == "model_not_configured":
		return &StepError{http.StatusConflict, code, "this model has no key; an admin can add one in Admin > Settings > AI models"}
	case status == http.StatusBadGateway && code == "provider_error" && reason != "":
		return &StepError{http.StatusBadGateway, code, reason}
	case status == http.StatusUnprocessableEntity && code == "refused":
		return &StepError{http.StatusUnprocessableEntity, code, "the question could not be sent to the model: " + reason}
	case status == http.StatusUnprocessableEntity:
		return &StepError{http.StatusUnprocessableEntity, "bad_request", "the model service refused the request"}
	case status == http.StatusUnauthorized:
		return &StepError{http.StatusBadGateway, "llm_service_error", "api-gateway and llm-service do not share INTERNAL_SERVICE_SECRET"}
	default:
		return &StepError{http.StatusBadGateway, "llm_service_error", fmt.Sprintf("the assistant's model service returned HTTP %d", status)}
	}
}
