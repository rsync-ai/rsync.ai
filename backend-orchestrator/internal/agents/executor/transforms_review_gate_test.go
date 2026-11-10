package executor

// A chat pipeline whose tables were named in the message never paused for table
// selection, so the chat's transform step (which opens from that pause) was
// never shown and data moved with no transforms. The gateway marks such a
// pipeline with config.transforms_review_pending; the first transfer pauses
// once, with the named tables, and clears the mark in the same statement.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const reviewPipelineID = "55555555-5555-5555-5555-555555555555"

const consumeReviewQuery = `UPDATE pipelines\s+SET config = config - 'transforms_review_pending',\s+updated_at = NOW\(\)\s+WHERE id = \$1\s+AND config->'transforms_review_pending' = 'true'::jsonb\s+RETURNING id`

func reviewPGTask(params map[string]interface{}) ExecutorTask {
	return ExecutorTask{
		TaskID:      "task-review",
		PipelineID:  reviewPipelineID,
		Operation:   "data_transfer",
		Source:      &ConnectorConfig{Type: "postgresql", Config: map[string]string{"host": "db.internal", "database": "appdb"}},
		Destination: &ConnectorConfig{Type: "gcs", Config: map[string]string{"bucket": "lake"}},
		Params:      params,
	}
}

func TestConsumeTransformsReview(t *testing.T) {
	cases := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
		want   bool
	}{
		{"mark set: cleared and reported", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(consumeReviewQuery).WithArgs(reviewPipelineID).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(reviewPipelineID))
		}, true},
		// The second dispatch after the resume, a rerun, a scheduled run.
		{"mark absent", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(consumeReviewQuery).WithArgs(reviewPipelineID).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
		}, false},
		// A review step, not a safety control: a DB error moves the data as before.
		{"db error", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(consumeReviewQuery).WithArgs(reviewPipelineID).
				WillReturnError(errors.New("connection reset"))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			tc.expect(mock)
			a := &Agent{db: db}
			if got := a.consumeTransformsReview(context.Background(), "  "+reviewPipelineID+" "); got != tc.want {
				t.Fatalf("consumeTransformsReview = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}

	t.Run("no db or a non-UUID id: no query", func(t *testing.T) {
		if (&Agent{}).consumeTransformsReview(context.Background(), reviewPipelineID) {
			t.Fatal("an Agent with no DB reported a pending review")
		}
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		if (&Agent{db: db}).consumeTransformsReview(context.Background(), "pl-not-a-uuid") {
			t.Fatal("a non-UUID pipeline id reported a pending review")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

func TestTransformsReviewPauseOffersOnlyTheNamedTables(t *testing.T) {
	tables := []string{"public.users", "public.orders"}
	resp := transformsReviewPause(reviewPGTask(nil), tables)

	if resp.Status != "waiting_for_table_selection" {
		t.Fatalf("status = %q, want the table-selection wait the chat opens its transform step from", resp.Status)
	}
	if resp.TaskID != "task-review" || resp.PipelineID != reviewPipelineID {
		t.Fatalf("ids = %q / %q", resp.TaskID, resp.PipelineID)
	}
	if resp.Result["review_transforms_only"] != true {
		t.Fatalf("review_transforms_only = %#v, want true", resp.Result["review_transforms_only"])
	}
	if !reflect.DeepEqual(resp.Result["preselected_tables"], tables) {
		t.Fatalf("preselected_tables = %#v, want %v", resp.Result["preselected_tables"], tables)
	}
	// A client that does not know review_transforms_only shows a picker holding
	// exactly the named tables, never the whole source.
	opts, _ := resp.Result["available_tables"].([]map[string]interface{})
	var names []string
	for _, o := range opts {
		names = append(names, o["name"].(string))
	}
	if !reflect.DeepEqual(names, tables) {
		t.Fatalf("available_tables names = %v, want %v", names, tables)
	}
	// The pause result must not alias the caller's slice.
	tables[0] = "changed"
	if resp.Result["preselected_tables"].([]string)[0] != "public.users" {
		t.Fatal("preselected_tables aliases the caller's slice")
	}
}

// The wiring: a pipeline carrying the mark pauses AFTER planNLTransforms (a
// "mask email" in the message is stored first) and before any data moves.
func TestDataTransferPausesOnceForTheTransformStep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT config->'nl_transforms' FROM pipelines").
		WithArgs(reviewPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"nl_transforms"}).AddRow([]byte("null")))
	mock.ExpectQuery(consumeReviewQuery).WithArgs(reviewPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(reviewPipelineID))

	a := &Agent{db: db}
	resp, reached := runPastGuard(func() ExecutorResponse {
		return a.executeDataTransfer(context.Background(), reviewPGTask(map[string]interface{}{
			"sync_mode":       "cdc",
			"selected_tables": []interface{}{"public.users"},
		}))
	})
	if reached != nil {
		t.Fatalf("a pipeline marked for the transform step went on into the data plane (panic: %v)", reached)
	}
	if resp.Status != "waiting_for_table_selection" || resp.Result["review_transforms_only"] != true {
		t.Fatalf("response = %q / %#v, want the one-time transform-step pause", resp.Status, resp.Result)
	}
	if !reflect.DeepEqual(resp.Result["preselected_tables"], []string{"public.users"}) {
		t.Fatalf("preselected_tables = %#v, want [public.users]", resp.Result["preselected_tables"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations (planNLTransforms must run before the pause): %v", err)
	}
}
