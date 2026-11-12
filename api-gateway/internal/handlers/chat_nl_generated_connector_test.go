package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// A connector generated at runtime (by the connector generator, an agent, or Ask
// rsync.ai) is a directory on disk and nothing else: only migrations write
// connector_catalog. The NL planner asked connector_catalog alone, so "sync
// acme-crm to postgresql" was refused as an unknown connector right after the
// generation that made acme-crm succeeded
// (docs/internal/ask-rsync-and-mcp-roadmap.md § 3).
//
// The class is "a connector the catalog page lists that the planner refuses", so
// the second test runs the real list handler over the repo's own connectors and
// asks the planner about each one it returns.

// writeDiskConnector lays a connector out the way the generator writes one:
// latest.json plus versions/v1.0.0/metadata.json, nothing at the root.
func writeDiskConnector(t *testing.T, root, rel string, meta map[string]any) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	vdir := filepath.Join(dir, "versions", "v1.0.0")
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		t.Fatal(err)
	}
	latest := []byte(`{"current_version":"v1.0.0","all_versions":["v1.0.0"]}`)
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), latest, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vdir, "metadata.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// catalogStates runs fn once with no catalog DB (the fallback set answers) and
// once with a catalog that knows postgresql only, as on an install whose
// migrations predate the connector.
func catalogStates(t *testing.T, fn func(t *testing.T)) {
	t.Run("no catalog DB", func(t *testing.T) {
		pinNoCatalogDB(t)
		fn(t)
	})
	t.Run("catalog without the connector", func(t *testing.T) {
		withFakeCatalog(t, "postgresql")
		fn(t)
	})
}

func TestAGeneratedConnectorIsKnownToThePlanner(t *testing.T) {
	root := t.TempDir()
	writeDiskConnector(t, root, "acme-crm", map[string]any{
		"id": "acme-crm", "connector_type": "acme-crm", "name": "Acme CRM",
		"category": "api_saas", "generated_at": "2026-10-01T12:00:00Z",
	})
	// Neither of these is on the catalog page, so neither may reach a pipeline.
	writeDiskConnector(t, root, "acme-internal", map[string]any{
		"id": "acme-internal", "connector_type": "acme-internal", "internal": true,
	})
	writeDiskConnector(t, root, "test-acme", map[string]any{
		"id": "test-acme", "connector_type": "test-acme",
	})
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", root)

	catalogStates(t, func(t *testing.T) {
		h := &ChatHandler{}
		if !h.isKnownConnector("acme-crm") {
			t.Fatal("a connector generated on disk is unknown to the planner")
		}
		for _, name := range []string{"acme-internal", "test-acme", "acme-erp", "i"} {
			if h.isKnownConnector(name) {
				t.Errorf("%q is known to the planner, but the catalog page does not list it", name)
			}
		}

		intent := h.quickParseDataSyncIntent("sync acme-crm to postgresql")
		if intent == nil {
			t.Fatal(`"sync acme-crm to postgresql" is not read by the pair parser`)
		}
		if intent.SourceType != "acme-crm" || intent.DestinationType != "postgresql" {
			t.Fatalf("parsed as %q -> %q, want acme-crm -> postgresql", intent.SourceType, intent.DestinationType)
		}

		// "Did you mean?" matches against the same names.
		if got, ok := h.fuzzyMatchKnownConnector("acme-crn"); !ok || got != "acme-crm" {
			t.Errorf(`fuzzy match of "acme-crn" = %q, %v; want acme-crm`, got, ok)
		}
	})
}

// catalogPageNames runs the catalog page's own handler and returns the name
// of every connector it lists.
func catalogPageNames(t *testing.T) []string {
	t.Helper()
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(docker.Close)
	t.Setenv("MCP_DOCKER_API_URL", docker.URL)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mcp/connectors", nil)
	ListMCPConnectors(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list handler: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Connectors []struct {
			Name string `json:"name"`
		} `json:"connectors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(body.Connectors))
	for _, c := range body.Connectors {
		names = append(names, c.Name)
	}
	return names
}

func TestEveryListedConnectorIsKnownToThePlanner(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "shared", "mcp-connectors", "public"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", root)
	names := catalogPageNames(t)
	// An empty list would pass the loop below without asking anything.
	if len(names) < 10 {
		t.Fatalf("the catalog page lists %d connectors from %s; the repo ships about 20", len(names), root)
	}

	catalogStates(t, func(t *testing.T) {
		h := &ChatHandler{}
		for _, name := range names {
			if !h.isKnownConnector(name) {
				t.Errorf("the catalog page lists %q but the planner calls it unknown", name)
			}
		}
		// Control: the disk names widen the planner's set by real connectors only.
		for _, word := range []string{"i", "data", "sync", "my", "table"} {
			if h.isKnownConnector(word) {
				t.Errorf("the ordinary word %q is a connector to the planner", word)
			}
		}
	})
}
