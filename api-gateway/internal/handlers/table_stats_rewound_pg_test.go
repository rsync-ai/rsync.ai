//go:build integration_pg

// Real-Postgres proof for the "Rewound" flag (migration 141): the table-stats API
// joins the orchestrator's rewind record at read time. sqlmock pins the statements
// (table_stats_rewound_test.go); only a server can say the EXISTS type-checks
// (a uuid execution_id against the record's text one), works inside a FILTER, and
// matches a record on the right run and table name only.
//
//	docker run -d --name rewind-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55471:5432 postgres:16-alpine
//	REWIND_PG_DSN='postgres://postgres:verify@localhost:55471/pipeline_db?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PGTableStatsRewound -v
package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgRwUser      = "dddddddd-1410-4000-8000-000000000001"
	pgRwWS        = "dddddddd-1410-4000-8000-000000000002"
	pgRwPipe      = "dddddddd-1410-4000-8000-000000000003"
	pgRwExec      = "dddddddd-1410-4000-8000-000000000004"
	pgRwOtherExec = "dddddddd-1410-4000-8000-000000000005"
)

func TestPGTableStatsRewound(t *testing.T) {
	dsn := os.Getenv("REWIND_PG_DSN")
	if dsn == "" {
		t.Skip("REWIND_PG_DSN not set — see the file header")
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
	purge := func() {
		wfExec(t, d, `DELETE FROM pipelines WHERE id = $1`, pgRwPipe)
		wfExec(t, d, `DELETE FROM workspaces WHERE id = $1`, pgRwWS)
		wfExec(t, d, `DELETE FROM users WHERE id = $1`, pgRwUser)
	}
	purge()
	t.Cleanup(purge)

	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'rewound-verify@example.invalid', 'x')`, pgRwUser)
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Rewound', 'rewound-pg', $2)`, pgRwWS, pgRwUser)
	wfExec(t, d, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'viewer')
	              ON CONFLICT DO NOTHING`, pgRwWS, pgRwUser)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode)
	              VALUES ($1, 'clickup to mongo', 'x', $2, 'batch')`, pgRwPipe, pgRwWS)
	// One run, four tables:
	//   list_task    short of rows, rewound (recorded by its qualified name)
	//   t            short of rows, rewound (the sink qualified a bare name: recorded as table_name)
	//   list_comment short of rows with dead-lettered rows, NOT rewound by this run
	//                (an earlier run's record must not count)
	//   space        complete; the rewind put it back too, but a complete table is not "Rewound"
	wfExec(t, d, `INSERT INTO pipeline_run_table_stats
		(pipeline_id, execution_id, table_name, qualified_name, mode, status, read_rows, inserted_rows, dlq_rows)
		VALUES
		($1, $2, 'list_task',    'list_task',    'batch', 'degraded', 1250, 1000, 0),
		($1, $2, 't',            'db.t',         'batch', 'degraded', 10,   4,    0),
		($1, $2, 'list_comment', 'list_comment', 'batch', 'degraded', 10,   5,    5),
		($1, $2, 'space',        'space',        'batch', 'completed', 3,   3,    0)`, pgRwPipe, pgRwExec)
	wfExec(t, d, `INSERT INTO pipeline_run_rewinds (pipeline_id, execution_id, source_table) VALUES
		($1, $2, 'list_task'), ($1, $2, 't'), ($1, $2, 'space'), ($1, $3, 'list_comment')`,
		pgRwPipe, pgRwExec, pgRwOtherExec)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", pgRwUser)
		c.Set(ctxWorkspaceID, pgRwWS)
		c.Set(ctxWorkspaceRole, "viewer")
		c.Next()
	})
	r.GET("/pipelines/:id/table-stats", GetPipelineTableStats)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pipelines/"+pgRwPipe+"/table-stats?execution_id="+pgRwExec, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET table-stats: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Summary TableStatsSummary `json:"summary"`
		Tables  []TableStat       `json:"tables"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tables) != 4 {
		t.Fatalf("listed %d tables, want 4 (a scan error drops rows): %s", len(resp.Tables), w.Body.String())
	}
	want := map[string]bool{"list_task": true, "db.t": true, "list_comment": false, "space": true}
	for _, s := range resp.Tables {
		if s.Rewound != want[s.QualifiedName] {
			t.Errorf("%s: rewound=%v, want %v", s.QualifiedName, s.Rewound, want[s.QualifiedName])
		}
	}
	if resp.Summary.TablesDegraded != 3 || resp.Summary.TablesRewound != 2 {
		t.Errorf("summary tables_degraded=%d tables_rewound=%d, want 3 and 2",
			resp.Summary.TablesDegraded, resp.Summary.TablesRewound)
	}
}
