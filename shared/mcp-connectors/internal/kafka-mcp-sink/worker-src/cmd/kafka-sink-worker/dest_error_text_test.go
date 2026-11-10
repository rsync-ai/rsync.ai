package main

import (
	"net/http"
	"strings"
	"testing"
)

// A destination that refuses a write says why in one of three shapes:
//   - {"success": false, "error": "…"}                     — most connectors
//   - {"success": false, "errors": ["…", …]}               — a generated REST
//     connector's write tools (connector.py.j2), and every connector answering
//     with base_connector.ImportResult, whose errors are {"code","message",…}
//   - {"jsonrpc": "2.0", "error": {"code": …, "message": "…"}} — the MCP server
//     itself (an unknown method, a handler that raised)
//
// The sink read only the first. A generated connector's refusal reached the run
// as "dest error: <nil>" (P5, live: 10 ClickUp tasks refused, the run said
// nothing about why), and a JSON-RPC error as "dest error: map[code:… …]".
// Bug class: a refusal's reason read from one key of a reply that has three.

func TestDestErrorText_ReadsEveryShapeOfARefusal(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"errors list of strings", `{"success":false,"rows_upserted":0,"errors":["HTTP 400","HTTP 400"]}`, "HTTP 400"},
		{"errors list of ImportResult items", `{"success":false,"written":0,"failed":2,"errors":[{"code":"MUTATION_FAILED","message":"name is required"}]}`, "name is required"},
		{"JSON-RPC error object", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"handler raised: boom"}}`, "handler raised: boom"},
	}
	for _, ep := range destEntryPoints() {
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				script := &hostScript{reply: everyHost(200, c.body)}
				err := ep.call(&http.Client{Transport: script})
				if err == nil {
					t.Fatal("a write the destination refused returned no error")
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("err = %q, want it to carry %q", err, c.want)
				}
				for _, junk := range []string{"<nil>", "map["} {
					if strings.Contains(err.Error(), junk) {
						t.Errorf("err = %q carries %q — the reason was in the reply and was not read", err, junk)
					}
				}
			})
		}
	}
}

func TestDestErrorText_ARefusalWithNoReasonSaysSo(t *testing.T) {
	for _, ep := range destEntryPoints() {
		t.Run(ep.name, func(t *testing.T) {
			script := &hostScript{reply: everyHost(200, `{"success":false}`)}
			err := ep.call(&http.Client{Transport: script})
			if err == nil {
				t.Fatal("a write the destination refused returned no error")
			}
			if strings.Contains(err.Error(), "<nil>") || !strings.Contains(err.Error(), "no reason") {
				t.Errorf("err = %q, want it to say the destination gave no reason", err)
			}
		})
	}
}

// The import_data fallback exists for a connector that has no upsert/delete
// tool. It fired on any error containing "not found" — so a destination that
// ran the upsert and answered "record 42 not found" had the whole batch sent
// again as a plain append (duplicates, on every API whose item route 404s).
// Only a reply that says the TOOL is missing may fall back.
func TestDestFallback_OnlyAMissingToolFallsBackToImportData(t *testing.T) {
	fallbackPaths := []string{"writeCDCToDestination", "writeToDestination"}
	cases := []struct {
		name, body   string
		wantFallback bool
	}{
		{"business not found", failingAnswer("record 42 not found"), false},
		{"business not found in errors", `{"success":false,"errors":["task not found"]}`, false},
		{"base connector unknown tool", failingAnswer("Unknown tool: postgresql_upsert_data"), true},
		{"tool not found", failingAnswer("tool not found: postgresql_upsert_data"), true},
		{"JSON-RPC method not found", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found: postgresql_upsert_data"}}`, true},
	}
	for _, ep := range destEntryPoints() {
		if !containsString(fallbackPaths, ep.name) {
			continue
		}
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				script := &hostScript{reply: func(host, tool string) (int, string, bool) {
					if strings.HasSuffix(tool, "_import_data") {
						return 200, `{"success":true,"rows_inserted":1}`, true
					}
					return 200, c.body, true
				}}
				_ = ep.call(&http.Client{Transport: script})
				tools := script.writeTools()
				fellBack := len(tools) == 2 && strings.HasSuffix(tools[1], "_import_data")
				if fellBack != c.wantFallback {
					t.Errorf("tools called = %v; fell back to import_data = %v, want %v", tools, fellBack, c.wantFallback)
				}
			})
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
