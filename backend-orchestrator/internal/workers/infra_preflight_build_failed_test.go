package workers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rsync-ai/backend-orchestrator/internal/agents/executor"
)

// A batch run whose source connector image fails to build stops at preflight with
// the build log's cause. Before, preflight read any start error on a batch source as
// "stdio fallback will be used" and passed, so the run went on to read on the
// orchestrator's own interpreter, which has none of the connector's dependencies.
func TestABatchRunFailsPreflightWhenTheConnectorImageFailedToBuild(t *testing.T) {
	const buildFailure = "connector image failed to build. End of the build log:\n" +
		"ERROR: Could not find a version that satisfies the requirement acme-sdk==9.9.9"

	deployAnswer := func(reason string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"building":false,"error_message":` + strconv.Quote(reason) + `}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	source := preflightService{
		name:        "acme MCP (source)",
		kind:        "mcp_user",
		requireHTTP: false,
		mcpName:     "acme",
		mcpVersion:  "v1.0.0",
	}

	agent := executor.NewAgent(nil, nil, t.TempDir())
	t.Cleanup(func() { _ = agent.Stop() })
	p := newInfraPreflightStage(agent, nil)

	t.Setenv("TOOL_GENERATOR_URL", deployAnswer(buildFailure).URL)
	r := p.checkOne(context.Background(), "pid", "eid", source)
	if r.OK {
		t.Fatalf("a batch source whose image failed to build passed preflight (mode %q)", r.Mode)
	}
	failures, _ := summarizePreflight([]preflightResult{r})
	if len(failures) != 1 || !strings.Contains(failures[0], buildFailure) {
		t.Errorf("the preflight failure does not carry the build log's cause: %q", failures)
	}

	// Control: a deploy that failed for another reason keeps the batch stdio fallback.
	t.Setenv("TOOL_GENERATOR_URL", deployAnswer("Deployer failed: daemon down").URL)
	if r := p.checkOne(context.Background(), "pid", "eid", source); !r.OK {
		t.Errorf("a non-build deploy failure now fails a batch preflight: %v", r.Err)
	}
}
