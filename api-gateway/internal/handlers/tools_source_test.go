package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// The connector list must report a tool-generated connector as `generated`
// even when its metadata.json carries no explicit `source`. The tool
// generator's template (metadata.json.j2) writes `generated_at` at the TOP
// level; the gateway used to look for it only under `qa_metadata`, so every
// connector it generated (github-rest, notion-rest, stripe, widgets-graphql,
// and any generated on an install we cannot patch) listed as `built_in`, and
// the confidence chip was suppressed for all of them.
//
// Bug class: a reader looks for a field where the writer does not put it.
// The writer side is pinned by llm-service's test_metadata_declares_origin.py;
// this pins the reader against the shape the writer has always produced.
func TestListMCPConnectors_ResolvesOriginFromGeneratorShapedMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	t.Setenv("MCP_CONNECTORS_PATH", tmpDir)
	publicRoot := filepath.Join(tmpDir, "public")

	// extra is spliced into each metadata.json after the common keys.
	cases := []struct {
		id    string
		extra string
		want  string
	}{
		// Exactly what metadata.json.j2 rendered before it wrote `source`.
		{"gen-top-level", `"generated_at": "2026-05-10T00:00:00Z"`, "generated"},
		// The shape the reader was written for keeps working.
		{"gen-qa-metadata", `"qa_metadata": {"generated_at": "2026-05-10T00:00:00Z"}`, "generated"},
		// An explicit source always wins: hand-curated connectors that also
		// carry a top-level generated_at (postgresql, mysql, gcs, …) stay built_in.
		{"explicit-built-in", `"source": "built_in", "generated_at": "2026-05-10T00:00:00Z"`, "built_in"},
		{"explicit-partner", `"source": "partner"`, "partner"},
		// No evidence of generator origin → the conservative default.
		{"no-evidence", `"status": "active"`, "built_in"},
	}

	for _, tc := range cases {
		connDir := filepath.Join(publicRoot, "api", tc.id)
		writeFile(t, filepath.Join(connDir, "latest.json"), []byte(`{"current_version": "v1.0.0"}`))
		writeFile(t, filepath.Join(connDir, "versions", "v1.0.0", "metadata.json"), []byte(`{
  "id": "`+tc.id+`",
  "name": "`+tc.id+`",
  "version": "1.0.0",
  "connector_type": "`+tc.id+`",
  "category": "api_saas",
  "supports_source": true,
  "config_schema": {"type": "object", "properties": {}},
  `+tc.extra+`
}`))
	}

	r := gin.New()
	r.GET("/api/v1/connectors", ListMCPConnectors)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connectors", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Connectors []MCPConnector `json:"connectors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]string{}
	for _, c := range resp.Connectors {
		got[c.Name] = c.Source
	}
	// An empty listing would pass every per-case check below vacuously.
	if len(got) != len(cases) {
		t.Fatalf("expected %d connectors listed, got %d: %v", len(cases), len(got), got)
	}
	for _, tc := range cases {
		if got[tc.id] != tc.want {
			t.Errorf("%s: source = %q, want %q", tc.id, got[tc.id], tc.want)
		}
	}
}

// The top-level signal must not turn internal plumbing into "generated":
// kafka-mcp-sink ships a top-level generated_at and no source.
func TestResolveConnectorSource_InternalTopLevelGeneratedAtStaysBuiltIn(t *testing.T) {
	meta := connectorMetadataDTO{GeneratedAt: "2026-05-10T00:00:00Z", Internal: true}
	if got := resolveConnectorSource(meta, true); got != "built_in" {
		t.Fatalf("declared-internal connector: source = %q, want built_in", got)
	}
	// A dev-only connector is forced internal for listing only; its declared
	// flag is false, so its generator origin still counts.
	if got := resolveConnectorSource(meta, false); got != "generated" {
		t.Fatalf("dev-only (not declared internal) connector: source = %q, want generated", got)
	}
	// Any JSON type counts as present, and none can fail the unmarshal.
	var fromJSON connectorMetadataDTO
	if err := json.Unmarshal([]byte(`{"generated_at": 1715299200}`), &fromJSON); err != nil {
		t.Fatalf("numeric generated_at must not fail the metadata unmarshal: %v", err)
	}
	if got := resolveConnectorSource(fromJSON, false); got != "generated" {
		t.Fatalf("numeric generated_at: source = %q, want generated", got)
	}
}
