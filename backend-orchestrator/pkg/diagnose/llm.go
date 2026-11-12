// llm.go — v2 LLM-backed Diagnoser, chained BEHIND the rules.
//
// ChainDiagnoser runs the RuleBasedDiagnoser first, always. The LLM is
// consulted only when the rules return CategoryUnknown — so every error
// class the rules recognise (CDC provisioning failures in particular,
// which MUST escalate per the CLAUDE.md rule) structurally never
// reaches the LLM.
//
// Safety rails on the LLM result:
//   - category/action are validated against the closed enums; anything
//     outside them discards the LLM answer and keeps the rules result.
//   - confidence is clamped to [0, llmConfidenceCap] where the cap sits
//     below heal's AutoBand (0.85) — the LLM can suggest, but can never
//     trigger auto-execution on its own.
//   - any transport/decode/validation error falls back to the rules
//     result; the healer path never fails because the LLM did.
//
// Privacy: the prompt text is llm-service's, not this file's. The diagnoser
// POSTs a registered prompt name (prompts/diagnose/healer_classify.yaml) and
// its variables to llm-service's /v1/completion, whose egress guard renders
// the prompt field by field — Go never reaches a model provider directly.
// signal.ErrorMessage passes llmscrub.ScrubMax before it is sent; every other
// variable is metadata (connector types, stage, and whether rows were read and
// written) — never a row count, row values, credentials, or PII.
package diagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rsync-ai/backend-orchestrator/pkg/llmjson"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"
)

const (
	defaultLLMServiceURL = "http://llm-service:5000"
	// The registered prompt llm-service renders for this diagnoser.
	llmDiagnosePromptName = "diagnose/healer_classify"
	llmRequestTimeout     = 15 * time.Second
	// Max runes of scrubbed error text included in the prompt.
	llmErrorMessageCap = 1500
	// Cap the LLM's confidence just below the auto-execute band so an LLM
	// diagnosis can reach HITL but never auto-execute. Derived from the single
	// source of truth AutoExecuteBand (0.85) so it can never drift above it;
	// TestLLMConfidenceCapBelowAutoExecuteBand fails closed if it ever does.
	llmConfidenceCap = AutoExecuteBand - 0.01
)

var (
	validLLMCategories = map[Category]bool{
		CategoryAuthExpired:  true,
		CategoryAuthScope:    true,
		CategoryRateLimit:    true,
		CategorySchemaDrift:  true,
		CategoryNetwork:      true,
		CategoryConnectorBug: true,
		CategoryDestCapacity: true,
		CategoryUserConfig:   true,
		CategoryUnknown:      true,
	}
	validLLMActions = map[Action]bool{
		ActionRegenerateConnector: true,
		ActionRefreshAuth:         true,
		ActionBackoffRetry:        true,
		ActionRequestUserConfig:   true,
		ActionEscalate:            true,
		ActionNoOp:                true,
	}

	// Guards the one-line "which diagnoser did we build" log in NewFromEnv.
	// Both production call sites construct a Diagnoser per sweep
	// (internal/agents/heal/worker.go and .../issue_sweep.go), so an
	// unconditional log would repeat on every sweep tick.
	diagnoserModeLogOnce sync.Once
)

// LLMDiagnoser classifies a Signal through llm-service's POST /v1/completion.
// llm-service picks the model (LLM_MODEL, else the prompt's default). It
// satisfies Diagnoser, but callers should normally use it via ChainDiagnoser
// so the rules run first.
type LLMDiagnoser struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewLLMDiagnoserFromEnv builds an LLMDiagnoser from env vars. This is the
// single, canonical LLM error-diagnosis path — it replaced a second, divergent
// LLM classifier that used to live in the healer's dead DLQ consumer.
func NewLLMDiagnoserFromEnv() *LLMDiagnoser {
	// Diagnoser-specific override FIRST, mirroring the model resolution below.
	// LLM_SERVICE_URL is a shared name with two consumers that disagree:
	// internal/workers/intent.go reads it with a fallback of
	// http://planner:5011, while this diagnoser falls back to
	// http://llm-service:5000. Neither is set anywhere today, so both run on
	// their own default — and an operator who sets the shared name to steer one
	// of them would silently repoint the other. RSYNC_LLM_DIAGNOSER_URL steers
	// only this one. Behaviour is unchanged when it is unset or empty.
	baseURL := os.Getenv("RSYNC_LLM_DIAGNOSER_URL")
	if baseURL == "" {
		baseURL = os.Getenv("LLM_SERVICE_URL")
	}
	if baseURL == "" {
		baseURL = defaultLLMServiceURL
	}
	return &LLMDiagnoser{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: llmRequestTimeout},
	}
}

// Diagnose satisfies the Diagnoser interface. On any LLM failure it
// returns the same low-confidence unknown fallback the rules produce.
func (d *LLMDiagnoser) Diagnose(signal Signal) Diagnosis {
	dx, err := d.diagnoseLLM(signal)
	if err != nil {
		log.WithError(err).Warn("diagnose: LLM diagnoser failed")
		return Diagnosis{
			Category:        CategoryUnknown,
			SuggestedAction: ActionEscalate,
			Confidence:      0.3,
			Rationale:       "no rule matched; human inspection required",
		}
	}
	return dx
}

// llmDiagnosisResponse is the JSON object the model is instructed to emit.
type llmDiagnosisResponse struct {
	Category   string  `json:"category"`
	Action     string  `json:"action"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

func (d *LLMDiagnoser) diagnoseLLM(signal Signal) (Diagnosis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), llmRequestTimeout)
	defer cancel()

	reqBody, err := json.Marshal(map[string]interface{}{
		"prompt_name": llmDiagnosePromptName,
		"variables":   llmDiagnoseVariables(signal),
		"pipeline_id": signal.PipelineID,
	})
	if err != nil {
		return Diagnosis{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", d.BaseURL+"/v1/completion", bytes.NewBuffer(reqBody))
	if err != nil {
		return Diagnosis{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: llmRequestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Diagnosis{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The status only: the body is not read, because an error answer can
		// quote the request and this error is logged.
		return Diagnosis{}, fmt.Errorf("llm-service returned status %d", resp.StatusCode)
	}

	// llm-service's /v1/completion answer: { "content": "...", "model": "...", ... }
	var llmResp struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&llmResp); err != nil {
		return Diagnosis{}, fmt.Errorf("failed to decode llm-service response: %w", err)
	}
	if llmResp.Content == "" {
		return Diagnosis{}, fmt.Errorf("invalid llm-service response: no content")
	}

	var parsed llmDiagnosisResponse
	if err := json.Unmarshal([]byte(llmjson.ExtractObject(llmResp.Content)), &parsed); err != nil {
		return Diagnosis{}, fmt.Errorf("failed to parse LLM diagnosis: %w", err)
	}

	category := Category(parsed.Category)
	if !validLLMCategories[category] {
		return Diagnosis{}, fmt.Errorf("LLM returned unknown category %q", parsed.Category)
	}
	action := Action(parsed.Action)
	if !validLLMActions[action] {
		return Diagnosis{}, fmt.Errorf("LLM returned unknown action %q", parsed.Action)
	}

	confidence := parsed.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > llmConfidenceCap {
		confidence = llmConfidenceCap
	}

	rationale := parsed.Rationale
	if rationale == "" {
		rationale = "classified by LLM diagnoser"
	}

	return Diagnosis{
		Category:        category,
		SuggestedAction: action,
		Confidence:      confidence,
		Rationale:       "llm: " + rationale,
	}, nil
}

// llmDiagnoseVariables builds the variables of prompts/diagnose/healer_classify.yaml,
// whose llm_fields tag each one for llm-service's egress guard. The error
// message is scrubbed + truncated here as well as there (privacy contract:
// schema metadata and user text only — never row values, credentials, or
// PII); everything else is metadata.
func llmDiagnoseVariables(signal Signal) map[string]string {
	return map[string]string{
		// Closed-set metadata today ("executor"/"sentinel", an execution
		// status, connector type names), so Scrub is a no-op on every real
		// value. Applied anyway so the metadata-only privacy claim holds by
		// construction rather than depending on every future caller keeping
		// these fields closed-set.
		"stage":            llmscrub.Scrub(signal.Stage),
		"executor_status":  llmscrub.Scrub(signal.ExecutorStatus),
		"source_type":      llmscrub.Scrub(signal.SourceType),
		"destination_type": llmscrub.Scrub(signal.DestinationType),
		// Yes or no, never the count: a row count describes the customer's
		// data. No table names either.
		"source_rows_read":   yesNo(signal.SourceRowCount > 0),
		"rows_written":       yesNo(signal.WrittenRows > 0),
		"tables_landed_none": yesNo(signal.TablesWithNoLandedRows > 0),
		"error_message":      llmscrub.ScrubMax(signal.ErrorMessage, llmErrorMessageCap),
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ChainDiagnoser — rules first, LLM only for what the rules can't name.
type ChainDiagnoser struct {
	Rules Diagnoser
	LLM   *LLMDiagnoser
}

// Diagnose runs the rules and returns their result whenever they
// produced a real classification. Only a CategoryUnknown result is
// handed to the LLM; on any LLM failure the rules result stands.
func (c *ChainDiagnoser) Diagnose(signal Signal) Diagnosis {
	rules := c.Rules.Diagnose(signal)
	if rules.Category != CategoryUnknown || c.LLM == nil {
		return rules
	}
	dx, err := c.LLM.diagnoseLLM(signal)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"pipeline_id":  signal.PipelineID,
			"execution_id": signal.ExecutionID,
		}).Warn("diagnose: LLM diagnoser failed; keeping rules result")
		return rules
	}
	log.WithFields(log.Fields{
		"pipeline_id":  signal.PipelineID,
		"execution_id": signal.ExecutionID,
		"category":     dx.Category,
		"action":       dx.SuggestedAction,
		"confidence":   dx.Confidence,
	}).Debug("diagnose: LLM diagnoser response")
	return dx
}

// NewFromEnv returns the production Diagnoser. With
// RSYNC_LLM_DIAGNOSER_ENABLED=true it chains the LLM diagnoser behind
// the rules; otherwise (the default) it returns the rules alone, so no
// HTTP client is ever constructed.
//
// Which of the two was built is logged ONCE per process, at Info. Without
// that line the flag is invisible from outside: a rules-only orchestrator and
// a rules+LLM one are indistinguishable in the logs until something fails.
func NewFromEnv() Diagnoser {
	enabled := os.Getenv("RSYNC_LLM_DIAGNOSER_ENABLED") == "true"
	diagnoserModeLogOnce.Do(func() {
		mode := "rules-only"
		if enabled {
			mode = "rules+llm"
		}
		log.WithFields(log.Fields{
			"diagnoser": mode,
			"flag":      "RSYNC_LLM_DIAGNOSER_ENABLED",
			"enabled":   enabled,
		}).Info("diagnose: constructed diagnoser")
	})
	if !enabled {
		return New()
	}
	return &ChainDiagnoser{
		Rules: New(),
		LLM:   NewLLMDiagnoserFromEnv(),
	}
}
