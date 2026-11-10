//go:build integration

// Integration test for migration 147 (dashboards-design.md § 14.2, decision 14): its SQL
// rewrites each version 1 chart in shared/chart_settings_v1_v2.json to the same version 2
// the gateway's v1 reader returns (internal/charts/settings_v1_test.go pins that side),
// in charts, in a dashboard draft and on both sides of a dashboard change; a second run
// changes nothing; and the connection's week start and fiscal month default to Monday
// and January and refuse anything out of range. Same gate as the 069 tests: the
// `integration` build tag plus MIGRATION_TEST_DSN (or TEST_DATABASE_URL).
//
//	docker run -d --rm --name pg147 -e POSTGRES_PASSWORD=pg -p 55435:5432 postgres:16-alpine
//	MIGRATION_TEST_DSN='postgres://postgres:pg@localhost:55435/postgres?sslmode=disable' \
//	  go test -tags=integration -run 'TestMigration147' ./internal/db/ -v
//	docker rm -f pg147
package db

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type settingsPair147 struct {
	Name string          `json:"name"`
	V1   json.RawMessage `json:"v1"`
	V2   json.RawMessage `json:"v2"`
}

func readSettingsPairs147(t *testing.T) []settingsPair147 {
	t.Helper()
	raw, err := os.ReadFile("../../../shared/chart_settings_v1_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []settingsPair147 `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 10 {
		t.Fatalf("%d cases; every v1 field needs one", len(f.Cases))
	}
	return f.Cases
}

// exec147 runs migration 147's body directly, so the test seeds version 1 rows first.
func exec147(t *testing.T, conn *sql.DB) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(migrationsDir, "147_*.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one 147_*.sql, got %v (%v)", matches, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(string(body)); err != nil {
		t.Fatalf("exec 147: %v", err)
	}
}

// sameJSONB says whether Postgres reads a and b as the same value.
func sameJSONB(t *testing.T, conn *sql.DB, a, b string) bool {
	t.Helper()
	return qBool(t, conn, `SELECT $1::jsonb = $2::jsonb`, a, b)
}

func TestMigration147_RewritesEachSavedChartToVersion2(t *testing.T) {
	conn, cleanup := freshSchema(t, testDSN(t))
	t.Cleanup(cleanup)
	if err := Migrate(migrationsDir); err != nil {
		t.Fatalf("full Migrate() including 147 failed on a fresh DB: %v", err)
	}
	user := qStr(t, conn, `INSERT INTO users (email, password_hash) VALUES ('c147@acme.test','x') RETURNING id`)
	ws := qStr(t, conn, `INSERT INTO workspaces (name, slug, owner_id) VALUES ('c147','c147',$1) RETURNING id`, user)
	dash := qStr(t, conn, `INSERT INTO dashboards (workspace_id, name) VALUES ($1, 'd') RETURNING id`, ws)

	pairs := readSettingsPairs147(t)
	chartIDs := make([]string, len(pairs))
	var draftCharts []json.RawMessage
	changeIDs := make([]string, len(pairs))
	for i, p := range pairs {
		chartIDs[i] = qStr(t, conn, `INSERT INTO charts (workspace_id, name, settings, settings_version, updated_at)
			VALUES ($1, $2, $3, 1, '2026-01-02T03:04:05Z') RETURNING id`, ws, p.Name, string(p.V1))
		c, _ := json.Marshal(map[string]any{"name": p.Name, "source_id": "s", "type": "t", "settings": p.V1})
		draftCharts = append(draftCharts, c)
		change, _ := json.Marshal(map[string]any{"dashboard_name": "d",
			"before": map[string]any{"name": p.Name, "type": "t", "settings": p.V1, "x": 0, "y": 0, "w": 6, "h": 4},
			"after":  map[string]any{"name": p.Name, "type": "t", "settings": p.V1, "x": 0, "y": 0, "w": 6, "h": 4},
			"moves":  []any{}})
		changeIDs[i] = qStr(t, conn, `INSERT INTO dashboard_changes (workspace_id, dashboard_id, tile_id, chart_id,
			created_by, actor_kind, base_updated_at, change, expires_at)
			VALUES ($1, $2, gen_random_uuid(), $3, $4, 'assistant', now(), $5, now() + interval '1 day') RETURNING id`,
			ws, dash, chartIDs[i], user, string(change))
	}
	// A draft chart with no settings is left as it is.
	draftCharts = append(draftCharts, json.RawMessage(`{"name":"none","source_id":"s","type":"t"}`))
	proposal, _ := json.Marshal(map[string]any{"name": "d", "charts": draftCharts, "filters": []any{}, "tiles": []any{}})
	draft := qStr(t, conn, `INSERT INTO dashboard_proposals (workspace_id, created_by, actor_kind, proposal, expires_at)
		VALUES ($1, $2, 'assistant', $3, now() + interval '1 day') RETURNING id`, ws, user, string(proposal))

	// Control: before the rewrite no row is version 2, so the checks below can fail.
	for i, p := range pairs {
		if got := qStr(t, conn, `SELECT settings::text FROM charts WHERE id = $1`, chartIDs[i]); sameJSONB(t, conn, got, string(p.V2)) {
			t.Fatalf("%s: already version 2 before the migration ran", p.Name)
		}
	}

	for run := 1; run <= 2; run++ {
		exec147(t, conn)
		for i, p := range pairs {
			got := qStr(t, conn, `SELECT settings::text FROM charts WHERE id = $1`, chartIDs[i])
			if !sameJSONB(t, conn, got, string(p.V2)) {
				t.Errorf("run %d, chart %q = %s, want %s", run, p.Name, got, p.V2)
			}
			if v := qInt(t, conn, `SELECT settings_version FROM charts WHERE id = $1`, chartIDs[i]); v != 2 {
				t.Errorf("run %d, chart %q settings_version = %d", run, p.Name, v)
			}
			// A pending change compares updated_at to the chart it was drafted on.
			if !qBool(t, conn, `SELECT updated_at = '2026-01-02T03:04:05Z' FROM charts WHERE id = $1`, chartIDs[i]) {
				t.Errorf("run %d, chart %q updated_at moved", run, p.Name)
			}
			got = qStr(t, conn, `SELECT proposal -> 'charts' -> $2::int -> 'settings' FROM dashboard_proposals WHERE id = $1`, draft, i)
			if !sameJSONB(t, conn, got, string(p.V2)) {
				t.Errorf("run %d, draft chart %q = %s, want %s", run, p.Name, got, p.V2)
			}
			for _, side := range []string{"before", "after"} {
				got = qStr(t, conn, `SELECT change -> $2::text -> 'settings' FROM dashboard_changes WHERE id = $1`, changeIDs[i], side)
				if !sameJSONB(t, conn, got, string(p.V2)) {
					t.Errorf("run %d, change %s %q = %s, want %s", run, side, p.Name, got, p.V2)
				}
			}
		}
		if got := qStr(t, conn, `SELECT proposal -> 'charts' -> -1 FROM dashboard_proposals WHERE id = $1`, draft); !sameJSONB(t, conn, got,
			`{"name":"none","source_id":"s","type":"t"}`) {
			t.Errorf("run %d, the draft chart with no settings became %s", run, got)
		}
	}
	// The migration drops its functions.
	if n := qInt(t, conn, `SELECT count(*) FROM pg_proc WHERE proname IN ('chart_settings_v2', 'chart_field_v2')`); n != 0 {
		t.Errorf("%d migration functions left behind", n)
	}
}

func TestMigration147_ConnectionCalendarDefaultsAndRange(t *testing.T) {
	conn, cleanup := freshSchema(t, testDSN(t))
	t.Cleanup(cleanup)
	if err := Migrate(migrationsDir); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	user := qStr(t, conn, `INSERT INTO users (email, password_hash) VALUES ('k147@acme.test','x') RETURNING id`)
	ws := qStr(t, conn, `INSERT INTO workspaces (name, slug, owner_id) VALUES ('k147','k147',$1) RETURNING id`, user)
	id := qStr(t, conn, `INSERT INTO connections (user_id, workspace_id, name, type, connector_type, config)
		VALUES ($1, $2, 'pg', 'source', 'postgresql', '{}') RETURNING id`, user, ws)
	if w, f := qInt(t, conn, `SELECT week_start FROM connections WHERE id = $1`, id),
		qInt(t, conn, `SELECT fiscal_year_start_month FROM connections WHERE id = $1`, id); w != 1 || f != 1 {
		t.Fatalf("defaults = week %d, fiscal month %d; want Monday (1) and January (1)", w, f)
	}
	for _, q := range []string{
		`UPDATE connections SET week_start = 7, fiscal_year_start_month = 12 WHERE id = $1`,
		`UPDATE connections SET week_start = 1, fiscal_year_start_month = 1 WHERE id = $1`,
	} {
		if _, err := conn.Exec(q, id); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`UPDATE connections SET week_start = 0 WHERE id = $1`,
		`UPDATE connections SET week_start = 8 WHERE id = $1`,
		`UPDATE connections SET fiscal_year_start_month = 0 WHERE id = $1`,
		`UPDATE connections SET fiscal_year_start_month = 13 WHERE id = $1`,
	} {
		if _, err := conn.Exec(q, id); err == nil {
			t.Errorf("%s was accepted", q)
		}
	}
}
