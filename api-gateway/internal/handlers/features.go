package handlers

import (
	"api-gateway/internal/config"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// GetFeatureFlags returns the current feature flags
// This endpoint is public (no auth required) as it only exposes UI-relevant flags
func GetFeatureFlags(c *gin.Context) {
	features := config.GetFeatures()
	if features == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "feature flags not initialized",
		})
		return
	}

	out := features.ToAPIResponse()
	out["llm_configured"] = llmConfigured(c.Request.Context())
	c.JSON(http.StatusOK, out)
}

// llm_configured says whether this install has an LLM set up, so the UI shows
// what needs one (editing a workflow) only when it does. llm-service owns the
// answer (llm_configured in its /health, from LLM_PROVIDER and the provider's
// keys), and every page load asks /features, so the answer is reused for
// llmConfiguredTTL instead of asked each time.
const (
	llmConfiguredTTL     = 30 * time.Second
	llmConfiguredTimeout = 2 * time.Second
)

var llmConfiguredCache struct {
	sync.Mutex
	value   bool
	checked time.Time
}

// llmConfigured is false whenever llm-service cannot say: down, slow, an
// error status, or an llm-service too old to report the field. The UI then
// hides what needs an LLM, which would fail without one anyway.
func llmConfigured(ctx context.Context) bool {
	llmConfiguredCache.Lock()
	defer llmConfiguredCache.Unlock()
	if !llmConfiguredCache.checked.IsZero() && time.Since(llmConfiguredCache.checked) < llmConfiguredTTL {
		return llmConfiguredCache.value
	}
	// Detached from the request: a page that navigates away mid-probe must not
	// leave a "false" in the cache for everyone else.
	llmConfiguredCache.value = probeLLMConfigured(context.WithoutCancel(ctx))
	llmConfiguredCache.checked = time.Now()
	return llmConfiguredCache.value
}

func probeLLMConfigured(ctx context.Context) bool {
	base := strings.TrimSpace(os.Getenv("LLM_SERVICE_URL"))
	if base == "" {
		base = "http://llm-service:5000"
	}
	ctx, cancel := context.WithTimeout(ctx, llmConfiguredTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return false
	}
	stampLLMWorkspace(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	var body struct {
		LLMConfigured bool `json:"llm_configured"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return false
	}
	return body.LLMConfigured
}
