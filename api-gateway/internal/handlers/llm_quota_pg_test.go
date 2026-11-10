//go:build integration_pg

// Real-Postgres coverage for the LLM spend quota (migrations 057, 149): what
// sqlmock cannot judge. A call's exact cost, a fraction of a cent, is stored and
// charged as it is; check_and_charge_user still refuses a charge past the
// quota; an integer charge still resolves; and the pre-check reads the month's
// exact total against the whole-cent quota.
//
// Needs a Postgres with every api-gateway migration applied (the test applies
// them itself) and nothing else in it:
//
//	docker run -d --name quota-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=cplane \
//	    -p 55473:5432 postgres:16-alpine
//	QUOTA_PG_DSN='postgres://postgres:verify@localhost:55473/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_LLMQuota -v
package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"api-gateway/internal/db"
)

const pgQuotaUser = "55555555-aaaa-4000-8000-000000000001"

func TestPG_LLMQuotaChargesTheExactCostAndRoundsOnlyTheTotal(t *testing.T) {
	dsn := os.Getenv("QUOTA_PG_DSN")
	if dsn == "" {
		t.Skip("QUOTA_PG_DSN not set — see the file header")
	}
	d, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = d
	t.Cleanup(func() { db.DB = prev; _ = d.Close() })
	if err := db.Migrate("../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	text := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := d.QueryRow(q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	charge := func(q string, args ...any) bool {
		t.Helper()
		var ok bool
		if err := d.QueryRow(q, args...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return ok
	}
	exec(`DELETE FROM llm_usage_events WHERE user_id = $1`, pgQuotaUser)
	exec(`DELETE FROM users WHERE id = $1`, pgQuotaUser)
	exec(`INSERT INTO users (id, email, password_hash, cost_quota_usd_cents) VALUES ($1, 'quota@example.com', 'x', 1)`, pgQuotaUser)

	// The ledger row keeps the call's exact cost (llm-service llm_cost.py writes it).
	exec(`INSERT INTO llm_usage_events (user_id, prompt_name, provider, model, cost_usd_cents)
	      VALUES ($1, 'chat/help_response', 'openai', 'gpt-4o-mini', 0.00045)`, pgQuotaUser)
	if got := text(`SELECT cost_usd_cents::text FROM llm_usage_events WHERE user_id = $1`, pgQuotaUser); got != "0.000450" {
		t.Fatalf("ledger cost = %s, want 0.000450", got)
	}

	// Ten 0.00045¢ calls against a 1¢ quota: 0.0045¢ spent, not 10¢.
	for i := 0; i < 10; i++ {
		if !charge(`SELECT check_and_charge_user($1::uuid, $2::numeric)`, pgQuotaUser, "0.00045") {
			t.Fatalf("charge %d of 0.00045¢ refused under a 1¢ quota", i+1)
		}
	}
	if got := text(`SELECT monthly_cost_usd_cents::text FROM users WHERE id = $1`, pgQuotaUser); got != "0.004500" {
		t.Fatalf("spent = %s, want 0.004500", got)
	}
	if !checkUserCostQuotaOK(context.Background(), d, pgQuotaUser) {
		t.Fatal("pre-check blocked a user who spent 0.0045¢ of 1¢")
	}

	// Up to the quota exactly: allowed, and then the pre-check blocks.
	if !charge(`SELECT check_and_charge_user($1::uuid, $2::numeric)`, pgQuotaUser, "0.9955") {
		t.Fatal("a charge that reaches the quota exactly was refused")
	}
	if checkUserCostQuotaOK(context.Background(), d, pgQuotaUser) {
		t.Fatal("pre-check allowed a user at 1¢ of a 1¢ quota")
	}
	// Past the quota by a millionth of a cent: refused, nothing charged.
	if charge(`SELECT check_and_charge_user($1::uuid, $2::numeric)`, pgQuotaUser, "0.000001") {
		t.Fatal("a charge past the quota was allowed")
	}
	if got := text(`SELECT monthly_cost_usd_cents::text FROM users WHERE id = $1`, pgQuotaUser); got != "1.000000" {
		t.Fatalf("spent = %s after a refused charge, want 1.000000", got)
	}

	// An integer charge, from an llm-service that predates migration 149, resolves.
	exec(`UPDATE users SET cost_quota_usd_cents = 5 WHERE id = $1`, pgQuotaUser)
	if !charge(`SELECT check_and_charge_user($1::uuid, 2)`, pgQuotaUser) {
		t.Fatal("an integer charge under the quota was refused")
	}
	if got := text(`SELECT monthly_cost_usd_cents::text FROM users WHERE id = $1`, pgQuotaUser); got != "3.000000" {
		t.Fatalf("spent = %s, want 3.000000", got)
	}
	// One charge function: the whole-cent signature is gone.
	if got := text(`SELECT count(*)::text FROM pg_proc WHERE proname = 'check_and_charge_user'`); got != "1" {
		t.Fatalf("check_and_charge_user overloads = %s, want 1", got)
	}
}
