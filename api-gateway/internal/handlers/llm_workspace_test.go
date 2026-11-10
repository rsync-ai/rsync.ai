package handlers

// A hop to a service that makes model calls carries the request's workspace.
//
// Live on 2026-09-30 every llm_egress line read workspace=-: the gateway knew
// the workspace, but no request it built for llm-service or the tool-generator
// said so. The bug class is a hop that drops the tenant. These tests cover the
// hop's two halves: the middleware puts the checked workspace on the request
// context, and every request built for one of those services is stamped from
// that context -- including the goroutines that outlive the request.

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// llmHopEngine is wsMiddlewareEngine with a probe that builds an outbound
// request from the request context, stamps it, and echoes what would be sent.
func llmHopEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		c.Next()
	})
	api := r.Group("/api/v1")
	api.Use(WorkspaceContextMiddleware())
	probe := func(c *gin.Context) {
		req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://llm-service:5000/v1/completion", nil)
		stampLLMWorkspace(req)
		c.String(http.StatusOK, req.Header.Get(workspaceHeader))
	}
	api.GET("/probe", probe)
	api.GET("/workspaces", probe) // optional-context route
	return r
}

func TestLLMHop_TheMiddlewarePutsTheCheckedWorkspaceOnTheContext(t *testing.T) {
	cases := []struct {
		name, path, header, want string
		setup                    func(sqlmock.Sqlmock)
	}{
		{"member header", "/api/v1/probe", wsTeam, wsTeam, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(membershipQ).WithArgs(wsTeam, wsUser).
				WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
		}},
		{"no selection", "/api/v1/probe", "", wsPersonal, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(personalQ).WithArgs(wsUser).
				WillReturnRows(sqlmock.NewRows([]string{"id", "role"}).AddRow(wsPersonal, "owner"))
		}},
		// A header naming a workspace the caller is not in never goes out:
		// the hop names the workspace the middleware fell back to.
		{"non-member header on an optional route", "/api/v1/workspaces", wsStale, wsPersonal, func(m sqlmock.Sqlmock) {
			m.ExpectQuery(membershipQ).WithArgs(wsStale, wsUser).WillReturnError(sql.ErrNoRows)
			m.ExpectQuery(personalQ).WithArgs(wsUser).
				WillReturnRows(sqlmock.NewRows([]string{"id", "role"}).AddRow(wsPersonal, "owner"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, mock, cleanup := setupMockDB(t)
			defer cleanup()
			tc.setup(mock)
			w := wsReq(llmHopEngine(), tc.path, wsUser, tc.header, "")
			if w.Code != http.StatusOK || w.Body.String() != tc.want {
				t.Fatalf("hop sent X-Workspace-ID %q (status %d), want %q", w.Body.String(), w.Code, tc.want)
			}
		})
	}
}

func TestLLMHop_AStampDropsAValueTheContextDoesNotCarry(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://tool-generator:5010/v1/generate", nil)
	req.Header.Set(workspaceHeader, wsStale)
	stampLLMWorkspace(req)
	if got := req.Header.Get(workspaceHeader); got != "" {
		t.Fatalf("an unchecked X-Workspace-ID went out: %q", got)
	}
}

// seenWorkspace is an upstream that records the X-Workspace-ID of each call.
func seenWorkspace(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(workspaceHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestLLMHop_RankTablesNamesTheWorkspace(t *testing.T) {
	srv, seen := seenWorkspace(t)
	t.Setenv("LLM_SERVICE_URL", srv.URL)
	tables := []TableMetadata{{Name: "orders"}, {Name: "customers"}}
	_, _ = rankTablesByLLM(withLLMWorkspace(t.Context(), wsTeam), tables, "orders", "", 5)
	if got := seen(); len(got) != 1 || got[0] != wsTeam {
		t.Fatalf("llm-service saw X-Workspace-ID %q, want [%s]", got, wsTeam)
	}
}

// The table-suggestion ranking runs in a goroutine after the /state poll has
// answered, so it cannot use the request's context -- but it keeps its workspace.
func TestLLMHop_BackgroundTableSuggestionsKeepTheWorkspace(t *testing.T) {
	srv, seen := seenWorkspace(t)
	t.Setenv("LLM_SERVICE_URL", srv.URL)
	database, mock, cleanup := setupMockDB(t)
	defer cleanup()
	mock.ExpectExec(`UPDATE pipeline_progress`).WillReturnResult(sqlmock.NewResult(0, 1))

	parent, cancel := context.WithCancel(withLLMWorkspace(context.Background(), wsTeam))
	cancel() // the request is over by the time the ranking runs
	computeAndPersistTableSuggestions(parent, database, "p1", []TableMetadata{{Name: "orders"}}, "orders")
	if got := seen(); len(got) != 1 || got[0] != wsTeam {
		t.Fatalf("llm-service saw X-Workspace-ID %q, want [%s]", got, wsTeam)
	}
}

// A background connector generation (an agent, Ask rsync.ai or the chat) runs
// after its request has answered, so it cannot use the request's context --
// but it keeps its workspace.
func TestLLMHop_BackgroundConnectorGenerationKeepsTheWorkspace(t *testing.T) {
	pinNoCatalogDB(t)
	srv, seen := seenWorkspace(t)
	t.Setenv("TOOL_GENERATOR_URL", srv.URL)
	executeGenerationRun("", wsTeam, map[string]any{"api_name": "acme"})
	if got := seen(); len(got) != 1 || got[0] != wsTeam {
		t.Fatalf("tool-generator saw X-Workspace-ID %q, want [%s]", got, wsTeam)
	}
}

// modelServiceEnv names the services that make model calls, as gateway code
// reads their address.
var modelServiceEnv = map[string]bool{
	`"LLM_SERVICE_URL"`:    true,
	`"TOOL_GENERATOR_URL"`: true,
	`"TEXT2SQL_ENDPOINT"`:  true,
}

type modelHop struct {
	name       string
	stamped    bool
	background bool
}

// modelServiceHops returns each api-gateway function that reads a model
// service's address and builds an HTTP request. A function that reads the
// address and builds nothing (toolGeneratorBaseURL) is an address helper: a
// call to it counts as reading the address, so a hop that takes its address
// from a helper is not missed.
func modelServiceHops(t *testing.T) []modelHop {
	t.Helper()
	type parsed struct {
		rel  string
		fset *token.FileSet
		file *ast.File
	}
	var files []parsed
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, parsed{rel, fset, file})
		return nil
	})
	if err != nil {
		t.Fatalf("scan api-gateway: %v", err)
	}

	type facts struct{ readsAddr, builds, stamped, background bool }
	inspect := func(fn *ast.FuncDecl, helpers map[string]bool) facts {
		var f facts
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if modelServiceEnv[x.Value] {
					f.readsAddr = true
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && helpers[id.Name] {
					f.readsAddr = true
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					switch {
					case id.Name == "http" && strings.HasPrefix(x.Sel.Name, "NewRequest"):
						f.builds = true
					case id.Name == "context" && (x.Sel.Name == "Background" || x.Sel.Name == "TODO"):
						f.background = true
					}
				}
			case *ast.Ident:
				if x.Name == "stampLLMWorkspace" || x.Name == "StampLLMWorkspace" {
					f.stamped = true
				}
			}
			return true
		})
		return f
	}
	funcs := func(yield func(parsed, *ast.FuncDecl)) {
		for _, p := range files {
			for _, decl := range p.file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
					yield(p, fn)
				}
			}
		}
	}

	helpers := map[string]bool{}
	funcs(func(_ parsed, fn *ast.FuncDecl) {
		if f := inspect(fn, nil); f.readsAddr && !f.builds && fn.Recv == nil {
			helpers[fn.Name.Name] = true
		}
	})
	var hops []modelHop
	funcs(func(p parsed, fn *ast.FuncDecl) {
		if f := inspect(fn, helpers); f.readsAddr && f.builds {
			hops = append(hops, modelHop{
				name:       p.rel + ":" + strconv.Itoa(p.fset.Position(fn.Pos()).Line) + " " + fn.Name.Name,
				stamped:    f.stamped,
				background: f.background,
			})
		}
	})
	sort.Slice(hops, func(i, j int) bool { return hops[i].name < hops[j].name })
	return hops
}

func TestLLMHop_ControlTheScanFindsTheKnownHops(t *testing.T) {
	names := map[string]bool{}
	for _, h := range modelServiceHops(t) {
		names[h.name[strings.LastIndex(h.name, " ")+1:]] = true
	}
	for _, want := range []string{
		"GenerateSQL", "ResolveExplorerTables", "GenerateSuggestions", "rankTablesByLLM",
		"callDiagnoseLLM", "parseIntent", "postToolGenerator", "probeGenerator", "ToolGeneratorProxy",
		"callLLMService",
	} {
		if !names[want] {
			t.Errorf("the scan no longer finds %s; it would pass without checking it (found %v)", want, names)
		}
	}
}

func TestLLMHop_EveryRequestToAModelServiceNamesTheWorkspace(t *testing.T) {
	for _, h := range modelServiceHops(t) {
		if !h.stamped {
			t.Errorf("%s calls a service that makes model calls without stampLLMWorkspace(req): its egress lines say workspace=-", h.name)
		}
		if h.background {
			t.Errorf("%s builds its hop on context.Background(), which carries no workspace: take the caller's context (detachedLLMContext for a goroutine)", h.name)
		}
	}
}
