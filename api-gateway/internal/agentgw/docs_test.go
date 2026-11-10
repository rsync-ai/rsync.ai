package agentgw

import (
	"flag"
	"os"
	"strings"
	"testing"

	"api-gateway/internal/services"
)

var updateDocs = flag.Bool("update", false, "rewrite the generated part of docs/api/agent-gateway.md")

const referenceDoc = "../../../docs/api/agent-gateway.md"

// The published reference must list exactly the tools, arguments, scopes,
// roles and returned keys the server enforces. A tool change leaves the page
// stale and fails here; `-update` rewrites the generated part. The page is in
// the go paths filter so a hand edit to it runs this too.
func TestReferenceDocIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(referenceDoc)
	if err != nil {
		t.Fatalf("read %s: %v", referenceDoc, err)
	}
	doc := string(raw)
	head, rest, ok := strings.Cut(doc, DocsBegin+"\n")
	if !ok {
		t.Fatalf("%s has no %q marker", referenceDoc, DocsBegin)
	}
	committed, tail, ok := strings.Cut(rest, DocsEnd)
	if !ok {
		t.Fatalf("%s has no %q marker after the begin marker", referenceDoc, DocsEnd)
	}

	want := RenderDocs(registry(Handlers{})) + "\n"
	if committed == want {
		return
	}
	if *updateDocs {
		if err := os.WriteFile(referenceDoc, []byte(head+DocsBegin+"\n"+want+DocsEnd+tail), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote the generated part of %s", referenceDoc)
		return
	}
	t.Fatalf("%s is stale: run `go test ./internal/agentgw -run TestReferenceDocIsCurrent -update` from api-gateway", referenceDoc)
}

// The renderer must say what each tool can return and what it needs, or a
// current page could still be an empty one.
func TestRenderDocs_NamesEveryToolScopeAndKey(t *testing.T) {
	tools := registry(Handlers{})
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	out := RenderDocs(tools)
	for _, v := range protocolVersions {
		if !strings.Contains(out, "`"+v+"`") {
			t.Errorf("protocol version %s missing", v)
		}
	}
	for _, tool := range tools {
		section, _, _ := strings.Cut(strings.SplitN(out, "### `"+tool.Name+"`", 2)[1], "\n### ")
		scope, _ := services.AgentScope(tool.Action)
		role, _ := services.MinRole(tool.Action)
		if scope == "" || !strings.Contains(section, "`"+scope+"`") || !strings.Contains(section, "`"+string(role)+"` or higher") {
			t.Errorf("%s: scope or role missing:\n%s", tool.Name, section)
		}
		keys := tool.Returns()
		if len(keys) == 0 {
			t.Errorf("%s returns no keys", tool.Name)
		}
		for _, k := range keys {
			if !strings.Contains(section, "`"+k+"`") {
				t.Errorf("%s: returned key %s missing", tool.Name, k)
			}
		}
		for _, a := range tool.Args {
			if !strings.Contains(section, "| `"+a.Name+"` |") {
				t.Errorf("%s: argument %s missing", tool.Name, a.Name)
			}
		}
	}
	if strings.Contains(out, "| no | | ") {
		t.Error("an argument row has an empty description")
	}
}
