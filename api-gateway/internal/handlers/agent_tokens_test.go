package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
)

// tokenRouter is a signed-in person (or, with agent set, an agent token) in the
// active workspace with role.
func tokenRouter(method, path string, h gin.HandlerFunc, role string, agent bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxWorkspaceRole, role)
		if agent {
			c.Set(ctxActorKind, actorKindAgent)
			c.Set(ctxTokenID, agTokenID)
		}
		c.Next()
	})
	r.Handle(method, path, h)
	return r
}

func serveToken(r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// argCapture matches any argument and keeps it.
type argCapture struct{ v driver.Value }

func (c *argCapture) Match(v driver.Value) bool { c.v = v; return true }

// within matches a time.Time no further than tol from want.
type within struct {
	want time.Time
	tol  time.Duration
}

func (w within) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	if !ok {
		return false
	}
	d := t.Sub(w.want)
	return d <= w.tol && d >= -w.tol
}

// expectLiveCount is the cap check: a transaction that takes the caller's
// advisory lock, then counts, so two requests at once cannot both pass a count
// of 19. At the cap it rolls back with nothing stored; under it, the test
// expects the INSERT and then the commit, in the same transaction.
func expectLiveCount(mock sqlmock.Sqlmock, n int) {
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtext\(\$1\)\)`).
		WithArgs("agent_tokens:" + wsScopeUser + ":" + wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM agent_tokens\s+WHERE user_id = \$1 AND workspace_id = \$2 AND revoked_at IS NULL AND expires_at > NOW\(\)`).
		WithArgs(wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
	if n >= agentTokenMaxLiveCount {
		mock.ExpectRollback()
	}
}

func expectPersonAudit(mock sqlmock.Sqlmock, action string, details sqlmock.Argument) {
	mock.ExpectExec(`INSERT INTO audit_logs \(user_id, action, resource_type, resource_id, details, ip_address, workspace_id\)\s+VALUES`).
		WithArgs(wsScopeUser, action, "agent_token", sqlmock.AnyArg(), details, sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// The token is shown once, stored only as its sha256 with a short display
// prefix, carries metadata:read, and expires in 30 days unless told otherwise.
// It never reaches the audit row.
func TestCreateAgentToken_ReturnsOnceStoresHash(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	hash, prefix, details := &argCapture{}, &argCapture{}, &argCapture{}
	expectLiveCount(mock, 19)
	mock.ExpectExec(`INSERT INTO agent_tokens \(id, user_id, workspace_id, name, token_hash, token_prefix, scopes, expires_at\)`).
		WithArgs(sqlmock.AnyArg(), wsScopeUser, wsScopeWS, "my laptop", hash, prefix, "metadata:read",
			within{time.Now().UTC().Add(30 * 24 * time.Hour), time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectPersonAudit(mock, "create_agent_token", details)

	r := tokenRouter(http.MethodPost, "/agent-tokens", CreateAgentToken, "viewer", false)
	w := serveToken(r, http.MethodPost, "/agent-tokens", "{\"name\":\"  my\\u0000 laptop\\n \"}")
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Token       string   `json:"token"`
		TokenPrefix string   `json:"token_prefix"`
		Name        string   `json:"name"`
		Scopes      []string `json:"scopes"`
		WorkspaceID string   `json:"workspace_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !strings.HasPrefix(got.Token, "rsat_") || len(got.Token) != len("rsat_")+64 {
		t.Fatalf("token = %q", got.Token)
	}
	if hash.v != crypto.HashSessionToken(got.Token) {
		t.Fatalf("stored hash %v is not sha256 of the returned token", hash.v)
	}
	if prefix.v != got.Token[:12] || got.TokenPrefix != got.Token[:12] {
		t.Fatalf("prefix stored %v, returned %q", prefix.v, got.TokenPrefix)
	}
	if got.Name != "my laptop" || len(got.Scopes) != 1 || got.Scopes[0] != "metadata:read" || got.WorkspaceID != wsScopeWS {
		t.Fatalf("got %+v", got)
	}
	if s, _ := details.v.(string); strings.Contains(s, got.Token) || strings.Contains(s, got.Token[12:]) {
		t.Fatalf("the token reached the audit row: %s", s)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAgentToken_Expiry(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectLiveCount(mock, 0)
	mock.ExpectExec(`INSERT INTO agent_tokens`).
		WithArgs(sqlmock.AnyArg(), wsScopeUser, wsScopeWS, "ci", sqlmock.AnyArg(), sqlmock.AnyArg(), "metadata:read",
			within{time.Now().UTC().Add(365 * 24 * time.Hour), time.Minute}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectPersonAudit(mock, "create_agent_token", sqlmock.AnyArg())

	r := tokenRouter(http.MethodPost, "/agent-tokens", CreateAgentToken, "member", false)
	if w := serveToken(r, http.MethodPost, "/agent-tokens", `{"name":"ci","expires_in_days":365}`); w.Code != http.StatusCreated {
		t.Fatalf("365 days: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Refused before anything is written.
func TestCreateAgentToken_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		live   int // -1: the count query is not reached
		agent  bool
		status int
	}{
		{"no name", `{"name":"  \u0007 "}`, -1, false, http.StatusBadRequest},
		{"not JSON", `name=x`, -1, false, http.StatusBadRequest},
		{"zero days", `{"name":"a","expires_in_days":0}`, -1, false, http.StatusBadRequest},
		{"over a year", `{"name":"a","expires_in_days":366}`, -1, false, http.StatusBadRequest},
		{"twenty live", `{"name":"a"}`, 20, false, http.StatusConflict},
		{"an agent token", `{"name":"a"}`, -1, true, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			if tc.live >= 0 {
				expectLiveCount(mock, tc.live)
			}
			r := tokenRouter(http.MethodPost, "/agent-tokens", CreateAgentToken, "owner", tc.agent)
			if w := serveToken(r, http.MethodPost, "/agent-tokens", tc.body); w.Code != tc.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// connectors:generate, models:write, workflows:write and dashboards:write are granted only
// by a workspace admin or owner, an unknown scope is refused, and metadata:read is always
// there, once.
func TestCreateAgentToken_Scopes(t *testing.T) {
	cases := []struct {
		name, role, body string
		status           int
		stored           string
	}{
		{"admin grants generate", "admin", `{"name":"a","scopes":["connectors:generate"]}`, http.StatusCreated, "metadata:read,connectors:generate"},
		{"owner grants generate", "owner", `{"name":"a","scopes":["metadata:read","connectors:generate","connectors:generate"]}`, http.StatusCreated, "metadata:read,connectors:generate"},
		{"no scopes", "member", `{"name":"a","scopes":[]}`, http.StatusCreated, "metadata:read"},
		{"member asks for generate", "member", `{"name":"a","scopes":["connectors:generate"]}`, http.StatusForbidden, ""},
		{"viewer asks for generate", "viewer", `{"name":"a","scopes":["connectors:generate"]}`, http.StatusForbidden, ""},
		{"unknown scope", "owner", `{"name":"a","scopes":["pipelines:write"]}`, http.StatusBadRequest, ""},
		{"admin grants models", "admin", `{"name":"a","scopes":["models:write"]}`, http.StatusCreated, "metadata:read,models:write"},
		{"owner grants both", "owner", `{"name":"a","scopes":["models:write","connectors:generate"]}`, http.StatusCreated, "metadata:read,models:write,connectors:generate"},
		{"member asks for models", "member", `{"name":"a","scopes":["models:write"]}`, http.StatusForbidden, ""},
		{"viewer asks for models", "viewer", `{"name":"a","scopes":["models:write"]}`, http.StatusForbidden, ""},
		{"admin grants workflows", "admin", `{"name":"a","scopes":["workflows:write"]}`, http.StatusCreated, "metadata:read,workflows:write"},
		{"member asks for workflows", "member", `{"name":"a","scopes":["workflows:write"]}`, http.StatusForbidden, ""},
		{"admin grants dashboards", "admin", `{"name":"a","scopes":["dashboards:write"]}`, http.StatusCreated, "metadata:read,dashboards:write"},
		{"member asks for dashboards", "member", `{"name":"a","scopes":["dashboards:write"]}`, http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			if tc.stored != "" {
				expectLiveCount(mock, 0)
				mock.ExpectExec(`INSERT INTO agent_tokens`).
					WithArgs(sqlmock.AnyArg(), wsScopeUser, wsScopeWS, "a", sqlmock.AnyArg(), sqlmock.AnyArg(), tc.stored, sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
				expectPersonAudit(mock, "create_agent_token", sqlmock.AnyArg())
			}
			r := tokenRouter(http.MethodPost, "/agent-tokens", CreateAgentToken, tc.role, false)
			w := serveToken(r, http.MethodPost, "/agent-tokens", tc.body)
			if w.Code != tc.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if tc.status == http.StatusForbidden && !strings.Contains(w.Body.String(), "admin or owner") {
				t.Fatalf("refusal does not say who can: %s", w.Body.String())
			}
			// An unknown scope's refusal names every scope a token can carry.
			if tc.status == http.StatusBadRequest {
				for _, s := range services.TokenScopes {
					if !strings.Contains(w.Body.String(), s) {
						t.Fatalf("refusal does not name %s: %s", s, w.Body.String())
					}
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A member lists their own tokens; an admin or owner lists the workspace's.
// The list never carries the token or its hash.
func TestListAgentTokens_MemberVersusAdmin(t *testing.T) {
	for _, tc := range []struct {
		role string
		all  bool
	}{{"viewer", false}, {"member", false}, {"admin", true}, {"owner", true}} {
		t.Run(tc.role, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			now := time.Now()
			mock.ExpectQuery(`FROM agent_tokens t\s+JOIN users u ON u.id = t.user_id\s+WHERE t.workspace_id = \$2 AND \(t.user_id = \$1 OR \$3\)`).
				WithArgs(wsScopeUser, wsScopeWS, tc.all).
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "token_prefix", "scopes", "expires_at", "revoked_at", "last_used_at", "created_at", "email", "mine"}).
					AddRow(agTokenID, "laptop", "rsat_0123456", "metadata:read", now.Add(time.Hour), nil, now, now, "dev@example.com", true))

			r := tokenRouter(http.MethodGet, "/agent-tokens", ListAgentTokens, tc.role, false)
			w := serveToken(r, http.MethodGet, "/agent-tokens", "")
			if w.Code != http.StatusOK {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			var got struct {
				Tokens []map[string]any `json:"tokens"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if len(got.Tokens) != 1 || got.Tokens[0]["owner_email"] != "dev@example.com" || got.Tokens[0]["mine"] != true {
				t.Fatalf("tokens = %v", got.Tokens)
			}
			for _, k := range []string{"token", "token_hash"} {
				if _, ok := got.Tokens[0][k]; ok {
					t.Fatalf("list carries %s", k)
				}
			}
			if _, ok := got.Tokens[0]["revoked_at"]; ok {
				t.Fatal("a live token lists a revoked_at")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListAgentTokens_RefusesAnAgentToken(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	r := tokenRouter(http.MethodGet, "/agent-tokens", ListAgentTokens, "owner", true)
	if w := serveToken(r, http.MethodGet, "/agent-tokens", ""); w.Code != http.StatusForbidden {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The owner revokes their own token; an admin or owner revokes any in the
// workspace; anything else is "not found". The last-used cache forgets it.
func TestRevokeAgentToken(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		id       string
		all      bool
		affected int64 // -1: the update is not reached
		agent    bool
		status   int
	}{
		{"member revokes own", "member", agTokenID, false, 1, false, http.StatusOK},
		{"member, not theirs or already revoked", "member", agTokenID, false, 0, false, http.StatusNotFound},
		{"admin revokes a member's", "admin", agTokenID, true, 1, false, http.StatusOK},
		{"not a uuid", "owner", "rsat_abc", false, -1, false, http.StatusNotFound},
		{"an agent token", "owner", agTokenID, false, -1, true, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			agentTokenTouched.Store(agTokenID, time.Now())
			defer agentTokenTouched.Delete(agTokenID)
			if tc.affected >= 0 {
				mock.ExpectExec(`UPDATE agent_tokens SET revoked_at = NOW\(\)\s+WHERE id = \$1 AND workspace_id = \$3 AND \(user_id = \$2 OR \$4\) AND revoked_at IS NULL`).
					WithArgs(tc.id, wsScopeUser, wsScopeWS, tc.all).
					WillReturnResult(sqlmock.NewResult(0, tc.affected))
			}
			if tc.status == http.StatusOK {
				expectPersonAudit(mock, "revoke_agent_token", sqlmock.AnyArg())
			}

			r := tokenRouter(http.MethodDelete, "/agent-tokens/:id", RevokeAgentToken, tc.role, tc.agent)
			if w := serveToken(r, http.MethodDelete, "/agent-tokens/"+tc.id, ""); w.Code != tc.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			_, cached := agentTokenTouched.Load(agTokenID)
			if tc.status == http.StatusOK && cached {
				t.Fatal("a revoked token stays in the last-used cache")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
