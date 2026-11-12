package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// SEC-M-08: per-workspace rate limit on connector generation.
func TestConnectorGenRateLimitMiddleware_LimitsPerWorkspace(t *testing.T) {
	gin.SetMode(gin.TestMode)

	prev := connectorGenLimiter
	connectorGenLimiter = newFixedWindowLimiter(1, time.Hour)
	defer func() { connectorGenLimiter = prev }()

	r := gin.New()
	r.POST("/connectors/generate",
		func(c *gin.Context) { c.Set("workspace_id", "ws1") }, // mirror WorkspaceContext .Use
		ConnectorGenRateLimitMiddleware(),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) },
	)

	// First call within the window is allowed.
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, httptest.NewRequest(http.MethodPost, "/connectors/generate", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("first call: expected %d, got %d", http.StatusOK, w1.Code)
	}

	// Second call in the same window is throttled with a Retry-After hint.
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/connectors/generate", nil))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call: expected %d, got %d", http.StatusTooManyRequests, w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429")
	}
}

func callGenerateConnector(body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/connectors/generate", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	GenerateConnector(c)
	return w
}

// SEC-M-08: generation must never overwrite a hand-curated core connector, even
// with force_regenerate set (which bypasses the already-exists short-circuit).
func TestGenerateConnector_RefusesProtectedCore(t *testing.T) {
	w := callGenerateConnector(`{"api_name":"postgresql","force_regenerate":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected %d for protected core connector, got %d (%s)", http.StatusForbidden, w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, w.Body.String())
	}
	if ok, _ := resp["success"].(bool); ok {
		t.Fatalf("expected success=false, got %s", w.Body.String())
	}
}

// The protected set is matched after normalization, so aliases (postgres ->
// postgresql) are refused too.
func TestGenerateConnector_RefusesProtectedCore_Alias(t *testing.T) {
	w := callGenerateConnector(`{"api_name":"postgres","force_regenerate":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected %d for protected alias, got %d (%s)", http.StatusForbidden, w.Code, w.Body.String())
	}
}

// A name the connector list hides (isDevOnlyConnector) is refused before any
// generation: the connector would be built and then never shown to anyone.
func TestGenerateConnector_RefusesANameTheListHides(t *testing.T) {
	for _, name := range []string{"test-acme", "http-test-connector", "postgresql-generated"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TOOL_GENERATOR_URL", "http://127.0.0.1:1") // never reached
			w := callGenerateConnector(`{"api_name":"` + name + `","force_regenerate":true}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected %d for %s, got %d (%s)", http.StatusBadRequest, name, w.Code, w.Body.String())
			}
			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v (%s)", err, w.Body.String())
			}
			if msg, _ := resp["error"].(string); !strings.Contains(msg, "hidden from the connector list") {
				t.Fatalf("the refusal does not say why: %s", w.Body.String())
			}
		})
	}
}

// A dry run saves nothing, so a hidden name is no harm there.
func TestGenerateConnector_DryRunOfAHiddenNameIsForwarded(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"status":"generated"}`))
	}))
	defer stub.Close()
	t.Setenv("TOOL_GENERATOR_URL", stub.URL)

	w := callGenerateConnector(`{"api_name":"test-acme","save_artifacts":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected %d for a dry run, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
}

// A non-protected id is not caught by the guard; generation proceeds and the
// request is forwarded to tool-generator (stubbed here).
func TestGenerateConnector_ForwardsNonProtected(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"generated"}`))
	}))
	defer stub.Close()
	t.Setenv("TOOL_GENERATOR_URL", stub.URL)

	w := callGenerateConnector(`{"api_name":"unit-test-fake-connector","force_regenerate":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected %d for non-protected connector, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, w.Body.String())
	}
	if ok, _ := resp["success"].(bool); !ok {
		t.Fatalf("expected success=true forwarded from tool-generator, got %s", w.Body.String())
	}
}

func TestIsProtectedCoreConnector(t *testing.T) {
	protected := []string{"postgresql", "postgres", "aws-s3", "s3", "stripe", "kafka-mcp-sink", "shopify-admin-graphql"}
	for _, id := range protected {
		if !isProtectedCoreConnector(id) {
			t.Errorf("expected %q to be protected", id)
		}
	}
	notProtected := []string{"pokeapi", "nasa-apod", "my-custom-api", "unit-test-fake-connector", "pipedrive"}
	for _, id := range notProtected {
		if isProtectedCoreConnector(id) {
			t.Errorf("expected %q to NOT be protected", id)
		}
	}
}

// Every reserved name must resolve to a connector in this repo. The list kept
// pipedrive, hubspot, slack, linear and metabase after #277 deleted those
// connectors, so generating one was refused as "a built-in connector" that the
// catalog did not show. A name may resolve through an alias (notion ->
// notion-rest).
func TestProtectedConnectorIDsNameAConnectorOnDisk(t *testing.T) {
	shared := filepath.Join("..", "..", "..", "shared", "mcp-connectors")
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", filepath.Join(shared, "public"))
	t.Setenv("MCP_INTERNAL_CONNECTORS_PATH", filepath.Join(shared, "internal"))

	// Controls: the check finds a real connector, and finds nothing for a name
	// no connector has, so a pass below is not a lookup that always says yes or no.
	if !mcpConnectorIsVersioned("github-rest") {
		t.Fatalf("github-rest did not resolve under %s; the lookup is not reading the repo tree", shared)
	}
	if mcpConnectorIsVersioned("unit-test-fake-connector") {
		t.Fatal("a name with no connector resolved; the lookup cannot tell a missing connector")
	}

	for id := range protectedConnectorIDs {
		if !mcpConnectorIsVersioned(id) {
			t.Errorf("%q is reserved but no connector in shared/mcp-connectors resolves to it: "+
				"drop it from protectedConnectorIDs and _PROTECTED_CORE_CONNECTORS (tool_generator/service.py)", id)
		}
	}
}
