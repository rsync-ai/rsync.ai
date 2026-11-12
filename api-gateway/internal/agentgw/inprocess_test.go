package agentgw

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// The in-app assistant runs these tools in-process through Call. Call must be
// the same path as tools/call: the actor's scope check, the argument checks,
// the typed output and one audit row per call.

var assistantActor = services.Actor{UserID: "u1", WorkspaceID: "w1", Role: security.WSViewer,
	Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}

// assistantAdmin is the assistant acting for a workspace admin, who may
// generate a connector.
var assistantAdmin = services.Actor{UserID: "u1", WorkspaceID: "w1", Role: security.WSAdmin,
	Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}

func inProcess(t *testing.T, actor services.Actor) (*Server, *gin.Context, *[]auditRow) {
	t.Helper()
	var rows []auditRow
	s, err := New(Options{
		Handlers: leakyHandlers(),
		Actor:    func(*gin.Context) services.Actor { return actor },
		Audit: func(_ *gin.Context, tool, rt, rid, outcome string, status int) {
			rows = append(rows, auditRow{tool, rt, rid, outcome, status})
		},
		Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/api/v1/assistant/messages", nil)
	c.Set("user_id", actor.UserID)
	c.Set("workspace_id", actor.WorkspaceID)
	return s, c, &rows
}

func TestToolSpecsListEveryToolWithAPortableSchema(t *testing.T) {
	s, _, _ := inProcess(t, assistantActor)
	specs := s.ToolSpecs()
	if len(specs) != len(s.Tools()) || len(specs) == 0 {
		t.Fatalf("ToolSpecs() = %d specs, Tools() = %d", len(specs), len(s.Tools()))
	}
	for i, spec := range specs {
		tool := s.Tools()[i]
		if spec.Name != tool.Name || spec.Description != tool.Description {
			t.Fatalf("spec %d = %q, tool = %q", i, spec.Name, tool.Name)
		}
		full := inputSchema(tool)
		if !reflect.DeepEqual(spec.InputSchema["required"], full["required"]) {
			t.Fatalf("%s: required %v, tools/list says %v", spec.Name, spec.InputSchema["required"], full["required"])
		}
		raw, _ := json.Marshal(spec.InputSchema)
		for _, kw := range []string{"additionalProperties", "format", "maxLength", "minimum"} {
			if strings.Contains(string(raw), `"`+kw+`"`) {
				t.Fatalf("%s: schema carries %q, which not every provider accepts: %s", spec.Name, kw, raw)
			}
		}
	}
}

func TestCallRunsEachToolAndReturnsOnlyItsDeclaredFields(t *testing.T) {
	s, c, rows := inProcess(t, assistantAdmin)
	for name, args := range validArgs {
		raw, _ := json.Marshal(args)
		text, isErr := s.Call(c, name, raw)
		if isErr {
			t.Fatalf("%s: error result %q", name, text)
		}
		// Control: the call returned real data, so the leak check reads something.
		if !strings.Contains(text, "orders") && !strings.Contains(text, "warehouse") && !strings.Contains(text, runID) &&
			!strings.Contains(text, "Google Cloud Storage") && !strings.Contains(text, "acme-crm") &&
			!strings.Contains(text, `"plan":"starter"`) {
			t.Fatalf("%s: answer carries none of the fixture's names: %s", name, text)
		}
		if strings.Contains(text, canary) || strings.Contains(text, fmt.Sprint(canaryNum)) {
			t.Fatalf("%s: answer leaks a field no tool declares: %s", name, text)
		}
	}
	if len(*rows) != len(validArgs) {
		t.Fatalf("audit rows = %d, want one per call (%d)", len(*rows), len(validArgs))
	}
	for _, r := range *rows {
		if r.outcome != "ok" {
			t.Fatalf("audit row %+v, want ok", r)
		}
	}
}

// The assistant carries the generate scope for everyone, so the role rule is
// what refuses a viewer or a member, and the refusal says who can.
func TestCallRefusesGenerationBelowAdmin(t *testing.T) {
	for _, role := range []security.WorkspaceRole{security.WSViewer, security.WSMember} {
		actor := assistantAdmin
		actor.Role = role
		s, c, rows := inProcess(t, actor)
		text, isErr := s.Call(c, "generate_connector", json.RawMessage(`{"name":"acme-crm","spec_url":"https://api.acme.example/openapi.json"}`))
		if !isErr || !strings.Contains(text, "only a workspace admin or owner") {
			t.Fatalf("%s: Call = (%q, %v), want the admin refusal", role, text, isErr)
		}
		if len(*rows) != 1 || (*rows)[0] != (auditRow{"generate_connector", "connector_generation", "", "forbidden", 403}) {
			t.Fatalf("%s: audit = %+v", role, *rows)
		}
	}
	// Control: an admin is not refused, and the row names the run it started.
	s, c, rows := inProcess(t, assistantAdmin)
	if text, isErr := s.Call(c, "generate_connector", json.RawMessage(`{"name":"acme-crm","spec_url":"https://api.acme.example/openapi.json"}`)); isErr {
		t.Fatalf("control: admin refused: %s", text)
	}
	if len(*rows) != 1 || (*rows)[0] != (auditRow{"generate_connector", "connector_generation", genID, "ok", 200}) {
		t.Fatalf("control: audit = %+v", *rows)
	}
}

func TestCallRefusesWhatToolsCallRefuses(t *testing.T) {
	s, c, rows := inProcess(t, assistantActor)
	cases := []struct {
		name, args, want string
	}{
		{"drop_pipeline", `{}`, "Unknown tool"},
		{"get_pipeline", `{"pipeline_id": "not-a-uuid"}`, "Invalid arguments"},
		{"get_pipeline", `{"pipeline_id": "` + pipeID + `", "sql": "drop"}`, "unknown argument"},
		{"get_pipeline", `{"pipeline_id": `, "Invalid arguments"},
	}
	for _, tc := range cases {
		text, isErr := s.Call(c, tc.name, json.RawMessage(tc.args))
		if !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("Call(%s, %s) = (%q, %v), want an error containing %q", tc.name, tc.args, text, isErr, tc.want)
		}
	}
	// Every call to a real tool is audited, unreadable JSON included, as the
	// MCP route does; an unknown tool never reaches one.
	if len(*rows) != 3 {
		t.Fatalf("audit rows = %+v, want three invalid_arguments rows", *rows)
	}
	for _, r := range *rows {
		if r.tool != "get_pipeline" || r.outcome != "invalid_arguments" || r.status != 400 {
			t.Fatalf("audit rows = %+v, want three invalid_arguments rows", *rows)
		}
	}
}

// Mark labels the context the handler runs in, so the rows the handler
// audits say who acted; the caller's own context is left as it was.
func TestMarkLabelsOnlyTheHandlerContext(t *testing.T) {
	var keys map[any]any
	h := leakyHandlers()
	b := leakyBodies()
	h.GetPipeline = func(c *gin.Context) {
		keys = c.Keys
		c.JSON(200, b["get_pipeline"])
	}
	s, err := New(Options{
		Handlers: h,
		Actor:    func(*gin.Context) services.Actor { return assistantActor },
		Audit:    func(*gin.Context, string, string, string, string, int) {},
		Mark:     func(sc *gin.Context) { sc.Set("actor_kind", "assistant") },
		Version:  "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/api/v1/assistant/messages", nil)
	c.Set("user_id", "u1")
	c.Set("workspace_id", "w1")
	if text, isErr := s.Call(c, "get_pipeline", json.RawMessage(`{"pipeline_id":"`+pipeID+`"}`)); isErr {
		t.Fatal(text)
	}
	if keys["actor_kind"] != "assistant" || keys["user_id"] != "u1" {
		t.Fatalf("handler keys = %v, want the caller's keys plus the mark", keys)
	}
	if _, ok := c.Keys["actor_kind"]; ok {
		t.Fatalf("the mark reached the caller's context: %v", c.Keys)
	}
}

func TestCallHoldsTheAssistantToItsScope(t *testing.T) {
	noScope := assistantActor
	noScope.Scopes = nil
	noScope.Role = security.WSOwner
	s, c, rows := inProcess(t, noScope)
	text, isErr := s.Call(c, "get_pipeline", json.RawMessage(`{"pipeline_id": "`+pipeID+`"}`))
	if !isErr || !strings.Contains(text, "The assistant may not use get_pipeline") {
		t.Fatalf("Call without the scope = (%q, %v), want the assistant's refusal", text, isErr)
	}
	if len(*rows) != 1 || (*rows)[0].outcome != "forbidden" {
		t.Fatalf("audit rows = %+v, want one forbidden row", *rows)
	}
}
