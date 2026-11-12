package workflowdef

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

// graphDef is the shape the step-5 editor preview showed: two pipelines in
// order, a model, a query on what it built, a condition and its send, and a
// plain send when the model is done.
func graphDef() Definition {
	return Definition{
		Trigger: Trigger{Type: "manual"},
		Steps: []Step{
			{ID: "load_orders", Type: StepRunPipeline, PipelineID: "p-orders"},
			{ID: "load_customers", Type: StepRunPipeline, PipelineID: "p-customers", After: []string{"load_orders"}},
			{ID: "revenue", Type: StepRefreshModel, ModelID: "m-revenue", After: []string{"load_customers"}},
			{ID: "check", Type: StepQuery, ConnectionID: "c1", SQL: "SELECT sum(total) AS revenue FROM revenue",
				After: []string{"revenue"}},
			{ID: "decide", Type: StepCondition, After: []string{"check"},
				Conditions: []Condition{{Type: "value", Column: "revenue", Op: "<", Threshold: 1000}}},
			{ID: "tell_finance", Type: StepSend, After: []string{"decide"}, Case: intp(0),
				Message: &Message{Body: "Revenue is {{revenue}}"}},
			{ID: "done", Type: StepSend, After: []string{"revenue"}, Rule: "all_done",
				Message: &Message{Body: "{{workflow_name}} finished at {{run_time}}"}},
		},
	}
}

func TestNormalize_AcceptsTheFullGraphAndFillsDefaults(t *testing.T) {
	d := graphDef()
	d.Steps[0].Message = &Message{Body: "dropped"} // a field its type does not use
	d.Steps[1].Rule = ""
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	lo, lc, rev, wait := d.Step("load_orders"), d.Step("load_customers"), d.Step("revenue"), d.Step("decide")
	if lo.Message != nil || lo.Rule != "" || lo.TimeoutMinutes != DefaultStepTimeoutMinutes {
		t.Fatalf("root = %+v", lo)
	}
	if lc.Rule != RuleAllSuccess || rev.TimeoutMinutes != 720 || wait.Rule != RuleAllSuccess {
		t.Fatalf("defaults: %+v %+v %+v", lc, rev, wait)
	}
	if d.IsLegacyShape() {
		t.Fatal("a graph with process steps is not slice 1's shape")
	}
	if len(d.ProcessSteps()) != 3 || len(d.SendSteps()) != 2 || d.Query().SQL == "" {
		t.Fatalf("accessors: %d process, %d sends", len(d.ProcessSteps()), len(d.SendSteps()))
	}

	// No query at all: sends after process steps only.
	only := Definition{Trigger: Trigger{Type: "manual"}, Steps: []Step{
		{ID: "load", Type: StepRunPipeline, PipelineID: "p"},
		{ID: "pause", Type: StepWait, Minutes: 5, After: []string{"load"}},
		{ID: "tell", Type: StepSend, After: []string{"pause"}, Rule: "any_failed", Message: &Message{Body: "{{link}}"}},
	}}
	if err := Normalize(&only); err != nil {
		t.Fatalf("a workflow without a query refused: %v", err)
	}
}

// Each broken graph is refused with the field and the step it is about.
func TestNormalize_RefusesEachBrokenGraph(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Definition)
		field string
		step  string
		says  string
	}{
		{"a cycle", func(d *Definition) { d.Steps[0].After = []string{"revenue"} }, "after", "load_orders", "circle"},
		{"after itself", func(d *Definition) { d.Steps[1].After = []string{"load_customers"} }, "after", "load_customers", "itself"},
		{"after a step that is not there", func(d *Definition) { d.Steps[1].After = []string{"nope"} }, "after", "load_customers", "not a step"},
		{"after one step twice", func(d *Definition) { d.Steps[1].After = []string{"load_orders", "load_orders"} }, "after", "load_customers", "twice"},
		{"two steps with one id", func(d *Definition) { d.Steps[1].ID = "load_orders" }, "id", "load_orders", "Two steps"},
		{"an id with capitals", func(d *Definition) { d.Steps[0].ID = "Load" }, "id", "Load", "not a step id"},
		{"an unknown type", func(d *Definition) { d.Steps[0].Type = "loop" }, "type", "load_orders", "not one of"},
		{"an unknown rule", func(d *Definition) { d.Steps[1].Rule = "one_success" }, "rule", "load_customers", "not one of"},
		{"run_pipeline without a pipeline", func(d *Definition) { d.Steps[0].PipelineID = " " }, "pipeline_id", "load_orders", "needs the pipeline"},
		{"run_pipeline of the trigger's pipeline", func(d *Definition) {
			d.Trigger = Trigger{Type: "after_pipeline", PipelineID: "p-customers"}
		}, "pipeline_id", "load_customers", "forever"},
		{"a timeout over a week", func(d *Definition) { d.Steps[0].TimeoutMinutes = MaxStepTimeoutMinutes + 1 }, "timeout_minutes", "load_orders", "1 to"},
		{"four retries", func(d *Definition) { d.Steps[0].Retries = 4 }, "retries", "load_orders", "0 to 3"},
		{"a model retried", func(d *Definition) { d.Steps[2].Retries = 1 }, "retries", "revenue", "not retried"},
		{"refresh_model without a model", func(d *Definition) { d.Steps[2].ModelID = "" }, "model_id", "revenue", "needs the model"},
		{"a wait of 0", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "pause", Type: StepWait, After: []string{"revenue"}})
		}, "minutes", "pause", "1 to 1440"},
		{"a wait over a day", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "pause", Type: StepWait, Minutes: 1441, After: []string{"revenue"}})
		}, "minutes", "pause", "1 to 1440"},
		{"no send", func(d *Definition) {
			d.Steps = []Step{{ID: "load", Type: StepRunPipeline, PipelineID: "p"}}
		}, "message", "", "needs a message"},
		{"two queries", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "check2", Type: StepQuery, ConnectionID: "c", SQL: "SELECT 1"})
		}, "steps", "check2", "one query"},
		{"a query no condition reads", func(d *Definition) { d.Steps = append(d.Steps[:4], d.Steps[6]) }, "steps", "check", "no condition"},
		{"a condition not after the query", func(d *Definition) { d.Steps[4].After = []string{"check", "revenue"} }, "after", "decide", "query step check alone"},
		{"a condition with rule all_done", func(d *Definition) { d.Steps[4].Rule = "all_done" }, "after", "decide", "all_success"},
		{"a step after the query besides the condition", func(d *Definition) { d.Steps[6].After = []string{"check"} }, "after", "done", "only the condition"},
		{"a plain send after the condition", func(d *Definition) { d.Steps[6].After = []string{"decide"} }, "after", "done", "not one of its sends"},
		{"a case send with rule all_done", func(d *Definition) { d.Steps[5].Rule = "all_done" }, "after", "tell_finance", "all_success"},
		{"two sends for one case", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "tell_ops", Type: StepSend, After: []string{"decide"}, Case: intp(0),
				Message: &Message{Body: "x"}})
		}, "case", "tell_ops", "both send for condition 1"},
		{"a case past the conditions", func(d *Definition) { d.Steps[5].Case = intp(1) }, "case", "tell_finance", "condition 2"},
		{"a condition with no send", func(d *Definition) {
			d.Steps[4].Conditions = append(d.Steps[4].Conditions, Condition{Type: "always"})
		}, "message", "decide", "Condition 2: A workflow needs a message"},
		{"a case send with no condition step", func(d *Definition) {
			d.Steps = []Step{{ID: "tell", Type: StepSend, Case: intp(0), Message: &Message{Body: "x"}}}
		}, "case", "tell", "no condition step"},
		{"a plain send using a query slot", func(d *Definition) { d.Steps[6].Message.Body = "{{row_count}} rows" }, "message", "done", "{{row_count}}"},
		{"a plain send with no body", func(d *Definition) { d.Steps[6].Message = nil }, "message", "done", "needs a message"},
		{"a query without SQL names its step", func(d *Definition) { d.Steps[3].SQL = "" }, "sql", "check", "SQL query"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := graphDef()
			tc.edit(&d)
			var inv *Invalid
			if !errors.As(Normalize(&d), &inv) {
				t.Fatal("not refused")
			}
			if inv.Field != tc.field || inv.Step != tc.step || !strings.Contains(inv.Message, tc.says) {
				t.Fatalf("got %+v", inv)
			}
		})
	}
	if MaxSteps < 2+2*MaxBranches {
		t.Fatalf("MaxSteps %d cannot hold %d conditions and their sends", MaxSteps, MaxBranches)
	}
	big := Definition{Trigger: Trigger{Type: "manual"}}
	for i := 0; i <= MaxSteps; i++ {
		big.Steps = append(big.Steps, Step{ID: "w" + strings.Repeat("a", i%30) + string(rune('a'+i%26)), Type: StepWait, Minutes: 1})
	}
	if inv := (*Invalid)(nil); !errors.As(Normalize(&big), &inv) || inv.Field != "steps" {
		t.Fatalf("%d steps accepted", MaxSteps+1)
	}
}

// The rules over every parent outcome, and a skip carried two levels down:
// all_success steps skip under a skipped parent, an all_done step runs.
func TestGate_TruthTable(t *testing.T) {
	ok := StepOutcome{Status: StatusSucceeded, Case: -1}
	warn := StepOutcome{Status: StatusWarned, Case: -1}
	fail := StepOutcome{Status: StatusFailed, Case: -1}
	skip := StepOutcome{Status: StatusSkipped, Case: -1}
	stop := StepOutcome{Status: StatusStopped, Case: -1}
	two := func(rule string) Step { return Step{ID: "c", Type: StepWait, After: []string{"a", "b"}, Rule: rule} }
	cases := []struct {
		name      string
		s         Step
		done      map[string]StepOutcome
		ready, ok bool
	}{
		{"root", Step{ID: "r", Type: StepWait}, nil, true, true},
		{"a parent still running", two(RuleAllSuccess), map[string]StepOutcome{"a": ok}, false, false},
		{"all_success, both passed", two(RuleAllSuccess), map[string]StepOutcome{"a": ok, "b": warn}, true, true},
		{"all_success, one failed", two(RuleAllSuccess), map[string]StepOutcome{"a": ok, "b": fail}, true, false},
		{"all_success, one skipped", two(RuleAllSuccess), map[string]StepOutcome{"a": ok, "b": skip}, true, false},
		{"all_success, one stopped", two(RuleAllSuccess), map[string]StepOutcome{"a": ok, "b": stop}, true, false},
		{"empty rule is all_success", two(""), map[string]StepOutcome{"a": ok, "b": fail}, true, false},
		{"all_done, failed and skipped", two(RuleAllDone), map[string]StepOutcome{"a": fail, "b": skip}, true, true},
		{"any_failed, one failed", two(RuleAnyFailed), map[string]StepOutcome{"a": ok, "b": fail}, true, true},
		{"any_failed, none failed", two(RuleAnyFailed), map[string]StepOutcome{"a": ok, "b": warn}, true, false},
		{"any_failed, skipped is not failed", two(RuleAnyFailed), map[string]StepOutcome{"a": skip, "b": skip}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, run, note := Gate(tc.s, tc.done)
			if ready != tc.ready || run != tc.ok {
				t.Fatalf("ready=%v run=%v, want %v %v", ready, run, tc.ready, tc.ok)
			}
			if ready && !run && note == "" {
				t.Fatal("a skipped step says why")
			}
		})
	}

	// a fails → b (all_success) skips → c (all_success) skips → d (all_done) runs.
	steps := []Step{
		{ID: "a", Type: StepWait},
		{ID: "b", Type: StepWait, After: []string{"a"}, Rule: RuleAllSuccess},
		{ID: "c", Type: StepWait, After: []string{"b"}, Rule: RuleAllSuccess},
		{ID: "d", Type: StepWait, After: []string{"c"}, Rule: RuleAllDone},
		{ID: "e", Type: StepWait, After: []string{"a"}, Rule: RuleAnyFailed},
	}
	done := map[string]StepOutcome{"a": fail}
	want := map[string]string{"b": StatusSkipped, "c": StatusSkipped, "d": StatusSucceeded, "e": StatusSucceeded}
	for _, s := range steps[1:] {
		ready, run, _ := Gate(s, done)
		if !ready {
			t.Fatalf("%s not ready", s.ID)
		}
		got := StatusSkipped
		if run {
			got = StatusSucceeded
		}
		if got != want[s.ID] {
			t.Fatalf("%s = %s, want %s", s.ID, got, want[s.ID])
		}
		done[s.ID] = StepOutcome{Status: got, Case: -1}
	}

	// A condition's sends: only the case it took runs.
	s0 := Step{ID: "s0", Type: StepSend, After: []string{"cond"}, Rule: RuleAllSuccess, Case: intp(0)}
	s1 := Step{ID: "s1", Type: StepSend, After: []string{"cond"}, Rule: RuleAllSuccess, Case: intp(1)}
	took1 := map[string]StepOutcome{"cond": {Status: StatusSucceeded, Case: 1}}
	if _, run, note := Gate(s0, took1); run || !strings.Contains(note, "Condition 2") {
		t.Fatalf("case 0 under case 1: run=%v note=%q", run, note)
	}
	if _, run, _ := Gate(s1, took1); !run {
		t.Fatal("case 1 did not run")
	}
	if _, run, note := Gate(s1, map[string]StepOutcome{"cond": {Status: StatusSucceeded, Case: -1}}); run || note == "" {
		t.Fatal("no case taken, a send ran")
	}
}

func TestRunStatus(t *testing.T) {
	d := graphDef()
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	all := func(s string) map[string]string {
		m := map[string]string{}
		for _, st := range d.Steps {
			m[st.ID] = s
		}
		return m
	}
	if got := RunStatus(d, all(StatusSucceeded), false); got != StatusSucceeded {
		t.Fatalf("all passed: %s", got)
	}
	st := all(StatusSucceeded)
	st["done"] = StatusFailed // a send not marked required
	if got := RunStatus(d, st, false); got != StatusSucceeded {
		t.Fatalf("an optional send failed: %s", got)
	}
	d.Step("done").Required = true
	if got := RunStatus(d, st, false); got != StatusFailed {
		t.Fatalf("a required send failed: %s", got)
	}
	st = all(StatusSucceeded)
	st["revenue"] = StatusFailed
	if got := RunStatus(d, st, false); got != StatusFailed {
		t.Fatalf("a model failed: %s", got)
	}
	st["revenue"] = StatusWarned
	if got := RunStatus(d, st, false); got != StatusSucceeded {
		t.Fatalf("a model warned: %s", got)
	}
	if got := RunStatus(d, st, true); got != StatusStopped {
		t.Fatalf("stopped: %s", got)
	}
	// Slice 1's sends are required: a failed delivery fails the run, as before.
	legacy := validDef().build()
	if !legacy.CaseSend(0).Required {
		t.Fatal("a legacy case send is not required")
	}
}

// Version 2 (a query and branches) reads as steps; version 3 is read as is.
func TestDefinition_ReadsVersion2And3(t *testing.T) {
	v2json := `{"version":2,"trigger":{"type":"manual"},"query":{"connection_id":"c","sql":"SELECT 1"},` +
		`"branches":[{"condition":{"type":"rows_exist"},"message":{"body":"a"}},{"condition":{"type":"always"},"message":{"body":"b"}}]}`
	var d Definition
	if err := json.Unmarshal([]byte(v2json), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 3 || len(d.Steps) != 4 || d.CaseSend(1).ID != "send_2" || d.Branches()[1].Message.Body != "b" ||
		!d.IsLegacyShape() {
		t.Fatalf("read as %+v", d)
	}
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}

	g := graphDef()
	out, _ := json.Marshal(g)
	var back Definition
	if err := json.Unmarshal(out, &back); err != nil || len(back.Steps) != len(g.Steps) || back.Step("load_orders") == nil {
		t.Fatalf("v3 round trip: %v", err)
	}
}

// Editing the conditions keeps each case's send id, so its channels stay.
func TestReplaceBranches_KeepsSendIDs(t *testing.T) {
	d := FromBranches(Trigger{Type: "manual"}, Query{ConnectionID: "c", SQL: "SELECT 1"},
		[]Branch{branch(Condition{Type: "rows_exist"}, "a"), branch(Condition{Type: "always"}, "b")}, 0)
	d.Steps = append(d.Steps, Step{ID: "after_all", Type: StepSend, After: []string{"send_1"}, Rule: RuleAllDone,
		Message: &Message{Body: "x"}})

	d.ReplaceBranches([]Branch{branch(Condition{Type: "no_rows"}, "A"), branch(Condition{Type: "rows_exist"}, "B"),
		branch(Condition{Type: "always"}, "C")})
	if d.CaseSend(0).ID != "send_1" || d.CaseSend(1).ID != "send_2" || d.CaseSend(2).ID != "send_3" ||
		d.CaseSend(2).After[0] != "condition" || d.Branches()[0].Message.Body != "A" || d.Step("after_all") == nil {
		t.Fatalf("grown: %+v", d.Steps)
	}
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}

	d.ReplaceBranches([]Branch{branch(Condition{Type: "always"}, "only")})
	if len(d.Branches()) != 1 || d.CaseSend(1) != nil || d.Step("send_2") != nil || d.CaseSend(0).ID != "send_1" {
		t.Fatalf("shrunk: %+v", d.Steps)
	}

	d.SetQuery(Query{ConnectionID: "c2", SQL: "SELECT 2"})
	if q := d.Query(); q.ConnectionID != "c2" || q.SQL != "SELECT 2" || len(d.Steps) != 4 {
		t.Fatalf("set query: %+v", d.Steps)
	}
}

func TestClone_SharesNothing(t *testing.T) {
	d := graphDef()
	c := d.Clone()
	c.Steps[0].ID = "x"
	c.Step("load_customers").After[0] = "x"
	c.ConditionStep().Conditions[0].Threshold = 1
	*c.CaseSend(0).Case = 5
	c.Step("done").Message.Body = "x"
	c.ReplaceBranches([]Branch{branch(Condition{Type: "always"}, "y")})
	if d.Steps[0].ID != "load_orders" || d.Step("load_customers").After[0] != "load_orders" ||
		d.ConditionStep().Conditions[0].Threshold != 1000 || d.CaseSend(0) == nil ||
		d.Step("done").Message.Body == "x" || d.Branches()[0].Message.Body != "Revenue is {{revenue}}" {
		t.Fatalf("the clone's edits reached the original: %+v", d.Steps)
	}
}

// chartDef is dashboards D3's shape: a model refresh, two charts of what it
// built, and one plain send that carries both.
func chartDef() Definition {
	return Definition{
		Trigger: Trigger{Type: "schedule", Cron: "0 9 * * 1"},
		Steps: []Step{
			{ID: "revenue", Type: StepRefreshModel, ModelID: "m-revenue"},
			{ID: "by_day", Type: StepChart, ChartID: " ch-1 ", After: []string{"revenue"}, Message: &Message{Body: "x"},
				SQL: "SELECT 1", Minutes: 5},
			{ID: "by_region", Type: StepChart, ChartID: "ch-2", After: []string{"revenue"}},
			{ID: "tell", Type: StepSend, After: []string{"by_day", "by_region"}, ChartID: "ch-3",
				Message: &Message{Subject: "Weekly revenue", Body: "{{workflow_name}} at {{run_time}}: {{link}}"}},
		},
	}
}

func TestNormalize_ChartStepBeforeAPlainSend(t *testing.T) {
	d := chartDef()
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	c := d.Step("by_day")
	if c.ChartID != "ch-1" || c.Message != nil || c.SQL != "" || c.Minutes != 0 || c.Rule != RuleAllSuccess {
		t.Fatalf("chart step = %+v", c)
	}
	if d.Step("tell").ChartID != "" {
		t.Fatal("a send keeps no chart_id: it carries the charts it runs after")
	}
	got := d.DrawingsBefore(d.Step("tell"))
	if len(got) != 2 || got[0].ID != "by_day" || got[1].ID != "by_region" {
		t.Fatalf("DrawingsBefore = %v", got)
	}
	if len(d.DrawingsBefore(d.Step("by_day"))) != 0 {
		t.Fatal("a refresh_model parent is not a chart")
	}
	if d.IsLegacyShape() || len(d.ProcessSteps()) != 1 {
		t.Fatal("a chart is neither slice 1's shape nor a process step")
	}
	if cs := d.ChartSteps(); len(cs) != 2 || cs[0].ID != "by_day" || cs[1].ID != "by_region" {
		t.Fatalf("ChartSteps = %v", cs)
	}

	// A chart at the root of a scheduled workflow, and a chart beside a
	// query's decision: the condition's sends stay as they were.
	root := Definition{Trigger: Trigger{Type: "manual"}, Steps: []Step{
		{ID: "chart", Type: StepChart, ChartID: "ch"},
		{ID: "tell", Type: StepSend, After: []string{"chart"}, Message: &Message{Body: "{{link}}"}},
	}}
	if err := Normalize(&root); err != nil {
		t.Fatalf("schedule → chart → send refused: %v", err)
	}
	g := graphDef()
	g.Steps = append(g.Steps, Step{ID: "chart", Type: StepChart, ChartID: "ch", After: []string{"revenue"}})
	g.Steps[6].After = []string{"chart"}
	if err := Normalize(&g); err != nil {
		t.Fatalf("a chart before the plain send of a graph with a decision refused: %v", err)
	}
}

// Each misplaced chart is refused with the field and the step it is about.
func TestNormalize_RefusesEachMisplacedChart(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Definition)
		field string
		step  string
		says  string
	}{
		{"no chart", func(d *Definition) { d.Steps[1].ChartID = " " }, "chart_id", "by_day", "needs the chart"},
		{"no send after it", func(d *Definition) { d.Steps[3].After = []string{"by_region"} }, "after", "by_day",
			"needs a send after it"},
		{"a wait after it", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "pause", Type: StepWait, Minutes: 1, After: []string{"by_day"}})
		}, "after", "pause", "only a send may"},
		{"a chart after it", func(d *Definition) { d.Steps[2].After = []string{"by_day"} }, "after", "by_region",
			"only a send may"},
		{"a condition's send after it", func(d *Definition) {
			g := graphDef()
			g.Steps = append(g.Steps, Step{ID: "chart", Type: StepChart, ChartID: "ch", After: []string{"revenue"}})
			g.Steps[5].After = []string{"decide", "chart"}
			*d = g
		}, "after", "tell_finance", "condition step decide alone"},
		{"a chart after the condition", func(d *Definition) {
			g := graphDef()
			g.Steps = append(g.Steps, Step{ID: "chart", Type: StepChart, ChartID: "ch", After: []string{"decide"}},
				Step{ID: "tell_chart", Type: StepSend, After: []string{"chart"}, Message: &Message{Body: "x"}})
			*d = g
		}, "after", "chart", "not one of its sends"},
		{"a chart after the query", func(d *Definition) {
			g := graphDef()
			g.Steps = append(g.Steps, Step{ID: "chart", Type: StepChart, ChartID: "ch", After: []string{"check"}},
				Step{ID: "tell_chart", Type: StepSend, After: []string{"chart"}, Message: &Message{Body: "x"}})
			*d = g
		}, "after", "chart", "only the condition"},
		{"a send after a chart using a query slot", func(d *Definition) { d.Steps[3].Message.Body = "{{row_count}}" },
			"message", "tell", "{{row_count}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := chartDef()
			tc.edit(&d)
			var inv *Invalid
			if !errors.As(Normalize(&d), &inv) {
				t.Fatal("not refused")
			}
			if inv.Field != tc.field || inv.Step != tc.step || !strings.Contains(inv.Message, tc.says) {
				t.Fatalf("got %+v", inv)
			}
		})
	}
}

// dashboardDef is D3's dashboard shape: a model refresh, then a whole board
// and one chart drawn from it, and one plain send that carries both.
func dashboardDef() Definition {
	return Definition{
		Trigger: Trigger{Type: "schedule", Cron: "0 9 * * 1"},
		Steps: []Step{
			{ID: "revenue", Type: StepRefreshModel, ModelID: "m-revenue"},
			{ID: "board", Type: StepDashboard, DashboardID: " db-1 ", ChartID: "ch-9", After: []string{"revenue"},
				Message: &Message{Body: "x"}, SQL: "SELECT 1", Minutes: 5},
			{ID: "by_day", Type: StepChart, ChartID: "ch-1", DashboardID: "db-9", After: []string{"revenue"}},
			{ID: "tell", Type: StepSend, After: []string{"board", "by_day"}, DashboardID: "db-3",
				Message: &Message{Subject: "Weekly revenue", Body: "{{workflow_name}} at {{run_time}}: {{link}}"}},
		},
	}
}

func TestNormalize_DashboardStepBeforeAPlainSend(t *testing.T) {
	d := dashboardDef()
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	b := d.Step("board")
	if b.DashboardID != "db-1" || b.ChartID != "" || b.Message != nil || b.SQL != "" || b.Minutes != 0 {
		t.Fatalf("dashboard step = %+v", b)
	}
	if d.Step("by_day").DashboardID != "" || d.Step("tell").DashboardID != "" {
		t.Fatal("only a dashboard step keeps a dashboard_id")
	}
	got := d.DrawingsBefore(d.Step("tell"))
	if len(got) != 2 || got[0].ID != "board" || got[1].ID != "by_day" {
		t.Fatalf("DrawingsBefore = %v", got)
	}
	if ds := d.DashboardSteps(); len(ds) != 1 || ds[0].ID != "board" {
		t.Fatalf("DashboardSteps = %v", ds)
	}
	if cs := d.ChartSteps(); len(cs) != 1 || cs[0].ID != "by_day" {
		t.Fatalf("ChartSteps = %v", cs)
	}
	if !b.IsDrawing() || b.IsProcess() || d.IsLegacyShape() || len(d.ProcessSteps()) != 1 {
		t.Fatal("a dashboard draws, and is neither slice 1's shape nor a process step")
	}
}

// Each misplaced dashboard is refused with the field and the step it is about.
func TestNormalize_RefusesEachMisplacedDashboard(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Definition)
		field string
		step  string
		says  string
	}{
		{"no dashboard", func(d *Definition) { d.Steps[1].DashboardID = " " }, "dashboard_id", "board",
			"needs the dashboard"},
		{"no send after it", func(d *Definition) { d.Steps[3].After = []string{"by_day"} }, "after", "board",
			"draws a dashboard for a message, so it needs a send after it"},
		{"a wait after it", func(d *Definition) {
			d.Steps = append(d.Steps, Step{ID: "pause", Type: StepWait, Minutes: 1, After: []string{"board"}})
		}, "after", "pause", "runs after the dashboard board, and only a send may"},
		{"a chart after it", func(d *Definition) { d.Steps[2].After = []string{"board"} }, "after", "by_day",
			"only a send may"},
		{"a dashboard after the query", func(d *Definition) {
			g := graphDef()
			g.Steps = append(g.Steps, Step{ID: "board", Type: StepDashboard, DashboardID: "db", After: []string{"check"}},
				Step{ID: "tell_board", Type: StepSend, After: []string{"board"}, Message: &Message{Body: "x"}})
			*d = g
		}, "after", "board", "only the condition"},
		{"a send after a dashboard using a query slot", func(d *Definition) { d.Steps[3].Message.Body = "{{row_count}}" },
			"message", "tell", "{{row_count}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := dashboardDef()
			tc.edit(&d)
			var inv *Invalid
			if !errors.As(Normalize(&d), &inv) {
				t.Fatal("not refused")
			}
			if inv.Field != tc.field || inv.Step != tc.step || !strings.Contains(inv.Message, tc.says) {
				t.Fatalf("got %+v", inv)
			}
		})
	}
}
