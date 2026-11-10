package healer

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A pending dropped-table decision whose table turns out to be in the source —
// back in the list, or left out of it only because the connection is missing a
// setting — is withdrawn: Approve on it would drop a destination table whose
// source table exists (prod t-clickup list_task, 2026-10-05). Only a pending
// drop_table row moves.
func TestWithdrawPendingTableDrops(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const reason = "The table is back in the source, so nothing was dropped and it stays in this pipeline."
	mock.ExpectQuery(q("UPDATE schema_change_approvals")+"(.|\\n)*"+
		q("SET status = 'withdrawn'")+"(.|\\n)*"+
		q("WHERE pipeline_id = $1")+"(.|\\n)*"+
		q("table_name IN (SELECT jsonb_array_elements_text($2::jsonb))")+"(.|\\n)*"+
		q("change_type = 'drop_table'")+"(.|\\n)*"+
		q("status = 'pending'")+"(.|\\n)*"+
		q("RETURNING table_name")).
		WithArgs(routingPipelineID, `["list_task","team"]`, appliedBySchemaDrift, reason).
		WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("list_task"))

	got := WithdrawPendingTableDrops(context.Background(), db, routingPipelineID, []string{"list_task", "team"}, reason)
	if len(got) != 1 || got[0] != "list_task" {
		t.Errorf("withdrawn = %v, want [list_task]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Nothing to check, nothing asked of the database.
	if got := WithdrawPendingTableDrops(context.Background(), db, routingPipelineID, nil, reason); got != nil {
		t.Errorf("no tables: withdrawn = %v, want nil", got)
	}
	if got := WithdrawPendingTableDrops(context.Background(), nil, routingPipelineID, []string{"list_task"}, reason); got != nil {
		t.Errorf("no db: withdrawn = %v, want nil", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
