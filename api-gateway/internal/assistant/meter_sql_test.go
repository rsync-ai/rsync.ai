package assistant

// The Usage page's "NL→SQL queries (this month)" counted only Explorer's Ask AI
// box (1 on prod, 2026-10-03), though Ask rsync.ai writes SQL too: plan_workflow's
// sql and create_model's query. Each one a tool saves now counts as one query,
// display only, as a generation in Explorer does. A refused call, and a tool the
// model writes no SQL for, count nothing.
//
// Bug class: a meter fed by one of the two places the product turns words into
// SQL. sqlTools lists the tools whose SQL the model writes; a new one must be
// added there.

import (
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

const meterConn = "6f24346f-0000-4000-8000-000000000001"

func TestSavedSQLCountsAsOneNLQuery(t *testing.T) {
	for _, c := range []struct {
		name, tool, args string
		failed           bool
		want             []string
	}{
		{name: "a saved workflow draft", tool: "plan_workflow",
			args: `{"connection_id":"` + meterConn + `","sql":"select 1"}`, want: []string{meterConn}},
		{name: "a saved model", tool: "create_model",
			args: `{"connection_id":" ` + meterConn + ` ","query":"select 1"}`, want: []string{meterConn}},
		{name: "a refused model", tool: "create_model", failed: true,
			args: `{"connection_id":"` + meterConn + `","query":"delete from t"}`},
		// rsync.ai writes a silver model's SQL itself; the model wrote none.
		{name: "a silver model", tool: "create_silver_model",
			args: `{"connection_id":"` + meterConn + `","table":"public.orders"}`},
		{name: "a pipeline plan", tool: "plan_pipeline",
			args: `{"source_connection_id":"` + meterConn + `"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Limits{},
				callTools(ToolCall{ID: "c1", Name: c.tool, Arguments: c.args}),
				say("done"),
			)
			h.tools.extra = []string{c.tool}
			h.tools.made = map[string]string{c.tool: "made-1"}
			if c.failed {
				h.tools.results[c.tool] = "rsync.ai refused the query: it writes"
				h.tools.errs[c.tool] = true
			}
			h.ask(t, "u1", map[string]any{"question": "make it"})
			if !reflect.DeepEqual(h.metered, c.want) {
				t.Fatalf("metered %q, want %q", h.metered, c.want)
			}
		})
	}
}

// sqlTools is checked against the real tools: every tool with an argument the
// model writes a SELECT in is metered, by an argument naming its connection.
func TestSQLToolsAreTheToolsTheModelWritesSQLFor(t *testing.T) {
	gw, err := agentgw.New(agentgw.Options{
		Handlers: agentgw.DefaultHandlers(),
		Actor:    func(*gin.Context) services.Actor { return services.Actor{} },
		Audit:    func(*gin.Context, string, string, string, string, int) {},
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	writesSQL := map[string]bool{}
	props := map[string]map[string]any{}
	for _, spec := range gw.ToolSpecs() {
		p, _ := spec.InputSchema["properties"].(map[string]any)
		props[spec.Name] = p
		for _, v := range p {
			arg, _ := v.(map[string]any)
			if desc, _ := arg["description"].(string); strings.Contains(desc, "SELECT") {
				writesSQL[spec.Name] = true
			}
		}
	}
	// Armed: the two tools this was written for are found by the description rule.
	if !writesSQL["plan_workflow"] || !writesSQL["create_model"] {
		t.Fatalf("found SQL tools %v; the rule no longer finds plan_workflow and create_model", writesSQL)
	}
	for name := range writesSQL {
		if _, ok := sqlTools[name]; !ok {
			t.Errorf("%s takes SQL the model writes but is not in sqlTools, so saving it is not counted", name)
		}
	}
	for name, connArg := range sqlTools {
		if !writesSQL[name] {
			t.Errorf("sqlTools lists %s, which takes no SQL", name)
		}
		if _, ok := props[name][connArg]; !ok {
			t.Errorf("sqlTools reads %s's connection from %q, which it does not take", name, connArg)
		}
	}
}
