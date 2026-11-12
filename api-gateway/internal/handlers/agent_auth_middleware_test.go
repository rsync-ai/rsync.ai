package handlers

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
)

const (
	agTokenID = "55555555-5555-4555-8555-555555555555"
	otherWS   = "66666666-6666-4666-8666-666666666666"
)

// agToken has the real shape (rsat_ + 64 hex) but is built, not written out: a
// literal that shape trips gitleaks' generic-api-key rule.
var agToken = "rsat_" + strings.Repeat("0123456789abcdef", 4)

// agentRouter mounts AgentTokenMiddleware in front of a handler that echoes the
// identity the middleware pinned, the way the agent gateway would read it.
func agentRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/mcp", AgentTokenMiddleware(), func(c *gin.Context) {
		a := AgentActor(c)
		c.JSON(http.StatusOK, gin.H{
			"user_id":        c.GetString("user_id"),
			"user_role":      c.GetString("user_role"),
			"workspace_id":   a.WorkspaceID,
			"workspace_role": string(a.Role),
			"actor_kind":     c.GetString(ctxActorKind),
			"token_id":       c.GetString(ctxTokenID),
			"channel":        string(a.Channel),
			"scopes":         a.Scopes,
		})
	})
	return r
}

// resetAgentTokenState gives each test a fresh rate limiter and last-used cache,
// both process globals.
func resetAgentTokenState(t *testing.T, limit int) {
	t.Helper()
	prev, prevRefusals := agentTokenLimiter, agentRefusalAudited
	agentTokenLimiter = newFixedWindowLimiter(limit, time.Minute)
	agentRefusalAudited = newFixedWindowLimiter(1, time.Minute)
	clearTouched := func() {
		agentTokenTouched.Range(func(k, _ any) bool { agentTokenTouched.Delete(k); return true })
	}
	clearTouched()
	t.Cleanup(func() {
		agentTokenLimiter, agentRefusalAudited = prev, prevRefusals
		clearTouched()
	})
}

func agentCall(r *gin.Engine, header map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// expectTokenLookup stamps the token query. The regex pins the liveness filter:
// a revoked or expired token must never match in SQL.
func expectTokenLookup(mock sqlmock.Sqlmock, token string) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`FROM agent_tokens t\s+JOIN users u ON u.id = t.user_id\s+WHERE t.token_hash = \$1 AND t.revoked_at IS NULL AND t.expires_at > NOW\(\)`).
		WithArgs(crypto.HashSessionToken(token))
}

func tokenRow(scopes, status string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "user_id", "workspace_id", "scopes", "email", "status"}).
		AddRow(agTokenID, wsScopeUser, wsScopeWS, scopes, "dev@example.com", status)
}

func expectMembership(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT role FROM workspace_members WHERE workspace_id = \$1 AND user_id = \$2`).
		WithArgs(wsScopeWS, wsScopeUser)
}

func expectTouch(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`UPDATE agent_tokens SET last_used_at = NOW\(\)`).
		WithArgs(agTokenID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectRefusalAudit stamps the audit row a refused call writes: as the token's
// user, workspace and token when the gateway could tell them, NULLs when it
// could not (no token, or one that is unknown, revoked or expired), and why.
func expectRefusalAudit(mock sqlmock.Sqlmock, known bool, code string, status int) {
	var user, ws, tok any = sql.NullString{}, sql.NullString{}, sql.NullString{}
	if known {
		user, ws, tok = wsScopeUser, wsScopeWS, agTokenID
	}
	mock.ExpectExec(`INSERT INTO audit_logs \(user_id, action, resource_type, resource_id, details, ip_address, workspace_id, actor_kind, token_id\)`).
		WithArgs(user, "agent_auth_refused", "agent_token", tok,
			jsonHas{"code": code, "status": float64(status)}, sqlmock.AnyArg(), ws, "agent", tok).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	code, _ := body["error"].(string)
	return code
}

// Anything but an rsat_ bearer token is refused before the database is asked:
// a session token, a session cookie, the dev X-User-ID header.
func TestAgentTokenMiddleware_OnlyAnAgentBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		cookie *http.Cookie
	}{
		{"no header", nil, nil},
		{"session token as bearer", map[string]string{"Authorization": "Bearer 3f2a9c0d1e4b5a6978"}, nil},
		{"session cookie only", nil, &http.Cookie{Name: "session_token", Value: agToken}},
		{"dev user header only", map[string]string{"X-User-ID": wsScopeUser}, nil},
		{"basic auth", map[string]string{"Authorization": "Basic " + agToken}, nil},
		{"bare prefix", map[string]string{"Authorization": "Bearer rsat_"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAgentTokenState(t, 120)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectRefusalAudit(mock, false, "missing_agent_token", http.StatusUnauthorized)

			w := agentCall(agentRouter(), tc.header, tc.cookie)
			if w.Code != http.StatusUnauthorized || errorCode(t, w) != "missing_agent_token" {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer ") {
				t.Fatalf("WWW-Authenticate = %q", w.Header().Get("WWW-Authenticate"))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// agentRefusalCases drives every refusal AgentTokenMiddleware answers with a
// token in the header; TestEveryAgentAuthRefusalIsAudited checks the list
// against the middleware's code.
var agentRefusalCases = []struct {
	name   string
	expect func(sqlmock.Sqlmock)
	status int
	code   string
	known  bool // the audit row names the token's user, workspace and token
}{
	{"unknown, revoked or expired", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnError(sql.ErrNoRows)
	}, http.StatusUnauthorized, "invalid_agent_token", false},
	{"deactivated user", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnRows(tokenRow("metadata:read", "Deactivated"))
	}, http.StatusUnauthorized, "account_deactivated", true},
	{"no longer a member", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
		expectMembership(m).WillReturnError(sql.ErrNoRows)
	}, http.StatusUnauthorized, "not_a_member", true},
	{"unknown workspace role", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
		expectMembership(m).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("superuser"))
	}, http.StatusForbidden, "unknown_workspace_role", true},
	{"no scope this server knows", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnRows(tokenRow("pipeline:write,admin", "active"))
		expectMembership(m).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("owner"))
	}, http.StatusForbidden, "no_known_scope", true},
	{"database error on the token", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnError(sql.ErrConnDone)
	}, http.StatusInternalServerError, "db_error", false},
	{"database error on the membership", func(m sqlmock.Sqlmock) {
		expectTokenLookup(m, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
		expectMembership(m).WillReturnError(sql.ErrConnDone)
	}, http.StatusInternalServerError, "db_error", true},
}

// Every refusal is answered with its code and writes an audit row as whoever
// the gateway could tell the caller was. Bug class: an auth refusal that leaves
// no audit row, so a leaked, revoked or demoted token's attempts are invisible.
func TestAgentTokenMiddleware_Refusals(t *testing.T) {
	for _, tc := range agentRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			resetAgentTokenState(t, 120)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			tc.expect(mock)
			expectRefusalAudit(mock, tc.known, tc.code, tc.status)

			w := agentCall(agentRouter(), map[string]string{"Authorization": "Bearer " + agToken}, nil)
			if w.Code != tc.status || errorCode(t, w) != tc.code {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A refused caller that retries writes one audit row a minute per token (or,
// with no live token, per client IP) and code, not one per retry.
func TestAgentTokenMiddleware_RefusalAuditIsThrottled(t *testing.T) {
	resetAgentTokenState(t, 120)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	r := agentRouter()
	auth := map[string]string{"Authorization": "Bearer " + agToken}

	// Unordered, so a second row would meet the armed expectation below
	// instead of failing against the next lookup.
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < 3; i++ {
		expectTokenLookup(mock, agToken).WillReturnRows(tokenRow("metadata:read", "Deactivated"))
	}
	expectRefusalAudit(mock, true, "account_deactivated", http.StatusUnauthorized)
	// Armed but must stay unmet: the retries inside the minute write nothing.
	expectRefusalAudit(mock, true, "account_deactivated", http.StatusUnauthorized)
	for i := 0; i < 3; i++ {
		if w := agentCall(r, auth, nil); errorCode(t, w) != "account_deactivated" {
			t.Fatalf("call %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("three refused calls inside a minute wrote more than one audit row")
	} else if !strings.Contains(err.Error(), "INSERT INTO audit_logs") {
		t.Fatalf("retries: %v", err)
	}

	// Control: another code from the same token is a new row.
	mock2, cleanup2 := wsScopeMockDB(t)
	defer cleanup2()
	expectTokenLookup(mock2, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
	expectMembership(mock2).WillReturnError(sql.ErrNoRows)
	expectRefusalAudit(mock2, true, "not_a_member", http.StatusUnauthorized)
	if w := agentCall(r, auth, nil); errorCode(t, w) != "not_a_member" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// With no database there is nowhere to write the row; the refusal still stands.
func TestAgentTokenMiddleware_NoDatabase(t *testing.T) {
	resetAgentTokenState(t, 120)
	prev := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = prev })
	w := agentCall(agentRouter(), map[string]string{"Authorization": "Bearer " + agToken}, nil)
	if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "database_unavailable" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// A live token acts as its user in its own workspace with the member's current
// role; X-Workspace-ID cannot move it, and it is never a platform admin.
func TestAgentTokenMiddleware_PinsTheTokensIdentity(t *testing.T) {
	resetAgentTokenState(t, 120)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectTokenLookup(mock, agToken).WillReturnRows(tokenRow("metadata:read,unknown:scope", "active"))
	expectMembership(mock).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("viewer"))
	expectTouch(mock)

	w := agentCall(agentRouter(), map[string]string{
		"Authorization":  "bearer  " + agToken + " ",
		"X-Workspace-ID": otherWS,
		"X-User-Role":    "admin",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	want := map[string]any{
		"user_id":        wsScopeUser,
		"user_role":      "user",
		"workspace_id":   wsScopeWS,
		"workspace_role": "viewer",
		"actor_kind":     "agent",
		"token_id":       agTokenID,
		"channel":        "agent",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if scopes, _ := got["scopes"].([]any); len(scopes) != 1 || scopes[0] != "metadata:read" {
		t.Errorf("scopes = %v, want only the known one", got["scopes"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTokenMiddleware_RateLimited(t *testing.T) {
	resetAgentTokenState(t, 1)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectTokenLookup(mock, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
	expectMembership(mock).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	expectTouch(mock)
	expectTokenLookup(mock, agToken).WillReturnRows(tokenRow("metadata:read", "active"))
	expectMembership(mock).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	expectRefusalAudit(mock, true, "rate_limit_exceeded", http.StatusTooManyRequests)

	r := agentRouter()
	auth := map[string]string{"Authorization": "Bearer " + agToken}
	if w := agentCall(r, auth, nil); w.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", w.Code, w.Body.String())
	}
	w := agentCall(r, auth, nil)
	if w.Code != http.StatusTooManyRequests || errorCode(t, w) != "rate_limit_exceeded" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second call: %d %s Retry-After=%q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A batch's further tool calls draw on the same per-token budget as the
// middleware's one charge per request; a context with no token spends nothing.
func TestAgentTokenSpend(t *testing.T) {
	resetAgentTokenState(t, 2)
	ctx := func(tokenID string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if tokenID != "" {
			c.Set(ctxTokenID, tokenID)
		}
		return c
	}
	if !AgentTokenSpend(ctx("tok-a")) || !AgentTokenSpend(ctx("tok-a")) {
		t.Fatal("the first two calls fit a limit of 2")
	}
	if AgentTokenSpend(ctx("tok-a")) {
		t.Fatal("a third call past a limit of 2 was allowed")
	}
	if !AgentTokenSpend(ctx("tok-b")) {
		t.Fatal("another token's budget was spent")
	}
	if AgentTokenSpend(ctx("")) {
		t.Fatal("a request with no token was allowed to spend")
	}
}

// last_used_at is written at most once a minute per token.
func TestTouchAgentToken_OncePerMinute(t *testing.T) {
	resetAgentTokenState(t, 120)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	now := time.Now()

	expectTouch(mock)
	touchAgentToken(db.GetDB(), agTokenID, now)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("first touch: %v", err)
	}

	// Armed but must stay unmet: a second touch inside the minute writes nothing.
	expectTouch(mock)
	touchAgentToken(db.GetDB(), agTokenID, now.Add(30*time.Second))
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("a second touch inside a minute wrote last_used_at")
	}

	touchAgentToken(db.GetDB(), agTokenID, now.Add(61*time.Second))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("touch after a minute: %v", err)
	}
}

func TestAgentBearerToken(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"Bearer":                 "",
		"Bearer ":                "",
		"Bearer rsat_":           "",
		"Bearer rsat_abc":        "rsat_abc",
		"BEARER rsat_abc":        "rsat_abc",
		"  bearer   rsat_abc   ": "rsat_abc",
		"Bearer sess_abc":        "",
		"Basic rsat_abc":         "",
		"rsat_abc":               "",
		"Bearerrsat_abc":         "",
	}
	for in, want := range cases {
		if got := agentBearerToken(in); got != want {
			t.Errorf("agentBearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// An agent's audit row says it was an agent and which token; a person's row
// keeps the 7-column insert with both left NULL.
func TestLogAudit_MarksAgentRows(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("agent", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectExec(`INSERT INTO audit_logs \(user_id, action, resource_type, resource_id, details, ip_address, workspace_id, actor_kind, token_id\)`).
			WithArgs(wsScopeUser, "agent_tool_call", "pipeline", wsScopePipeline,
				jsonHas{"tool": "get_pipeline_health", "outcome": "ok", "status": float64(200)},
				sqlmock.AnyArg(), wsScopeWS, "agent", agTokenID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/mcp", nil)
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxActorKind, actorKindAgent)
		c.Set(ctxTokenID, agTokenID)
		LogAgentToolCall(c, "get_pipeline_health", "pipeline", wsScopePipeline, "ok", 200)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	// A handler the assistant's tool ran in-process (create_model's saved
	// query) writes its own row: MarkAssistantCall makes it say assistant,
	// with no token.
	t.Run("assistant handler", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectExec(`INSERT INTO audit_logs \(user_id, action, resource_type, resource_id, details, ip_address, workspace_id, actor_kind, token_id\)`).
			WithArgs(wsScopeUser, "create_saved_query", "saved_query", wsScopePipeline, sqlmock.AnyArg(),
				sqlmock.AnyArg(), wsScopeWS, "assistant", sql.NullString{}).
			WillReturnResult(sqlmock.NewResult(0, 1))

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/saved-queries", nil)
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		MarkAssistantCall(c)
		logAudit(c, "create_saved_query", "saved_query", wsScopePipeline, map[string]interface{}{})
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("person", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectExec(`INSERT INTO audit_logs \(user_id, action, resource_type, resource_id, details, ip_address, workspace_id\)\s+VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, \$7\)`).
			WithArgs(wsScopeUser, "pause_pipeline", "pipeline", wsScopePipeline, sqlmock.AnyArg(), sqlmock.AnyArg(), wsScopeWS).
			WillReturnResult(sqlmock.NewResult(0, 1))

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/p", nil)
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		logAudit(c, "pause_pipeline", "pipeline", wsScopePipeline, map[string]interface{}{})
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// jsonHas matches a JSON string argument that carries each key with its value.
type jsonHas map[string]any

func (j jsonHas) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var got map[string]any
	if json.Unmarshal([]byte(s), &got) != nil {
		return false
	}
	for k, want := range j {
		if got[k] != want {
			return false
		}
	}
	return true
}

// Every refusal AgentTokenMiddleware can answer goes through refuseAgentCall,
// the path that writes the audit row, and has a test above that checks the row.
// A new refusal written as a bare abort, or without a case, fails here.
func TestEveryAgentAuthRefusalIsAudited(t *testing.T) {
	tested := map[string]string{
		"missing_agent_token":  "TestAgentTokenMiddleware_OnlyAnAgentBearerToken",
		"rate_limit_exceeded":  "TestAgentTokenMiddleware_RateLimited",
		"database_unavailable": "TestAgentTokenMiddleware_NoDatabase (no database to write the row to)",
	}
	for _, tc := range agentRefusalCases {
		tested[tc.code] = "agentRefusalCases"
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "agent_auth_middleware.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var mw *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "AgentTokenMiddleware" {
			mw = fd
		}
	}
	if mw == nil {
		t.Fatal("AgentTokenMiddleware not found")
	}
	refusals := 0
	ast.Inspect(mw, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		switch {
		case name == "refuseAgentCall":
			refusals++
			if len(call.Args) != 5 {
				t.Errorf("%s: refuseAgentCall takes 5 arguments", fset.Position(call.Pos()))
				return true
			}
			var code string
			if lit, ok := call.Args[3].(*ast.BasicLit); ok {
				code, _ = strconv.Unquote(lit.Value)
			}
			if code == "" {
				t.Errorf("%s: name the refusal code with a string literal", fset.Position(call.Pos()))
			} else if tested[code] == "" {
				t.Errorf("%s: refusal %q has no test that checks its audit row; add it to agentRefusalCases",
					fset.Position(call.Pos()), code)
			}
		case name == "agentAuthAbort" || strings.HasPrefix(name, "Abort"):
			t.Errorf("%s: a refusal that skips the audit row; answer it with refuseAgentCall", fset.Position(call.Pos()))
		}
		return true
	})
	if refusals < 10 {
		t.Fatalf("found %d refuseAgentCall calls, want at least 10: the scan is broken", refusals)
	}
}
