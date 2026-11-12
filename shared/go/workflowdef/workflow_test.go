package workflowdef

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// v2 is slice 1's shape, which most of these tests edit field by field
// before reading it as steps (build).
type v2 struct {
	Trigger      Trigger
	Query        Query
	Branches     []Branch
	QuietMinutes int
}

func (l v2) build() Definition { return FromBranches(l.Trigger, l.Query, l.Branches, l.QuietMinutes) }

func validDef() v2 {
	return v2{
		Trigger: Trigger{Type: "schedule", Cron: "0 9 * * 1"},
		Query:   Query{ConnectionID: "c1", SQL: "SELECT 1"},
		Branches: []Branch{{
			Condition: Condition{Type: "always"},
			Message:   Message{Subject: "Weekly", Body: "Revenue: {{revenue}}"},
		}},
	}
}

func TestNormalize_RefusesEachBrokenField(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*v2)
		field string
	}{
		{"unknown trigger", func(d *v2) { d.Trigger.Type = "hourly" }, "trigger"},
		{"schedule without cron", func(d *v2) { d.Trigger.Cron = "" }, "cron"},
		{"six-field cron", func(d *v2) { d.Trigger.Cron = "0 0 9 * * 1" }, "cron"},
		{"bad timezone", func(d *v2) { d.Trigger.Timezone = "Mars/Base" }, "timezone"},
		{"Local timezone", func(d *v2) { d.Trigger.Timezone = "Local" }, "timezone"},
		{"after_pipeline without pipeline", func(d *v2) { d.Trigger = Trigger{Type: "after_pipeline"} }, "pipeline_id"},
		{"no connection", func(d *v2) { d.Query.ConnectionID = " " }, "connection_id"},
		{"no sql", func(d *v2) { d.Query.SQL = "" }, "sql"},
		{"sql too long", func(d *v2) { d.Query.SQL = "SELECT " + strings.Repeat("1", MaxSQLLen) }, "sql"},
		{"unknown condition", func(d *v2) { d.Branches[0].Condition.Type = "changed" }, "condition"},
		{"value without op", func(d *v2) { d.Branches[0].Condition = Condition{Type: "value"} }, "condition_op"},
		{"no body", func(d *v2) { d.Branches[0].Message.Body = "  " }, "message"},
		{"long subject", func(d *v2) { d.Branches[0].Message.Subject = strings.Repeat("x", MaxSubjectLen+1) }, "subject"},
		{"logic in body", func(d *v2) { d.Branches[0].Message.Body = "{{#each rows}}x{{/each}}" }, "message"},
		{"dotted name", func(d *v2) { d.Branches[0].Message.Body = "{{rows.0.total}}" }, "message"},
		{"negative quiet", func(d *v2) { d.QuietMinutes = -1 }, "quiet_minutes"},
		{"quiet over a week", func(d *v2) { d.QuietMinutes = MaxQuietMinutes + 1 }, "quiet_minutes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := validDef()
			tc.edit(&l)
			d := l.build()
			err := Normalize(&d)
			var inv *Invalid
			if !errors.As(err, &inv) {
				t.Fatalf("want *Invalid, got %v", err)
			}
			if inv.Field != tc.field {
				t.Fatalf("field = %q, want %q (%s)", inv.Field, tc.field, inv.Message)
			}
		})
	}
}

func TestNormalize_FillsDefaultsAndDropsFieldsTheTriggerDoesNotUse(t *testing.T) {
	l := validDef()
	l.Trigger.Cron = "  0  9 * *   1 "
	l.Trigger.PipelineID = "p1"
	l.Branches[0].Condition = Condition{Type: "", Column: "x", Op: ">", Threshold: 3}
	d := l.build()
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	if d.Trigger.Cron != "0 9 * * 1" || d.Trigger.Timezone != "UTC" || d.Trigger.PipelineID != "" {
		t.Fatalf("trigger = %+v", d.Trigger)
	}
	if d.Branches()[0].Condition != (Condition{Type: ConditionAlways}) {
		t.Fatalf("condition = %+v", d.Branches()[0].Condition)
	}
	if d.Version != 3 {
		t.Fatalf("version = %d", d.Version)
	}

	ml := validDef()
	ml.Trigger = Trigger{Type: "manual", Cron: "0 9 * * 1", PipelineID: "p"}
	ml.Branches[0].Condition = Condition{Type: "value", Op: "greater_than", Threshold: 50}
	m := ml.build()
	if err := Normalize(&m); err != nil {
		t.Fatal(err)
	}
	if m.Trigger != (Trigger{Type: "manual"}) || m.Branches()[0].Condition.Op != ">" {
		t.Fatalf("manual = %+v, op = %q", m.Trigger, m.Branches()[0].Condition.Op)
	}
}

func TestEvaluate_TruthTable(t *testing.T) {
	none := Result{Columns: []string{"n"}}
	one := Result{Columns: []string{"failed", "label"}, Rows: []map[string]any{{"failed": "51", "label": "x"}}, RowCount: 1}
	cases := []struct {
		name string
		c    Condition
		r    Result
		met  bool
	}{
		{"always, no rows", Condition{Type: "always"}, none, true},
		{"rows_exist, none", Condition{Type: "rows_exist"}, none, false},
		{"rows_exist, one", Condition{Type: "rows_exist"}, one, true},
		{"no_rows, none", Condition{Type: "no_rows"}, none, true},
		{"no_rows, one", Condition{Type: "no_rows"}, one, false},
		{"value > 50 (text numeric)", Condition{Type: "value", Op: ">", Threshold: 50}, one, true},
		{"value > 51", Condition{Type: "value", Op: ">", Threshold: 51}, one, false},
		{"value >= 51", Condition{Type: "value", Op: ">=", Threshold: 51}, one, true},
		{"value < 60 named column", Condition{Type: "value", Column: "FAILED", Op: "<", Threshold: 60}, one, true},
		{"value = 51", Condition{Type: "value", Op: "=", Threshold: 51}, one, true},
		{"value != 51", Condition{Type: "value", Op: "!=", Threshold: 51}, one, false},
		{"value, no rows", Condition{Type: "value", Op: ">", Threshold: 0}, none, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(tc.c, tc.r)
			if err != nil {
				t.Fatal(err)
			}
			if got.Met != tc.met {
				t.Fatalf("met = %v, want %v", got.Met, tc.met)
			}
		})
	}
}

func TestEvaluate_RefusesToGuess(t *testing.T) {
	r := Result{Columns: []string{"label"}, Rows: []map[string]any{{"label": "high"}}, RowCount: 1}
	if _, err := Evaluate(Condition{Type: "value", Op: ">", Threshold: 1}, r); err == nil {
		t.Fatal("a text value compared with a number must fail the run, not guess")
	}
	if _, err := Evaluate(Condition{Type: "value", Column: "missing", Op: ">", Threshold: 1}, r); err == nil {
		t.Fatal("a missing column must fail the run")
	}
	null := Result{Columns: []string{"n"}, Rows: []map[string]any{{"n": nil}}, RowCount: 1}
	got, err := Evaluate(Condition{Type: "value", Op: ">", Threshold: 1}, null)
	if err != nil || got.Met || got.Note == "" {
		t.Fatalf("NULL: met=%v note=%q err=%v", got.Met, got.Note, err)
	}
	for _, v := range []any{int64(7), 7, float32(7), []byte("7"), " 7 "} {
		r := Result{Columns: []string{"n"}, Rows: []map[string]any{{"n": v}}, RowCount: 1}
		got, err := Evaluate(Condition{Type: "value", Op: "=", Threshold: 7}, r)
		if err != nil || !got.Met || got.Value == nil || *got.Value != 7 {
			t.Fatalf("%T: met=%v err=%v", v, got.Met, err)
		}
	}
}

func TestRender_SlackCannotPingOrLink(t *testing.T) {
	r := Result{Columns: []string{"name"}, Rows: []map[string]any{{"name": "<!channel> <https://evil|click>"}}, RowCount: 1}
	out, unknown := Render("Hi <@U123> {{name}} & {{workflow_name}}", Vars{WorkflowName: "a<b", Result: r}, FormatSlack)
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v", unknown)
	}
	if strings.ContainsAny(out, "<>") {
		t.Fatalf("raw markup reached Slack: %q", out)
	}
	if !strings.Contains(out, "&lt;!channel&gt;") || !strings.Contains(out, "&amp;") {
		t.Fatalf("not escaped: %q", out)
	}
}

func TestRender_HTMLEscapesTemplateAndValues(t *testing.T) {
	r := Result{Columns: []string{"x"}, Rows: []map[string]any{{"x": "<script>alert(1)</script>"}}, RowCount: 1}
	out, _ := Render("<b>{{x}}</b>\nline2\n{{table}}", Vars{Result: r}, FormatHTML)
	if strings.Contains(out, "<script>") || strings.Contains(out, "<b>") {
		t.Fatalf("unescaped HTML: %q", out)
	}
	if !strings.Contains(out, "<br>") || !strings.Contains(out, "<table") {
		t.Fatalf("missing line break or table: %q", out)
	}
}

// A message is read by people: a number gets its thousands grouped, in a slot,
// in {{value}} and in each table, whether the warehouse sent it as a number or,
// like BigQuery, as text. Digits that are a label (an id, a code, a phone, a
// year) and text that is not a plain decimal stay as they are.
func TestRender_GroupsThousandsButNotLabels(t *testing.T) {
	cols := []string{"spend", "txns", "neg", "customer_id", "accountId", "fiscal_year", "zip", "card_number", "ref", "ratio"}
	r := Result{Columns: cols, RowCount: 1, Rows: []map[string]any{{
		"spend": "22803", "txns": int64(1000000000), "neg": -1234567.25, "customer_id": "100234",
		"accountId": int64(55501), "fiscal_year": int64(2026), "zip": "02139", "card_number": "4111222233334444",
		"ref": "00012345", "ratio": "1e6",
	}}}
	v := 1234.5
	tmpl := "{{spend}} {{txns}} {{neg}} {{customer_id}} {{accountId}} {{fiscal_year}} {{zip}} {{card_number}} {{ref}} {{ratio}} {{value}}"
	out, _ := Render(tmpl, Vars{Result: r, Value: &v}, FormatText)
	want := "22,803 1,000,000,000 -1,234,567.25 100234 55501 2026 02139 4111222233334444 00012345 1e6 1,234.5"
	if out != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
	for _, f := range []Format{FormatText, FormatSlack, FormatHTML} {
		table, _ := Render("{{table}}", Vars{Result: r}, f)
		if !strings.Contains(table, "1,000,000,000") || !strings.Contains(table, "100234") {
			t.Errorf("format %d table: %q", f, table)
		}
	}
	first := Result{Columns: []string{"total"}, Rows: []map[string]any{{"total": "9876"}}, RowCount: 1}
	if out, _ := Render("{{value}}", Vars{Result: first}, FormatText); out != "9,876" {
		t.Fatalf("{{value}} from the first column = %q", out)
	}
}

func TestRender_FillsFixedAndColumnVarsAndReportsUnknown(t *testing.T) {
	v := 42.5
	r := Result{Columns: []string{"Revenue", "orders"}, Rows: []map[string]any{{"Revenue": 1200.5, "orders": int64(7)}}, RowCount: 1}
	vars := Vars{WorkflowName: "Weekly", RunTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), Link: "https://x/w/1", Result: r, Value: &v}
	out, unknown := Render("{{ workflow_name }}: {{revenue}} from {{orders}} ({{row_count}}) {{value}} {{run_time}} {{link}} {{nope}}", vars, FormatText)
	want := "Weekly: 1,200.5 from 7 (1) 42.5 2026-10-02 09:00 UTC https://x/w/1 "
	if out != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
	if len(unknown) != 1 || unknown[0] != "{{nope}}" {
		t.Fatalf("unknown = %v", unknown)
	}
	subj, _ := RenderSubject("Report {{table}}\n{{workflow_name}}", vars)
	if subj != "Report Weekly" {
		t.Fatalf("subject = %q", subj)
	}
}

func TestRender_SlackTableStaysInsideItsBudget(t *testing.T) {
	r := Result{Columns: []string{"id", "note"}, RowCount: 500, Truncated: true}
	for i := 0; i < 500; i++ {
		r.Rows = append(r.Rows, map[string]any{"id": i, "note": strings.Repeat("y", 100)})
	}
	out, _ := Render("{{table}}", Vars{Result: r}, FormatSlack)
	if len(out) > SlackMessageMax {
		t.Fatalf("table is %d chars, over Slack's %d", len(out), SlackMessageMax)
	}
	if !strings.HasPrefix(out, "```\n") || !strings.Contains(out, "\n```") || !strings.Contains(out, "not shown") {
		t.Fatalf("table shape: %q", out[:80])
	}
	empty, _ := Render("{{table}}", Vars{Result: Result{Columns: []string{"a"}}}, FormatText)
	if empty != "(no rows)" {
		t.Fatalf("empty table = %q", empty)
	}
}

func TestNextRun_HonoursTheTimezone(t *testing.T) {
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC) // a Friday
	next, err := NextRun(Trigger{Type: "schedule", Cron: "0 9 * * *", Timezone: "Asia/Kolkata"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 IST is 03:30 UTC, so the next one is tomorrow's.
	if want := time.Date(2026, 10, 3, 3, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	if _, err := NextRun(Trigger{Type: "manual"}, now); err == nil {
		t.Fatal("a manual trigger has no next run")
	}
}

func TestParseThreshold(t *testing.T) {
	for in, want := range map[string]float64{"50": 50, " 0.05 ": 0.05, "1,000": 1000, "20%": 20, "-3": -3} {
		got, err := ParseThreshold(in)
		if err != nil || got != want {
			t.Fatalf("%q -> %v, %v", in, got, err)
		}
	}
	if _, err := ParseThreshold("lots"); err == nil {
		t.Fatal("text is not a threshold")
	}
}

// Two drafts Ask rsync.ai wrote on prod (2026-10-02) put a number in the
// message that was not the number the person asked for: "Total users:
// {{row_count}}" under a one-row COUNT (it said 1), and "the total is
// {{row_count}}" under LIMIT 10 (it said 10). Both slots describe the query's
// result, not the data, so a draft that uses them as a data total is refused
// with the fix: name the column in the SQL.
func TestNormalize_RefusesSlotsThatMisstateTheData(t *testing.T) {
	refused := []struct {
		name string
		edit func(*v2)
	}{
		{"prod draft 1: {{value}} for a total, condition always", func(d *v2) {
			d.Query.SQL = "SELECT COUNT(*) AS total_users, SUM(CASE WHEN is_premium THEN 1 ELSE 0 END) AS total_premium_users FROM public.users;"
			d.Branches[0].Message.Body = "Daily users digest: Total users: {{row_count}}, Total premium users: {{value}}"
		}},
		{"prod draft 2: {{row_count}} as a total under LIMIT", func(d *v2) {
			d.Query.SQL = "SELECT id, gender, age, created_at FROM public.users WHERE is_premium = true ORDER BY created_at DESC LIMIT 10;"
			d.Branches[0].Message.Body = "The total number of premium users in the workspace is {{row_count}}. Here are the 10 newest premium users:"
		}},
		{"{{row_count}} in the subject under limit", func(d *v2) {
			d.Query.SQL = "select * from orders order by id desc limit 5"
			d.Branches[0].Message.Subject = "{{row_count}} orders"
			d.Branches[0].Message.Body = "{{table}}"
		}},
		{"{{value}} with rows_exist", func(d *v2) {
			d.Branches[0].Condition = Condition{Type: "rows_exist"}
			d.Branches[0].Message.Body = "Value: {{value}}"
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			l := validDef()
			tc.edit(&l)
			d := l.build()
			err := Normalize(&d)
			var inv *Invalid
			if !errors.As(err, &inv) {
				t.Fatalf("want a refusal, got %v", err)
			}
			if !strings.Contains(inv.Message, "AS ") {
				t.Fatalf("the refusal should show how to name the column: %q", inv.Message)
			}
		})
	}

	allowed := []struct {
		name string
		edit func(*v2)
	}{
		{"exception list: {{row_count}} without a LIMIT", func(d *v2) {
			d.Query.SQL = "SELECT id FROM orders WHERE status = 'pending' AND created_at < now() - interval '1 day'"
			d.Branches[0].Condition = Condition{Type: "rows_exist"}
			d.Branches[0].Message.Body = "{{row_count}} orders are stuck:\n{{table}}"
		}},
		{"{{value}} with a value condition", func(d *v2) {
			d.Branches[0].Condition = Condition{Type: "value", Op: ">", Threshold: 50}
			d.Branches[0].Message.Body = "{{value}} payments failed"
		}},
		{"a column named for the total", func(d *v2) {
			d.Query.SQL = "SELECT id, count(*) OVER () AS total_premium FROM users WHERE is_premium ORDER BY created_at DESC LIMIT 10"
			d.Branches[0].Message.Body = "{{total_premium}} premium users. Newest:\n{{table}}"
		}},
		{"LIMIT inside a subquery only", func(d *v2) {
			d.Query.SQL = "SELECT * FROM orders WHERE id IN (SELECT id FROM flagged LIMIT 100)"
			d.Branches[0].Message.Body = "{{row_count}} flagged orders"
		}},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			l := validDef()
			tc.edit(&l)
			d := l.build()
			if err := Normalize(&d); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// A trigger reads as a sentence, never as raw cron in backticks.
func TestDescribeTrigger_ReadsLikeASentence(t *testing.T) {
	cases := map[string]string{
		"0 9 * * *":   "every day at 09:00 (UTC)",
		"30 8 * * 1":  "every Monday at 08:30 (UTC)",
		"0 9 * * 1-5": "every weekday at 09:00 (UTC)",
		// The hourly shapes read as the editor's cron note does (cronSentence.ts).
		"0 * * * *":    "every hour on the hour (UTC)",
		"1 * * * *":    "every hour at 1 minute past (UTC)",
		"15 * * * *":   "every hour at 15 minutes past (UTC)",
		"*/5 * * * *":  "every 5 minutes (UTC)",
		"0 */6 * * *":  "every 6 hours from 00:00 (UTC)",
		"30 */5 * * *": "every 5 hours from 00:30 to 20:30 (UTC)",
		"0 */12 * * *": "every day at 00:00 and 12:00 (UTC)",
		"0 */1 * * *":  "every hour on the hour (UTC)",
		"0 7 1 * *":    "on day 1 of every month at 07:00 (UTC)",
		"0 9 * * 0,6":  "on cron 0 9 * * 0,6 (UTC)",
		"5 4 * 1 *":    "on cron 5 4 * 1 * (UTC)",
	}
	for cron, want := range cases {
		got := DescribeTrigger(Trigger{Type: TriggerSchedule, Cron: cron, Timezone: "UTC"}, "")
		if got != want {
			t.Errorf("%q: got %q, want %q", cron, got, want)
		}
		if strings.Contains(got, "`") {
			t.Errorf("%q: backticks in %q", cron, got)
		}
	}
}

// A draft's one run is checked against its definition: a slot is a fixed
// variable or a column (any case), a value condition's column exists and is a
// number. The refusal names columns, never a value.
func TestCheckResult(t *testing.T) {
	res := Result{Columns: []string{"Revenue", "orders"}, Rows: []map[string]any{{"Revenue": "12.5", "orders": "secret-ish"}}, RowCount: 1}
	d := validDef()
	d.Branches[0].Message = Message{Subject: "{{workflow_name}} {{run_time}}", Body: "{{revenue}} from {{orders}} {{table}} {{row_count}} {{link}} {{value}}"}
	if inv := CheckResult(d.build(), res); inv != nil {
		t.Fatalf("a good draft refused: %v", inv)
	}

	d.Branches[0].Message.Body = "{{revenue}} {{total}} {{total}} {{margin}}"
	inv := CheckResult(d.build(), res)
	if inv == nil || inv.Field != "message" || !strings.Contains(inv.Message, "{{total}}, {{margin}};") ||
		!strings.Contains(inv.Message, "Revenue, orders") {
		t.Fatalf("unknown slots: %+v", inv)
	}

	d = validDef()
	d.Branches[0].Message.Body = "{{revenue}}"
	d.Branches[0].Condition = Condition{Type: ConditionValue, Column: "orders", Op: ">", Threshold: 1}
	if inv := CheckResult(d.build(), res); inv == nil || inv.Field != "condition_column" || strings.Contains(inv.Message, "secret") {
		t.Fatalf("a text column compared: %+v", inv)
	}
	d.Branches[0].Condition.Column = "revenue"
	if inv := CheckResult(d.build(), res); inv != nil {
		t.Fatalf("a numeric-text column refused: %v", inv)
	}
	d.Branches[0].Condition.Column = "missing"
	if inv := CheckResult(d.build(), Result{Columns: []string{"revenue"}}); inv == nil || !strings.Contains(inv.Message, `"missing"`) {
		t.Fatalf("a missing column with no rows: %+v", inv)
	}
}

// A version 1 definition (stored workflows, and the copies runs keep) reads
// as a query, a condition and one send, and is written back as version 3.
func TestDefinition_ReadsVersion1(t *testing.T) {
	v1 := `{"version":1,"trigger":{"type":"manual"},"query":{"connection_id":"c","sql":"SELECT 1"},` +
		`"condition":{"type":"value","column":"n","op":">","threshold":5},"message":{"subject":"S","body":"B"},"quiet_minutes":30}`
	var d Definition
	if err := json.Unmarshal([]byte(v1), &d); err != nil {
		t.Fatal(err)
	}
	want := Branch{Condition: Condition{Type: "value", Column: "n", Op: ">", Threshold: 5}, Message: Message{Subject: "S", Body: "B"}}
	if len(d.Branches()) != 1 || d.Branches()[0] != want || d.QuietMinutes != 30 || d.Query().SQL != "SELECT 1" ||
		!d.IsLegacyShape() {
		t.Fatalf("read as %+v", d)
	}
	if err := Normalize(&d); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(d)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(out, &top)
	if _, ok := top["condition"]; ok || top["message"] != nil || top["query"] != nil || top["branches"] != nil ||
		string(top["version"]) != "3" || top["steps"] == nil {
		t.Fatalf("written as %s", out)
	}
	var again Definition
	if err := json.Unmarshal(out, &again); err != nil || len(again.Branches()) != 1 || again.Branches()[0] != want {
		t.Fatalf("round trip: %+v %v", again, err)
	}
}

func branch(cond Condition, body string) Branch {
	return Branch{Condition: cond, Message: Message{Body: body}}
}

// Any number of conditions up to a hidden ceiling, an otherwise included and
// never required: an always one anywhere but last would hide every one below
// it. The person reads a branch as a condition, so every refusal says so.
func TestNormalize_Branches(t *testing.T) {
	gt := func(n float64) Condition { return Condition{Type: "value", Column: "failed", Op: ">", Threshold: n} }
	ok := validDef()
	ok.Branches = []Branch{branch(gt(100), "critical"), branch(gt(10), "warning"), branch(gt(0), "notice"),
		branch(Condition{}, "all quiet")}
	okd := ok.build()
	if err := Normalize(&okd); err != nil {
		t.Fatalf("three conditions and an otherwise refused: %v", err)
	}
	if okd.Branches()[3].Condition.Type != ConditionAlways {
		t.Fatalf("an empty condition is always: %+v", okd.Branches()[3])
	}
	if got := DescribeBranches(okd.Branches()); got != "send when failed is above 100; else send when failed is above 10; "+
		"else send when failed is above 0; otherwise send" {
		t.Errorf("described as %q", got)
	}
	if got := DescribeBranches(validDef().Branches); got != "always send" {
		t.Fatalf("one branch described as %q", got)
	}

	six := validDef()
	six.Branches = nil
	for n := 6; n > 0; n-- {
		six.Branches = append(six.Branches, branch(gt(float64(n*1000)), "level"))
	}
	sixd := six.build()
	if err := Normalize(&sixd); err != nil || len(sixd.Branches()) != 6 {
		t.Errorf("six conditions and no otherwise refused: %v (%d)", err, len(sixd.Branches()))
	}
	ceiling := validDef()
	ceiling.Branches = nil
	for n := 0; n < MaxBranches; n++ {
		ceiling.Branches = append(ceiling.Branches, branch(gt(float64(n)), "level"))
	}
	ceild := ceiling.build()
	if err := Normalize(&ceild); err != nil {
		t.Errorf("%d conditions refused: %v", MaxBranches, err)
	}
	if MaxBranches != 100 {
		t.Errorf("the ceiling is %d; the refusal below and the docs say 100", MaxBranches)
	}
	over := make([]Branch, 101)
	for i := range over {
		over[i] = branch(gt(float64(i)), "level")
	}

	cases := []struct {
		name     string
		branches []Branch
		field    string
		branch   int
		says     string
	}{
		{"none", nil, "message", 0, "needs a message"},
		{"always first", []Branch{branch(Condition{Type: "always"}, "a"), branch(gt(1), "b")}, "condition", 1,
			"Condition 1: Only the last condition may be always"},
		{"101", over, "branches", 0, "A workflow has at most 100 conditions; this one has 101."},
		{"{{value}} in the otherwise", []Branch{branch(gt(1), "{{value}} failed"), branch(Condition{}, "only {{value}}")},
			"message", 2, "Condition 2: {{value}}"},
		{"a bad op in condition 2", []Branch{branch(gt(1), "a"), branch(Condition{Type: "value", Op: "about"}, "b")},
			"condition_op", 2, "Condition 2:"},
		{"an empty message in condition 2", []Branch{branch(gt(1), "a"), branch(gt(0), " ")}, "message", 2, "Condition 2:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := validDef()
			l.Branches = tc.branches
			d := l.build()
			var inv *Invalid
			if !errors.As(Normalize(&d), &inv) {
				t.Fatal("not refused")
			}
			if inv.Field != tc.field || inv.Branch != tc.branch || !strings.Contains(inv.Message, tc.says) {
				t.Fatalf("got %+v", inv)
			}
			if strings.Contains(strings.ToLower(inv.Message), "branch") {
				t.Fatalf("a refusal the person reads says branch: %q", inv.Message)
			}
		})
	}
}

// A condition reads as words: the operator spelled out, the number with
// thousands separators, the way the editor's describeCondition writes it.
func TestDescribeCondition_InWords(t *testing.T) {
	v := func(col, op string, n float64) Condition {
		return Condition{Type: "value", Column: col, Op: op, Threshold: n}
	}
	for _, tc := range []struct {
		c    Condition
		want string
	}{
		{v("count", ">", 5000), "send when count is above 5,000"},
		{v("count", ">=", 1234567.5), "send when count is at least 1,234,567.5"},
		{v("count", "<", -12000), "send when count is below -12,000"},
		{v("count", "<=", 999), "send when count is at most 999"},
		{v("count", "=", 0), "send when count is 0"},
		{v("count", "!=", 0.25), "send when count is not 0.25"},
		{v("", ">", 1), "send when the first column is above 1"},
		{Condition{Type: "rows_exist"}, "send when the query returns rows"},
		{Condition{Type: "no_rows"}, "send when the query returns no rows"},
		{Condition{Type: "always"}, "always send"},
	} {
		if got := DescribeCondition(tc.c); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.c, got, tc.want)
		}
	}
}

// The first branch that holds sends; the ones below it are not evaluated, so
// one that could not be decided does not fail a run that already matched.
// One above it that cannot be decided fails the run: a later branch never
// sends in its place.
func TestDecide(t *testing.T) {
	res := Result{Columns: []string{"failed", "label"}, Rows: []map[string]any{{"failed": int64(42), "label": "x"}}, RowCount: 1}
	gt := func(col string, n float64) Condition {
		return Condition{Type: "value", Column: col, Op: ">", Threshold: n}
	}
	bs := []Branch{branch(gt("failed", 100), "a"), branch(gt("failed", 10), "b"), branch(gt("label", 0), "c"), branch(Condition{Type: "always"}, "d")}

	got, err := Decide(bs, res)
	if err != nil || got.Branch != 1 || got.Outcome.Value == nil || *got.Outcome.Value != 42 || len(got.Notes) != 0 {
		t.Fatalf("first match: %+v %v", got, err)
	}

	got, err = Decide([]Branch{branch(gt("failed", 100), "a"), branch(gt("failed", 50), "b")}, res)
	if err != nil || got.Branch != -1 || len(got.Notes) != 1 || got.Notes[0] != "No condition was true, so nothing was sent." {
		t.Fatalf("none held: %+v %v", got, err)
	}
	got, err = Decide([]Branch{branch(gt("failed", 100), "a"), branch(Condition{Type: "always"}, "b")}, res)
	if err != nil || got.Branch != 1 || got.Outcome.Value != nil {
		t.Fatalf("the otherwise: %+v %v", got, err)
	}

	if _, err := Decide([]Branch{branch(gt("label", 0), "a"), branch(Condition{Type: "always"}, "b")}, res); err == nil ||
		!strings.HasPrefix(err.Error(), "Condition 1: ") {
		t.Fatalf("an undecidable branch above the match must fail the run: %v", err)
	}

	empty := Result{Columns: []string{"failed"}}
	got, err = Decide([]Branch{branch(gt("failed", 1), "a"), branch(Condition{Type: "no_rows"}, "b")}, empty)
	if err != nil || got.Branch != 1 || len(got.Notes) != 1 || !strings.HasPrefix(got.Notes[0], "Condition 1: The query returned no rows") {
		t.Fatalf("a branch with no value to compare: %+v %v", got, err)
	}

	got, err = Decide(validDef().Branches[:1], empty)
	if err != nil || got.Branch != 0 {
		t.Fatalf("one always branch: %+v %v", got, err)
	}
	got, _ = Decide([]Branch{branch(Condition{Type: "rows_exist"}, "a")}, empty)
	if got.Branch != -1 || len(got.Notes) != 1 || got.Notes[0] != "The condition was not true, so nothing was sent." {
		t.Fatalf("one branch keeps its note: %+v", got)
	}
}

// The check names the branch whose slot or column the result does not have.
func TestCheckResult_NamesTheBranch(t *testing.T) {
	res := Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": 3}}, RowCount: 1}
	d := validDef()
	d.Branches = []Branch{
		branch(Condition{Type: "value", Column: "failed", Op: ">", Threshold: 1}, "{{failed}} failed"),
		branch(Condition{Type: "always"}, "{{passed}} passed"),
	}
	inv := CheckResult(d.build(), res)
	if inv == nil || inv.Branch != 2 || inv.Step != "send_2" || inv.Field != "message" ||
		!strings.HasPrefix(inv.Message, "Condition 2: ") {
		t.Fatalf("got %+v", inv)
	}
}
