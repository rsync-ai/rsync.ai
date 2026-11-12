package handlers

import (
	"api-gateway/internal/config"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func resetLLMConfiguredCache(t *testing.T) {
	t.Helper()
	clear := func() {
		llmConfiguredCache.Lock()
		llmConfiguredCache.value = false
		llmConfiguredCache.checked = time.Time{}
		llmConfiguredCache.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// llmHealthServer is llm-service's /health answering status and body, and
// counts how often it was asked.
func llmHealthServer(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probed %s, want /health", r.URL.Path)
		}
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LLM_SERVICE_URL", srv.URL+"/")
	return &hits
}

func TestLLMConfigured_ReadsLLMServiceHealth(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"an LLM is set up", 200, `{"status":"ok","service":"llm-gateway","llm_configured":true}`, true},
		{"no LLM", 200, `{"status":"ok","service":"llm-gateway","llm_configured":false}`, false},
		{"an llm-service too old to say", 200, `{"status":"ok"}`, false},
		{"an error status", 503, `{"llm_configured":true}`, false},
		{"not JSON", 200, `<html>bad gateway</html>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLLMConfiguredCache(t)
			llmHealthServer(t, tc.status, tc.body)
			if got := llmConfigured(t.Context()); got != tc.want {
				t.Errorf("llmConfigured = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("llm-service down", func(t *testing.T) {
		resetLLMConfiguredCache(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		t.Setenv("LLM_SERVICE_URL", url)
		if llmConfigured(t.Context()) {
			t.Error("llmConfigured = true with llm-service down, want false")
		}
	})
}

func TestLLMConfigured_ReusesTheAnswer(t *testing.T) {
	resetLLMConfiguredCache(t)
	hits := llmHealthServer(t, 200, `{"llm_configured":true}`)
	for range 3 {
		if !llmConfigured(t.Context()) {
			t.Fatal("llmConfigured = false, want true")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("/health asked %d times in one TTL, want 1", n)
	}

	// Past the TTL it asks again.
	llmConfiguredCache.Lock()
	llmConfiguredCache.checked = time.Now().Add(-llmConfiguredTTL - time.Second)
	llmConfiguredCache.Unlock()
	llmConfigured(t.Context())
	if n := hits.Load(); n != 2 {
		t.Fatalf("/health asked %d times after the TTL, want 2", n)
	}
}

func TestGetFeatureFlags_SaysWhetherAnLLMIsSetUp(t *testing.T) {
	config.LoadFeatures()
	for _, want := range []bool{true, false} {
		resetLLMConfiguredCache(t)
		body, _ := json.Marshal(map[string]bool{"llm_configured": want})
		llmHealthServer(t, 200, string(body))

		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.GET("/api/v1/features", GetFeatureFlags)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/features", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["llm_configured"] != want {
			t.Errorf("llm_configured = %v, want %v (body %s)", got["llm_configured"], want, w.Body.String())
		}
		if _, ok := got["assistant"]; !ok {
			t.Errorf("the other flags went missing: %s", w.Body.String())
		}
	}
}
