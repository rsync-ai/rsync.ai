//go:build integration_pg

// Real-Postgres coverage for a workspace's own model layers (migration 135,
// workspace_layers.go): the SQL sqlmock cannot judge. The deferred position
// constraint a reorder relies on, the unnest WITH ORDINALITY rewrite, the
// case-insensitive name index, the composite foreign key a delete must clear
// first, the $5::uuid a model's layer is written through, and the read paths
// that turn layer_id back into a name.
//
// Same database and harness as workflows_pg_test.go (pgWorkflowDB applies every
// migration and seeds the workspace):
//
//	docker run -d --name wf-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55471:5432 postgres:16-alpine
//	WF_PG_DSN='postgres://postgres:verify@localhost:55471/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_WorkspaceLayer -v
package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

const (
	pgLayerOther   = "11111111-aaaa-4000-8000-000000000002" // another member of the workspace
	pgLayerModel   = "55555555-aaaa-4000-8000-000000000001" // the owner's model
	pgLayerPrivate = "55555555-aaaa-4000-8000-000000000002" // the other member's private model
)

func pgLayerRouter(role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgWFUser)
		c.Set(ctxWorkspaceID, pgWFWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/model-layers", ListWorkspaceLayers)
	r.POST("/model-layers", CreateWorkspaceLayer)
	r.PATCH("/model-layers/:id", UpdateWorkspaceLayer)
	r.DELETE("/model-layers/:id", DeleteWorkspaceLayer)
	r.PUT("/explorer/saved/:id/materialization", SetSavedQueryMaterialization)
	r.GET("/explorer/saved", ListSavedQueries)
	r.GET("/explorer/saved/:id", GetSavedQuery)
	r.GET("/explorer/schedules", ListSavedQuerySchedules)
	r.GET("/explorer/asset-graph", GetWorkspaceAssetGraph)
	return r
}

// pgLayerDB is pgWorkflowDB plus a second member and two models: the owner's, and
// the other member's private one, which the owner must not count.
func pgLayerDB(t *testing.T) *sql.DB {
	t.Helper()
	d := pgWorkflowDB(t)
	clean := func() {
		for _, q := range []string{
			`DELETE FROM saved_queries WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM workspace_layers WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM audit_logs WHERE workspace_id = '` + pgWFWS + `'`,
			`DELETE FROM workspace_members WHERE user_id = '` + pgLayerOther + `'`,
			`DELETE FROM users WHERE id = '` + pgLayerOther + `'`,
		} {
			if _, err := d.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'member@example.com', 'x')`, pgLayerOther)
	wfExec(t, d, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, pgWFWS, pgLayerOther)
	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by)
		VALUES ($1, $2, $3, 'Daily revenue', 'SELECT 1 AS n', $4)`, pgLayerModel, pgWFWS, pgWFConn, pgWFUser)
	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by, visibility)
		VALUES ($1, $2, $3, 'My scratch', 'SELECT 2 AS n', $4, 'private')`, pgLayerPrivate, pgWFWS, pgWFConn, pgLayerOther)
	wfExec(t, d, `INSERT INTO saved_query_schedules (saved_query_id, schedule_type, schedule_spec,
		temporal_schedule_id, run_as_user_id, created_by)
		VALUES ($1, 'interval', '{"every":"1h"}', 'sq-layers-pg', $2, $2)`, pgLayerModel, pgWFUser)
	return d
}

// pgLayers lists the workspace's layers through the handler, as "name:count".
func pgLayers(t *testing.T, r *gin.Engine) ([]string, map[string]string) {
	t.Helper()
	code, out := pgDo(t, r, http.MethodGet, "/model-layers", nil)
	if code != http.StatusOK {
		t.Fatalf("list layers: %d %v", code, out)
	}
	var got []string
	ids := map[string]string{}
	for _, raw := range out["layers"].([]any) {
		l := raw.(map[string]any)
		got = append(got, l["name"].(string)+":"+strconv.Itoa(int(l["model_count"].(float64))))
		ids[l["name"].(string)] = l["id"].(string)
		if int(l["position"].(float64)) != len(got)-1 {
			t.Errorf("layer %v at position %v, want %d: positions must stay dense", l["name"], l["position"], len(got)-1)
		}
	}
	return got, ids
}

func TestPG_WorkspaceLayers(t *testing.T) {
	d := pgLayerDB(t)
	owner, viewer := pgLayerRouter("owner"), pgLayerRouter("viewer")

	// A new workspace has no layers, and a model cannot be put in one.
	if got, _ := pgLayers(t, owner); len(got) != 0 {
		t.Fatalf("a workspace starts with no layers; got %v", got)
	}
	code, out := pgDo(t, owner, http.MethodPut, "/explorer/saved/"+pgLayerModel+"/materialization",
		map[string]any{"materialization": "table", "target_table": "public.daily_revenue", "layer": "Gold"})
	if code != http.StatusBadRequest {
		t.Fatalf("a layer in a workspace with none: %d %v, want 400", code, out)
	}

	for _, name := range []string{"Silver", "Gold", "  Data   marts "} {
		if code, out := pgDo(t, owner, http.MethodPost, "/model-layers", map[string]any{"name": name}); code != http.StatusCreated {
			t.Fatalf("create %q: %d %v", name, code, out)
		}
	}
	if code, out := pgDo(t, owner, http.MethodPost, "/model-layers", map[string]any{"name": "GOLD"}); code != http.StatusConflict {
		t.Fatalf("a second layer named GOLD: %d %v, want 409", code, out)
	}
	if code, _ := pgDo(t, viewer, http.MethodPost, "/model-layers", map[string]any{"name": "Raw"}); code != http.StatusForbidden {
		t.Fatalf("a viewer added a layer: %d, want 403", code)
	}
	got, ids := pgLayers(t, owner)
	if want := []string{"Silver:0", "Gold:0", "Data marts:0"}; !equalStrings(got, want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	if _, out := pgDo(t, viewer, http.MethodGet, "/model-layers", nil); out["can_manage"] != false {
		t.Errorf("a viewer was told it can manage layers: %v", out)
	}

	// Move the last layer first: three positions rewritten in one statement, which
	// only commits because the position constraint is deferred.
	if code, out := pgDo(t, owner, http.MethodPatch, "/model-layers/"+ids["Data marts"], map[string]any{"position": 0}); code != http.StatusOK {
		t.Fatalf("move: %d %v", code, out)
	}
	if code, out := pgDo(t, owner, http.MethodPatch, "/model-layers/"+ids["Silver"], map[string]any{"name": "Staging"}); code != http.StatusOK {
		t.Fatalf("rename: %d %v", code, out)
	}
	ids["Staging"] = ids["Silver"]
	if code, out := pgDo(t, owner, http.MethodPatch, "/model-layers/"+ids["Gold"], map[string]any{"name": "staging"}); code != http.StatusConflict {
		t.Fatalf("rename onto another layer's name: %d %v, want 409", code, out)
	}
	if got, _ := pgLayers(t, owner); !equalStrings(got, []string{"Data marts:0", "Staging:0", "Gold:0"}) {
		t.Fatalf("after move and rename, layers = %v", got)
	}

	// A model goes in a layer by name, in any case; the id is what is stored.
	code, out = pgDo(t, owner, http.MethodPut, "/explorer/saved/"+pgLayerModel+"/materialization",
		map[string]any{"materialization": "table", "target_table": "public.daily_revenue", "layer": "gold"})
	if code != http.StatusOK || out["layer"] != "Gold" {
		t.Fatalf("put the model in gold: %d %v", code, out)
	}
	var stored string
	if err := d.QueryRow(`SELECT layer_id::text FROM saved_queries WHERE id = $1`, pgLayerModel).Scan(&stored); err != nil || stored != ids["Gold"] {
		t.Fatalf("stored layer_id = %q (%v), want Gold's %s", stored, err, ids["Gold"])
	}
	code, out = pgDo(t, owner, http.MethodPut, "/explorer/saved/"+pgLayerModel+"/materialization",
		map[string]any{"materialization": "table", "target_table": "public.daily_revenue", "layer": "bronze"})
	if code != http.StatusBadRequest || out["error"] != `This workspace has no layer named "bronze". Its layers are Data marts, Staging, Gold.` {
		t.Fatalf("an unknown layer: %d %v", code, out)
	}
	// Leaving the layer out keeps it.
	if code, out := pgDo(t, owner, http.MethodPut, "/explorer/saved/"+pgLayerModel+"/materialization",
		map[string]any{"materialization": "table", "target_table": "public.daily_revenue"}); code != http.StatusOK {
		t.Fatalf("re-save without a layer: %d %v", code, out)
	}

	// The other member's private model is in Staging; the owner cannot see it, so
	// does not count it.
	wfExec(t, d, `UPDATE saved_queries SET layer_id = $2 WHERE id = $1`, pgLayerPrivate, ids["Staging"])
	if got, _ := pgLayers(t, owner); !equalStrings(got, []string{"Data marts:0", "Staging:0", "Gold:1"}) {
		t.Fatalf("model counts = %v: a private model of another member was counted, or the model was not", got)
	}

	// Every read path says the layer by name.
	_, out = pgDo(t, owner, http.MethodGet, "/explorer/saved", nil)
	if l := findByField(out["saved_queries"], "id", pgLayerModel)["layer"]; l != "Gold" {
		t.Errorf("list says layer %v, want Gold", l)
	}
	_, out = pgDo(t, owner, http.MethodGet, "/explorer/saved/"+pgLayerModel, nil)
	if out["layer"] != "Gold" {
		t.Errorf("get says layer %v, want Gold", out["layer"])
	}
	_, out = pgDo(t, owner, http.MethodGet, "/explorer/schedules", nil)
	if l := findByField(out["schedules"], "saved_query_id", pgLayerModel)["layer"]; l != "Gold" {
		t.Errorf("schedules say layer %v, want Gold", l)
	}
	_, out = pgDo(t, owner, http.MethodGet, "/explorer/asset-graph", nil)
	if l := findByField(out["nodes"], "id", assetKindModel+":"+pgLayerModel)["layer"]; l != "Gold" {
		t.Errorf("asset graph says layer %v, want Gold", l)
	}

	// Deleting a layer takes its models out of it, the owner's and the private one
	// alike, and closes the gap in the order.
	code, out = pgDo(t, owner, http.MethodDelete, "/model-layers/"+ids["Staging"], nil)
	if code != http.StatusOK || out["models_unlabelled"] != float64(1) {
		t.Fatalf("delete Staging: %d %v, want 1 model unlabelled", code, out)
	}
	code, out = pgDo(t, owner, http.MethodDelete, "/model-layers/"+ids["Gold"], nil)
	if code != http.StatusOK || out["models_unlabelled"] != float64(1) {
		t.Fatalf("delete Gold: %d %v, want 1 model unlabelled", code, out)
	}
	if got, _ := pgLayers(t, owner); !equalStrings(got, []string{"Data marts:0"}) {
		t.Fatalf("after two deletes, layers = %v", got)
	}
	var labelled int
	if err := d.QueryRow(`SELECT COUNT(*) FROM saved_queries WHERE workspace_id = $1 AND layer_id IS NOT NULL`, pgWFWS).Scan(&labelled); err != nil || labelled != 0 {
		t.Fatalf("%d models still point at a deleted layer (%v)", labelled, err)
	}
	_, out = pgDo(t, owner, http.MethodGet, "/explorer/saved/"+pgLayerModel, nil)
	if l, present := out["layer"]; present && l != "" {
		t.Errorf("after the delete the model says layer %v", l)
	}
	var audited int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE workspace_id = $1 AND resource_type = 'workspace_layer'`, pgWFWS).Scan(&audited); err != nil || audited != 7 {
		t.Errorf("audit rows = %d (%v), want 7: three creates, a move, a rename, two deletes", audited, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func findByField(list any, field, value string) map[string]any {
	items, _ := list.([]any)
	for _, raw := range items {
		if m, ok := raw.(map[string]any); ok && m[field] == value {
			return m
		}
	}
	return map[string]any{}
}
