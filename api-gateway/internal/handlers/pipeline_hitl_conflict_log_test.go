package handlers

import (
	"database/sql/driver"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

// A HITL answer the gateway refuses (409) is the user's "my answer did nothing",
// and the gateway kept no trace of it: none of the five refusals was logged, so a
// pause that would not take an answer could not be told from a request that never
// arrived. The class is every 409 a HITL answer gets: each is logged once, at
// info, with the pipeline, the run and the code, and never what the user picked.

// hitlLogs captures the standard logger at info for one test.
func hitlLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	std := logrus.StandardLogger()
	prevHooks := std.ReplaceHooks(make(logrus.LevelHooks))
	prevLevel := std.GetLevel()
	std.SetLevel(logrus.InfoLevel)
	t.Cleanup(func() {
		std.ReplaceHooks(prevHooks)
		std.SetLevel(prevLevel)
	})
	return logrustest.NewLocal(std)
}

func assertHITLRefusalLogged(t *testing.T, hook *logrustest.Hook, code string) {
	t.Helper()
	var got []*logrus.Entry
	var seen []string
	for _, e := range hook.AllEntries() {
		seen = append(seen, e.Level.String()+" "+e.Message)
		if e.Data["code"] == code {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("want the %s refusal logged once, got %d; the log was %q", code, len(got), seen)
	}
	e := got[0]
	if e.Level != logrus.InfoLevel {
		t.Errorf("a refusal is the user's own race, not a server fault: logged at %s", e.Level)
	}
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"code", "execution_id", "pipeline_id"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("logged fields %v, want exactly %v (nothing the user picked)", keys, want)
	}
	if e.Data["pipeline_id"] != wsScopePipeline || e.Data["execution_id"] != hitlTestExec {
		t.Errorf("the refusal does not name the pipeline and the run answered: %v", e.Data)
	}
}

// loseTheTableClaim scripts another tab's different selection already in flight.
func loseTheTableClaim(f *hitlFakeDB) {
	claim := `{"hitl_tables_selected":{"execution_id":"` + hitlTestExec + `","selected_tables":["public.customers"]}}`
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append([]hitlFakeRule{
		{match: regexp.MustCompile(`'\{hitl_tables_selected\}', \$1::jsonb, true\),\s+stage_last_heartbeat_at`), exec: func([]driver.Value) int64 { return 0 }},
		{match: regexp.MustCompile(`SELECT metadata FROM pipeline_progress WHERE pipeline_id`), query: hitlRows([]string{"metadata"}, []driver.Value{claim})},
	}, f.rules...)
}

func TestHITLResume_EveryRefusalIsLogged(t *testing.T) {
	atThePause := func(t *testing.T, e hitlEndpoint) *hitlFakeDB {
		return newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
	}
	listening := func(t *testing.T, e hitlEndpoint) *hitlFakeDB {
		f := atThePause(t, e)
		installHITLTemporal(t, []string{hitlTestExec}, nil)
		stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)
		return f
	}
	reported := func(e hitlEndpoint) bool { return e.waitType != "" }
	for _, s := range []struct {
		code    string
		applies func(hitlEndpoint) bool
		arrange func(*testing.T, hitlEndpoint)
		answers int
	}{
		{"run_finished", func(hitlEndpoint) bool { return true }, func(t *testing.T, e hitlEndpoint) {
			atThePause(t, e)
			installHITLTemporal(t, nil, []string{hitlTestExec})
			stubHITLRun(t, []string{"completed"}, hitlWaitAnswer{}, errHITLRunNotOpen)
		}, 1},
		{"pause_already_answered", reported, func(t *testing.T, e hitlEndpoint) {
			atThePause(t, e)
			installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, listeningAt("APPROVAL"), nil)
		}, 1},
		{"pause_busy", reported, func(t *testing.T, e hitlEndpoint) {
			atThePause(t, e)
			installHITLTemporal(t, []string{hitlTestExec}, nil)
			ans := listeningAt(e.waitType)
			ans.Receiving = false
			stubHITLRun(t, []string{""}, ans, nil)
		}, 1},
		{"hitl_answer_in_flight", func(e hitlEndpoint) bool {
			return e.kind == hitlKindConnections || e.kind == hitlKindConnectors
		}, func(t *testing.T, e hitlEndpoint) { listening(t, e) }, 2},
		{"table_selection_in_flight", func(e hitlEndpoint) bool { return e.kind == hitlKindTables },
			func(t *testing.T, e hitlEndpoint) { loseTheTableClaim(listening(t, e)) }, 1},
	} {
		for _, e := range hitlEndpoints {
			if !s.applies(e) {
				continue
			}
			t.Run(s.code+"/"+e.name, func(t *testing.T) {
				s.arrange(t, e)
				hook := hitlLogs(t)
				var w *httptest.ResponseRecorder
				for i := 0; i < s.answers; i++ {
					w = e.call(t, hitlTestExec)
				}
				if w.Code != http.StatusConflict || hitlErrorCode(t, w) != s.code {
					t.Fatalf("want 409 %s, got %d: %s", s.code, w.Code, w.Body.String())
				}
				assertHITLRefusalLogged(t, hook, s.code)
			})
		}
	}
}

// Every 409 in the HITL handlers goes through respondHITLConflict, so a refusal
// added later is logged too.
func TestHITLRefusalsGoThroughOneLoggedAnswer(t *testing.T) {
	files, err := filepath.Glob("pipeline_hitl*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	inHelper := 0
	var outside []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				conflict := false
				switch n := n.(type) {
				case *ast.SelectorExpr:
					conflict = n.Sel.Name == "StatusConflict"
				case *ast.BasicLit:
					conflict = n.Value == "409"
				}
				if conflict {
					if fn.Name.Name == "respondHITLConflict" {
						inHelper++
					} else {
						outside = append(outside, fset.Position(n.Pos()).String()+" in "+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if inHelper != 1 {
		t.Fatalf("respondHITLConflict should answer the one 409; found %d there (files %v)", inHelper, files)
	}
	for _, at := range outside {
		t.Errorf("a HITL 409 that is not logged: %s; answer it with respondHITLConflict", at)
	}
}
