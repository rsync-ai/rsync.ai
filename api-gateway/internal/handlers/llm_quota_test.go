package handlers

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The month's spend is NUMERIC(14,6) since migration 149, and Postgres sends it
// as "0.004500", which an int scan rejects. The pre-check fails open on a scan
// error, so without the cast to whole cents the quota never blocks a run.
// llm_quota_pg_test.go shows that on a real Postgres; CI has none for Go, so
// this pins the cast and what the pre-check does with what it reads.
func TestThePreCheckReadsTheExactSpendAsWholeCents(t *testing.T) {
	for _, tc := range []struct {
		name         string
		quota, spent int
		want         bool
	}{
		{"under the quota", 1, 0, true},
		{"at the quota", 1, 1, false},
		{"unmetered", 0, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			mock.ExpectQuery(`THEN FLOOR\(monthly_cost_usd_cents\)::int`).
				WithArgs("u1").
				WillReturnRows(sqlmock.NewRows([]string{"cost_quota_usd_cents", "current_cents", "monthly_cost_period"}).
					AddRow(tc.quota, tc.spent, "2026-10"))

			if got := checkUserCostQuotaOK(context.Background(), d, "u1"); got != tc.want {
				t.Fatalf("checkUserCostQuotaOK = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("the pre-check must read the spend through FLOOR(...)::int: %v", err)
			}
		})
	}
}
