package handlers

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Naming the tables in a chat message pre-selects them, which skips the
// table-selection pause the chat's transform step opens from. A run that starts
// now marks the pipeline transforms_review_pending so the orchestrator pauses
// once for that step; a schedule-only pipeline is not marked, because its first
// run is unattended and would park until someone opened the chat.
func TestPersistPreselectedTablesMarksTheTransformStep(t *testing.T) {
	const pipelineID = "66666666-6666-6666-6666-666666666666"
	update := regexp.QuoteMeta(`SET config = jsonb_set(COALESCE(config, '{}'::jsonb), '{selected_tables}', $2::jsonb, true) || $3::jsonb`)

	cases := []struct {
		name   string
		review bool
		mark   string
	}{
		{"run now: marked", true, `{"transforms_review_pending": true}`},
		{"schedule only: not marked", false, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			mock.ExpectExec(update).
				WithArgs(pipelineID, `["public.users","public.orders"]`, tc.mark).
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := persistPreselectedTables(db, pipelineID, []string{"public.users", "public.orders"}, tc.review); err != nil {
				t.Fatalf("persistPreselectedTables: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}
