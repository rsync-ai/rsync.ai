package handlers

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A table picked at a DAG node's input request ran, but was never saved:
// pipelines.config.selected_tables stayed NULL on prod (2026-09-30, pipeline
// ecd5d308), so the next run asked again and the post-run check had no list to
// compare against. The linear table step saves its pick; the node answer must too.
// Bug class: one answer, two resume paths, only one of them persists it.

const nodeInputPipeline = "55555555-5555-5555-5555-555555555555"

func TestNodeInputTablePickIsSavedToThePipeline(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer database.Close()

	mock.ExpectExec(`'\{selected_tables\}'`).
		WithArgs(`["datingapp.matches"]`, nodeInputPipeline).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipelines`).
		WithArgs(sqlmock.AnyArg(), nodeInputPipeline).
		WillReturnResult(sqlmock.NewResult(0, 1))

	saved := persistNodeInputTableSelection(database, nodeInputPipeline, map[string]interface{}{
		"selected_tables":    []interface{}{"datingapp.matches"},
		"destination_config": map[string]interface{}{"schema": "demo"},
	})
	if len(saved) != 1 || saved[0] != "datingapp.matches" {
		t.Fatalf("saved = %v, want [datingapp.matches]", saved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// An answer that picks no tables (a free-text reply, or a patch for another
// setting) leaves the pipeline's saved selection alone.
func TestNodeInputWithoutATablePickSavesNothing(t *testing.T) {
	for name, patch := range map[string]map[string]interface{}{
		"no patch":        nil,
		"no tables key":   {"destination_config": map[string]interface{}{"schema": "demo"}},
		"empty list":      {"selected_tables": []interface{}{}},
		"blank names":     {"selected_tables": []interface{}{" ", ""}},
		"not a list":      {"selected_tables": "datingapp.matches"},
		"non-string item": {"selected_tables": []interface{}{"datingapp.matches", 7}},
	} {
		t.Run(name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer database.Close()
			// sqlmock does not flag a statement nobody expected, so the returned
			// list is what proves nothing was written.
			if saved := persistNodeInputTableSelection(database, nodeInputPipeline, patch); saved != nil {
				t.Fatalf("saved = %v, want nothing", saved)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("SQL: %v", err)
			}
		})
	}
}
