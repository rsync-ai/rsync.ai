package main

import (
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
	"testing"

	"github.com/gin-gonic/gin"
)

// notFromABrowser names each request header the gateway reads that a browser
// page never sets itself, so the CORS preflight need not allow it.
var notFromABrowser = map[string]string{
	"origin":                    "set by the browser, never by the page",
	"user-agent":                "set by the browser, never by the page",
	"x-slack-signature":         "sent by Slack's servers to the Slack webhook",
	"x-slack-request-timestamp": "sent by Slack's servers to the Slack webhook",
	"x-internal-secret":         "service-to-service only; a browser must never hold it",
	"x-user-role":               "dev-only identity override, ignored in production",
	"x-user-id":                 "dev-only identity header, refused in production (allowed in dev CORS)",
}

// TestCORSAllowsEveryHeaderABrowserClientSends: every request header the
// gateway reads is allowed by the production CORS preflight, unless it is
// named above. A header read but not allowed fails only in a browser, as an
// opaque "Failed to fetch": the MCP route reads MCP-Protocol-Version, which a
// browser MCP client sends on every request after initialize, and the
// preflight refused it. Bug class: a request header the gateway reads that
// its CORS does not allow.
func TestCORSAllowsEveryHeaderABrowserClientSends(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("RSYNC_CORS_ORIGINS", "https://app.example.com")

	e := gin.New()
	e.Use(corsMiddleware())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/agent/v1/mcp", nil)
	req.Header.Set("Origin", "https://app.example.com")
	e.ServeHTTP(w, req)
	allowed := map[string]bool{}
	for _, h := range strings.Split(w.Header().Get("Access-Control-Allow-Headers"), ",") {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}

	read := requestHeadersRead(t, "../..")
	if len(read) < 8 {
		t.Fatalf("found only %d request headers read (%v): the scan is broken, not the CORS", len(read), read)
	}
	for _, h := range read {
		if !allowed[h] && notFromABrowser[h] == "" {
			t.Errorf("the gateway reads request header %q but the production CORS preflight does not allow it; "+
				"add it to corsMiddleware's allowHeaders, or to notFromABrowser with why a browser never sends it", h)
		}
	}
	readSet := map[string]bool{}
	for _, h := range read {
		readSet[h] = true
	}
	for h := range notFromABrowser {
		if !readSet[h] {
			t.Errorf("notFromABrowser names %q, which nothing reads any more; drop it", h)
		}
	}
}

// requestHeadersRead lists, lowercased, every request header the module's
// non-test code reads by name: c.GetHeader(x) and <x>.Request.Header.Get(x),
// where x is a string literal or a string constant.
func requestHeadersRead(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "vendor" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	consts := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if vs, ok := n.(*ast.ValueSpec); ok {
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								consts[name.Name] = v
							}
						}
					}
				}
			}
			return true
		})
	}

	seen := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			isRead := sel.Sel.Name == "GetHeader"
			if hdr, ok := sel.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" && hdr.Sel.Name == "Header" {
				if r, ok := hdr.X.(*ast.SelectorExpr); ok && r.Sel.Name == "Request" {
					isRead = true
				}
			}
			if !isRead {
				return true
			}
			var name string
			switch a := call.Args[0].(type) {
			case *ast.BasicLit:
				name, _ = strconv.Unquote(a.Value)
			case *ast.Ident:
				name = consts[a.Name]
			}
			if name == "" {
				t.Errorf("%s: a header read by a name this scan cannot resolve; name it with a literal or a string constant",
					fset.Position(call.Pos()))
				return true
			}
			seen[strings.ToLower(name)] = true
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
