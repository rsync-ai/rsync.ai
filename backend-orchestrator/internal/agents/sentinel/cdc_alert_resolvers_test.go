package sentinel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The bug class: a CDC alert that only a person can close. cdc-sink-wedged- was raised by
// the sink auto-restart rung and closed by nothing — not when the sink drained again, not on
// stop, not on start — so the row stayed open until the pipeline was deleted. Every other
// cdc- alert has a function that closes it when its own condition clears; this test holds the
// list of them, and fails for an alert id that has none.

// alertResolvers names, for each CDC alert id, the function that closes it once its
// condition has cleared. Closing on stop (cdc_stopped_alerts.go) does not count: that is the
// pipeline ending, not the problem going away.
var alertResolvers = map[string][]string{
	"cdc-capture-stopped-": {"surfaceCaptureDown"},
	"cdc-connector-down-":  {"checkActivePipelines"},
	"cdc-source-stalled-":  {"checkSourceFreshness"},
	"cdc-lag-":             {"resolveSourceLagAlert"},
	"cdc-sink-lag-":        {"checkSinkConsumerLag"},
	"cdc-sink-absent-":     {"actOnSinkPresence"},
	"cdc-sink-wedged-":     {"resolveSinkWedged"},
	"cdc-rows-parked-":     {"checkRowsParked"},
	"cdc-position-":        {"resolveHorizonAlert"},
	"cdc-wal-":             {"resolveWALAlert"},
}

// closesARow is how a function in this package closes an alert: one of the shared resolvers,
// or its own DELETE (inline, or in a package-level SQL constant it names).
var closesARow = regexp.MustCompile(`\bresolveLagIssue\(|\bresolveWALAlert\(|\bresolveHorizonAlert\(|DELETE FROM sentinel_active_issues`)

// TestEveryCDCAlertHasAFunctionThatClosesIt: for every cdc- alert id spelled in the
// package's source, the named resolver must exist, must name that id (the literal, or a
// function that builds it), must close a row, and must be called from somewhere — a resolver
// nothing calls closes nothing.
func TestEveryCDCAlertHasAFunctionThatClosesIt(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	body := map[string]string{} // function name → its source
	called := map[string]bool{} // every function name some non-test code calls
	var deleteSQL []string      // package-level constants and vars whose SQL closes a row
	var all strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(src)
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		text := func(n ast.Node) string {
			return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					body[d.Name.Name] = text(d)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && closesARow.MatchString(text(vs)) {
						for _, n := range vs.Names {
							deleteSQL = append(deleteSQL, n.Name)
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				called[fun.Name] = true
			case *ast.SelectorExpr:
				called[fun.Sel.Name] = true
			}
			return true
		})
	}

	literal := regexp.MustCompile(`"(cdc-[a-z-]+-)(%s)?"`)
	seen := map[string]bool{}
	for _, m := range literal.FindAllStringSubmatch(all.String(), -1) {
		if notAlertIDs[m[1]] == "" {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no cdc- id literals: the scan is broken, not the package clean")
	}

	for prefix := range seen {
		resolvers, ok := alertResolvers[prefix]
		if !ok {
			t.Errorf("alert id %q has no entry in alertResolvers. Which function closes it once its "+
				"condition clears? Without one the alert stays open until the pipeline is deleted.", prefix)
			continue
		}
		// The functions that spell the id: a resolver names one of them, or the literal itself.
		var builders []string
		for name, src := range body {
			if strings.Contains(src, `"`+prefix) {
				builders = append(builders, name)
			}
		}
		for _, name := range resolvers {
			src, ok := body[name]
			if !ok {
				t.Errorf("%q: resolver %s does not exist", prefix, name)
				continue
			}
			names := strings.Contains(src, `"`+prefix)
			for _, b := range builders {
				if b != name && regexp.MustCompile(`\b`+b+`\(`).MatchString(src) {
					names = true
				}
			}
			if !names {
				t.Errorf("%q: resolver %s never names that id (the literal, or one of %v)", prefix, name, builders)
			}
			closes := closesARow.MatchString(src)
			for _, c := range deleteSQL {
				if regexp.MustCompile(`\b` + c + `\b`).MatchString(src) {
					closes = true
				}
			}
			if !closes {
				t.Errorf("%q: resolver %s closes no row", prefix, name)
			}
			if !called[name] {
				t.Errorf("%q: resolver %s is never called", prefix, name)
			}
		}
	}
	for prefix := range alertResolvers {
		if !seen[prefix] {
			t.Errorf("alertResolvers names %q, which is not spelled anywhere in the package: drop it", prefix)
		}
	}
}
