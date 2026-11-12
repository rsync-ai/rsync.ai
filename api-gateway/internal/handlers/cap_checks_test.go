package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// capChecks names each function here that counts rows and refuses at a cap
// (a COUNT(*) compared with >= to a max* constant). Each is either
// "lock: <fn>", where fn takes a lock that holds the count and the insert
// together (fn is the function itself, one it calls, or one that calls it), or
// "gap: <why>". Without a lock, two requests at once both read N-1 and both
// insert, so the cap is passed. Bug class: a cap checked by count-then-insert
// outside a lock.
var capChecks = map[string]string{
	"insertAgentTokenUnderCap": "lock: insertAgentTokenUnderCap",
	"admitGenerationRun":       "lock: admitGenerationRun",
	"CreateWorkspaceLayer":     "lock: beginLayerWrite",
	"saveModelCheck":           "lock: lockCheckTarget",
	"applyApprovedModelCheck":  "lock: reviewModelCheckProposal",
	"ProposeDashboardChange":   "gap: a per-user cap on unapplied drafts; a race passes it by the requests in flight",
	"ProposeDashboard":         "gap: a per-user cap on unsaved drafts; a race passes it by the requests in flight",
	"ProposePipelinePlan":      "gap: a per-user cap on open plans; a race passes it by the requests in flight",
	"ProposeWorkflowChange":    "gap: a per-user cap on unapplied changes; a race passes it by the requests in flight",
	"storeWorkflowDraft":       "gap: a per-user cap on drafts, drafted or built; a race passes it by the requests in flight",
}

var (
	capIdentRe = regexp.MustCompile(`(?i)max`)
	lockSQLRe  = regexp.MustCompile(`pg_advisory_xact_lock|FOR UPDATE`)
)

type capFunc struct {
	file         string
	counts, caps bool
	locks        bool
	calls        map[string]bool
}

// TestEveryCapCheckIsLockedOrAKnownGap: a new count-then-cap must take a lock
// (insertAgentTokenUnderCap is the model), or be listed above with why not.
func TestEveryCapCheckIsLockedOrAKnownGap(t *testing.T) {
	funcs := scanCapFuncs(t)
	found := 0
	for name, f := range funcs {
		if !f.counts || !f.caps {
			continue
		}
		found++
		want, ok := capChecks[name]
		if !ok {
			t.Errorf("%s %s refuses at a cap it counted: hold the count and the insert under one lock "+
				"(see insertAgentTokenUnderCap), then list it in capChecks", f.file, name)
			continue
		}
		lockFn, isLock := strings.CutPrefix(want, "lock: ")
		if !isLock {
			continue
		}
		l, ok := funcs[lockFn]
		if !ok || !l.locks {
			t.Errorf("%s: capChecks says %s takes its lock, but %s has no pg_advisory_xact_lock or FOR UPDATE", name, lockFn, lockFn)
			continue
		}
		if lockFn != name && !f.calls[lockFn] && !l.calls[name] {
			t.Errorf("%s: capChecks says %s takes its lock, but neither calls the other", name, lockFn)
		}
	}
	for name := range capChecks {
		if f, ok := funcs[name]; !ok || !f.counts || !f.caps {
			t.Errorf("capChecks names %s, which no longer refuses at a counted cap; drop it", name)
		}
	}
	if found < 5 {
		t.Fatalf("found %d cap checks: the scan is broken", found)
	}
}

// scanCapFuncs reads every function in this package's non-test files.
func scanCapFuncs(t *testing.T) map[string]*capFunc {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*capFunc{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			cf := &capFunc{file: p, calls: map[string]bool{}}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						cf.counts = cf.counts || strings.Contains(x.Value, "COUNT(*)")
						cf.locks = cf.locks || lockSQLRe.MatchString(x.Value)
					}
				case *ast.BinaryExpr:
					if id, ok := x.Y.(*ast.Ident); ok && x.Op == token.GEQ && capIdentRe.MatchString(id.Name) {
						cf.caps = true
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						cf.calls[id.Name] = true
					}
				}
				return true
			})
			out[fn.Name.Name] = cf
		}
	}
	return out
}
