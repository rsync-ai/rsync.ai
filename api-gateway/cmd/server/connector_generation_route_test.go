package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// GET /connectors/generations/:id sits beside GET /connectors/:name. An agent
// polls the first for its run, and the connector page reads the second, so
// each must reach its own handler: a run id must not be read as a connector
// named "generations", and a connector must not be shadowed by the new path.
// The router is built inline in main, so this reads every /connectors route out
// of main.go and serves them together, each answering with its handler's name.
// gin panics on a registration that conflicts with another.
func TestConnectorGenerationRouteCoexistsWithTheConnectorRoutes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	routes := mainRoutes(t, string(src), "/connectors")
	if len(routes) < 5 {
		t.Fatalf("found %d /connectors routes in main.go; the reader is not finding them", len(routes))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	for _, rt := range routes {
		name := rt.handler
		r.Handle(rt.method, rt.path, func(c *gin.Context) { c.String(http.StatusOK, name) })
	}

	const runID = "44444444-4444-4444-4444-444444444444"
	cases := []struct{ path, want string }{
		{"/connectors/generations/" + runID, "GetConnectorGeneration"},
		{"/connectors/acme-crm", "GetMCPConnector"},
		{"/connectors/generations", "GetMCPConnector"}, // a connector may be named so; it is not a run
		{"/connectors/acme-crm/logo", "GetMCPConnectorLogo"},
		{"/connectors/namespace-models", "GetConnectorNamespaceModels"},
		{"/connectors", "ListMCPConnectors"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("GET %s reached %q (%d), want %s", tc.path, w.Body.String(), w.Code, tc.want)
		}
	}
}

type mainRoute struct{ method, path, handler string }

// mainRoutes returns every api.<METHOD>("<prefix>…", …, handlers.X) call in func
// main, with X as its handler.
func mainRoutes(t *testing.T, src, prefix string) []mainRoute {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	methods := map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
		http.MethodPatch: true, http.MethodDelete: true}
	var routes []mainRoute
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Recv != nil || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall || len(call.Args) < 2 {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || !methods[sel.Sel.Name] {
				return true
			}
			if recv, isIdent := sel.X.(*ast.Ident); !isIdent || recv.Name != "api" {
				return true
			}
			path, ok := stringLit(call.Args[0])
			if !ok || (path != prefix && !strings.HasPrefix(path, prefix+"/")) {
				return true
			}
			last, isSel := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			routes = append(routes, mainRoute{sel.Sel.Name, path, last.Sel.Name})
			return true
		})
	}
	return routes
}
