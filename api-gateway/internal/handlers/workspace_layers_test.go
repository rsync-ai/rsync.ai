package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	layerSilver = "66666666-6666-6666-6666-666666666661"
	layerGold   = "66666666-6666-6666-6666-666666666662"
	layerMarts  = "66666666-6666-6666-6666-666666666663"
)

func TestWorkspaceLayerNameAndDescription(t *testing.T) {
	for raw, want := range map[string]string{
		"  Silver ":        "Silver",
		"Staging\t\n area": "Staging area",
		"Gold":             "Gold",
	} {
		if got, err := validWorkspaceLayerName(raw); err != nil || got != want {
			t.Errorf("name %q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "   ", strings.Repeat("x", 41), "Gold\x00", "Si\x1flver"} {
		if got, err := validWorkspaceLayerName(raw); err == nil {
			t.Errorf("name %q accepted as %q", raw, got)
		}
	}
	if _, err := validWorkspaceLayerName(strings.Repeat("é", 40)); err != nil {
		t.Errorf("40 characters is the limit, not 40 bytes: %v", err)
	}
	if _, err := validWorkspaceLayerDescription(strings.Repeat("d", 201)); err == nil {
		t.Error("a 201-character description was accepted")
	}
	if got, err := validWorkspaceLayerDescription(""); err != nil || got != "" {
		t.Errorf("an empty description: %q, %v", got, err)
	}
}

// Anyone in the workspace lists the layers; only an owner or admin manages
// them, and the count covers only the models the caller can see.
func TestListWorkspaceLayers(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM workspace_layers wl\s+WHERE wl.workspace_id = \$1\s+ORDER BY wl.position`).
		WithArgs(wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "position", "count"}).
			AddRow(layerSilver, "Silver", "Cleaned", 0, 3).
			AddRow(layerGold, "Gold", "", 1, 0))
	w := doJSON(savedQueryRouter(http.MethodGet, "/model-layers", "viewer", ListWorkspaceLayers),
		http.MethodGet, "/model-layers", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"name":"Silver"`, `"model_count":3`, `"position":1`, `"can_manage":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("list body lacks %s: %s", want, body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceLayerWrites_OwnerOrAdminOnly(t *testing.T) {
	_, cleanup := wsScopeMockDB(t) // no expectations: a refused call touches nothing
	defer cleanup()
	for _, role := range []string{"viewer", "member"} {
		if w := doJSON(savedQueryRouter(http.MethodPost, "/model-layers", role, CreateWorkspaceLayer),
			http.MethodPost, "/model-layers", map[string]any{"name": "Marts"}); w.Code != http.StatusForbidden {
			t.Errorf("%s create: %d", role, w.Code)
		}
		if w := doJSON(savedQueryRouter(http.MethodPatch, "/model-layers/:id", role, UpdateWorkspaceLayer),
			http.MethodPatch, "/model-layers/"+layerGold, map[string]any{"name": "Marts"}); w.Code != http.StatusForbidden {
			t.Errorf("%s update: %d", role, w.Code)
		}
		if w := doJSON(savedQueryRouter(http.MethodDelete, "/model-layers/:id", role, DeleteWorkspaceLayer),
			http.MethodDelete, "/model-layers/"+layerGold, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s delete: %d", role, w.Code)
		}
	}
}

func expectLayerLock(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtext\(\$1\)\)`).
		WithArgs("workspace_layers:" + wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 0))
}

// A new layer goes after the last one, under the workspace's layer lock, with
// its name tidied; a second layer with the same name is a 409 that says so.
func TestCreateWorkspaceLayer(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	post := func(body map[string]any) (int, string) {
		w := doJSON(savedQueryRouter(http.MethodPost, "/model-layers", "admin", CreateWorkspaceLayer),
			http.MethodPost, "/model-layers", body)
		return w.Code, w.Body.String()
	}

	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT COUNT\(\*\), COALESCE\(MAX\(position\) \+ 1, 0\)\s+FROM workspace_layers WHERE workspace_id = \$1`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"n", "next"}).AddRow(2, 2))
	mock.ExpectQuery(`INSERT INTO workspace_layers`).
		WithArgs(wsScopeWS, "Data marts", "Facts and dimensions", 2, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(layerMarts))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	if code, body := post(map[string]any{"name": " Data   marts ", "description": "Facts and dimensions"}); code != http.StatusCreated ||
		!strings.Contains(body, layerMarts) {
		t.Fatalf("create: %d %s", code, body)
	}

	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"n", "next"}).AddRow(2, 2))
	mock.ExpectQuery(`INSERT INTO workspace_layers`).WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectRollback()
	if code, body := post(map[string]any{"name": "gold"}); code != http.StatusConflict ||
		!strings.Contains(body, `already has a layer named \"gold\"`) {
		t.Fatalf("duplicate: %d %s", code, body)
	}

	if code, _ := post(map[string]any{"description": "no name"}); code != http.StatusBadRequest {
		t.Fatalf("no name: %d", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The 20-layer cap is checked under the lock, so two admins adding at once
// cannot both pass it.
func TestCreateWorkspaceLayer_Cap(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WillReturnRows(sqlmock.NewRows([]string{"n", "next"}).AddRow(20, 20))
	mock.ExpectRollback()
	w := doJSON(savedQueryRouter(http.MethodPost, "/model-layers", "owner", CreateWorkspaceLayer),
		http.MethodPost, "/model-layers", map[string]any{"name": "One more"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "at most 20 layers") {
		t.Fatalf("cap: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Moving a layer rewrites every position in one statement, in the new order;
// a position past the end means last.
func TestUpdateWorkspaceLayer_Move(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT id FROM workspace_layers WHERE workspace_id = \$1 ORDER BY position`).
		WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(layerSilver).AddRow(layerGold).AddRow(layerMarts))
	mock.ExpectExec(`UPDATE workspace_layers wl SET position = v.ord - 1\s+FROM unnest\(string_to_array\(\$2, ','\)::uuid\[\]\) WITH ORDINALITY`).
		WithArgs(wsScopeWS, layerGold+","+layerMarts+","+layerSilver).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	w := doJSON(savedQueryRouter(http.MethodPatch, "/model-layers/:id", "admin", UpdateWorkspaceLayer),
		http.MethodPatch, "/model-layers/"+strings.ToUpper(layerSilver), map[string]any{"position": 9})
	if w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateWorkspaceLayer_RenameAndRefusals(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	patch := func(id string, body map[string]any) (int, string) {
		w := doJSON(savedQueryRouter(http.MethodPatch, "/model-layers/:id", "admin", UpdateWorkspaceLayer),
			http.MethodPatch, "/model-layers/"+id, body)
		return w.Code, w.Body.String()
	}
	order := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id"}).AddRow(layerSilver).AddRow(layerGold)
	}

	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT id FROM workspace_layers`).WillReturnRows(order())
	mock.ExpectExec(`UPDATE workspace_layers\s+SET name = COALESCE\(\$3, name\), description = COALESCE\(\$4, description\)`).
		WithArgs(layerGold, wsScopeWS, "Marts", nil).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	if code, body := patch(layerGold, map[string]any{"name": "Marts"}); code != http.StatusOK {
		t.Fatalf("rename: %d %s", code, body)
	}

	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT id FROM workspace_layers`).WillReturnRows(order())
	mock.ExpectExec(`UPDATE workspace_layers`).WillReturnError(&pgconn.PgError{Code: "23505"})
	mock.ExpectRollback()
	if code, body := patch(layerGold, map[string]any{"name": "SILVER"}); code != http.StatusConflict ||
		!strings.Contains(body, "already has a layer named") {
		t.Fatalf("rename onto another: %d %s", code, body)
	}

	// Another workspace's layer is not in this workspace's order: 404.
	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT id FROM workspace_layers`).WillReturnRows(order())
	mock.ExpectRollback()
	if code, _ := patch(layerMarts, map[string]any{"name": "Mine"}); code != http.StatusNotFound {
		t.Fatalf("foreign layer: %d", code)
	}

	for _, body := range []map[string]any{{}, {"position": -1}, {"name": "  "}} {
		if code, _ := patch(layerGold, body); code != http.StatusBadRequest {
			t.Errorf("%v: %d, want 400", body, code)
		}
	}
	if code, _ := patch("not-a-uuid", map[string]any{"name": "x"}); code != http.StatusNotFound {
		t.Errorf("bad id: %d", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Deleting a layer keeps its models, with no layer, and closes the gap in the
// positions; the audit row says how many models lost the label.
func TestDeleteWorkspaceLayer(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT name FROM workspace_layers\s+WHERE id = \$1 AND workspace_id = \$2 FOR UPDATE`).
		WithArgs(layerSilver, wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Silver"))
	mock.ExpectExec(`UPDATE saved_queries SET layer_id = NULL\s+WHERE workspace_id = \$1 AND layer_id = \$2`).
		WithArgs(wsScopeWS, layerSilver).WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(`DELETE FROM workspace_layers WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(layerSilver, wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM workspace_layers`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(layerGold).AddRow(layerMarts))
	mock.ExpectExec(`UPDATE workspace_layers wl SET position`).
		WithArgs(wsScopeWS, layerGold+","+layerMarts).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	var details string
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(wsScopeUser, "delete_workspace_layer", "workspace_layer", layerSilver,
			captureJSONArg(&details), sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := doJSON(savedQueryRouter(http.MethodDelete, "/model-layers/:id", "owner", DeleteWorkspaceLayer),
		http.MethodDelete, "/model-layers/"+layerSilver, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"models_unlabelled":4`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(details, `"models_unlabelled":4`) || !strings.Contains(details, `"name":"Silver"`) {
		t.Errorf("audit details = %s", details)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteWorkspaceLayer_NotFound(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLayerLock(mock)
	mock.ExpectQuery(`SELECT name FROM workspace_layers`).WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectRollback()
	w := doJSON(savedQueryRouter(http.MethodDelete, "/model-layers/:id", "admin", DeleteWorkspaceLayer),
		http.MethodDelete, "/model-layers/"+layerMarts, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
