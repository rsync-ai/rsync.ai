package agentgw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// writeModelTools are the model tools that change something; each needs the
// models:write scope and an admin.
var writeModelTools = map[string]bool{
	"create_model": true, "create_silver_model": true, "schedule_model": true,
	"run_model": true, "pause_model_schedule": true, "resume_model_schedule": true,
}

// recorder is a fake handler that keeps each request it was sent: method, the
// :id param and the JSON body.
type recorder struct {
	calls []recorded
}

type recorded struct {
	method, id string
	body       map[string]any
}

func (r *recorder) handler(status int, resp any) gin.HandlerFunc {
	return func(c *gin.Context) {
		rec := recorded{method: c.Request.Method, id: c.Param("id")}
		if raw, _ := c.GetRawData(); len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		r.calls = append(r.calls, rec)
		c.JSON(status, resp)
	}
}

// A metadata:read token, a viewer, and an admin token without models:write are
// each refused every model write, before any handler runs; the refusal names
// the scope and the role. Control: the admin token with the scope succeeds.
func TestModelWrites_NeedScopeAndAdmin(t *testing.T) {
	var reached []string
	h := leakyHandlers()
	hit := func(name string, next gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) { reached = append(reached, name); next(c) }
	}
	h.CreateSavedQuery = hit("create", h.CreateSavedQuery)
	h.GetSilverSQL = hit("silver", h.GetSilverSQL)
	h.CreateSavedQuerySchedule = hit("schedule", h.CreateSavedQuerySchedule)
	h.StartSavedQueryModelRun = hit("run", h.StartSavedQueryModelRun)
	h.PauseSavedQuerySchedule = hit("pause", h.PauseSavedQuerySchedule)
	h.ResumeSavedQuerySchedule = hit("resume", h.ResumeSavedQuerySchedule)

	noScope, member := adminActor, adminActor
	noScope.Scopes = []string{services.ScopeMetadataRead, services.ScopeConnectorsGenerate}
	member.Role = security.WSMember
	for who, actor := range map[string]services.Actor{"metadata:read only": noScope, "member": member, "viewer": readerActor} {
		r, audit := testServer(t, h, actor)
		for tool := range writeModelTools {
			*audit = nil
			res := call(t, r, tool, validArgs[tool])
			if !isError(res) || !strings.Contains(resultText(res), "models:write") || !strings.Contains(resultText(res), "admin") {
				t.Fatalf("%s %s: want a refusal naming the scope and role, got %v", who, tool, res)
			}
			if len(*audit) != 1 || (*audit)[0].outcome != "forbidden" || (*audit)[0].resourceType != "model" {
				t.Fatalf("%s %s: audit = %+v", who, tool, *audit)
			}
		}
	}
	if len(reached) != 0 {
		t.Fatalf("a refused call reached a handler: %v", reached)
	}
	r, _ := testServer(t, h, adminActor)
	for tool := range writeModelTools {
		if res := call(t, r, tool, validArgs[tool]); isError(res) {
			t.Fatalf("control: admin %s refused: %s", tool, resultText(res))
		}
	}
	if len(reached) < len(writeModelTools) {
		t.Fatalf("control: admin calls reached %v", reached)
	}
}

// The reads are open to a metadata:read viewer, and the in-app assistant
// carries models:write for everyone, so a viewer there is refused by role.
func TestModelReads_OpenToAViewer(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	for _, tool := range []string{"list_models", "get_model", "list_model_runs"} {
		if res := call(t, r, tool, validArgs[tool]); isError(res) {
			t.Fatalf("%s refused a viewer: %s", tool, resultText(res))
		}
	}
	s, c, _ := inProcess(t, assistantActor)
	text, isErr := s.Call(c, "run_model", json.RawMessage(`{"model_id":"`+modelID+`"}`))
	if !isErr || !strings.Contains(text, "only a workspace admin or owner") {
		t.Fatalf("assistant viewer run_model = (%q, %v), want the admin refusal", text, isErr)
	}
}

// What the model reads return: the model, never its query; a plain saved
// query is not listed; a run's row count and runner never come back, and its
// error has its literal, host, address and numbers masked but keeps its words.
func TestModelReads_Shape(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	list := data(t, call(t, r, "list_models", map[string]any{}))
	models := list["models"].([]any)
	if len(models) != 1 || list["total"] != float64(1) {
		t.Fatalf("list_models = %v, want only the model, not the plain saved query", list)
	}
	m := models[0].(map[string]any)
	if m["model_id"] != modelID || m["layer"] != "Gold" || m["target_table"] != "gold.daily_orders" ||
		m["schedule_status"] != "active" || m["materialization"] != "table" {
		t.Fatalf("list_models row = %v", m)
	}
	if e := m["last_run_error"].(string); !strings.Contains(e, "does not exist") || strings.Contains(e, "10.1.2.3") {
		t.Fatalf("last_run_error = %q, want its words with the host masked", e)
	}
	if got := fmt.Sprint(list["layers"]); got != "[Silver Gold]" {
		t.Fatalf("list_models layers = %s, want the workspace's own, in order", got)
	}
	if got := data(t, call(t, r, "list_models", map[string]any{"layer": "silver"})); got["total"] != float64(0) || got["note"] == "" {
		t.Fatalf("list_models layer=silver = %v, want none with a note", got)
	}
	if got := data(t, call(t, r, "list_models", map[string]any{"layer": "GOLD"})); got["total"] != float64(1) {
		t.Fatalf("list_models layer=GOLD = %v, want the Gold model: layers match ignoring case", got)
	}
	// A layer the workspace has not made is refused with the ones it has, not
	// answered with an empty list that reads as "no models there".
	res := call(t, r, "list_models", map[string]any{"layer": "bronze"})
	if !isError(res) || !strings.Contains(resultText(res), `no layer named "bronze"; its layers are Silver, Gold`) {
		t.Fatalf("list_models layer=bronze = %v, want a refusal naming the layers", res)
	}

	one := data(t, call(t, r, "get_model", validArgs["get_model"]))
	sched := one["schedule"].(map[string]any)
	up := sched["upstreams"].([]any)[0].(map[string]any)
	if sched["schedule_type"] != "after_upstream" || up["kind"] != "pipeline" || up["id"] != pipeID || sched["blocked"] != true {
		t.Fatalf("get_model schedule = %v", sched)
	}
	if strings.Contains(sched["blocked_reason"].(string), "jane.doe") {
		t.Fatalf("blocked_reason = %q, want the address masked", sched["blocked_reason"])
	}

	runs := data(t, call(t, r, "list_model_runs", validArgs["list_model_runs"]))
	run := runs["runs"].([]any)[0].(map[string]any)
	if run["status"] != "failed" || run["trigger_source"] != "upstream" || run["upstream_name"] != "orders sync" || runs["truncated"] != true {
		t.Fatalf("list_model_runs = %v", runs)
	}
	if e := run["error"].(string); !strings.Contains(e, "invalid input syntax") || strings.Contains(e, "987654321") {
		t.Fatalf("run error = %q, want its words with the row count masked", e)
	}
}

// A schedule's paused_reason is typed by a person and may name anyone or
// anything: it comes back with addresses, hosts and numbers masked, as the
// other reasons do.
func TestGetModel_PausedReasonScrubbed(t *testing.T) {
	sched := leakyBodies()["model_schedule"].(map[string]any)
	sched["paused_reason"] = "paused by jane.doe@example.com: host 10.1.2.3 down, " + fmt.Sprint(canaryNum) + " rows behind"
	h := leakyHandlers()
	h.GetSavedQuerySchedule = jsonHandler(200, sched)
	r, _ := testServer(t, h, readerActor)
	got := data(t, call(t, r, "get_model", validArgs["get_model"]))["schedule"].(map[string]any)["paused_reason"].(string)
	if !strings.Contains(got, "paused by") || !strings.Contains(got, "rows behind") {
		t.Fatalf("paused_reason = %q, want its words kept", got)
	}
	for _, leak := range []string{"jane.doe", "10.1.2.3", fmt.Sprint(canaryNum)} {
		if strings.Contains(got, leak) {
			t.Fatalf("paused_reason = %q, want %q masked", got, leak)
		}
	}
}

// get_model of a model with no schedule: the handler's 404 means "none", not
// an error.
func TestGetModel_NoSchedule(t *testing.T) {
	h := leakyHandlers()
	h.GetSavedQuerySchedule = jsonHandler(http.StatusNotFound, gin.H{"error": "no schedule for this saved query"})
	r, _ := testServer(t, h, readerActor)
	one := data(t, call(t, r, "get_model", validArgs["get_model"]))
	if _, ok := one["schedule"]; ok || one["model_id"] != modelID {
		t.Fatalf("get_model = %v, want the model and no schedule", one)
	}
}

// get_model says which tables the model's query reads and what writes each, so
// "what does this model read from?" is answered from one call: every table,
// the pipeline or model that writes one, and the tables nothing is known to
// write. A lookup that fails leaves the model's answer whole and says the
// tables could not be read, never that it reads none.
func TestGetModel_Reads(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	one := data(t, call(t, r, "get_model", validArgs["get_model"]))
	reads, ok := one["reads"].(map[string]any)
	if !ok {
		t.Fatalf("get_model = %v, want the tables it reads", one)
	}
	if got := fmt.Sprint(reads["tables"]); got != "[analytics.orders customers]" {
		t.Fatalf("reads.tables = %s, want every table the query names", got)
	}
	by := reads["written_by"].([]any)
	w := by[0].(map[string]any)
	if len(by) != 1 || w["kind"] != "pipeline" || w["id"] != pipeID || w["name"] != "orders sync" || w["table"] != "analytics.orders" {
		t.Fatalf("reads.written_by = %v, want the pipeline that writes analytics.orders", by)
	}
	if got := fmt.Sprint(reads["no_known_writer"]); got != "[customers]" || reads["ambiguous"] != true {
		t.Fatalf("reads = %v, want customers with no known writer, and ambiguous", reads)
	}

	h := leakyHandlers()
	h.SuggestSavedQueryUpstreams = jsonHandler(http.StatusInternalServerError, gin.H{"error": "failed to look up upstream producers"})
	r, _ = testServer(t, h, readerActor)
	one = data(t, call(t, r, "get_model", validArgs["get_model"]))
	if _, ok := one["reads"]; ok || one["model_id"] != modelID || one["schedule"] == nil {
		t.Fatalf("get_model with the lookup failing = %v, want the model and its schedule, no reads", one)
	}
	if note, _ := one["reads_note"].(string); !strings.Contains(note, "could not") {
		t.Fatalf("reads_note = %q, want it to say the tables could not be read", note)
	}
}

// create_model sends the query as written, line breaks and all, saves it for
// the workspace, then makes it a table model with the layer. The name, not
// Raw, is cleaned to one line.
func TestCreateModel_SavesThenMaterializes(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.CreateSavedQuery = rec.handler(http.StatusCreated, gin.H{"id": modelID})
	h.SetSavedQueryMaterialization = rec.handler(http.StatusOK, gin.H{"materialization": "table", "target_table": "gold.daily_orders", "layer": "Gold"})
	r, audit := testServer(t, h, adminActor)
	args := map[string]any{}
	for k, v := range validArgs["create_model"] {
		args[k] = v
	}
	args["name"] = "daily\norders"
	out := data(t, call(t, r, "create_model", args))
	if len(rec.calls) != 2 {
		t.Fatalf("calls = %+v, want a save and a materialization", rec.calls)
	}
	save, mat := rec.calls[0], rec.calls[1]
	if save.method != http.MethodPost || save.body["sql_text"] != validArgs["create_model"]["query"] ||
		save.body["visibility"] != "workspace" || save.body["name"] != "daily orders" || save.body["connection_id"] != connID {
		t.Fatalf("save = %+v", save)
	}
	if mat.method != http.MethodPut || mat.id != modelID || mat.body["materialization"] != "table" ||
		mat.body["target_table"] != "gold.daily_orders" || mat.body["layer"] != "gold" {
		t.Fatalf("materialization = %+v", mat)
	}
	// The layer goes as the agent named it; the handler matches it to the
	// workspace's own, and its spelling is what comes back.
	if out["model_id"] != modelID || out["layer"] != "Gold" || out["next"] == "" {
		t.Fatalf("create_model = %v", out)
	}
	if len(*audit) != 1 || (*audit)[0].resourceID != modelID {
		t.Fatalf("audit = %+v, want the new model's id", *audit)
	}
}

// A Raw query carrying a bidi override, a zero-width or a control character
// is refused, not cleaned: what runs must be what a reviewer reads. Control:
// tabs and line breaks pass.
func TestCreateModel_RefusesHiddenCharacters(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.CreateSavedQuery = rec.handler(http.StatusCreated, gin.H{"id": modelID})
	r, _ := testServer(t, h, adminActor)
	for name, q := range map[string]string{
		"bidi override": "SELECT 1 ‮-- ;DROP TABLE x",
		"zero width":    "SELECT​ 1",
		"tag character": "SELECT 1\U000E0041",
		"control":       "SELECT 1\x00",
	} {
		args := map[string]any{"connection_id": connID, "name": "m", "target_table": "t", "query": q}
		res := call(t, r, "create_model", args)
		if !isError(res) || !strings.Contains(resultText(res), "query has a control or invisible formatting character") {
			t.Fatalf("%s: want a refusal, got %v", name, res)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a refused query reached the handler: %+v", rec.calls)
	}
	args := map[string]any{"connection_id": connID, "name": "m", "target_table": "t", "query": "SELECT\n\t1"}
	if res := call(t, r, "create_model", args); isError(res) || len(rec.calls) != 1 || rec.calls[0].body["sql_text"] != "SELECT\n\t1" {
		t.Fatalf("control: %v, calls %+v", res, rec.calls)
	}
}

// A materialization the handler refuses (a query that writes, a target rsync.ai
// may not take over) deletes the query it just saved, and the agent reads the
// handler's reason. If the delete fails too, the agent is told which id is
// left behind.
func TestCreateModel_RollsBackARefusal(t *testing.T) {
	refusal := gin.H{"error": "a model that writes to a table must be a read query"}
	for _, deleteFails := range []bool{false, true} {
		var rec recorder
		h := leakyHandlers()
		h.CreateSavedQuery = rec.handler(http.StatusCreated, gin.H{"id": modelID})
		h.SetSavedQueryMaterialization = rec.handler(http.StatusBadRequest, refusal)
		if deleteFails {
			h.DeleteSavedQuery = rec.handler(http.StatusInternalServerError, gin.H{"error": "boom"})
		} else {
			h.DeleteSavedQuery = rec.handler(http.StatusOK, gin.H{"deleted": true})
		}
		r, audit := testServer(t, h, adminActor)
		res := call(t, r, "create_model", validArgs["create_model"])
		text := resultText(res)
		if !isError(res) || !strings.Contains(text, "must be a read query") {
			t.Fatalf("deleteFails=%v: want the handler's reason, got %v", deleteFails, res)
		}
		if len(rec.calls) != 3 || rec.calls[2].method != http.MethodDelete || rec.calls[2].id != modelID {
			t.Fatalf("deleteFails=%v: calls = %+v, want save, materialize, delete", deleteFails, rec.calls)
		}
		if strings.Contains(text, modelID) != deleteFails {
			t.Fatalf("deleteFails=%v: text = %q; the id belongs in it only when the query is left behind", deleteFails, text)
		}
		if (*audit)[0].outcome == "ok" {
			t.Fatalf("deleteFails=%v: audit = %+v", deleteFails, *audit)
		}
	}
}

// A refused write tells the agent why; a refused read keeps its fixed text,
// because a read's refusal has nothing for the agent to fix.
func TestRefusalReason_OnWritesOnly(t *testing.T) {
	h := leakyHandlers()
	h.CreateSavedQuery = jsonHandler(http.StatusConflict, gin.H{"error": "a saved query with this name already exists in this workspace"})
	h.GetSavedQuery = jsonHandler(http.StatusBadRequest, gin.H{"error": "invalid saved query id 'LEAKCANARY'"})
	r, _ := testServer(t, h, adminActor)
	if text := resultText(call(t, r, "create_model", validArgs["create_model"])); !strings.Contains(text, "already exists") {
		t.Fatalf("create_model refusal = %q, want the reason", text)
	}
	if text := resultText(call(t, r, "get_model", validArgs["get_model"])); text != "rsync.ai refused the request as invalid." {
		t.Fatalf("get_model refusal = %q, want the fixed text only", text)
	}
	// The reason is scrubbed: a quoted literal in it is masked.
	h.CreateSavedQuery = jsonHandler(http.StatusBadRequest, gin.H{"error": "column 'LEAKCANARY' at 10.1.2.3 is not allowed"})
	r, _ = testServer(t, h, adminActor)
	if text := resultText(call(t, r, "create_model", validArgs["create_model"])); strings.Contains(text, canary) ||
		strings.Contains(text, "10.1.2.3") || !strings.Contains(text, "is not allowed") {
		t.Fatalf("create_model refusal = %q, want the reason with its values masked", text)
	}
}

// create_silver_model asks rsync.ai for the generated query, then saves it as a
// silver model on the suggested target, named for its pipeline table, in the
// workspace's layer named Silver.
func TestCreateSilverModel(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.GetSilverSQL = rec.handler(http.StatusOK, gin.H{"source_table": "public.orders_history", "shape": "history",
		"keys": []string{"id"}, "columns": []string{"id", "status"}, "sql_text": "SELECT 'generated'",
		"target_table": "public.orders_history_silver"})
	h.CreateSavedQuery = rec.handler(http.StatusCreated, gin.H{"id": modelID})
	h.SetSavedQueryMaterialization = rec.handler(http.StatusOK, gin.H{"materialization": "table",
		"target_table": "public.orders_history_silver", "layer": "Silver"})
	r, _ := testServer(t, h, adminActor)
	out := data(t, call(t, r, "create_silver_model", map[string]any{"connection_id": connID,
		"table": "public.orders_history", "keys": " id , tenant_id ,"}))
	if len(rec.calls) != 3 {
		t.Fatalf("calls = %+v", rec.calls)
	}
	plan, save, mat := rec.calls[0], rec.calls[1], rec.calls[2]
	if plan.id != connID || plan.body["table"] != "public.orders_history" {
		t.Fatalf("plan request = %+v", plan)
	}
	if keys, _ := plan.body["keys"].([]any); len(keys) != 2 || keys[0] != "id" || keys[1] != "tenant_id" {
		t.Fatalf("plan keys = %v", plan.body["keys"])
	}
	if save.body["sql_text"] != "SELECT 'generated'" || save.body["name"] != "silver public.orders_history" {
		t.Fatalf("save = %+v", save)
	}
	if mat.body["layer"] != "Silver" || mat.body["target_table"] != "public.orders_history_silver" {
		t.Fatalf("materialization = %+v", mat)
	}
	if out["shape"] != "history" || out["model_id"] != modelID || out["layer"] != "Silver" {
		t.Fatalf("create_silver_model = %v", out)
	}
	if strings.Contains(mustJSON(t, out), "generated") {
		t.Fatalf("create_silver_model returned the query: %v", out)
	}
}

// A workspace without a layer named Silver gets a silver model in no layer: its
// layers are its own, and naming one it has not made would refuse the model.
func TestCreateSilverModel_NoSilverLayer(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.ListWorkspaceLayers = jsonHandler(200, map[string]any{"can_manage": true, "layers": []any{
		map[string]any{"id": genID, "name": "Marts", "position": 0},
	}})
	h.SetSavedQueryMaterialization = rec.handler(http.StatusOK, gin.H{"materialization": "table",
		"target_table": "public.orders_history_silver", "layer": ""})
	r, _ := testServer(t, h, adminActor)
	data(t, call(t, r, "create_silver_model", validArgs["create_silver_model"]))
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %+v", rec.calls)
	}
	if layer, sent := rec.calls[0].body["layer"]; !sent || layer != "" {
		t.Fatalf("materialization layer = %v (sent %v), want \"\": no layer", layer, sent)
	}
}

// schedule_model builds the handler's body from flat arguments, and refuses a
// combination that has no meaning before the handler sees it.
func TestScheduleModel(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.CreateSavedQuerySchedule = rec.handler(http.StatusCreated, leakyBodies()["model_schedule"])
	r, _ := testServer(t, h, adminActor)

	call(t, r, "schedule_model", map[string]any{"model_id": modelID, "schedule_type": "chain",
		"after_pipelines": pipeID, "after_models": genID + "," + runID, "upstream_policy": "all"})
	b := rec.calls[0].body
	ups, _ := b["upstreams"].([]any)
	if b["schedule_type"] != "after_upstream" || len(ups) != 3 || b["upstream_policy"] != "all" ||
		ups[0].(map[string]any)["kind"] != "pipeline" || ups[2].(map[string]any)["id"] != runID {
		t.Fatalf("after_upstream body = %v", b)
	}

	call(t, r, "schedule_model", map[string]any{"model_id": modelID, "schedule_type": "every", "every_seconds": 300})
	spec := rec.calls[1].body["schedule_spec"].(map[string]any)
	if rec.calls[1].body["schedule_type"] != "interval" || spec["every_seconds"] != float64(300) || spec["timezone"] != "UTC" {
		t.Fatalf("interval body = %v", rec.calls[1].body)
	}
	if _, ok := rec.calls[1].body["upstreams"]; ok {
		t.Fatalf("interval body carries upstreams: %v", rec.calls[1].body)
	}

	for name, args := range map[string]map[string]any{
		"interval under a minute":     {"schedule_type": "interval", "every_seconds": 30},
		"cron without an expression":  {"schedule_type": "cron"},
		"after_upstream with nothing": {"schedule_type": "after_upstream"},
		"upstreams on a cron":         {"schedule_type": "cron", "cron": "0 * * * *", "after_pipelines": pipeID},
		"an upstream that is no id":   {"schedule_type": "after_upstream", "after_pipelines": "orders sync"},
	} {
		args["model_id"] = modelID
		if res := call(t, r, "schedule_model", args); !isError(res) || !strings.Contains(resultText(res), "Invalid arguments") {
			t.Fatalf("%s: want an argument error, got %v", name, res)
		}
	}
	if len(rec.calls) != 2 {
		t.Fatalf("a refused schedule reached the handler: %+v", rec.calls)
	}
}

// run_model starts the run, returns at once and names the table it replaces;
// a refusal carries its reason.
func TestRunModel(t *testing.T) {
	var rec recorder
	h := leakyHandlers()
	h.StartSavedQueryModelRun = rec.handler(http.StatusAccepted, gin.H{"status": "started", "model_id": modelID})
	r, _ := testServer(t, h, adminActor)
	out := data(t, call(t, r, "run_model", validArgs["run_model"]))
	if out["status"] != "started" || out["name"] != "daily orders" || out["target_table"] != "gold.daily_orders" ||
		len(rec.calls) != 1 || rec.calls[0].method != http.MethodPost || rec.calls[0].id != modelID {
		t.Fatalf("run_model = %v, calls %+v", out, rec.calls)
	}
	h.StartSavedQueryModelRun = jsonHandler(http.StatusBadRequest, gin.H{"error": "this saved query is not a model; give it a materialization first"})
	r, _ = testServer(t, h, adminActor)
	if text := resultText(call(t, r, "run_model", validArgs["run_model"])); !strings.Contains(text, "not a model") {
		t.Fatalf("run_model refusal = %q, want the reason", text)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
