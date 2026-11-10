package handlers

import (
	"context"
	"database/sql/driver"
	"net/http"
	"strings"
	"testing"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"api-gateway/internal/security"

	"github.com/DATA-DOG/go-sqlmock"
)

// workflows-design.md § 2: a model's place after a pipeline or another model
// belongs to the model (its after_upstream trigger), and a workflow never
// draws model → model. A refresh_model step right after another, or one for a
// model the workflow's own pipeline or model already rebuilds through that
// trigger, rebuilt the model twice and saved without a word.

const (
	ownPipe   = "44444444-0000-4000-8000-000000000001"
	ownPipe2  = "44444444-0000-4000-8000-000000000002"
	ownModelA = "55555555-0000-4000-8000-00000000000a"
	ownModelB = "55555555-0000-4000-8000-00000000000b"
	ownModelW = "55555555-0000-4000-8000-00000000000c"
)

const ownAncestorsSQL = `WITH RECURSIVE up\(kind, id, depth\)`

func ownRefresh(id, model string, after ...string) workflow.Step {
	return workflow.Step{ID: id, Type: workflow.StepRefreshModel, ModelID: model, After: after}
}

func ownRun(id, pipe string, after ...string) workflow.Step {
	return workflow.Step{ID: id, Type: workflow.StepRunPipeline, PipelineID: pipe, After: after}
}

func ownDef(trigger workflow.Trigger, steps ...workflow.Step) workflow.Definition {
	var last []string
	for _, s := range steps {
		last = append(last, s.ID)
	}
	steps = append(steps, workflow.Step{ID: "tell", Type: workflow.StepSend, After: last, Rule: workflow.RuleAllDone,
		Message: &workflow.Message{Subject: "Done", Body: "The models are rebuilt."}})
	return workflow.Definition{Version: 3, Trigger: trigger, Steps: steps}
}

func expectOwnModel(mock sqlmock.Sqlmock, model string) {
	mock.ExpectQuery(`SELECT 1 FROM saved_queries`).WithArgs(model, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
}

func expectOwnPipe(mock sqlmock.Sqlmock, pipe, name string) {
	mock.ExpectQuery(`SELECT name, source_connection_id::text, sync_mode, cdc_mode FROM pipelines`).
		WithArgs(pipe, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "src", "sync_mode", "cdc_mode"}).AddRow(name, nil, "batch", nil))
}

// expectOwnAncestors answers the upstreams a model rebuilds after, through
// its own trigger and those of the models above it: rows of kind, id, name,
// depth, and the model's own name.
func expectOwnAncestors(mock sqlmock.Sqlmock, model string, rows ...[]any) {
	r := sqlmock.NewRows([]string{"kind", "id", "name", "depth", "model_name"})
	for _, row := range rows {
		vals := make([]driver.Value, len(row))
		for i, v := range row {
			vals[i] = v
		}
		r.AddRow(vals...)
	}
	mock.ExpectQuery(ownAncestorsSQL).WithArgs(model, wsScopeWS, maxTriggerChainDepth).WillReturnRows(r)
}

func TestValidateWorkflowDefinition_ModelOwnership(t *testing.T) {
	manual := workflow.Trigger{Type: workflow.TriggerManual}
	afterOrders := workflow.Trigger{Type: workflow.TriggerAfterPipeline, PipelineID: ownPipe}
	for _, tc := range []struct {
		name   string
		def    workflow.Definition
		expect func(sqlmock.Sqlmock)
		code   string
		step   string
		want   []string
	}{
		// B4: model → model.
		{name: "a refresh straight after a refresh",
			def: ownDef(manual, ownRefresh("stage", ownModelA), ownRefresh("publish", ownModelB, "stage")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
				expectOwnModel(m, ownModelB)
			},
			code: "model_after_model", step: "publish",
			want: []string{"Step publish refreshes a model right after step stage refreshes one",
				"schedule to rebuild after the first", "remove step publish"}},
		{name: "a refresh after a refresh, through a wait",
			def: ownDef(manual, ownRefresh("stage", ownModelA),
				workflow.Step{ID: "settle", Type: workflow.StepWait, Minutes: 5, After: []string{"stage"}},
				ownRefresh("publish", ownModelB, "settle")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
				expectOwnModel(m, ownModelB)
			},
			code: "model_after_model", step: "publish", want: []string{"right after step stage"}},
		{name: "two refreshes side by side, neither following the other",
			def: ownDef(manual, ownRefresh("a", ownModelA), ownRefresh("b", ownModelB)),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
				expectOwnModel(m, ownModelB)
				expectOwnAncestors(m, ownModelA)
				expectOwnAncestors(m, ownModelB)
			}},
		{name: "a refresh, a pipeline, then a refresh",
			def: ownDef(manual, ownRefresh("a", ownModelA), ownRun("load", ownPipe, "a"), ownRefresh("b", ownModelB, "load")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
				expectOwnPipe(m, ownPipe, "orders")
				expectOwnModel(m, ownModelB)
				expectOwnAncestors(m, ownModelA, []any{"pipeline", ownPipe2, "customers", 1, "revenue"})
				expectOwnAncestors(m, ownModelB)
			}},

		// B3: an edge the model's own trigger already owns.
		{name: "the model already rebuilds after the pipeline the workflow runs after",
			def: ownDef(afterOrders, ownRefresh("rebuild", ownModelA)),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
				m.ExpectQuery(`SELECT name FROM pipelines WHERE id = \$1 AND workspace_id = \$2`).
					WithArgs(ownPipe, wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("orders"))
				expectOwnAncestors(m, ownModelA, []any{"pipeline", ownPipe, "orders", 1, "revenue"})
			},
			code: "model_already_follows", step: "rebuild",
			want: []string{`Step rebuild: model "revenue" already rebuilds after the pipeline "orders" finishes, ` +
				`through its own trigger`, "this workflow runs after it", "would rebuild twice",
				"Remove step rebuild, or change the model's trigger."}},
		{name: "the model already rebuilds after a pipeline a step runs",
			def: ownDef(manual, ownRun("load", ownPipe), ownRefresh("rebuild", ownModelA, "load")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnPipe(m, ownPipe, "orders")
				expectOwnModel(m, ownModelA)
				expectOwnAncestors(m, ownModelA, []any{"pipeline", ownPipe, "orders", 1, "revenue"})
			},
			code: "model_already_follows", step: "rebuild",
			want: []string{`after the pipeline "orders" finishes`, "step load runs it"}},
		{name: "the model rebuilds after that pipeline through a model between",
			def: ownDef(manual, ownRun("load", ownPipe), ownRefresh("rebuild", ownModelA, "load")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnPipe(m, ownPipe, "orders")
				expectOwnModel(m, ownModelA)
				expectOwnAncestors(m, ownModelA,
					[]any{"model", ownModelW, "orders_clean", 1, "revenue"},
					[]any{"pipeline", ownPipe, "orders", 2, "revenue"})
			},
			code: "model_already_follows", step: "rebuild",
			want: []string{`after the pipeline "orders" finishes, through the triggers of the models between them`}},
		{name: "the model already rebuilds after a model another step refreshes",
			def: ownDef(manual, ownRefresh("clean", ownModelW), ownRefresh("rebuild", ownModelA)),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelW)
				expectOwnModel(m, ownModelA)
				expectOwnAncestors(m, ownModelW)
				expectOwnAncestors(m, ownModelA, []any{"model", ownModelW, "orders_clean", 1, "revenue"})
			},
			code: "model_already_follows", step: "rebuild",
			want: []string{`after the model "orders_clean" finishes`, "step clean refreshes it"}},
		{name: "the model rebuilds after another pipeline only",
			def: ownDef(manual, ownRun("load", ownPipe), ownRefresh("rebuild", ownModelA, "load")),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnPipe(m, ownPipe, "orders")
				expectOwnModel(m, ownModelA)
				expectOwnAncestors(m, ownModelA, []any{"pipeline", ownPipe2, "customers", 1, "revenue"})
			}},
		{name: "a refresh with nothing else in the workflow asks nothing more",
			def: ownDef(manual, ownRefresh("rebuild", ownModelA)),
			expect: func(m sqlmock.Sqlmock) {
				expectOwnModel(m, ownModelA)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.MatchExpectationsInOrder(false)
			tc.expect(mock)
			def := tc.def

			_, _, refusal, err := validateWorkflowDefinition(context.Background(), database, wsScopeWS, wsScopeUser,
				&def, security.WSAdmin, false)
			if err != nil {
				t.Fatal(err)
			}
			mockMet(t, mock)
			if tc.code == "" {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal)
				}
				return
			}
			if refusal == nil || refusal.Code != tc.code || refusal.Status != http.StatusUnprocessableEntity ||
				refusal.Step != tc.step {
				t.Fatalf("refusal = %+v, want %s 422 on step %s", refusal, tc.code, tc.step)
			}
			for _, s := range tc.want {
				if !strings.Contains(refusal.Message, s) {
					t.Errorf("message %q lacks %q", refusal.Message, s)
				}
			}
		})
	}
}
