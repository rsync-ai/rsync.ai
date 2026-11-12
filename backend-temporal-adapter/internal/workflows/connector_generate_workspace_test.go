package workflows

// The connector generator names the workspace a pipeline runs for.
//
// The tool-generator logs every model call in an llm_egress line and names its
// workspace from the request's X-Workspace-ID (llm-service src/egress/scope.py).
// The workflow knows the pipeline's workspace (NLPipelineWorkflowV2Input.WorkspaceID),
// but the generate request did not carry it, so those lines read workspace=-.

import (
	"net/http"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

const genWS = "33333333-3333-3333-3333-333333333333"

const genGateBody = `{"error":"llm_not_configured","message":"` + llmGateCustomSentence + `"}`

func TestGenerateConnectorActivityV2_NamesTheWorkspace(t *testing.T) {
	t.Run("the request's workspace", func(t *testing.T) {
		gen := newLLMGateGenerator(t, http.StatusServiceUnavailable, genGateBody)
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestActivityEnvironment()
		env.RegisterActivity(GenerateConnectorActivityV2)
		_, _ = env.ExecuteActivity(GenerateConnectorActivityV2, GenerateConnectorRequest{
			CorrelationID: "pipe-llm-1-exec-llm-1",
			ConnectorType: "gcs",
			WorkspaceID:   genWS,
		})
		if got := gen.workspaceHeaders(); len(got) != 1 || got[0] != genWS {
			t.Fatalf("tool-generator saw X-Workspace-ID %q, want [%s]", got, genWS)
		}
	})

	t.Run("control: none known sends none", func(t *testing.T) {
		gen := newLLMGateGenerator(t, http.StatusServiceUnavailable, genGateBody)
		_ = runGenerateConnectorActivity(t)
		if got := gen.workspaceHeaders(); len(got) != 1 || got[0] != "" {
			t.Fatalf("tool-generator saw X-Workspace-ID %q, want one request with none", got)
		}
	})
}

// The workflow hands the pipeline's workspace to the generate request.
func TestNLPipelineV2_ConnectorGenerationNamesThePipelineWorkspace(t *testing.T) {
	gen := newLLMGateGenerator(t, http.StatusServiceUnavailable, genGateBody)
	versionCalls := 0
	r := runLLMGatePipeline(t, &llmGateHarness{workspaceID: genWS}, countReasonVersion(&versionCalls))

	requireReachedGeneration(t, r)
	if got := gen.workspaceHeaders(); len(got) != 1 || got[0] != genWS {
		t.Fatalf("tool-generator saw X-Workspace-ID %q, want [%s]", got, genWS)
	}
}
