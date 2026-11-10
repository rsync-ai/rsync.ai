package handlers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// app.rsync.ai sits behind Cloudflare, which swaps an origin 502 or 504 for its
// own HTML page. A gateway answer with one of those statuses therefore never
// reaches the browser: whatever reason it carries is lost, and the UI shows a
// bare "HTTP 502". The table preview did exactly this until it moved to 424/500
// (sample_preview_status_test.go).
//
// This test counts every place the api-gateway uses http.StatusBadGateway or
// http.StatusGatewayTimeout as a value -- comparing a response status against
// them is fine and not counted -- and holds the count to a baseline of the
// sites that predate it. Those are filed as DX-EdgeReplaced502 in BACKLOG.md.
// A new site fails here; a fixed one fails until its baseline line is removed.

// edgeReplacedBaseline is the count per file:function. Shrink it, never grow it.
var edgeReplacedBaseline = map[string]int{
	"internal/agentgw/call.go:toolCall.do":                                          1,
	"internal/agentgw/models.go:createModel":                                        1,
	"internal/assistant/http.go:writeError":                                         1,
	"internal/assistant/llm.go:HTTPStepper.callLLMService":                          3,
	"internal/assistant/llm.go:stepRefusal":                                         3,
	"internal/handlers/cdc_tables.go:UpdatePipelineCDCTables":                       3,
	"internal/handlers/connector_generator.go:ToolGeneratorProxy":                   3,
	"internal/handlers/demo.go:respondDemoSeedFailure":                              1,
	"internal/handlers/diagnose.go:DiagnosePipeline":                                1,
	"internal/handlers/diagnose.go:callDiagnoseLLM":                                 2,
	"internal/handlers/errors.go:ParseDBError":                                      1,
	"internal/handlers/explorer.go:ExecuteExplorerQuery":                            2,
	"internal/handlers/explorer.go:GetExplorerNextSteps":                            1,
	"internal/handlers/explorer_documents.go:documentToolViaOrchestrator":           6,
	"internal/handlers/notification_channels.go:AdminTestNotificationChannel":       1,
	"internal/handlers/pii.go:PIIHandler.TriggerScan":                               1,
	"internal/handlers/pipeline_cdc.go:BackfillPipelineCDCTables":                   1,
	"internal/handlers/pipeline_cdc.go:GetPipelineCDCBackfillCapability":            1,
	"internal/handlers/pipeline_cdc.go:GetPipelineCDCStatus":                        1,
	"internal/handlers/pipeline_cdc.go:PausePipelineCDC":                            1,
	"internal/handlers/pipeline_cdc.go:RecoverPipelineCDC":                          1,
	"internal/handlers/pipeline_cdc.go:RestartPipelineCDC":                          1,
	"internal/handlers/pipeline_cdc.go:ResumePipelineCDC":                           1,
	"internal/handlers/pipeline_hitl.go:ResumePipelineTables":                       1,
	"internal/handlers/pipeline_tables.go:UpdatePipelineTables":                     1,
	"internal/handlers/pipelines.go:CreatePipeline":                                 1,
	"internal/handlers/pipelines.go:stopCDCStreaming":                               1,
	"internal/handlers/plan_upgrade.go:AuthHandler.UpgradeRequest":                  1,
	"internal/handlers/schema_evolution.go:removeTableFromPipeline":                 3,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.CheckCompatibility": 2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.DeleteSubject":      2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.GetConfig":          2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.GetRegistryInfo":    2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.GetSchema":          2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.GetSubjectVersions": 2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.ListSubjects":       2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.RegisterSchema":     2,
	"internal/handlers/schema_registry.go:SchemaRegistryHandler.SetConfig":          2,
	"internal/handlers/service_principal.go:browserStatusForUpstream":               1,
	"internal/handlers/silver_models.go:loadSilverSource":                           1,
	"internal/handlers/suggestions.go:GenerateSuggestions":                          1,
}

var edgeReplacedStatuses = map[string]bool{"StatusBadGateway": true, "StatusGatewayTimeout": true}

// edgeReplacedStatusUses returns, per enclosing function, how often a file uses
// a 502/504 constant as a value. Functions are "Func" or "Recv.Func"; anything
// outside a function is "<package>".
func edgeReplacedStatusUses(file *ast.File) map[string]int {
	compared := map[ast.Expr]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				compared[n.X], compared[n.Y] = true, true
			}
		case *ast.CaseClause:
			for _, e := range n.List {
				compared[e] = true
			}
		}
		return true
	})

	uses := map[string]int{}
	count := func(owner string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || compared[sel] || !edgeReplacedStatuses[sel.Sel.Name] {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
				uses[owner]++
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			count("<package>", decl)
			continue
		}
		owner := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			t := fn.Recv.List[0].Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			if id, ok := t.(*ast.Ident); ok {
				owner = id.Name + "." + owner
			}
		}
		count(owner, fn)
	}
	return uses
}

func TestEdgeReplacedStatusDetector(t *testing.T) {
	// Non-zero control: the ratchet below is only as good as this detector.
	const src = `package p
import "net/http"
func Writes(c ctx)  { c.JSON(http.StatusBadGateway, nil) }
func Timeout(c ctx) { c.JSON(http.StatusGatewayTimeout, nil) }
func Reads(code int) bool {
	switch code {
	case http.StatusBadGateway:
		return true
	}
	return code == http.StatusGatewayTimeout || http.StatusBadGateway != code
}
func Passes(c ctx) { c.JSON(http.StatusFailedDependency, nil) }
func (s *svc) Method(c ctx) { c.JSON(http.StatusBadGateway, nil) }
var fallback = http.StatusBadGateway
`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := edgeReplacedStatusUses(file)
	want := map[string]int{"Writes": 1, "Timeout": 1, "svc.Method": 1, "<package>": 1}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("uses = %v, want %v", got, want)
	}
}

func TestNoNewEdgeReplacedStatus(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	total := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for owner, n := range edgeReplacedStatusUses(file) {
			got[filepath.ToSlash(rel)+":"+owner] = n
			total += n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("found no 502/504 uses at all: the walk is looking in the wrong place")
	}

	var problems []string
	for site, n := range got {
		if base := edgeReplacedBaseline[site]; n > base {
			problems = append(problems, fmt.Sprintf("%s uses a 502/504 %d time(s), baseline %d. Cloudflare replaces "+
				"an origin 502/504 with its own page, so the reason in this answer never reaches the browser. "+
				"Answer with 424 (a dependency the request needs failed), 500 (the gateway's own failure) or "+
				"503 (unavailable, try again) -- all three pass through.", site, n, base))
		}
	}
	for site, base := range edgeReplacedBaseline {
		if n := got[site]; n < base {
			problems = append(problems, fmt.Sprintf("%s now uses a 502/504 %d time(s), baseline %d: "+
				"lower its line in edgeReplacedBaseline (delete it at 0) so the fix stays fixed.", site, n, base))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		sites := make([]string, 0, len(got))
		for site := range got {
			sites = append(sites, site)
		}
		sort.Strings(sites)
		var current strings.Builder
		for _, site := range sites {
			fmt.Fprintf(&current, "\t%q: %d,\n", site, got[site])
		}
		t.Fatalf("%s\n\ncurrent counts:\n%s", strings.Join(problems, "\n"), current.String())
	}
}
