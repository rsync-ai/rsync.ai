package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeGenerator is a tool-generator: /health answers health, /v1/generate
// answers genStatus and genBody, and both count their calls.
type fakeGenerator struct {
	health    atomic.Int32
	generates atomic.Int32
	// catalog is the empty connector folder MCP_PUBLIC_CONNECTORS_PATH points
	// at, so no name is "already on disk" because of the checkout the test runs in.
	catalog string

	mu     sync.Mutex
	body   map[string]any
	header http.Header
}

func (g *fakeGenerator) lastRequest() (map[string]any, http.Header) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.body, g.header
}

func newFakeGenerator(t *testing.T, healthStatus int, health string, genStatus int, genBody string) *fakeGenerator {
	t.Helper()
	g := &fakeGenerator{catalog: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			g.health.Add(1)
			w.WriteHeader(healthStatus)
			_, _ = w.Write([]byte(health))
		case "/v1/generate":
			g.generates.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.mu.Lock()
			g.body, g.header = body, r.Header.Clone()
			g.mu.Unlock()
			w.WriteHeader(genStatus)
			_, _ = w.Write([]byte(genBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TOOL_GENERATOR_URL", srv.URL)
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", g.catalog)
	t.Setenv("MCP_INTERNAL_CONNECTORS_PATH", g.catalog)
	return g
}

const genOK = `{"success": true, "version": "v1.0.0", "operation_count": 3}`

// stubGenerator is a generator whose /health answers health and whose every
// generation succeeds. It returns the count of /v1/generate calls.
func stubGenerator(t *testing.T, health string) *atomic.Int32 {
	t.Helper()
	return &newFakeGenerator(t, http.StatusOK, health, http.StatusOK, genOK).generates
}

// freshGenLimiter gives the test its own hourly limiter, so tests cannot
// spend each other's budget.
func freshGenLimiter(t *testing.T) {
	t.Helper()
	prev := connectorGenLimiter
	connectorGenLimiter = newFixedWindowLimiter(10, time.Hour)
	t.Cleanup(func() { connectorGenLimiter = prev })
}

// syncLaunch runs a started generation inline, so the test sees how it ended.
func syncLaunch(t *testing.T) {
	t.Helper()
	prev := launchGeneration
	launchGeneration = func(run func()) { run() }
	t.Cleanup(func() { launchGeneration = prev })
}

func chatAdmin() services.Actor {
	return services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSAdmin, Channel: services.ChannelChat}
}

func agentWith(role security.WorkspaceRole, scopes ...string) services.Actor {
	return services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: role, Channel: services.ChannelAgent, Scopes: scopes}
}

func assistantAs(role security.WorkspaceRole) services.Actor {
	return services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: role,
		Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}
}

const (
	genSpecURL = "https://api.acme.example/openapi.json"
	genTokenID = "33333333-3333-3333-3333-333333333333"
	genRunID   = "44444444-4444-4444-4444-444444444444"
)

var genInput = GenerationInput{Name: "acme-crm", SpecURL: genSpecURL}

// refusalOf returns err as a refusal, failing the test when it is not one.
func refusalOf(t *testing.T, err error) *GenerationRefusal {
	t.Helper()
	var r *GenerationRefusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}

// reachedAdmission reports whether a request passed every check and was
// stopped only by the missing database (pinNoCatalogDB) at the run row.
func reachedAdmission(err error) bool {
	var r *GenerationRefusal
	return err != nil && !errors.As(err, &r) && err.Error() == "database unavailable"
}

// The chat "generate <name>" command is one more way to generate, so it meets
// the guards the Generate page does: here the workspace's hourly limit. It
// used to post to the generator with no limit at all.
func TestChatGenerateCommand_MeetsTheHourlyLimit(t *testing.T) {
	pinNoCatalogDB(t)
	generates := stubGenerator(t, `{}`)
	freshGenLimiter(t)
	now := time.Now()
	for i := 0; i < 10; i++ {
		connectorGenLimiter.Allow(wsScopeWS, now)
	}

	resp, handled := (&ChatHandler{}).maybeHandleGenerateCommand(context.Background(), chatAdmin(), "generate acme-crm", "trace")
	if !handled {
		t.Fatal("generate command not handled")
	}
	if resp.Type == "connector_generation_started" {
		t.Fatalf("an 11th generation in the hour started: %+v", resp)
	}
	if !strings.Contains(resp.Message, "in the last hour") {
		t.Fatalf("the refusal does not say why: %q", resp.Message)
	}
	time.Sleep(50 * time.Millisecond)
	if n := generates.Load(); n != 0 {
		t.Fatalf("the generator was asked to generate %d times, want 0", n)
	}
}

// A built-in connector rsync.ai maintains is never generated over, whichever
// way the request comes. The chat used to refuse only names in its common
// set, so "generate debezium" reached the generator.
func TestChatGenerateCommand_RefusesABuiltInConnector(t *testing.T) {
	pinNoCatalogDB(t)
	generates := stubGenerator(t, `{}`)
	freshGenLimiter(t)

	for _, name := range []string{"debezium", "kafka-mcp-sink", "github-rest", "shopify-admin-graphql"} {
		resp, handled := (&ChatHandler{}).maybeHandleGenerateCommand(context.Background(), chatAdmin(), "generate "+name, "trace")
		if !handled {
			t.Fatalf("generate %s not handled", name)
		}
		if resp.Type == "connector_generation_started" {
			t.Fatalf("generate %s started a run over a built-in connector: %+v", name, resp)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := generates.Load(); n != 0 {
		t.Fatalf("the generator was asked to generate %d times, want 0", n)
	}
}

// Only an owner or admin generates, and an agent token or Ask rsync.ai also needs
// the connectors:generate scope. A refused caller never reaches the generator,
// and is told the reason that applies to it.
func TestGenerationRefusesACallerWhoMayNotGenerate(t *testing.T) {
	refused := []struct {
		name  string
		actor services.Actor
		says  string
	}{
		{"the REST channel", services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner, Channel: services.ChannelREST}, "Generate page"},
		{"a token without the scope", agentWith(security.WSAdmin, services.ScopeMetadataRead), services.ScopeConnectorsGenerate},
		{"a token without any scope", agentWith(security.WSOwner), services.ScopeConnectorsGenerate},
		{"a member's token with the scope", agentWith(security.WSMember, services.ScopeConnectorsGenerate), "not an admin or owner"},
		{"a viewer's token with the scope", agentWith(security.WSViewer, services.ScopeConnectorsGenerate), "not an admin or owner"},
		{"Ask rsync.ai for a member", assistantAs(security.WSMember), "Only a workspace admin or owner"},
		{"the chat for a member", services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSMember, Channel: services.ChannelChat}, "Only a workspace admin or owner"},
		{"the chat for a viewer", services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSViewer, Channel: services.ChannelChat}, "Only a workspace admin or owner"},
		{"no active workspace", services.Actor{UserID: wsScopeUser, Role: security.WSOwner, Channel: services.ChannelChat}, "forbidden"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			pinNoCatalogDB(t)
			g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
			freshGenLimiter(t)
			_, err := startConnectorGeneration(context.Background(),
				generationCaller{actor: tc.actor, userText: "use " + genSpecURL}, genInput)
			r := refusalOf(t, err)
			if r.Code != "forbidden" || r.Status != http.StatusForbidden {
				t.Fatalf("refusal = %s/%d, want forbidden/403", r.Code, r.Status)
			}
			if tc.says != "forbidden" && !strings.Contains(r.Message, tc.says) {
				t.Errorf("message %q does not say %q", r.Message, tc.says)
			}
			if g.health.Load() != 0 || g.generates.Load() != 0 {
				t.Errorf("a refused caller reached the generator (health %d, generate %d)", g.health.Load(), g.generates.Load())
			}
		})
	}

	// Control: each channel's admin or owner passes every check, so the rows
	// above were refused for who they are and not for their input.
	allowed := map[string]services.Actor{
		"an admin's token with the scope": agentWith(security.WSAdmin, services.ScopeConnectorsGenerate),
		"an owner's token with the scope": agentWith(security.WSOwner, services.ScopeMetadataRead, services.ScopeConnectorsGenerate),
		"Ask rsync.ai for an admin":       assistantAs(security.WSAdmin),
		"the chat for an owner":           {UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner, Channel: services.ChannelChat},
	}
	for name, actor := range allowed {
		t.Run("allowed: "+name, func(t *testing.T) {
			pinNoCatalogDB(t)
			newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
			freshGenLimiter(t)
			_, err := startConnectorGeneration(context.Background(),
				generationCaller{actor: actor, userText: "use " + genSpecURL}, genInput)
			if !reachedAdmission(err) {
				t.Fatalf("an allowed caller did not reach the run row: %v", err)
			}
		})
	}
}

// Every input check runs before the generator is asked anything: the name, the
// connectors it must not overwrite, and a URL the generator would fetch.
func TestGenerationChecksItsInputBeforeAskingTheGenerator(t *testing.T) {
	cases := []struct {
		name string
		in   GenerationInput
		code string
		says string
	}{
		{"not kebab-case", GenerationInput{Name: "Acme_CRM", SpecURL: genSpecURL}, "invalid_name", `Try "acme-crm"`},
		{"leading hyphen", GenerationInput{Name: "-acme", SpecURL: genSpecURL}, "invalid_name", ""},
		{"over 100 characters", GenerationInput{Name: strings.Repeat("a", 101), SpecURL: genSpecURL}, "invalid_name", "100"},
		{"a built-in connector", GenerationInput{Name: "postgresql", SpecURL: genSpecURL}, "protected_connector", "built-in"},
		{"an alias of a built-in", GenerationInput{Name: "postgres", SpecURL: genSpecURL}, "protected_connector", "postgresql"},
		{"the s3 alias", GenerationInput{Name: "s3", SpecURL: genSpecURL}, "protected_connector", "aws-s3"},
		{"an internal connector", GenerationInput{Name: "debezium", SpecURL: genSpecURL}, "protected_connector", ""},
		{"a test- name the list hides", GenerationInput{Name: "test-acme", SpecURL: genSpecURL}, "hidden_name", "hidden from the connector list"},
		{"a dev-only name the list hides", GenerationInput{Name: "postgresql-generated", SpecURL: genSpecURL}, "hidden_name", "postgresql-generated"},
		{"spec and docs both", GenerationInput{Name: "acme-crm", SpecURL: genSpecURL, DocsURL: "https://docs.acme.example/"}, "invalid_url", "not both"},
		{"no URL from an agent", GenerationInput{Name: "acme-crm"}, "url_required", "spec_url"},
		{"plain http", GenerationInput{Name: "acme-crm", SpecURL: "http://api.acme.example/openapi.json"}, "invalid_url", "https://"},
		{"another scheme", GenerationInput{Name: "acme-crm", DocsURL: "ftp://api.acme.example/spec"}, "invalid_url", "https://"},
		{"no host", GenerationInput{Name: "acme-crm", SpecURL: "https:///openapi.json"}, "invalid_url", ""},
		{"a private IPv4", GenerationInput{Name: "acme-crm", SpecURL: "https://10.0.0.5/openapi.json"}, "invalid_url", "IP address"},
		{"the metadata IP", GenerationInput{Name: "acme-crm", SpecURL: "https://169.254.169.254/latest/meta-data"}, "invalid_url", "IP address"},
		{"an IPv6 literal", GenerationInput{Name: "acme-crm", SpecURL: "https://[::1]/openapi.json"}, "invalid_url", "IP address"},
		{"credentials in the URL", GenerationInput{Name: "acme-crm", SpecURL: "https://me:secret@api.acme.example/openapi.json"}, "invalid_url", "password"},
		{"localhost", GenerationInput{Name: "acme-crm", SpecURL: "https://localhost/openapi.json"}, "invalid_url", "public host"},
		{"a .localhost name", GenerationInput{Name: "acme-crm", SpecURL: "https://api.localhost/openapi.json"}, "invalid_url", "public host"},
		{"a .internal name", GenerationInput{Name: "acme-crm", SpecURL: "https://metadata.google.internal/computeMetadata/v1"}, "invalid_url", "public host"},
		{"a .local name", GenerationInput{Name: "acme-crm", DocsURL: "https://printer.local/docs"}, "invalid_url", "public host"},
		{"a single-label name", GenerationInput{Name: "acme-crm", SpecURL: "https://intranet/openapi.json"}, "invalid_url", "public host"},
		{"a URL over the limit", GenerationInput{Name: "acme-crm", SpecURL: "https://api.acme.example/" + strings.Repeat("a", maxGenerationURLLen)}, "invalid_url", "longer than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinNoCatalogDB(t)
			g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
			freshGenLimiter(t)
			_, err := startConnectorGeneration(context.Background(),
				generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)}, tc.in)
			r := refusalOf(t, err)
			if r.Code != tc.code {
				t.Fatalf("refusal code = %s (%q), want %s", r.Code, r.Message, tc.code)
			}
			if tc.says != "" && !strings.Contains(r.Message, tc.says) {
				t.Errorf("message %q does not say %q", r.Message, tc.says)
			}
			if g.health.Load() != 0 || g.generates.Load() != 0 {
				t.Errorf("a refused input reached the generator (health %d, generate %d)", g.health.Load(), g.generates.Load())
			}
		})
	}

	t.Run("a connector already on disk", func(t *testing.T) {
		pinNoCatalogDB(t)
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		writeDiskConnector(t, g.catalog, "acme-crm", map[string]any{"id": "acme-crm", "connector_type": "acme-crm"})
		_, err := startConnectorGeneration(context.Background(),
			generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)}, genInput)
		if r := refusalOf(t, err); r.Code != "already_exists" || r.Status != http.StatusConflict {
			t.Fatalf("refusal = %s/%d, want already_exists/409", r.Code, r.Status)
		}
		// Control: another name in the same catalog passes.
		_, err = startConnectorGeneration(context.Background(),
			generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)},
			GenerationInput{Name: "acme-erp", SpecURL: genSpecURL})
		if !reachedAdmission(err) {
			t.Fatalf("a new name was refused: %v", err)
		}
	})

	t.Run("the chat may name a connector without a URL", func(t *testing.T) {
		pinNoCatalogDB(t)
		newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		_, err := startConnectorGeneration(context.Background(), generationCaller{actor: chatAdmin()}, GenerationInput{Name: "acme-crm"})
		if !reachedAdmission(err) {
			t.Fatalf("the chat's name-only request was refused: %v", err)
		}
	})
}

// Ask rsync.ai's model chooses the URL it passes, so the URL must be one the user
// typed: a URL the model found by itself is never fetched.
func TestAskRsyncGeneratesOnlyFromAURLTheUserTyped(t *testing.T) {
	cases := []struct {
		name     string
		userText string
		in       GenerationInput
		want     string // a refusal code, or "" for one that passes
	}{
		{"no user text", "", genInput, "url_not_from_user"},
		{"the user named no URL", "make a connector for the Acme CRM", genInput, "url_not_from_user"},
		{"the user typed another URL", "use https://api.acme.example/v2/openapi.json", GenerationInput{Name: "acme-crm", SpecURL: "https://evil.example/openapi.json"}, "url_not_from_user"},
		{"a docs URL the user did not type", "generate acme-crm from " + genSpecURL, GenerationInput{Name: "acme-crm", DocsURL: "https://docs.evil.example/"}, "url_not_from_user"},
		{"no URL at all", "generate acme-crm", GenerationInput{Name: "acme-crm"}, "url_required"},
		{"the user's own URL", "generate acme-crm from " + genSpecURL + " please", genInput, ""},
		{"the user's URL with a slash added", "docs are at https://docs.acme.example/api", GenerationInput{Name: "acme-crm", DocsURL: "https://docs.acme.example/api/"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinNoCatalogDB(t)
			g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
			freshGenLimiter(t)
			_, err := startConnectorGeneration(context.Background(),
				generationCaller{actor: assistantAs(security.WSAdmin), userText: tc.userText}, tc.in)
			if tc.want == "" {
				if !reachedAdmission(err) {
					t.Fatalf("the user's own URL was refused: %v", err)
				}
				return
			}
			if r := refusalOf(t, err); r.Code != tc.want {
				t.Fatalf("refusal = %s (%q), want %s", r.Code, r.Message, tc.want)
			}
			if g.health.Load() != 0 {
				t.Error("a refused URL reached the generator")
			}
		})
	}
}

// What the generator installed here can do is read from its /health: an
// edition that generates from an uploaded spec only, or a catalog no other
// service reads (Helm), is refused before a run is recorded.
func TestGenerationAsksTheGeneratorWhatItCanDo(t *testing.T) {
	cases := []struct {
		name   string
		status int
		health string
		want   string
	}{
		{"spec upload only", http.StatusOK, `{"status":"healthy","generate_from_url":false}`, "upload_spec_required"},
		{"a private catalog", http.StatusOK, `{"status":"healthy","catalog_shared":false}`, "private_catalog"},
		{"unhealthy", http.StatusServiceUnavailable, `{}`, "generator_unavailable"},
		{"an unreadable answer", http.StatusOK, `<html>`, "generator_unavailable"},
		{"an older generator", http.StatusOK, `{"status":"healthy"}`, ""},
		{"the full flow", http.StatusOK, `{"generate_from_url":true,"catalog_shared":true}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinNoCatalogDB(t)
			g := newFakeGenerator(t, tc.status, tc.health, http.StatusOK, genOK)
			freshGenLimiter(t)
			_, err := startConnectorGeneration(context.Background(),
				generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)}, genInput)
			if g.health.Load() != 1 {
				t.Fatalf("health probed %d times, want 1", g.health.Load())
			}
			if tc.want == "" {
				if !reachedAdmission(err) {
					t.Fatalf("a generator that can generate was refused: %v", err)
				}
				return
			}
			if r := refusalOf(t, err); r.Code != tc.want {
				t.Fatalf("refusal = %s (%q), want %s", r.Code, r.Message, tc.want)
			}
			if g.generates.Load() != 0 {
				t.Error("a refused generation reached /v1/generate")
			}
		})
	}

	t.Run("the generator is down", func(t *testing.T) {
		pinNoCatalogDB(t)
		newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		down := httptest.NewServer(http.NotFoundHandler())
		down.Close()
		t.Setenv("TOOL_GENERATOR_URL", down.URL)
		_, err := startConnectorGeneration(context.Background(),
			generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)}, genInput)
		if r := refusalOf(t, err); r.Code != "generator_unavailable" || r.Status != http.StatusServiceUnavailable {
			t.Fatalf("refusal = %s/%d, want generator_unavailable/503", r.Code, r.Status)
		}
	})
}

// An agent spends the same hourly budget the Generate page does, per workspace.
func TestGenerationMeetsTheHourlyLimit(t *testing.T) {
	pinNoCatalogDB(t)
	newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
	freshGenLimiter(t)
	now := time.Now()
	for i := 0; i < 10; i++ {
		connectorGenLimiter.Allow(wsScopeWS, now)
	}
	agent := agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)
	_, err := startConnectorGeneration(context.Background(), generationCaller{actor: agent}, genInput)
	r := refusalOf(t, err)
	if r.Code != "rate_limited" || r.Status != http.StatusTooManyRequests {
		t.Fatalf("refusal = %s/%d, want rate_limited/429", r.Code, r.Status)
	}
	if r.RetryAfter <= 0 || r.RetryAfter > time.Hour {
		t.Errorf("RetryAfter = %s, want within the hour", r.RetryAfter)
	}

	// Control: the limit is the workspace's, not the instance's.
	other := agent
	other.WorkspaceID = "55555555-5555-5555-5555-555555555555"
	if _, err := startConnectorGeneration(context.Background(), generationCaller{actor: other}, genInput); !reachedAdmission(err) {
		t.Fatalf("another workspace was limited too: %v", err)
	}
}

var (
	genLockSQL   = regexp.QuoteMeta(`SELECT pg_advisory_xact_lock(hashtext($1))`)
	genStaleSQL  = `UPDATE connector_generation_runs\s+SET status = 'failed'.*WHERE status = 'running' AND started_at < NOW\(\) - INTERVAL '20 minutes'`
	genCountSQL  = regexp.QuoteMeta(`SELECT COUNT(*) FROM connector_generation_runs WHERE workspace_id = $1 AND status = 'running'`)
	genInsertSQL = `INSERT INTO connector_generation_runs \(workspace_id, user_id, actor_kind, token_id, connector_name, source_url\)`
	genFinishSQL = `UPDATE connector_generation_runs\s+SET status = \$2, version = NULLIF\(\$3, ''\)`
)

// expectAdmission expects the run row's transaction up to the running count.
func expectAdmission(mock sqlmock.Sqlmock, running int) {
	mock.ExpectBegin()
	mock.ExpectExec(genLockSQL).WithArgs("connector_generation:" + wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(genStaleSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(genCountSQL).WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(running))
}

// A workspace runs at most two generations at once, and one connector name is
// generated once at a time across workspaces (the partial unique index).
func TestGenerationAdmitsARunOnlyWithinTheLimits(t *testing.T) {
	agent := agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)

	t.Run("two already running", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		expectAdmission(mock, maxRunningGenerations)
		mock.ExpectRollback()

		_, err := startConnectorGeneration(context.Background(), generationCaller{actor: agent}, genInput)
		if r := refusalOf(t, err); r.Code != "too_many_running" || r.Status != http.StatusTooManyRequests {
			t.Fatalf("refusal = %s/%d, want too_many_running/429", r.Code, r.Status)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		if g.generates.Load() != 0 {
			t.Error("a refused run reached the generator")
		}
	})

	t.Run("the name is being generated elsewhere", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		expectAdmission(mock, 0)
		mock.ExpectQuery(genInsertSQL).WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
		mock.ExpectRollback()

		_, err := startConnectorGeneration(context.Background(), generationCaller{actor: agent}, genInput)
		if r := refusalOf(t, err); r.Code != "already_running" || r.Status != http.StatusConflict {
			t.Fatalf("refusal = %s/%d, want already_running/409", r.Code, r.Status)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		if g.generates.Load() != 0 {
			t.Error("a refused run reached the generator")
		}
	})

	t.Run("any other database error is not a refusal", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		expectAdmission(mock, 0)
		mock.ExpectQuery(genInsertSQL).WillReturnError(errors.New("connection reset"))
		mock.ExpectRollback()

		_, err := startConnectorGeneration(context.Background(), generationCaller{actor: agent}, genInput)
		var r *GenerationRefusal
		if err == nil || errors.As(err, &r) {
			t.Fatalf("err = %v, want the database error itself", err)
		}
	})
}

// A started run is recorded as running, sent to the generator with the
// caller's workspace, and recorded again when the generator answers.
func TestAGenerationRunIsRecordedFromStartToEnd(t *testing.T) {
	syncLaunch(t)
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-internal-secret")

	t.Run("an agent's run that succeeds", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		expectAdmission(mock, 1)
		mock.ExpectQuery(genInsertSQL).
			WithArgs(wsScopeWS, wsScopeUser, "agent", genTokenID, "acme-crm", genSpecURL).
			WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow(genRunID, started))
		mock.ExpectCommit()
		mock.ExpectExec(genFinishSQL).
			WithArgs(genRunID, "succeeded", "v1.0.0", sql.NullBool{}, sql.NullInt64{Int64: 3, Valid: true}, "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		run, err := startConnectorGeneration(context.Background(),
			generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate), tokenID: genTokenID},
			GenerationInput{Name: "acme-crm", SpecURL: genSpecURL, Description: "  Acme's CRM  "})
		if err != nil {
			t.Fatal(err)
		}
		if run.ID != genRunID || run.Status != "running" || run.ActorKind != "agent" || !run.StartedAt.Equal(started) {
			t.Errorf("run = %+v", run)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}

		body, header := g.lastRequest()
		want := map[string]any{
			"api_name": "acme-crm", "description": "Acme's CRM", "openapi_spec_url": genSpecURL,
			"force_regenerate": false, "save_artifacts": true, "enable_chaos": true,
		}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("generator got %s = %v, want %v", k, body[k], v)
			}
		}
		if _, ok := body["docs_url"]; ok {
			t.Error("a spec URL was also sent as docs_url")
		}
		if got := header.Get(workspaceHeader); got != wsScopeWS {
			t.Errorf("the generator was told workspace %q, want %q (its egress log names it)", got, wsScopeWS)
		}
		if got := header.Get("X-Internal-Secret"); got != "unit-test-internal-secret" {
			t.Errorf("X-Internal-Secret = %q: the generator refuses /v1/generate without it", got)
		}
	})

	t.Run("a chat run from a name only", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		freshGenLimiter(t)
		expectAdmission(mock, 0)
		mock.ExpectQuery(genInsertSQL).
			WithArgs(wsScopeWS, wsScopeUser, "chat", "", "acme-crm", "").
			WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow(genRunID, time.Now()))
		mock.ExpectCommit()
		mock.ExpectExec(genFinishSQL).WithArgs(genRunID, "succeeded", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		if _, err := startConnectorGeneration(context.Background(), generationCaller{actor: chatAdmin()}, GenerationInput{Name: "acme-crm"}); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		body, _ := g.lastRequest()
		if _, ok := body["openapi_spec_url"]; ok {
			t.Error("a name-only run sent a spec URL")
		}
		if body["description"] != "User-requested connector for acme-crm" {
			t.Errorf("description = %v", body["description"])
		}
	})

	t.Run("a run that fails", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		// The generator's error is shown to Ask rsync.ai's model and to agents, so
		// a value inside it must not survive, and it is cut short.
		detail := "Could not reach the API: duplicate key value 'alice@example.com' " + strings.Repeat("and more ", 100)
		newFakeGenerator(t, http.StatusOK, `{}`, http.StatusBadGateway, `{"detail": "`+detail+`"}`)
		freshGenLimiter(t)
		expectAdmission(mock, 0)
		mock.ExpectQuery(genInsertSQL).
			WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow(genRunID, time.Now()))
		mock.ExpectCommit()
		var recorded string
		mock.ExpectExec(genFinishSQL).
			WithArgs(genRunID, "failed", "", sql.NullBool{}, sql.NullInt64{}, capture{&recorded}).
			WillReturnResult(sqlmock.NewResult(0, 1))

		if _, err := startConnectorGeneration(context.Background(),
			generationCaller{actor: agentWith(security.WSAdmin, services.ScopeConnectorsGenerate)}, genInput); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(recorded, "Could not reach the API") {
			t.Errorf("the recorded error lost the generator's reason: %q", recorded)
		}
		if strings.Contains(recorded, "alice@example.com") {
			t.Errorf("a value in the generator's error was recorded unscrubbed: %q", recorded)
		}
		if n := utf8.RuneCountInString(recorded); n > maxGenerationErrRunes {
			t.Errorf("the recorded error is %d runes, want at most %d", n, maxGenerationErrRunes)
		}
	})
}

// Through the gin entry point, Ask rsync.ai's run takes the user's words from the
// request and records no token: the assistant runs on the user's session.
func TestStartConnectorGeneration_AskRsyncReadsTheUsersWords(t *testing.T) {
	syncLaunch(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
	freshGenLimiter(t)
	expectAdmission(mock, 0)
	mock.ExpectQuery(genInsertSQL).
		WithArgs(wsScopeWS, wsScopeUser, "assistant", "", "acme-crm", genSpecURL).
		WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow(genRunID, time.Now()))
	mock.ExpectCommit()
	mock.ExpectExec(genFinishSQL).WillReturnResult(sqlmock.NewResult(0, 1))

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/assistant/ask", nil)
	c.Set(services.CtxUserText, "please generate acme-crm from "+genSpecURL)
	c.Set(ctxTokenID, genTokenID) // not an agent: must not be recorded

	run, err := StartConnectorGeneration(c, assistantAs(security.WSAdmin), genInput)
	if err != nil {
		t.Fatal(err)
	}
	if run.ActorKind != "assistant" {
		t.Errorf("actor kind = %q, want assistant", run.ActorKind)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A run whose process stopped is failed by the sweep, and only such a run: the
// sweep's age is longer than a run's own limit, so it never fails a live one.
func TestTheSweepFailsOnlyRunsAProcessLeftBehind(t *testing.T) {
	m := regexp.MustCompile(`INTERVAL '(\d+) minutes'`).FindStringSubmatch(staleGenerationSQL)
	if m == nil {
		t.Fatal("staleGenerationSQL has no INTERVAL in minutes")
	}
	if got := m[1] + "m"; got != generationStaleAfter.String()[:len(got)] || generationStaleAfter != 20*time.Minute {
		t.Fatalf("the sweep's SQL waits %s minutes, generationStaleAfter is %s", m[1], generationStaleAfter)
	}
	if generationStaleAfter <= generationRunLimit {
		t.Fatalf("the sweep fails runs after %s but a run may last %s: it would fail live runs",
			generationStaleAfter, generationRunLimit)
	}

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectExec(genStaleSQL).WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := SweepStaleConnectorGenerations(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("sweep = %d, %v; want 3, nil", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A run is read back only in the workspace that started it.
func TestGetConnectorGeneration_IsScopedToTheActiveWorkspace(t *testing.T) {
	cols := []string{"id", "connector_name", "status", "source_url", "actor_kind", "version",
		"validation_passed", "tool_count", "error", "started_at", "finished_at"}
	getSQL := `FROM connector_generation_runs WHERE id = \$1 AND workspace_id = \$2`

	t.Run("its own run", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		mock.ExpectQuery(getSQL).WithArgs(genRunID, wsScopeWS).
			WillReturnRows(sqlmock.NewRows(cols).AddRow(genRunID, "acme-crm", "succeeded", genSpecURL, "agent",
				"v1.0.0", true, 3, "", start, start.Add(4*time.Minute)))
		w := httptest.NewRecorder()
		wsScopeRouter(http.MethodGet, "/connectors/generations/:id", GetConnectorGeneration).
			ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connectors/generations/"+genRunID, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var run GenerationRun
		if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		if run.Status != "succeeded" || run.ToolCount == nil || *run.ToolCount != 3 || run.FinishedAt == nil ||
			run.ValidationPassed == nil || !*run.ValidationPassed {
			t.Errorf("run = %+v", run)
		}
	})

	t.Run("another workspace's run", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(getSQL).WithArgs(genRunID, wsScopeWS).WillReturnRows(sqlmock.NewRows(cols))
		w := httptest.NewRecorder()
		wsScopeRouter(http.MethodGet, "/connectors/generations/:id", GetConnectorGeneration).
			ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connectors/generations/"+genRunID, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", w.Code)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("not a run id", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		w := httptest.NewRecorder()
		wsScopeRouter(http.MethodGet, "/connectors/generations/:id", GetConnectorGeneration).
			ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/connectors/generations/x';--", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", w.Code)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// The connector page says an agent generated a connector to everyone, and
// names the token only to the workspace whose run it was.
func TestConnectorProvenanceNamesTheTokenOnlyToItsWorkspace(t *testing.T) {
	cols := []string{"actor_kind", "workspace_id", "name", "finished_at"}
	at := time.Date(2026, 10, 1, 12, 4, 0, 0, time.UTC)
	cases := []struct {
		name, kind, viewer string
		want               *ConnectorProvenance
	}{
		{"the token's workspace", "agent", wsScopeWS, &ConnectorProvenance{GeneratedByAgent: true, GeneratedAt: at, By: "ci token"}},
		{"another workspace", "agent", "55555555-5555-5555-5555-555555555555", &ConnectorProvenance{GeneratedByAgent: true, GeneratedAt: at}},
		{"Ask rsync.ai", "assistant", wsScopeWS, &ConnectorProvenance{GeneratedByAgent: true, GeneratedAt: at, By: "Ask rsync.ai"}},
		{"a person on the Generate page", "ui", wsScopeWS, nil},
		{"the chat command", "chat", wsScopeWS, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(`FROM connector_generation_runs r LEFT JOIN agent_tokens t`).WithArgs("acme-crm").
				WillReturnRows(sqlmock.NewRows(cols).AddRow(tc.kind, wsScopeWS, "ci token", at))
			got := connectorProvenance(context.Background(), "acme-crm", tc.viewer)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("provenance = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A run's validation_passed comes from the generator's /v1/generate answer,
// whose contract (llm-service contracts/generate_v1.py GenerateResponseV1) has
// no validation_passed field: the fast and spec-first paths report their check
// as a "preflight" stage, the agentic path as a "Validation" stage, and an
// agentic draft sets is_draft. Each answer below is in that real shape, so a
// run reads the check the generator actually made, or nothing when it made none.
func TestARunRecordsTheValidationTheGeneratorReported(t *testing.T) {
	cases := []struct {
		name, answer string
		want         sql.NullBool
	}{
		{"fast path, preflight passed",
			`{"success": true, "version": "v1.0.0", "operation_count": 1, "workflow_stages": [{"name": "build", "status": "completed"}, {"name": "preflight", "status": "completed"}]}`,
			sql.NullBool{Bool: true, Valid: true}},
		{"fast path, preflight failed",
			`{"success": true, "version": "v1.0.0", "workflow_stages": [{"name": "preflight", "status": "failed"}]}`,
			sql.NullBool{Bool: false, Valid: true}},
		{"agentic path, validation passed",
			`{"success": true, "version": "v1.0.0", "workflow_stages": [{"name": "Code Generation", "status": "completed"}, {"name": "Validation", "status": "completed"}]}`,
			sql.NullBool{Bool: true, Valid: true}},
		{"agentic draft",
			`{"success": true, "is_draft": true, "workflow_stages": [{"name": "Validation", "status": "completed"}]}`,
			sql.NullBool{Bool: false, Valid: true}},
		{"no check reported",
			`{"success": true, "version": "v1.0.0", "workflow_stages": [{"name": "build", "status": "completed"}]}`,
			sql.NullBool{}},
	}
	for _, tc := range cases {
		var payload map[string]any
		if err := json.Unmarshal([]byte(tc.answer), &payload); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := outcomeOf(generatorResult{status: http.StatusOK, payload: payload})
		if got.status != "succeeded" || got.validationPassed != tc.want {
			t.Errorf("%s: status %q, validation_passed %+v; want succeeded, %+v", tc.name, got.status, got.validationPassed, tc.want)
		}
	}
}
