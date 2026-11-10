package agentgw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// checksPlan is a plan as handlers.PlanModelChecks answers, for public.orders
// (id, customer_id in leakyBodies' schema index).
func checksPlan(checks ...map[string]any) map[string]any {
	list := make([]any, len(checks))
	for i, c := range checks {
		list[i] = c
	}
	return map[string]any{"connection_id": connID, "target_schema": "public", "target_table": "orders",
		"columns_case_sensitive": false, "can_block": true, "existing": 3, "max_checks": 20, "checks": list}
}

func plannedCheck(name, typ, severity string, columns ...string) map[string]any {
	cols := []any{}
	for _, c := range columns {
		cols = append(cols, c)
	}
	return map[string]any{"name": name, "type": typ, "columns": cols, "severity": severity}
}

// The draft reaches the handler as the Checks tab sends a check: synonyms
// resolved, columns split, the string settings as numbers ("50%" too), the SQL
// verbatim and the severity left to the handler when not given.
func TestPlanModelChecks_SendsTheDraftAsTheHandlerTakesIt(t *testing.T) {
	rec := &recorder{}
	h := leakyHandlers()
	h.PlanModelChecks = rec.handler(200, checksPlan(
		plannedCheck("ids", "unique", "block", "id", "customer_id"),
		plannedCheck("volume", "row_count_change", "warn"),
		plannedCheck("size", "row_count", "block"),
		plannedCheck("no negatives", "custom_sql", "block")))
	r, _ := testServer(t, h, readerActor)
	sql := "SELECT *\nFROM {{this}}\nWHERE total < 0"
	d := data(t, call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
		map[string]any{"name": "ids", "type": "distinct", "columns": " id , customer_id,", "severity": "error"},
		map[string]any{"name": "volume", "type": "volume", "max_drop_pct": "50%", "max_growth_pct": "200.5"},
		map[string]any{"name": "size", "type": "rows", "min": "1", "max": "1000000"},
		map[string]any{"name": "no negatives", "type": "sql", "sql": sql},
	}}))
	if len(rec.calls) != 1 || rec.calls[0].method != http.MethodPost || rec.calls[0].id != modelID {
		t.Fatalf("handler calls = %+v", rec.calls)
	}
	sent, _ := json.Marshal(rec.calls[0].body)
	want := `{"checks":[` +
		`{"columns":["id","customer_id"],"name":"ids","params":{},"severity":"block","sql_text":"","type":"unique"},` +
		`{"columns":[],"name":"volume","params":{"max_drop_pct":50,"max_growth_pct":200.5},"severity":"","sql_text":"","type":"row_count_change"},` +
		`{"columns":[],"name":"size","params":{"max":1000000,"min":1},"severity":"","sql_text":"","type":"row_count"},` +
		`{"columns":[],"name":"no negatives","params":{},"severity":"","sql_text":"SELECT *\nFROM {{this}}\nWHERE total ` + "\\u003c" + ` 0","type":"custom_sql"}]}`
	if string(sent) != want {
		t.Fatalf("sent  %s\nwant %s", sent, want)
	}
	checks := d["checks"].([]any)
	if len(checks) != 4 || d["target_table"] != "public.orders" || d["columns_unchecked"] != nil {
		t.Fatalf("draft = %v", d)
	}
	if c0 := checks[0].(map[string]any); c0["verdict"] != "valid" || c0["type"] != "unique" || c0["severity"] != "block" {
		t.Fatalf("checks[0] = %v", c0)
	}
}

// A setting that is not a number is refused before rsync.ai is asked, naming
// the check and the field. Control: the same draft with a number reaches it.
func TestPlanModelChecks_RefusesASettingThatIsNotANumber(t *testing.T) {
	for _, tc := range []struct{ field, value, want string }{
		{"min", "1.5", "checks[1].min must be a whole number"},
		{"max", "lots", "checks[1].max must be a whole number"},
		{"max_drop_pct", "half", "checks[1].max_drop_pct must be a number"},
		{"max_growth_pct", "NaN", "checks[1].max_growth_pct must be a number"},
	} {
		rec := &recorder{}
		h := leakyHandlers()
		h.PlanModelChecks = rec.handler(200, checksPlan(plannedCheck("a", "row_count", "block"),
			plannedCheck("b", "row_count", "block")))
		r, audit := testServer(t, h, readerActor)
		args := map[string]any{"model_id": modelID, "checks": []any{
			map[string]any{"name": "a", "type": "row_count"},
			map[string]any{"name": "b", "type": "row_count", tc.field: tc.value}}}
		res := call(t, r, "plan_model_checks", args)
		if !isError(res) || !strings.Contains(resultText(res), tc.want) || len(rec.calls) != 0 {
			t.Fatalf("%s=%q: got %v, handler calls %d", tc.field, tc.value, res, len(rec.calls))
		}
		if (*audit)[0].outcome != "invalid_arguments" {
			t.Fatalf("%s: audit = %+v", tc.field, *audit)
		}
		args["checks"].([]any)[1].(map[string]any)[tc.field] = "7"
		if res := call(t, r, "plan_model_checks", args); isError(res) || len(rec.calls) != 1 {
			t.Fatalf("control %s: %v", tc.field, res)
		}
	}
}

// One refused check refuses the draft: the answer gives each check's verdict,
// with the handler's reason scrubbed, and how many checks the model has.
func TestPlanModelChecks_ARefusedCheckRefusesTheDraft(t *testing.T) {
	refusedCheck := plannedCheck("dup", "row_count", "block")
	refusedCheck["refusal"] = "the model already has a check named dup, asked by jane.doe@example.com from 10.1.2.3"
	h := leakyHandlers()
	h.PlanModelChecks = jsonHandler(200, checksPlan(plannedCheck("size", "row_count", "block"), refusedCheck))
	r, audit := testServer(t, h, readerActor)
	res := call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
		map[string]any{"name": "size", "type": "row_count"}, map[string]any{"name": "dup", "type": "row_count"}}})
	text := resultText(res)
	for _, want := range []string{"refused 1 of the 2 checks", "nothing was saved", "1. size: valid.",
		"2. dup: refused: the model already has a check named dup", "already has 3 checks; a model has at most 20."} {
		if !isError(res) || !strings.Contains(text, want) {
			t.Fatalf("want %q in %q", want, text)
		}
	}
	if strings.Contains(text, "jane.doe@example.com") || strings.Contains(text, "10.1.2.3") {
		t.Fatalf("the refusal was not scrubbed: %q", text)
	}
	if (*audit)[0].outcome != "invalid_arguments" {
		t.Fatalf("audit = %+v", *audit)
	}
}

// A check on a column the model's table does not have is refused, naming the
// table's columns; a case-insensitive engine matches any case, a sensitive one
// only the exact name. Control: a column the table has passes.
func TestPlanModelChecks_RefusesAColumnTheTableDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		column    string
		sensitive bool
		refused   bool
	}{
		{"customer_id", false, false}, {"CUSTOMER_ID", false, false},
		{"CUSTOMER_ID", true, true}, {"order_id", false, true},
	} {
		plan := checksPlan(plannedCheck("size", "row_count", "block"), plannedCheck("required", "not_null", "block", tc.column))
		plan["columns_case_sensitive"] = tc.sensitive
		h := leakyHandlers()
		h.PlanModelChecks = jsonHandler(200, plan)
		r, _ := testServer(t, h, readerActor)
		res := call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
			map[string]any{"name": "size", "type": "row_count"},
			map[string]any{"name": "required", "type": "not_null", "columns": tc.column}}})
		if isError(res) != tc.refused {
			t.Fatalf("%s (case-sensitive %v): refused = %v, want %v: %s", tc.column, tc.sensitive, isError(res), tc.refused, resultText(res))
		}
		want := "2. required: refused: the model's table has no column " + tc.column + "; its columns are id, customer_id."
		if tc.refused && !strings.Contains(resultText(res), want) {
			t.Fatalf("%s: want %q in %q", tc.column, want, resultText(res))
		}
	}
}

// When the model's table cannot be found in the schema index, or the index
// cannot be read, the draft passes with columns_unchecked and a note saying
// what an unchecked column costs. A draft naming no column never reads it.
func TestPlanModelChecks_ColumnsUncheckedSaysWhy(t *testing.T) {
	twoOrders := map[string]any{"tables": []any{
		map[string]any{"schema": "public", "name": "orders", "columns": []any{map[string]any{"name": "id"}}},
		map[string]any{"schema": "sales", "name": "orders", "columns": []any{map[string]any{"name": "id"}}}}}
	for _, tc := range []struct {
		name   string
		table  string
		schema string
		index  gin.HandlerFunc
		want   string
	}{
		{"not built yet", "daily_orders", "public", nil, "not in the connection's schema yet"},
		{"index fails", "orders", "public", jsonHandler(500, map[string]any{"error": "boom"}), "could not read the connection's schema"},
		{"ambiguous", "orders", "", jsonHandler(200, twoOrders), "several schemas have a table named orders"},
	} {
		plan := checksPlan(plannedCheck("required", "not_null", "block", "nope"))
		plan["target_table"], plan["target_schema"] = tc.table, tc.schema
		h := leakyHandlers()
		h.PlanModelChecks = jsonHandler(200, plan)
		if tc.index != nil {
			h.GetSchemaIndex = tc.index
		}
		r, _ := testServer(t, h, readerActor)
		d := data(t, call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
			map[string]any{"name": "required", "type": "not_null", "columns": "nope"}}}))
		note, _ := d["note"].(string)
		if d["columns_unchecked"] != true || !strings.Contains(note, tc.want) || !strings.Contains(note, "a blocking one holds the table back") {
			t.Fatalf("%s: draft = %v", tc.name, d)
		}
	}

	read := 0
	h := leakyHandlers()
	h.PlanModelChecks = jsonHandler(200, checksPlan(plannedCheck("size", "row_count", "block")))
	h.GetSchemaIndex = func(c *gin.Context) { read++; c.JSON(200, map[string]any{"tables": []any{}}) }
	r, _ := testServer(t, h, readerActor)
	d := data(t, call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
		map[string]any{"name": "size", "type": "row_count"}}}))
	if read != 0 || d["columns_unchecked"] != nil {
		t.Fatalf("a draft naming no column read the index %d times: %v", read, d)
	}
}

// A refusal of the whole request passes through with its reason; a plan that
// does not answer every check is an error, never a draft missing a check.
func TestPlanModelChecks_HandlerRefusalAndShortPlan(t *testing.T) {
	h := leakyHandlers()
	h.PlanModelChecks = jsonHandler(400, map[string]any{"error": "this model cannot have checks: a statement model"})
	r, _ := testServer(t, h, readerActor)
	args := map[string]any{"model_id": modelID, "checks": []any{
		map[string]any{"name": "size", "type": "row_count"}, map[string]any{"name": "more", "type": "row_count"}}}
	res := call(t, r, "plan_model_checks", args)
	if !isError(res) || !strings.Contains(resultText(res), "cannot have checks: a statement model") {
		t.Fatalf("handler refusal: %v", res)
	}
	h.PlanModelChecks = jsonHandler(200, checksPlan(plannedCheck("size", "row_count", "block")))
	r, _ = testServer(t, h, readerActor)
	if res := call(t, r, "plan_model_checks", args); !isError(res) || !strings.Contains(resultText(res), "could not read") {
		t.Fatalf("short plan: %v", res)
	}
}

// Next tells the assistant about the card and an agent about the Checks tab,
// and mentions the approval only for a custom_sql check.
func TestPlanModelChecks_NextSaysWhereThePersonAddsThem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		assistant bool
		custom    bool
		want      string
	}{
		{"assistant", true, false, "card under your answer"},
		{"agent", false, false, "Checks tab in rsync.ai"},
		{"custom_sql", false, true, "waits for an admin's approval"},
	} {
		typ := "row_count"
		if tc.custom {
			typ = "custom_sql"
		}
		actor := readerActor
		if tc.assistant {
			actor = assistantActor
		}
		h := leakyHandlers()
		h.PlanModelChecks = jsonHandler(200, checksPlan(plannedCheck("c", typ, "block")))
		r, _ := testServer(t, h, actor)
		next, _ := data(t, call(t, r, "plan_model_checks", map[string]any{"model_id": modelID, "checks": []any{
			map[string]any{"name": "c", "type": typ, "sql": "SELECT * FROM {{this}} WHERE 1 = 0"}}}))["next"].(string)
		if !strings.Contains(next, tc.want) || strings.Contains(next, "approval") != tc.custom {
			t.Fatalf("%s: next = %q", tc.name, next)
		}
	}
}

// list_model_checks passes on what each check is; a model with none gets a
// note pointing to plan_model_checks, unless it cannot have checks at all.
func TestListModelChecks_SaysWhatEachCheckIs(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	d := data(t, call(t, r, "list_model_checks", map[string]any{"model_id": modelID}))
	c0 := d["checks"].([]any)[0].(map[string]any)
	if c0["check_id"] != genID || c0["name"] != "orders id is unique" || c0["type"] != "unique" ||
		c0["severity"] != "block" || c0["enabled"] != true || d["can_block"] != true || d["note"] != nil {
		t.Fatalf("list = %v", d)
	}
	for _, tc := range []struct {
		body map[string]any
		note bool
	}{
		{map[string]any{"checks": []any{}, "max_checks": 20, "can_block": true}, true},
		{map[string]any{"checks": []any{}, "max_checks": 20, "unsupported_reason": "a statement model"}, false},
	} {
		h := leakyHandlers()
		h.ListModelChecks = jsonHandler(200, tc.body)
		r, _ := testServer(t, h, readerActor)
		d := data(t, call(t, r, "list_model_checks", map[string]any{"model_id": modelID}))
		if note, _ := d["note"].(string); strings.Contains(note, "plan_model_checks") != tc.note {
			t.Fatalf("%v: note = %q", tc.body, note)
		}
	}
}
