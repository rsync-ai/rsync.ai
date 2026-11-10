package assistant

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
	"unicode/utf8"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

const stepRoutes = "../../../llm-service/src/gateway/assistant_routes.py"

// llm-service refuses a step whose tool list is longer than its StepRequest
// allows, and that refusal fails every question. The list is every agent
// gateway tool plus sample_rows and generate_sql, so a tool added past the cap
// would break Ask rsync.ai with nothing failing in Go. The cap was 24 with 23 tools offered.
func TestToolListFitsTheLLMServiceCap(t *testing.T) {
	raw, err := os.ReadFile(stepRoutes)
	if err != nil {
		t.Fatalf("read %s: %v", stepRoutes, err)
	}
	m := regexp.MustCompile(`tools: list\[StepTool\] = Field\([^)]*max_length=(\d+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no max_length on StepRequest.tools; update this test to where the cap is", stepRoutes)
	}
	limit, _ := strconv.Atoi(string(m[1]))

	specs := realToolSpecs(t)
	if len(specs) < 20 {
		t.Fatalf("the agent gateway lists %d tools; this test would pass on too little", len(specs))
	}
	if offered := len(specs) + 2; offered > limit { // + sample_rows, generate_sql
		t.Fatalf("Ask rsync.ai offers %d tools but llm-service accepts at most %d (%s): raise the cap",
			offered, limit, stepRoutes)
	}
}

// Every step sends the whole tool list, so each character in it is paid on
// every step of every question. The list the loop sends, with sample_rows and
// generate_sql, was 43,136 characters before the tools' rules moved to the
// guides (docs/internal/assistant-token-design.md § 7.1) and 35,244 after, with
// read_guide added. A new tool or a longer description past the budget fails
// here, so the list grows only when someone raises it on purpose: 36,000 became
// 36,200 for a chart's date grain (dashboards-design.md § 14.3), an argument of
// plan_dashboard and plan_dashboard_change, its rule in the dashboards guide; 38,400
// for D4b's five chart types, a combo's second measure, a histogram's bins and a
// scorecard's sparkline (§ 14.9), nine arguments in each of those two tools, with
// the list of what each type takes moved from the type argument to the guide; 39,600
// for D4c's scatter, bubble and pivot table: a size, a pivot's row fields and a third
// measure, five arguments in each of those two tools; 40,400 for D4d's eight types: a
// bullet chart's target, and the x, y, second measure and row fields of the new types;
// 40,800 for D4e's map: its shapes, an argument of each of those two tools, and a map's
// codes in the x argument.
const maxToolListChars = 40800

func TestToolListStaysWithinItsBudget(t *testing.T) {
	h := newHarnessSQL(t, &memStore{}, openSamples(), writesSQL(), Limits{}, say("ok"))
	h.tools.specs = realToolSpecs(t)
	h.ask(t, "u1", map[string]any{"question": "q"})
	if len(h.llm.reqs) != 1 {
		t.Fatalf("steps = %d, want 1", len(h.llm.reqs))
	}
	tools := h.llm.reqs[0].Tools
	// Armed: the list is the whole one, with the assistant's own two tools.
	for _, name := range []string{SampleRowsTool, GenerateSQLTool, agentgw.GuideTool, "plan_workflow"} {
		if !offers(h.llm.reqs[0], name) {
			t.Fatalf("%s is not in the tool list; this test would measure too little", name)
		}
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCount(raw); n > maxToolListChars {
		t.Fatalf("the %d tools sent on every step take %d characters, over the budget of %d: "+
			"move a rule to its guide, or raise maxToolListChars on purpose", len(tools), n, maxToolListChars)
	}
}

// realToolSpecs are the agent gateway's tools, as the assistant is given them.
func realToolSpecs(t *testing.T) []agentgw.ToolSpec {
	t.Helper()
	gw, err := agentgw.New(agentgw.Options{
		Handlers: agentgw.DefaultHandlers(),
		Actor:    func(*gin.Context) services.Actor { return services.Actor{} },
		Audit:    func(*gin.Context, string, string, string, string, int) {},
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw.ToolSpecs()
}
