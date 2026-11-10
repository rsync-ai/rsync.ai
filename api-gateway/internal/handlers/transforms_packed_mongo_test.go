package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
)

// The bug class: a consumer rule written for a flat row meets a MongoDB CDC
// change, which the sink keeps as {_id, document}. Only mask_pii and
// type_convert run inside the document; the sink refuses any other rule and
// sends every change of the table to the DLQ. The save and the enable refuse
// such a rule first, and only on a MongoDB CDC pipeline.

// sourceShapeRe matches pipelineIsMongoCDC's lookup.
const sourceShapeRe = `SELECT COALESCE\(sc\.connector_type, ''\),`

func sourceShapeRows(connectorType string, cdc bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"connector_type", "is_cdc"}).AddRow(connectorType, cdc)
}

func postTransforms(t *testing.T, items []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	h := NewTransformHandler(db.GetDB(), "")
	r := wsScopeRouter(http.MethodPost, "/api/v1/transforms/pipeline/:pipeline_id", h.SavePipelineTransforms)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transforms/pipeline/"+idorPipelineID,
		bytes.NewReader(idorMustJSON(map[string]any{"transforms": items})))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func expectSave(mock sqlmock.Sqlmock, inserts int) {
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM transform_definitions WHERE pipeline_id = \$1`).
		WithArgs(idorPipelineID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	for i := 0; i < inserts; i++ {
		mock.ExpectExec(`INSERT INTO transform_definitions`).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
}

func consumerFilter() map[string]any {
	return map[string]any{"transform_type": "consumer", "enabled": true,
		"transform_config": map[string]any{"operation": "filter", "condition": "id > 1"}}
}

func TestSavePipelineTransforms_RefusesConsumerRulesOnMongoCDC(t *testing.T) {
	for _, src := range []string{"mongodb", "mongodb-atlas"} {
		t.Run(src, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
			mock.ExpectQuery(sourceShapeRe).WithArgs(idorPipelineID).WillReturnRows(sourceShapeRows(src, true))

			w := postTransforms(t, []map[string]any{
				{"transform_type": "consumer", "enabled": true,
					"transform_config": map[string]any{"operation": "mask_pii", "column": "email"}},
				consumerFilter(),
				// Disabled, but PUT can enable it later: judged as if on.
				{"transform_type": "consumer", "enabled": false,
					"transform_config": map[string]any{"operation": "select", "columns": []any{"email"}}},
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{"transform[1]: filter", "transform[2]: select_columns", "MongoDB CDC"} {
				if !strings.Contains(body, want) {
					t.Errorf("response does not say %q: %s", want, body)
				}
			}
			if strings.Contains(body, "transform[0]") {
				t.Errorf("mask_pii runs inside the document and must not be refused: %s", body)
			}
			// No BEGIN/DELETE: nothing was replaced.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// The same consumer filter is fine where rows are flat: a MongoDB batch
// pipeline (each row is the document's own fields) and a relational CDC one.
func TestSavePipelineTransforms_AllowsConsumerRulesWhereRowsAreFlat(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		cdc       bool
	}{
		{"mongodb batch", "mongodb", false},
		{"postgresql cdc", "postgresql", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
			mock.ExpectQuery(sourceShapeRe).WithArgs(idorPipelineID).WillReturnRows(sourceShapeRows(tc.src, tc.cdc))
			expectSave(mock, 1)

			if w := postTransforms(t, []map[string]any{consumerFilter()}); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// Rules that run on a packed row, and producer rules (the sink does not run
// them), need no lookup at all.
func TestSavePipelineTransforms_NoSourceLookupWhenEveryConsumerRuleRunsInTheDocument(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
	expectSave(mock, 3)

	w := postTransforms(t, []map[string]any{
		{"transform_type": "consumer", "enabled": true,
			"transform_config": map[string]any{"operation": "mask", "column": "email"}},
		{"transform_type": "consumer", "enabled": true,
			"transform_config": map[string]any{"operation": "type_convert", "column": "age", "to": "int"}},
		{"transform_type": "producer", "enabled": true,
			"transform_config": map[string]any{"operation": "filter", "condition": "id > 1"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// When the source cannot be read the save does not guess.
func TestSavePipelineTransforms_SourceLookupFailureSavesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
	mock.ExpectQuery(sourceShapeRe).WithArgs(idorPipelineID).WillReturnError(errors.New("connection reset"))

	if w := postTransforms(t, []map[string]any{consumerFilter()}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// Enabling a stored consumer filter on a MongoDB CDC pipeline is the other way
// to arm one; it is refused the same way and nothing is updated.
func TestUpdateTransform_RefusesEnablingAConsumerRuleOnMongoCDC(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(pipelineLookupRe).WithArgs(idorTransformID).
		WillReturnRows(sqlmock.NewRows([]string{"pipeline_id"}).AddRow(idorPipelineID))
	mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
	mock.ExpectQuery(`SELECT transform_config FROM transform_definitions WHERE id = \$1`).WithArgs(idorTransformID).
		WillReturnRows(sqlmock.NewRows([]string{"transform_config"}).AddRow([]byte(`{"operation":"filter","condition":"id > 1"}`)))
	mock.ExpectQuery(`SELECT transform_type, pipeline_id::text FROM transform_definitions WHERE id = \$1`).WithArgs(idorTransformID).
		WillReturnRows(sqlmock.NewRows([]string{"transform_type", "pipeline_id"}).AddRow("consumer", idorPipelineID))
	mock.ExpectQuery(sourceShapeRe).WithArgs(idorPipelineID).WillReturnRows(sourceShapeRows("mongodb", true))

	h := NewTransformHandler(db.GetDB(), "")
	r := wsScopeRouter(http.MethodPut, "/api/v1/transforms/:id", h.UpdateTransform)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/transforms/"+idorTransformID,
		bytes.NewReader(idorMustJSON(map[string]any{"enabled": true})))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "filter cannot run on a MongoDB CDC change") {
		t.Fatalf("status = %d, want 400 naming the rule: %s", w.Code, w.Body.String())
	}
	// No UPDATE expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A new config on a producer row is not the sink's concern.
func TestUpdateTransform_AllowsAProducerRuleOnMongoCDC(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(pipelineLookupRe).WithArgs(idorTransformID).
		WillReturnRows(sqlmock.NewRows([]string{"pipeline_id"}).AddRow(idorPipelineID))
	mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("member"))
	mock.ExpectQuery(`SELECT transform_type, pipeline_id::text FROM transform_definitions WHERE id = \$1`).WithArgs(idorTransformID).
		WillReturnRows(sqlmock.NewRows([]string{"transform_type", "pipeline_id"}).AddRow("producer", idorPipelineID))
	mock.ExpectExec(`UPDATE transform_definitions SET updated_at`).WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewTransformHandler(db.GetDB(), "")
	r := wsScopeRouter(http.MethodPut, "/api/v1/transforms/:id", h.UpdateTransform)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/transforms/"+idorTransformID,
		bytes.NewReader(idorMustJSON(map[string]any{"transform_config": map[string]any{"operation": "select", "columns": []any{"id"}}})))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// The create dialog offered array_expand on a MongoDB CDC pipeline and the save
// refused it. The GET says which consumer rule types such a pipeline runs, so a
// UI offers only those; elsewhere it says nothing, and every type is offered.
func TestGetPipelineTransforms_NamesTheConsumerRuleTypesOnlyOnMongoCDC(t *testing.T) {
	cases := []struct {
		src  string
		cdc  bool
		want []string
	}{
		{"mongodb", true, []string{"mask_pii", "type_convert"}},
		{"mongodb-atlas", true, []string{"mask_pii", "type_convert"}},
		{"mongodb", false, nil},
		{"postgresql", true, nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s cdc=%v", tc.src, tc.cdc), func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(gateQueryRe).WithArgs(idorPipelineID, wsScopeUser, wsScopeWS).WillReturnRows(gateRoleRows("viewer"))
			mock.ExpectQuery(`FROM transform_definitions\s+WHERE pipeline_id = \$1`).WithArgs(idorPipelineID).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "pipeline_id", "transform_type", "transform_order",
					"transform_config", "enabled", "created_at", "updated_at",
				}))
			mock.ExpectQuery(sourceShapeRe).WithArgs(idorPipelineID).WillReturnRows(sourceShapeRows(tc.src, tc.cdc))

			h := NewTransformHandler(db.GetDB(), "")
			r := wsScopeRouter(http.MethodGet, "/api/v1/transforms/pipeline/:pipeline_id", h.GetPipelineTransforms)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/transforms/pipeline/"+idorPipelineID, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			var body struct {
				ConsumerRuleTypes []string `json:"consumer_rule_types"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(body.ConsumerRuleTypes, tc.want) {
				t.Fatalf("consumer_rule_types = %v, want %v", body.ConsumerRuleTypes, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}
