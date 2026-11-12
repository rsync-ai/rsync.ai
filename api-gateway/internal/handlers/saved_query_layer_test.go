package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

// expectMaterializationPreamble is what SetSavedQueryMaterialization reads before its
// UPDATE in table mode: the role and visibility gates, the model, the run-as role.
func expectMaterializationPreamble(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))
	mock.ExpectQuery(`FROM saved_queries\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(savedQueryID, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(1))
}

func expectMaterializationModel(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM saved_queries sq\s+JOIN connections c`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "connection_id", "name", "sql_text",
			"materialization", "target_table", "target_owned", "connector_type", "config",
		}).AddRow(savedQueryID, wsScopeWS, savedQueryConn, "Daily MRR", "SELECT 1",
			matNone, "", false, "postgresql", "{}"))
	mock.ExpectQuery(`SELECT role FROM workspace_members`).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))
}

// expectWorkspaceLayers is resolveModelLayer's read of the model's own workspace's
// layers, in order.
func expectWorkspaceLayers(mock sqlmock.Sqlmock, layers ...[2]string) {
	rows := sqlmock.NewRows([]string{"id", "name"})
	for _, l := range layers {
		rows.AddRow(l[0], l[1])
	}
	mock.ExpectQuery(`FROM workspace_layers wl\s+JOIN saved_queries sq ON sq.workspace_id = wl.workspace_id`).
		WithArgs(savedQueryID).
		WillReturnRows(rows)
}

// A layer is one of the workspace's own (workspace_layers.go), named in any case. It is
// set with the model's materialization, kept when the request does not name one (a
// caller that predates layers must not wipe one an admin set), cleared by "", and
// dropped when the query stops being a model, whatever the request names. A name the
// workspace has no layer for is refused with the names it has.
func TestSetSavedQueryMaterialization_Layer(t *testing.T) {
	const stagingID, martsID = "77777777-7777-7777-7777-777777777771", "77777777-7777-7777-7777-777777777772"
	staging, marts := [2]string{stagingID, "Staging"}, [2]string{martsID, "Marts"}
	cases := []struct {
		name string
		body map[string]any
		// layers is the workspace's; nil means the handler must not read them.
		layers [][2]string
		status int
		// update is the UPDATE's $4 (keep) and $5 (layer id); nil means no table-mode UPDATE.
		update []driver.Value
		// clears is true when the request turns the model off.
		clears    bool
		wantLayer any
		wantError string
	}{
		{"sets one by name, any case", map[string]any{"materialization": "table", "target_table": "public.daily_mrr", "layer": " marts "},
			[][2]string{staging, marts}, http.StatusOK, []driver.Value{false, martsID}, false, "Marts", ""},
		{"absent keeps", map[string]any{"materialization": "table", "target_table": "public.daily_mrr"},
			nil, http.StatusOK, []driver.Value{true, nil}, false, nil, ""},
		{"empty clears", map[string]any{"materialization": "table", "target_table": "public.daily_mrr", "layer": ""},
			nil, http.StatusOK, []driver.Value{false, nil}, false, "", ""},
		{"unknown refused", map[string]any{"materialization": "table", "target_table": "public.daily_mrr", "layer": "bronze"},
			[][2]string{staging, marts}, http.StatusBadRequest, nil, false, nil,
			`This workspace has no layer named "bronze". Its layers are Staging, Marts.`},
		{"none yet refused", map[string]any{"materialization": "table", "target_table": "public.daily_mrr", "layer": "Gold"},
			[][2]string{}, http.StatusBadRequest, nil, false, nil,
			`This workspace has no layers yet, so a model cannot go in "Gold". An owner or admin adds layers in Workspace settings.`},
		{"none drops it", map[string]any{"materialization": "none", "layer": "silver"},
			nil, http.StatusOK, nil, true, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectMaterializationPreamble(mock)
			if tc.layers != nil {
				expectWorkspaceLayers(mock, tc.layers...)
			}
			switch {
			case tc.clears:
				mock.ExpectExec(`UPDATE saved_queries SET materialization = 'none', layer_id = NULL`).
					WithArgs(savedQueryID).
					WillReturnResult(sqlmock.NewResult(0, 1))
			case tc.update != nil:
				expectMaterializationModel(mock)
				mock.ExpectExec(`UPDATE saved_queries[\s\S]+layer_id\s+= CASE WHEN \$4 THEN layer_id ELSE \$5::uuid END`).
					WithArgs(savedQueryID, "public.daily_mrr", matTable, tc.update[0], tc.update[1]).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}

			r := savedQueryRouter(http.MethodPut, "/explorer/saved/:id/materialization", "admin", SetSavedQueryMaterialization)
			w := doJSON(r, http.MethodPut, "/explorer/saved/"+savedQueryID+"/materialization", tc.body)
			if w.Code != tc.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && resp["error"] != tc.wantError {
				t.Fatalf("error = %q\nwant    %q", resp["error"], tc.wantError)
			}
			if tc.status == http.StatusOK && !tc.clears && resp["layer"] != tc.wantLayer {
				t.Fatalf("layer in response = %v, want %v", resp["layer"], tc.wantLayer)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The layer is deleted between the lookup and the write: the composite foreign key
// refuses the UPDATE, and the user is told to pick another rather than seeing a 500.
func TestSetSavedQueryMaterialization_LayerDeletedMeanwhile(t *testing.T) {
	const layerID = "77777777-7777-7777-7777-777777777771"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectMaterializationPreamble(mock)
	expectWorkspaceLayers(mock, [2]string{layerID, "Staging"})
	expectMaterializationModel(mock)
	mock.ExpectExec(`UPDATE saved_queries[\s\S]+layer_id`).
		WithArgs(savedQueryID, "public.daily_mrr", matTable, false, layerID).
		WillReturnError(&pgconn.PgError{Code: "23503"})

	r := savedQueryRouter(http.MethodPut, "/explorer/saved/:id/materialization", "admin", SetSavedQueryMaterialization)
	w := doJSON(r, http.MethodPut, "/explorer/saved/"+savedQueryID+"/materialization",
		map[string]any{"materialization": "table", "target_table": "public.daily_mrr", "layer": "Staging"})
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d %s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `The layer \"Staging\" was just deleted`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
