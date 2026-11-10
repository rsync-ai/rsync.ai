package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	"go.temporal.io/sdk/client"
)

// The writes a browser action makes after the caller's tab may already be closed: an
// answer to a paused run, a model delete's Temporal clean-up, a schedule create's
// commit, the first-run namespace lock and an OAuth refresh. Each one used to run on
// the request's context, so the hang-up cancelled it half done.

// ---------------------------------------------------------------------------
// a Temporal schedule client that notices a cancelled context
// ---------------------------------------------------------------------------

type scheduleFakeTemporal struct {
	client.Client // nil: anything not faked here panics
	sc            *scheduleFakeClient
}

func (f *scheduleFakeTemporal) ScheduleClient() client.ScheduleClient { return f.sc }

type scheduleFakeClient struct {
	client.ScheduleClient

	// hangUp closes the caller's tab the moment Temporal has been reached.
	hangUp func()

	mu      sync.Mutex
	created []string
	deleted []string
}

func (s *scheduleFakeClient) hangUpNow() {
	if s.hangUp != nil {
		s.hangUp()
	}
}

// Create accepts the schedule, then the tab closes while Temporal's answer is on its
// way back.
func (s *scheduleFakeClient) Create(ctx context.Context, opts client.ScheduleOptions) (client.ScheduleHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.created = append(s.created, opts.ID)
	s.mu.Unlock()
	s.hangUpNow()
	return &scheduleFakeHandle{s: s, id: opts.ID}, nil
}

func (s *scheduleFakeClient) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	s.hangUpNow()
	return &scheduleFakeHandle{s: s, id: id}
}

func (s *scheduleFakeClient) snapshot() (created, deleted []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.created...), append([]string(nil), s.deleted...)
}

type scheduleFakeHandle struct {
	client.ScheduleHandle
	s  *scheduleFakeClient
	id string
}

// Delete gives up on a cancelled context, as the real client does.
func (h *scheduleFakeHandle) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.s.mu.Lock()
	h.s.deleted = append(h.s.deleted, h.id)
	h.s.mu.Unlock()
	return nil
}

func installScheduleTemporal(t *testing.T, hangUp func()) *scheduleFakeClient {
	t.Helper()
	sc := &scheduleFakeClient{hangUp: hangUp}
	temporalMu.Lock()
	prev := temporalClient
	temporalClient = &scheduleFakeTemporal{sc: sc}
	temporalMu.Unlock()
	t.Cleanup(func() {
		temporalMu.Lock()
		temporalClient = prev
		temporalMu.Unlock()
	})
	return sc
}

// ---------------------------------------------------------------------------
// HITL answers
// ---------------------------------------------------------------------------

// A tab closed as the answer left for the run failed the signal, so a user who had
// picked their tables saw the run go on waiting for them.
func TestHITLResume_AnAnswerOutlivesTheCaller(t *testing.T) {
	for _, e := range hitlEndpoints {
		t.Run(e.name, func(t *testing.T) {
			newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.beforeSignal = cancel

			w := e.callWithContext(t, ctx, hitlTestExec)

			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
			}
			if got := tc.delivered(); len(got) != 1 || !strings.HasPrefix(got[0], hitlTestExec+" ") {
				t.Fatalf("the answer never reached the run after the tab closed: %v", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// model delete
// ---------------------------------------------------------------------------

// A tab closed after the delete committed cancelled the Temporal clean-up, leaving the
// model's schedule firing with no row behind it.
func TestDeleteSavedQuery_RemovesItsTemporalScheduleAfterTheCallerHangsUp(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := installScheduleTemporal(t, cancel)

	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))
	mock.ExpectQuery(`FROM saved_queries sq\s+LEFT JOIN saved_query_schedules s`).
		WithArgs(savedQueryID).
		WillReturnRows(savedQueryRows(wsScopeUser, "workspace", "SELECT 1", "read"))
	mock.ExpectQuery(`SELECT temporal_schedule_id FROM saved_query_schedules WHERE saved_query_id = \$1 AND temporal_schedule_id IS NOT NULL`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_schedule_id"}).AddRow("ts-1"))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_query_schedules s`).
		WithArgs(savedQueryID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM saved_queries WHERE id = \$1`).
		WithArgs(savedQueryID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	r := savedQueryRouter(http.MethodDelete, "/explorer/saved/:id", "admin", DeleteSavedQuery)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/explorer/saved/"+savedQueryID, nil)
	r.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if _, deleted := sc.snapshot(); len(deleted) != 1 || deleted[0] != "ts-1" {
		t.Fatalf("the deleted model's Temporal schedule was left running: deleted=%v", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// schedule create
// ---------------------------------------------------------------------------

// A tab closed after Temporal accepted the schedule cancelled the transaction. The
// commit failed, the clean-up was skipped, and the schedule ran with no row behind it:
// invisible in the UI and impossible to delete from it.
func TestCreatePipelineSchedule_CommitsAfterTheCallerHangsUp(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := installScheduleTemporal(t, cancel)

	mock.ExpectQuery(`SELECT wm\.role\s+FROM pipelines r`).
		WithArgs(wsScopePipeline, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	mock.ExpectQuery(`SELECT p\.sync_mode, p\.cdc_mode, sc\.sync_mode`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"sync_mode", "cdc_mode", "source_sync_mode"}).AddRow("batch", nil, nil))
	mock.ExpectQuery(`SELECT config FROM pipelines WHERE id = \$1`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow([]byte(`{"selected_tables":["public.orders"]}`)))
	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM pipeline_schedules`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO pipeline_schedules`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	now := time.Now()
	mock.ExpectQuery(`FROM pipeline_schedules\s+WHERE schedule_id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{
			"schedule_id", "pipeline_id", "schedule_type", "schedule_spec", "temporal_schedule_id",
			"status", "created_by", "created_at", "updated_at", "paused_at", "paused_reason",
		}).AddRow("sched-1", wsScopePipeline, "interval", []byte(`{"every_seconds":3600}`), "sched-1",
			"active", wsScopeUser, now, now, nil, nil))

	r := wsScopeRouterAsRole(http.MethodPost, "/api/v1/pipelines/:id/schedules", CreatePipelineSchedule, "member")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+wsScopePipeline+"/schedules",
		bytes.NewBufferString(`{"schedule_type":"interval","schedule_spec":{"every_seconds":3600}}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req.WithContext(ctx))

	created, deleted := sc.snapshot()
	if len(created) != 1 {
		t.Fatalf("want one Temporal schedule created, got %v", created)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s — the Temporal schedule %s has no row behind it (deleted=%v)",
			w.Code, w.Body.String(), created[0], deleted)
	}
	if len(deleted) != 0 {
		t.Fatalf("a committed schedule was deleted from Temporal: %v", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A new pipeline's auto-schedule is the same create on the same context: a tab closed
// as Temporal accepted it left that schedule running with no row behind it.
func TestCreatePipelineScheduleBestEffort_CommitsAfterTheCallerHangsUp(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := installScheduleTemporal(t, cancel)

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM pipeline_schedules`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO pipeline_schedules`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	id, err := createPipelineScheduleBestEffort(ctx, db.DB, wsScopeUser, wsScopePipeline, "cron",
		ScheduleSpec{Cron: "0 0 * * *", Timezone: "UTC"})
	created, deleted := sc.snapshot()
	if err != nil || id == "" {
		t.Fatalf("the auto-schedule failed after the caller hung up: id=%q err=%v created=%v deleted=%v", id, err, created, deleted)
	}
	if len(created) != 1 || created[0] != id || len(deleted) != 0 {
		t.Fatalf("want exactly the committed schedule in Temporal: created=%v deleted=%v", created, deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// first-run namespace lock
// ---------------------------------------------------------------------------

// The executor's hang-up failed the collision probe, and the chosen namespace was then
// locked for good, unverified — into tables another pipeline owns.
func TestLockPipelineNamespaceInternal_ProbesAfterTheCallerHangsUp(t *testing.T) {
	const pipelineID = "12c3579c-52bc-47f2-96ae-10719e4e943c"
	f := &hitlFakeDB{}
	f.onQuery(`SELECT workspace_id::text FROM pipelines`, hitlRows([]string{"workspace_id"}, []driver.Value{wsScopeWS}))
	f.onQuery(`COALESCE\(config, '\{\}'::jsonb\)::text`, hitlRows([]string{"config", "mode"},
		[]driver.Value{`{"destination_namespace":"analytics"}`, nil}))
	f.onQuery(`JOIN connections c ON c\.id = p\.destination_connection_id`, hitlRows([]string{"id", "connector_type"},
		[]driver.Value{hitlTestConn, "postgresql"}))
	sqlDB := sql.OpenDB(hitlFakeConnector{f: f})
	prevDB := db.DB
	db.DB = sqlDB
	t.Cleanup(func() {
		db.DB = prevDB
		_ = sqlDB.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prevProbe := namespaceCollisionProbe
	namespaceCollisionProbe = func(ctx context.Context, _ *sql.DB, _, _, _, namespace, _ string, _, _ []string) (namespaceProbe, error) {
		cancel() // the executor gives up while the destination is being asked
		if err := ctx.Err(); err != nil {
			return namespaceProbe{}, err
		}
		if namespace == "analytics" {
			return namespaceProbe{CollidingTables: []string{"orders"}, OwnerPipelineID: hitlTestForeign}, nil
		}
		return namespaceProbe{}, nil
	}
	t.Cleanup(func() { namespaceCollisionProbe = prevProbe })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/internal/pipelines/:id/namespace/lock", LockPipelineNamespaceInternal)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/pipelines/"+pipelineID+"/namespace/lock",
		bytes.NewBufferString(`{"selected_tables":["public.orders"]}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var locked []string
	f.mu.Lock()
	for _, s := range f.stmts {
		if strings.Contains(s.sql, "'{destination_namespace_locked}'") && len(s.args) > 1 {
			ns, _ := s.args[1].(string)
			locked = append(locked, ns)
		}
	}
	f.mu.Unlock()
	if len(locked) != 1 || locked[0] != "rsync_analytics" {
		t.Fatalf("want the namespace locked on rsync_analytics, clear of the other pipeline's tables; locked %v (body %s)", locked, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// OAuth refresh
// ---------------------------------------------------------------------------

func installOAuthTestClient(t *testing.T) {
	t.Helper()
	prev := newOAuthRefreshClient
	newOAuthRefreshClient = func(d time.Duration) *http.Client { return &http.Client{Timeout: d} }
	t.Cleanup(func() { newOAuthRefreshClient = prev })
}

// The provider rotates the refresh token the moment it answers, so a caller that hung
// up before the new pair was written lost the only refresh token that still worked.
func TestRefreshTokenByID_WritesTheRotatedTokenAfterTheCallerHangsUp(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	installOAuthTestClient(t)
	encAccess, err := crypto.EncryptString("old-a")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.EncryptString("old-r")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel() // the caller hangs up while the provider rotates the token
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-a","refresh_token":"new-r","expires_in":3600}`))
	}))
	defer provider.Close()

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(`SELECT pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`FROM oauth_tokens`).
		WithArgs("tok-1").
		WillReturnRows(sqlmock.NewRows([]string{"provider", "access_token", "refresh_token", "expires_at"}).
			AddRow("acme", encAccess, encRefresh, time.Now().Add(-time.Hour)))
	mock.ExpectExec(`UPDATE oauth_tokens`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SELECT pg_advisory_unlock`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := &OAuthHandler{db: mockDB, providers: map[string]*OAuthProvider{
		"acme": {Name: "acme", ClientID: "cid", ClientSecret: "secret", TokenURL: provider.URL},
	}}
	if err := h.RefreshTokenByID(ctx, "tok-1"); err != nil {
		t.Fatalf("the refresh failed after the caller hung up: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the rotated token was not written, or the lock not released: %v", err)
	}
}

// An unlock that fails must not return the session to the pool still holding the
// lock: every later refresh of that token would find it taken and skip.
func TestRefreshTokenByID_AFailedUnlockDropsTheConnection(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`FROM oauth_tokens`).
		WithArgs("tok-1").
		WillReturnRows(sqlmock.NewRows([]string{"provider", "access_token", "refresh_token", "expires_at"}).
			AddRow("acme", "a", "r", time.Now().Add(time.Hour)))
	mock.ExpectExec(`SELECT pg_advisory_unlock`).
		WillReturnError(errors.New("connection reset"))
	mock.ExpectClose()

	h := &OAuthHandler{db: mockDB, providers: map[string]*OAuthProvider{}}
	if err := h.RefreshTokenByID(context.Background(), "tok-1"); !errors.Is(err, ErrTokenFresh) {
		t.Fatalf("want ErrTokenFresh, got %v", err)
	}
	// Checked before mockDB.Close(), which would close the connection itself.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the connection still holding the lock went back to the pool: %v", err)
	}
	_ = mockDB.Close()
}

// ---------------------------------------------------------------------------
// session advisory locks
// ---------------------------------------------------------------------------

// sessionAdvisoryLockSQL is a session-level advisory lock or unlock. The _xact_ forms
// end with their transaction, so they are not held by a connection.
var sessionAdvisoryLockSQL = regexp.MustCompile(`pg_(try_)?advisory_(lock|unlock)(_shared)?\(`)

// A session advisory lock belongs to the connection that took it. On a pooled *sql.DB
// the unlock runs on whichever connection is free, so the lock stays held on the first
// one for its whole life. oauth.go did exactly that until this guard. Every lock and
// unlock must go through a *sql.Conn the same function reserved.
func TestSessionAdvisoryLocksRunOnOneConn(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bad []string
	sites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			conns := reservedConns(fd.Body)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !takesSessionAdvisoryLock(call) {
					return true
				}
				sites++
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && conns[id.Name] {
						return true
					}
				}
				bad = append(bad, fd.Name.Name+" at "+fset.Position(call.Pos()).String())
				return true
			})
		}
	}
	if sites == 0 {
		t.Fatal("found no advisory-lock call at all: the guard no longer sees what it guards")
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s takes or releases a session advisory lock on a pooled handle: reserve a *sql.Conn with database.Conn(ctx) and use it for both", b)
	}
}

// reservedConns names the variables in body assigned from <x>.Conn(...).
func reservedConns(body ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Conn" && len(as.Lhs) > 0 {
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

func takesSessionAdvisoryLock(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if s, err := strconv.Unquote(lit.Value); err == nil && sessionAdvisoryLockSQL.MatchString(s) {
			return true
		}
	}
	return false
}
